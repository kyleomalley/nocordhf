package ft8

import "testing"

// TestOSDNoiseless: with crystal-clear LLRs, OSD's order-0 candidate (hard
// decisions on the MRB) must recover the exact message.
func TestOSDNoiseless(t *testing.T) {
	bits, err := packCQ("KO6IEH", "DM13")
	if err != nil {
		t.Fatalf("packCQ: %v", err)
	}
	cw := encodeLDPC(bits)
	var llr [N]float64
	for i, b := range cw {
		if b == 0 {
			llr[i] = 5.0
		} else {
			llr[i] = -5.0
		}
	}
	got, ok := decodeOSD(llr)
	if !ok {
		t.Fatal("OSD returned !ok on noiseless codeword")
	}
	if got != bits {
		t.Fatalf("OSD recovered wrong message")
	}
}

// TestOSDWithFlips: inject a handful of bit-flips (LLRs with wrong sign) at
// LEAST-reliable positions. OSD-2 should still recover.
func TestOSDWithFlips(t *testing.T) {
	bits, err := packCQ("KO6IEH", "DM13")
	if err != nil {
		t.Fatalf("packCQ: %v", err)
	}
	cw := encodeLDPC(bits)
	// Base LLRs with strong confidence for most bits, weak for a few.
	var llr [N]float64
	for i, b := range cw {
		mag := 5.0
		if i%30 == 0 { // 6 weak positions
			mag = 0.3
		}
		if b == 0 {
			llr[i] = mag
		} else {
			llr[i] = -mag
		}
	}
	// Flip sign of two weak LLRs: those become errors BP might not fix.
	llr[0] = -llr[0]
	llr[30] = -llr[30]

	got, ok := decodeOSD(llr)
	if !ok {
		t.Fatal("OSD returned !ok with weight-2 flips on weak positions")
	}
	if got != bits {
		t.Fatalf("OSD recovered wrong message")
	}
}
