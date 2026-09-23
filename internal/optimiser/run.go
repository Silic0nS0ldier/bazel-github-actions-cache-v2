package optimiser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

// Pruner deletes immutable cache entries by exact key.
type Pruner interface {
	Delete(ctx context.Context, key string) error
}

type RunOptions struct {
	Backend   cache.Backend
	Catalog   cache.Catalog
	Pruner    Pruner
	Records   *RecordSource
	CacheDir  string
	KeyPrefix string
	// MaxRecords bounds the window of runs a plan is built from.
	MaxRecords  int
	MaxBlobSize int64
	Timeout     time.Duration
	// UploadsPerMinute spaces publications, as the cache server does.
	UploadsPerMinute int
	// MaxNewBytes stops a pass once it has published this much. A cache near
	// its quota cannot hold the old and new layouts at once, and stopping early
	// leaves the remainder for the next pass.
	MaxNewBytes int64
	// Reap deletes packs no manifest names at all. Opt-in, because it is the
	// one thing here that removes data no replacement was published for.
	Reap bool
	// ReapOlderThan keeps a pack a running job has published but not yet
	// committed a manifest for out of reach.
	ReapOlderThan time.Duration
	Lister        EntryLister
	Plan          Options
	// DryRun plans and reports without publishing or deleting anything.
	DryRun bool
	Log    func(string, ...any)
}

type Result struct {
	Records        int
	Packs          int
	Entries        int
	Rebuilt        int
	Deleted        int
	ReclaimedBytes int64
	WastedBytes    int64
	DryRun         bool
	// Unreadable counts manifests the listing reported but this job could not
	// restore, so a plan that saw few of them can be recognised as such.
	Unreadable int
	// PublishedBytes is what this pass added before freeing anything.
	PublishedBytes int64
	// Incomplete marks a pass that stopped at its byte budget with work left.
	Incomplete bool
	// Reaped counts packs removed because no manifest named them.
	Reaped int
	// Reapable counts packs that passed the age filter, whether or not this
	// pass was allowed to delete them.
	Reapable int
	// Unmanifested counts packs no manifest names, whether or not they were
	// reaped.
	Unmanifested int
	// Skipped explains why a pass did no work. A quiet repository is not a
	// failure, and a scheduled pass must not go red for having nothing to do.
	Skipped string
}

