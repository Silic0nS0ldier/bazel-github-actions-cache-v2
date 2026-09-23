package optimiser

import (
	"context"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

type fakeLister struct {
	entries []cache.CatalogEntry
}

func (l fakeLister) ListEntries(context.Context, string) ([]cache.CatalogEntry, error) {
	return l.entries, nil
}

func reapOptions(pruner Pruner, entries []cache.CatalogEntry) RunOptions {
	return RunOptions{
		Pruner:        pruner,
		Lister:        fakeLister{entries: entries},
		KeyPrefix:     "prefix",
		ReapOlderThan: 24 * time.Hour,
		Log:           func(string, ...any) {},
	}
}

func TestReapRemovesOldPacksNoManifestNames(t *testing.T) {
	pruner := &recordingPruner{}
	layout := server.Layout{UnmanifestedPacks: []server.LayoutPack{
		{ID: "old", Key: "prefix-car-pack-v1-old"},
	}}
	options := reapOptions(pruner, []cache.CatalogEntry{
		{Key: "prefix-car-pack-v1-old", CreatedAt: time.Now().Add(-72 * time.Hour)},
	})

	reapable, reaped, err := reap(context.Background(), options, layout)
	if err != nil {
		t.Fatal(err)
	}
	if reapable != 1 || reaped != 1 || len(pruner.deleted) != 1 {
		t.Fatalf("reapable %d, reaped %d, deleted %v", reapable, reaped, pruner.deleted)
	}
}

// A pack a running job has published but not yet committed a manifest for looks
// exactly like one whose manifest was lost. Only age tells them apart.
func TestReapLeavesRecentPacksAlone(t *testing.T) {
	pruner := &recordingPruner{}
	layout := server.Layout{UnmanifestedPacks: []server.LayoutPack{
		{ID: "fresh", Key: "prefix-car-pack-v1-fresh"},
	}}
	options := reapOptions(pruner, []cache.CatalogEntry{
		{Key: "prefix-car-pack-v1-fresh", CreatedAt: time.Now().Add(-time.Minute)},
	})

	reapable, reaped, err := reap(context.Background(), options, layout)
	if err != nil {
		t.Fatal(err)
	}
	if reapable != 0 || reaped != 0 || len(pruner.deleted) != 0 {
		t.Fatalf("reapable %d, reaped %d, deleted %v", reapable, reaped, pruner.deleted)
	}
}

// Without a creation time there is no way to tell a lost pack from one being
// written right now.
func TestReapLeavesPacksWithNoKnownAge(t *testing.T) {
	pruner := &recordingPruner{}
	layout := server.Layout{UnmanifestedPacks: []server.LayoutPack{
		{ID: "unknown", Key: "prefix-car-pack-v1-unknown"},
	}}

	for _, entries := range [][]cache.CatalogEntry{
		nil,
		{{Key: "prefix-car-pack-v1-unknown"}},
	} {
		reapable, reaped, err := reap(context.Background(), reapOptions(pruner, entries), layout)
		if err != nil {
			t.Fatal(err)
		}
		if reapable != 0 || reaped != 0 || len(pruner.deleted) != 0 {
			t.Fatalf("reapable %d, reaped %d, deleted %v", reapable, reaped, pruner.deleted)
		}
	}
}

// A pack whose manifest exists but could not be read is not unmanifested:
// another ref may still be reading it, so it never reaches the reaper.
func TestReapOnlyEverSeesUnmanifestedPacks(t *testing.T) {
	pruner := &recordingPruner{}
	layout := server.Layout{
		Packs:               []server.LayoutPack{{ID: "live", Key: "prefix-car-pack-v1-live"}},
		OrphanPacks:         5,
		UnreadableManifests: 5,
	}
	options := reapOptions(pruner, []cache.CatalogEntry{
		{Key: "prefix-car-pack-v1-live", CreatedAt: time.Now().Add(-72 * time.Hour)},
	})

	reapable, reaped, err := reap(context.Background(), options, layout)
	if err != nil {
		t.Fatal(err)
	}
	if reapable != 0 || reaped != 0 || len(pruner.deleted) != 0 {
		t.Fatalf("reapable %d, reaped %d, deleted %v", reapable, reaped, pruner.deleted)
	}
}

// A dry run has to apply the age filter, or its count says nothing about what a
// real pass would remove.
func TestReapCountsCandidatesOnADryRunWithoutDeleting(t *testing.T) {
	pruner := &recordingPruner{}
	layout := server.Layout{UnmanifestedPacks: []server.LayoutPack{
		{ID: "old", Key: "prefix-car-pack-v1-old"},
		{ID: "fresh", Key: "prefix-car-pack-v1-fresh"},
	}}
	options := reapOptions(pruner, []cache.CatalogEntry{
		{Key: "prefix-car-pack-v1-old", CreatedAt: time.Now().Add(-72 * time.Hour)},
		{Key: "prefix-car-pack-v1-fresh", CreatedAt: time.Now().Add(-time.Minute)},
	})
	options.DryRun = true

	reapable, reaped, err := reap(context.Background(), options, layout)
	if err != nil {
		t.Fatal(err)
	}
	if reapable != 1 {
		t.Fatalf("reapable = %d, want the one old enough", reapable)
	}
	if reaped != 0 || len(pruner.deleted) != 0 {
		t.Fatalf("a dry run deleted %d packs: %v", reaped, pruner.deleted)
	}
}

func TestReapRefusesWithoutAnAgeThreshold(t *testing.T) {
	layout := server.Layout{UnmanifestedPacks: []server.LayoutPack{
		{ID: "old", Key: "prefix-car-pack-v1-old"},
	}}
	options := reapOptions(&recordingPruner{}, nil)
	options.ReapOlderThan = 0
	if _, _, err := reap(context.Background(), options, layout); err == nil {
		t.Fatal("reaped without an age threshold")
	}
}
