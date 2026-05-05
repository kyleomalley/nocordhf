package meshcore

import (
	"encoding/hex"
	"testing"
)

// TestDeriveHashtagChannelSecretVolcano locks the algorithm against
// a known reference: the live #volcano community channel's secret
// is `ac89b14740496815cf2ccc99c3522330`, which is exactly the first
// 16 bytes of SHA-256("#volcano"). If a future firmware change
// switches the derivation, this test breaks first — saving us the
// "why are messages not decrypting" debug session.
func TestDeriveHashtagChannelSecretVolcano(t *testing.T) {
	const wantHex = "ac89b14740496815cf2ccc99c3522330"
	got := DeriveHashtagChannelSecret("#volcano")
	if hex.EncodeToString(got[:]) != wantHex {
		t.Fatalf("derived %x; want %s", got, wantHex)
	}
}

// TestIsHashtagChannelName covers the few edge cases callers care
// about — bare "#" doesn't qualify, plain text doesn't, anything
// starting with "#" + at least one more char does.
func TestIsHashtagChannelName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"#volcano", true},
		{"#meshbud", true},
		{"#a", true},
		{"#", false},
		{"", false},
		{"public", false},
		{"Public", false},
	} {
		if got := IsHashtagChannelName(c.name); got != c.want {
			t.Errorf("IsHashtagChannelName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
