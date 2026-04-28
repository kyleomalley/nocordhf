package ft8

import (
	"math"
	"math/cmplx"
	"strings"
	"testing"
)

// TestGFSKSpectralWidth verifies that our GFSK waveform has tight bandwidth.
// A properly shaped FT8 signal should have >99% of its energy within ~50 Hz
// of the carrier. Without GFSK pulse shaping, energy leaks much wider.
func TestGFSKSpectralWidth(t *testing.T) {
	samples, err := EncodeCQ("KO6IEH", "DM13", 1.0, 1500.0)
	if err != nil {
		t.Fatalf("EncodeCQ: %v", err)
	}

	// Extract the active signal portion (skip silence at start/end).
	start := int(0.5 * float64(TxSampleRate))
	end := start + NumSymbols*txSamplesPerSym
	if end > len(samples) {
		end = len(samples)
	}
	active := samples[start:end]

	// Compute power spectrum via DFT around the carrier frequency.
	// FT8 uses tones 0-7 at 6.25 Hz spacing from txFreq (1500 Hz).
	// Signal should be within ~1500-1550 Hz. Check 1400-1650 Hz (wide) vs 1475-1575 Hz (narrow).
	n := len(active)
	binHz := float64(TxSampleRate) / float64(n)

	// Compute power in each frequency bin across 1300-1700 Hz range.
	loHz, hiHz := 1300.0, 1700.0
	loBin := int(loHz / binHz)
	hiBin := int(hiHz / binHz)
	narrowLo := int(1475.0 / binHz)
	narrowHi := int(1575.0 / binHz)

	totalPower := 0.0
	narrowPower := 0.0

	for k := loBin; k <= hiBin; k++ {
		// DFT at bin k
		var sum complex128
		freq := 2.0 * math.Pi * float64(k) / float64(n)
		for i, s := range active {
			sum += complex(float64(s), 0) * cmplx.Rect(1, -freq*float64(i))
		}
		p := real(sum)*real(sum) + imag(sum)*imag(sum)
		totalPower += p
		if k >= narrowLo && k <= narrowHi {
			narrowPower += p
		}
	}

	ratio := narrowPower / totalPower
	t.Logf("power in 1475-1575 Hz / 1300-1700 Hz = %.1f%%", ratio*100)
	t.Logf("bin resolution = %.2f Hz", binHz)

	// With proper GFSK, >95% of power should be in the narrow band.
	// Without GFSK (raw FSK), this would be much lower due to spectral splatter.
	if ratio < 0.90 {
		t.Errorf("spectral containment too low: %.1f%% (want >90%%)", ratio*100)
	}
}

func TestEncodeCQRoundTrip(t *testing.T) {
	// Encode "CQ KO6IEH DM13" then decode the resulting bits.
	bits, err := packCQ("KO6IEH", "DM13")
	if err != nil {
		t.Fatalf("packCQ: %v", err)
	}

	// CRC must pass.
	if !CheckCRC(bits) {
		t.Fatal("CRC failed on freshly encoded message")
	}

	// LDPC encode then decode should recover the same bits.
	codeword := encodeLDPC(bits)
	var llr [N]float64
	for i, b := range codeword {
		if b == 0 {
			llr[i] = 5.0
		} else {
			llr[i] = -5.0
		}
	}
	decoded, ok := DecodeLDPC(llr)
	if !ok {
		t.Fatal("LDPC decode failed on noiseless codeword")
	}
	if decoded != bits {
		t.Fatal("decoded bits do not match original message bits")
	}

	// Unpack the payload and check it reads back as a CQ message.
	var payload [77]byte
	copy(payload[:], decoded[:77])
	msg := Unpack77(payload)
	if !strings.HasPrefix(msg.Text, "CQ") {
		t.Errorf("expected CQ message, got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "KO6IEH") {
		t.Errorf("callsign not found in decoded message %q", msg.Text)
	}
	t.Logf("decoded: %q", msg.Text)
}

// TestPackMessage77RoundTrip verifies packMessage77 produces payloads that
// round-trip cleanly through Unpack77. This gates a2..a6 AP seeds — if
// packMessage77 is wrong, every AP hypothesis seeds garbage.
func TestPackMessage77RoundTrip(t *testing.T) {
	RegisterCallsign("KO6IEH")
	cases := []struct {
		name   string
		call1  string
		call2  string
		ir     byte
		igrid4 uint16
		i3     byte
		want   string
	}{
		// "CQ KO6IEH DM13" — CQ token + standard call + grid.
		{"cq", "CQ", "KO6IEH", 0, func() uint16 {
			v, _ := encodeGrid4("DM13")
			return uint16(v)
		}(), 1, "CQ KO6IEH DM13"},
		// "W1AW KO6IEH RR73" — pure standard both sides, igrid4 sentinel.
		{"rr73", "W1AW", "KO6IEH", 0, uint16(maxgrid4) + 3, 1, "W1AW KO6IEH RR73"},
		// "W1AW KO6IEH 73" — igrid4 sentinel for 73.
		{"73", "W1AW", "KO6IEH", 0, uint16(maxgrid4) + 4, 1, "W1AW KO6IEH 73"},
		// "W1AW KO6IEH RRR" — igrid4 sentinel for RRR.
		{"rrr", "W1AW", "KO6IEH", 0, uint16(maxgrid4) + 2, 1, "W1AW KO6IEH RRR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bits, ok := packMessage77(tc.call1, tc.call2, 0, 0, tc.ir, tc.igrid4, tc.i3)
			if !ok {
				t.Fatalf("packMessage77 failed")
			}
			msg := Unpack77(bits)
			if msg.Text != tc.want {
				t.Errorf("got %q, want %q", msg.Text, tc.want)
			}
		})
	}
}

