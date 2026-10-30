package optimiser

import (
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

const day = 24 * time.Hour

type timedFixture struct {
	now    time.Time
	layout Layout
	ages   map[string]time.Time
}

func newTimedFixture() *timedFixture {
	return &timedFixture{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), ages: make(map[string]time.Time)}
}

// pack adds a pack of count entries aged the given number of days.
func (f *timedFixture) pack(id string, count int, age time.Duration) []Entry {
	entries := entriesFor(id, "cas", count, 100)
	f.layout.Packs = append(f.layout.Packs, declare(id, entries, 0))
	f.layout.Entries = append(f.layout.Entries, entries...)
	f.ages[id] = f.now.Add(-age)
	return entries
}

func (f *timedFixture) timing(recent *Demand) Timing {
	return Timing{Now: f.now, Ages: f.ages, EvictAfter: 7 * day, ReshapeAfter: 14 * day, Recent: recent}
}

func usedBy(entries ...Entry) []server.UsageEntry {
	used := make([]server.UsageEntry, 0, len(entries))
	for _, entry := range entries {
		used = append(used, downloaded(entry.Kind, entry.Digest))
	}
	return used
}

// A pack holding a little dead weight is cleaned on its own: nothing else
// moves, and it keeps its age so that reshaping still reaches it in time.
func TestTimedPlanEvictsStaleEntriesFromOnePackAlone(t *testing.T) {
	fixture := newTimedFixture()
	mixed := fixture.pack("pack-a", 3, 10*day)
	clean := fixture.pack("pack-b", 2, 10*day)
	recent := demandFrom(t, map[string][]server.UsageEntry{"run-1": usedBy(mixed[0], mixed[1], clean[0], clean[1])})

	plan, err := NewTimedPlan(fixture.layout, recent, testOptions(), fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rewrite) != 1 || plan.Rewrite[0] != "pack-a" || len(plan.Keep) != 1 || plan.Keep[0] != "pack-b" {
		t.Fatalf("rewrite %v, keep %v; want only the pack with stale entries rewritten", plan.Rewrite, plan.Keep)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("groups = %+v", plan.Groups)
	}
	evicted := plan.Groups[0]
	if evicted.Evicts != "pack-a" || len(evicted.Entries) != 2 || !evicted.Since.Equal(fixture.ages["pack-a"]) {
		t.Fatalf("group = %+v, want pack-a's two used entries at its original age", evicted)
	}
	if plan.Evicted != 1 || plan.EvictedEntries != 1 || plan.EvictedBytes != 100 || plan.ReclaimedBytes != 100 {
		t.Fatalf("evicted %d packs, %d entries, %d bytes; reclaimed %d",
			plan.Evicted, plan.EvictedEntries, plan.EvictedBytes, plan.ReclaimedBytes)
	}
}

// Presence checks are how a job, or a reader validating an action result,
// depends on an entry without downloading it.
func TestTimedPlanKeepsEntriesOnlyCheckedForPresence(t *testing.T) {
	fixture := newTimedFixture()
	entries := fixture.pack("pack-a", 2, 10*day)
	recent := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": {downloaded("cas", entries[0].Digest), checked("cas", entries[1].Digest)},
	})

	plan, err := NewTimedPlan(fixture.layout, recent, testOptions(), fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want nothing evicted", plan)
	}
}

// Entries written less than the eviction threshold ago may simply not have
// been needed yet.
func TestTimedPlanLeavesYoungPacksAlone(t *testing.T) {
	fixture := newTimedFixture()
	entries := fixture.pack("pack-a", 3, 3*day)
	recent := demandFrom(t, map[string][]server.UsageEntry{"run-1": usedBy(entries[0])})

	plan, err := NewTimedPlan(fixture.layout, recent, testOptions(), fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want a young pack left alone", plan)
	}
}

