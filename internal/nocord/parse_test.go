package nocord

import "testing"

// Regression: "CQ NA BH4ECL" was returning "NA" because the inline modifier
// list here had drifted from lib/ft8.IsCQModifier and only knew about DX /
// POTA / SOTA / TEST. The fix routes both parsers through the canonical
// helper; this test pins the behaviour for every documented modifier so a
// future drift is caught at test time instead of in the field.
func TestRemoteCallFromMessage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"CQ KO6IEH DM87", "KO6IEH"},
		{"CQ DX KO6IEH DM87", "KO6IEH"},
		{"CQ NA BH4ECL", "BH4ECL"}, // the bug
		{"CQ EU G0ABC IO91", "G0ABC"},
		{"CQ POTA KE8WCR EN80", "KE8WCR"},
		{"CQ 3 UA9CC MO26", "UA9CC"},
		{"KO6IEH BG2ATH PN26", "KO6IEH"},
		{"", ""},
	}
	for _, c := range cases {
		if got := remoteCallFromMessage(c.in); got != c.want {
			t.Errorf("remoteCallFromMessage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSenderFromMessage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"CQ KO6IEH DM87", "KO6IEH"},
		{"CQ DX KO6IEH DM87", "KO6IEH"},
		{"CQ NA BH4ECL", "BH4ECL"},
		{"CQ EU G0ABC IO91", "G0ABC"},
		{"CQ POTA KE8WCR EN80", "KE8WCR"},
		{"KO6IEH BG2ATH PN26", "BG2ATH"},
		{"<...> BG2ATH PN26", "BG2ATH"}, // hashed first token → sender is the second
		{"BG2ATH <...> PN26", ""},       // sender position is hashed → reject
	}
	for _, c := range cases {
		if got := senderFromMessage(c.in); got != c.want {
			t.Errorf("senderFromMessage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
