package meshcore

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.bug.st/serial"
)

// serialTransport speaks the companion-radio protocol over a USB /
// UART serial port. Frames on the wire are length-prefixed: each
// outgoing payload is wrapped with the FrameOutgoing marker (`<`
// 0x3c) and a uint16-LE length; each incoming frame is decoded by
// FrameReader and the payload handed to recv as a single chunk.
type serialTransport struct {
	port io.ReadWriteCloser
	fr   *FrameReader

	recv    chan []byte
	closing chan struct{}
	closed  bool
	closeMu sync.Mutex
	wg      sync.WaitGroup
}

// newSerialTransport opens the given device path at the given baud
// rate (or DefaultBaud if baud <= 0) and starts the read goroutine.
func newSerialTransport(portPath string, baud int) (*serialTransport, error) {
	if baud <= 0 {
		baud = DefaultBaud
	}
	mode := &serial.Mode{BaudRate: baud}
	port, err := serial.Open(portPath, mode)
	if err != nil {
		return nil, fmt.Errorf("meshcore: open %s: %w", portPath, err)
	}
	// Generous read timeout. A blocking read with no timeout means
	// Close can't unblock the reader; a too-short timeout busy-loops
	// the goroutine. 250 ms is a fine compromise — Close polls the
	// closing channel between reads.
	if err := port.SetReadTimeout(250 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("meshcore: set read timeout: %w", err)
	}
	t := &serialTransport{
		port:    port,
		fr:      NewFrameReader(port),
		recv:    make(chan []byte, 32),
		closing: make(chan struct{}),
	}
	t.wg.Add(1)
	go t.readLoop()
	return t, nil
}

func (t *serialTransport) Send(payload []byte) error {
	_, err := t.port.Write(EncodeOutgoing(payload))
	return err
}

func (t *serialTransport) Receive() <-chan []byte { return t.recv }

func (t *serialTransport) Close() error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closing)
	t.closeMu.Unlock()
	err := t.port.Close()
	t.wg.Wait()
	return err
}

// readLoop pumps decoded frames into recv. Drops outgoing-marker
// frames (a misconfigured loopback would otherwise echo our own
// commands back as "responses"). Exits when the port returns a
// non-transient error or Close fires.
func (t *serialTransport) readLoop() {
	defer t.wg.Done()
	defer close(t.recv)
	for {
		frame, err := t.fr.Next()
		if err != nil {
			select {
			case <-t.closing:
				return
			default:
			}
			// SetReadTimeout above means a "no data" tick surfaces
			// as either io.EOF or a timeout-flavoured error from
			// go.bug.st/serial. Either way, just loop — we want
			// the reader to keep running until the port truly closes.
			if isTransientReadError(err) {
				continue
			}
			return
		}
		if frame.Marker != FrameIncoming {
			continue
		}
		select {
		case t.recv <- frame.Payload:
		case <-t.closing:
			return
		}
	}
}

// isTransientReadError flags read errors that just mean "no data
// available right now" — we want the read loop to keep going. The
// go.bug.st/serial library surfaces a timeout as a zero-byte read +
// nil error so we don't usually see one here, but io.EOF on a still-
// open port is also transient on macOS during reconnect.
func isTransientReadError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	return false
}
