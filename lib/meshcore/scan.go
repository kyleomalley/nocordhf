// Package meshcore provides device discovery + (eventually) the
// companion-mode wire protocol for USB-attached LoRa radios running
// MeshCore firmware. v1.1.0 ships the device-selection scaffolding
// only — the protocol layer lands incrementally on top.
package meshcore

import (
	"path/filepath"
	"strings"

	"go.bug.st/serial"
)

// DefaultBaud is the companion-mode serial baud rate used by stock
// MeshCore firmware. Matches every officially-supported devboard.
const DefaultBaud = 115200

// KnownBoard describes a USB-attached LoRa devboard known to run
// MeshCore companion firmware. Type is the stable identifier
// persisted in fyne.Preferences; Name is the GUI label.
type KnownBoard struct {
	Name string // display name shown in Settings
	Type string // stable identifier persisted across launches
	Baud int    // default serial baud
	// USBHint is a substring matched against the lower-cased serial
	// device basename to prefer this board when auto-detecting. Empty
	// means "no hint" — the board only shows up via manual selection.
	USBHint string
}

// KnownBoards is the ordered list of MeshCore-firmware boards offered
// in the Settings → MeshCore tab. Generic entries cover unrecognised
// adapters so an unlisted board is still usable.
var KnownBoards = []KnownBoard{
	{Name: "LilyGO T1000-E (USB CDC)", Type: "t1000_e", Baud: DefaultBaud, USBHint: "usbmodem"},
	{Name: "Heltec WiFi LoRa 32 V3 (CP210x)", Type: "heltec_v3", Baud: DefaultBaud, USBHint: "slab"},
	{Name: "LilyGO T-Beam (CP210x)", Type: "tbeam", Baud: DefaultBaud, USBHint: "slab"},
	{Name: "LilyGO T-Deck (CH9102)", Type: "tdeck", Baud: DefaultBaud, USBHint: "wch"},
	{Name: "LilyGO T-Echo (USB CDC)", Type: "techo", Baud: DefaultBaud, USBHint: "usbmodem"},
	{Name: "RAK4631 / RAK19007 (CH340)", Type: "rak4631", Baud: DefaultBaud, USBHint: "wch"},
	{Name: "Adafruit Feather nRF52840 + LoRa (USB CDC)", Type: "feather_nrf52840", Baud: DefaultBaud, USBHint: "usbmodem"},
	{Name: "Seeed XIAO nRF52840 + LoRa (USB CDC)", Type: "xiao_nrf52840", Baud: DefaultBaud, USBHint: "usbmodem"},
	{Name: "Generic USB LoRa (MeshCore firmware)", Type: "generic", Baud: DefaultBaud},
}

// BoardNames returns the display names of every KnownBoard, in order.
func BoardNames() []string {
	names := make([]string, len(KnownBoards))
	for i, b := range KnownBoards {
		names[i] = b.Name
	}
	return names
}

// BoardByName returns the KnownBoard matching a display name.
func BoardByName(name string) (KnownBoard, bool) {
	for _, b := range KnownBoards {
		if b.Name == name {
			return b, true
		}
	}
	return KnownBoard{}, false
}

// BoardByType returns the KnownBoard matching a persisted type string.
func BoardByType(t string) (KnownBoard, bool) {
	for _, b := range KnownBoards {
		if b.Type == t {
			return b, true
		}
	}
	return KnownBoard{}, false
}

// ScanPorts returns every available serial port device path. Returns
// nil on error so callers can render "no ports found" without
// distinguishing failure modes.
func ScanPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil
	}
	return ports
}

// IsLikelyMeshCorePort applies a lightweight basename heuristic to
// decide whether a serial port is plausibly a USB LoRa devboard. We
// admit every USB-CDC / USB-serial flavour MeshCore firmware ships on
// (CP210x, CH340/CH9102, FTDI, native nRF52840 CDC) and exclude the
// usual non-radio noise (Bluetooth pairing pseudo-ports, debug
// consoles, Wi-Fi modems). Used by the Settings tab to filter the
// dropdown so the operator isn't picking through dozens of irrelevant
// devices on a busy macOS machine.
func IsLikelyMeshCorePort(port string) bool {
	base := strings.ToLower(filepath.Base(port))
	for _, skip := range []string{"bluetooth", "debug-console", "wlan"} {
		if strings.Contains(base, skip) {
			return false
		}
	}
	for _, hint := range []string{"usbmodem", "usbserial", "slab", "wch", "serial"} {
		if strings.Contains(base, hint) {
			return true
		}
	}
	return false
}
