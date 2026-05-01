// nocordhf is a Discord-style chat-focused FT8 client. Presents the
// receive stream as an IRC/Discord-style chat pane and confines TX to
// two well-formed primitives — bare CQ or a directed call to a heard
// station.
//
// Build:    go build ./cmd/nocordhf
// Run:      ./nocordhf -call YOURCALL -grid AA12 -audio "USB Audio CODEC"
//
// SPDX-License-Identifier: GPL-3.0-or-later
//
// NocordHF is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License v3.0 as published
// by the Free Software Foundation. See the LICENSE file at the repo
// root for the full text.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fyne.io/fyne/v2/app"
	"go.uber.org/zap"

	"github.com/kyleomalley/nocordhf/internal/nocord"
	"github.com/kyleomalley/nocordhf/lib/audio"
	"github.com/kyleomalley/nocordhf/lib/cat"
	"github.com/kyleomalley/nocordhf/lib/ft8"
	"github.com/kyleomalley/nocordhf/lib/logging"
	"github.com/kyleomalley/nocordhf/lib/pskreporter"
	"github.com/kyleomalley/nocordhf/lib/waterfall"
)

// BuildID is injected at build time via `-ldflags "-X main.BuildID=..."` from
// the Makefile. Falls back to short-git-hash + UTC timestamp on plain
// `go run` so each rebuild is uniquely identifiable in nocordhf.log.
var BuildID = "dev"

func init() {
	if BuildID == "dev" {
		hash := "nongit"
		if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
			hash = strings.TrimSpace(string(out))
		}
		BuildID = hash + "-" + time.Now().UTC().Format("20060102150405")
	}
}

