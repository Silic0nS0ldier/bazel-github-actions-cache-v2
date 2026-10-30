package optimiser

import (
	"encoding/binary"
	"math/bits"
)

// runSet is a set of observed runs, numbered by Demand.index.
type runSet []uint64

func newRunSet(runs int) runSet { return make(runSet, (runs+63)/64) }

func (s runSet) add(at int)      { s[at/64] |= 1 << (at % 64) }
func (s runSet) has(at int) bool { return s[at/64]&(1<<(at%64)) != 0 }

func (s runSet) count() int {
	total := 0
	for _, word := range s {
		total += bits.OnesCount64(word)
	}
	return total
}

func (s runSet) empty() bool {
	for _, word := range s {
		if word != 0 {
			return false
		}
	}
	return true
}

// without counts the runs in s that are not in other.
func (s runSet) without(other runSet) int {
	total := 0
	for at, word := range s {
		total += bits.OnesCount64(word &^ other[at])
	}
	return total
}

func (s runSet) union(other runSet) {
	for at := range s {
		s[at] |= other[at]
	}
}

func (s runSet) clone() runSet { return append(runSet(nil), s...) }

func (s runSet) key() string {
	buffer := make([]byte, 8*len(s))
	for at, word := range s {
		binary.LittleEndian.PutUint64(buffer[8*at:], word)
	}
	return string(buffer)
}

// before orders sets by their lowest differing run, the set holding it first.
// Runs are numbered busiest first, so this nests sets under the jobs that
// restore the most, which keeps sets that share those jobs adjacent.
func (s runSet) before(other runSet) bool {
	for at, word := range s {
		if differ := word ^ other[at]; differ != 0 {
			return word&(differ&-differ) != 0
		}
	}
	return false
}
