// civ.go implements the ICOM CI-V CAT protocol over USB serial.
// Targets the IC-7300 at address 0x94 via /dev/tty.SLAB_USBtoUART.
//
// The IC-7300 runs in CI-V transceive mode by default: it broadcasts an
// unsolicited frequency frame whenever the VFO changes:
//
//	FE FE 00 94 00 <5 BCD bytes> FD
//
// address 0x00 = broadcast-to-all, cmd 0x00 = set-freq broadcast.
// We listen for these frames rather than polling with cmd 0x03.
//
// CI-V frequency encoding — 5 bytes, LSB first, each byte = 2 BCD digits:
//
//	14,028,080 Hz → bytes 0x00 0x80 0x02 0x14 0x00
//	  byte[0]=0x00 →    0 Hz  (ones=0, tens=0)
//	  byte[1]=0x80 → 8000 Hz  (ones=0, tens=8) × 100
//	  byte[2]=0x02 →20000 Hz  (ones=2, tens=0) × 10000
//	  byte[3]=0x14 →  14 MHz  (ones=4, tens=1) × 1000000
//	  byte[4]=0x00 →    0     × 100000000
//	  total: 14,028,080 Hz
package cat

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	DefaultPort    = "/dev/tty.SLAB_USBtoUART"
	DefaultBaud    = 115200
	ControllerAddr = 0xE0 // PC controller address
	RadioAddr      = 0x94 // IC-7300 default CI-V address

	preamble = 0xFE
	eom      = 0xFD

	cmdSetFreq   = 0x05
	cmdReadFreq  = 0x03
	cmdSetMode   = 0x06
	cmdSplit     = 0x0F
	cmdReadMeter = 0x15
	cmdPTT       = 0x1C

	// Sub-commands for cmdReadMeter (0x15).
	meterSMeter = 0x02 // S-meter (RX): 0–255
	meterPower  = 0x11 // RF power output (TX): 0–255
	meterSWR    = 0x12 // SWR (TX): 0–255
	meterALC    = 0x13 // ALC (TX): 0–255

	// cmdFreqBroadcast is the unsolicited frequency broadcast the IC-7300
	// sends to address 0x00 (all) whenever the VFO changes (transceive mode).
	cmdFreqBroadcast = 0x00
	addrBroadcast    = 0x00
)

// Radio manages a CI-V session with the IC-7300.
// IcomRadio manages a CI-V session with the IC-7300.
// Implements the Radio interface.
type IcomRadio struct {
	portName string
	baud     int
	port     serial.Port
	mu       sync.Mutex
	lastHz   atomic.Uint64
	// Meter readings: raw 0–255 values from the radio.
	lastSMeter atomic.Uint32
	lastPower  atomic.Uint32
	lastSWR    atomic.Uint32
	lastALC    atomic.Uint32
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// Open opens the serial port and returns a ready Radio.
// Starts a background goroutine that listens for unsolicited frequency broadcasts.
func OpenIcom(portName string, baud int) (*IcomRadio, error) {
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	p, err := serial.Open(portName, mode)
	if err != nil {
		return nil, fmt.Errorf("open serial %s: %w", portName, err)
	}
	if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
		p.Close() //nolint
		return nil, fmt.Errorf("set timeout: %w", err)
	}
	logging.L.Infow("CI-V port opened", "port", portName, "baud", baud)
	r := &IcomRadio{
		portName: portName,
		baud:     baud,
		port:     p,
		stopCh:   make(chan struct{}),
	}
	// Send a freq query immediately so the radio can lock onto our baud rate
	// (required when CI-V USB Baud Rate is set to "Auto" on the IC-7300).
	_ = r.send(cmdReadFreq, nil)
	r.wg.Add(1)
	go r.listenLoop()
	return r, nil
}

// Close stops the listen loop and closes the serial port.
// Signals stopCh and closes the port to interrupt any in-progress Read, then
// waits up to 500ms for the goroutine to exit. If it doesn't exit in time
// (macOS serial Read sometimes ignores port close), we proceed anyway — the
// goroutine will exit on its own at the next 200ms read timeout.
func (r *IcomRadio) Close() error {
	logging.L.Infow("shutdown: signalling CAT listenLoop to stop")
	close(r.stopCh)
	logging.L.Infow("shutdown: closing port to unblock Read")
	r.port.Close() //nolint

	logging.L.Infow("shutdown: waiting for CAT listenLoop to exit (max 500ms)")
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logging.L.Infow("shutdown: CAT listenLoop exited cleanly")
	case <-time.After(500 * time.Millisecond):
		logging.L.Warnw("shutdown: CAT listenLoop did not exit in time, continuing anyway")
	}
	return nil
}

// Frequency returns the most recently observed VFO frequency in Hz.
// Returns 0 if no broadcast has been received yet.
func (r *IcomRadio) Frequency() uint64 {
	return r.lastHz.Load()
}

// SMeter returns the last S-meter reading (0–255). Only valid during RX.
func (r *IcomRadio) SMeter() uint32 { return r.lastSMeter.Load() }

