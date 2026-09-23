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
	Plan        Options
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
	options.Log("layout has %d packs, %d live entries, %d orphaned packs, %d orphaned manifests",
		len(layout.Packs), len(layout.Entries), layout.OrphanPacks, layout.OrphanManifests)

	demand := NewDemand()
	records, err := options.Records.Collect(ctx, demand, options.MaxRecords, options.Log)
	if err != nil {
		return Result{}, fmt.Errorf("collect usage records: %w", err)
	}

	result := Result{
		Records: records,
		Packs:   len(layout.Packs),
		Entries: len(layout.Entries),
		DryRun:  options.DryRun,
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
	options.Log("rebuilding %d packs into %d, deleting %d empty ones; %d wasted bytes per restore, %d bytes reclaimed",
		len(plan.Rewrite), len(plan.Groups), len(plan.Dead), plan.WastedBytes, plan.ReclaimedBytes)
	if options.DryRun {
		return result, nil
	}

	repacker, err := server.NewRepacker(server.RepackOptions{
		Backend:     options.Backend,
		CacheDir:    options.CacheDir,
		KeyPrefix:   options.KeyPrefix,
		MaxBlobSize: options.MaxBlobSize,
		Timeout:     options.Timeout,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		return result, err
	}
	defer repacker.Close()

	published := make(map[string]struct{}, len(plan.Groups)*2)
	for index, group := range plan.Groups {
		pack, manifestID, err := repacker.Publish(ctx, group.Entries)
		if err != nil {
			// Whatever was published already stays: it duplicates entries the
			// originals still serve, and the originals are still there.
			return result, fmt.Errorf("publish rebuilt pack %d of %d: %w", index+1, len(plan.Groups), err)
		}
		published[pack.Key] = struct{}{}
		published[server.ManifestKey(options.KeyPrefix, pack.ID, manifestID)] = struct{}{}
		result.Rebuilt++
		options.Log("published pack %s with %d entries under manifest %s", pack.ID, len(group.Entries), manifestID)
	}

	deleted, err := prune(ctx, options, layout, plan, published)
	result.Deleted = deleted
	return result, err
}

// prune removes the packs the plan replaced. A pack is only reachable through
// a manifest that names it, and a manifest whose pack is gone is skipped and
// left to expire, so the pack is what has to go.
//
// Packs are content-addressed, so a group that reproduces an existing pack byte
// for byte republishes under the key the plan is about to delete. Skipping keys
// this pass just wrote is what stops that from deleting live data.
func prune(ctx context.Context, options RunOptions, layout server.Layout, plan Plan, published map[string]struct{}) (int, error) {
	replaced := make(map[string]struct{}, len(plan.Rewrite)+len(plan.Dead))
	for _, id := range plan.Rewrite {
		replaced[id] = struct{}{}
	}
	for _, id := range plan.Dead {
		replaced[id] = struct{}{}
	}

	deleted := 0
	for _, pack := range layout.Packs {
		if _, drop := replaced[pack.ID]; !drop {
			continue
		}
		if _, republished := published[pack.Key]; republished {
			options.Log("keeping pack %s: the rebuild reproduced it exactly", pack.ID)
			continue
		}
		if err := options.Pruner.Delete(ctx, pack.Key); err != nil {
			return deleted, fmt.Errorf("delete replaced pack %s: %w", pack.ID, err)
		}
		deleted++
	}
	// Tidiness only, and safe in either order: an orphaned manifest is already
	// skipped by readers.
	for _, reference := range layout.Manifests {
		if _, drop := replaced[reference.Pack]; !drop {
			continue
		}
		if _, republished := published[reference.Key]; republished {
			continue
		}
		if err := options.Pruner.Delete(ctx, reference.Key); err != nil {
			options.Log("could not delete superseded manifest %s: %v", reference.ID, err)
		}
	}
	return deleted, nil
}