func main() {
	myCall := flag.String("call", os.Getenv("NOCORDHF_CALL"), "operator callsign (default $NOCORDHF_CALL)")
	myGrid := flag.String("grid", os.Getenv("NOCORDHF_GRID"), "operator grid square (default $NOCORDHF_GRID)")
	// Default to a permissive "USB Audio" substring rather than the
	// IC-7300-specific "USB Audio CODEC" — different macOS versions
	// surface the same physical interface as either "USB Audio CODEC"
	// or "USB Audio Device" (and similar variants for Yaesu rigs), and
	// "USB Audio" matches both. Pass an exact -audio value to override.
	audioDev := flag.String("audio", "USB Audio", "capture device name (case-insensitive substring)")
	playbackDev := flag.String("audio-out", "USB Audio", "playback device name (TX audio to the rig)")
	noCAT := flag.Bool("no-cat", false, "skip radio control — RX-only, no PTT/tune")
	listAudio := flag.Bool("list-audio", false, "list audio capture devices and exit")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	if err := logging.InitFile(*debug, BuildID, "nocordhf.log"); err != nil {
		fmt.Fprintf(os.Stderr, "logging init: %v\n", err)
		os.Exit(1)
	}
	// Tee panics / runtime stack dumps into a file so a crash isn't lost
	// when the process exited from a terminal that's already gone. The
	// Go runtime writes panic stacks to FD 2 directly (bypassing zap), so
	// we dup-redirect the underlying file descriptor.
	if errFile, err := os.OpenFile("nocordhf-stderr.log",
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_ = redirectStderr(errFile)
	}
	log := logging.L

	if *listAudio {
		if err := audio.ListDevices(); err != nil {
			log.Fatalw("list devices failed", "err", err)
		}
		return
	}

	log.Infow("nocordhf starting", "build", BuildID, "audio", *audioDev, "call", *myCall, "grid", *myGrid)

	// Live decoder wall-clock budget; gives the audio capture goroutine
	// CPU headroom during decode.
	ft8.SetDecodeBudget(ft8.LiveDecodeWallBudget)

	// Channels into the GUI. txCh delivers TX requests; tuneCh delivers
	// frequency-change requests. main owns the channels (closes on exit).
	txCh := make(chan nocord.TxRequest, 4)
	tuneCh := make(chan uint64, 1)

	fyneApp := app.NewWithID("com.nocordhf.app")
	g := nocord.NewGUI(fyneApp, BuildID, txCh, tuneCh)
	// CLI flags / env vars win over the persisted profile, but if both
	// are empty fall back to whatever was last saved via the settings
	// dialog so a clean `./nocordhf` still launches with the operator's
	// callsign and grid populated.
	resolvedCall, resolvedGrid := *myCall, *myGrid
	if resolvedCall == "" || resolvedGrid == "" {
		savedCall, savedGrid := g.LoadSavedProfile()
		if resolvedCall == "" {
			resolvedCall = savedCall
		}
		if resolvedGrid == "" {
			resolvedGrid = savedGrid
		}
	}
	g.SetProfile(resolvedCall, resolvedGrid)
	g.ApplySavedToggles()

	// PSKReporter activity counts — fetches per-band station counts from
	// pskreporter.info and renders them as "#20m (123)" in the channel
	// list. Refreshed every 5 minutes; cached on disk to survive restarts.
	cacheRoot, _ := os.UserCacheDir()
	pskrCache := ""
	if cacheRoot != "" {
		pskrCache = filepath.Join(cacheRoot, "nocordhf", "pskreporter")
	}
	pskrClient := pskreporter.New(*myCall, pskrCache)
	pskrBands := make([]pskreporter.BandSpec, 0, len(nocord.DefaultBands))
	for _, b := range nocord.DefaultBands {
		// FT8 USB dial freq → ±1500 Hz audio passband ≈ ±1.5 kHz of RF.
		// Wider window (~3 kHz on each side) captures stations slightly
		// off the canonical dial, which pskreporter sees a lot of.
		pskrBands = append(pskrBands, pskreporter.BandSpec{
			Name:    b.Name,
			LowerHz: b.Hz - 3_000,
			UpperHz: b.Hz + 3_000,
		})
	}
	g.SetBandActivity(func(band string) int {
		s, ok := pskrClient.Stats(band)
		if !ok {
			return 0
		}
		return s.Reports
	})
	go func() {
		// Initial refresh kicks immediately so the operator sees counts
		// within ~5s of launch; thereafter every 5 min.
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := pskrClient.Refresh(ctx, pskrBands)
			cancel()
			if err != nil {
				log.Warnw("pskreporter refresh failed", "err", err)
			} else {
				g.RefreshBandList()
			}
			<-ticker.C
		}
	}()

	// Radio (CAT) — hot-swappable via AtomicRadio so a disconnected radio
	// doesn't crash the app. Resolution order:
	//   1. -no-cat flag → start with no radio.
	//   2. Saved profile (Settings → Radio) → open exactly what the user
	//      configured; failure logs and falls through to "no radio" so a
	//      missing rig at launch is never fatal.
	//   3. Otherwise auto-detect.
	radio := cat.NewAtomicRadio(nil)
	defer func() {
		log.Infow("shutdown: closing radio CAT")
		radio.Close()
	}()
	g.SetRadio(radio)
	switch {
	case *noCAT:
		log.Infow("CAT disabled by -no-cat flag")
	default:
		if rType, rPort, rBaud, ok := g.LoadSavedRadio(); ok {
			log.Infow("opening saved radio profile", "type", rType, "port", rPort, "baud", rBaud)
			if r, err := cat.OpenByType(rType, rPort, rBaud); err != nil {
				log.Warnw("saved radio profile failed to open; running without radio control", "err", err)
				g.AppendSystem(fmt.Sprintf("⚠ no radio: %s on %s — %v", rType, rPort, err))
			} else {
				radio.Swap(r)
				log.Infow("CAT connected from saved profile", "type", rType, "port", rPort)
				g.AppendSystem(fmt.Sprintf("ⓘ radio: %s on %s", rType, rPort))
			}
		} else {
			log.Infow("auto-detecting radio")
			if res, err := cat.AutoDetect(); err != nil {
				log.Warnw("CAT auto-detect failed; running without radio control", "err", err)
				g.AppendSystem("⚠ no radio: " + err.Error())
			} else {
				radio.Swap(res.Radio)
				log.Infow("CAT connected", "radio", res.Name, "port", res.Port)
				g.AppendSystem(fmt.Sprintf("ⓘ radio: %s on %s", res.Name, res.Port))
			}
		}
	}

	// Audio capture. Frames flow into a single goroutine that runs Decode()
	// and pushes results into the chat. The capturer also fans samples out
	// to the waterfall processor via SetSink, decoupled from the slot-frame
	// queue so the scope updates continuously rather than once per slot.
	capturer := audio.New(*audioDev)
	wfProc := waterfall.New(128)
	capturer.SetSink(func(samples []float32, now time.Time) {
		wfProc.Write(samples, now)
	})
	go func() {
		for row := range wfProc.Rows() {
			g.SetWaterfallRow(row)
		}
	}()
	// Audio failure is NOT fatal: we still want the GUI to start so
	// the operator can pick a different device from the chat (or
	// `-list-audio` to see what's available) instead of the binary
	// exiting with no UI to recover from. If Start() succeeds, the
	// frame loop below begins decoding; otherwise we surface the
	// error in chat and the app runs as a no-RX shell.
	audioOK := false
	if err := capturer.Start(); err != nil {
		log.Errorw("audio start failed", "err", err)
		g.AppendSystem(fmt.Sprintf("audio device not available: %v -run with -list-audio to see options", err))
	} else {
		audioOK = true
		defer capturer.Stop()
	}

	// In -debug mode, persist every captured RX slot as a WAV so a missed
	// decode can be replayed through WSJT-X (or jt9) for comparison.
	var rxRecorder *audio.FrameRecorder
	if audioOK && *debug {
		freqFn := func() uint64 {
			if r := radio.Inner(); r != nil {
				return radio.Frequency()
			}
			return 0
		}
		if rec, err := audio.NewFrameRecorder("recordings", BuildID, freqFn); err != nil {
			log.Warnw("rx recorder init failed", "err", err)
		} else {
			rxRecorder = rec
			log.Infow("rx frame recording enabled", "dir", "recordings")
		}
	}

	if audioOK {
		go func() {
			for frame := range capturer.Frames() {
				if rxRecorder != nil {
					if path, err := rxRecorder.Save(frame); err != nil {
						log.Warnw("rx frame save failed", "err", err)
					} else {
						log.Debugw("rx frame saved", "path", path, "slot", frame.SlotStart.Format("15:04:05"))
					}
				}
				if *myCall != "" {
					ft8.SetAPContext(*myCall, "")
				}
				start := time.Now()
				onDecode := func(d ft8.Decoded) { g.AppendDecode(d) }
				results := ft8.Decode(frame.Samples, frame.SlotStart, onDecode)
				elapsed := time.Since(start)
				log.Infow("decode complete",
					"slot", frame.SlotStart.Format("15:04:05"),
					"decoded", len(results),
					"elapsed_ms", elapsed.Milliseconds(),
				)
				// Plot the slot's decoded stations on the map. The MapWidget
				// internally dedups across slots and prunes pins older than
				// its own retention window, so we can hand it the full batch
				// every slot without growing the spot table unbounded.
				g.AddSpots(results)
			}
		}()
	}

	// Band-tune loop — coalesces back-to-back tune requests so a quick
	// channel-double-click doesn't cause two CAT writes.
	go func() {
		for hz := range tuneCh {
			// Drain any further requests that arrived while we were busy
			// and use the latest.
		drain:
			for {
				select {
				case newer := <-tuneCh:
					hz = newer
				default:
					break drain
				}
			}
			if radio.Inner() == nil {
				log.Debugw("tune ignored: no radio", "hz", hz)
				continue
			}
			if err := radio.SetFrequency(hz); err != nil {
				log.Warnw("tune failed", "hz", hz, "err", err)
				g.AppendSystem(fmt.Sprintf("⚠ tune to %.3f MHz failed: %v", float64(hz)/1e6, err))
			} else {
				log.Infow("tuned", "hz", hz)
			}
		}
	}()

	// TX loop: encode → wait for slot boundary → mute capturer → PTT on →
	// settle → play samples → PTT off → unmute. Nocord is a chat client; we
	// either CQ or send a single directed call, no signal-report ladder.
	player := audio.NewPlayer(*playbackDev)
	go func() {
		for req := range txCh {
			runTX(g, log, radio, capturer, player, req)
		}
	}()

	g.Run()
}

