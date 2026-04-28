package audio

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

// Player streams a float32 audio buffer to a named output device.
// Only one Play call may be active at a time.
type Player struct {
	deviceName string
	// LastPeak is the peak |sample| sent during the most recent Play call,
	// in [0, 1]. Useful for diagnosing TX audio overdrive: at peak ≥ 0.95
	// you're approaching digital clipping at the audio output, well before
	// the rig's ALC sees anything. reference design-style TX setup wants peak around
	// 0.5–0.7 (≈ -6 to -3 dBFS) — high enough that the rig's USB MOD LEVEL
	// can run at a sensible spot, low enough to leave headroom for any
	// downstream gain stage.
	LastPeak atomic.Uint64 // float64 bits via math.Float64bits
}

// NewPlayer creates a Player targeting a device whose name contains deviceName.
func NewPlayer(deviceName string) *Player {
	return &Player{deviceName: deviceName}
}

// Play opens the output device, streams samples, and blocks until playback
// is complete or stopCh is closed (whichever comes first).
// When stopCh fires, playback is silenced immediately and Play returns early.
// Pass a nil channel to play to completion unconditionally.
func (p *Player) Play(samples []float32, stopCh <-chan struct{}) error {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendCoreaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("malgo init context: %w", err)
	}
	defer ctx.Uninit() //nolint

	devices, err := ctx.Devices(malgo.Playback)
	if err != nil {
		return fmt.Errorf("enumerate devices: %w", err)
	}

	var targetID malgo.DeviceID
	found := false
	for _, d := range devices {
		logging.L.Debugw("playback device available", "name", d.Name())
		if strings.Contains(strings.ToLower(d.Name()), strings.ToLower(p.deviceName)) {
			targetID = d.ID
			found = true
			logging.L.Infow("playback device matched", "name", d.Name())
			break
		}
	}
	if !found {
		names := make([]string, len(devices))
		for i, d := range devices {
			names[i] = d.Name()
		}
		return fmt.Errorf("output device %q not found; available: %s",
			p.deviceName, strings.Join(names, ", "))
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.DeviceID = targetID.Pointer()
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	cfg.SampleRate = 48000
	cfg.Alsa.NoMMap = 1

	var pos atomic.Int64
	var stopped atomic.Bool
	total := int64(len(samples))

	// Peak amplitude across all samples actually sent. Computed once upfront
	// (samples is read-only) and published via defer so it lands regardless
	// of which path Play exits via — the previous version only logged on the
	// natural-completion path, but TX always exits via the slot-end stopCh
	// path, so peak/clip stats were never published. Hence every TX-done
	// log line was reading the initial "0" out of the atomic.
	var peak float64
	for _, s := range samples {
		a := float64(s)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	clipped := 0
	for _, s := range samples {
		if s >= 1.0 || s <= -1.0 {
			clipped++
		}
	}
	defer func() {
		p.LastPeak.Store(math.Float64bits(peak))
		dbfs := -120.0
		if peak > 0 {
			dbfs = 20 * math.Log10(peak)
		}
		logging.L.Infow("tx audio levels",
			"peak", fmt.Sprintf("%.3f", peak),
			"peak_dbfs", fmt.Sprintf("%.1f", dbfs),
			"clipped_samples", clipped,
			"total_samples", len(samples),
		)
		if peak >= 0.95 {
			logging.L.Warnw("tx audio approaching digital clip — splatter likely",
				"peak", fmt.Sprintf("%.3f", peak),
				"hint", "lower the PWR slider in nocordhf; keep macOS output volume at 100% and adjust IC-7300 USB MOD LEVEL on the rig",
			)
		}
	}()

	onSend := func(output, _ []byte, frameCount uint32) {
		n := int(frameCount)
		out := unsafe.Slice((*int16)(unsafe.Pointer(&output[0])), n)

		if stopped.Load() {
			for i := range out {
				out[i] = 0
			}
			return
		}

		cur := pos.Load()
		remaining := total - cur
		if remaining <= 0 {
			for i := range out {
				out[i] = 0
			}
			return
		}

		send := int64(n)
		if send > remaining {
			send = remaining
		}
		for i := int64(0); i < send; i++ {
			s := samples[cur+i]
			if s > 1.0 {
				s = 1.0
			} else if s < -1.0 {
				s = -1.0
			}
			out[i] = int16(s * 32767)
		}
		for i := send; i < int64(n); i++ {
			out[i] = 0
		}
		pos.Add(send)
	}

	callbacks := malgo.DeviceCallbacks{Data: onSend}
	device, err := malgo.InitDevice(ctx.Context, cfg, callbacks)
	if err != nil {
		return fmt.Errorf("init device: %w", err)
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		return fmt.Errorf("start device: %w", err)
	}
	logging.L.Infow("playback started", "device", p.deviceName, "samples", len(samples), "sampleRate", cfg.SampleRate)

	// Dump TX audio to WAV for debugging.
	debugPath := fmt.Sprintf("recordings/tx_debug_%d.wav", time.Now().Unix())
	if err := SaveWAV(debugPath, samples, int(cfg.SampleRate)); err != nil {
		logging.L.Warnw("tx debug wav save failed", "err", err)
	} else {
		logging.L.Infow("tx debug wav saved", "path", debugPath)
	}

	// Poll until all samples have been handed to the callback, or stopCh fires.
	for pos.Load() < total {
		if stopCh != nil {
			select {
			case <-stopCh:
				stopped.Store(true)
				device.Stop() // stop immediately; callback already outputs silence
				return nil
			default:
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// One extra sleep to let the device clock out its internal buffer.
	time.Sleep(200 * time.Millisecond)

	device.Stop()
	return nil
}
