package ft8

// decode.go — FT8 decode pipeline matching ft8_lib (kgoba/ft8_lib) monitor/decode approach.
//
//   time_osr=2, freq_osr=2:
//   block_size = 1920 (samples per symbol at 12kHz)
//   subblock_size = block_size / time_osr = 960
//   nfft = block_size * freq_osr = 3840
//   df = sampleRate / nfft = 3.125 Hz/bin
//   num_bins per tone = freq_osr = 2
//   max_blocks = 93 (15s / 0.16s)
//   block_stride = time_osr * freq_osr * num_bins (waterfall layout)
//
// Waterfall layout (per ft8_lib monitor.c):
//   mag[block * block_stride + time_sub * freq_osr * num_bins + freq_sub * num_bins + bin]
//
// Pipeline:
//  1. Hann-windowed 3840-pt FFT spectrogram at 960-sample (half-symbol) stride
//  2. ft8_lib-style sync score: compare expected tone to neighbors
//  3. Per-candidate: extract 174 LLRs from waterfall using gray map
//  4. LDPC BP decoder → CRC check → unpack message

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gonum.org/v1/gonum/dsp/fourier"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	sampleRate    = 12000
	windowSamples = sampleRate * 15 // 180,000

	SymbolDuration = 0.16
	ToneSpacing    = 6.25
	NumSymbols     = 79
	NumTones       = 8
	SamplesPerSym  = 1920 // block_size

	timeOSR      = 2
	freqOSR      = 2
	nfft         = SamplesPerSym * freqOSR    // 3840
	subblockSize = SamplesPerSym / timeOSR    // 960 (half-symbol stride)
	dfHz         = float64(sampleRate) / nfft // 3.125 Hz/bin

	// Frequency range for candidate search. fMinHz lowered from 200 to 100 Hz
	// after reference design comparison showed recurring strong decodes at 143/186 Hz
	// that we never detected because they fell below our floor. IC-7300
	// passband extends well below 200 Hz, and reference design scans that low too.
	fMinHz = 100.0
	fMaxHz = 3600.0

	// Number of bins in frequency range
	minBin = int(fMinHz / (float64(sampleRate) / nfft)) // ~32
	maxBin = int(fMaxHz/(float64(sampleRate)/nfft)) + 1 // ~897

	numBins = maxBin - minBin // bins in range

	// Waterfall: max_blocks = 15s / 0.16s = 93
	maxBlocks   = sampleRate * 15 / SamplesPerSym // 93
	blockStride = timeOSR * freqOSR * numBins

	// ft8 sync
	numSyncBlocks = 3  // 3 Costas arrays
	lenSyncBlock  = 7  // 7 symbols each
	syncOffset    = 36 // symbols between Costas arrays
)

var costasSeq = [7]int{3, 1, 4, 0, 6, 5, 2}

var grayMap = [8]int{0, 1, 3, 2, 5, 6, 4, 7}

// waterfall holds the magnitude spectrogram in ft8_lib's uint8 format.
// Indexed as: mag[block*blockStride + timeSub*freqOSR*numBins + freqSub*numBins + (bin-minBin)]
type waterfall struct {
	mag       []uint8
	numBlocks int
}

type Candidate struct {
	TimeOffset int     // block (symbol) offset from start of waterfall
	TimeSub    int     // time sub-sample (0..timeOSR-1)
	FreqSub    int     // freq sub-sample (0..freqOSR-1)
	FreqOffset int     // bin offset within numBins
	Score      int     // sync score
	Freq       float64 // Hz (derived)
	TimeSecs   float64 // seconds (derived)
	SNR        float64 // dB estimate
}

type Decoded struct {
	Candidate
	Message   Message
	SlotStart time.Time
	// Method indicates which decoder path produced this result:
	//   ""    — belief-propagation (normal BP+CRC)
	//   "osd" — OSD-2 fallback after BP failed
	//   "a1"  — AP hypothesis "CQ ? ?" seeded into OSD
	// Surfaced in the UI so weak/rescued decodes are visibly distinguishable.
	Method string
	// Agreement is the fraction of the 174 re-encoded codeword bits whose signs
	// match the original LLRs. Real decodes at -20 dB match ~70–80%; random
	// codewords match ~50%. Logged for post-hoc confidence analysis — lets us
	// compare nocordhf decodes the reference design missed (should be high agreement) against
	// possible phantoms that escape our filters (would be borderline).
	Agreement float64
	// tones is the 79-entry GFSK tone sequence reconstructed from the decoded
	// bits. Unexported; used by the subtract-and-retry second pass to
	// regenerate this signal's waveform for removal from the slot audio.
	tones [NumSymbols]int
	// refinedXdt is the coherent-refined signal start time in seconds from the
	// slot boundary (ibest / 200 Hz baseband rate). Unexported; used only by
	// the subtraction pass — the candidate's TimeSecs is quantized to 80 ms
	// by the STFT grid, whereas the refined value is ~5 ms accurate.
	refinedXdt float64
}

// LiveDecodeWallBudget is the recommended wall-clock cap for live single-
// slot decoding. The 15-second FT8 slot is the hard real-time deadline: if
// Decode runs past it, the audio callback is competing with decoder workers
// for cores and the next slot's audio comes through degraded. 13 s leaves
// ~2 s of headroom; pass via SetDecodeBudget to enable. Default 0 means
// "no budget" — appropriate for tests where slots run in parallel and
// per-slot wall time is inflated by core contention.
const LiveDecodeWallBudget = 13 * time.Second

var decodeBudget atomic.Int64 // ns; 0 = unlimited

// SetDecodeBudget caps total wall time inside Decode so the caller's audio
// capture goroutine has guaranteed CPU during the next slot. When the
// budget is hit mid-decode, in-flight workers finish their current
// candidate and exit. Whatever decodes were found stream out normally;
// remaining candidates go un-attempted; pass 2 is skipped.
//
// Pass 0 (default) for unlimited — used by tests that run many slots in
// parallel where wall time per slot is contention-inflated. Pass
// LiveDecodeWallBudget for live operation.
func SetDecodeBudget(d time.Duration) { decodeBudget.Store(int64(d)) }