// runTX executes one TX request to completion. Factored so the main
// goroutine isn't a wall of nested select/defer blocks.
func runTX(
	g *nocord.GUI,
	log *zap.SugaredLogger,
	radio *cat.AtomicRadio,
	capturer *audio.Capturer,
	player *audio.Player,
	req nocord.TxRequest,
) {
	// TX audio amplitude. Read live from the GUI so the operator can
	// adjust it on the fly via Settings → Radio → TX level (the
	// slider drives prefs; we pick up the current value at TX time).
	// 0.18 is the conservative default if no preference is set yet.
	txLevel := g.TxLevel()
	txOffsetHz := g.TxFreq()

	// Flip Send → Stop for the duration of this request. Covers the slot
	// countdown too, so the operator can cancel before keying as well as
	// during playback. Cleared via defer regardless of which exit path runs.
	g.SetTxState(true, req.StopCh)
	defer g.SetTxState(false, nil)

	var (
		samples    []float32
		err        error
		displayMsg string
	)
	switch {
	case req.Tune:
		// Pure-carrier tune transmission. Generate ~3 seconds of a
		// single sine wave at the operator-selected audio offset
		// (default 1500 Hz) at the standard FT8 TX level. Skipping
		// the FT8 modulator means tuners see a clean unmodulated
		// carrier rather than a frequency-hopping signal.
		const tuneSeconds = 3.0
		samples = generateTuneTone(txOffsetHz, txLevel, tuneSeconds)
		displayMsg = fmt.Sprintf("TUNE %.0f Hz (%.0fs)", txOffsetHz, tuneSeconds)
	case req.RemoteCall != "":
		displayMsg = fmt.Sprintf("%s %s %s", req.RemoteCall, req.Callsign, req.Grid)
		samples, err = ft8.EncodeStandard(displayMsg, txLevel, txOffsetHz)
	default:
		displayMsg = fmt.Sprintf("CQ %s %s", req.Callsign, req.Grid)
		samples, err = ft8.EncodeCQ(req.Callsign, req.Grid, txLevel, txOffsetHz)
	}
	if err != nil {
		log.Warnw("TX encode failed", "err", err)
		g.AppendSystem("⚠ encode error: " + err.Error())
		return
	}

	// Slot-boundary countdown. FT8 transmissions must start within ~2s of
	// the 15-second UTC boundary; if we missed this slot, wait for the
	// next one. Stop button (req.StopCh) cancels.
	//
	// Tune transmissions skip this — they're not slot-aligned signals
	// and the operator usually wants the carrier on the air NOW so
	// they can finish tuning before missing another slot.
	const lateTxMaxRem = 2
	for !req.Tune {
		now := time.Now().UTC()
		rem := now.Unix() % 15
		if rem <= lateTxMaxRem {
			break
		}
		toNext := 15 - rem
		// Only emit a countdown chat line every 5 seconds so the chat
		// doesn't fill with "TX in 14s" "TX in 13s" "TX in 12s" rows.
		if toNext%5 == 0 {
			g.AppendSystem(fmt.Sprintf("TX in %ds: %s", toNext, displayMsg))
		}
		nextSec := now.Truncate(time.Second).Add(time.Second)
		select {
		case <-req.StopCh:
			log.Infow("TX cancelled before keying")
			g.AppendSystem("✕ TX cancelled")
			return
		case <-time.After(time.Until(nextSec)):
		}
	}

	// Slot-end stop channel. Combined with req.StopCh so either a natural
	// slot end or a user Stop kills audio output and drops PTT cleanly.
	slotEnd := time.Now().UTC().Truncate(15 * time.Second).Add(15 * time.Second)
	slotStopCh := make(chan struct{})
	go func() {
		time.Sleep(time.Until(slotEnd))
		close(slotStopCh)
	}()
	combinedStop := make(chan struct{})
	go func() {
		select {
		case <-slotStopCh:
		case <-req.StopCh:
			log.Infow("TX hard-stopped by user")
		}
		close(combinedStop)
	}()

	g.AppendSystem("● TX " + displayMsg)
	log.Infow("TX start", "msg", displayMsg)
	capturer.Mute()
	defer capturer.Unmute()

	// PTT on with 200 ms RX→TX settle delay before audio flows. Skipping
	// the settle radiates a broadband transient through a still-settling
	// chain. 200 ms matches reference design default and covers cold-key worst case.
	if radio.Inner() != nil {
		if err := radio.PTTOn(); err != nil {
			log.Warnw("PTT on failed", "err", err)
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-combinedStop:
		}
	}

	if err := player.Play(samples, combinedStop); err != nil {
		log.Warnw("TX play failed", "err", err)
		if radio.Inner() != nil {
			pttOffWithRetry(radio, log)
		}
		g.AppendSystem("⚠ play error: " + err.Error())
		return
	}

	if radio.Inner() != nil {
		pttOffWithRetry(radio, log)
	}

	// Peak readout — lets the operator dial in TxLevel + macOS output +
	// USB MOD LEVEL.
	peak := math.Float64frombits(player.LastPeak.Load())
	peakDBFS := -120.0
	if peak > 0 {
		peakDBFS = 20 * math.Log10(peak)
	}
	peakNote := fmt.Sprintf("peak %.2f (%.1f dBFS)", peak, peakDBFS)
	if peak >= 0.95 {
		peakNote = "⚠ " + peakNote + " — clipping likely"
	}
	log.Infow("TX done", "msg", displayMsg, "peak", peak, "peak_dbfs", peakDBFS)
	g.AppendTxEcho(displayMsg)
	g.AppendSystem("✓ TX done · " + peakNote)
}

