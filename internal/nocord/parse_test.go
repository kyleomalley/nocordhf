package nocord

import (
	"strings"
	"testing"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// Regression: lat/long pairs in chat (e.g. "34.14289, -118.03159"
// posted in #wardriving) should render as Google Maps links. Pins
// the regex against false-positives ("1, 2", message-id pairs)
// and out-of-range values that look superficially valid.
// Regression: mc://contact/<base64> URLs in chat must be detected
// as contact-card segments (with a decoded card payload), not
// confused with regular http(s) URLs by the URL regex.
func TestMcParseChatSegmentsContactCard(t *testing.T) {
	card := meshcore.ContactCard{
		Type:     meshcore.AdvTypeChat,
		AdvLatE6: 33023292,
		AdvLonE6: -117078028,
		Name:     "Kohaku",
	}
	for i := range card.PubKey {
		card.PubKey[i] = byte(i ^ 0x5a)
	}
	cardURL := meshcore.EncodeContactCard(card)
	cases := []struct {
		name        string
		in          string
		wantPubName string // expected card.Name when a card is found, "" when not
	}{
		{
			"bare card",
			cardURL,
			card.Name,
		},
		{
			"card with surrounding chat",
			"check this out: " + cardURL + " — KO9OXR you nearby?",
			card.Name,
		},
		{
			"plain http URL is not a card",
			"https://example.com/page",
			"",
		},
		{
			"malformed card decodes to nothing",
			"mc://contact/!!!",
			"",
		},
	}
	for _, c := range cases {
		segs := mcParseChatSegments(c.in, nil, "")
		var foundName string
		for _, s := range segs {
			if s.card != nil {
				foundName = s.card.Name
				break
			}
		}
		if foundName != c.wantPubName {
			t.Errorf("%s: got card name %q, want %q", c.name, foundName, c.wantPubName)
		}
	}
}

func TestMcParseChatSegmentsGeo(t *testing.T) {
	cases := []struct {
		in       string
		wantHref string // empty when no geo segment is expected
	}{
		// Canonical positive case from the user's report.
		{"see this 34.14289, -118.03159 great spot", "https://www.google.com/maps?q=34.14289,-118.03159"},
		// Both negative (Sydney).
		{"-33.86882, 151.20929", "https://www.google.com/maps?q=-33.86882,151.20929"},
		// Comma without space.
		{"40.7128,-74.0060", "https://www.google.com/maps?q=40.7128,-74.0060"},
		// Sentence-trailing punctuation must not pollute the href.
		{"meet at 51.5074, -0.1278.", "https://www.google.com/maps?q=51.5074,-0.1278"},
		// Out-of-range latitude → not a geo link.
		{"id pair 91.5, 12.3", ""},
		// Out-of-range longitude.
		{"id pair 12.3, 200.0", ""},
		// Integer-only commas should NOT match (would false-positive
		// on enumerations like "1, 2, 3").
		{"items 1, 2, 3 in stock", ""},
		// No comma → no match.
		{"34.14289 -118.03159", ""},
	}
	for _, c := range cases {
		got := mcParseChatSegments(c.in, nil, "")
		var href string
		for _, s := range got {
			if s.url != "" && strings.HasPrefix(s.url, "https://www.google.com/maps?q=") {
				href = s.url
				break
			}
		}
		if href != c.wantHref {
			t.Errorf("geo parse %q → %q, want %q", c.in, href, c.wantHref)
		}
	}
}

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
