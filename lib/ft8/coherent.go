package ft8

// coherent.go — per-candidate coherent downsampling and LLR extraction.
//
// Mirrors reference design ft8_downsample.f90 + ft8b.f90 (through line 233):
//  1. Compute long real→complex FFT of the full slot once, share across candidates.
//  2. For each candidate: extract the band [f0−1.5·baud, f0+8.5·baud] from the
//     long spectrum, edge-taper, circular-shift so the carrier is at bin 0,
//     inverse-FFT to complex baseband at 200 Hz (3200 samples).
//  3. Coherent DT/freq refinement against the 3×7 Costas reference:
//     first a ±10-sample DT sweep (±50 ms at 200 Hz), then a ±5×0.5 Hz freq
//     peak-up with a complex-twiddle applied to the Costas reference.
//  4. Apply the best freq correction to the baseband and re-run the downsample,
//     then a second ±4-sample DT refinement.
//  5. Per-symbol 32-point complex FFT at the refined ibest gives 8 complex
//     tone coefficients per symbol; LLRs are built exactly like extractLLR
//     (max-logmap over bit-set vs bit-clear tones) but using those refined
//     magnitudes instead of the integer-bin STFT magnitudes.
//
// Phase-1a scope: single-symbol max-logmap only (bmeta). Multi-symbol joint
// LLRs (bmetb/bmetc) and bit-normalized LLR (bmetd) are deferred to a later
// pass that runs them as alternates when the single-symbol LLR fails to decode.

import (
	"math"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	coFS2      = 200            // baseband sample rate (Hz)
	coNDown    = 60             // decimation factor (12000 / 200)
	coNFFT1    = 192000         // long FFT size (zero-pads the 180000-sample slot)
	coNFFT2    = 3200           // short FFT size for the baseband IFFT
	coNP2      = 2812           // usable baseband samples (matches reference design NP2)
	coSymSamps = 32             // samples per FT8 symbol at 200 Hz
	coTaperLen = 100            // edge taper length (each side of the extracted band)
	coFT8Baud  = 12000.0 / 1920 // = 6.25 Hz per tone
)

// coTaper is a half-raised-cosine window used to soften the spectrum extraction
// edges before the inverse FFT. taper[0]=1 at the near-DC edge, taper[N]=0 at
// the stop-band edge. Matches ft8_downsample.f90:18-20.
var coTaper [coTaperLen + 1]float64

func init() {
	for i := 0; i <= coTaperLen; i++ {
		coTaper[i] = 0.5 * (1.0 + math.Cos(float64(i)*math.Pi/float64(coTaperLen)))
	}
}

// slotSpectrum holds the long r2c FFT of a full 15-second slot, computed once
// at the start of Decode and shared read-only across all candidate workers.
type slotSpectrum struct {
	spec []complex128 // length coNFFT1/2 + 1
}

// costasPattern is the 3-by-7 sync block pattern in FT8: same 7-symbol Costas
// array at sym indices 0..6, 36..42, 72..78 (0-based). Same as costasSeq but
// expressed as a flat table for coherent sync use.
var costasPattern = [7]int{3, 1, 4, 0, 6, 5, 2}

// costasRef[i][j] is the unit-magnitude complex waveform of the i-th Costas
// tone sampled at FFT-bin spacing over the 32 symbol samples. Pre-computed at
// init so coherent sync doesn't rebuild it per call. Mirrors the csync array
// in sync8d.f90:24-30.
var costasRef [7][coSymSamps]complex128

func init() {
	twopi := 2 * math.Pi
	for i := 0; i < 7; i++ {
		dphi := twopi * float64(costasPattern[i]) / float64(coSymSamps)
		phi := 0.0
		for j := 0; j < coSymSamps; j++ {
			costasRef[i][j] = complex(math.Cos(phi), math.Sin(phi))
			phi = math.Mod(phi+dphi, twopi)
		}
	}
}

