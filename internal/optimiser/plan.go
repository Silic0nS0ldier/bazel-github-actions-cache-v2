package optimiser

import (
	"errors"
	"fmt"
	"sort"
	"time"

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
	// but does not want before rebuilding it is worth the upload. A download's
	// fixed cost counts as such bytes, which is what makes a small pack worth
	// merging even when every job restoring it wants all of it.
	MinWasteFraction float64
	// MinImprovement is how much of a pack's restore cost its rebuild must
	// remove. Without it, a pack no grouping can improve is rebuilt on every
	// pass.
	MinImprovement float64
	// DownloadOverhead is the fixed cost of restoring one pack, in the bytes
	// it is worth: a request and a manifest to read before any content flows.
	// It is what lets fewer, larger packs win over many small ones.
	DownloadOverhead int64
	// MinRuns refuses to plan from a window too small to mean anything.
	MinRuns int
}

func DefaultOptions() Options {
	return Options{
		TargetPackSize:   8 * 1024 * 1024,
		MinWasteFraction: 0.25,
		MinImprovement:   0.25,
		DownloadOverhead: 8 * 1024 * 1024,
		MinRuns:          3,
	}
}

// Timing gates the two kinds of rewrite by pack age. Eviction rewrites one
// pack at a time to drop entries nothing has used for EvictAfter. Reshaping
// regroups packs whose shape is at least ReshapeAfter old.
type Timing struct {
	Now time.Time
	// Ages is when each pack's contents took their current shape. A pack
	// missing from it is treated as brand new. Nil turns the age gates off.
	Ages         map[string]time.Time
	EvictAfter   time.Duration
	ReshapeAfter time.Duration
	// Recent holds every usage record since Now-EvictAfter. Nil, because those
	// records are incomplete, turns eviction off.
	Recent *Demand
}

// Group is one pack the optimiser will build. A reshaped group never mixes
// kinds: Bazel reads action results before it fetches any output, so a pack of
// pure action results answers many lookups for one small restore.
type Group struct {
	Kind string
	// Signature names the runs that would have restored this pack, and is
	// empty for a pack no observed run wanted anything from.
	Signature string
	Entries   []Entry
	Bytes     int64
	// Since is the age a pack rewritten only to evict stale entries inherits
	// from its source. Zero for a reshaped pack, whose age starts afresh.
	Since time.Time
	// Evicts names the pack this group rewrites without its stale entries.
	Evicts string
	runs   runSet
}

type Plan struct {
	Groups []Group
	// Rewrite packs have live entries that all reappear in Groups, apart from
	// any evicted as stale.
	Rewrite []string
	// Dead packs have no live entries left; every digest they hold is served
	// from a lower pack ID.
	Dead []string
	Keep []string
	// ReclaimedBytes counts object bytes that stop being stored at all.
	ReclaimedBytes int64
	// WastedBytes counts what an average restore of the reshaped packs paid
	// for and did not want.
	WastedBytes int64
	// OverfetchBefore is what an average observed job downloaded from the
	// reshaped packs and did not want; OverfetchAfter is the same for their
	// replacements. Downloads count the packs it restored to get it.
	OverfetchBefore int64
	OverfetchAfter  int64
	DownloadsBefore float64
	DownloadsAfter  float64
	// Unimproved counts packs over the waste threshold that were left alone
	// because no regrouping found would cut their restore cost enough.
	Unimproved int
	// Evicted counts packs rewritten on their own to drop EvictedEntries stale
	// entries holding EvictedBytes.
	Evicted        int
	EvictedEntries int
	EvictedBytes   int64
	// Abandoned counts packs every entry of which is stale. Nothing restores
	// them, so GitHub expires them; rewriting would only renew them.
	Abandoned int
	// ReshapeEligible counts packs old enough to reshape, and ReshapeSkipped
	// says why reshaping did not run, when it did not.
	ReshapeEligible int
	ReshapeSkipped  string
}

func (p Plan) Empty() bool { return len(p.Rewrite) == 0 && len(p.Dead) == 0 }

var errTooFewRuns = errors.New("too few usage records to plan from")

// maxRefinements bounds how often the planner drops packs a regrouping does
// not improve and regroups what is left.
const maxRefinements = 8

