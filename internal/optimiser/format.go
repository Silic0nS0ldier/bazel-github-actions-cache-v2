package optimiser

import (
	"strconv"
	"strings"
)

var byteUnits = []struct {
	name string
	size int64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// HumanBytes renders a size in the largest unit that fits, as "2.32GiB".
// Trailing zeros are dropped so a round number stays short.
func HumanBytes(value int64) string {
	if value < 0 {
		return "-" + HumanBytes(-value)
	}
	for _, unit := range byteUnits {
		// Bytes are the floor, so anything under a kibibyte lands there.
		if value < unit.size && unit.size > 1 {
			continue
		}
		scaled := strconv.FormatFloat(float64(value)/float64(unit.size), 'f', 3, 64)
		return groupInteger(trimTrailingZeros(scaled)) + unit.name
	}
	return "0B"
}

func trimTrailingZeros(text string) string {
	if !strings.Contains(text, ".") {
		return text
	}
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}

func groupInteger(text string) string {
	integer, fraction, hasFraction := strings.Cut(text, ".")
	value, err := strconv.ParseInt(integer, 10, 64)
	if err != nil {
		return text
	}
	if hasFraction {
		return HumanCount(value) + "." + fraction
	}
	return HumanCount(value)
}

// HumanCount groups digits so a long number can be read at a glance.
func HumanCount(value int64) string {
	text := strconv.FormatInt(value, 10)
	sign := ""
	if text[0] == '-' {
		sign, text = "-", text[1:]
	}
	lead := len(text) % 3
	if lead == 0 {
		lead = 3
	}
	grouped := text[:lead]
	for index := lead; index < len(text); index += 3 {
		grouped += "," + text[index:index+3]
	}
	return sign + grouped
}