// computeSlotSpectrum does the one-shot long FFT of a slot's audio samples.
// Input is len-180000 float32 at 12 kHz; padded to coNFFT1 for a nicer FFT size.
func computeSlotSpectrum(samples []float32) *slotSpectrum {
	in := make([]float64, coNFFT1)
	n := len(samples)
	if n > coNFFT1 {
		n = coNFFT1
	}
	for i := 0; i < n; i++ {
		in[i] = float64(samples[i])
	}
	plan := fourier.NewFFT(coNFFT1)
	spec := plan.Coefficients(nil, in)
	return &slotSpectrum{spec: spec}
}

// coherentScratch is per-worker reusable state. fourier.CmplxFFT is NOT
// goroutine-safe (holds scratch buffers internally), so each goroutine that
// calls coherent operations must own its own coherentScratch.
type coherentScratch struct {
	ifft *fourier.CmplxFFT // inverse FFT plan of size coNFFT2
	fft  *fourier.CmplxFFT // forward FFT plan of size coSymSamps (per-symbol 32-pt)
	c1   []complex128      // working buffer of length coNFFT2 for downsample
	bb   []complex128      // refined baseband of length coNFFT2
	sym  []complex128      // per-symbol scratch of length coSymSamps
}

func newCoherentScratch() *coherentScratch {
	return &coherentScratch{
		ifft: fourier.NewCmplxFFT(coNFFT2),
		fft:  fourier.NewCmplxFFT(coSymSamps),
		c1:   make([]complex128, coNFFT2),
		bb:   make([]complex128, coNFFT2),
		sym:  make([]complex128, coSymSamps),
	}
}

// downsample extracts the band around f0Hz from the slot's long spectrum and
// inverse-transforms to a 200-Hz complex baseband (3200 samples). Writes the
// result into out (must be len(coNFFT2)). Matches ft8_downsample.f90.
func (s *slotSpectrum) downsample(sc *coherentScratch, f0Hz float64, out []complex128) {
	df := float64(sampleRate) / float64(coNFFT1) // 0.0625 Hz/bin
	i0 := int(math.Round(f0Hz / df))             // bin index of carrier
	ft := f0Hz + 8.5*coFT8Baud                   // top edge (8 tones + 0.5 guard above)
	it := int(math.Round(ft / df))
	if it > coNFFT1/2 {
		it = coNFFT1 / 2
	}
	fb := f0Hz - 1.5*coFT8Baud // bottom edge (1.5-tone guard below)
	ib := int(math.Round(fb / df))
	if ib < 1 {
		ib = 1
	}

	c1 := sc.c1
	for i := range c1 {
		c1[i] = 0
	}
	k := 0
	for i := ib; i <= it && k < coNFFT2; i++ {
		if i >= 0 && i < len(s.spec) {
			c1[k] = s.spec[i]
		}
		k++
	}

	// Edge taper on the extracted band, mirroring ft8_downsample.f90:43-44.
	if k > 2*coTaperLen {
		for i := 0; i <= coTaperLen; i++ {
			c1[i] *= complex(coTaper[coTaperLen-i], 0)
		}
		for i := 0; i <= coTaperLen; i++ {
			c1[k-1-coTaperLen+i] *= complex(coTaper[i], 0)
		}
	}

	// Circular shift by (i0 - ib) positions so the carrier lands at bin 0.
	// Fortran cshift(array, n>0) rotates left; same semantics here.
	shift := i0 - ib
	if shift != 0 {
		shift = ((shift % coNFFT2) + coNFFT2) % coNFFT2
		rotateLeftComplex(c1, shift)
	}

	// Inverse FFT (freq → time). gonum's Sequence is unnormalized IFFT, so a
	// full forward+inverse round-trip multiplies by N. reference design additionally
	// scales by 1/sqrt(nfft1 * nfft2) as a unit convention; overall scale is
	// absorbed by LLR normalization downstream so the exact factor is moot.
	sc.ifft.Sequence(out, c1)
	fac := 1.0 / math.Sqrt(float64(coNFFT1)*float64(coNFFT2))
	for i := range out {
		out[i] *= complex(fac, 0)
	}
}

