// Package audio handles capture of PCM audio from the ICOM 7300 USB audio device
// using malgo (miniaudio cgo bindings). No external library installation required.
package audio

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	SampleRate       = 12000
	NativeSampleRate = 48000 // open malgo at native rate; decimate 4× in user code
	Channels         = 1
	WindowSeconds    = 15
	WindowSamples    = SampleRate * WindowSeconds // 180,000 samples per FT8 slot
)

// Frame holds one 15-second audio window aligned to a UTC FT8 time slot.
type Frame struct {
	Samples   []float32
	SlotStart time.Time // UTC time this slot began
}

// SampleSink is an optional callback called on every raw audio buffer as it
// arrives from the device — before it is accumulated into 15-second frames.
// Used by the waterfall processor for continuous, low-latency display.
// The slice is only valid for the duration of the call; copy if needed.
type SampleSink func(samples []float32, now time.Time)

// Capturer streams audio from a named input device and emits aligned 15-second frames.
type Capturer struct {
	deviceName string
	frames     chan Frame
	stop       chan struct{}
	wg         sync.WaitGroup
	sinkMu     sync.RWMutex
	sink       SampleSink
	muted      atomic.Bool // when true, incoming samples are zeroed (TX mute)
	dropped    atomic.Int64
}

// New creates a Capturer targeting a device whose name contains deviceName (case-insensitive).
// Use "USB Audio CODEC" for the ICOM 7300.
func New(deviceName string) *Capturer {
	return &Capturer{
		deviceName: deviceName,
		frames:     make(chan Frame, 16),
		stop:       make(chan struct{}),
	}
}

// Mute silences the audio input (zeroes samples) while transmitting.
func (c *Capturer) Mute() { c.muted.Store(true) }

// Unmute resumes normal audio capture after TX.
func (c *Capturer) Unmute() { c.muted.Store(false) }

// SetSink registers a SampleSink to receive every raw audio buffer.
// Must be called before Start(). Safe to call with nil to clear.
func (c *Capturer) SetSink(s SampleSink) {
	c.sinkMu.Lock()
	c.sink = s
	c.sinkMu.Unlock()
}

// Frames returns the channel on which decoded 15-second windows are delivered.
func (c *Capturer) Frames() <-chan Frame {
	return c.frames
}

