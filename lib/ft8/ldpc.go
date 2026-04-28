package ft8

// LDPC codec for FT8: [174, 91] code with 83 parity bits.
//
// The reordered form means
// plain[0..90] are the 91 systematic message bits directly after decoding.
//
// Each row lists 1-indexed codeword bit positions; 0 = padding.

// parityChecks: 83 parity check rows, 1-indexed, 0 = padding (max 7 per row).
var parityChecks = [83][7]int{
	{4, 31, 59, 91, 92, 96, 153},
	{5, 32, 60, 93, 115, 146, 0},
	{6, 24, 61, 94, 122, 151, 0},
	{7, 33, 62, 95, 96, 143, 0},
	{8, 25, 63, 83, 93, 96, 148},
	{6, 32, 64, 97, 126, 138, 0},
	{5, 34, 65, 78, 98, 107, 154},
	{9, 35, 66, 99, 139, 146, 0},
	{10, 36, 67, 100, 107, 126, 0},
	{11, 37, 67, 87, 101, 139, 158},
	{12, 38, 68, 102, 105, 155, 0},
	{13, 39, 69, 103, 149, 162, 0},
	{8, 40, 70, 82, 104, 114, 145},
	{14, 41, 71, 88, 102, 123, 156},
	{15, 42, 59, 106, 123, 159, 0},
	{1, 33, 72, 106, 107, 157, 0},
	{16, 43, 73, 108, 141, 160, 0},
	{17, 37, 74, 81, 109, 131, 154},
	{11, 44, 75, 110, 121, 166, 0},
	{45, 55, 64, 111, 130, 161, 173},
	{8, 46, 71, 112, 119, 166, 0},
	{18, 36, 76, 89, 113, 114, 143},
	{19, 38, 77, 104, 116, 163, 0},
	{20, 47, 70, 92, 138, 165, 0},
	{2, 48, 74, 113, 128, 160, 0},
	{21, 45, 78, 83, 117, 121, 151},
	{22, 47, 58, 118, 127, 164, 0},
	{16, 39, 62, 112, 134, 158, 0},
	{23, 43, 79, 120, 131, 145, 0},
	{19, 35, 59, 73, 110, 125, 161},
	{20, 36, 63, 94, 136, 161, 0},
	{14, 31, 79, 98, 132, 164, 0},
	{3, 44, 80, 124, 127, 169, 0},
	{19, 46, 81, 117, 135, 167, 0},
	{7, 49, 58, 90, 100, 105, 168},
	{12, 50, 61, 118, 119, 144, 0},
	{13, 51, 64, 114, 118, 157, 0},
	{24, 52, 76, 129, 148, 149, 0},
	{25, 53, 69, 90, 101, 130, 156},
	{20, 46, 65, 80, 120, 140, 170},
	{21, 54, 77, 100, 140, 171, 0},
	{35, 82, 133, 142, 171, 174, 0},
	{14, 30, 83, 113, 125, 170, 0},
	{4, 29, 68, 120, 134, 173, 0},
	{1, 4, 52, 57, 86, 136, 152},
	{26, 51, 56, 91, 122, 137, 168},
	{52, 84, 110, 115, 145, 168, 0},
	{7, 50, 81, 99, 132, 173, 0},
	{23, 55, 67, 95, 172, 174, 0},
	{26, 41, 77, 109, 141, 148, 0},
	{2, 27, 41, 61, 62, 115, 133},
	{27, 40, 56, 124, 125, 126, 0},
	{18, 49, 55, 124, 141, 167, 0},
	{6, 33, 85, 108, 116, 156, 0},
	{28, 48, 70, 85, 105, 129, 158},
	{9, 54, 63, 131, 147, 155, 0},
	{22, 53, 68, 109, 121, 174, 0},
	{3, 13, 48, 78, 95, 123, 0},
	{31, 69, 133, 150, 155, 169, 0},
	{12, 43, 66, 89, 97, 135, 159},
	{5, 39, 75, 102, 136, 167, 0},
	{2, 54, 86, 101, 135, 164, 0},
	{15, 56, 87, 108, 119, 171, 0},
	{10, 44, 82, 91, 111, 144, 149},
	{23, 34, 71, 94, 127, 153, 0},
	{11, 49, 88, 92, 142, 157, 0},
	{29, 34, 87, 97, 147, 162, 0},
	{30, 50, 60, 86, 137, 142, 162},
	{10, 53, 66, 84, 112, 128, 165},
	{22, 57, 85, 93, 140, 159, 0},
	{28, 32, 72, 103, 132, 166, 0},
	{28, 29, 84, 88, 117, 143, 150},
	{1, 26, 45, 80, 128, 147, 0},
	{17, 27, 89, 103, 116, 153, 0},
	{51, 57, 98, 163, 165, 172, 0},
	{21, 37, 73, 138, 152, 169, 0},
	{16, 47, 76, 130, 137, 154, 0},
	{3, 24, 30, 72, 104, 139, 0},
	{9, 40, 90, 106, 134, 151, 0},
	{15, 58, 60, 74, 111, 150, 163},
	{18, 42, 79, 144, 146, 152, 0},
	{25, 38, 65, 99, 122, 160, 0},
	{17, 42, 75, 129, 170, 172, 0},
}