// rotateLeftComplex rotates c left in place by n positions (n ≥ 0).
func rotateLeftComplex(c []complex128, n int) {
	if n == 0 || n >= len(c) {
		return
	}
	reverseComplex(c[:n])
	reverseComplex(c[n:])
	reverseComplex(c)
}
func reverseComplex(c []complex128) {
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
}

// sync8d computes the coherent Costas sync power at baseband start sample i0.
// If twk is non-nil, each Costas reference waveform is multiplied element-wise
// by twk before correlation — used for the fractional-Hz freq peak-up step.
// Mirrors sync8d.f90.
func sync8d(cd0 []complex128, i0 int, twk []complex128) float64 {
	var sync float64
	for i := 0; i < 7; i++ {
		i1 := i0 + i*coSymSamps
		i2 := i1 + 36*coSymSamps
		i3 := i1 + 72*coSymSamps
		var z1, z2, z3 complex128
		ref := costasRef[i]
		if i1 >= 0 && i1+coSymSamps-1 <= coNP2-1 {
			for j := 0; j < coSymSamps; j++ {
				r := ref[j]
				if twk != nil {
					r = twk[j] * r
				}
				// cd0 * conj(ref)
				z1 += cd0[i1+j] * complex(real(r), -imag(r))
			}
		}
		if i2 >= 0 && i2+coSymSamps-1 <= coNP2-1 {
			for j := 0; j < coSymSamps; j++ {
				r := ref[j]
				if twk != nil {
					r = twk[j] * r
				}
				z2 += cd0[i2+j] * complex(real(r), -imag(r))
			}
		}
		if i3 >= 0 && i3+coSymSamps-1 <= coNP2-1 {
			for j := 0; j < coSymSamps; j++ {
				r := ref[j]
				if twk != nil {
					r = twk[j] * r
				}
				z3 += cd0[i3+j] * complex(real(r), -imag(r))
			}
		}
		sync += power(z1) + power(z2) + power(z3)
	}
	return sync
}

func power(z complex128) float64 {
	r, i := real(z), imag(z)
	return r*r + i*i
}

// refineCandidate performs reference design two-pass time/frequency refinement of a
// candidate that was flagged by the STFT-based sync scanner. Returns the best
// baseband start sample (ibest) and the frequency correction delf (Hz).
// ft8b.f90:104-151. The caller should apply delf to the freq used for the
// second downsample call, and use ibest for symbol extraction.
//
// timeSecs is the candidate's nominal Costas-start time in seconds from the
// slot boundary (ft8_lib convention — already includes the ~0.5 s FT8 signal
// offset). The baseband produced by downsample is aligned to slot time 0, so
// the Costas start lands at baseband sample index timeSecs*fs2.
func refineCandidate(sc *coherentScratch, spec *slotSpectrum, f0Hz, timeSecs float64) (ibest int, delf float64) {
	// First downsample at f0.
	spec.downsample(sc, f0Hz, sc.bb)

	// Initial DT guess in baseband samples. The baseband time origin matches
	// the slot start (downsample is just a linear frequency-domain operation
	// on the slot's long FFT), so this is simply timeSecs * fs2.
	i0 := int(math.Round(timeSecs * float64(coFS2)))

	// DT sweep ±30 samples (±150 ms). Our candidate generator uses time_osr=2
	// → 80 ms timeSecs quantization, so the initial i0 can be off by ±40 ms;
	// the signal itself can then be offset another 50-100 ms on top (propagation,
	// local clock skew). The wider sweep absorbs both without materially
	// increasing noise sync-locks thanks to Costas matched-filter selectivity.
	smax := 0.0
	ibest = i0
	for idt := i0 - 40; idt <= i0+40; idt++ {
		sync := sync8d(sc.bb, idt, nil)
		if sync > smax {
			smax = sync
			ibest = idt
		}
	}

	// Freq peak-up ±2.5 Hz at 0.5 Hz resolution. Apply a complex twiddle to the
	// Costas reference (equivalent to shifting the signal — reference design shifts the
	// reference; same effect because correlation is symmetric).
	twopi := 2 * math.Pi
	dt2 := 1.0 / float64(coFS2)
	twk := make([]complex128, coSymSamps)
	smax = 0.0
	delf = 0.0
	for ifr := -5; ifr <= 5; ifr++ {
		df := float64(ifr) * 0.5
		dphi := twopi * df * dt2
		phi := 0.0
		for j := 0; j < coSymSamps; j++ {
			twk[j] = complex(math.Cos(phi), math.Sin(phi))
			phi = math.Mod(phi+dphi, twopi)
		}
		sync := sync8d(sc.bb, ibest, twk)
		if sync > smax {
			smax = sync
			delf = df
		}
	}

	// Apply the freq correction by re-downsampling at the corrected carrier,
	// then refine DT once more at ±4 samples for a final lock.
	spec.downsample(sc, f0Hz+delf, sc.bb)
	smax = 0.0
	best2 := ibest
	for idt := ibest - 4; idt <= ibest+4; idt++ {
		sync := sync8d(sc.bb, idt, nil)
		if sync > smax {
			smax = sync
			best2 = idt
		}
	}
	ibest = best2
	return ibest, delf
}

