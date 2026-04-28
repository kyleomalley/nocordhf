package ft8

// osd.go — Ordered Statistics Decoding (OSD-2) fallback for when BP fails.
//
// Sort codeword positions by LLR reliability (|llr|) descending, pick K=91
// linearly-independent Gmat rows from the top of that list to form the Most
// Reliable Basis (MRB), invert the K×K submatrix, then enumerate weight-0/1/2
// flips of the hard decisions at MRB positions. Each flipped candidate is
// mapped back to a message via the MRB inverse, re-encoded to a full codeword,
// and scored by soft correlation against the LLRs. Best CRC-valid message wins.
//
// This closes much of the ~3 dB gap between plain BP and reference design decoder at
// low SNR (the a1/a3/a7 decodes that reference design annotates come from AP + OSD).

import (
	"math"
	"math/bits"
	"sort"
	"sync"
)

// osdScratch is the per-call working memory for decodeOSDCore. The `work`
// buffer is the hot one — 174×91 bytes allocated fresh per OSD call in the
// naive implementation, multiplied by ~374 candidates × up to 8 OSD calls
// each (base + AP retries) per slot → thousands of 16 KB allocations.
// Pooled via sync.Pool so the backing storage is reused across goroutines.
type osdScratch struct {
	order [N]int
	work  [N][K]byte
}

var osdScratchPool = sync.Pool{
	New: func() any { return new(osdScratch) },
}

// osdFlipTail is the number of LEAST-reliable MRB positions over which
// we enumerate weight-2 flips. Smaller = faster; larger = deeper search.
// 30 → 1 + 91 + 435 = 527 candidate codewords per OSD call. With the
// tighter nharderrors ≤ 38 gate, 30 ties 40's F1 at 6% lower wall
// (each weight-2 candidate now has fewer phantom-paths to cause harm,
// so the wider search stops paying off). 25 loses 5 real TPs.
const osdFlipTail = 30

// osdFlipTail3 is the tail depth for the optional weight-3 pass. Smaller
// than osdFlipTail to keep total work bounded: C(20,3)=1140 extra candidates
// so total order-2+3 ≈ 2000 per call. Only triggered when order-0..2 don't
// produce any CRC-valid codeword, so on strong signals this costs nothing.
const osdFlipTail3 = 20

// buildGmat returns the full N×K generator (codeword row = linear combo of msg bits).
// Rows 0..K-1 are identity (systematic); rows K..N-1 are the parity rows.
func buildGmat() [N][K]byte {
	var g [N][K]byte
	for i := 0; i < K; i++ {
		g[i][i] = 1
	}
	for i := 0; i < M; i++ {
		row := generatorRows[i]
		col := 0
		for _, ch := range row {
			var nibble byte
			if ch >= '0' && ch <= '9' {
				nibble = byte(ch - '0')
			} else if ch >= 'a' && ch <= 'f' {
				nibble = byte(ch-'a') + 10
			}
			for bit := 3; bit >= 0 && col < K; bit-- {
				if (nibble>>uint(bit))&1 == 1 {
					g[K+i][col] = 1
				}
				col++
			}
		}
	}
	return g
}

var osdGmat = buildGmat()

// decodeOSD attempts OSD-2 decoding. Returns (msg, true) if a CRC-valid message
// is found, else ({}, false).
func decodeOSD(llr [N]float64) ([K]byte, bool) {
	return decodeOSDSeededOpts(llr, nil, nil, true)
}

// decodeOSDFast is decodeOSD without the order-3 fallback. Used on candidates
// that are below the top-N rank by sync score — their LLRs are almost always
// noise, so the order-3 pass (1140 extra codeword trials) costs a lot of CPU
// for essentially no decode yield on the tail.
func decodeOSDFast(llr [N]float64) ([K]byte, bool) {
	return decodeOSDSeededOpts(llr, nil, nil, false)
}

// decodeOSDSeeded runs OSD-2 with a subset of message-bit positions forced to
// known values via saturated LLRs. Used for AP (a-priori) hypotheses like
// "this is a CQ" — the caller pins the known bits and OSD searches the rest.
// seedPos entries must be in [0, K); seedVal aligns 1:1.
func decodeOSDSeeded(llrIn [N]float64, seedPos []int, seedVal []byte) ([K]byte, bool) {
	return decodeOSDSeededOpts(llrIn, seedPos, seedVal, true)
}

func decodeOSDSeededOpts(llrIn [N]float64, seedPos []int, seedVal []byte, allowOrder3 bool) ([K]byte, bool) {
	llr := llrIn
	for i, pos := range seedPos {
		if pos < 0 || pos >= K {
			continue
		}
		if seedVal[i] == 0 {
			llr[pos] = 100.0
		} else {
			llr[pos] = -100.0
		}
	}
	return decodeOSDCore(llr, allowOrder3)
}