// Run collects usage records, plans a repacking, and applies it. Nothing is
// deleted until every replacement pack and manifest is published, so an
// interrupted pass leaves duplicated storage rather than a hole.
func Run(ctx context.Context, options RunOptions) (Result, error) {
	if options.Log == nil {
		options.Log = func(string, ...any) {}
	}
	if options.Pruner == nil && !options.DryRun {
		return Result{}, errors.New("applying a plan needs a pruner")
	}

	layout, err := server.ReadLayout(ctx, server.LayoutOptions{
		Backend:     options.Backend,
		Catalog:     options.Catalog,
		KeyPrefix:   options.KeyPrefix,
		MaxBlobSize: options.MaxBlobSize,
		Timeout:     options.Timeout,
		Warn:        options.Log,
	})
	if err != nil {
		return Result{}, fmt.Errorf("read the stored layout: %w", err)
	}
	options.Log("layout has %s packs, %s live entries, %s orphaned packs (%s named by no manifest), %s orphaned manifests, %s manifests this job cannot restore",
		HumanCount(int64(len(layout.Packs))), HumanCount(int64(len(layout.Entries))),
		HumanCount(int64(layout.OrphanPacks)), HumanCount(int64(len(layout.UnmanifestedPacks))),
		HumanCount(int64(layout.OrphanManifests)), HumanCount(int64(layout.UnreadableManifests)))

	demand := NewDemand()
	records, err := options.Records.Collect(ctx, demand, options.MaxRecords, options.Log)
	if err != nil {
		return Result{}, fmt.Errorf("collect usage records: %w", err)
	}

	result := Result{
		Records:      records,
		Packs:        len(layout.Packs),
		Entries:      len(layout.Entries),
		Unreadable:   layout.UnreadableManifests,
		Unmanifested: len(layout.UnmanifestedPacks),
		DryRun:       options.DryRun,
	}
	// Reaping is independent of any repacking plan: an unmanifested pack is
	// unreachable whether or not the rest of the layout is worth rebuilding. A
	// dry run still applies the age filter, so its count means something.
	if options.Reap {
		reapable, reaped, err := reap(ctx, options, layout)
		result.Reapable = reapable
		result.Reaped = reaped
		if err != nil {
			return result, err
		}
		if options.DryRun {
			options.Log("%d packs no manifest names are old enough to reap", reapable)
		} else {
			options.Log("reaped %d packs no manifest names", reaped)
		}
	}
	plan, err := NewPlan(layout, demand, options.Plan)
	if errors.Is(err, errTooFewRuns) {
		result.Skipped = err.Error()
		options.Log("%s; leaving the layout alone", err)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.ReclaimedBytes = plan.ReclaimedBytes
	result.WastedBytes = plan.WastedBytes
	if plan.Empty() {
		result.Skipped = "every pack is already grouped for how it is read"
		options.Log("%s; nothing to do", result.Skipped)
		return result, nil
	}
	options.Log("rebuilding %d packs into %d, deleting %d empty ones; %s wasted per restore, %s reclaimed",
		len(plan.Rewrite), len(plan.Groups), len(plan.Dead), HumanBytes(plan.WastedBytes), HumanBytes(plan.ReclaimedBytes))
	if options.DryRun {
		return result, nil
	}

	repacker, err := server.NewRepacker(server.RepackOptions{
		Backend:          options.Backend,
		CacheDir:         options.CacheDir,
		KeyPrefix:        options.KeyPrefix,
		MaxBlobSize:      options.MaxBlobSize,
		Timeout:          options.Timeout,
		UploadsPerMinute: options.UploadsPerMinute,
		Parents:          layout.Heads,
		Packs:            layout.Packs,
	})
	if err != nil {
		return result, err
	}
	defer repacker.Close()

	remover := newRemover(ctx, options, layout)
	// Dead packs hold nothing that is served, so removing them before anything
	// is published frees quota rather than adding to the peak.
	for _, packID := range plan.Dead {
		if err := remover.remove(packID); err != nil {
			return result, err
		}
	}

	// A source pack can go as soon as every entry it served has been
	// republished, which keeps the old and new layouts from both being resident
	// in full.
	outstanding := make(map[string]int, len(plan.Rewrite))
	for _, group := range plan.Groups {
		for _, entry := range group.Entries {
			outstanding[entry.Pack]++
		}
	}

	var newBytes int64
	for index, group := range plan.Groups {
		if options.MaxNewBytes > 0 && newBytes >= options.MaxNewBytes {
			// Stopping early is safe: the packs still to be rebuilt were never
			// touched, so they keep serving. The next pass re-plans.
			options.Log("stopping after %d of %d packs, having published %s this pass",
				index, len(plan.Groups), HumanBytes(newBytes))
			result.Incomplete = true
			break
		}
		pack, manifestID, err := repacker.Publish(ctx, group.Entries)
		if err != nil {
			// Whatever was published already stays: it duplicates entries the
			// originals still serve, and the originals are still there.
			return result, fmt.Errorf("publish rebuilt pack %d of %d: %w", index+1, len(plan.Groups), err)
		}
		remover.republished(pack.Key)
		remover.republished(server.ManifestKey(options.KeyPrefix, pack.ID, manifestID))
		newBytes += pack.StoredSize
		result.Rebuilt++
		options.Log("published pack %s with %d entries under manifest %s", pack.ID, len(group.Entries), manifestID)

		for _, entry := range group.Entries {
			outstanding[entry.Pack]--
			if outstanding[entry.Pack] > 0 {
				continue
			}
			delete(outstanding, entry.Pack)
			repacker.Release(entry.Pack)
			if err := remover.remove(entry.Pack); err != nil {
				return result, err
			}
		}
	}
	result.Deleted = remover.deleted
	result.PublishedBytes = newBytes
	return result, nil
}

// remover deletes a pack and the manifests describing it. A pack is only
// reachable through a manifest that names it, and a manifest whose pack is gone
// is skipped and left to expire, so the pack is what has to go.
type remover struct {
	ctx       context.Context
	options   RunOptions
	packs     map[string]server.LayoutPack
	manifests map[string][]server.LayoutManifest
	// Packs are content-addressed, so a group that reproduces an existing pack
	// byte for byte republishes under the key about to be deleted. Keys written
	// by this pass are never removed.
	written map[string]struct{}
	deleted int
}

func newRemover(ctx context.Context, options RunOptions, layout server.Layout) *remover {
	packs := make(map[string]server.LayoutPack, len(layout.Packs))
	for _, pack := range layout.Packs {
		packs[pack.ID] = pack
	}
	manifests := make(map[string][]server.LayoutManifest, len(layout.Manifests))
	for _, reference := range layout.Manifests {
		manifests[reference.Pack] = append(manifests[reference.Pack], reference)
	}
	return &remover{
		ctx:       ctx,
		options:   options,
		packs:     packs,
		manifests: manifests,
		written:   make(map[string]struct{}),
	}
}

func (r *remover) republished(key string) { r.written[key] = struct{}{} }

func (r *remover) remove(packID string) error {
	pack, known := r.packs[packID]
	if !known {
		return fmt.Errorf("no descriptor for pack %s", packID)
	}
	if _, written := r.written[pack.Key]; written {
		r.options.Log("keeping pack %s: the rebuild reproduced it exactly", packID)
	} else {
		if err := r.options.Pruner.Delete(r.ctx, pack.Key); err != nil {
			return fmt.Errorf("delete replaced pack %s: %w", packID, err)
		}
		r.deleted++
	}
	// Tidiness only, and safe in either order: an orphaned manifest is already
	// skipped by readers.
	for _, reference := range r.manifests[packID] {
		if _, written := r.written[reference.Key]; written {
			continue
		}
		if err := r.options.Pruner.Delete(r.ctx, reference.Key); err != nil {
			r.options.Log("could not delete superseded manifest %s: %v", reference.ID, err)
		}
	}
	return nil
}
