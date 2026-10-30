package optimiser

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	// Concurrency is how many packs are rebuilt at once.
	Concurrency int
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
	// EvictAfter is how long an entry may go unused before it is evicted from
	// its pack. Reshaping waits for packs twice as old, so that a pack is
	// cleaned in place a few times between regroupings. Zero disables both
	// age gates and eviction.
	EvictAfter time.Duration
	// Lister reports cache entries' creation times, which stand in for a
	// pack's age when its manifest does not record one.
	Lister EntryLister
	Plan   Options
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
	// OverfetchBefore and OverfetchAfter are what an average observed job
	// downloaded and did not want from the rebuilt packs, and would from their
	// replacements.
	OverfetchBefore int64
	OverfetchAfter  int64
	// DownloadsBefore and DownloadsAfter are how many of those packs an average
	// observed job restored, and would restore.
	DownloadsBefore float64
	DownloadsAfter  float64
	// Unimproved counts wasteful packs left alone because no regrouping found
	// would have cut their restore cost enough to be worth the upload.
	Unimproved int
	// RecordsInWindow counts usage records inside the eviction window, and
	// EvictionSkipped says why nothing was evicted when the records were
	// incomplete.
	RecordsInWindow int
	EvictionSkipped string
	// Evicted counts packs rewritten on their own to drop EvictedEntries stale
	// entries holding EvictedBytes. Abandoned counts packs every entry of which
	// is stale, left for GitHub to expire.
	Evicted        int
	EvictedEntries int
	EvictedBytes   int64
	Abandoned      int
	// ReshapeEligible counts packs old enough to reshape; ReshapeSkipped says
	// why reshaping did not run, when it did not.
	ReshapeEligible int
	ReshapeSkipped  string
	DryRun          bool
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
	// OrphanManifests counts manifests whose pack is gone; ReapableManifests
	// and ReapedManifests are those old enough to reap, and those reaped.
	OrphanManifests   int
	ReapableManifests int
	ReapedManifests   int
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

	now := time.Now()
	timing := Timing{Now: now}
	window := Window{}
	if options.EvictAfter > 0 {
		timing.EvictAfter = options.EvictAfter
		timing.ReshapeAfter = 2 * options.EvictAfter
		timing.Ages, err = packAges(ctx, options, layout)
		if err != nil {
			return Result{}, err
		}
		window = Window{Since: now.Add(-options.EvictAfter), Recent: NewDemand()}
	}
	demand := NewDemand()
	records, coverage, err := options.Records.CollectWindow(ctx, demand, options.MaxRecords, window, options.Log)
	if err != nil {
		return Result{}, fmt.Errorf("collect usage records: %w", err)
	}

	result := Result{
		Records:         records,
		RecordsInWindow: coverage.Records,
		Packs:           len(layout.Packs),
		Entries:         len(layout.Entries),
		Unreadable:      layout.UnreadableManifests,
		Unmanifested:    len(layout.UnmanifestedPacks),
		OrphanManifests: layout.OrphanManifests,
		DryRun:          options.DryRun,
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

		// Readers already skip these, so failing to remove one costs only the
		// quota it holds until it expires; it is no reason to stop the pass.
		reapable, reaped, err = reapManifests(ctx, options, layout)
		result.ReapableManifests = reapable
		result.ReapedManifests = reaped
		switch {
		case err != nil:
			options.Log("could not reap every manifest whose pack is gone: %v", err)
		case options.DryRun:
			options.Log("%d manifests whose pack is gone are old enough to reap", reapable)
		default:
			options.Log("reaped %d manifests whose pack is gone", reaped)
		}
	}
	if options.EvictAfter > 0 {
		if coverage.Complete {
			timing.Recent = window.Recent
		} else {
			result.EvictionSkipped = coverage.Gap
			options.Log("evicting nothing: %s, so an entry that looks unused may not be", coverage.Gap)
		}
	}
	plan, err := NewTimedPlan(layout, demand, options.Plan, timing)
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
	result.OverfetchBefore = plan.OverfetchBefore
	result.OverfetchAfter = plan.OverfetchAfter
	result.DownloadsBefore = plan.DownloadsBefore
	result.DownloadsAfter = plan.DownloadsAfter
	result.Unimproved = plan.Unimproved
	result.Evicted = plan.Evicted
	result.EvictedEntries = plan.EvictedEntries
	result.EvictedBytes = plan.EvictedBytes
	result.Abandoned = plan.Abandoned
	result.ReshapeEligible = plan.ReshapeEligible
	result.ReshapeSkipped = plan.ReshapeSkipped
	if plan.Abandoned > 0 {
		options.Log("left %d packs alone that nothing has used for %s; GitHub expires them unread", plan.Abandoned, options.EvictAfter)
	}
	if plan.ReshapeSkipped != "" {
		options.Log("not reshaping: %s", plan.ReshapeSkipped)
	}
	if plan.Unimproved > 0 {
		options.Log("left %d wasteful packs alone: no regrouping found would cut their overfetch by %.0f%% for jobs it was not fitted to",
			plan.Unimproved, options.Plan.MinImprovement*100)
	}
	if plan.Empty() {
		result.Skipped = "every pack is already grouped for how it is read"
		options.Log("%s; nothing to do", result.Skipped)
		return result, nil
	}
	options.Log("evicting %s stale entries (%s) from %d packs, each rewritten on its own",
		HumanCount(int64(plan.EvictedEntries)), HumanBytes(plan.EvictedBytes), plan.Evicted)
	options.Log("reshaping %d of %d packs old enough into %d; per job, overfetch %s -> %s and downloads %.1f -> %.1f",
		len(plan.Rewrite)-plan.Evicted, plan.ReshapeEligible, len(plan.Groups)-plan.Evicted,
		HumanBytes(plan.OverfetchBefore), HumanBytes(plan.OverfetchAfter), plan.DownloadsBefore, plan.DownloadsAfter)
	options.Log("deleting %d empty packs; %s reclaimed", len(plan.Dead), HumanBytes(plan.ReclaimedBytes))
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

	newBytes, err := publishGroups(ctx, options, plan.Groups, repacker, func(group Group, pack server.LayoutPack, manifestID string) error {
		remover.republished(pack.Key)
		remover.republished(server.ManifestKey(options.KeyPrefix, pack.ID, manifestID))
		result.Rebuilt++
		if group.Evicts != "" {
			options.Log("published pack %s with %d entries under manifest %s, replacing %s without its stale entries",
				pack.ID, len(group.Entries), manifestID, group.Evicts)
		} else {
			options.Log("published pack %s with %d entries under manifest %s", pack.ID, len(group.Entries), manifestID)
		}
		for _, entry := range group.Entries {
			outstanding[entry.Pack]--
			if outstanding[entry.Pack] > 0 {
				continue
			}
			delete(outstanding, entry.Pack)
			repacker.Release(entry.Pack)
			if err := remover.remove(entry.Pack); err != nil {
				return err
			}
		}
		return nil
	}, &result.Incomplete)
	result.Deleted = remover.deleted
	result.PublishedBytes = newBytes
	return result, err
}

