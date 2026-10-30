package optimiser

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

type recordingPruner struct {
	deleted []string
	err     error
}

func (p *recordingPruner) Delete(_ context.Context, key string) error {
	if p.err != nil {
		return p.err
	}
	p.deleted = append(p.deleted, key)
	return nil
}

func layoutWith(packs []server.LayoutPack, manifests []server.LayoutManifest) server.Layout {
	return server.Layout{Packs: packs, Manifests: manifests}
}

// A scheduled pass runs whether or not the repository was busy, so having too
// little to plan from has to be a clean no-op rather than a red workflow.
func TestTooFewRecordsIsReportedNotFailed(t *testing.T) {
	demand := NewDemand()
	demand.Observe("run-1", server.UsageReport{})

	_, err := NewPlan(server.Layout{}, demand, Options{TargetPackSize: 1024, MinRuns: 3})
	if !errors.Is(err, errTooFewRuns) {
		t.Fatalf("error = %v, want errTooFewRuns", err)
	}
}

// Packs are content addressed, so a rebuild that reproduces one exactly
// republishes under the very key about to be deleted.
func TestRemoverKeepsAPackTheRebuildReproduced(t *testing.T) {
	pruner := &recordingPruner{}
	layout := layoutWith(
		[]server.LayoutPack{
			{ID: "same", Key: "prefix-car-pack-v1-same"},
			{ID: "other", Key: "prefix-car-pack-v1-other"},
		},
		[]server.LayoutManifest{
			{ID: "m1", Key: "prefix-car-manifest-v2-same-m1", Pack: "same"},
			{ID: "m2", Key: "prefix-car-manifest-v2-other-m2", Pack: "other"},
		},
	)
	options := RunOptions{Pruner: pruner, Log: func(string, ...any) {}}
	remover := newRemover(context.Background(), options, layout)
	remover.republished("prefix-car-pack-v1-same")

	for _, packID := range []string{"same", "other"} {
		if err := remover.remove(packID); err != nil {
			t.Fatal(err)
		}
	}
	if remover.deleted != 1 {
		t.Fatalf("deleted %d packs", remover.deleted)
	}
	for _, key := range pruner.deleted {
		if key == "prefix-car-pack-v1-same" {
			t.Fatal("deleted the pack the rebuild had just republished")
		}
	}
}

func TestRemoverTakesTheManifestsDescribingAPack(t *testing.T) {
	pruner := &recordingPruner{}
	layout := layoutWith(
		[]server.LayoutPack{
			{ID: "kept", Key: "prefix-car-pack-v1-kept"},
			{ID: "dead", Key: "prefix-car-pack-v1-dead"},
		},
		[]server.LayoutManifest{
			{ID: "m1", Key: "prefix-car-manifest-v2-kept-m1", Pack: "kept"},
			{ID: "m2", Key: "prefix-car-manifest-v2-dead-m2", Pack: "dead"},
		},
	)
	options := RunOptions{Pruner: pruner, Log: func(string, ...any) {}}
	remover := newRemover(context.Background(), options, layout)
	if err := remover.remove("dead"); err != nil {
		t.Fatal(err)
	}
	for _, key := range pruner.deleted {
		if key == "prefix-car-pack-v1-kept" || key == "prefix-car-manifest-v2-kept-m1" {
			t.Fatalf("deleted %s, which was not being replaced", key)
		}
	}
	if len(pruner.deleted) != 2 {
		t.Fatalf("deleted %v", pruner.deleted)
	}
}

// A pack that will not delete must stop the pass rather than leave the rest of
// the old layout half removed under a silent failure.
func TestRemoverStopsOnAFailedPackDeletion(t *testing.T) {
	pruner := &recordingPruner{err: errors.New("no permission")}
	layout := layoutWith(
		[]server.LayoutPack{{ID: "dead", Key: "prefix-car-pack-v1-dead"}},
		nil,
	)
	options := RunOptions{Pruner: pruner, Log: func(string, ...any) {}}
	remover := newRemover(context.Background(), options, layout)
	if err := remover.remove("dead"); err == nil {
		t.Fatal("a failed deletion was ignored")
	}
}

