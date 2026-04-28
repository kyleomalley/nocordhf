package ft8

import "testing"

// TestHashKnownValues cross-checks our hash against values observed on the
// wire from reference design. These were captured live: a Type 4 message received with
// n12=2286 was displayed by reference design as <K4ZXV>, pinning K4ZXV→2286.
func TestHashKnownValues(t *testing.T) {
	cases := []struct {
		call string
		h22  uint32
		h12  uint16
	}{
		{"K4ZXV", 0, 2286}, // h22 unknown from evidence; h12 from reference design live decode
	}
	for _, c := range cases {
		got12 := hash12(c.call)
		if got12 != c.h12 {
			t.Errorf("hash12(%q) = %d, want %d", c.call, got12, c.h12)
		}
		if c.h22 != 0 {
			got22 := hash22(c.call)
			if got22 != c.h22 {
				t.Errorf("hash22(%q) = %d, want %d", c.call, got22, c.h22)
			}
		}
	}
}
