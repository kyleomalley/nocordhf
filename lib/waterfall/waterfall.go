// Package waterfall computes a continuous STFT spectrogram from a raw audio
// stream and emits pixel rows for display — independent of FT8 decoding.
//
// Parameters:
//
//	FFT size  : 4096 samples  @ 12 kHz → ~341 ms window, 2.93 Hz resolution
//	Stride    : 512 samples              → ~42 ms between rows (≈ 24 rows/sec)
//	Window    : Hann
//	Freq range: 0–3600 Hz → 1228 output bins
//
// Post-FFT smoothing (in process()): 5-tap Gaussian blur across bins +
// α=0.5 EMA across rows + 50 dB dynamic range. This produces the smooth
// continuous FT8 traces you'd see on a hardware scope rather than raw
// speckled FFT rows.
package waterfall

import (
	"image/color"
	"math"
	"sort"
	"time"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	FFTSize    = 4096
	Stride     = 512 // samples between successive FFTs (~87.5% overlap)
	SampleRate = 12000

	FreqMin = 0.0    // Hz
	FreqMax = 3600.0 // Hz

	// Number of output frequency bins.
	// FFT bin resolution = SampleRate / FFTSize = 2.93 Hz/bin — finer than
	// FT8's 6.25 Hz tone spacing so individual tones don't straddle bins.
	// Bins covering 0–3600 Hz = 3600 / 2.93 ≈ 1228 bins.
	NumBins = 1228
)

// Row is one horizontal line of the waterfall — one STFT frame.
type Row struct {
	Time   time.Time    // UTC timestamp of this row
	Power  []float32    // NumBins magnitude values (linear, not dB)
	Pixels []color.RGBA // NumBins pre-coloured pixels ready to blit
}

// Processor consumes a continuous stream of float32 samples and emits Rows.
// Feed it samples via Write(); read rows from Rows().
type Processor struct {
	buf        []float32
	fft        *fourier.FFT
	window     []float64
	rows       chan Row
	noiseFloor float64   // running estimate, for AGC normalisation
	scratch    []float32 // reusable sort buffer — avoids per-frame allocation
	blurred    []float32 // scratch buffer for bin-axis Gaussian blur
	prevPower  []float32 // previous frame's smoothed power, for temporal EMA
	haveEMA    bool      // false until first row seeds prevPower
}

// New creates a Processor. rows channel has capacity cap (suggest 64).
func New(cap int) *Processor {
	return &Processor{
		buf:       make([]float32, 0, FFTSize*2),
		fft:       fourier.NewFFT(FFTSize),
		window:    hannWindow(FFTSize),
		rows:      make(chan Row, cap),
		scratch:   make([]float32, NumBins),
		blurred:   make([]float32, NumBins),
		prevPower: make([]float32, NumBins),
	}
}

// Write appends samples and emits a Row for every Stride samples accumulated.
// Non-blocking — if the rows channel is full, oldest data is discarded.
func (p *Processor) Write(samples []float32, now time.Time) {
	p.buf = append(p.buf, samples...)

	for len(p.buf) >= FFTSize {
		row := p.process(p.buf[:FFTSize], now)
		select {
		case p.rows <- row:
		default:
			// consumer is behind — drop
		}
		p.buf = p.buf[Stride:]
	}
}

// Rows returns the channel on which completed rows are delivered.
func (p *Processor) Rows() <-chan Row {
	return p.rows
}