// Decode runs the full FT8 decode pipeline on a single 15-second slot.
// onDecode (optional) is called from the worker goroutines the moment each
// candidate produces a CRC-valid message — BP decodes (fast) fire early,
// OSD/AP rescues fire as they finish, and pass-2 (post-subtraction) decodes
// fire later. The caller is responsible for serialising any UI state updates
// the callback drives. Pass nil for batch-only behavior.
//
// The returned slice is the final, deduped, cross-pass-merged set after all
// candidates are processed; it's a superset of what the callback emitted
// (callback fires pre-dedup so a small number of streamed decodes may be
// merged out by the time Decode returns).
//
// Decode enforces a wall-clock budget set via SetDecodeBudget and returns
// early if the budget is hit, leaving CPU headroom for the next slot's
// audio capture.
func Decode(samples []float32, slotStart time.Time, onDecode func(Decoded)) []Decoded {
	var deadline time.Time
	if budget := time.Duration(decodeBudget.Load()); budget > 0 {
		deadline = time.Now().Add(budget)
	}
	hasDeadline := !deadline.IsZero()
	// Accept short slots (USB capture occasionally loses ~tens of ms at the
	// tail); zero-pad to the full 15-second window so the FFT grid still
	// aligns. The 12.64 s FT8 signal body starts ~0.5 s into the slot, so
	// FT8 signal occupies seconds 0.5–13.14 within the slot, so as little as
	// 13.14 s of captured audio still holds the full body. Live capture on
	// USB CODEC devices drifts under macOS audio scheduling — observed slots
	// at 13.2–13.5 s being short-circuited by an over-strict 13.5 s gate
	// even though the signal was fully present, producing the user-reported
	// "every other slot decodes 0" pattern. Threshold lowered to 13 s
	// (156000 samples) to cover the typical USB drift; below that the signal
	// tail is genuinely cut and there's nothing to decode.
	const minSamples = sampleRate * 13
	if len(samples) < minSamples {
		return nil
	}
	if len(samples) < windowSamples {
		padded := make([]float32, windowSamples)
		copy(padded, samples)
		samples = padded
	} else {
		samples = samples[:windowSamples]
	}

	// Multi-pass decode with time-domain signal subtraction between passes.
	// Pass 1 runs the full pipeline on the raw slot audio. Each high-
	// confidence decode is then subtracted (time-varying complex amplitude,
	// reference design style) from a working copy of the audio. Pass 2 runs the same
	// pipeline on the subtracted audio, uncovering weak signals that were
	// adjacent-channel-masked by the stronger decodes removed in pass 1.
	//
	// Subtraction only uses BP decodes with high agreement (≥ 0.95) —
	// rescue-path decodes are lower confidence and subtracting them risks
	// creating artifacts if the reconstructed signal is slightly wrong.
	//
	// Mirrors ft8_decode.f90:172-192 (reference design 3-pass structure; we do 2 for
	// latency). Merged pass-1 + pass-2 decodes feed the sidelobe filter and
	// dedup below as if they came from a single pass.
	work := make([]float32, len(samples))
	copy(work, samples)

	// Streaming-layer dedup: workers fire onDecode the moment a candidate
	// produces a CRC-valid message, but strong signals routinely register at
	// 2–3 adjacent sub-bins (~3 Hz apart) and produce the same decoded text
	// each time. Without this gate the GUI would see "YC1EMV KA5PTG DM65"
	// 3× at 1775 / 1778 / 1778 Hz before the post-decode dedup at the bottom
	// of Decode() collapses them. Mirrors that final dedup's 10 Hz / same-
	// text rule, but applied online from the worker goroutines.
	type streamKey struct {
		text string
		freq float64
	}
	var streamMu sync.Mutex
	var streamSeen []streamKey
	dedupedOnDecode := onDecode
	if onDecode != nil {
		dedupedOnDecode = func(d Decoded) {
			streamMu.Lock()
			for _, s := range streamSeen {
				if s.text == d.Message.Text && math.Abs(s.freq-d.Freq) < 10.0 {
					streamMu.Unlock()
					return
				}
			}
			streamSeen = append(streamSeen, streamKey{d.Message.Text, d.Freq})
			streamMu.Unlock()
			onDecode(d)
		}
	}

	runPass := func(passIdx int, samples []float32) []Decoded {

		tWF := time.Now()
		wf := buildWaterfall(samples)
		wfMs := time.Since(tWF).Milliseconds()

		// Long FFT of the slot: computed once, shared across all candidate workers
		// for coherent per-candidate baseband extraction (reference design parity; see
		// coherent.go). This is the single expensive prep step for the coherent
		// path — 192000-point r2c FFT costs ~15 ms and is amortized across all
		// candidates.
		tSpec := time.Now()
		spec := computeSlotSpectrum(samples)
		specMs := time.Since(tSpec).Milliseconds()

		tCand := time.Now()
		candidates := findCandidates(wf)
		candMs := time.Since(tCand).Milliseconds()
		tDecode := time.Now()
		if logging.L != nil {
			logging.L.Debugw("FT8 candidates", "count", len(candidates), "slot", slotStart.Format("15:04:05"), "pass", passIdx)
		}

		// Sort by sync score descending so AP rank-gate (below) keeps the
		// strongest candidates. Real weak decodes cluster near the top; the
		// long tail is overwhelmingly noise that wastes OSD work.
		// Use SliceStable: tied sync scores must keep their original
		// (timeSub × freqSub × timeOff × freqOff) order for run-to-run
		// determinism. Prior to this, sort.Slice on equal-score candidates
		// shuffled the apRankLimit cutoff, occasionally putting the real-
		// signal candidate just outside top-N and skipping its OSD/AP rescue.
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].Score > candidates[j].Score
		})
		const apRankLimit = 100

		// Diagnostic: dump the full candidate list for post-hoc analysis of
		// missed decodes. Opt-in via NOCORDHF_CANDIDATE_DUMP=1 because the line can
		// be ~10 KB on a busy slot. Compact "freq:score" pairs, sorted by score.
		// When investigating a specific reference-design decode nocordhf missed, grep the slot
		// for the expected frequency; if it's absent, sync/minScore is the
		// culprit; if present but no matching `decoded` or `rescue` line, BP and
		// OSD both failed on that candidate.
		if logging.L != nil && os.Getenv("NOCORDHF_CANDIDATE_DUMP") == "1" {
			buf := make([]string, 0, len(candidates))
			for _, c := range candidates {
				buf = append(buf, fmt.Sprintf("%.1f:%d", c.Freq, c.Score))
			}
			logging.L.Debugw("candidates",
				"slot", slotStart.Format("15:04:05"),
				"n", len(candidates),
				"list", strings.Join(buf, ","),
			)
		}

		// Per-candidate decode work is CPU-bound and fully independent (read-only
		// access to wf; no shared mutable state across OSD/BP). Parallelize across
		// GOMAXPROCS workers with an atomic candidate-index for work stealing.
		// Per-worker result slices are merged after the pool drains — avoids a
		// mutex on the hot path.
		type workerOut struct{ results []Decoded }
		// Cap workers more aggressively than GOMAXPROCS to leave headroom for
		// the audio callback + GUI thread. On macOS USB-CODEC capture, every
		// 5-10ms callback runs Go code via cgo; if all cores are saturated by
		// decoder workers, the cgo bridge stalls and audio either drops samples
		// or arrives late, degrading every-other slot's decode quality (the
		// user-observed self-perpetuating pattern: heavy decode slot N starves
		// audio capture for slot N+1 → N+1 decodes nothing → its decode is
		// fast → N+2 audio is clean → repeat). GOMAXPROCS/2 + 1 leaves enough
		// idle cores that the audio callback always finds one available.
		gmp := runtime.GOMAXPROCS(0)
		numWorkers := gmp/2 + 1
		if numWorkers > len(candidates) {
			numWorkers = len(candidates)
		}
		if numWorkers < 1 {
			numWorkers = 1
		}
		var next atomic.Int64
		outs := make([]workerOut, numWorkers)
		var wg sync.WaitGroup
		wg.Add(numWorkers)

		decodeOne := func(idx int, cand Candidate, sc *coherentScratch) (Decoded, bool) {
			// Coherent LLR path: refine the candidate's time/freq offset against the
			// Costas reference in a 200-Hz baseband, then extract FOUR LLR variants
			// (single-symbol bmeta, 2-symbol joint bmetb, 3-symbol joint bmetc, and
			// bit-normalized bmetd) from the per-symbol 32-pt FFT. We try BP on each
			// variant in order and stop at the first CRC-valid result. Joint-symbol
			// variants recover decodes where symbol-boundary timing jitter or
			// between-symbol interference smears energy that single-symbol LLRs
			// can't resolve. Matches reference design ft8b.f90 passes 1–4.
			//
			// NOCORDHF_LEGACY_LLR=1 falls back to the old STFT-bin path (bmeta only).
			var arr [N]float64
			var llrs *coherentLLRs
			var refinedXdt float64
			if os.Getenv("NOCORDHF_LEGACY_LLR") == "1" {
				legacy := extractLLR(wf, cand)
				copy(arr[:], legacy)
				refinedXdt = cand.TimeSecs
			} else {
				llrs, refinedXdt = coherentDecodeOne(sc, spec, cand)
				arr = llrs.bmeta
			}

			bits, bpOK, bmetaSnapshots := DecodeLDPCEx(arr)
			crcOK := bpOK && CheckCRC(bits)

			// Try alternate LLR variants if primary BP failed. Joint-symbol metrics
			// occasionally rescue candidates where bmeta's LLRs are too noisy for BP
			// to converge. Each is cheap compared to OSD — BP early-exits on stall
			// in ~20 iterations, so 3 extra BP calls per unresolved candidate is
			// roughly 1–2 ms vs OSD's 5–10 ms. Capture each variant's BP snapshots
			// so the snapshot-OSD rescue path below has 12 soft-iterate starting
			// points (4 variants × 3 snapshots), each giving OSD a different MRB
			// ordering to find the real codeword when channel LLRs land on phantom.
			var altSnapshots [3][5][N]float64
			if !crcOK && llrs != nil {
				for i, alt := range []*[N]float64{&llrs.bmetb, &llrs.bmetc, &llrs.bmetd} {
					b, ok, snaps := DecodeLDPCEx(*alt)
					altSnapshots[i] = snaps
					if ok && CheckCRC(b) {
						bits = b
						crcOK = true
						arr = *alt
						break
					}
				}
			}
			// Final BP retry on legacy STFT-bin LLRs when the coherent variants
			// all fail. Coherent demod is a net win on average (corpus F1 0.796
			// vs 0.695 legacy) but has signal-specific failure modes: certain
			// slots (e.g. 20260424T151845) have -4 to -9 dB signals where
			// coherent BP produces garbage LLRs but legacy extractLLR's STFT-bin
			// BP converges cleanly to the right codeword. With this fallback
			// the bad slot recovered ~18 TPs without adding any phantoms
			// (corpus F1 0.796 → 0.809). Cheap: only fires after the 4 coherent
			// variants have all failed, and BP early-exits on stalled noise.
			if !crcOK && os.Getenv("NOCORDHF_LEGACY_LLR") != "1" {
				var legacyArr [N]float64
				legacy := extractLLR(wf, cand)
				copy(legacyArr[:], legacy)
				b, ok, _ := DecodeLDPCEx(legacyArr)
				if ok && CheckCRC(b) {
					bits = b
					crcOK = true
					arr = legacyArr
				}
			}

			// OSD-2 fallback: if BP failed on all variants, run OSD on bmeta (the
			// most-reliable single-symbol metric). For top-rank candidates we also
			// try OSD on bmetb/bmetc — those occasionally recover where bmeta's OSD
			// lands on a phantom because the joint-symbol reliability ordering
			// produces a different MRB.
			method := ""
			allowOrder3 := idx < apRankLimit
			tryOSD := func(llr [N]float64, seedPos []int, seedVal []byte) bool {
				var osdBits [K]byte
				var osdOK bool
				if seedPos == nil {
					if allowOrder3 {
						osdBits, osdOK = decodeOSD(llr)
					} else {
						osdBits, osdOK = decodeOSDFast(llr)
					}
				} else {
					osdBits, osdOK = decodeOSDSeeded(llr, seedPos, seedVal)
				}
				if !osdOK {
					return false
				}
				var p [77]byte
				copy(p[:], osdBits[:77])
				// Inline cheap phantom pre-filter: reject OSD hits that unpack to
				// patterns overwhelmingly characteristic of CRC-collision phantoms,
				// BEFORE committing crcOK=true. Without this, the outer OSD chain
				// stops at the first "CRC-valid" hit even when a later path would
				// have found the real codeword — i.e. one phantom here blocks
				// alt-LLR OSD, AP, and snapshot OSD from running on this candidate.
				// Corpus evidence: N3ALN/WZ9B at 585 Hz blocked by a `/R` phantom
				// in OSD-on-bmeta that failed the compound_slash filter downstream.
				m := Unpack77(p)
				if m.Type == 6 || m.Type == 7 {
					return false
				}
				if strings.HasPrefix(m.Text, "[") {
					// Unpack77 error placeholder — definite phantom.
					return false
				}
				if strings.ContainsRune(m.Text, '/') {
					// /P, /R, other compound suffixes are disproportionately
					// phantoms. BP decodes of real compound calls bypass this
					// path entirely.
					return false
				}
				if hasHashToken(m.Text) {
					// Raw <digits> hash token Type-4 phantoms.
					return false
				}
				if !isValidType1Message(m.Text) {
					return false
				}
				if !messageHasAllocatedCall(m.Text) {
					return false
				}
				bits = osdBits
				crcOK = true
				arr = llr // agreement math below uses this LLR variant
				return true
			}
			if !crcOK {
				if tryOSD(arr, nil, nil) {
					method = "osd"
				}
			}
			// Alt-LLR OSD rescue: when OSD on bmeta hits a phantom (or fails), the
			// joint-symbol metrics bmetb/bmetc produce a different MRB ordering
			// that occasionally lands on the real codeword. Rescue-log evidence
			// shows real strong signals (e.g. W3CVD K3ARM @ 928 Hz +2 dB, KD9VUK
			// KA5RBZ RR73 @ 1312 Hz 0 dB) where OSD-on-bmeta returned a CRC-valid
			// phantom while BP on all four variants failed to converge. With the
			// reference-design style nharderrors ≤ 36 gate below, phantoms from alternate
			// OSD variants are rejected uniformly, so this is a net-positive
			// rescue path. Gated to top-rank candidates because alt-OSD on noise
			// candidates adds latency with no real-decode yield.
			if !crcOK && llrs != nil && idx < apRankLimit {
				for _, alt := range []*[N]float64{&llrs.bmetb, &llrs.bmetc, &llrs.bmetd} {
					if tryOSD(*alt, nil, nil) {
						method = "osd"
						break
					}
				}
			}
			if !crcOK && idx < apRankLimit {
				// AP hypotheses: seed known bits (CQ structure, our own call,
				// partner's call, full sign-off messages) and retry OSD. OSD
				// can flip any bit at sufficient correlation cost — including
				// the seeded ones — so each hypothesis carries a `verify`
				// callback that confirms the decoded text matches the seed's
				// intent. Mismatches are dropped. Gated to top-N candidates by
				// sync score — tail candidates are noise and waste OSD time.
				for _, h := range apHypothesesForCandidate() {
					if tryOSD(arr, h.pos, h.val) {
						var p [77]byte
						copy(p[:], bits[:77])
						m := Unpack77(p)
						if h.verify != nil && !h.verify(m.Text) {
							crcOK = false
							continue
						}
						method = h.name
						break
					}
				}
			}
			// BP-snapshot OSD rescue — last line of defense. reference design decode174_91
			// feeds a BP-iterated posterior LLR vector (zsum) to OSD so the MRB
			// reliability ordering reflects BP's progress rather than raw channel
			// LLRs. This specifically rescues the "OSD lands on phantom at right
			// freq" class (e.g. Z35U/WA4FL @2512 Hz, OK1EP/VA7JC @1656 Hz) where
			// the real codeword isn't in the raw-LLR top-K MRB but IS in the
			// BP-softened top-K. Runs AFTER AP so AP's per-hypothesis verify
			// callback can rescue real decodes that would otherwise collide with a
			// snapshot-OSD phantom. Gated to top-rank candidates because BP
			// snapshots on noise produce ~unchanged LLRs (BP makes no progress);
			// snapshot OSD on those just wastes CPU.
			// Snapshot OSD with per-variant BP-iterated soft vectors. Up to 12
			// OSD attempts (4 variants × 3 snapshots) for candidates that all
			// prior paths missed. The inline tryOSD pre-filter rejects phantoms
			// early and the hash-bracket gate downstream catches the remaining
			// cache-resolved-hash noise; remaining phantom rate is ~1 per 22
			// slots, still comfortably above 0.98 precision.
			if !crcOK && idx < apRankLimit {
				allSnaps := [][5][N]float64{bmetaSnapshots, altSnapshots[0], altSnapshots[1], altSnapshots[2]}
				for _, snaps := range allSnaps {
					done := false
					for s := 0; s < 5; s++ {
						if tryOSD(snaps[s], nil, nil) {
							method = "osds"
							done = true
							break
						}
					}
					if done {
						break
					}
				}
			}
			if !crcOK {
				return Decoded{}, false
			}

			var payload [77]byte
			copy(payload[:], bits[:77])
			msg := Unpack77(payload)

			// Reject unpack-error placeholders ("[free-text err]", "[i3=0 n3=6]",
			// etc.). Unpack77 returns these when the bit pattern landed on a
			// message type that doesn't map back to legal text (e.g. an all-zero
			// free-text payload, which BP occasionally produces as a trivial
			// codeword phantom). Applies to BP path too: observed a 0.908-agree
			// BP decode at 2309 Hz that unpacked to "[free-text err]".
			if strings.HasPrefix(msg.Text, "[") {
				return Decoded{}, false
			}

			// Reject decodes where both call positions are reserved n28 tokens
			// (DE/QRZ/CQ). "DE DE AA00"-shaped messages are structurally valid
			// codewords that never occur as real on-air traffic — they're CRC
			// collisions landing on the low-value n28 slots in both positions.
			// Applies to BP too: a 0.93-agreement BP hit on "DE DE ..." was seen
			// in logs, so trusting BP here isn't enough.
			if toks := strings.Fields(msg.Text); len(toks) >= 2 {
				reserved := func(t string) bool {
					return t == "DE" || t == "QRZ" || t == "CQ"
				}
				if reserved(toks[0]) && reserved(toks[1]) {
					return Decoded{}, false
				}
			}

			codeword := encodeLDPC(bits)
			tones := modulateTones(codeword)
			cand.SNR = computeSNR(wf, cand, tones)

			// Two agreement measures:
			//   agreePct: fraction of sign-matches (each bit counted equally).
			//   agreeW:   |LLR|-weighted agreement — matches contribute proportional
			//             to channel confidence. For a real signal the strong bits
			//             (|LLR| large) almost always agree with the codeword, so
			//             agreeW ≫ agreePct. For a phantom codeword the agreement
			//             is essentially random across all reliabilities, so
			//             agreeW ≈ agreePct. Used together below as a defense-in-
			//             depth check against OSD landing on a random CRC-valid
			//             codeword when the LLRs are dominated by interference.
			// reference design phantom gate is computed against the primary (single-symbol,
			// unsaturated) channel LLR — not whichever variant OSD happened to use.
			// Alt-LLR variants are only a search-path tool for finding the codeword;
			// the codeword's fit to the raw channel is what separates real from
			// phantom. See ft8b.f90:422 (nharderrors against `llr` = llra).
			primaryLLR := arr
			if llrs != nil {
				primaryLLR = llrs.bmeta
			}
			agree := 0
			var sumAbs, sumAbsAgree float64
			for i := 0; i < N; i++ {
				hard := byte(0)
				if primaryLLR[i] < 0 {
					hard = 1
				}
				abs := primaryLLR[i]
				if abs < 0 {
					abs = -abs
				}
				sumAbs += abs
				if codeword[i] == hard {
					agree++
					sumAbsAgree += abs
				}
			}
			agreePct := float64(agree) / float64(N)
			nharderrors := N - agree
			agreeW := 0.5
			if sumAbs > 0 {
				agreeW = sumAbsAgree / sumAbs
			}

			// logRescue emits a structured audit line for rescue-path decisions.
			// Every candidate that produced a CRC-valid message via OSD/AP is
			// logged with its filter outcome ("accepted" or a reject reason), so
			// we can later brute-force study signals reference-design decoded that we rejected.
			logRescue := func(reason string) {
				if logging.L == nil {
					return
				}
				logging.L.Infow("rescue",
					"slot", slotStart.Format("15:04:05"),
					"freq_hz", fmt.Sprintf("%.1f", cand.Freq),
					"snr", fmt.Sprintf("%+.0f", cand.SNR),
					"method", method,
					"agree", fmt.Sprintf("%.3f", agreePct),
					"agree_w", fmt.Sprintf("%.3f", agreeW),
					"t", cand.TimeOffset,
					"msg", msg.Text,
					"reason", reason,
				)
			}

			// Rescue-path false-positive filters. BP decodes are trusted (a full
			// parity + CRC match on the actual signal). OSD/AP decodes re-encode
			// from a search, so CRC alone admits random matches at ~2^-14 rate —
			// amplified across hundreds of candidates per slot.
			if method != "" {
				// (0) Time-offset sanity: FT8 transmitters start ~0.5s into the
				// 15-s slot (protocol), so our candidate grid's real-decode
				// cluster sits at t ∈ [+4, +10]. A candidate with t < -5 means
				// the supposed signal started well before slot boundary, which
				// is physically impossible for a legitimate transmission. OSD's
				// search space across the full [-15, +15] candidate grid picks
				// up noise-space CRC collisions there. Observed phantom:
				// "CQ KP5KRK OQ76" @ t=-12 in slot 055630.
				if cand.TimeOffset < -5 {
					logRescue("time_offset")
					return Decoded{}, false
				}
				// (1) SNR floor: FT8's physical decode limit with AP is around
				// -24 dB (reference design published floor). Anything below is almost
				// certainly a phantom CRC collision on noise.
				if cand.SNR < osdMinSNR {
					logRescue("snr_floor")
					return Decoded{}, false
				}
				// (2) Message must parse as a well-formed Type-1 FT8 message with
				// all call positions structurally plausible. Rejects rescue-path
				// phantoms where one callsign is gibberish ("200BSQ ND1UVX LI57")
				// even if the other passes, and gibberish-both cases like
				// "KPCHIRCA/XA <2077> 73".
				if !isValidType1Message(msg.Text) {
					logRescue("struct_invalid")
					return Decoded{}, false
				}
				// (2b) Reject rescues containing raw <digits> hash tokens — these
				// are Type-4 unpackings that OSD hits frequently as phantom
				// matches (e.g. "PQ2AR 6N8TX <64> RR73"). Real Type-4 decodes
				// exist but without a populated hash table we can't resolve them
				// anyway, so filtering out rescue-path Type-4 is a good trade.
				if hasHashToken(msg.Text) {
					logRescue("hash_token")
					return Decoded{}, false
				}
				// (2c) Reject rescues containing any '/' in the message. FT8 uses
				// '/' only in compound callsigns (portable /P, rover /R, contest
				// suffixes). These are disproportionately represented in OSD/AP
				// phantom codewords and rare in normal QSO traffic. BP decodes of
				// real compound calls are unaffected.
				if strings.ContainsRune(msg.Text, '/') {
					logRescue("compound_slash")
					return Decoded{}, false
				}
				// (2d) ITU prefix whitelist. Requires at least one call position
				// to start with a 2-char prefix that's allocated to a DXCC entity.
				// Catches rescues that land on structurally-valid but unassigned
				// prefix blocks (e.g. "QA", "MQ", "1K", "2S"). BP decodes bypass
				// this — a successful BP on the raw signal is its own confirmation.
				// Always-on for rescue path; the SetITUFilterEnabled toggle is
				// retained for API compatibility but the rescue path no longer
				// consults it (phantoms with unallocated prefixes are never worth
				// surfacing).
				if !messageHasAllocatedCall(msg.Text) {
					logRescue("itu_prefix")
					return Decoded{}, false
				}
				// (3) reference design primary phantom gate: the codeword must disagree
				// with the channel LLR sign on no more than 36 of the 174 bits.
				// Mirrors ft8b.f90:422 (`if nharderrors.gt.36 cycle`). This
				// maps to hard-bit agreement ≥ 0.793 — 7 percentage points
				// tighter than our old 0.72 threshold, and precisely the gap
				// where our live-slot phantoms (0.736, 0.764) were slipping
				// through. Applied uniformly to OSD and AP decodes; BP decodes
				// still bypass the rescue filter stack because BP convergence
				// is itself a strong codeword proof (full LDPC parity passes).
				// nharderrors gate, method-dependent. AP hypotheses pin known bits
				// at ±100 LLR before OSD, so OSD's soft score is INFLATED even
				// when the seed is wrong for the signal — meaning AP decodes need
				// to clear a tighter primary-LLR-agreement bar to be trustworthy.
				// Corpus sweep on the 53-slot regression set:
				//   36 → F1 0.738 (P=0.971, R=0.595)
				//   38 → F1 0.739 (P=0.949, R=0.601)  ← peak
				//   39 → F1 0.738 (P=0.952, R=0.602)
				//   42 → F1 0.735 (P=0.931, R=0.608)
				// 38 maximises F1: trades 4 weak real decodes for 15 fewer
				// snapshot-OSD phantoms (which cluster at 0.76–0.78 agreePct).
				maxHardErrors := 38
				if strings.HasPrefix(method, "a") {
					maxHardErrors = 36
				}
				// Snapshot-OSD (BP-iterate posterior basis) is the most permissive
				// rescue path — it's the last line of defense, runs after BP and
				// AP, and gets to pick the BP-softened MRB on whatever progress
				// BP made. That's exactly when phantom CRC collisions pile up,
				// because BP's posterior on noise still has signal-like structure
				// from random partial parity satisfaction. Tighter gate here.
				if method == "osds" {
					maxHardErrors = 36
				}
				// Non-standard message types (Type 3 RTTY-RU, Type 4 hashed/nonstd,
				// Type 5 EU VHF) have more bit-degrees-of-freedom than the standard
				// FT8 layout — random codewords land on a "valid" Type-3/4/5
				// message more often than on a valid Type-1/2 (CALL CALL
				// grid/report). Tighter gate here. (Type 100+ i3=0 sub-types
				// excluded: corpus testing showed 100+ has legitimate decodes
				// that the tighter gate cuts.)
				if (msg.Type == 3 || msg.Type == 4 || msg.Type == 5) && method != "" {
					maxHardErrors = 32
				}
				if nharderrors > maxHardErrors {
					logRescue("hard_errors_high")
					return Decoded{}, false
				}
				// Tighter gate for decodes containing any `<...>` bracket — either
				// unresolved `<digits>` hash tokens (mostly filtered earlier) or
				// cache-resolved hash names. Per-variant snapshot-OSD tends to
				// land on hash-bracket phantoms in the 0.75–0.82 agreement band;
				// real hash decodes from BP come in at ≥ 0.90.
				if strings.ContainsRune(msg.Text, '<') && agreePct < 0.90 {
					logRescue("hash_bracket_low_agreement")
					return Decoded{}, false
				}
				// (3b) |LLR|-weighted agreement. Real signals concentrate their
				// disagreements on low-|LLR| bits (uncertain channel observations
				// that the LDPC check network decided differently), so agreeW runs
				// noticeably higher than agreePct. Phantom codewords — random
				// CRC-valid bit patterns fitted to interference-dominated LLRs —
				// disagree indiscriminately, producing agreeW ≈ agreePct.
				// Observed phantom "2O8WVB 8M2RZY RA82" at agreePct=0.770 had
				// agreeW close to 0.77; real rescues at similar agreePct have
				// agreeW ≥ 0.80. Tune conservatively; only bite when both signals
				// are present.
				if agreeW < agreeWeightedThreshold {
					logRescue("weighted_agreement_low")
					return Decoded{}, false
				}
				logRescue("accepted")
			}

			return Decoded{
				Candidate:  cand,
				Message:    msg,
				SlotStart:  slotStart,
				Method:     method,
				Agreement:  agreePct,
				tones:      tones,
				refinedXdt: refinedXdt,
			}, true
		}

		for w := 0; w < numWorkers; w++ {
			go func(wIdx int) {
				defer wg.Done()
				// fourier.CmplxFFT isn't goroutine-safe (internal scratch buffers),
				// so each worker owns a private coherentScratch.
				sc := newCoherentScratch()
				for {
					// Wall-clock budget check: if a deadline is set and has
					// passed, abort. In-flight candidates already mid-decodeOne
					// finish their current attempt; remaining candidates go
					// un-processed. This protects the next slot's audio capture
					// from CPU starvation.
					if hasDeadline && time.Now().After(deadline) {
						return
					}
					i := int(next.Add(1) - 1)
					if i >= len(candidates) {
						return
					}
					if d, ok := decodeOne(i, candidates[i], sc); ok {
						outs[wIdx].results = append(outs[wIdx].results, d)
						if dedupedOnDecode != nil {
							dedupedOnDecode(d)
						}
					}
				}
			}(w)
		}
		wg.Wait()
		decMs := time.Since(tDecode).Milliseconds()
		if logging.L != nil {
			logging.L.Infow("decode phases",
				"slot", slotStart.Format("15:04:05"),
				"pass", passIdx,
				"wf_ms", wfMs,
				"spec_ms", specMs,
				"cand_ms", candMs,
				"decode_ms", decMs,
				"candidates", len(candidates),
			)
		}

		var results []Decoded
		for _, o := range outs {
			results = append(results, o.results...)
		}
		return results
	} // end runPass

	// Pass 1: decode raw audio.
	results := runPass(0, work)

	// Subtract high-confidence BP decodes, then re-run pass 2 IF the slot
	// looks busy enough for adjacent-masking to matter. Subtraction is only
	// safe on decodes we're very confident about — incorrect tones at an
	// incorrect freq/time would leave a corrupted residue that could produce
	// phantom decodes in pass 2. BP-verified decodes at agreement ≥ 0.95 are
	// ~certain to be correct.
	//
	// Gate: pass 2 only fires when ≥ 3 high-confidence BP decodes appear in
	// pass 1. On quieter slots the cost (~2.5 s / slot of extra waterfall,
	// spectrum, candidate, and BP work) isn't paid back by enough new
	// decodes — corpus testing showed pass 2 adds ~6 decodes total across
	// 22 slots, concentrated in the busiest slots where masking dominates.
	subtracted := 0
	if len(results) >= 3 {
		ss := newSubtractScratch(len(work))
		for _, d := range results {
			// Subtract only BP decodes (Method=="" — full-codeword certain).
			// Corpus sweep on the agreement gate:
			//   ≥ 0.95 (was) F1 0.739 (R=0.598)
			//   ≥ 0.85       F1 0.750 (R=0.613)
			//   ≥ 0.80       F1 0.751 (R=0.616) ← peak
			//   ≥ 0.75       F1 0.750 (R=0.615)
			// Lowering from 0.95 → 0.80 unmasks ~11 weak signals that were
			// adjacent-channel-occluded by medium-agreement strong decodes.
			// Adding OSD/AP subtraction (≥ 0.95) added phantoms without
			// helping recall — the reconstructed tones aren't precise
			// enough and the residue creates pass-2 phantom candidates.
			if d.Method == "" && d.Agreement < 0.85 {
				continue
			}
			if d.Method != "" && d.Agreement < 0.98 {
				continue
			}
			subtractFT8(work, ss, d.tones, d.Freq, d.refinedXdt)
			subtracted++
		}
	}
	// Pass-2 also respects the wall budget: skip if the deadline is past.
	// Pass 2 typically takes another 5-7 s, so if we're already late after
	// pass 1 we'd push well past the slot boundary and starve next slot's
	// audio capture.
	deadlinePast := hasDeadline && time.Now().After(deadline)
	if subtracted >= 3 && !deadlinePast && os.Getenv("NOCORDHF_NO_PASS2") != "1" {
		pass2 := runPass(1, work)
		// Merge: keep all pass-1 results; add pass-2 decodes that aren't a
		// duplicate of a pass-1 decode's message text (same signal re-decoded
		// via residual energy after imperfect subtraction). A pass-2 rescue
		// that lands within 30 Hz of a pass-1 BP decode is also filtered as
		// likely residue — BP wouldn't have missed a real signal that close
		// in pass 1, so the rescue-on-subtracted-audio is almost certainly
		// reading remainder bits from the subtracted signal. Real adjacent
		// signals ≥30 Hz apart come through, which is the unmask win.
		for _, r := range pass2 {
			dup := false
			for _, s := range results {
				if s.Message.Text == r.Message.Text {
					dup = true
					break
				}
				if r.Method != "" && s.Method == "" && math.Abs(s.Freq-r.Freq) < 15 {
					dup = true
					break
				}
			}
			if !dup {
				results = append(results, r)
			}
		}
	}

	// Sidelobe filter: originally dropped any OSD/AP rescue within 30 Hz of
	// a BP decode on the theory that an adjacent rescue was a CRC-collision
	// phantom reading sidelobe bits from the real signal. With the reference design-
	// style `nharderrors ≤ 36` gate already in place, this filter now drops
	// real third-party decodes (corpus-confirmed: N3ALN/WZ9B at 585 Hz got
	// killed by an adjacent BP decode even though OSD matched the real
	// codeword at 0.85 agreement). The gate alone is strong enough without
	// the positional mask, so the filter is now a no-op that we keep as a
	// label for future targeted tightening if a specific phantom class
	// re-emerges.
	filtered := results

	// Same-message dedup: strong signals sometimes register as candidates at
	// two adjacent sub-bins (e.g. "N6YYZ KK9AWA EN52" at 1703.1 and 1706.2).
	// Collapse duplicates within 10 Hz of each other, keeping the strongest SNR.
	const dedupHz = 10.0
	kept := filtered[:0]
	for i, r := range filtered {
		dup := false
		for j := 0; j < i; j++ {
			s := filtered[j]
			if s.Message.Text == r.Message.Text && math.Abs(s.Freq-r.Freq) < dedupHz {
				if r.SNR > s.SNR {
					filtered[j] = r // replace weaker earlier entry
				}
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, r)
		}
	}
	return kept
}