// Power returns the last RF power meter reading (0–255). Only valid during TX.
func (r *IcomRadio) Power() uint32 { return r.lastPower.Load() }

// SWR returns the last SWR meter reading (0–255). Only valid during TX.
func (r *IcomRadio) SWR() uint32 { return r.lastSWR.Load() }

// ALC returns the last ALC meter reading (0–255). Only valid during TX.
func (r *IcomRadio) ALC() uint32 { return r.lastALC.Load() }

// ReadMeters sends CI-V queries for S-meter, power, SWR, and ALC.
// Responses arrive asynchronously via handleFrame.
func (r *IcomRadio) ReadMeters() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.send(cmdReadMeter, []byte{meterSMeter})
	_ = r.send(cmdReadMeter, []byte{meterPower})
	_ = r.send(cmdReadMeter, []byte{meterSWR})
	_ = r.send(cmdReadMeter, []byte{meterALC})
}

// listenLoop reads the serial port continuously, parsing any CI-V frames it
// finds. It updates r.lastHz whenever a frequency broadcast arrives.
func (r *IcomRadio) listenLoop() {
	defer r.wg.Done()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 64)
	logging.L.Infow("CI-V listen loop started")

	for {
		select {
		case <-r.stopCh:
			logging.L.Infow("CI-V listen loop: stop signal received, exiting")
			return
		default:
		}

		// Snapshot the port reference without holding the lock during Read.
		// This lets Close() close the port concurrently to interrupt a blocking Read.
		r.mu.Lock()
		p := r.port
		r.mu.Unlock()

		n, err := p.Read(tmp)
		if err != nil {
			// Check stopCh first — if we're shutting down, exit immediately
			// regardless of the error type (port closed, EOF, etc.).
			select {
			case <-r.stopCh:
				logging.L.Infow("CI-V listen loop: exiting after read error on shutdown", "err", err)
				return
			default:
			}
			errStr := err.Error()
			if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "Timeout") {
				// Normal 200ms poll timeout — ignore.
				continue
			}
			// Persistent error (USB reset, "device not configured", etc.) — reconnect.
			logging.L.Warnw("CI-V read error, reconnecting", "err", err)
			r.lastHz.Store(0) // signal UI that CAT is down
			r.mu.Lock()
			r.port.Close() //nolint
			r.mu.Unlock()

			// Wait for the device to recover, then reopen.
			select {
			case <-r.stopCh:
				return
			case <-time.After(3 * time.Second):
			}

			mode := &serial.Mode{
				BaudRate: r.baud,
				DataBits: 8,
				Parity:   serial.NoParity,
				StopBits: serial.OneStopBit,
			}
			for {
				select {
				case <-r.stopCh:
					return
				default:
				}
				p, openErr := serial.Open(r.portName, mode)
				if openErr != nil {
					logging.L.Warnw("CI-V reopen failed, retrying", "err", openErr)
					select {
					case <-r.stopCh:
						return
					case <-time.After(3 * time.Second):
					}
					continue
				}
				if setErr := p.SetReadTimeout(200 * time.Millisecond); setErr != nil {
					p.Close() //nolint
					continue
				}
				r.mu.Lock()
				r.port = p
				logging.L.Infow("CI-V reconnected", "port", r.portName)
				_ = r.send(cmdReadFreq, nil)
				r.mu.Unlock()
				buf = buf[:0]
				break
			}
			continue
		}
		if n == 0 {
			continue
		}
		logging.L.Debugw("CI-V raw bytes", "hex", hex.EncodeToString(tmp[:n]))
		buf = append(buf, tmp[:n]...)

		// Keep buffer from growing unbounded.
		if len(buf) > 512 {
			buf = buf[len(buf)-256:]
		}

		// Parse all complete frames out of the buffer.
		buf = r.parseFrames(buf)
	}
}

// parseFrames scans buf for complete CI-V frames (ending with 0xFD), processes
// them, and returns the unconsumed tail.
func (r *IcomRadio) parseFrames(buf []byte) []byte {
	for {
		// Find next preamble pair.
		start := -1
		for i := 0; i+1 < len(buf); i++ {
			if buf[i] == preamble && buf[i+1] == preamble {
				start = i
				break
			}
		}
		if start < 0 {
			return buf[:0]
		}

		// Find EOM.
		end := -1
		for i := start + 2; i < len(buf); i++ {
			if buf[i] == eom {
				end = i
				break
			}
		}
		if end < 0 {
			// Incomplete frame — keep from start onward.
			return buf[start:]
		}

		frame := buf[start : end+1]
		r.handleFrame(frame)
		buf = buf[end+1:]
	}
}

