package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

// LayoutPack is an existing pack as the manifest DAG declares it. Declared
// totals cover everything the pack holds, including copies that lost the merge.
type LayoutPack struct {
	ID            string
	Key           string
	StoredSize    int64
	DeclaredBytes int64
	DeclaredCount int
}

// LayoutEntry is the copy of a digest a reader would actually be served.
type LayoutEntry struct {
	Kind     string
	Digest   string
	CID      string
	Block    string
	Encoding string
	Pack     string
	Size     int64
	// ClosurePacks is carried forward verbatim. Nothing reads it at runtime, so
	// it may name packs a later pass has already removed.
	ClosurePacks []string
}

// LayoutManifest names a manifest that contributed to the merged view.
type LayoutManifest struct {
	ID   string
	Key  string
	Pack string
}

// Layout is the merged manifest view, built by the same tie-breaks a cache
// server uses so that anything reasoning about the stored layout resolves a
// digest to the copy readers get.
type Layout struct {
	Packs     []LayoutPack
	Entries   []LayoutEntry
	Manifests []LayoutManifest
	// Heads are the manifests no other manifest claims as a parent. A manifest
	// published afterwards names them so the DAG stays connected.
	Heads []string
	// OrphanPacks are listed packs no live manifest declares. They are reported
	// rather than acted on: a pack a concurrent job has published but not yet
	// committed a manifest for is indistinguishable from a truly orphaned one.
	OrphanPacks int
	// OrphanManifests are manifests whose pack is gone. They self-expire.
	OrphanManifests int
	// UnreadableManifests were listed but could not be restored, because they
	// belong to another ref's cache scope or were evicted. A plan then rests on
	// the part of the layout this job can actually see.
	UnreadableManifests int
}

type LayoutOptions struct {
	Backend     cache.Backend
	Catalog     cache.Catalog
	KeyPrefix   string
	MaxBlobSize int64
	Timeout     time.Duration
	Warn        func(string, ...any)
}

// ManifestKey names the cache entry a manifest is committed under.
func ManifestKey(prefix, packID, manifestID string) string {
	return manifestKeyFor(prefix, packID, manifestID)
}

