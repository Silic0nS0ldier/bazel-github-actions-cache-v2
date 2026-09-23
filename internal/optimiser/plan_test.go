package optimiser

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

func demandFrom(t *testing.T, runs map[string][]server.UsageEntry) *Demand {
	t.Helper()
	demand := NewDemand()
	for run, entries := range runs {
		demand.Observe(run, server.UsageReport{Entries: entries})
	}
	return demand
}

func downloaded(kind, digest string) server.UsageEntry {
	return server.UsageEntry{Kind: kind, Digest: digest, Downloads: 1}
}

func checked(kind, digest string) server.UsageEntry {
	return server.UsageEntry{Kind: kind, Digest: digest, PresenceChecks: 1}
}

func entriesFor(pack string, kind string, count int, each int64) []Entry {
	entries := make([]Entry, 0, count)
	for index := range count {
		entries = append(entries, Entry{
			Kind:   kind,
			Digest: kind + "-" + pack + "-" + strconv.Itoa(index),
			Pack:   pack,
			Size:   each,
		})
	}
	return entries
}

func declare(id string, entries []Entry, extraDead int64) Pack {
	var bytes int64
	for _, entry := range entries {
		bytes += entry.Size
	}
	return Pack{
		ID:            id,
		StoredSize:    bytes + extraDead,
		DeclaredBytes: bytes + extraDead,
		DeclaredCount: len(entries),
	}
}

func testOptions() Options {
	options := DefaultOptions()
	options.TargetPackSize = 1000
	options.MinRuns = 1
	return options
}

// The whole pass exists to stop a restore paying for bytes nothing wanted.
func TestPlanSeparatesHotEntriesFromColdOnes(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 4, 100)
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", entries[0].Digest), downloaded("cas", entries[1].Digest)},
	})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rewrite) != 1 || plan.Rewrite[0] != "pack-a" {
		t.Fatalf("rewrite = %v", plan.Rewrite)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %+v, want a hot one and a cold one", plan.Groups)
	}
	for _, group := range plan.Groups {
		wantHot := group.Signature != ""
		for _, entry := range group.Entries {
			if demand.Downloaded(entry.Kind, entry.Digest) != wantHot {
				t.Fatalf("group %+v mixes hot and cold entries", group)
			}
		}
	}
	if plan.WastedBytes != 200 {
		t.Fatalf("wasted = %d, want 200", plan.WastedBytes)
	}
}

// A pack that is entirely hot, or entirely cold, is already segregated. Paying
// to rebuild it would be pure churn.
func TestPlanLeavesUnmixedPacksAlone(t *testing.T) {
	hot := entriesFor("pack-hot", "cas", 3, 100)
	cold := entriesFor("pack-cold", "cas", 3, 100)
	layout := Layout{
		Packs:   []Pack{declare("pack-hot", hot, 0), declare("pack-cold", cold, 0)},
		Entries: append(append([]Entry{}, hot...), cold...),
	}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", hot[0].Digest), downloaded("cas", hot[1].Digest), downloaded("cas", hot[2].Digest)},
	})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want no work", plan)
	}
	if len(plan.Keep) != 2 {
		t.Fatalf("keep = %v", plan.Keep)
	}
}

// A presence check is answered from the manifest without restoring the pack, so
// it costs a reader nothing and must not be treated as demand for the bytes.
func TestPlanTreatsPresenceOnlyEntriesAsCold(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 4, 100)
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {checked("cas", entries[0].Digest), checked("cas", entries[1].Digest)},
	})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want a wholly cold pack left alone", plan)
	}
}

// Entries wanted by the same jobs belong together; a later job then restores
// one pack instead of several.
func TestPlanGroupsEntriesWantedByTheSameRuns(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 4, 100)
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", entries[0].Digest), downloaded("cas", entries[2].Digest)},
		"run-2": {downloaded("cas", entries[1].Digest), downloaded("cas", entries[3].Digest)},
	})

	options := testOptions()
	options.TargetPackSize = 200
	plan, err := NewPlan(layout, demand, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %+v", plan.Groups)
	}
	for _, group := range plan.Groups {
		signatures := make(map[string]struct{})
		for _, entry := range group.Entries {
			signatures[demand.Signature(entry.Kind, entry.Digest)] = struct{}{}
		}
		if len(signatures) != 1 {
			t.Fatalf("group %+v mixes runs: %v", group.Entries, signatures)
		}
	}
}

// Bazel reads action results before fetching any output, so a pack of pure
// action results answers many lookups for one small restore.
func TestPlanNeverMixesKindsInOnePack(t *testing.T) {
	blobs := entriesFor("pack-a", "cas", 2, 100)
	actions := entriesFor("pack-a", "ac", 2, 10)
	all := append(append([]Entry{}, blobs...), actions...)
	layout := Layout{Packs: []Pack{declare("pack-a", all, 0)}, Entries: all}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", blobs[0].Digest), downloaded("ac", actions[0].Digest)},
	})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range plan.Groups {
		for _, entry := range group.Entries {
			if entry.Kind != group.Kind {
				t.Fatalf("group of kind %q holds %+v", group.Kind, entry)
			}
		}
	}
}

