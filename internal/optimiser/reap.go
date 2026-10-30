package optimiser

import (
	"context"
	"fmt"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

// EntryLister reports listed cache entries with their creation time.
type EntryLister interface {
	ListEntries(ctx context.Context, keyPrefix string) ([]cache.CatalogEntry, error)
}

// reap deletes packs that no manifest names. Such a pack is unreachable: a
// reader only ever finds a pack through a manifest, so nothing can serve it.
// It reports how many packs passed the age filter and how many were deleted,
// which differ on a dry run.
//
// Two things make this safe enough to offer at all. Packs whose manifest merely
// failed to load are excluded by ReadLayout, because another ref may still be
// reading them. And an age threshold well beyond any job's lifetime separates a
// pack whose manifest was lost from one a running job has published but not yet
// committed a manifest for.
func reap(ctx context.Context, options RunOptions, layout server.Layout) (int, int, error) {
	keys := make([]string, 0, len(layout.UnmanifestedPacks))
	for _, pack := range layout.UnmanifestedPacks {
		keys = append(keys, pack.Key)
	}
	return reapKeys(ctx, options, server.PackKeyPrefix(options.KeyPrefix), "unmanifested packs", keys)
}

// reapManifests deletes manifests whose pack is gone. Readers already skip
// them, and they would expire unread within a week, but until then they take
// quota from a cache whose packs are being evicted for want of it. The age
// threshold covers a listing taken between a job publishing a pack and that
// pack appearing in the listing.
func reapManifests(ctx context.Context, options RunOptions, layout server.Layout) (int, int, error) {
	keys := make([]string, 0, len(layout.PacklessManifests))
	for _, manifest := range layout.PacklessManifests {
		keys = append(keys, manifest.Key)
	}
	return reapKeys(ctx, options, server.ManifestKeyPrefix(options.KeyPrefix), "manifests whose pack is gone", keys)
}

func reapKeys(ctx context.Context, options RunOptions, listPrefix, noun string, keys []string) (int, int, error) {
	if len(keys) == 0 {
		return 0, 0, nil
	}
	if options.Lister == nil {
		return 0, 0, fmt.Errorf("reaping needs a catalog that reports creation times")
	}
	if options.ReapOlderThan <= 0 {
		return 0, 0, fmt.Errorf("reaping needs a positive age threshold")
	}

	entries, err := options.Lister.ListEntries(ctx, listPrefix)
	if err != nil {
		return 0, 0, fmt.Errorf("list %s for reaping: %w", noun, err)
	}
	created := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		created[entry.Key] = entry.CreatedAt
	}

	cutoff := time.Now().Add(-options.ReapOlderThan)
	reapable := 0
	deleted := 0
	tooYoung := 0
	for _, key := range keys {
		at, known := created[key]
		// An entry with no creation time is left alone: without it there is no
		// way to tell it apart from one being written right now.
		if !known || at.IsZero() || at.After(cutoff) {
			tooYoung++
			continue
		}
		reapable++
		if options.DryRun {
			continue
		}
		if err := options.Pruner.Delete(ctx, key); err != nil {
			return reapable, deleted, fmt.Errorf("reap %s: %w", key, err)
		}
		deleted++
	}
	if tooYoung > 0 {
		options.Log("left %d %s alone: newer than %s, or with no known creation time",
			tooYoung, noun, options.ReapOlderThan)
	}
	return reapable, deleted, nil
}
