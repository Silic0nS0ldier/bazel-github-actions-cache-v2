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

	// Built lazily from the above, and dropped by Observe.
	order    []string
	position map[string]int
	sets     map[string]runSet
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
	d.order, d.position, d.sets = nil, nil, nil
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

// minHoldoutRuns is the smallest window split into fitting and checking runs;
// below it every run does both.
const minHoldoutRuns = 4

// split sets aside every fourth run, so that a regrouping fitted to the rest
// can be judged on jobs it never saw. Judged on its own runs, any regrouping
// looks like an improvement, and a pass would chase each window's noise.
func (d *Demand) split() (fit, check *Demand) {
	if len(d.runs) < minHoldoutRuns {
		return d, d
	}
	ids := make([]string, 0, len(d.runs))
	for id := range d.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	held := make(map[string]bool, len(ids)/4)
	for at, id := range ids {
		if at%4 == 3 {
			held[id] = true
		}
	}
	return d.filter(func(id string) bool { return !held[id] }), d.filter(func(id string) bool { return held[id] })
}

func (d *Demand) filter(keep func(string) bool) *Demand {
	filtered := NewDemand()
	for id := range d.runs {
		if keep(id) {
			filtered.runs[id] = struct{}{}
		}
	}
	for key, runs := range d.downloads {
		for id := range runs {
			if !keep(id) {
				continue
			}
			if filtered.downloads[key] == nil {
				filtered.downloads[key] = make(map[string]struct{})
			}
			filtered.downloads[key][id] = struct{}{}
		}
	}
	return filtered
}

// Downloaded reports whether any observed job needed the entry's bytes. A
// presence-only entry is answered from the manifest without restoring its pack,
// so it costs a reader nothing and is not worth separating out.
func (d *Demand) Downloaded(kind, digest string) bool {
	return len(d.downloads[demandKey(kind, digest)]) > 0
}

// index numbers the runs, busiest first, so that ordering run sets by their
// lowest bits clusters entries by the jobs that restore the most.
func (d *Demand) index() {
	if d.position != nil {
		return
	}
	busy := make(map[string]int, len(d.runs))
	for _, runs := range d.downloads {
		for run := range runs {
			busy[run]++
		}
	}
	d.order = make([]string, 0, len(d.runs))
	for run := range d.runs {
		d.order = append(d.order, run)
	}
	sort.Slice(d.order, func(i, j int) bool {
		if busy[d.order[i]] != busy[d.order[j]] {
			return busy[d.order[i]] > busy[d.order[j]]
		}
		return d.order[i] < d.order[j]
	})
	d.position = make(map[string]int, len(d.order))
	for at, run := range d.order {
		d.position[run] = at
	}
	d.sets = make(map[string]runSet)
}

// Used reports whether any observed job downloaded the entry or checked that it
// exists. Either keeps it: a presence check is how a job, or a reader validating
// an action result, depends on an entry it does not need the bytes of.
func (d *Demand) Used(kind, digest string) bool {
	key := demandKey(kind, digest)
	return len(d.downloads[key]) > 0 || d.presence[key] > 0
}

// runSet is the jobs that downloaded an entry, as a bitset over index order.
// The result is shared and must not be modified.
func (d *Demand) runSet(kind, digest string) runSet {
	d.index()
	key := demandKey(kind, digest)
	if set, found := d.sets[key]; found {
		return set
	}
	set := newRunSet(len(d.order))
	for run := range d.downloads[key] {
		set.add(d.position[run])
	}
	d.sets[key] = set
	return set
}

// names lists the runs in a set, sorted, in the form Signature uses.
func (d *Demand) names(set runSet) string {
	d.index()
	ids := make([]string, 0, set.count())
	for at, run := range d.order {
		if set.has(at) {
			ids = append(ids, run)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, " ")
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