// fakePublisher publishes instantly or after a delay, and records how many
// publications overlapped.
type fakePublisher struct {
	mu      sync.Mutex
	delay   time.Duration
	failAt  int
	calls   int
	running int
	peak    int
}

func (p *fakePublisher) Publish(ctx context.Context, entries []server.LayoutEntry, _ time.Time) (server.LayoutPack, string, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.running++
	p.peak = max(p.peak, p.running)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}()
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return server.LayoutPack{}, "", ctx.Err()
	}
	if call == p.failAt {
		return server.LayoutPack{}, "", errors.New("upload refused")
	}
	var size int64
	for _, entry := range entries {
		size += entry.Size
	}
	return server.LayoutPack{ID: entries[0].Digest, Key: "pack-" + entries[0].Digest, StoredSize: size}, "manifest", nil
}

func groupsOf(count int, size int64) []Group {
	groups := make([]Group, 0, count)
	for index := range count {
		entries := entriesFor("source-"+strconv.Itoa(index), "cas", 1, size)
		groups = append(groups, Group{Kind: "cas", Entries: entries, Bytes: size})
	}
	return groups
}

// A pack mostly waits on the network, so rebuilding them one at a time is what
// made a pass take most of half an hour.
func TestPublishGroupsRunsSeveralAtOnce(t *testing.T) {
	publisher := &fakePublisher{delay: 20 * time.Millisecond}
	options := RunOptions{Concurrency: 4, Log: func(string, ...any) {}}
	var incomplete bool
	var finished int
	published, err := publishGroups(context.Background(), options, groupsOf(12, 100), publisher,
		func(Group, server.LayoutPack, string) error { finished++; return nil }, &incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if finished != 12 || published != 1200 || incomplete {
		t.Fatalf("finished %d, published %d, incomplete %v", finished, published, incomplete)
	}
	if publisher.peak != 4 {
		t.Fatalf("at most %d publications overlapped, want 4", publisher.peak)
	}
}

// Work in flight counts against the budget, or every worker would start just
// under it and overshoot together.
func TestPublishGroupsStopsAtTheByteBudget(t *testing.T) {
	publisher := &fakePublisher{delay: 5 * time.Millisecond}
	options := RunOptions{Concurrency: 4, MaxNewBytes: 250, Log: func(string, ...any) {}}
	var incomplete bool
	published, err := publishGroups(context.Background(), options, groupsOf(10, 100), publisher,
		func(Group, server.LayoutPack, string) error { return nil }, &incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete || publisher.calls != 3 || published != 300 {
		t.Fatalf("calls %d, published %d, incomplete %v; want 3 packs then a stop", publisher.calls, published, incomplete)
	}
}

// Nothing may be deleted on the strength of a publication that failed, and no
// more work starts once one has.
func TestPublishGroupsStopsOnAFailure(t *testing.T) {
	publisher := &fakePublisher{delay: 5 * time.Millisecond, failAt: 3}
	options := RunOptions{Concurrency: 2, Log: func(string, ...any) {}}
	var incomplete bool
	var done []string
	_, err := publishGroups(context.Background(), options, groupsOf(20, 100), publisher,
		func(_ Group, pack server.LayoutPack, _ string) error { done = append(done, pack.ID); return nil }, &incomplete)
	if err == nil || !strings.Contains(err.Error(), "upload refused") {
		t.Fatalf("error = %v", err)
	}
	// Each worker can have started one more before the failure was seen.
	if publisher.calls > 3+options.Concurrency {
		t.Fatalf("%d publications started, want dispatch to stop at the failure", publisher.calls)
	}
	if len(done) > 2+options.Concurrency {
		t.Fatalf("%d publications were acted on after the failure", len(done))
	}
}
