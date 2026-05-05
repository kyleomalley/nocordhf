package meshcore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// MeshCore companion-firmware BLE protocol (Nordic UART Service
// flavour), per liamcottle/meshcore.js src/constants.js. The host
// WRITES to RX, the host receives notifications from TX. There's no
// per-payload framing — each GATT notification IS one application-
// level payload, same shape as the body of a serial frame.
const (
	BLEServiceUUID = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
	BLECharRxUUID  = "6e400002-b5a3-f393-e0a9-e50e24dcca9e" // host → device
	BLECharTxUUID  = "6e400003-b5a3-f393-e0a9-e50e24dcca9e" // device → host (notify)
)

// DefaultBLEScanDuration is how long ScanBLE runs before returning
// the discovered devices. Long enough to catch a sleepy advert
// rotation; short enough that a typo at the dialog doesn't make the
// operator wait visibly.
const DefaultBLEScanDuration = 5 * time.Second

// DiscoveredBLEDevice is one result from ScanBLE — the address used
// to reconnect via OpenBLE plus a human-friendly name (when the
// peripheral broadcasts one in its advert).
type DiscoveredBLEDevice struct {
	Address string
	Name    string
	RSSI    int16
}

// bleAdapter is the package-wide singleton bluetooth.DefaultAdapter,
// guarded so multiple Open / Scan calls don't trip the "already
// enabled" path on the underlying OS API. Most BLE backends maintain
// global state internally; tinygo's API enforces the same pattern.
var (
	bleAdapterOnce sync.Once
	bleAdapterErr  error
)

func enableBLEAdapter() error {
	bleAdapterOnce.Do(func() {
		bleAdapterErr = bluetooth.DefaultAdapter.Enable()
	})
	return bleAdapterErr
}

// ScanBLE scans for advertising peripherals exposing the MeshCore
// service UUID and returns the deduped list at the end of the scan
// window. tinygo's API doesn't (yet) support a service-UUID filter
// at scan-start, so we filter manually inside the callback.
//
// duration is clamped to a 1-second floor to avoid degenerate empty
// scans; pass 0 to use DefaultBLEScanDuration.
func ScanBLE(duration time.Duration) ([]DiscoveredBLEDevice, error) {
	if duration <= 0 {
		duration = DefaultBLEScanDuration
	}
	if duration < time.Second {
		duration = time.Second
	}
	if err := enableBLEAdapter(); err != nil {
		return nil, fmt.Errorf("meshcore: enable BLE: %w", err)
	}
	wantService, err := bluetooth.ParseUUID(BLEServiceUUID)
	if err != nil {
		return nil, fmt.Errorf("meshcore: parse service UUID: %w", err)
	}
	seen := map[string]DiscoveredBLEDevice{}
	var seenMu sync.Mutex
	scanErrCh := make(chan error, 1)
	go func() {
		scanErrCh <- bluetooth.DefaultAdapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
			if !res.AdvertisementPayload.HasServiceUUID(wantService) {
				return
			}
			addr := res.Address.String()
			seenMu.Lock()
			defer seenMu.Unlock()
			if _, ok := seen[addr]; ok {
				return
			}
			seen[addr] = DiscoveredBLEDevice{
				Address: addr,
				Name:    res.LocalName(),
				RSSI:    res.RSSI,
			}
		})
	}()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case err := <-scanErrCh:
		// Scan returned before timeout — surface the error if any.
		if err != nil {
			return nil, fmt.Errorf("meshcore: scan: %w", err)
		}
	case <-timer.C:
	}
	if err := bluetooth.DefaultAdapter.StopScan(); err != nil {
		// Best-effort — StopScan can race with a self-completing Scan.
		_ = err
	}
	// Drain the goroutine if it's still pending.
	select {
	case <-scanErrCh:
	case <-time.After(time.Second):
	}
	seenMu.Lock()
	out := make([]DiscoveredBLEDevice, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	seenMu.Unlock()
	return out, nil
}

// bleTransport speaks the companion-radio protocol over BLE GATT.
// Holds the connected peripheral plus the RX (write) and TX (notify)
// characteristics. Send writes to RX; notifications on TX feed recv.
type bleTransport struct {
	device  *bluetooth.Device
	rx      bluetooth.DeviceCharacteristic
	tx      bluetooth.DeviceCharacteristic
	recv    chan []byte
	closing chan struct{}
	closed  bool
	closeMu sync.Mutex
}

