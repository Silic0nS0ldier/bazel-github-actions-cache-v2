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
//
// Two things make this safe enough to offer at all. Packs whose manifest merely
// failed to load are excluded by ReadLayout, because another ref may still be
// reading them. And an age threshold well beyond any job's lifetime separates a
// pack whose manifest was lost from one a running job has published but not yet
// committed a manifest for.
func reap(ctx context.Context, options RunOptions, layout server.Layout) (int, error) {
	if len(layout.UnmanifestedPacks) == 0 {
		return 0, nil
	}
	if options.Lister == nil {
		return 0, fmt.Errorf("reaping needs a catalog that reports creation times")
	}
	if options.ReapOlderThan <= 0 {
		return 0, fmt.Errorf("reaping needs a positive age threshold")
	}

	entries, err := options.Lister.ListEntries(ctx, server.PackKeyPrefix(options.KeyPrefix))
	if err != nil {
		return 0, fmt.Errorf("list packs for reaping: %w", err)
	}
	created := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		created[entry.Key] = entry.CreatedAt
	}

	cutoff := time.Now().Add(-options.ReapOlderThan)
	reaped := 0
	tooYoung := 0
	for _, pack := range layout.UnmanifestedPacks {
		at, known := created[pack.Key]
		// An entry with no creation time is left alone: without it there is no
		// way to tell it apart from one being written right now.
		if !known || at.IsZero() || at.After(cutoff) {
			tooYoung++
			continue
		}
		if err := options.Pruner.Delete(ctx, pack.Key); err != nil {
			return reaped, fmt.Errorf("reap pack %s: %w", pack.ID, err)
		}
		reaped++
	}
	if tooYoung > 0 {
		options.Log("left %d unmanifested packs alone: newer than %s, or with no known creation time",
			tooYoung, options.ReapOlderThan)
	}
	return reaped, nil
}
