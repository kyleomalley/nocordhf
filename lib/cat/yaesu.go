// yaesu.go implements the Yaesu NewCAT protocol over USB serial.
// Targets the FT-891 via DigiRig DR-891.
//
// NewCAT uses ASCII text commands terminated by semicolons:
//
//	FA014074000;   — set VFO-A to 14.074 MHz
//	FA;            — read VFO-A frequency
//	TX1;           — PTT on
//	TX0;           — PTT off
//	RM1;           — read S-meter
package cat

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	// YaesuDefaultBaud is the FT-891 default CAT baud rate.
	YaesuDefaultBaud = 9600
)

// YaesuRadio manages a NewCAT session with the Yaesu FT-891.
// Implements the Radio interface.
type YaesuRadio struct {
	portName string
	baud     int
	stopBits serial.StopBits
	port     serial.Port
	mu       sync.Mutex // protects port writes and reads
	lastHz   atomic.Uint64

	lastSMeter atomic.Uint32
	lastPower  atomic.Uint32
	lastSWR    atomic.Uint32
	lastALC    atomic.Uint32

	stopCh chan struct{}
	wg     sync.WaitGroup

	// Log dedup: suppress repeated identical messages.
	lastLogMsg  string
	lastLogTime time.Time
	logRepeat   int
}

// OpenYaesu opens the serial port for a Yaesu FT-891 and returns a ready radio.
// Starts a background goroutine that polls frequency.
func OpenYaesu(portName string, baud int) (*YaesuRadio, error) {
	// Try 2 stop bits first (FT-891 spec), fall back to 1 stop bit.
	// Some USB-serial chips (e.g. Silicon Labs CP210x on DigiRig) reject
	// TwoStopBits via the go.bug.st/serial library.
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.TwoStopBits,
	}
	p, err := serial.Open(portName, mode)
	if err != nil {
		logging.L.Warnw("Yaesu 2-stop-bit open failed, trying 1 stop bit", "port", portName, "err", err)
		mode.StopBits = serial.OneStopBit
		p, err = serial.Open(portName, mode)
		if err != nil {
			return nil, fmt.Errorf("open serial %s: %w", portName, err)
		}
	}
	if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
		p.Close() //nolint
		return nil, fmt.Errorf("set timeout: %w", err)
	}
	logging.L.Infow("Yaesu port opened", "port", portName, "baud", baud, "stopBits", mode.StopBits)
	r := &YaesuRadio{
		portName: portName,
		baud:     baud,
		stopBits: mode.StopBits,
		port:     p,
		stopCh:   make(chan struct{}),
	}

	// Verify we can talk to the radio by reading its ID.
	if resp, err := r.command("ID;"); err == nil {
		logging.L.Infow("Yaesu identified", "id", resp)
	} else {
		logging.L.Warnw("Yaesu ID query failed (continuing anyway)", "err", err)
	}

	// Disable all auto-information — AI1 floods the bus with RM meter readings
	// which corrupt command responses (MD0C, TX1, FA). We poll freq instead.
	_, _ = r.command("AI0;")

	// Seed initial frequency.
	r.pollFreq()

	r.wg.Add(1)
	go r.listenLoop()
	return r, nil
}

// Close stops the listen loop and closes the serial port.
// We signal stopCh first and wait for listenLoop to exit before closing the
// port — this avoids a double-close race when reconnect() is in progress.
// The port has a 200 ms read timeout so the loop exits within one iteration.
func (r *YaesuRadio) Close() error {
	logging.L.Infow("shutdown: signalling Yaesu listenLoop to stop")
	close(r.stopCh)

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logging.L.Infow("shutdown: Yaesu listenLoop exited cleanly")
	case <-time.After(500 * time.Millisecond):
		logging.L.Warnw("shutdown: Yaesu listenLoop did not exit in time")
	}

	// Safe to close port now — listenLoop has exited (or timed out).
	r.mu.Lock()
	if r.port != nil {
		r.port.Close() //nolint
	}
	r.mu.Unlock()
	return nil
}

func (r *YaesuRadio) Frequency() uint64 { return r.lastHz.Load() }
func (r *YaesuRadio) SMeter() uint32    { return r.lastSMeter.Load() }
func (r *YaesuRadio) Power() uint32     { return r.lastPower.Load() }
func (r *YaesuRadio) SWR() uint32       { return r.lastSWR.Load() }
func (r *YaesuRadio) ALC() uint32       { return r.lastALC.Load() }