func (p *Processor) process(samples []float32, t time.Time) Row {
	// Apply Hann window and convert to float64
	in := make([]float64, FFTSize)
	for i, s := range samples {
		in[i] = float64(s) * p.window[i]
	}

	out := p.fft.Coefficients(nil, in)

	// Compute magnitudes for 0–FreqMax Hz
	power := make([]float32, NumBins)
	for i := 0; i < NumBins; i++ {
		re := real(out[i])
		im := imag(out[i])
		power[i] = float32(math.Sqrt(re*re+im*im)) / float32(FFTSize)
	}

	// Bin-axis Gaussian blur (σ≈1, 5-tap kernel). Smooths the per-bin noise
	// wobble that otherwise renders as vertical grain in the waterfall. Edges
	// are clamped; the kernel is symmetric so the DC end of the spectrum is
	// slightly biased toward itself, which is fine for a visual display.
	const k0, k1, k2 = 0.06, 0.24, 0.40
	for i := 0; i < NumBins; i++ {
		im2 := i - 2
		im1 := i - 1
		ip1 := i + 1
		ip2 := i + 2
		if im2 < 0 {
			im2 = 0
		}
		if im1 < 0 {
			im1 = 0
		}
		if ip1 >= NumBins {
			ip1 = NumBins - 1
		}
		if ip2 >= NumBins {
			ip2 = NumBins - 1
		}
		p.blurred[i] = k0*power[im2] + k1*power[im1] + k2*power[i] + k1*power[ip1] + k0*power[ip2]
	}
	copy(power, p.blurred)

	// Temporal EMA across rows (α = 0.5). A 1-row half-life kills the
	// random-noise flicker without smearing short FT8 callsigns (which last
	// many rows). First row seeds prevPower and skips the mix.
	if p.haveEMA {
		for i := range power {
			power[i] = 0.5*power[i] + 0.5*p.prevPower[i]
		}
	} else {
		p.haveEMA = true
	}
	copy(p.prevPower, power)

	// Estimate noise floor using the 20th-percentile bin magnitude.
	// Using the mean would be inflated by strong signals on a busy band,
	// pushing weak signals below the threshold and rendering them black.
	// The 20th percentile is robust: up to 80% of bins can contain signals
	// before the estimate is affected.
	copy(p.scratch, power)
	sort.Slice(p.scratch, func(i, j int) bool { return p.scratch[i] < p.scratch[j] })
	percentile20 := float64(p.scratch[NumBins/5])

	if p.noiseFloor == 0 {
		p.noiseFloor = percentile20
	} else {
		// Exponential moving average: τ ≈ 100 rows (~4 s at 24 rows/s)
		p.noiseFloor = 0.99*p.noiseFloor + 0.01*percentile20
	}

	// Colour the row.
	// power[i] is FFT magnitude (amplitude), so use 20*log10 for correct dB scaling.
	// Scale: 0 dB = noise floor, 50 dB amplitude above floor = full brightness.
	// Wider range than 30 dB gives the gentle fade-to-black edges of a hardware
	// scope, instead of a hard cliff that makes signals look punched-out.
	pixels := make([]color.RGBA, NumBins)
	ref := p.noiseFloor
	if ref < 1e-9 {
		ref = 1e-9
	}
	for i, v := range power {
		db := 20 * math.Log10(float64(v)/ref)
		t := db / 50.0 // 0..50 dB amplitude → 0..1
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		pixels[i] = heatmap(t)
	}

	return Row{Time: t, Power: power, Pixels: pixels}
}

// heatmap maps 0..1 to a black→blue→cyan→green→yellow→red palette.
func heatmap(t float64) color.RGBA {
	type stop struct {
		pos     float64
		r, g, b uint8
	}
	stops := []stop{
		{0.00, 0, 0, 0},
		{0.15, 0, 0, 180},
		{0.35, 0, 180, 220},
		{0.55, 0, 220, 0},
		{0.75, 230, 230, 0},
		{1.00, 255, 40, 0},
	}
	for i := 1; i < len(stops); i++ {
		if t <= stops[i].pos {
			s, e := stops[i-1], stops[i]
			f := (t - s.pos) / (e.pos - s.pos)
			lerp := func(a, b uint8) uint8 {
				return uint8(float64(a) + f*float64(int(b)-int(a)))
			}
			return color.RGBA{lerp(s.r, e.r), lerp(s.g, e.g), lerp(s.b, e.b), 255}
		}
	}
	return color.RGBA{255, 40, 0, 255}
}

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}