// maxGroupEntries keeps a rebuilt pack's manifest well inside the entry limit
// readers enforce, however small its entries.
const maxGroupEntries = 50_000

type packState struct {
	pack      Pack
	entries   []Entry
	liveBytes int64
	// Stale entries have been taken out of entries, and their bytes count as
	// dead from here on.
	staleEntries int
	staleBytes   int64
}

func (s *packState) deadBytes() int64 { return max(s.pack.DeclaredBytes-s.liveBytes, 0) }

func (s *packState) restorers(demand *Demand) runSet {
	restorers := newRunSet(demand.Runs())
	for _, entry := range s.entries {
		restorers.union(demand.runSet(entry.Kind, entry.Digest))
	}
	return restorers
}

// waste estimates the bytes an average restore of this pack pays for and does
// not want. Measuring it per restoring run is what catches a pack whose entries
// are all wanted, but by different jobs, so every one of them overfetches.
func (s *packState) waste(demand *Demand) int64 {
	restorers := s.restorers(demand)
	// A pack no observed run restored costs only the storage its dead copies
	// take; nothing ever downloads it to overfetch from.
	if restorers.empty() {
		return s.deadBytes()
	}
	var total int64
	for at := range demand.Runs() {
		if !restorers.has(at) {
			continue
		}
		var wanted int64
		for _, entry := range s.entries {
			if demand.runSet(entry.Kind, entry.Digest).has(at) {
				wanted += entry.Size
			}
		}
		total += s.pack.DeclaredBytes - wanted
	}
	return total / int64(restorers.count())
}

// wasteful reports whether a pack is worth considering for a rebuild at all.
func (s *packState) wasteful(demand *Demand, options Options) bool {
	if s.pack.DeclaredBytes <= 0 {
		return false
	}
	waste, size := s.waste(demand), s.pack.DeclaredBytes
	if !s.restorers(demand).empty() {
		waste += options.DownloadOverhead
		size += options.DownloadOverhead
	}
	return float64(waste)/float64(size) >= options.MinWasteFraction
}

// cost is what restoring packs costs every observed run together: the bytes
// downloaded and not wanted, and the downloads it took.
type cost struct {
	overfetch int64
	downloads float64
}

func (c cost) total(overhead int64) float64 {
	return float64(c.overfetch) + c.downloads*float64(overhead)
}

// placement is where a regrouping puts an entry: how many runs restore its new
// pack, and its share of that pack's downloads.
type placement struct {
	restorers int
	share     float64
}

// costs measures the pack now, and once its entries sit where placed says.
// Every run that wants one entry of a pack downloads all of it, so each entry
// costs its size once for every restoring run that did not want it.
func (s *packState) costs(demand *Demand, placed map[string]placement) (before, after cost) {
	current := s.restorers(demand).count()
	before.overfetch = s.deadBytes() * int64(current)
	before.downloads = float64(current)
	for _, entry := range s.entries {
		wanted := int64(demand.runSet(entry.Kind, entry.Digest).count())
		at := placed[demandKey(entry.Kind, entry.Digest)]
		before.overfetch += entry.Size * (int64(current) - wanted)
		after.overfetch += entry.Size * (int64(at.restorers) - wanted)
		after.downloads += float64(at.restorers) * at.share
	}
	return before, after
}

// worthRebuilding reports whether the regrouping pays for the upload, either
// by cutting what restoring the pack costs or by dropping enough dead copies
// to be worth it for the storage alone.
func (s *packState) worthRebuilding(before, after cost, options Options) bool {
	was, now := before.total(options.DownloadOverhead), after.total(options.DownloadOverhead)
	if gain := was - now; gain > 0 && gain >= options.MinImprovement*was {
		return true
	}
	dead := s.deadBytes()
	return dead > 0 && float64(dead) >= options.MinWasteFraction*float64(s.pack.DeclaredBytes)
}

// NewPlan decides which packs to rebuild and how to regroup their contents,
// treating every pack as old enough to reshape and evicting nothing.
func NewPlan(layout Layout, demand *Demand, options Options) (Plan, error) {
	return NewTimedPlan(layout, demand, options, Timing{})
}