// coherentLLRs holds all four reference design LLR variants for one candidate. Each
// variant is a [N=174]-element soft-bit vector in the same sign convention as
// the LDPC decoder (positive = bit 0). They differ in how tone magnitudes are
// combined:
//
//	bmeta — single-symbol max-logmap (one symbol per bit triple)
//	bmetb — two-symbol JOINT max-logmap (two adjacent symbols, complex-summed)
//	bmetc — three-symbol JOINT max-logmap (three adjacent symbols, complex-summed)
//	bmetd — bmeta bit-normalized to [-1, 1]
//
// Joint-symbol variants capture the coherent energy that gets smeared across
// symbol boundaries by timing jitter, Doppler spread, or between-symbol
// interference — situations where single-symbol bmeta shows a weak, noisy LLR
// but the combined signal energy is unambiguous. The decoder tries variants in
// order and stops at the first CRC-valid result.
type coherentLLRs struct {
	bmeta [N]float64
	bmetb [N]float64
	bmetc [N]float64
	bmetd [N]float64
}

// extractLLRCoherentAll builds all four reference design LLR variants in a single pass
// over the refined baseband. Mirrors ft8b.f90:154-233.
func extractLLRCoherentAll(sc *coherentScratch, cd0 []complex128, ibest int) *coherentLLRs {
	// Per-symbol complex coefficients: 8 tone bins per symbol for all 79
	// symbols. Joint-symbol LLRs (bmetb/bmetc) need COMPLEX values because
	// they sum across adjacent symbols before taking magnitude — incoherent
	// magnitude summation would lose the phase information that makes the
	// joint metric valuable.
	var cs [79][8]complex128
	for k := 0; k < 79; k++ {
		i1 := ibest + k*coSymSamps
		for j := 0; j < coSymSamps; j++ {
			sp := i1 + j
			if sp >= 0 && sp < coNP2 {
				sc.sym[j] = cd0[sp]
			} else {
				sc.sym[j] = 0
			}
		}
		sc.fft.Coefficients(sc.sym, sc.sym)
		for t := 0; t < 8; t++ {
			cs[k][t] = sc.sym[t]
		}
	}

	result := &coherentLLRs{}

	absC := func(z complex128) float64 {
		r, i := real(z), imag(z)
		return math.Sqrt(r*r + i*i)
	}

	// Per reference design: outer loop nsym=1..3 builds bmeta, bmetb, bmetc respectively.
	// bmetd is a by-product of the nsym=1 pass. For each nsym, inner loop walks
	// the 58 data symbols in two halves (syms 7..35 and 43..71) and emits bits
	// to a per-iteration position i32.
	var s2 [512]float64 // max nt is 2^9 for nsym=3
	for nsym := 1; nsym <= 3; nsym++ {
		nt := 1 << (3 * nsym)
		var ibmax int
		switch nsym {
		case 1:
			ibmax = 2 // 3 bits per iteration
		case 2:
			ibmax = 5 // 6 bits per pair
		case 3:
			ibmax = 8 // 9 bits per triple
		}
		for ihalf := 0; ihalf < 2; ihalf++ {
			for k := 0; k < 29; k += nsym {
				var ks int
				if ihalf == 0 {
					ks = k + 7
				} else {
					ks = k + 43
				}
				// Compute all nt candidate soft metrics for this (nsym, ks).
				for i := 0; i < nt; i++ {
					i1 := (i >> 6) & 7
					i2 := (i >> 3) & 7
					i3 := i & 7
					var z complex128
					switch nsym {
					case 1:
						z = cs[ks][grayMap[i3]]
					case 2:
						z = cs[ks][grayMap[i2]] + cs[ks+1][grayMap[i3]]
					case 3:
						z = cs[ks][grayMap[i1]] + cs[ks+1][grayMap[i2]] + cs[ks+2][grayMap[i3]]
					}
					s2[i] = absC(z)
				}
				// Position in the 174-bit vector (0-indexed). The k*3 stride
				// covers 3 bits per nsym=1 iter, and block 2 starts at 87.
				i32 := k*3 + ihalf*87
				for ib := 0; ib <= ibmax; ib++ {
					bitPos := uint(ibmax - ib)
					var maxSet, maxClear float64
					maxSet = -math.Inf(1)
					maxClear = -math.Inf(1)
					for i := 0; i < nt; i++ {
						v := s2[i]
						if (i>>bitPos)&1 == 1 {
							if v > maxSet {
								maxSet = v
							}
						} else {
							if v > maxClear {
								maxClear = v
							}
						}
					}
					bm := maxSet - maxClear
					pos := i32 + ib
					if pos >= N {
						continue
					}
					switch nsym {
					case 1:
						result.bmeta[pos] = bm
						den := maxSet
						if maxClear > den {
							den = maxClear
						}
						if den > 0 {
							result.bmetd[pos] = bm / den
						}
					case 2:
						result.bmetb[pos] = bm
					case 3:
						result.bmetc[pos] = bm
					}
				}
			}
		}
	}

	normalizeLLRs(&result.bmeta)
	normalizeLLRs(&result.bmetb)
	normalizeLLRs(&result.bmetc)
	normalizeLLRs(&result.bmetd)
	return result
}

// normalizeLLRs rescales an LLR vector to variance 24 and flips sign to match
// our LDPC convention (positive LLR = bit 0). reference design normalizes to unit variance
// then scales by scalefac=2.83 ≈ sqrt(8); variance 24 is equivalent — the LDPC
// sum-product decoder is scale-invariant over a reasonable range so either
// convention decodes identically.
func normalizeLLRs(llr *[N]float64) {
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
}

// coherentDecodeOne is the per-candidate coherent LLR path: refine the
// candidate's time/freq offset against the Costas reference, then build all
// four LLR variants from the refined baseband. Returns the LLR variants plus
// the refined signal-start time in seconds (ibest / 200 Hz), which the
// subtract-and-retry pass needs to accurately regenerate the signal waveform.
func coherentDecodeOne(sc *coherentScratch, spec *slotSpectrum, cand Candidate) (*coherentLLRs, float64) {
	ibest, _ := refineCandidate(sc, spec, cand.Freq, cand.TimeSecs)
	refinedXdt := float64(ibest) / float64(coFS2)
	return extractLLRCoherentAll(sc, sc.bb, ibest), refinedXdt
}