// ReadMeters sends NewCAT meter queries.
func (r *YaesuRadio) ReadMeters() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if resp, err := r.cmdLocked("RM1;"); err == nil {
		r.lastSMeter.Store(parseRM(resp))
	}
	if resp, err := r.cmdLocked("RM5;"); err == nil {
		r.lastPower.Store(parseRM(resp))
	}
	if resp, err := r.cmdLocked("RM6;"); err == nil {
		r.lastSWR.Store(parseRM(resp))
	}
	if resp, err := r.cmdLocked("RM4;"); err == nil {
		r.lastALC.Store(parseRM(resp))
	}
}

// SetFrequency sets VFO-A to freq Hz.
func (r *YaesuRadio) SetFrequency(freq uint64) error {
	logging.L.Infow("Yaesu SetFrequency", "freq_hz", freq)
	cmd := fmt.Sprintf("FA%09d;", freq)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.cmdLocked(cmd)
	return err
}

// PTTOn switches VFO-A to DATA-USB mode (routes audio from DigiRig DATA jack)
// and keys the transmitter. No split needed — we switch the mode directly.
func (r *YaesuRadio) PTTOn() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Set DATA-USB mode so the radio routes audio from the DATA jack (DigiRig).
	// TX1 keys the transmitter — in DATA-USB mode the DATA jack is the audio source.
	r.sendLocked("MD0C;")
	time.Sleep(100 * time.Millisecond)
	r.sendLocked("TX1;")
	return nil
}

// PTTOff unkeys and restores USB mode for RX.
func (r *YaesuRadio) PTTOff() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendLocked("TX0;")
	r.sendLocked("MD02;")
	return nil
}

// SplitOn enables split mode (VFO-A RX, VFO-B TX).
func (r *YaesuRadio) SplitOn() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.cmdLocked("ST1;")
	return err
}

// SplitOff disables split mode.
func (r *YaesuRadio) SplitOff() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.cmdLocked("ST0;")
	return err
}

// listenLoop polls frequency and reads unsolicited data from the radio.
func (r *YaesuRadio) listenLoop() {
	defer r.wg.Done()
	logging.L.Infow("Yaesu listen loop started")

	buf := make([]byte, 0, 256)
	tmp := make([]byte, 128)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			logging.L.Infow("Yaesu listen loop: stop signal received")
			return
		default:
		}

		// Read any available data (unsolicited AI broadcasts).
		r.mu.Lock()
		p := r.port
		r.mu.Unlock()

		n, err := p.Read(tmp)
		if err != nil {
			select {
			case <-r.stopCh:
				return
			default:
			}
			errStr := err.Error()
			if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "Timeout") {
				// Normal poll timeout.
			} else {
				logging.L.Warnw("Yaesu read error, reconnecting", "err", err)
				r.lastHz.Store(0)
				r.reconnect()
				buf = buf[:0]
			}
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 512 {
				buf = buf[len(buf)-256:]
			}
			buf = r.parseResponses(buf)
		}

		// Periodic frequency poll (in case AI broadcasts aren't working).
		select {
		case <-ticker.C:
			r.pollFreq()
		default:
		}
	}
}

// pollFreq queries the radio for the current VFO-A frequency.
func (r *YaesuRadio) pollFreq() {
	r.mu.Lock()
	resp, err := r.cmdLocked("FA;")
	r.mu.Unlock()
	if err != nil {
		return
	}
	if hz := parseFA(resp); hz > 0 {
		old := r.lastHz.Swap(hz)
		if old != hz {
			logging.L.Infow("Yaesu frequency", "hz", hz, "mhz", fmt.Sprintf("%.4f", float64(hz)/1e6))
		}
	}
}

// parseResponses extracts complete NewCAT responses (terminated by ;) from buf.
func (r *YaesuRadio) parseResponses(buf []byte) []byte {
	for {
		idx := strings.IndexByte(string(buf), ';')
		if idx < 0 {
			return buf
		}
		resp := string(buf[:idx+1])
		buf = buf[idx+1:]
		r.handleResponse(resp)
	}
}

