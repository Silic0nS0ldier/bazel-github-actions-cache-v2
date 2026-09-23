package optimiser

import "testing"

func TestHumanBytesUsesTheLargestUnitThatFits(t *testing.T) {
	cases := map[int64]string{
		0:             "0B",
		12:            "12B",
		1023:          "1,023B",
		1024:          "1KiB",
		2491416576:    "2.32GiB",
		241432494:     "230.248MiB",
		1 << 40 * 3:   "3TiB",
		1<<30 + 1<<29: "1.5GiB",
		// Trailing zeros go, so a round size stays short.
		2 << 20: "2MiB",
		-1024:   "-1KiB",
	}
	for value, want := range cases {
		if got := HumanBytes(value); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestHumanCountGroupsDigits(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		999999:  "999,999",
		1000000: "1,000,000",
		-12345:  "-12,345",
	}
	for value, want := range cases {
		if got := HumanCount(value); got != want {
			t.Errorf("HumanCount(%d) = %q, want %q", value, got, want)
		}
	}
}
