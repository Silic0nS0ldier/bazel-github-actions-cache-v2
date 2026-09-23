package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

// Repacker rebuilds packs out of existing ones. It only ever adds: a new pack
// and its manifest leave the originals readable, so failing part way through
// costs storage rather than cache hits.
type Repacker struct {
	options RepackOptions
	sources map[string]string
	packs   map[string]LayoutPack
}

type RepackOptions struct {
	Backend     cache.Backend
	CacheDir    string
	KeyPrefix   string
	MaxBlobSize int64
	Timeout     time.Duration
	// Parents are the manifest heads a published manifest descends from.
	Parents []string
	// Packs supplies the source pack descriptors entries are copied out of.
	Packs []LayoutPack
}

func NewRepacker(options RepackOptions) (*Repacker, error) {
	if options.Backend == nil {
		return nil, errors.New("repacking needs a backend")
	}
	if options.CacheDir == "" {
		return nil, errors.New("repacking needs a spool directory")
	}
	if options.MaxBlobSize <= 0 {
		options.MaxBlobSize = 512 * 1024 * 1024
	}
	packs := make(map[string]LayoutPack, len(options.Packs))
	for _, pack := range options.Packs {
		packs[pack.ID] = pack
	}
	return &Repacker{options: options, sources: make(map[string]string), packs: packs}, nil
}

// Publish writes the entries into one new pack and commits a manifest for it.
// Entries still name the pack they are being copied out of.
func (r *Repacker) Publish(ctx context.Context, entries []LayoutEntry) (LayoutPack, string, error) {
	if len(entries) == 0 {
		return LayoutPack{}, "", errors.New("cannot publish an empty pack")
	}
	type block struct {
		id   cid.Cid
		data []byte
	}
	gathered := make([]block, 0, len(entries))
	for _, entry := range entries {
		data, err := r.readBlock(ctx, entry)
		if err != nil {
			return LayoutPack{}, "", err
		}
		blockCID, err := cid.Decode(entry.Block)
		if err != nil {
			return LayoutPack{}, "", err
		}
		gathered = append(gathered, block{id: blockCID, data: data})
	}

	path, err := writePrivateTemp(r.options.CacheDir, "repack-*", nil)
	if err != nil {
		return LayoutPack{}, "", err
	}
	defer os.Remove(path)
	store, err := openPackWriter(path, gathered[0].id)
	if err != nil {
		return LayoutPack{}, "", err
	}
	for _, item := range gathered {
		value, err := blocks.NewBlockWithCid(item.data, item.id)
		if err != nil {
			store.Discard()
			return LayoutPack{}, "", err
		}
		if err := store.Put(ctx, value); err != nil {
			store.Discard()
			return LayoutPack{}, "", err
		}
	}
	if err := store.Finalize(); err != nil {
		return LayoutPack{}, "", err
	}

	info, err := os.Stat(path)
	if err != nil {
		return LayoutPack{}, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return LayoutPack{}, "", err
	}
	packID, hashErr := hashFile(file)
	closeErr := file.Close()
	if hashErr != nil {
		return LayoutPack{}, "", hashErr
	}
	if closeErr != nil {
		return LayoutPack{}, "", closeErr
	}

	pack := LayoutPack{
		ID:         packID,
		Key:        packKeyFor(r.options.KeyPrefix, packID),
		StoredSize: info.Size(),
	}
	// The pack goes first so no manifest ever advertises data a reader cannot
	// restore, which is the same ordering the cache server commits with.
	if err := r.publishFile(ctx, pack.Key, path, pack.StoredSize); err != nil {
		return LayoutPack{}, "", fmt.Errorf("publish rebuilt pack: %w", err)
	}

	manifestID, err := r.publishManifest(ctx, pack, entries)
	if err != nil {
		return LayoutPack{}, "", err
	}
	return pack, manifestID, nil
}

func (r *Repacker) publishManifest(ctx context.Context, pack LayoutPack, entries []LayoutEntry) (string, error) {
	value := manifest{
		Version: manifestFormatVersion,
		Parents: append([]string(nil), r.options.Parents...),
		Packs:   []packDescriptor{{ID: pack.ID, Key: pack.Key, Size: pack.StoredSize}},
	}
	for _, entry := range entries {
		switch entry.Kind {
		case "cas":
			value.CAS = append(value.CAS, manifestObject{
				Digest: entry.Digest, CID: entry.CID, Block: entry.Block,
				Encoding: entry.Encoding, PackID: pack.ID, Size: entry.Size,
			})
		case "ac":
			value.Actions = append(value.Actions, manifestAction{
				Digest: entry.Digest, CID: entry.CID, Block: entry.Block,
				Encoding: entry.Encoding, PackID: pack.ID, Size: entry.Size,
				ClosurePacks: entry.ClosurePacks,
			})
		default:
			return "", fmt.Errorf("unknown entry kind %q", entry.Kind)
		}
	}
	data, manifestID, err := encodeManifest(value)
	if err != nil {
		return "", err
	}
	// Decoding is what enforces the manifest's own invariants, so round-tripping
	// here refuses to publish a commit point a reader would reject.
	if _, err := decodeManifest(data, manifestID); err != nil {
		return "", fmt.Errorf("rebuilt manifest is not readable: %w", err)
	}
	path, err := writePrivateTemp(r.options.CacheDir, "repack-manifest-*", data)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	key := manifestKeyFor(r.options.KeyPrefix, pack.ID, manifestID)
	if err := r.publishFile(ctx, key, path, int64(len(data))); err != nil {
		return "", fmt.Errorf("publish rebuilt manifest: %w", err)
	}
	return manifestID, nil
}

func (r *Repacker) readBlock(ctx context.Context, entry LayoutEntry) ([]byte, error) {
	path, err := r.sourcePath(ctx, entry.Pack)
	if err != nil {
		return nil, err
	}
	// Reading by the block CID verifies the stored bytes, so a copy can never
	// launder a corrupt block into a new pack.
	return readCARBlock(ctx, path, entry.Block, r.options.MaxBlobSize)
}

func (r *Repacker) sourcePath(ctx context.Context, packID string) (string, error) {
	if path, found := r.sources[packID]; found {
		return path, nil
	}
	descriptor, known := r.packs[packID]
	if !known {
		return "", fmt.Errorf("no descriptor for source pack %s", packID)
	}
	path, err := writePrivateTemp(r.options.CacheDir, "source-*", nil)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		os.Remove(path)
		return "", err
	}
	loadCtx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	found, loadErr := r.options.Backend.Load(loadCtx, descriptor.Key, file)
	cancel()
	closeErr := file.Close()
	if loadErr != nil || closeErr != nil || !found {
		os.Remove(path)
		if loadErr != nil {
			return "", loadErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return "", fmt.Errorf("source pack %s is gone", packID)
	}
	info, err := os.Stat(path)
	if err != nil {
		os.Remove(path)
		return "", err
	}
	if info.Size() != descriptor.StoredSize {
		os.Remove(path)
		return "", fmt.Errorf("source pack %s has size %d; the manifest declares %d",
			packID, info.Size(), descriptor.StoredSize)
	}
	r.sources[packID] = path
	return path, nil
}

func (r *Repacker) publishFile(ctx context.Context, key, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	saveCtx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	return r.options.Backend.Save(saveCtx, key, file, size)
}

// Close removes the source packs this repacker spooled locally.
func (r *Repacker) Close() error {
	for _, path := range r.sources {
		os.Remove(path)
	}
	r.sources = make(map[string]string)
	return nil
}