// NewTimedPlan plans both kinds of rewrite.
//
// Eviction drops entries no job has used for EvictAfter from packs at least
// that old, one pack at a time and keeping each pack's age, so a pack holding a
// little dead weight is cleaned without disturbing anything else. Packs whose
// every entry is stale are left for GitHub to expire.
//
// Reshaping regroups packs whose shape is at least ReshapeAfter old, and only
// when there are at least two: one pack can only have its contents shuffled.
// A reshaped pack starts a new age, which is what keeps reshaping from
// revisiting the same packs on every pass.
//
// Nothing but disuse drops a live entry. A manifest records an action's
// closure as pack IDs rather than digests, so no entry can be proven
// unreferenced; but a reader validates an action's closure before serving it,
// so evicting something still referenced costs a cache miss rather than a
// broken build.
func NewTimedPlan(layout Layout, demand *Demand, options Options, timing Timing) (Plan, error) {
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

	oldEnough := func(pack string, age time.Duration) bool {
		if timing.Ages == nil {
			return true
		}
		at, known := timing.Ages[pack]
		return known && !at.IsZero() && !at.After(timing.Now.Add(-age))
	}
	evicting := timing.Recent != nil && timing.EvictAfter > 0

	plan := Plan{}
	eligible := make([]*packState, 0)
	evictOnly := make([]*packState, 0)
	settle := func(state *packState) {
		if state.staleEntries > 0 {
			evictOnly = append(evictOnly, state)
		} else {
			plan.Keep = append(plan.Keep, state.pack.ID)
		}
	}
	for _, pack := range sortedPackIDs(layout.Packs) {
		state := states[pack]
		if len(state.entries) == 0 {
			plan.Dead = append(plan.Dead, pack)
			plan.ReclaimedBytes += state.pack.DeclaredBytes
			continue
		}
		if evicting && oldEnough(pack, timing.EvictAfter) {
			fresh := state.entries[:0:0]
			for _, entry := range state.entries {
				if timing.Recent.Used(entry.Kind, entry.Digest) {
					fresh = append(fresh, entry)
					continue
				}
				state.staleEntries++
				state.staleBytes += entry.Size
			}
			if len(fresh) == 0 {
				plan.Abandoned++
				plan.Keep = append(plan.Keep, pack)
				continue
			}
			state.entries = fresh
			state.liveBytes -= state.staleBytes
		}
		if oldEnough(pack, timing.ReshapeAfter) {
			eligible = append(eligible, state)
		} else {
			settle(state)
		}
	}
	plan.ReshapeEligible = len(eligible)

	reshaped := make([]*packState, 0)
	if len(eligible) < 2 {
		plan.ReshapeSkipped = fmt.Sprintf("%d packs are old enough to reshape, and reshaping needs at least 2", len(eligible))
		for _, state := range eligible {
			settle(state)
		}
	} else {
		candidates := make([]*packState, 0, len(eligible))
		for _, state := range eligible {
			if state.wasteful(demand, options) {
				candidates = append(candidates, state)
			} else {
				settle(state)
			}
		}
		// A pack can be wasteful and still be the best grouping its entries
		// allow, so each candidate has to earn its rebuild against the
		// regrouping actually on offer, judged on runs that regrouping was not
		// fitted to. Dropping one changes what the rest are grouped with, hence
		// the loop.
		fit, check := demand.split()
		for iteration := 0; len(candidates) > 0 && iteration < maxRefinements; iteration++ {
			trial := group(movingEntries(candidates), fit, options)
			before, after := assess(candidates, trial, check)
			kept := make([]*packState, 0, len(candidates))
			for _, state := range candidates {
				if state.worthRebuilding(before[state], after[state], options) {
					kept = append(kept, state)
					continue
				}
				plan.Unimproved++
				settle(state)
			}
			if len(kept) == len(candidates) {
				break
			}
			candidates = kept
		}
		reshaped = candidates
	}

	sort.Slice(evictOnly, func(i, j int) bool { return evictOnly[i].pack.ID < evictOnly[j].pack.ID })
	for _, state := range evictOnly {
		plan.Groups = append(plan.Groups, evictionGroup(state, demand, timing.Ages))
		plan.Rewrite = append(plan.Rewrite, state.pack.ID)
		plan.ReclaimedBytes += state.deadBytes()
		plan.Evicted++
	}

	// The packs actually built use every run, now that the choice of which to
	// rebuild no longer rests on them.
	groups := group(movingEntries(reshaped), demand, options)
	before, after := assess(reshaped, groups, demand)
	var was, now cost
	for _, state := range reshaped {
		plan.Rewrite = append(plan.Rewrite, state.pack.ID)
		plan.ReclaimedBytes += state.deadBytes()
		plan.WastedBytes += state.waste(demand)
		was.overfetch += before[state].overfetch
		was.downloads += before[state].downloads
		now.overfetch += after[state].overfetch
		now.downloads += after[state].downloads
	}
	for _, state := range append(append([]*packState(nil), evictOnly...), reshaped...) {
		plan.EvictedEntries += state.staleEntries
		plan.EvictedBytes += state.staleBytes
	}
	if runs := demand.Runs(); runs > 0 {
		plan.OverfetchBefore = was.overfetch / int64(runs)
		plan.OverfetchAfter = now.overfetch / int64(runs)
		plan.DownloadsBefore = was.downloads / float64(runs)
		plan.DownloadsAfter = now.downloads / float64(runs)
	}
	plan.Groups = append(plan.Groups, groups...)
	sort.Strings(plan.Keep)
	if err := plan.verify(movingEntries(append(evictOnly, reshaped...))); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// evictionGroup rewrites one pack without its stale entries, in the same order
// and with the same age, so that nothing but the stale entries changes.
func evictionGroup(state *packState, demand *Demand, ages map[string]time.Time) Group {
	evicted := Group{
		Kind:    state.entries[0].Kind,
		Entries: append([]Entry(nil), state.entries...),
		Since:   ages[state.pack.ID],
		Evicts:  state.pack.ID,
		runs:    state.restorers(demand),
	}
	for _, entry := range state.entries {
		evicted.Bytes += entry.Size
		if entry.Kind != evicted.Kind {
			evicted.Kind = "mixed"
		}
	}
	evicted.Signature = demand.names(evicted.runs)
	return evicted
}

func movingEntries(states []*packState) []Entry {
	moving := make([]Entry, 0)
	for _, state := range states {
		moving = append(moving, state.entries...)
	}
	return moving
}

// assess measures each pack's restore cost now and once regrouped, under the
// given demand. That demand need not be what the groups were built from.
func assess(states []*packState, groups []Group, demand *Demand) (before, after map[*packState]cost) {
	placed := make(map[string]placement)
	for _, group := range groups {
		restorers := newRunSet(demand.Runs())
		for _, entry := range group.Entries {
			restorers.union(demand.runSet(entry.Kind, entry.Digest))
		}
		count := restorers.count()
		for _, entry := range group.Entries {
			// A pack's downloads are shared out by bytes, or evenly among
			// entries when it holds none.
			share := 1 / float64(len(group.Entries))
			if group.Bytes > 0 {
				share = float64(entry.Size) / float64(group.Bytes)
			}
			placed[demandKey(entry.Kind, entry.Digest)] = placement{restorers: count, share: share}
		}
	}
	before = make(map[*packState]cost, len(states))
	after = make(map[*packState]cost, len(states))
	for _, state := range states {
		before[state], after[state] = state.costs(demand, placed)
	}
	return before, after
}

// candidateWindow bounds how far ahead a filling pack looks for its next chunk.
// The ordering already puts similar run sets close together, so a short look
// ahead finds nearly everything a full search would, at linear cost.
const candidateWindow = 64

// chunk is entries wanted by exactly the same runs, cut to fit a pack.
type chunk struct {
	runs    runSet
	bytes   int64
	entries []Entry
}

// group fills packs so that the jobs restoring each one want as much of it as
// possible. Cold entries, which no observed job downloaded, get packs of their
// own: mixing them in is the waste this whole pass exists to remove.
func group(entries []Entry, demand *Demand, options Options) []Group {
	byKind := make(map[string][]Entry)
	for _, entry := range entries {
		byKind[entry.Kind] = append(byKind[entry.Kind], entry)
	}
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)

	groups := make([]Group, 0)
	for _, kind := range kinds {
		var hot, cold []Entry
		for _, entry := range byKind[kind] {
			if demand.runSet(entry.Kind, entry.Digest).empty() {
				cold = append(cold, entry)
			} else {
				hot = append(hot, entry)
			}
		}
		groups = append(groups, groupHot(kind, hot, demand, options)...)
		groups = append(groups, groupCold(kind, cold, demand, options.TargetPackSize)...)
	}
	return groups
}