// Nothing restores a pack every entry of which is stale, so GitHub expires it.
// Rewriting it would only renew it.
func TestTimedPlanLeavesWhollyStalePacksToExpire(t *testing.T) {
	fixture := newTimedFixture()
	fixture.pack("pack-a", 3, 10*day)
	used := fixture.pack("pack-b", 1, 10*day)
	recent := demandFrom(t, map[string][]server.UsageEntry{"run-1": usedBy(used...)})

	plan, err := NewTimedPlan(fixture.layout, recent, testOptions(), fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || plan.Abandoned != 1 {
		t.Fatalf("plan = %+v, want the stale pack abandoned untouched", plan)
	}
}

// Without every record in the window, an entry that looks unused may have been
// used by a record that could not be read.
func TestTimedPlanEvictsNothingWithoutCompleteRecords(t *testing.T) {
	fixture := newTimedFixture()
	entries := fixture.pack("pack-a", 3, 10*day)
	demand := demandFrom(t, map[string][]server.UsageEntry{"run-1": usedBy(entries[0])})

	plan, err := NewTimedPlan(fixture.layout, demand, testOptions(), fixture.timing(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want no eviction", plan)
	}
}

// One pack old enough to reshape can only have its contents shuffled, so it is
// not reshaped, though its stale entries still go.
func TestTimedPlanDoesNotReshapeASinglePack(t *testing.T) {
	fixture := newTimedFixture()
	old := fixture.pack("pack-a", 4, 20*day)
	young := fixture.pack("pack-b", 2, 2*day)
	// Run-1 and run-2 want different halves of the old pack, which reshaping
	// would split, and nothing wants its last entry.
	recent := demandFrom(t, map[string][]server.UsageEntry{
		"run-1": usedBy(old[0], young[0], young[1]),
		"run-2": usedBy(old[1], old[2]),
	})

	plan, err := NewTimedPlan(fixture.layout, recent, testOptions(), fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReshapeEligible != 1 || plan.ReshapeSkipped == "" {
		t.Fatalf("eligible %d, skipped %q", plan.ReshapeEligible, plan.ReshapeSkipped)
	}
	if len(plan.Groups) != 1 || plan.Groups[0].Evicts != "pack-a" || len(plan.Groups[0].Entries) != 3 {
		t.Fatalf("groups = %+v, want pack-a evicted in place", plan.Groups)
	}
}

// Reshaping regroups only packs old enough, merges small packs restored by the
// same jobs to save their downloads, and starts the result's age afresh.
func TestTimedPlanReshapesOldPacksIntoFewerDownloads(t *testing.T) {
	fixture := newTimedFixture()
	first := fixture.pack("pack-a", 2, 20*day)
	second := fixture.pack("pack-b", 2, 20*day)
	young := fixture.pack("pack-c", 2, 2*day)
	runs := make(map[string][]server.UsageEntry)
	for _, run := range []string{"run-1", "run-2", "run-3", "run-4"} {
		runs[run] = usedBy(append(append(append([]Entry(nil), first...), second...), young...)...)
	}
	recent := demandFrom(t, runs)

	options := testOptions()
	options.DownloadOverhead = 1000
	plan, err := NewTimedPlan(fixture.layout, recent, options, fixture.timing(recent))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rewrite) != 2 || len(plan.Groups) != 1 || len(plan.Groups[0].Entries) != 4 {
		t.Fatalf("rewrite %v into %+v, want the two old packs merged", plan.Rewrite, plan.Groups)
	}
	if !plan.Groups[0].Since.IsZero() || plan.Groups[0].Evicts != "" {
		t.Fatalf("group = %+v, want a reshaped pack with a fresh age", plan.Groups[0])
	}
	if plan.DownloadsBefore != 2 || plan.DownloadsAfter != 1 {
		t.Fatalf("downloads per job %v -> %v, want 2 -> 1", plan.DownloadsBefore, plan.DownloadsAfter)
	}
	for _, kept := range plan.Keep {
		if kept == "pack-c" {
			return
		}
	}
	t.Fatalf("keep = %v, want the young pack untouched", plan.Keep)
}

// However tiny the entries, a pack's manifest has to stay within what readers
// accept.
func TestPlanCapsEntriesPerPack(t *testing.T) {
	entries := entriesFor("pack-a", "cas", maxGroupEntries+10, 1)
	layout := withBystander(Layout{Packs: []Pack{declare("pack-a", entries, 0)}, Entries: entries})
	demand := demandFrom(t, map[string][]server.UsageEntry{"run-1": usedBy(entries[:5]...)})

	options := testOptions()
	options.TargetPackSize = 1 << 30
	plan, err := NewPlan(layout, demand, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) == 0 {
		t.Fatal("nothing was regrouped")
	}
	for _, group := range plan.Groups {
		if len(group.Entries) > maxGroupEntries {
			t.Fatalf("a group holds %d entries", len(group.Entries))
		}
	}
}