// handleFrame processes a single complete CI-V frame.
func (r *IcomRadio) handleFrame(frame []byte) {
	// Minimum frame: FE FE dst src cmd FD = 6 bytes
	if len(frame) < 6 {
		return
	}
	// frame[0..1] = FE FE
	dst := frame[2]
	src := frame[3]
	cmd := frame[4]
	data := frame[5 : len(frame)-1] // strip leading FE FE dst src cmd and trailing FD

	logging.L.Debugw("CI-V frame", "dst", fmt.Sprintf("%02X", dst), "src", fmt.Sprintf("%02X", src), "cmd", fmt.Sprintf("%02X", cmd), "data", hex.EncodeToString(data))

	// Accept frequency broadcasts: FE FE 00 94 00 <5 bytes> FD
	// Also accept explicit responses to our query: FE FE E0 94 03 <5 bytes> FD
	isFreqBroadcast := dst == addrBroadcast && src == RadioAddr && cmd == cmdFreqBroadcast
	isFreqResponse := dst == ControllerAddr && src == RadioAddr && cmd == cmdReadFreq

	if (isFreqBroadcast || isFreqResponse) && len(data) == 5 {
		hz := bcdToHz(data)
		if hz > 0 {
			old := r.lastHz.Swap(hz)
			if old != hz {
				logging.L.Infow("CI-V frequency", "hz", hz, "mhz", fmt.Sprintf("%.4f", float64(hz)/1e6))
			}
		}
		return
	}

	// Meter responses: FE FE E0 94 15 <sub> <2 BCD bytes> FD
	// The 2 BCD bytes encode a value 0000–0255.
	if dst == ControllerAddr && src == RadioAddr && cmd == cmdReadMeter && len(data) >= 3 {
		sub := data[0]
		val := bcdMeterValue(data[1:3])
		switch sub {
		case meterSMeter:
			r.lastSMeter.Store(val)
		case meterPower:
			r.lastPower.Store(val)
		case meterSWR:
			r.lastSWR.Store(val)
		case meterALC:
			r.lastALC.Store(val)
		}
	}
}

// SetFrequency sets the VFO to freq Hz.
func (r *IcomRadio) SetFrequency(freq uint64) error {
	logging.L.Infow("SetFrequency", "freq_hz", freq, "mhz", fmt.Sprintf("%.4f", float64(freq)/1e6))
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.send(cmdSetFreq, hzToBCD(freq)); err != nil {
		logging.L.Warnw("SetFrequency send failed", "freq_hz", freq, "err", err)
		return err
	}
	time.Sleep(50 * time.Millisecond) // give radio time to ack
	return nil
}

// SplitOn enables split TX/RX mode. VFO-A is used for receive, VFO-B for
// transmit. Call before PTTOn so the radio shows the split TX indicator.
func (r *IcomRadio) SplitOn() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.send(cmdSplit, []byte{0x01})
}

// SplitOff disables split TX/RX mode. Call after PTTOff to return the radio
// to normal simplex operation.
func (r *IcomRadio) SplitOff() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.send(cmdSplit, []byte{0x00}); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

// PTTOn keys the transmitter.
func (r *IcomRadio) PTTOn() error {
	return r.setPTT(true)
}

// PTTOff unkeys the transmitter.
func (r *IcomRadio) PTTOff() error {
	return r.setPTT(false)
}

func (r *IcomRadio) setPTT(on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	val := byte(0x00)
	if on {
		val = 0x01
	}
	if err := r.send(cmdPTT, []byte{0x00, val}); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

// send writes: FE FE <RadioAddr> <ControllerAddr> <cmd> [data...] FD
// Caller must hold r.mu (or accept the race during Open's initial probe).
func (r *IcomRadio) send(cmd byte, data []byte) error {
	frame := []byte{preamble, preamble, RadioAddr, ControllerAddr, cmd}
	frame = append(frame, data...)
	frame = append(frame, eom)
	logging.L.Debugw("CI-V send", "hex", hex.EncodeToString(frame))
	_, err := r.port.Write(frame)
	return err
}

// bcdMeterValue decodes 2 BCD bytes (MSB first) into a 0–255 meter value.
// byte[0] = hundreds+tens, byte[1] = ones+tenths (tenths ignored).
// E.g. 0x01 0x28 → 128.
func bcdMeterValue(b []byte) uint32 {
	hi := uint32((b[0]>>4)&0x0F)*100 + uint32(b[0]&0x0F)*10
	lo := uint32((b[1] >> 4) & 0x0F)
	return hi + lo
}

// bcdToHz decodes 5 CI-V BCD bytes (LSB first) to Hz.
// Each byte: high nibble = tens digit, low nibble = ones digit.
// byte[0] × 1, byte[1] × 100, byte[2] × 10000, byte[3] × 1000000, byte[4] × 100000000
func bcdToHz(b []byte) uint64 {
	var hz uint64
	mult := uint64(1)
	for _, v := range b {
		tens := uint64((v >> 4) & 0x0F)
		ones := uint64(v & 0x0F)
		hz += (tens*10 + ones) * mult
		mult *= 100
	}
	return hz
}

// hzToBCD encodes Hz to 5 CI-V BCD bytes (LSB first).
func hzToBCD(hz uint64) []byte {
	b := make([]byte, 5)
	for i := range b {
		pair := hz % 100
		hz /= 100
		tens := (pair / 10) & 0x0F
		ones := (pair % 10) & 0x0F
		b[i] = byte(tens<<4 | ones)
	}
	return b
}