// packAges is when each pack's contents took their current shape: what its
// manifest records, or failing that when its cache entry was created. A pack
// with neither is left out, and so treated as brand new.
func packAges(ctx context.Context, options RunOptions, layout server.Layout) (map[string]time.Time, error) {
	ages := make(map[string]time.Time, len(layout.Packs))
	var created map[string]time.Time
	for _, pack := range layout.Packs {
		if !pack.Since.IsZero() {
			ages[pack.ID] = pack.Since
			continue
		}
		if created == nil {
			created = make(map[string]time.Time)
			if options.Lister != nil {
				entries, err := options.Lister.ListEntries(ctx, server.PackKeyPrefix(options.KeyPrefix))
				if err != nil {
					return nil, fmt.Errorf("list packs for their ages: %w", err)
				}
				for _, entry := range entries {
					created[entry.Key] = entry.CreatedAt
				}
			}
		}
		if at, known := created[pack.Key]; known && !at.IsZero() {
			ages[pack.ID] = at
		}
	}
	return ages, nil
}

// publisher is the part of the repacker publishGroups drives.
type publisher interface {
	Publish(ctx context.Context, entries []server.LayoutEntry, since time.Time) (server.LayoutPack, string, error)
}

// publishGroups builds and uploads groups on several workers at once, since a
// pack spends most of its time waiting on downloads and uploads rather than on
// the upload rate limit. done runs on this goroutine, one group at a time, so
// whatever it deletes is decided by completed publications only. It stops
// dispatching once the byte budget is spent or anything fails, and lets work
// already started finish.
func publishGroups(
	ctx context.Context,
	options RunOptions,
	groups []Group,
	repacker publisher,
	done func(Group, server.LayoutPack, string) error,
	incomplete *bool,
) (int64, error) {
	workers := max(options.Concurrency, 1)
	publishCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		index      int
		pack       server.LayoutPack
		manifestID string
		err        error
	}
	jobs := make(chan int)
	results := make(chan outcome, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				pack, manifestID, err := repacker.Publish(publishCtx, groups[index].Entries, groups[index].Since)
				results <- outcome{index: index, pack: pack, manifestID: manifestID, err: err}
			}
		}()
	}
	defer func() {
		close(jobs)
		wait.Wait()
	}()

	var newBytes, pendingBytes int64
	var failure error
	next, running := 0, 0
	for {
		for failure == nil && running < workers && next < len(groups) {
			// Counting work in flight keeps parallel workers from all starting
			// just under the budget and overshooting it together.
			if options.MaxNewBytes > 0 && newBytes+pendingBytes >= options.MaxNewBytes {
				*incomplete = true
				break
			}
			jobs <- next
			pendingBytes += groups[next].Bytes
			next++
			running++
		}
		if running == 0 {
			break
		}
		finished := <-results
		running--
		pendingBytes -= groups[finished.index].Bytes
		if failure != nil {
			// Anything still finishing after a failure only duplicates entries
			// the originals still serve, so it is left in place untouched.
			continue
		}
		if finished.err != nil {
			// Whatever was published already stays: it duplicates entries the
			// originals still serve, and the originals are still there.
			failure = fmt.Errorf("publish rebuilt pack %d of %d: %w", finished.index+1, len(groups), finished.err)
			cancel()
			continue
		}
		newBytes += finished.pack.StoredSize
		if err := done(groups[finished.index], finished.pack, finished.manifestID); err != nil {
			failure = err
			cancel()
		}
	}
	if *incomplete && failure == nil {
		// Stopping early is safe: the packs still to be rebuilt were never
		// touched, so they keep serving. The next pass re-plans.
		options.Log("stopping after %d of %d packs, having published %s this pass",
			next, len(groups), HumanBytes(newBytes))
	}
	return newBytes, failure
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
