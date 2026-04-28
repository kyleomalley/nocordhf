package ft8

// subtract.go — time-domain subtraction of successfully-decoded FT8 signals
// from the slot's raw audio, enabling a second decode pass that uncovers
// signals that were adjacent-channel-masked in the first pass.
//
// Mirrors reference design subtractft8.f90. The subtraction is NOT a fixed-amplitude
// regeneration — the received signal's amplitude varies over the 12.64 s
// transmission due to QSB/Doppler, and a fixed amplitude would leave visible
// residue at the edges. Instead we estimate the *time-varying* complex
// amplitude by mixing the measured signal against a unit-magnitude complex
// reference and low-pass filtering the result:
//
//   cref(t)   = exp(j·[2π·f0·t + φ(t)])   — unit-magnitude GFSK reference
//   camp(t)   = dd(t) · conj(cref(t))     — mix to DC; carrier removed
//   cfilt(t)  = LPF[camp(t)]              — slowly-varying complex amplitude
//   dd(t)    -= 2·Re[cref(t) · cfilt(t)]  — subtract the reconstructed signal
//
// The LPF (NFILT=4000 cos² window at 12 kHz ≈ 333 ms time constant) is wide
// enough to track fade variation but narrow enough not to remove adjacent
// signal energy from the mixed-down result.

import (
	"math"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	subNFilt   = 4000                 // LPF length (taps at 12 kHz) — matches reference design NFILT
	subNSym    = 79                   // FT8 total symbols
	subSymSamp = 1920                 // samples per symbol at 12 kHz = SamplesPerSym
	subNFrame  = subNSym * subSymSamp // signal length in samples = 151680 ≈ 12.64 s
)

// genFT8WaveRef generates a unit-magnitude complex reference waveform for one
// FT8 transmission at 12 kHz sample rate. Produces exactly `subNFrame` samples
// (the 12.64 s signal body, no slot padding). Uses the same GFSK pulse-shaping
// as the encoder's synthesise() so tone transitions match the real signal's
// phase trajectory.
//
// f0Hz is the tone-0 (carrier) frequency in Hz.
func genFT8WaveRef(tones [NumSymbols]int, f0Hz float64) []complex128 {
	const (
		bt   = 2.0 // GFSK bandwidth-time product (matches reference design)
		hmod = 1.0 // modulation index
	)
	twopi := 2.0 * math.Pi
	nsps := subSymSamp
	nsym := NumSymbols

	// Gaussian pulse, 3 symbols wide.
	pulseLen := 3 * nsps
	pulse := make([]float64, pulseLen)
	for i := 0; i < pulseLen; i++ {
		t := (float64(i) - 1.5*float64(nsps)) / float64(nsps)
		pulse[i] = gfskPulse(bt, t)
	}

	// dphi array, (nsym+2)*nsps samples (two dummy symbols at edges).
	dphiLen := (nsym + 2) * nsps
	dphi := make([]float64, dphiLen)
	dphiPeak := twopi * hmod / float64(nsps)
	for j := 0; j < nsym; j++ {
		ib := j * nsps
		for i := 0; i < pulseLen; i++ {
			dphi[ib+i] += dphiPeak * pulse[i] * float64(tones[j])
		}
	}
	// Edge dummy-symbol extensions (stabilise phase at signal boundaries).
	for i := nsps; i < pulseLen; i++ {
		dphi[i-nsps] += dphiPeak * float64(tones[0]) * pulse[i]
	}
	for i := 0; i < 2*nsps; i++ {
		idx := nsym*nsps + i
		if idx < dphiLen {
			dphi[idx] += dphiPeak * float64(tones[nsym-1]) * pulse[i]
		}
	}

	// Carrier offset.
	dt := 1.0 / float64(sampleRate)
	f0dphi := twopi * f0Hz * dt
	for i := range dphi {
		dphi[i] += f0dphi
	}

	// Integrate phase, emit exp(j·phi). Skip first dummy symbol.
	out := make([]complex128, subNFrame)
	phi := 0.0
	for j := 0; j < nsym*nsps && j < subNFrame; j++ {
		out[j] = complex(math.Cos(phi), math.Sin(phi))
		phi = math.Mod(phi+dphi[nsps+j], twopi)
	}
	return out
}

// subtractScratch holds reusable buffers for the subtraction LPF (FFT-based
// convolution). The LPF window is pre-computed once in the frequency domain.
// One scratch per goroutine — fourier plans aren't goroutine-safe.
type subtractScratch struct {
	fft  *fourier.CmplxFFT // size = nfft (= len(samples)), for subtraction
	nfft int
	// cw is the LPF impulse response, FFT'd, scaled by 1/nfft. Computed on
	// first use (depends on nfft == len(samples)).
	cw []complex128
	// endCorrection[j] = 1 / (1 − sum(window[j-1:NFILT/2]) / sumw) at j=1..NFILT/2+1.
	// Compensates for the LPF output's edge droop (less of the window overlaps
	// the signal near the boundaries), matching subtractft8.f90:39-41.
	endCorrection [subNFilt/2 + 1]float64
	// reusable complex buffer of length nfft.
	scratch []complex128
}

