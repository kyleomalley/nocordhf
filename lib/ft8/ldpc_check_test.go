package ft8

import (
	"testing"
)

func TestLDPCAllZeros(t *testing.T) {
	// All-zeros codeword: positive LLR should decode to all zeros
	var llr [N]float64
	for i := range llr { llr[i] = 5.0 }  // strong "bit=0" belief
	bits, ok := DecodeLDPC(llr)
	if !ok {
		t.Error("all-zeros codeword should always pass parity")
	}
	for i, b := range bits {
		if b != 0 {
			t.Errorf("bit[%d]=%d want 0", i, b)
		}
	}
}

func TestLDPCParityTable(t *testing.T) {
	// parityChecks uses 1-based indices; 0 = padding.
	for i, row := range parityChecks {
		for _, j := range row {
			if j == 0 {
				continue // padding
			}
			if j < 1 || j > N {
				t.Errorf("parityChecks[%d] has out-of-range index %d", i, j)
			}
		}
	}
}