// groupCold keeps entries that came from the same pack together, since they
// were written by the same job and are the likeliest to be wanted together.
func groupCold(kind string, entries []Entry, demand *Demand, targetSize int64) []Group {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Pack != entries[j].Pack {
			return entries[i].Pack < entries[j].Pack
		}
		return entries[i].Digest < entries[j].Digest
	})
	groups := make([]Group, 0)
	var current *Group
	for _, entry := range entries {
		if current == nil || current.Bytes+entry.Size > targetSize || len(current.Entries) >= maxGroupEntries {
			groups = append(groups, Group{Kind: kind, runs: newRunSet(demand.Runs())})
			current = &groups[len(groups)-1]
		}
		current.Entries = append(current.Entries, entry)
		current.Bytes += entry.Size
	}
	return groups
}

func groupHot(kind string, entries []Entry, demand *Demand, options Options) []Group {
	targetSize := options.TargetPackSize
	sort.Slice(entries, func(i, j int) bool { return entries[i].Digest < entries[j].Digest })
	classes := make(map[string][]Entry)
	sets := make(map[string]runSet)
	for _, entry := range entries {
		set := demand.runSet(entry.Kind, entry.Digest)
		key := set.key()
		classes[key] = append(classes[key], entry)
		sets[key] = set
	}
	chunks := make([]chunk, 0, len(classes))
	for key, members := range classes {
		var current *chunk
		for _, entry := range members {
			if current == nil || current.bytes+entry.Size > targetSize || len(current.entries) >= maxGroupEntries {
				chunks = append(chunks, chunk{runs: sets[key]})
				current = &chunks[len(chunks)-1]
			}
			current.entries = append(current.entries, entry)
			current.bytes += entry.Size
		}
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].runs.before(chunks[j].runs) {
			return true
		}
		if chunks[j].runs.before(chunks[i].runs) {
			return false
		}
		return chunks[i].entries[0].Digest < chunks[j].entries[0].Digest
	})

	overhead := options.DownloadOverhead

	// next links the chunks still to be placed, in order; len(chunks) ends it.
	next := make([]int, len(chunks))
	for at := range next {
		next[at] = at + 1
	}
	head := 0
	groups := make([]Group, 0)
	for head < len(chunks) {
		seed := chunks[head]
		head = next[head]
		current := Group{
			Kind:    kind,
			Entries: append([]Entry(nil), seed.entries...),
			Bytes:   seed.bytes,
			runs:    seed.runs.clone(),
		}
		for current.Bytes < targetSize {
			best, bestPrevious := -1, -1
			var bestBenefit int64
			previous, scanned := -1, 0
			for at := head; at < len(chunks) && scanned < candidateWindow; previous, at = at, next[at] {
				scanned++
				candidate := &chunks[at]
				if current.Bytes+candidate.bytes > targetSize ||
					len(current.Entries)+len(candidate.entries) > maxGroupEntries {
					continue
				}
				// Every run restoring either part restores the merged pack. Merging
				// saves those wanting both a download, and costs the others the
				// part they do not want.
				restoring := int64(current.runs.count())
				wanting := int64(candidate.runs.count())
				merged := restoring + int64(candidate.runs.without(current.runs))
				benefit := restoring*(current.Bytes+overhead) + wanting*(candidate.bytes+overhead) -
					merged*(current.Bytes+candidate.bytes+overhead)
				if best < 0 || benefit > bestBenefit {
					best, bestPrevious, bestBenefit = at, previous, benefit
				}
			}
			if best < 0 || bestBenefit < 0 {
				break
			}
			candidate := &chunks[best]
			if bestPrevious < 0 {
				head = next[best]
			} else {
				next[bestPrevious] = next[best]
			}
			current.Entries = append(current.Entries, candidate.entries...)
			current.Bytes += candidate.bytes
			current.runs.union(candidate.runs)
		}
		current.Signature = demand.names(current.runs)
		groups = append(groups, current)
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
