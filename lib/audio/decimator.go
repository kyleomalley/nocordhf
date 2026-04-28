package audio

import "math"

// firTaps for 4× decimation from 48 kHz to 12 kHz. Hann-windowed sinc
// low-pass with passband ≤ 5000 Hz and stopband from ≥ 6000 Hz (Nyquist
// of the output rate). 65 taps gives ~70 dB stopband attenuation, which
// puts any out-of-band aliasing well below FT8's −24 dB decode floor.
//
// Computed once at package init (deterministic, no float-rounding drift
// from build-to-build) rather than hard-coded to keep the design intent
// readable.
const (
	firN     = 65         // taps
	firFs    = 48000.0    // input rate
	firFc    = 5000.0     // -3 dB cutoff
	decimM   = 4          // 48000 / 12000
	decimMid = (firN - 1) // last write index in history
)

var firTaps [firN]float32

func init() {
	// h[n] = 2*fc/fs * sinc(2*fc/fs * (n - (N-1)/2)) * hann(n, N)
	// Normalised so DC gain is exactly 1 (sum of taps == 1).
	mid := float64(firN-1) / 2
	cutoff := 2 * firFc / firFs
	var sum float64
	tmp := make([]float64, firN)
	for n := 0; n < firN; n++ {
		x := float64(n) - mid
		var s float64
		if x == 0 {
			s = cutoff
		} else {
			s = math.Sin(math.Pi*cutoff*x) / (math.Pi * x)
		}
		// Hann window
		w := 0.5 * (1 - math.Cos(2*math.Pi*float64(n)/float64(firN-1)))
		tmp[n] = s * w
		sum += tmp[n]
	}
	for n := 0; n < firN; n++ {
		firTaps[n] = float32(tmp[n] / sum)
	}
}

// decimator is a stateful 4× decimating FIR filter. Samples in at 48 kHz,
// samples out at 12 kHz, with anti-alias filtering. The state (history)
// must persist across callback invocations so the filter sees a continuous
// signal across audio buffer boundaries.
//
// Replaces malgo's internal resampler, which was observed to drop ~1 s of
// audio every ~30 s on macOS USB CODEC capture under decoder CPU load —
// the resampler's rate compensator deprioritises sample retention vs
// output-clock fidelity. Doing the rate conversion in user code, on the
// same thread that pulled the samples out of the device, avoids that
// failure mode entirely.
type decimator struct {
	hist  [firN]float32 // ring of last firN samples (oldest at hist[0])
	phase int           // counts 0..decimM-1; emit when (phase+1)%decimM==0
}

// processInto consumes len(in) input samples (at 48 kHz) and APPENDS
// approximately len(in)/4 filtered output samples (at 12 kHz) to dst,
// returning the extended slice. Reuses dst's underlying array when it
// has capacity. Avoids the per-callback allocation that the previous
// `process` API forced — that allocation was at 48 kHz callback rate
// (~50 Hz) and added ~24 KB/sec of garbage, increasing GC pressure
// during decode and stalling the cgo audio bridge.
func (d *decimator) processInto(dst, in []float32) []float32 {
	for _, x := range in {
		// Shift history left by one, append new sample at the end.
		// O(firN) per input sample but firN=65 and input rate=48 kHz =>
		// ~3 M ops/s, trivial vs FT8 decode cost.
		copy(d.hist[:], d.hist[1:])
		d.hist[firN-1] = x
		d.phase++
		if d.phase >= decimM {
			d.phase = 0
			var sum float32
			for i := 0; i < firN; i++ {
				sum += firTaps[i] * d.hist[i]
			}
			dst = append(dst, sum)
		}
	}
	return dst
}