// Start begins audio capture. It blocks until the device is opened, then returns.
// Call Stop to shut down.
func (c *Capturer) Start() error {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendCoreaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("malgo init context: %w", err)
	}

	devices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		ctx.Uninit() //nolint
		return fmt.Errorf("enumerate devices: %w", err)
	}

	var targetID malgo.DeviceID
	found := false
	for _, d := range devices {
		if strings.Contains(strings.ToLower(d.Name()), strings.ToLower(c.deviceName)) {
			targetID = d.ID
			found = true
			break
		}
	}
	if !found {
		ctx.Uninit() //nolint
		return fmt.Errorf("audio device %q not found; available devices: %s", c.deviceName, deviceList(devices))
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.DeviceID = targetID.Pointer()
	cfg.Capture.Format = malgo.FormatF32
	cfg.Capture.Channels = Channels
	// Open at NATIVE 48 kHz instead of asking malgo to deliver 12 kHz directly.
	// malgo's internal resampler was observed to drop ~1 s of audio every
	// ~30 s under decoder CPU contention (every-other-slot 0-decode bug),
	// while reference design on the same hardware was unaffected — reference design also opens
	// at native rate and downsamples in app code. We replicate that here:
	// the malgo callback delivers untouched 48 kHz samples and our own
	// FIR decimator (decimator.go) does the 4× rate conversion on the
	// audio-thread side of the cgo boundary, with no resampler clock to
	// fall behind.
	cfg.SampleRate = NativeSampleRate
	cfg.Alsa.NoMMap = 1

	// dec is the stateful 48 kHz → 12 kHz decimating FIR. State persists
	// across callbacks so the filter sees a continuous input stream.
	var dec decimator

	// buf accumulates POST-DECIMATION samples (at 12 kHz) between callbacks;
	// protected by the callback goroutine only.
	buf := make([]float32, 0, WindowSamples*2)
	// Start from the current slot boundary (truncate now to 15s) so we begin
	// accumulating immediately rather than waiting up to 15s for the next boundary.
	now0 := time.Now().UTC()
	slotStart := now0.Truncate(WindowSeconds * time.Second)
	nextBoundary := slotStart.Add(WindowSeconds * time.Second)

	onRecv := func(_, input []byte, frameCount uint32) {
		// input is raw float32 LE bytes at NativeSampleRate (48 kHz)
		n := int(frameCount)
		floats := unsafe.Slice((*float32)(unsafe.Pointer(&input[0])), n)

		now := time.Now().UTC()

		// Mute is applied to the native-rate stream so the decimator's
		// state is consistent (zero in → zero out, same as before mute
		// was the input rate).
		if c.muted.Load() {
			for i := range floats {
				floats[i] = 0
			}
		}

		// Decimate 48 kHz → 12 kHz with our FIR, appending directly to buf.
		// processInto reuses buf's existing capacity (allocated to 2× slot
		// size at startup) so the audio callback runs allocation-free in
		// the steady state — eliminates GC pressure that was stalling the
		// cgo bridge during heavy decode and degrading capture every other
		// slot.
		bufStart := len(buf)
		buf = dec.processInto(buf, floats)
		newSamples := buf[bufStart:]

		// Deliver decimated samples to the waterfall sink. The waterfall is
		// built against the 12 kHz output rate, so sending native-rate
		// samples here would have its own consequences (4× too many bins
		// shown). The user can still watch their TX via the IC-7300
		// monitor — TX path is unchanged; only RX capture moved to native.
		c.sinkMu.RLock()
		s := c.sink
		c.sinkMu.RUnlock()
		if s != nil && len(newSamples) > 0 {
			s(newSamples, now)
		}

		// Emit a frame whenever we cross a slot boundary.
		// The first frame may be partial (launched mid-slot) — the decoder clips
		// to windowSamples and zero-pads, so shorter frames decode correctly.
		for now.After(nextBoundary) || now.Equal(nextBoundary) {
			// How many samples belong to the slot that just ended?
			emit := len(buf)
			origBuf := emit
			truncated := false
			if emit > WindowSamples {
				emit = WindowSamples
				truncated = true
			}
			frame := Frame{
				Samples:   make([]float32, emit),
				SlotStart: slotStart,
			}
			copy(frame.Samples, buf[:emit])
			buf = buf[emit:]
			// Diagnose "every-other-slot 0 decodes" pattern by logging buf
			// fullness and any leftover. If buf was over WindowSamples (overrun
			// from a delayed callback), the leftover bytes carry the previous
			// slot's tail audio into the next slot — which messes up sync
			// detection on the new slot. If buf was very short, the callback
			// fired before the audio device finished delivering this slot's
			// samples (USB scheduling glitch on macOS).
			if logging.L != nil {
				short := WindowSamples - emit
				leftover := len(buf)
				if short > WindowSamples/30 || leftover > 0 || truncated {
					logging.L.Infow("slot frame timing",
						"slot", slotStart.Format("15:04:05"),
						"buf_at_boundary", origBuf,
						"emit", emit,
						"short_samples", short,
						"leftover_bytes", leftover,
						"truncated", truncated,
						"now_after_boundary_ms", now.Sub(nextBoundary).Milliseconds(),
					)
				}
			}
			slotStart = nextBoundary
			nextBoundary = nextBoundary.Add(WindowSeconds * time.Second)
			select {
			case c.frames <- frame:
			default:
				// Decoder is more than 16 slots behind — drop the oldest queued
				// frame to make room rather than the new one, so the live
				// waterfall stays current. Log the drop so it's visible in
				// post-mortem analysis when "specific receive cycles" come
				// back blank.
				dropped := c.dropped.Add(1)
				select {
				case <-c.frames:
				default:
				}
				select {
				case c.frames <- frame:
				default:
				}
				if logging.L != nil {
					logging.L.Warnw("audio frame dropped: decoder backlog",
						"slot", frame.SlotStart.Format("15:04:05"),
						"dropped_total", dropped,
					)
				}
			}
		}
	}

	callbacks := malgo.DeviceCallbacks{Data: onRecv}
	device, err := malgo.InitDevice(ctx.Context, cfg, callbacks)
	if err != nil {
		ctx.Uninit() //nolint
		return fmt.Errorf("init device: %w", err)
	}

	if err := device.Start(); err != nil {
		device.Uninit()
		ctx.Uninit() //nolint
		return fmt.Errorf("start device: %w", err)
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		<-c.stop
		logging.L.Infow("shutdown: audio stop signal received, calling device.Stop()")
		device.Stop()
		logging.L.Infow("shutdown: device.Stop() returned, uninitialising")
		device.Uninit()
		ctx.Uninit() //nolint
		logging.L.Infow("shutdown: audio cleanup complete, closing frames channel")
		close(c.frames)
	}()

	return nil
}

// Stop shuts down capture and waits for cleanup.
func (c *Capturer) Stop() {
	logging.L.Infow("shutdown: Capturer.Stop() called, closing stop channel")
	close(c.stop)
	logging.L.Infow("shutdown: waiting for audio cleanup goroutine")
	c.wg.Wait()
	logging.L.Infow("shutdown: Capturer.Stop() done")
}

// ListDevices prints all available capture device names to stdout.
func ListDevices() error {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendCoreaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return err
	}
	defer ctx.Uninit() //nolint

	devices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return err
	}
	for _, d := range devices {
		fmt.Printf("  capture: %s\n", d.Name())
	}
	return nil
}

func deviceList(devices []malgo.DeviceInfo) string {
	names := make([]string, len(devices))
	for i, d := range devices {
		names[i] = d.Name()
	}
	return strings.Join(names, ", ")
}
