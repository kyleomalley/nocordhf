package ft8

import "testing"

func TestIsValidType1Message(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// Known-good messages seen from reference design ground truth.
		{"CQ KO6IEH DM13", true},
		{"CQ W3UA FN42", true},
		{"CQ DX K1ABC FN42", true},
		{"CQ NA W6TOX DM24", true},
		{"CQ 3 UA9CC MO26", true},
		{"CQ POTA KE8WCR EN80", true}, // Parks on the Air activity
		{"CQ SOTA G4ABC IO91", true},  // Summits on the Air activity
		{"CQ WP4SZA FK68", true},
		{"HA1BF N8DH EN81", true},
		{"IU1CYA KD7MW CN87", true},
		{"HA1UA TI5RTZ R-19", true},
		{"IN3IZQ HK3AB -22", true},
		{"TX9W LA0FA JO59", true},
		{"LZ2CP W3UA -8", true},
		{"WP4SZA ZL3XJ RF73", true},
		{"ZL3XJ WP4SZA +4", true},
		{"KO6IEH W1AW RR73", true},
		{"KO6IEH W1AW 73", true},
		{"HI0DMRA <KO6IEH>", true},      // hash in call2 position
		{"<KO6IEH> HI0DMRA RR73", true}, // hash in call1 position

		// Phantoms observed in live logs — must be rejected.
		{"200BSQ ND1UVX LI57", false},   // all-digit prefix on call1
		{"CQ 200BSQ LI57", false},       // bogus CQ target
		{"K70 DU 290ACR R GM69", false}, // both calls malformed
		{"290ACR K70 R GM69", false},    // both calls malformed, no CQ
		{"PA4SPD/R 376QGY JD56", false}, // second call all-digit prefix
		// Note: messages with '/' that are structurally plausible
		// (e.g. "CQ L35TBH/R R RM07") are rejected at a different filter
		// layer in decode.go; this function just checks structure.

		// Edge cases.
		{"", false},
		{"CQ", false},
		{"CQ DX", false}, // modifier but no call
		{"JUSTONETOKEN", false},
	}
	for _, c := range cases {
		got := isValidType1Message(c.text)
		if got != c.want {
			t.Errorf("isValidType1Message(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestMessageHasAllocatedCall(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// Known-good messages from reference design ground truth — must pass.
		{"CQ KO6IEH DM13", true},
		{"CQ W3UA FN42", true},
		{"HA1BF N8DH EN81", true},
		{"IU1CYA KD7MW CN87", true},
		{"HA1UA TI5RTZ R-19", true}, // HA (Hungary) + TI (Costa Rica)
		{"TX9W LA0FA JO59", true},   // TX (France DOM) + LA (Norway)
		{"JH2BJL KD2AGW FN24", true},
		{"CQ CO8LY FL20", true},        // CO (Cuba)
		{"VE2FVV WB5AEE R-09", true},   // VE Canada
		{"LZ2II KB1ZBZ R-05", true},    // LZ Bulgaria
		{"CQ TI5RTZ EK70", true},       // TI Costa Rica — regression guard
		{"CQ OX8YPZ PC08", true},       // OX Greenland — regression guard
		{"CQ POTA KE8WCR EN80", true},  // POTA activity CQ
		{"CQ SOTA G4ABC IO91", true},   // SOTA activity CQ
		{"<W1AW/9> K0RAR EM28", true},  // hash in call1
		{"8A60BTG <K2TQC> RR73", true}, // hash in call2

		// Phantoms with structurally-valid but likely-unallocated prefixes.
		{"CQ QA5XYZ AA00", false},   // QA — Q-codes, not allocated
		{"MQ5XYZ CALL AA00", false}, // MQ — unassigned UK block
	}
	for _, c := range cases {
		got := messageHasAllocatedCall(c.text)
		if got != c.want {
			t.Errorf("messageHasAllocatedCall(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestIsValidCallsignToken(t *testing.T) {
	cases := []struct {
		t    string
		want bool
	}{
		{"KO6IEH", true},
		{"W1AW", true},
		{"N8DH", true},
		{"3D2AG", true},  // legit Fiji prefix starting with digit
		{"7J1ABC", true}, // legit Japan digit-first prefix
		{"4U1UN", true},  // legit UN New York digit-first prefix
		{"W1AW/P", true}, // portable suffix
		{"G0ABC/M", true},
		{"K70", false},    // no letter suffix
		{"200BSQ", false}, // all-digit prefix
		{"290ACR", false},
		{"V31DL", true},   // Belize: letter+digit prefix, area digit, letter suffix
		{"G52ZRE", true},  // structurally valid per reference-design n28; phantom filter lives elsewhere
		{"CQ", false},     // too short / no digit
		{"DN15", false},   // grid
		{"DM13", false},   // grid
		{"1234", false},   // all digits
		{"ABCDEF", false}, // no digit
	}
	for _, c := range cases {
		got := isValidCallsignToken(c.t)
		if got != c.want {
			t.Errorf("isValidCallsignToken(%q) = %v, want %v", c.t, got, c.want)
		}
	}
}
