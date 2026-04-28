package audio

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// SaveWAV writes a mono float32 PCM slice to a 16-bit WAV file.
// Samples are clamped to [-1, 1] and converted to int16.
func SaveWAV(path string, samples []float32, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create wav: %w", err)
	}
	defer f.Close()

	nSamples := len(samples)
	dataSize := nSamples * 2 // 16-bit = 2 bytes per sample

	// RIFF header
	write := func(v any) { binary.Write(f, binary.LittleEndian, v) } //nolint
	f.WriteString("RIFF")
	write(uint32(36 + dataSize))
	f.WriteString("WAVE")

	// fmt chunk
	f.WriteString("fmt ")
	write(uint32(16)) // chunk size
	write(uint16(1))  // PCM
	write(uint16(1))  // mono
	write(uint32(sampleRate))
	write(uint32(sampleRate * 2)) // byte rate
	write(uint16(2))              // block align
	write(uint16(16))             // bits per sample

	// data chunk
	f.WriteString("data")
	write(uint32(dataSize))
	for _, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		write(int16(s * 32767))
	}
	return nil
}

// FrameRecorder saves audio frames as WAV files in dir.
type FrameRecorder struct {
	dir     string
	buildID string
	freqHz  func() uint64 // called at save time to get current frequency
}

// NewFrameRecorder creates a FrameRecorder that saves to dir (created if needed).
// buildID is stamped into each filename. freqHz is called at save time for the
// current VFO frequency (pass nil if CAT is unavailable).
func NewFrameRecorder(dir, buildID string, freqHz func() uint64) (*FrameRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if freqHz == nil {
		freqHz = func() uint64 { return 0 }
	}
	return &FrameRecorder{dir: dir, buildID: buildID, freqHz: freqHz}, nil
}

// Save writes a frame to a timestamped WAV file.
// Filename: ft8_YYYYMMDD_HHMMSS_<buildID>_<freqHz>.wav
func (r *FrameRecorder) Save(f Frame) (string, error) {
	hz := r.freqHz()
	ts := f.SlotStart.UTC().Format("20060102_150405")
	name := fmt.Sprintf("%s/ft8_%s_%s_%d.wav", r.dir, ts, r.buildID, hz)
	return name, SaveWAV(name, f.Samples, SampleRate)
}

// suppress unused import
var _ = time.Time{}