// newBLETransport connects to the peripheral identified by address
// (the value returned by DiscoveredBLEDevice.Address or a previously-
// saved Address.String()), discovers the MeshCore service /
// characteristics, and subscribes to TX notifications.
//
// connectTimeout caps the scan-and-connect step; the underlying
// Connect call itself blocks until the host BLE stack returns, so we
// can't hard-cancel it without restarting the adapter. Pass 0 to use
// DefaultBLEScanDuration as the find-the-peripheral budget.
func newBLETransport(address string, connectTimeout time.Duration) (*bleTransport, error) {
	if err := enableBLEAdapter(); err != nil {
		return nil, fmt.Errorf("meshcore: enable BLE: %w", err)
	}
	if connectTimeout <= 0 {
		connectTimeout = DefaultBLEScanDuration
	}
	addr, err := findPeripheral(address, connectTimeout)
	if err != nil {
		return nil, err
	}
	device, err := bluetooth.DefaultAdapter.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("meshcore: BLE connect %s: %w", address, err)
	}
	svcUUID, _ := bluetooth.ParseUUID(BLEServiceUUID)
	rxUUID, _ := bluetooth.ParseUUID(BLECharRxUUID)
	txUUID, _ := bluetooth.ParseUUID(BLECharTxUUID)
	services, err := device.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil || len(services) == 0 {
		_ = device.Disconnect()
		return nil, fmt.Errorf("meshcore: BLE service %s not found: %w", BLEServiceUUID, err)
	}
	chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{rxUUID, txUUID})
	if err != nil || len(chars) < 2 {
		_ = device.Disconnect()
		return nil, fmt.Errorf("meshcore: BLE characteristics not found: %w", err)
	}
	var rxChar, txChar bluetooth.DeviceCharacteristic
	for _, ch := range chars {
		switch ch.UUID() {
		case rxUUID:
			rxChar = ch
		case txUUID:
			txChar = ch
		}
	}
	if rxChar.UUID() != rxUUID || txChar.UUID() != txUUID {
		_ = device.Disconnect()
		return nil, errors.New("meshcore: missing RX or TX characteristic")
	}
	t := &bleTransport{
		device:  &device,
		rx:      rxChar,
		tx:      txChar,
		recv:    make(chan []byte, 32),
		closing: make(chan struct{}),
	}
	if err := txChar.EnableNotifications(t.onNotification); err != nil {
		_ = device.Disconnect()
		return nil, fmt.Errorf("meshcore: enable BLE notifications: %w", err)
	}
	return t, nil
}

// findPeripheral runs a fresh scan looking for the address we want.
// tinygo's API doesn't expose a "connect by address without scanning
// first" path on every backend, so the scan-and-match dance is the
// portable approach.
func findPeripheral(address string, timeout time.Duration) (bluetooth.Address, error) {
	matchCh := make(chan bluetooth.Address, 1)
	scanDone := make(chan struct{})
	go func() {
		_ = bluetooth.DefaultAdapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
			if res.Address.String() != address {
				return
			}
			select {
			case matchCh <- res.Address:
				_ = bluetooth.DefaultAdapter.StopScan()
			default:
			}
		})
		close(scanDone)
	}()
	select {
	case addr := <-matchCh:
		<-scanDone
		return addr, nil
	case <-time.After(timeout):
		_ = bluetooth.DefaultAdapter.StopScan()
		<-scanDone
		return bluetooth.Address{}, fmt.Errorf("meshcore: BLE peripheral %s not found in %s", address, timeout)
	}
}

func (t *bleTransport) onNotification(buf []byte) {
	if len(buf) == 0 {
		return
	}
	// Copy out — tinygo reuses the underlying buffer between calls.
	cp := make([]byte, len(buf))
	copy(cp, buf)
	select {
	case t.recv <- cp:
	case <-t.closing:
	}
}

func (t *bleTransport) Send(payload []byte) error {
	// Use WriteWithoutResponse for throughput — the firmware doesn't
	// gate on the link-layer ack and Write (with response) round-trips
	// every packet, doubling latency. If a packet is too large for the
	// negotiated MTU the OS returns an error and we fall back to a
	// regular Write so the operator at least sees a single failed-send
	// rather than a silent drop.
	if _, err := t.rx.WriteWithoutResponse(payload); err != nil {
		if _, err2 := t.rx.Write(payload); err2 != nil {
			return fmt.Errorf("meshcore: BLE write: %w", err2)
		}
	}
	return nil
}

func (t *bleTransport) Receive() <-chan []byte { return t.recv }

func (t *bleTransport) Close() error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closing)
	t.closeMu.Unlock()
	var err error
	if t.device != nil {
		err = t.device.Disconnect()
	}
	close(t.recv)
	return err
}