func newSubtractScratch(nfft int) *subtractScratch {
	s := &subtractScratch{
		fft:     fourier.NewCmplxFFT(nfft),
		nfft:    nfft,
		scratch: make([]complex128, nfft),
	}
	// Build LPF: cos² window of width NFILT+1 samples, normalized to unit sum,
	// circularly shifted so the window is centered at sample 0 (not NFILT/2).
	cw := make([]complex128, nfft)
	window := make([]float64, subNFilt+1)
	sumw := 0.0
	for j := 0; j <= subNFilt; j++ {
		w := math.Cos(math.Pi * float64(j-subNFilt/2) / float64(subNFilt))
		w = w * w
		window[j] = w
		sumw += w
	}
	for j := 0; j <= subNFilt; j++ {
		cw[j] = complex(window[j]/sumw, 0)
	}
	rotateLeftComplex(cw, subNFilt/2+1)
	s.fft.Coefficients(cw, cw)
	// Scale by 1/nfft so the round-trip FFT→IFFT convolution is unit gain.
	fac := 1.0 / float64(nfft)
	for i := range cw {
		cw[i] *= complex(fac, 0)
	}
	s.cw = cw
	// Pre-compute end-correction taps.
	for j := 1; j <= subNFilt/2+1; j++ {
		partial := 0.0
		for k := j - 1; k <= subNFilt/2; k++ {
			partial += window[k]
		}
		denom := 1.0 - partial/sumw
		if denom > 1e-6 {
			s.endCorrection[j-1] = 1.0 / denom
		} else {
			s.endCorrection[j-1] = 1.0
		}
	}
	return s
}

// subtractFT8 subtracts one decoded signal's reconstructed waveform from the
// slot audio. Modifies dd in place. tones are the 79 GFSK tone indices, f0Hz
// is the tone-0 carrier, and xdtSecs is the signal's start time relative to
// the slot boundary (can be negative for signals that started before the slot
// started sampling).
func subtractFT8(dd []float32, sc *subtractScratch, tones [NumSymbols]int, f0Hz, xdtSecs float64) {
	cref := genFT8WaveRef(tones, f0Hz)
	nStart := int(math.Round(xdtSecs * float64(sampleRate)))
	nfft := sc.nfft

	// camp(t) = dd(t) * conj(cref(t)), zero outside the signal window. This
	// mixes the signal down to DC and will contain:
	//  - a slowly-varying complex amplitude (our target)
	//  - higher-frequency residual from adjacent signals (LPF will remove)
	camp := sc.scratch
	for i := range camp {
		camp[i] = 0
	}
	for i := 0; i < subNFrame; i++ {
		j := nStart + i
		if j >= 0 && j < nfft {
			r := real(cref[i])
			im := imag(cref[i])
			// dd(j) * conj(cref(i)) = dd(j) * (r - j·im)
			camp[i] = complex(float64(dd[j])*r, -float64(dd[j])*im)
		}
	}

	// FFT-convolve camp with cw (LPF impulse). The scale 1/nfft was baked into
	// cw at init, so the round-trip FFT·IFFT produces a properly-normalized
	// convolution.
	sc.fft.Coefficients(camp, camp)
	for i := range camp {
		camp[i] *= sc.cw[i]
	}
	sc.fft.Sequence(camp, camp)

	// End-correction: the LPF window loses mass at the edges of the signal
	// support (less of it overlaps the non-zero input), so cfilt droops near
	// the signal start/end. The correction factor ramps the estimate back up
	// so the subtraction cleanly zeros out the first/last few hundred ms.
	for j := 1; j <= subNFilt/2+1; j++ {
		if j-1 < len(camp) {
			camp[j-1] *= complex(sc.endCorrection[j-1], 0)
		}
		mirror := subNFrame - j + 1
		if mirror >= 0 && mirror < len(camp) {
			camp[mirror] *= complex(sc.endCorrection[j-1], 0)
		}
	}

	// Subtract 2·Re[cref(i) · cfilt(i)] from dd at each signal-window sample.
	for i := 0; i < subNFrame; i++ {
		j := nStart + i
		if j < 0 || j >= nfft {
			continue
		}
		z := cref[i] * camp[i]
		dd[j] -= 2.0 * float32(real(z))
	}
}