// ProbeMatch describes how well one candidate's soft-decision LLRs agree with
// the hard-decision bits of an expected codeword. Used by Probe() to answer
// "is the signal for this message present at this freq?"
type ProbeMatch struct {
	Candidate  Candidate
	Agreement  float64 // fraction of 174 codeword bits whose sign matches the LLR
	BPDecoded  bool    // did plain BP converge to this message?
	BPMatches  bool    // did BP converge to these exact bits (vs a different message)?
	OSDDecoded bool    // did OSD find a CRC-valid codeword from these LLRs?
	OSDMatches bool    // did OSD recover the expected message exactly?
	LLRMeanAbs float64 // mean |llr| — proxy for LLR magnitude / channel confidence
}

// Probe packs msgText as a Type-1 message, builds its expected 174-bit codeword,
// and reports LLR agreement for every candidate within ±freqHalfWindowHz of
// probeFreqHz. Use freqHalfWindowHz=0 to scan the entire candidate list.
// Results are sorted by agreement descending.
//
// Purpose: brute-force diagnosis of "why did nocordhf miss this decode?". If the
// top match has high agreement (≥ ~0.70) but BPDecoded is false, the signal is
// present and correctly located — BP simply failed to converge and we have a
// decoder bug / parameter issue. If all candidates' agreement is ≤ ~0.55, the
// signal isn't at the expected freq (bad reference-report freq, or missing candidate).
func Probe(samples []float32, msgText string, probeFreqHz, freqHalfWindowHz float64) ([]ProbeMatch, error) {
	const probeMinSamples = sampleRate * 27 / 2
	if len(samples) < probeMinSamples {
		return nil, fmt.Errorf("need ≥ %d samples, got %d", probeMinSamples, len(samples))
	}
	if len(samples) < windowSamples {
		padded := make([]float32, windowSamples)
		copy(padded, samples)
		samples = padded
	} else {
		samples = samples[:windowSamples]
	}

	expected, err := packStandard(msgText)
	if err != nil {
		return nil, fmt.Errorf("pack %q: %w", msgText, err)
	}
	if !CheckCRC(expected) {
		return nil, fmt.Errorf("packStandard produced invalid CRC for %q", msgText)
	}
	codeword := encodeLDPC(expected)

	wf := buildWaterfall(samples)
	cands := findCandidates(wf)

	var out []ProbeMatch
	for _, c := range cands {
		if freqHalfWindowHz > 0 && math.Abs(c.Freq-probeFreqHz) > freqHalfWindowHz {
			continue
		}
		llr := extractLLR(wf, c)
		agree := 0
		for i := 0; i < N; i++ {
			hard := byte(0)
			if llr[i] < 0 {
				hard = 1
			}
			if codeword[i] == hard {
				agree++
			}
		}
		var arr [N]float64
		copy(arr[:], llr)
		decoded, ok := DecodeLDPC(arr)
		var msgBits [K]byte
		copy(msgBits[:], expected[:K])
		osdBits, osdOK := decodeOSD(arr)
		osdMatches := osdOK && osdBits == msgBits
		var sumAbs float64
		for _, v := range llr {
			if v < 0 {
				sumAbs -= v
			} else {
				sumAbs += v
			}
		}
		out = append(out, ProbeMatch{
			Candidate:  c,
			Agreement:  float64(agree) / float64(N),
			BPDecoded:  ok && CheckCRC(decoded),
			BPMatches:  ok && decoded == msgBits,
			OSDDecoded: osdOK && CheckCRC(osdBits),
			OSDMatches: osdMatches,
			LLRMeanAbs: sumAbs / float64(N),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Agreement > out[j].Agreement })
	return out, nil
}

// computeSNR estimates the candidate's SNR in reference design 2500 Hz-reference
// convention. Signal power is the mean magnitude at the known-TX tone over
// all 79 symbols; noise power is the median of nearby off-tone bins on each
// side of the signal, taking the lower of the two sides to stay robust
// against an adjacent strong signal contaminating one side's noise band.
// The bin-bandwidth→2500 Hz correction turns an FFT-bin SNR into the
// wider-bandwidth SNR reference design reports.
//
// Accuracy: matches reference design within ~±3 dB for typical signals above –20 dB.
// The lower-side/upper-side min-of-medians approach specifically handles the
// case where a +10..+20 dB signal sits within ±100 Hz of the candidate and
// inflates a simple linear-mean noise estimate by 20+ dB.
func computeSNR(wf *waterfall, cand Candidate, tones [NumSymbols]int) float64 {
	// mag is stored as 0.5 dB per unit offset from –120 dB floor, so linear
	// power relative to full-scale is 10^((m-240)/10 * 0.5) = 10^((m-240)/20).
	magToPower := func(m uint8) float64 {
		return math.Pow(10, (float64(m)-240)/20)
	}

	var sumSig float64
	var nSig int
	var noiseLow, noiseHigh []float64
	for k := 0; k < NumSymbols; k++ {
		block := cand.TimeOffset + k
		if block < 0 || block >= wf.numBlocks {
			continue
		}
		base := block*blockStride + cand.TimeSub*freqOSR*numBins + cand.FreqSub*numBins

		// Signal: power at the actual transmitted tone (via gray-map inverse).
		tone := tones[k]
		sigBin := cand.FreqOffset + tone
		if sigBin >= 0 && sigBin < numBins {
			sumSig += magToPower(wf.mag[base+sigBin])
			nSig++
		}

		// Noise: 8 bins below and 8 bins above the 8-tone range, collected
		// separately so we can pick the cleaner side.
		for off := -16; off <= -9; off++ {
			i := cand.FreqOffset + off
			if i >= 0 && i < numBins {
				noiseLow = append(noiseLow, magToPower(wf.mag[base+i]))
			}
		}
		for off := 9; off <= 16; off++ {
			i := cand.FreqOffset + off
			if i >= 0 && i < numBins {
				noiseHigh = append(noiseHigh, magToPower(wf.mag[base+i]))
			}
		}
	}
	if nSig == 0 || (len(noiseLow) == 0 && len(noiseHigh) == 0) {
		return cand.SNR
	}
	avgSig := sumSig / float64(nSig)
	medLow := medianFloat(noiseLow)
	medHigh := medianFloat(noiseHigh)
	avgNoise := medLow
	if medHigh > 0 && (avgNoise == 0 || medHigh < avgNoise) {
		avgNoise = medHigh
	}
	if avgNoise <= 0 || avgSig <= 0 {
		return cand.SNR
	}
	// Per-bin SNR → 2500 Hz reference: noise in 2500 Hz = noise_per_bin × (2500/dfHz).
	// A real-valued tone splits its power between +f and -f FFT bins, so the positive-
	// frequency bin we measure holds half the true signal power. The +3 dB corrects
	// for that one-sided-spectrum underestimate.
	snrBin := 10 * math.Log10(avgSig/avgNoise)
	return snrBin - 10*math.Log10(2500.0/dfHz) + 10*math.Log10(2.0)
}

// medianFloat returns the median of xs. Returns 0 for empty input. Uses an
// in-place partial sort (O(n log n)) — fine for the small slices here.
func medianFloat(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	sort.Float64s(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return 0.5 * (xs[n/2-1] + xs[n/2])
}

// buildWaterfall computes the ft8_lib-style waterfall using Hann-windowed 3840-pt FFTs
// at 960-sample (half-symbol) stride. Magnitudes are stored as dB-scaled uint8 (0-255,
// covering -120..0 dB in 0.5 dB steps), matching ft8_lib monitor.c.
// wfWindow is the Hann window scaled by fft_norm = 2/nfft, pre-computed once
// at package init (it's position-independent and used by every FFT frame).
var wfWindow = func() []float64 {
	fftNorm := 2.0 / float64(nfft)
	w := make([]float64, nfft)
	for i := range w {
		s := math.Sin(math.Pi * float64(i) / float64(nfft))
		w[i] = fftNorm * s * s
	}
	return w
}()

// buildWaterfall parallelizes the STFT across (block, timeSub) frames.
// Each frame is computed directly from its absolute sample position rather
// than by slide-from-previous, which makes the frames embarrassingly
// parallel. gonum's fourier.FFT is not goroutine-safe (it holds scratch
// state in the plan), so each worker gets its own plan + input buffer.
func buildWaterfall(samples []float32) *waterfall {
	wf := &waterfall{
		mag: make([]uint8, maxBlocks*blockStride),
	}
	wf.numBlocks = maxBlocks

	totalFrames := maxBlocks * timeOSR
	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers > totalFrames {
		numWorkers = totalFrames
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			plan := fourier.NewFFT(nfft)
			in := make([]float64, nfft)
			out := make([]complex128, nfft/2+1)
			for {
				fi := int(next.Add(1) - 1)
				if fi >= totalFrames {
					return
				}
				block := fi / timeOSR
				timeSub := fi % timeOSR
				// Absolute sample position of this frame's LAST sample+1.
				// (block*timeOSR + timeSub + 1) subblockSize chunks have been
				// "consumed" at that point, and the window is the last nfft
				// samples ending there. Pre-slot samples are zero.
				endSample := (block*timeOSR + timeSub + 1) * subblockSize
				startSample := endSample - nfft
				for pos := 0; pos < nfft; pos++ {
					sp := startSample + pos
					var s float64
					if sp >= 0 && sp < len(samples) {
						s = float64(samples[sp])
					}
					in[pos] = wfWindow[pos] * s
				}
				out = plan.Coefficients(out, in)

				offset := block*blockStride + timeSub*freqOSR*numBins
				for freqSub := 0; freqSub < freqOSR; freqSub++ {
					base := offset + freqSub*numBins
					for binIdx := 0; binIdx < numBins; binIdx++ {
						srcBin := (minBin+binIdx)*freqOSR + freqSub
						if srcBin >= len(out) {
							continue
						}
						re := real(out[srcBin])
						im := imag(out[srcBin])
						mag2 := re*re + im*im
						db := 10.0 * math.Log10(1e-12+mag2)
						scaled := int(2*db + 240)
						if scaled < 0 {
							scaled = 0
						} else if scaled > 255 {
							scaled = 255
						}
						wf.mag[base+binIdx] = uint8(scaled)
					}
				}
			}
		}()
	}
	wg.Wait()
	return wf
}

// wfMag retrieves a magnitude value from the waterfall at the given position.
func wfMag(wf *waterfall, block, timeSub, freqSub, bin int) int {
	if block < 0 || block >= wf.numBlocks || bin < 0 || bin >= numBins {
		return 0
	}
	offset := block*blockStride + timeSub*freqOSR*numBins + freqSub*numBins + bin
	return int(wf.mag[offset])
}

// ft8SyncScore computes the sync score for a candidate matching ft8_lib's ft8_sync_score.
// Compares expected Costas tone to neighboring bins in frequency and time.
func ft8SyncScore(wf *waterfall, timeOffset, timeSub, freqSub, freqOffset int) int {
	score := 0
	numAverage := 0

	for m := 0; m < numSyncBlocks; m++ {
		for k := 0; k < lenSyncBlock; k++ {
			block := syncOffset*m + k
			blockAbs := timeOffset + block
			if blockAbs < 0 {
				continue
			}
			if blockAbs >= wf.numBlocks {
				break
			}

			sm := costasSeq[k]
			// Magnitude at expected tone
			cur := wfMag(wf, blockAbs, timeSub, freqSub, freqOffset+sm)

			if sm > 0 {
				score += cur - wfMag(wf, blockAbs, timeSub, freqSub, freqOffset+sm-1)
				numAverage++
			}
			if sm < 7 {
				score += cur - wfMag(wf, blockAbs, timeSub, freqSub, freqOffset+sm+1)
				numAverage++
			}
			if k > 0 && blockAbs > 0 {
				// One symbol back in time (previous block, same timeSub)
				prev := wfMag(wf, blockAbs-1, timeSub, freqSub, freqOffset+sm)
				score += cur - prev
				numAverage++
			}
			if k+1 < lenSyncBlock && blockAbs+1 < wf.numBlocks {
				next := wfMag(wf, blockAbs+1, timeSub, freqSub, freqOffset+sm)
				score += cur - next
				numAverage++
			}
		}
	}

	if numAverage > 0 {
		score /= numAverage
	}
	return score
}

// findCandidates scans the waterfall with ft8_lib's sync scoring.
// time_offset ranges -15 to +18 symbols (~±2.4s late, ~2.4s early) to capture
// peers with slightly fast clocks plus the typical TX-late-by-200ms drift.
//
// minScore tuning: nocordhf previously used minScore=5 (vs jt9's 2.0).
// Live testing on busy FT8 slots showed nocordhf missing ~70% of jt9's decodes
// because minScore=5 filtered them out before decoder even tried. Root cause:
// sync8 in ft8d.f90 (reference jt9 implementation) uses syncmin=2.0, far more
// permissive than nocordhf's 5. The downstream phantom-rejection gates
// (hard_errors≤36, time_offset, hash_bracket, weighted_agreement) are
// sufficient to reject noise candidates at low sync scores. Lowering to
// minScore=2 aligns nocordhf with jt9 and recovers ~60-70% more decodes.
func findCandidates(wf *waterfall) []Candidate {
	const minScore = 2

	// Parallelize across the 4 (timeSub, freqSub) combinations. The inner
	// (timeOff, freqOff) sweeps are fully independent reads into wf.mag.
	type bucket struct{ cands []Candidate }
	buckets := make([]bucket, timeOSR*freqOSR)
	var wg sync.WaitGroup
	for timeSub := 0; timeSub < timeOSR; timeSub++ {
		for freqSub := 0; freqSub < freqOSR; freqSub++ {
			wg.Add(1)
			go func(timeSub, freqSub int) {
				defer wg.Done()
				b := &buckets[timeSub*freqOSR+freqSub]
				for timeOff := -15; timeOff <= 18; timeOff++ {
					for freqOff := 0; freqOff+7 < numBins; freqOff++ {
						score := ft8SyncScore(wf, timeOff, timeSub, freqSub, freqOff)
						if score < minScore {
							continue
						}
						srcBin := float64((minBin+freqOff)*freqOSR + freqSub)
						freq := srcBin * dfHz
						startSample := timeOff*SamplesPerSym + timeSub*subblockSize
						timeSecs := float64(startSample) / float64(sampleRate)
						b.cands = append(b.cands, Candidate{
							TimeOffset: timeOff,
							TimeSub:    timeSub,
							FreqSub:    freqSub,
							FreqOffset: freqOff,
							Score:      score,
							Freq:       freq,
							TimeSecs:   timeSecs,
							SNR:        float64(score) - 20.0,
						})
					}
				}
			}(timeSub, freqSub)
		}
	}
	wg.Wait()

	total := 0
	for i := range buckets {
		total += len(buckets[i].cands)
	}
	cands := make([]Candidate, 0, total)
	for i := range buckets {
		cands = append(cands, buckets[i].cands...)
	}
	// Sort candidates by sync score descending. Dedup walks neighbours so it
	// needs sorted input; the same order is then used by the cap below to
	// keep the highest-scoring survivors.
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].Score > cands[j].Score
	})
	deduped := deduplicateCandidates(cands)
	capped := capCandidates(deduped)
	if logging.L != nil {
		logging.L.Debugw("candidates", "raw", len(cands), "deduped", len(deduped), "capped", len(capped))
	}
	return capped
}