const (
	N       = 174 // codeword length
	K       = 91  // systematic message bits
	M       = 83  // parity checks
	maxIter = 100 // BP iterations
)

// ptanh is a fast Padé-(4,4) approximation of tanh:
//
//	tanh(x) ≈ x(135135 + 17325x² + 378x⁴ + x⁶) / (135135 + 62370x² + 3150x⁴ + 28x⁶)
//
// Max error ~1e-4 in [-3, 3] (Padé-(2,2) had ~0.02). Replaces math.Tanh which
// was ~33% of decode CPU on linux/amd64. F1 within noise on the corpus — the
// BP fixed point is insensitive to this much error.
func ptanh(x float64) float64 {
	if x > 4.5 {
		return 0.99999
	}
	if x < -4.5 {
		return -0.99999
	}
	x2 := x * x
	x4 := x2 * x2
	x6 := x4 * x2
	num := 135135.0 + 17325.0*x2 + 378.0*x4 + x6
	den := 135135.0 + 62370.0*x2 + 3150.0*x4 + 28.0*x6
	return x * num / den
}

// platanh is reference design piecewise-linear approximation of atanh, used inside
// the BP check-node update. ~5× faster than math.Atanh for inputs in the
// range BP actually sees, with negligible decode-rate impact (the BP fixed
// point isn't sensitive to the exact functional form here). Mirrors
// platanh.f90.
func platanh(x float64) float64 {
	sign := 1.0
	z := x
	if x < 0 {
		sign = -1
		z = -x
	}
	switch {
	case z <= 0.664:
		return x / 0.83
	case z <= 0.9217:
		return sign * (z - 0.4064) / 0.322
	case z <= 0.9951:
		return sign * (z - 0.8378) / 0.0524
	case z <= 0.9998:
		return sign * (z - 0.9914) / 0.0012
	default:
		return sign * 7.0
	}
}

// varToChecks[i] is the list of parity-check rows that include variable i.
// Each variable appears in only 3 of the 83 parity checks (174×3 = 522
// non-zero entries in the sparse 174×83 incidence matrix), so iterating
// this list during BP's variable-node update is ~28× faster than the
// dense `for j := 0; j < M` loop that re-reads zero extrinsics.
// Built once at package init from parityChecks.
var varToChecks [N][]int

func init() {
	for j := 0; j < M; j++ {
		for _, j1 := range parityChecks[j] {
			if j1 == 0 {
				continue
			}
			i := j1 - 1
			varToChecks[i] = append(varToChecks[i], j)
		}
	}
}

// DecodeLDPC performs belief-propagation (sum-product) decoding on a soft codeword.
// llr is an array of 174 log-likelihood ratios (positive = bit 0, negative = bit 1).
// Returns the decoded 91 message bits and true if all parity checks pass.
func DecodeLDPC(llr [N]float64) ([K]byte, bool) {
	result, ok, _ := DecodeLDPCEx(llr)
	return result, ok
}

