package optimiser

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

// The planner works on the merged manifest view directly, so that the entries
// it groups carry the block identity the repacker needs to copy them.
type (
	Entry  = server.LayoutEntry
	Pack   = server.LayoutPack
	Layout = server.Layout
)

type Options struct {
	// TargetPackSize bounds a rebuilt pack, in bytes of object content.
	TargetPackSize int64
	// MinWasteFraction is how much of a pack must be bytes a restore pays for
	// but does not want before rebuilding it is worth the upload.
	MinWasteFraction float64
	// MinRuns refuses to plan from a window too small to mean anything.
	MinRuns int
}

func DefaultOptions() Options {
	return Options{TargetPackSize: 8 * 1024 * 1024, MinWasteFraction: 0.25, MinRuns: 3}
}

// Group is one pack the optimiser will build. A group never mixes kinds:
// Bazel reads action results before it fetches any output, so a pack of pure
// action results answers many lookups for one small restore.
type Group struct {
	Kind      string
	Signature string
	Entries   []Entry
	Bytes     int64
}

type Plan struct {
	Groups []Group
	// Rewrite packs have live entries that all reappear in Groups.
	Rewrite []string
	// Dead packs have no live entries left; every digest they hold is served
	// from a lower pack ID.
	Dead []string
	Keep []string
	// ReclaimedBytes counts object bytes that stop being stored at all.
	ReclaimedBytes int64
	// WastedBytes counts what an average restore of the rewritten packs paid
	// for and did not want.
	WastedBytes int64
}

func (p Plan) Empty() bool { return len(p.Rewrite) == 0 && len(p.Dead) == 0 }

var errTooFewRuns = errors.New("too few usage records to plan from")

type packState struct {
	pack      Pack
	entries   []Entry
	liveBytes int64
}

// waste estimates the bytes an average restore of this pack pays for and does
// not want. Measuring it per restoring run is what catches a pack whose entries
// are all wanted, but by different jobs, so every one of them overfetches.
func (s *packState) waste(demand *Demand) int64 {
	deadBytes := max(s.pack.DeclaredBytes-s.liveBytes, 0)
	restorers := make(map[string]struct{})
	for _, entry := range s.entries {
		for run := range demand.runsFor(entry.Kind, entry.Digest) {
			restorers[run] = struct{}{}
		}
	}
	// A pack no observed run restored costs only the storage its dead copies
	// take; nothing ever downloads it to overfetch from.
	if len(restorers) == 0 {
		return deadBytes
	}
	var total int64
	for run := range restorers {
		var wanted int64
		for _, entry := range s.entries {
			if _, found := demand.runsFor(entry.Kind, entry.Digest)[run]; found {
				wanted += entry.Size
			}
		}
		total += s.pack.DeclaredBytes - wanted
	}
	return total / int64(len(restorers))
}

// NewPlan decides which packs to rebuild and how to regroup their contents. It
// never drops a CAS entry: a manifest records an action's closure as pack IDs
// rather than digests, so the optimiser cannot prove any blob is unreferenced.
func NewPlan(layout Layout, demand *Demand, options Options) (Plan, error) {
	if demand.Runs() < options.MinRuns {
		return Plan{}, fmt.Errorf("%w: have %d, want %d", errTooFewRuns, demand.Runs(), options.MinRuns)
	}
	if options.TargetPackSize <= 0 {
		return Plan{}, errors.New("target pack size must be positive")
	}

	states := make(map[string]*packState, len(layout.Packs))
	for _, pack := range layout.Packs {
		states[pack.ID] = &packState{pack: pack}
	}
	for _, entry := range layout.Entries {
		state, known := states[entry.Pack]
		if !known {
			return Plan{}, fmt.Errorf("entry %s/%s names unknown pack %s", entry.Kind, entry.Digest, entry.Pack)
		}
		state.entries = append(state.entries, entry)
		state.liveBytes += entry.Size
	}

	plan := Plan{}
	moving := make([]Entry, 0)
	for _, pack := range sortedPackIDs(layout.Packs) {
		state := states[pack]
		if len(state.entries) == 0 {
			plan.Dead = append(plan.Dead, pack)
			plan.ReclaimedBytes += state.pack.DeclaredBytes
			continue
		}
		deadBytes := max(state.pack.DeclaredBytes-state.liveBytes, 0)
		wasted := state.waste(demand)
		if state.pack.DeclaredBytes <= 0 ||
			float64(wasted)/float64(state.pack.DeclaredBytes) < options.MinWasteFraction {
			plan.Keep = append(plan.Keep, pack)
			continue
		}
		plan.Rewrite = append(plan.Rewrite, pack)
		plan.ReclaimedBytes += deadBytes
		plan.WastedBytes += wasted
		moving = append(moving, state.entries...)
	}

	plan.Groups = group(moving, demand, options.TargetPackSize)
	if err := plan.verify(moving); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// group orders entries so that those wanted by the same jobs end up adjacent,
// then fills packs in that order.
func group(entries []Entry, demand *Demand, targetSize int64) []Group {
	type keyed struct {
		entry      Entry
		signature  string
		popularity int
	}
	ordered := make([]keyed, 0, len(entries))
	for _, entry := range entries {
		signature := demand.Signature(entry.Kind, entry.Digest)
		ordered = append(ordered, keyed{
			entry:      entry,
			signature:  signature,
			popularity: len(strings.Fields(signature)),
		})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].entry.Kind != ordered[j].entry.Kind {
			return ordered[i].entry.Kind < ordered[j].entry.Kind
		}
		// Most-wanted first, so the entries a restore is most likely to be for
		// are the ones that end up sharing a pack.
		if ordered[i].popularity != ordered[j].popularity {
			return ordered[i].popularity > ordered[j].popularity
		}
		if ordered[i].signature != ordered[j].signature {
			return ordered[i].signature < ordered[j].signature
		}
		return ordered[i].entry.Digest < ordered[j].entry.Digest
	})

	groups := make([]Group, 0)
	var current *Group
	for _, item := range ordered {
		split := current == nil ||
			current.Kind != item.entry.Kind ||
			// Never let a cold entry share a pack with a hot one; that mixing is
			// the waste this whole pass exists to remove.
			(current.Signature == "") != (item.signature == "") ||
			current.Bytes+item.entry.Size > targetSize
		if split {
			groups = append(groups, Group{Kind: item.entry.Kind, Signature: item.signature})
			current = &groups[len(groups)-1]
		}
		current.Entries = append(current.Entries, item.entry)
		current.Bytes += item.entry.Size
	}
	return groups
}

// verify is the safety net for the one invariant that matters: a pack is only
// ever deleted once every entry it served has somewhere else to come from.
func (p Plan) verify(moving []Entry) error {
	placed := make(map[string]struct{})
	for _, group := range p.Groups {
		for _, entry := range group.Entries {
			key := demandKey(entry.Kind, entry.Digest)
			if _, duplicate := placed[key]; duplicate {
				return fmt.Errorf("plan places %s/%s into two packs", entry.Kind, entry.Digest)
			}
			placed[key] = struct{}{}
		}
	}
	for _, entry := range moving {
		if _, found := placed[demandKey(entry.Kind, entry.Digest)]; !found {
			return fmt.Errorf("plan drops %s/%s", entry.Kind, entry.Digest)
		}
	}
	return nil
}

func sortedPackIDs(packs []Pack) []string {
	ids := make([]string, 0, len(packs))
	for _, pack := range packs {
		ids = append(ids, pack.ID)
	}
	sort.Strings(ids)
	return ids
}