// pttOffWithRetry keeps attempting PTTOff until it succeeds or 10s elapses.
// Critical: the radio must not be left transmitting if a single CAT command
// was dropped or timed out.
func pttOffWithRetry(radio cat.Radio, log *zap.SugaredLogger) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := radio.PTTOff()
		if err == nil {
			return
		}
		log.Warnw("PTT off failed; retrying", "err", err)
		if time.Now().After(deadline) {
			log.Errorw("PTT off retries exhausted — radio may still be transmitting", "err", err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// generateTuneTone produces a pure sine wave at audioFreqHz for the
// given duration, suitable for keying an antenna tuner. Output is
// 48 kHz mono float32 matching ft8.EncodeStandard's sample format so
// the same audio.Player path can play it. Includes short raised-
// cosine ramps at the head and tail to avoid keying clicks.
func generateTuneTone(audioFreqHz, level, seconds float64) []float32 {
	const sr = 48000
	n := int(seconds * float64(sr))
	out := make([]float32, n)
	twoPi := 2.0 * math.Pi
	dphi := twoPi * audioFreqHz / float64(sr)
	rampN := int(0.02 * float64(sr)) // 20 ms ramps
	if rampN < 1 {
		rampN = 1
	}
	phi := 0.0
	for i := 0; i < n; i++ {
		amp := level
		switch {
		case i < rampN:
			amp *= 0.5 * (1 - math.Cos(math.Pi*float64(i)/float64(rampN)))
		case i >= n-rampN:
			amp *= 0.5 * (1 - math.Cos(math.Pi*float64(n-1-i)/float64(rampN)))
		}
		out[i] = float32(amp * math.Sin(phi))
		phi += dphi
		if phi > twoPi {
			phi -= twoPi
		}
	}
	return out
}

// redirectStderr points FD 2 (and the Go runtime's panic destination) at
// the given file. The Go runtime writes panic stacks to FD 2 directly via
// syscall.Write, bypassing os.Stderr, so we have to dup at the FD level
// rather than just reassigning os.Stderr. Best-effort — failure means we
// just don't capture panics, no impact on normal execution.
func redirectStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