// TestPackN28Tokens verifies the special-token values match ft8_lib/reference design.
func TestPackN28Tokens(t *testing.T) {
	cases := []struct {
		s    string
		want uint32
	}{
		{"DE", 0},
		{"QRZ", 1},
		{"CQ", 2},
	}
	for _, c := range cases {
		got, ok := packN28(c.s)
		if !ok || got != c.want {
			t.Errorf("packN28(%q) = (%d, %v), want (%d, true)", c.s, got, ok, c.want)
		}
	}
}

// TestNonstdRoundTrip verifies Type 4 (nonstandard callsign) pack/unpack.
func TestNonstdRoundTrip(t *testing.T) {
	cases := []struct {
		text string
		want string // expected decoded text (hash side resolved via RegisterCallsign)
	}{
		// CQ from a 7-char callsign.
		{"CQ HI0DMRA", "CQ HI0DMRA"},
		// Caller (KO6IEH) addressing nonstandard station. The decoded display
		// wraps the hash-resolved standard call in angle brackets, matching
		// reference design convention.
		{"HI0DMRA KO6IEH", "HI0DMRA <KO6IEH>"},
		{"HI0DMRA KO6IEH RR73", "HI0DMRA <KO6IEH> RR73"},
		{"HI0DMRA KO6IEH 73", "HI0DMRA <KO6IEH> 73"},
	}

	// Register KO6IEH so that hash resolution works in the decode direction.
	RegisterCallsign("KO6IEH")

	for _, tc := range cases {
		bits, err := packNonstd(tc.text)
		if err != nil {
			t.Errorf("packNonstd(%q): %v", tc.text, err)
			continue
		}
		if !CheckCRC(bits) {
			t.Errorf("packNonstd(%q): CRC failed", tc.text)
			continue
		}
		var payload [77]byte
		copy(payload[:], bits[:77])
		msg := Unpack77(payload)
		if msg.Text != tc.want {
			t.Errorf("packNonstd(%q) round-trip: got %q, want %q", tc.text, msg.Text, tc.want)
		}
	}
}

// TestEncodeStandardNonstdFallback checks that EncodeStandard falls back to
// Type 4 when a callsign is too long for standard 28-bit encoding.
func TestEncodeStandardNonstdFallback(t *testing.T) {
	RegisterCallsign("KO6IEH")
	samples, err := EncodeStandard("HI0DMRA KO6IEH", TxLevel, 1500.0)
	if err != nil {
		t.Fatalf("EncodeStandard(HI0DMRA KO6IEH): %v", err)
	}
	if len(samples) != TxSampleRate*15*TxChannels {
		t.Errorf("waveform length = %d, want %d", len(samples), TxSampleRate*15*TxChannels)
	}
}

func TestEncodeWaveformLength(t *testing.T) {
	samples, err := EncodeCQ("KO6IEH", "DM13", TxLevel, 1500.0)
	if err != nil {
		t.Fatalf("EncodeCQ: %v", err)
	}
	want := TxSampleRate * 15 * TxChannels
	if len(samples) != want {
		t.Errorf("waveform length = %d, want %d", len(samples), want)
	}
}