// ReadLayout discovers and merges the manifest DAG. It publishes nothing, and
// the listings it relies on read metadata only.
func ReadLayout(ctx context.Context, options LayoutOptions) (Layout, error) {
	if options.Backend == nil || options.Catalog == nil {
		return Layout{}, errors.New("reading the layout needs a backend and a catalog")
	}
	if options.Warn == nil {
		options.Warn = func(string, ...any) {}
	}
	if options.MaxBlobSize <= 0 {
		options.MaxBlobSize = 512 * 1024 * 1024
	}

	manifestKeys, err := options.Catalog.List(ctx, manifestKeyPrefix(options.KeyPrefix))
	if err != nil {
		return Layout{}, err
	}
	packKeys, err := options.Catalog.List(ctx, packKeyFor(options.KeyPrefix, ""))
	if err != nil {
		return Layout{}, err
	}
	listedPacks := make(map[string]struct{}, len(packKeys))
	for _, key := range packKeys {
		if id, ok := parsePackKey(options.KeyPrefix, key); ok {
			listedPacks[id] = struct{}{}
		}
	}

	layout := Layout{}
	decoded := make(map[string]manifest, len(manifestKeys))
	references := make(map[string]LayoutManifest, len(manifestKeys))
	for _, key := range manifestKeys {
		packID, manifestID, ok := parseManifestKey(options.KeyPrefix, key)
		if !ok {
			continue
		}
		if _, listed := listedPacks[packID]; !listed {
			layout.OrphanManifests++
			continue
		}
		value, err := loadManifestFrom(ctx, options, key, manifestID)
		if err != nil {
			// The REST listing reports entries this job's cache scope cannot
			// restore, and entries evicted since it was taken, so this is routine.
			// Skipping is safe because packs enter the layout only through a
			// manifest that loaded: one that did not is never a deletion candidate.
			layout.UnreadableManifests++
			options.Warn("skipping manifest %s: %s", manifestID, safeError(err))
			continue
		}
		if len(value.Packs) != 1 || value.Packs[0].ID != packID {
			// Readers skip this deterministically, so the merged view matches
			// theirs by skipping it too.
			options.Warn("ignoring manifest %s: it does not commit the pack %s named by its key", manifestID, packID)
			continue
		}
		decoded[manifestID] = value
		references[manifestID] = LayoutManifest{ID: manifestID, Key: key, Pack: packID}
	}

	packs := make(map[string]packDescriptor)
	declaredBytes := make(map[string]int64)
	declaredCount := make(map[string]int)
	casWinners := make(map[string]manifestObject)
	actionWinners := make(map[string]manifestAction)
	conflicts := make(map[string]struct{})

	ids := make([]string, 0, len(decoded))
	for id := range decoded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	heads := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		value := decoded[id]
		heads[id] = struct{}{}
		for _, parent := range value.Parents {
			delete(heads, parent)
		}
		for _, descriptor := range value.Packs {
			if existing, found := packs[descriptor.ID]; !found || preferPack(descriptor, existing) {
				packs[descriptor.ID] = descriptor
			}
		}
		for _, object := range value.CAS {
			declaredBytes[object.PackID] += object.Size
			declaredCount[object.PackID]++
			if existing, found := casWinners[object.Digest]; !found || preferObject(object, existing) {
				casWinners[object.Digest] = object
			}
		}
		for _, action := range value.Actions {
			declaredBytes[action.PackID] += action.Size
			declaredCount[action.PackID]++
			if existing, found := actionWinners[action.Digest]; found {
				if existing.CID != action.CID {
					conflicts[action.Digest] = struct{}{}
				}
				continue
			}
			actionWinners[action.Digest] = action
		}
	}
	for packID := range listedPacks {
		if _, declared := packs[packID]; !declared {
			layout.OrphanPacks++
		}
	}

	for _, id := range ids {
		layout.Manifests = append(layout.Manifests, references[id])
		if _, head := heads[id]; head {
			layout.Heads = append(layout.Heads, id)
		}
	}
	for _, descriptor := range packs {
		layout.Packs = append(layout.Packs, LayoutPack{
			ID:            descriptor.ID,
			Key:           descriptor.Key,
			StoredSize:    descriptor.Size,
			DeclaredBytes: declaredBytes[descriptor.ID],
			DeclaredCount: declaredCount[descriptor.ID],
		})
	}
	sort.Slice(layout.Packs, func(i, j int) bool { return layout.Packs[i].ID < layout.Packs[j].ID })

	for _, object := range casWinners {
		layout.Entries = append(layout.Entries, LayoutEntry{
			Kind: "cas", Digest: object.Digest, CID: object.CID, Block: object.Block,
			Encoding: object.Encoding, Pack: object.PackID, Size: object.Size,
		})
	}
	for _, action := range actionWinners {
		// A digest two jobs disagree about is never served, so it is not part of
		// the layout a reader sees and must not be repacked as though it were.
		if _, conflict := conflicts[action.Digest]; conflict {
			continue
		}
		layout.Entries = append(layout.Entries, LayoutEntry{
			Kind: "ac", Digest: action.Digest, CID: action.CID, Block: action.Block,
			Encoding: action.Encoding, Pack: action.PackID, Size: action.Size,
			ClosurePacks: action.ClosurePacks,
		})
	}
	sort.Slice(layout.Entries, func(i, j int) bool {
		if layout.Entries[i].Kind != layout.Entries[j].Kind {
			return layout.Entries[i].Kind < layout.Entries[j].Kind
		}
		return layout.Entries[i].Digest < layout.Entries[j].Digest
	})
	return layout, nil
}

func loadManifestFrom(ctx context.Context, options LayoutOptions, key, id string) (manifest, error) {
	var data bytesBuffer
	loadCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	found, err := options.Backend.Load(loadCtx, key, &data)
	cancel()
	if err != nil {
		return manifest{}, err
	}
	if !found {
		return manifest{}, errors.New("manifest cache entry is missing")
	}
	if int64(data.Len()) > options.MaxBlobSize {
		return manifest{}, fmt.Errorf("manifest exceeds the maximum object size")
	}
	return decodeManifest(data.Bytes(), id)
}