// handleResponse processes a single NewCAT response.
func (r *YaesuRadio) handleResponse(resp string) {
	if len(resp) < 3 {
		return
	}
	cmd := resp[:2]
	// Don't log RM (meter) responses — they fire hundreds of times per second.
	if cmd != "RM" {
		r.debugDedup("Yaesu response", "resp", resp)
	}

	switch cmd {
	case "FA":
		if hz := parseFA(resp); hz > 0 {
			old := r.lastHz.Swap(hz)
			if old != hz {
				logging.L.Infow("Yaesu frequency (broadcast)", "hz", hz)
			}
		}
	case "RM":
		if len(resp) >= 7 {
			meter := resp[2:3]
			val := parseRM(resp)
			switch meter {
			case "1":
				r.lastSMeter.Store(val)
			case "4":
				r.lastALC.Store(val)
			case "5":
				r.lastPower.Store(val)
			case "6":
				r.lastSWR.Store(val)
			}
		}
	}
}

// command sends a NewCAT command and reads the response. Thread-safe.
func (r *YaesuRadio) command(cmd string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmdLocked(cmd)
}

// debugDedup logs a debug message, suppressing identical repeats.
// Flushes a summary when the message changes or 60s elapses.
func (r *YaesuRadio) debugDedup(msg string, kvs ...interface{}) {
	now := time.Now()
	key := fmt.Sprintf("%s %v", msg, kvs)
	if key == r.lastLogMsg && now.Sub(r.lastLogTime) < 60*time.Second {
		r.logRepeat++
		return
	}
	if r.logRepeat > 0 {
		logging.L.Debugw("Yaesu (repeated)", "msg", r.lastLogMsg, "count", r.logRepeat)
	}
	r.lastLogMsg = key
	r.lastLogTime = now
	r.logRepeat = 0
	logging.L.Debugw(msg, kvs...)
}

// sendLocked writes a command without waiting for a response.
// Use for set commands (FB, SV, MD, ST) that produce no reply.
func (r *YaesuRadio) sendLocked(cmd string) {
	r.debugDedup("Yaesu send", "cmd", cmd)
	r.port.Write([]byte(cmd)) //nolint
	time.Sleep(50 * time.Millisecond)
}

// cmdLocked sends a command and reads response. Caller must hold r.mu.
func (r *YaesuRadio) cmdLocked(cmd string) (string, error) {
	r.debugDedup("Yaesu send", "cmd", cmd)
	if _, err := r.port.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("yaesu write: %w", err)
	}
	time.Sleep(50 * time.Millisecond) // post-write delay

	// Read response until semicolon or timeout.
	buf := make([]byte, 0, 64)
	tmp := make([]byte, 64)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := r.port.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if idx := strings.IndexByte(string(buf), ';'); idx >= 0 {
				resp := string(buf[:idx+1])
				r.debugDedup("Yaesu recv", "resp", resp)
				return resp, nil
			}
		}
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "Timeout") {
				continue
			}
			return "", fmt.Errorf("yaesu read: %w", err)
		}
	}
	// Some set commands have no response.
	if len(buf) == 0 {
		return "", nil
	}
	return string(buf), nil
}

// reconnect closes and reopens the serial port.
func (r *YaesuRadio) reconnect() {
	r.mu.Lock()
	r.port.Close() //nolint
	r.mu.Unlock()

	select {
	case <-r.stopCh:
		return
	case <-time.After(3 * time.Second):
	}

	mode := &serial.Mode{
		BaudRate: r.baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: r.stopBits,
	}
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}
		p, err := serial.Open(r.portName, mode)
		if err != nil {
			logging.L.Warnw("Yaesu reopen failed, retrying", "err", err)
			select {
			case <-r.stopCh:
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
			p.Close() //nolint
			continue
		}
		r.mu.Lock()
		r.port = p
		r.mu.Unlock()
		logging.L.Infow("Yaesu reconnected", "port", r.portName)
		_, _ = r.command("AI0;") // keep auto-info off (AI1 floods serial with RM readings)
		return
	}
}

// parseFA extracts frequency in Hz from "FA014074000;" response.
func parseFA(resp string) uint64 {
	resp = strings.TrimSuffix(resp, ";")
	if len(resp) < 4 || resp[:2] != "FA" {
		return 0
	}
	hz, err := strconv.ParseUint(resp[2:], 10, 64)
	if err != nil {
		return 0
	}
	return hz
}

// parseRM extracts the 3-digit meter value (0–255) from "RM1128;" response.
func parseRM(resp string) uint32 {
	resp = strings.TrimSuffix(resp, ";")
	if len(resp) < 6 || resp[:2] != "RM" {
		return 0
	}
	// RM<meter><3 digits> — meter is 1 char, value is 3 digits
	valStr := resp[3:]
	val, err := strconv.ParseUint(valStr, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(val)
}