// A digest can end up in several packs, but only the lowest pack ID is ever
// served. A pack left holding nothing live is pure storage cost.
func TestPlanDeletesPacksWithNoLiveEntries(t *testing.T) {
	live := entriesFor("pack-a", "cas", 2, 100)
	shadowed := entriesFor("pack-b", "cas", 2, 100)
	layout := Layout{
		Packs:   []Pack{declare("pack-a", live, 0), declare("pack-b", shadowed, 0)},
		Entries: live,
	}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", live[0].Digest), downloaded("cas", live[1].Digest)},
	})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Dead) != 1 || plan.Dead[0] != "pack-b" {
		t.Fatalf("dead = %v", plan.Dead)
	}
	if len(plan.Groups) != 0 {
		t.Fatalf("a dead pack has nothing to rebuild, got %+v", plan.Groups)
	}
	if plan.ReclaimedBytes != 200 {
		t.Fatalf("reclaimed = %d, want 200", plan.ReclaimedBytes)
	}
}

// Deleting a pack is only safe once every entry it served has somewhere else to
// come from, so a plan that would drop one must not be returned at all.
func TestPlanRefusesToDropAnEntry(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 2, 100)
	plan := Plan{Groups: []Group{{Kind: "cas", Entries: entries[:1]}}}
	if err := plan.verify(entries); err == nil {
		t.Fatal("verify accepted a plan that drops an entry")
	}

	duplicated := Plan{Groups: []Group{
		{Kind: "cas", Entries: entries[:1]},
		{Kind: "cas", Entries: entries[:1]},
	}}
	if err := duplicated.verify(entries[:1]); err == nil {
		t.Fatal("verify accepted a plan that stores an entry twice")
	}
}

func TestPlanRewritesEveryEntryOfARewrittenPack(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 40, 100)
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	observed := make([]server.UsageEntry, 0)
	for index := 0; index < 10; index++ {
		observed = append(observed, downloaded("cas", entries[index].Digest))
	}
	demand := demandFrom(t, map[string][]server.UsageEntry{"run-1": observed})

	plan, err := NewPlan(layout, demand, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	placed := 0
	for _, group := range plan.Groups {
		if group.Bytes > testOptions().TargetPackSize {
			t.Fatalf("group of %d bytes exceeds the target", group.Bytes)
		}
		placed += len(group.Entries)
	}
	if placed != len(entries) {
		t.Fatalf("placed %d of %d entries", placed, len(entries))
	}
}

// One entry larger than the target still has to go somewhere.
func TestPlanPlacesAnOversizedEntry(t *testing.T) {
	entries := []Entry{
		{Kind: "cas", Digest: "big", Pack: "pack-a", Size: 5000},
		{Kind: "cas", Digest: "small", Pack: "pack-a", Size: 10},
	}
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", "big")},
	})

	options := testOptions()
	options.MinWasteFraction = 0
	plan, err := NewPlan(layout, demand, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rewrite) != 1 {
		t.Fatalf("rewrite = %v", plan.Rewrite)
	}
	if err := plan.verify(entries); err != nil {
		t.Fatal(err)
	}
}

// Planning from one job's record would chase that job's particular shape.
func TestPlanRefusesTooSmallAWindow(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 2, 100)
	layout := Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{"run-1": nil})

	options := testOptions()
	options.MinRuns = 3
	if _, err := NewPlan(layout, demand, options); err == nil {
		t.Fatal("planned from a single run")
	}
}

func TestPlanRejectsAnEntryNamingAnUnknownPack(t *testing.T) {
	entries := entriesFor("pack-a", "cas", 1, 100)
	layout := Layout{Packs: nil, Entries: entries}
	demand := demandFrom(t, map[string][]server.UsageEntry{"run-1": nil})

	_, err := NewPlan(layout, demand, testOptions())
	if err == nil || !strings.Contains(err.Error(), "unknown pack") {
		t.Fatalf("error = %v", err)
	}
}

func TestDemandSignatureNamesTheRunsThatDownloaded(t *testing.T) {
	demand := demandFrom(t, map[string][]server.UsageEntry{
		"run-2": {downloaded("cas", "a"), checked("cas", "b")},
		"run-1": {downloaded("cas", "a")},
	})
	if got := demand.Signature("cas", "a"); got != "run-1 run-2" {
		t.Fatalf("signature = %q", got)
	}
	if got := demand.Signature("cas", "b"); got != "" {
		t.Fatalf("a presence check is not demand for bytes, got %q", got)
	}
	if demand.Runs() != 2 {
		t.Fatalf("runs = %d", demand.Runs())
	}

}
