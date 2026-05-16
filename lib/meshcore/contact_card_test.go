package meshcore

import (
	"strings"
	"testing"
)

func TestContactCardRoundTrip(t *testing.T) {
	in := ContactCard{
		Type:     AdvTypeChat,
		AdvLatE6: 33023292,
		AdvLonE6: -117078028,
		Name:     "Kohaku",
	}
	for i := range in.PubKey {
		in.PubKey[i] = byte(i ^ 0xa5)
	}
	url := EncodeContactCard(in)
	if !strings.HasPrefix(url, ContactCardURLPrefix) {
		t.Fatalf("encoded URL missing prefix: %q", url)
	}
	if len(url) > 140 {
		t.Fatalf("encoded URL %d bytes — exceeds MeshCore text cap", len(url))
	}
	out, err := DecodeContactCard(url)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PubKey != in.PubKey {
		t.Errorf("pubkey round-trip: got %x want %x", out.PubKey, in.PubKey)
	}
	if out.Type != in.Type {
		t.Errorf("type round-trip: got %d want %d", out.Type, in.Type)
	}
	if out.AdvLatE6 != in.AdvLatE6 || out.AdvLonE6 != in.AdvLonE6 {
		t.Errorf("lat/lon round-trip: got (%d,%d) want (%d,%d)", out.AdvLatE6, out.AdvLonE6, in.AdvLatE6, in.AdvLonE6)
	}
	if out.Name != in.Name {
		t.Errorf("name round-trip: got %q want %q", out.Name, in.Name)
	}
}

// Even at the worst-case 32-byte name, the URL must fit in one
// MeshCore channel text message (140-byte on-air cap).
func TestContactCardFitsInChannelMessage(t *testing.T) {
	in := ContactCard{
		Type:     AdvTypeRepeater,
		AdvLatE6: -33868820,
		AdvLonE6: 151209290,
		Name:     "thirty-two-character-name-aaaa12", // exactly 32 bytes
	}
	url := EncodeContactCard(in)
	if len(url) > 140 {
		t.Errorf("encoded card too long for channel message: %d bytes\n%s", len(url), url)
	}
}

func TestContactCardOversizedNameTruncates(t *testing.T) {
	in := ContactCard{
		Type: AdvTypeChat,
		Name: strings.Repeat("x", 64),
	}
	url := EncodeContactCard(in)
	out, err := DecodeContactCard(url)
	if err != nil {
		t.Fatalf("decode after truncation: %v", err)
	}
	if len(out.Name) != 32 {
		t.Errorf("expected 32-byte truncated name, got %d", len(out.Name))
	}
}

func TestContactCardRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"missing prefix":  "not-a-card",
		"bad base64":      "mc://contact/!!!not-base64!!!",
		"truncated body":  "mc://contact/AQID",                     // 3 bytes after b64 decode
		"unknown version": EncodeContactCard(ContactCard{}) + "FF", // mangled tail
	}
	// Build an explicit version-bumped sample for "unknown version".
	cases["unknown version"] = "mc://contact/" + "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAA"
	for name, url := range cases {
		if _, err := DecodeContactCard(url); err == nil {
			t.Errorf("%s: expected error, got nil for %q", name, url)
		}
	}
}
