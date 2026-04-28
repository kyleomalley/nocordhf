package ft8

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// decimate4 drops fs from 48 kHz to 12 kHz by taking every 4th sample.
// The FT8 waveform is narrowband (~50 Hz around 1500 Hz), so aliasing from
// out-of-band signal energy is negligible. Noise is added AFTER decimation,
// so the 12 kHz noise is clean and white by construction.
func decimate4(samples []float32) []float32 {
	out := make([]float32, len(samples)/4)
	for i := range out {
		out[i] = samples[i*4]
	}
	return out
}

// addAWGN adds white Gaussian noise to samples at the target SNR, where SNR
// is expressed in reference design convention: signal power over noise power in a
// 2500 Hz reference bandwidth.
//
// With per-sample variance σ² at fs=12000, total noise power spreads uniformly
// over 0..fs/2 = 6000 Hz. Power in a 2500 Hz window is σ² * 2500 / 6000.
// Solving for σ² given Ps and target linear SNR:
//
//	σ² = Ps * 6000 / (2500 * linearSNR) = Ps * 2.4 / linearSNR
func addAWGN(samples []float32, signalPower, snrDB float64, rng *rand.Rand) []float32 {
	linearSNR := math.Pow(10, snrDB/10)
	sigma := math.Sqrt(signalPower * 2.4 / linearSNR)
	out := make([]float32, len(samples))
	for i, s := range samples {
		out[i] = s + float32(rng.NormFloat64()*sigma)
	}
	return out
}

// TestDecodeSensitivityCliff measures decode success rate vs synthetic SNR.
// Establishes the current BP-only cliff as a regression baseline for future
// decoder improvements (OSD, multipass, etc.).
//
// Skipped in short mode: run with `go test ./internal/ft8 -run Sensitivity -v`.
func TestDecodeSensitivityCliff(t *testing.T) {
	if testing.Short() {
		t.Skip("sensitivity sweep is slow; run explicitly")
	}

	RegisterCallsign("KO6IEH")

	tx48k, err := EncodeCQ("KO6IEH", "DM13", 1.0, 1500.0)
	if err != nil {
		t.Fatalf("EncodeCQ: %v", err)
	}
	tx12k := decimate4(tx48k)
	if len(tx12k) != windowSamples {
		t.Fatalf("decimated length = %d, want %d", len(tx12k), windowSamples)
	}

	// Signal power from the active modulated region. EncodeCQ starts the
	// waveform at 0.5 s into the 15 s window and the signal lasts 79 * 0.16 s
	// ≈ 12.64 s. Use 1.0 s–13.0 s to stay well inside the ramps.
	const activeStart = 1 * sampleRate
	const activeEnd = 13 * sampleRate
	var sumSq float64
	for _, s := range tx12k[activeStart:activeEnd] {
		sumSq += float64(s) * float64(s)
	}
	signalPower := sumSq / float64(activeEnd-activeStart)
	t.Logf("signal power = %.6g (rms = %.4f)", signalPower, math.Sqrt(signalPower))

	// Sweep from easy (+5 dB) down past reference design cliff (–25 dB).
	snrs := []float64{+5, 0, -5, -10, -12, -14, -16, -18, -20, -22, -24}
	const trials = 10
	const want = "CQ KO6IEH DM13"

	type row struct {
		snr  float64
		ok   int
		avgN int // candidates per trial (average)
	}
	var results []row

	for _, snr := range snrs {
		rng := rand.New(rand.NewSource(int64(snr*1000) + 42))
		ok := 0
		totalCands := 0
		var reportedSum float64
		reportedN := 0
		for trial := 0; trial < trials; trial++ {
			noisy := addAWGN(tx12k, signalPower, snr, rng)
			decoded := Decode(noisy, time.Time{}, nil)
			wf := buildWaterfall(noisy)
			totalCands += len(findCandidates(wf))
			for _, d := range decoded {
				if d.Message.Text == want {
					ok++
					reportedSum += d.SNR
					reportedN++
					break
				}
			}
		}
		results = append(results, row{snr: snr, ok: ok, avgN: totalCands / trials})
		reportedAvg := math.NaN()
		if reportedN > 0 {
			reportedAvg = reportedSum / float64(reportedN)
		}
		t.Logf("SNR_in = %+5.1f dB: %2d/%d decoded (%3.0f%%)  SNR_reported ≈ %+5.1f dB (n=%d)",
			snr, ok, trials, 100*float64(ok)/float64(trials), reportedAvg, reportedN)
	}

	// Report the cliff: highest SNR where success rate drops below 50%.
	cliff := math.Inf(-1)
	for _, r := range results {
		if float64(r.ok)/float64(trials) >= 0.5 {
			cliff = r.snr
		}
	}
	t.Logf("50%% decode cliff ≈ %.1f dB", cliff)
}