// DecodeLDPCEx is DecodeLDPC with BP-iterate snapshots. In addition to the
// hard-decision return, it captures the accumulated posterior LLR sum at the
// end of each of the first 5 BP iterations. reference design decode174_91.f90 feeds
// these "zsum" snapshots to OSD when BP fails to converge — OSD's MRB
// reliability ordering on a BP-softened LLR vector usually picks a different
// basis than on the raw channel LLRs, which directly rescues the OSD-on-
// phantom failure mode (the real codeword's MRB isn't in the raw-LLR top-K
// most-reliable set, but it IS in the BP-softened top-K).
//
// snapshots[0] = posterior after iter 1 (Fortran zsave[1]), snapshots[k] = after iter k+1.
// Corpus sweep: 3 → F1 0.793, 5 → F1 0.795 (peak), 7 → F1 0.791 (later iters
// add phantoms — BP posteriors past iter 5 start landing on noise patterns).
func DecodeLDPCEx(llr [N]float64) ([K]byte, bool, [5][N]float64) {
	var snapshots [5][N]float64
	// m[j][i] = message from check j to variable i, initialised to channel LLR
	var m [M][N]float64
	var e [M][N]float64
	for j := 0; j < M; j++ {
		for i := 0; i < N; i++ {
			m[j][i] = llr[i]
		}
	}

	// zsum accumulates the posterior LLR across BP iterations. Seeded with the
	// iteration-0 posterior (which is just llr since tov=0 before any check
	// update), matching decode174_91.f90:51-61.
	var zsum [N]float64
	copy(zsum[:], llr[:])

	var result [K]byte
	minErrors := M
	// Early-exit bookkeeping: if parity-error count hasn't improved in
	// `stallLimit` iterations AND we're still further than `stallMinError`
	// from convergence, the candidate is almost certainly noise that BP will
	// never resolve. Real-but-hard signals (e.g. -23 dB with LLR agreement
	// ~0.77 against the true codeword) usually show *some* downward progress
	// in the first ~30 iters; pure noise sits flat at errors > ~40 from
	// iter 5 onward. stallMinError=15 lets candidates that have made it down
	// near convergence (10–15 errors) keep iterating — corpus tested 10 vs 15
	// vs 20: 15 is the sweet spot (+1 TP vs 10, no phantom cost; 20 admits
	// 4 more phantoms).
	iterSinceImproved := 0
	const (
		stallLimit    = 15
		stallMinError = 15
	)

	for iter := 0; iter < maxIter; iter++ {
		// Check-node update (sum-product / tanh rule).
		//
		// The extrinsic e[j][i1] uses the product of tanh(m[j][i2]/2) over all
		// i2 in this check EXCEPT i1. The naive formulation recomputes that
		// product for every (i1, i2) pair — O(rowSize²) tanh ops per row.
		//
		// Here we compute tanh values once per entry, build a leading-product
		// prefix L[k] = Π(t[0..k-1]), then walk right-to-left with a running
		// trailing product R to get "full product except k" = L[k] * R in O(1).
		// Per-row work drops from O(rowSize²) to O(rowSize) — ~7× speedup on
		// FT8 rows of up to 7 non-zero entries, the hottest inner loop in BP.
		for j := 0; j < M; j++ {
			row := &parityChecks[j]
			var n int
			var idx [7]int
			var t [7]float64
			for _, j1 := range row {
				if j1 == 0 {
					continue
				}
				i := j1 - 1
				idx[n] = i
				t[n] = ptanh(m[j][i] / 2.0)
				n++
			}
			var L [8]float64
			L[0] = 1.0
			for k := 0; k < n; k++ {
				L[k+1] = L[k] * t[k]
			}
			R := 1.0
			for k := n - 1; k >= 0; k-- {
				a := L[k] * R
				if a > 0.99999 {
					a = 0.99999
				} else if a < -0.99999 {
					a = -0.99999
				}
				e[j][idx[k]] = 2.0 * platanh(a)
				R *= t[k]
			}
		}

		// Variable-node hard decision, parity check, and posterior accumulation
		// for OSD-via-BP-snapshot. The posterior ll = llr + sum(extrinsics) is
		// the per-bit soft estimate this iteration; zsum accumulates it across
		// iterations. Early-loop snapshots (iter 0..2) are saved as LLR inputs
		// OSD can fall back to when BP fails to converge.
		var plain [N]byte
		for i := 0; i < N; i++ {
			ll := llr[i]
			for _, j := range varToChecks[i] {
				ll += e[j][i]
			}
			zsum[i] += ll
			if ll <= 0 {
				plain[i] = 1
			}
		}
		if iter < 5 {
			snapshots[iter] = zsum
		}
		errors := parityErrors(plain)
		if errors < minErrors {
			minErrors = errors
			iterSinceImproved = 0
			if errors == 0 {
				copy(result[:], plain[:K])
				return result, true, snapshots
			}
		} else {
			iterSinceImproved++
			if iterSinceImproved >= stallLimit && minErrors > stallMinError {
				break
			}
		}

		// Variable-node message update with damping. Without damping, synchronous
		// flooding BP oscillates on marginal-SNR signals and can sit at a few
		// parity errors for many iterations without converging. Observed on
		// "TA1RJV K5NCW R-04" @ 2009 Hz -23 dB: sync=15, LLR-agree 0.776 with
		// the true codeword (clearly a real signal), yet BP never finds it.
		// α = 0.7 matches reference design FT8 decoder.
		const alpha = 0.7
		for i := 0; i < N; i++ {
			checks := varToChecks[i]
			sum := llr[i]
			for _, j := range checks {
				sum += e[j][i]
			}
			for _, j := range checks {
				target := sum - e[j][i]
				m[j][i] = alpha*target + (1-alpha)*m[j][i]
			}
		}
	}

	// Return best hard decision. If BP ran fewer than 3 iterations (stall-exit
	// very early) the later snapshot slots are still zero — OSD will treat
	// them as uninformative LLR and skip them harmlessly.
	var plain [N]byte
	for i := 0; i < N; i++ {
		ll := llr[i]
		for _, j := range varToChecks[i] {
			ll += e[j][i]
		}
		if ll <= 0 {
			plain[i] = 1
		}
	}
	copy(result[:], plain[:K])
	return result, false, snapshots
}