func decodeOSDCore(llr [N]float64, allowOrder3 bool) ([K]byte, bool) {
	scratch := osdScratchPool.Get().(*osdScratch)
	defer osdScratchPool.Put(scratch)

	// Hard decisions + reliability-sorted position order.
	var hard [N]byte
	order := scratch.order[:]
	for i := 0; i < N; i++ {
		if llr[i] < 0 {
			hard[i] = 1
		}
		order[i] = i
	}
	// SliceStable: tied |LLR| magnitudes must keep original bit-position
	// order across runs so the MRB selection is deterministic.
	sort.SliceStable(order, func(a, b int) bool {
		return math.Abs(llr[order[a]]) > math.Abs(llr[order[b]])
	})

	// GE to find the MRB: walk positions top-down (most reliable first), reduce
	// each Gmat row by previously chosen pivots, and keep the row if it still
	// has a 1 in an un-pivoted column. Stop once we have K pivots.
	work := &scratch.work
	for r := 0; r < N; r++ {
		work[r] = osdGmat[order[r]]
	}
	var mrbRow [K]int
	var pivotCol [K]int
	var colUsed [K]bool
	var mrbPos [K]int // original codeword index of the r-th MRB member
	nPivots := 0

	for r := 0; r < N && nPivots < K; r++ {
		for p := 0; p < nPivots; p++ {
			if work[r][pivotCol[p]] == 1 {
				for j := 0; j < K; j++ {
					work[r][j] ^= work[mrbRow[p]][j]
				}
			}
		}
		pc := -1
		for j := 0; j < K; j++ {
			if work[r][j] == 1 && !colUsed[j] {
				pc = j
				break
			}
		}
		if pc < 0 {
			continue
		}
		mrbRow[nPivots] = r
		pivotCol[nPivots] = pc
		colUsed[pc] = true
		mrbPos[nPivots] = order[r]
		nPivots++
	}
	if nPivots < K {
		return [K]byte{}, false
	}

	// Invert A = Gmat[mrbPos] (K×K) via Gauss-Jordan with augmented identity.
	var aug [K][2 * K]byte
	for p := 0; p < K; p++ {
		for j := 0; j < K; j++ {
			aug[p][j] = osdGmat[mrbPos[p]][j]
		}
		aug[p][K+p] = 1
	}
	for col := 0; col < K; col++ {
		pr := -1
		for r := col; r < K; r++ {
			if aug[r][col] == 1 {
				pr = r
				break
			}
		}
		if pr < 0 {
			return [K]byte{}, false
		}
		if pr != col {
			aug[col], aug[pr] = aug[pr], aug[col]
		}
		for r := 0; r < K; r++ {
			if r != col && aug[r][col] == 1 {
				for j := 0; j < 2*K; j++ {
					aug[r][j] ^= aug[col][j]
				}
			}
		}
	}
	// Bit-pack ainv: each of the 91 rows becomes (a,b) uint64s where a holds
	// bits 0..63 and b holds bits 64..90. computeMsg below collapses the
	// inner 91-byte XOR loop to 3 instructions per row (AND/AND/XOR/popcount)
	// — ~30× faster on the OSD inner loop, which the profile pegged at 40%
	// of total decode CPU.
	type packedRow struct{ a, b uint64 }
	var ainvPacked [K]packedRow
	for p := 0; p < K; p++ {
		var a, b uint64
		for j := 0; j < K; j++ {
			if aug[p][K+j] == 1 {
				if j < 64 {
					a |= uint64(1) << uint(j)
				} else {
					b |= uint64(1) << uint(j-64)
				}
			}
		}
		ainvPacked[p] = packedRow{a, b}
	}

	// Hard decisions at MRB positions (in MRB order, most-reliable first).
	var cMRB [K]byte
	for p := 0; p < K; p++ {
		cMRB[p] = hard[mrbPos[p]]
	}

	scoreCodeword := func(cw [N]byte) float64 {
		s := 0.0
		for i := 0; i < N; i++ {
			if cw[i] == 0 {
				s += llr[i]
			} else {
				s -= llr[i]
			}
		}
		return s
	}
	computeMsg := func(c [K]byte) [K]byte {
		// Pack c to (ca, cb) once per trial — same layout as ainvPacked rows.
		var ca, cb uint64
		for j := 0; j < K; j++ {
			if c[j] != 0 {
				if j < 64 {
					ca |= uint64(1) << uint(j)
				} else {
					cb |= uint64(1) << uint(j-64)
				}
			}
		}
		var msg [K]byte
		for i := 0; i < K; i++ {
			r := ainvPacked[i]
			// Parity of (r.a & ca) XOR (r.b & cb): bit-wise AND, then count 1s mod 2.
			msg[i] = byte(bits.OnesCount64((r.a&ca)^(r.b&cb)) & 1)
		}
		return msg
	}

	var bestMsg [K]byte
	bestScore := math.Inf(-1)
	found := false

	try := func(c [K]byte) {
		msg := computeMsg(c)
		if !CheckCRC(msg) {
			return
		}
		cw := encodeLDPC(msg)
		s := scoreCodeword(cw)
		if s > bestScore {
			bestScore = s
			bestMsg = msg
			found = true
		}
	}

	// Order 0
	try(cMRB)
	// Order 1: flip each MRB bit
	for p := 0; p < K; p++ {
		c := cMRB
		c[p] ^= 1
		try(c)
	}
	// Order 2: pairs over the least-reliable tail of the MRB.
	tailStart := K - osdFlipTail
	if tailStart < 0 {
		tailStart = 0
	}
	for p := tailStart; p < K; p++ {
		for q := p + 1; q < K; q++ {
			c := cMRB
			c[p] ^= 1
			c[q] ^= 1
			try(c)
		}
	}

	// Order 3: triple flips over a shorter tail, only when orders 0..2 found
	// nothing. Catches third-party weak decodes (no AP seed applies) where
	// the error burst is larger than a pair. Bounded by osdFlipTail3 to keep
	// cost low — on strong signals this block is never entered.
	if !found && allowOrder3 {
		tail3Start := K - osdFlipTail3
		if tail3Start < 0 {
			tail3Start = 0
		}
		for p := tail3Start; p < K; p++ {
			for q := p + 1; q < K; q++ {
				for r := q + 1; r < K; r++ {
					c := cMRB
					c[p] ^= 1
					c[q] ^= 1
					c[r] ^= 1
					try(c)
				}
			}
		}
	}

	return bestMsg, found
}
