package optimiser

import (
	"context"
	"errors"
	"testing"

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
