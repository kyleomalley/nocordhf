package meshcore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame is one decoded host/radio serial frame: the marker byte that
// identifies direction (FrameIncoming or FrameOutgoing) plus the
// length-prefixed payload. Payload is the application-level message —
// the first byte is a ResponseCode/PushCode (incoming) or a
// CommandCode (outgoing).
type Frame struct {
	Marker  byte
	Payload []byte
}

// MaxFramePayload caps a single frame's payload length to a value
// large enough for any companion-protocol message we expect (channel
// names, contact records up to 32-byte names + 64-byte path + 32-byte
// pubkey, telemetry blobs) but small enough to detect a runaway length
// field — a stray byte misinterpreted as a marker would otherwise
// stall the reader on a multi-megabyte allocation.
const MaxFramePayload = 8 * 1024

// EncodeFrame serialises a Frame to its on-wire form:
// [marker:1][length:uint16-LE][payload:length].
func EncodeFrame(f Frame) []byte {
	out := make([]byte, 3+len(f.Payload))
	out[0] = f.Marker
	binary.LittleEndian.PutUint16(out[1:3], uint16(len(f.Payload)))
	copy(out[3:], f.Payload)
	return out
}

// EncodeOutgoing wraps a host → radio command payload in the Outgoing
// frame marker. Convenience wrapper used by every Client send method.
func EncodeOutgoing(payload []byte) []byte {
	return EncodeFrame(Frame{Marker: FrameOutgoing, Payload: payload})
}

// FrameReader decodes a stream of incoming serial bytes into Frames.
// Mirrors the resync behaviour of the upstream JS reference: when an
// unexpected first byte is encountered we discard one byte and try
// again, so a partial flush after a reconnect doesn't permanently
// desync the stream.
type FrameReader struct {
	r   io.Reader
	buf []byte
	tmp [512]byte
}

// NewFrameReader wraps r so callers can pull whole frames via Next.
// r is typically a serial port handle.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r}
}

// Next reads bytes from the underlying reader until a complete frame
// has been assembled, then returns it. Blocks while waiting for
// bytes; returns the underlying read error on failure (callers
// usually treat io.EOF and "port closed" identically — the Client
// shutdown path closes the port to unblock this read).
func (fr *FrameReader) Next() (Frame, error) {
	for {
		if frame, ok, err := fr.tryDecode(); err != nil {
			return Frame{}, err
		} else if ok {
			return frame, nil
		}
		n, err := fr.r.Read(fr.tmp[:])
		if n > 0 {
			fr.buf = append(fr.buf, fr.tmp[:n]...)
		}
		if err != nil {
			return Frame{}, err
		}
	}
}

// tryDecode pulls one frame off the buffer if a whole frame is
// available. Returns (frame, true, nil) on success, (zero, false,
// nil) when more bytes are needed, or (zero, false, err) on a
// fatal protocol error.
func (fr *FrameReader) tryDecode() (Frame, bool, error) {
	for len(fr.buf) >= 3 {
		marker := fr.buf[0]
		if marker != FrameIncoming && marker != FrameOutgoing {
			// Resync: drop one byte and look again. Matches the
			// upstream JS behaviour exactly so a noisy boot-up
			// banner doesn't permanently corrupt the read stream.
			fr.buf = fr.buf[1:]
			continue
		}
		length := int(binary.LittleEndian.Uint16(fr.buf[1:3]))
		if length == 0 {
			// Zero-length frame is meaningless — same resync
			// behaviour as an unknown marker.
			fr.buf = fr.buf[1:]
			continue
		}
		if length > MaxFramePayload {
			return Frame{}, false, fmt.Errorf("meshcore: frame length %d exceeds max %d", length, MaxFramePayload)
		}
		need := 3 + length
		if len(fr.buf) < need {
			return Frame{}, false, nil
		}
		payload := make([]byte, length)
		copy(payload, fr.buf[3:need])
		fr.buf = fr.buf[need:]
		return Frame{Marker: marker, Payload: payload}, true, nil
	}
	return Frame{}, false, nil
}

// ErrShortPayload is returned by parsers when an incoming payload is
// shorter than the protocol requires. Callers usually log + drop the
// frame; a malformed message shouldn't kill the Client.
var ErrShortPayload = errors.New("meshcore: payload shorter than expected")