func parityErrors(bits [N]byte) int {
	n := 0
	for _, row := range parityChecks {
		sum := byte(0)
		for _, j1 := range row {
			if j1 == 0 {
				continue
			}
			sum ^= bits[j1-1]
		}
		if sum != 0 {
			n++
		}
	}
	return n
}

func checkParity(bits [N]byte) bool {
	return parityErrors(bits) == 0
}

// crc14Table is the byte-stride lookup for CRC14 with polynomial 0x2757.
// Replaces the bit-by-bit inner loop in crc14 — ~8× faster, and CheckCRC
// was 16% of decode CPU before this. Computed once at package init.
var crc14Table [256]uint16

func init() {
	const poly = uint16(0x2757)
	for b := 0; b < 256; b++ {
		r := uint16(b) << 6
		for k := 0; k < 8; k++ {
			if r&(1<<13) != 0 {
				r = (r << 1) ^ poly
			} else {
				r <<= 1
			}
		}
		crc14Table[b] = r & 0x3FFF
	}
}

// crc14 computes a 14-bit CRC over num_bits bits of msg (MSB first, byte-packed).
// Matches ft8_lib ftx_compute_crc with polynomial 0x2757.
//
// Table-driven byte stride: for each input byte, XOR-fold with the precomputed
// CRC14 table and shift the remainder left 8. Trailing partial-byte bits (up
// to 7) fall back to the bit-by-bit path.
func crc14(msg []byte, numBits int) uint16 {
	const poly = uint16(0x2757)
	const topbit = uint16(1 << 13)
	remainder := uint16(0)
	fullBytes := numBits / 8
	for i := 0; i < fullBytes; i++ {
		// Top 6 bits of remainder XOR with msg byte to form table index;
		// shift remainder up by 8 and XOR with the table entry.
		idx := byte(remainder>>6) ^ msg[i]
		remainder = ((remainder << 8) ^ crc14Table[idx]) & 0x3FFF
	}
	// Remaining bits (0..7) of the last byte.
	tailBits := numBits - fullBytes*8
	if tailBits > 0 {
		remainder ^= uint16(msg[fullBytes]) << 6
		for k := 0; k < tailBits; k++ {
			if remainder&topbit != 0 {
				remainder = (remainder << 1) ^ poly
			} else {
				remainder <<= 1
			}
		}
	}
	return remainder & 0x3FFF
}

// CheckCRC verifies the 14-bit CRC over the 91-bit decoded codeword.
// Matches ft8_lib: pack 91 bits into bytes, zero bits 77-90 (CRC field),
// compute CRC over 82 bits (= 96 - 14), compare to stored CRC in bits 77-90.
func CheckCRC(bits [K]byte) bool {
	// Pack 91 bits MSB-first into 12 bytes
	var b [12]byte
	for i := 0; i < K; i++ {
		if bits[i]&1 == 1 {
			b[i/8] |= 1 << (7 - uint(i%8))
		}
	}

	// Extract stored CRC from bits 77-90 before clearing
	stored := uint16(b[9]&0x07)<<11 | uint16(b[10])<<3 | uint16(b[11]>>5)

	// Zero CRC field (bits 77-90) for computation
	b[9] &= 0xF8
	b[10] = 0
	b[11] = 0

	// Compute CRC over 82 bits (96 - 14)
	computed := crc14(b[:], 82)
	return computed == stored
}