// candCapPercent / candCapFloor / candCapCeiling shape the per-slot
// candidate budget that BP+OSD will actually process. Profile data shows
// BP+OSD accounts for ~87% of decode CPU, so capping here is the highest-
// leverage place to control wall time.
//
// Quiet slots (≤floor candidates) pass through untouched so weak-signal
// recall is preserved. Busy slots are clamped to the ceiling so a noise-
// flooded band can't blow the wall budget. Between those bounds we keep
// the top X% by sync score, mirroring jt9 sync8's percentile-floor design.
const (
	candCapPercent = 60  // keep top N% by sync score
	candCapFloor   = 200 // always process at least this many
	candCapCeiling = 600 // never process more than this many
)

func capCandidates(cands []Candidate) []Candidate {
	n := len(cands)
	if n <= candCapFloor {
		return cands
	}
	keep := n * candCapPercent / 100
	if keep < candCapFloor {
		keep = candCapFloor
	}
	if keep > candCapCeiling {
		keep = candCapCeiling
	}
	return cands[:keep]
}

// extractLLR extracts 174 soft LLR values from the waterfall for a candidate,
// matching ft8_lib's ft8_extract_likelihood and ft8_extract_symbol.
func extractLLR(wf *waterfall, cand Candidate) []float64 {
	llr := make([]float64, N)
	llrIdx := 0

	// FT8_ND = 58 data symbols; skip Costas blocks at 0-6, 36-42, 72-78
	for k := 0; k < 58 && llrIdx < N; k++ {
		var symIdx int
		if k < 29 {
			symIdx = k + 7
		} else {
			symIdx = k + 14
		}

		blockAbs := cand.TimeOffset + symIdx
		if blockAbs < 0 || blockAbs >= wf.numBlocks {
			llr[llrIdx] = 0
			llr[llrIdx+1] = 0
			llr[llrIdx+2] = 0
			llrIdx += 3
			continue
		}

		// Get pointer base: waterfall offset for this symbol
		base := blockAbs*blockStride + cand.TimeSub*freqOSR*numBins + cand.FreqSub*numBins + cand.FreqOffset

		// s2[value] = mag at tone grayMap[value]
		var s2 [8]float64
		for v := 0; v < 8; v++ {
			tone := grayMap[v]
			if cand.FreqOffset+tone < numBins {
				s2[v] = float64(wf.mag[base+tone])
			}
		}

		max4 := func(a, b, c, d float64) float64 {
			m := a
			if b > m {
				m = b
			}
			if c > m {
				m = c
			}
			if d > m {
				m = d
			}
			return m
		}

		llr[llrIdx+0] = max4(s2[4], s2[5], s2[6], s2[7]) - max4(s2[0], s2[1], s2[2], s2[3])
		llr[llrIdx+1] = max4(s2[2], s2[3], s2[6], s2[7]) - max4(s2[0], s2[1], s2[4], s2[5])
		llr[llrIdx+2] = max4(s2[1], s2[3], s2[5], s2[7]) - max4(s2[0], s2[2], s2[4], s2[6])
		llrIdx += 3
	}

	// Normalize: scale so variance = 24 (matching ft8_lib's ftx_normalize_logl).
	// ft8_lib positive = bit1; our LDPC positive = bit0, so negate after normalization.
	var sum, sum2 float64
	for _, v := range llr {
		sum += v
		sum2 += v * v
	}
	invN := 1.0 / float64(N)
	variance := (sum2 - sum*sum*invN) * invN
	if variance > 0 {
		norm := math.Sqrt(24.0 / variance)
		for i := range llr {
			llr[i] = -llr[i] * norm
		}
	}

	return llr
}

