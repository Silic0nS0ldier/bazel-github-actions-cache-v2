package optimiser

import (
	"sort"
	"strings"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

// Demand is what a window of recent jobs did with each cache entry. It is the
// only thing the manifests cannot say: pack composition can be read back from
// them, but only a job knows which entries it needed.
type Demand struct {
	downloads map[string]map[string]struct{}
	presence  map[string]uint64
	runs      map[string]struct{}
}

func NewDemand() *Demand {
	return &Demand{
		downloads: make(map[string]map[string]struct{}),
		presence:  make(map[string]uint64),
		runs:      make(map[string]struct{}),
	}
}

func demandKey(kind, digest string) string { return kind + "/" + digest }

// Observe folds one job's usage record into the demand. Keying downloads by run
// is what lets the planner tell entries that are wanted together from entries
// that merely happen to be popular.
func (d *Demand) Observe(runID string, report server.UsageReport) {
	if runID == "" {
		return
	}
	d.runs[runID] = struct{}{}
	for _, entry := range report.Entries {
		key := demandKey(entry.Kind, entry.Digest)
		d.presence[key] += entry.PresenceChecks
		if entry.Downloads == 0 {
			continue
		}
		runs, found := d.downloads[key]
		if !found {
			runs = make(map[string]struct{})
			d.downloads[key] = runs
		}
		runs[runID] = struct{}{}
	}
}

// Runs counts the jobs that contributed a record. A plan built from too few is
// a plan built from noise.
func (d *Demand) Runs() int { return len(d.runs) }

// Downloaded reports whether any observed job needed the entry's bytes. A
// presence-only entry is answered from the manifest without restoring its pack,
// so it costs a reader nothing and is not worth separating out.
func (d *Demand) Downloaded(kind, digest string) bool {
	return len(d.downloads[demandKey(kind, digest)]) > 0
}

// runsFor is the set of jobs that downloaded an entry.
func (d *Demand) runsFor(kind, digest string) map[string]struct{} {
	return d.downloads[demandKey(kind, digest)]
}

// Signature names the set of runs that downloaded an entry, and is empty when
// none did. Two entries sharing a signature were wanted by exactly the same
// jobs, so placing them in one pack costs a later job nothing it did not
// already have to fetch.
func (d *Demand) Signature(kind, digest string) string {
	runs := d.downloads[demandKey(kind, digest)]
	if len(runs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, " ")
}