func deduplicateCandidates(cands []Candidate) []Candidate {
	used := make([]bool, len(cands))
	out := make([]Candidate, 0, len(cands))
	for i := range cands {
		if used[i] {
			continue
		}
		// Collect cluster: all candidates within 6.25 Hz and 2 symbols of c.
		// Keep the top 2 by sync score — a weak real signal and a same-bin
		// phantom can both score similarly, and picking only the best meant
		// the real decode never reached BP. Confirmed cases in live logs:
		// real HI3QMT at 2866 stepped on by phantom sync at 2865.6; real
		// KO4USQ at 1601 stepped on by phantom at 1603.
		cluster := []int{i}
		for j := i + 1; j < len(cands); j++ {
			if used[j] {
				continue
			}
			freqDiff := math.Abs(cands[j].Freq - cands[i].Freq)
			timeDiff := math.Abs(float64(cands[j].TimeOffset-cands[i].TimeOffset)) * SymbolDuration
			if freqDiff < ToneSpacing && timeDiff < SymbolDuration*2 {
				cluster = append(cluster, j)
			}
		}
		sort.SliceStable(cluster, func(a, b int) bool {
			return cands[cluster[a]].Score > cands[cluster[b]].Score
		})
		for _, idx := range cluster {
			used[idx] = true
		}
		// Keep only the top-3 per cluster. Each (4 timeSub × 2 freqSub)
		// oversampling cell contributes one entry per real signal, so a
		// good signal typically produces 8 near-duplicate candidates;
		// past index 3 the rest are sub-optimal-timing duplicates that
		// rarely produce a different decode. Capping at 3 cuts BP/OSD
		// work substantially without measurably reducing recall (reference design
		// keeps just the single best per cluster).
		keep := len(cluster)
		if keep > 3 {
			keep = 3
		}
		for k := 0; k < keep; k++ {
			out = append(out, cands[cluster[k]])
		}
	}
	return out
}

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}
