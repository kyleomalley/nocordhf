package cat

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.bug.st/serial"
)

// KnownRadio describes a supported radio model.
type KnownRadio struct {
	Name string // display name shown in the GUI
	Type string // "icom" or "yaesu" — matches the -radio flag
	Baud int    // default CAT baud rate
}

// KnownRadios is the ordered list of supported radio models.
var KnownRadios = []KnownRadio{
	{Name: "IC-7300 (Icom)", Type: "icom", Baud: DefaultBaud},
	{Name: "FT-891 + DigiRig (Yaesu)", Type: "yaesu", Baud: YaesuDefaultBaud},
}

// RadioTypeNames returns the display names of all KnownRadios.
func RadioTypeNames() []string {
	names := make([]string, len(KnownRadios))
	for i, r := range KnownRadios {
		names[i] = r.Name
	}
	return names
}

// RadioByName returns the KnownRadio matching the display name, or the zero value.
func RadioByName(name string) (KnownRadio, bool) {
	for _, r := range KnownRadios {
		if r.Name == name {
			return r, true
		}
	}
	return KnownRadio{}, false
}

// RadioByType returns the KnownRadio matching the type string, or the zero value.
func RadioByType(t string) (KnownRadio, bool) {
	for _, r := range KnownRadios {
		if r.Type == t {
			return r, true
		}
	}
	return KnownRadio{}, false
}

// ScanPorts returns the list of available serial port device paths.
// Returns nil on error (e.g. no ports or permission denied).
func ScanPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil
	}
	return ports
}

// preferredType returns the most likely radio type for a port based on its
// device name. Returns "" if the port name gives no useful signal.
func preferredType(port string) string {
	base := strings.ToLower(filepath.Base(port))
	if strings.Contains(base, "slab") {
		return "icom" // Silicon Labs CP210x — IC-7300 built-in USB
	}
	if strings.Contains(base, "usbserial") {
		return "yaesu" // generic USB serial — DigiRig / FT-891
	}
	return ""
}

// isLikelyRadio returns true if the port name looks like a radio CAT port
// rather than a Bluetooth, audio, or debug device.
func isLikelyRadio(port string) bool {
	base := strings.ToLower(filepath.Base(port))
	for _, skip := range []string{"bluetooth", "debug-console", "wlan", "usbmodem"} {
		if strings.Contains(base, skip) {
			return false
		}
	}
	return strings.Contains(base, "slab") ||
		strings.Contains(base, "usbserial") ||
		strings.Contains(base, "serial")
}

// AutoDetectResult holds the result of a successful auto-detect.
type AutoDetectResult struct {
	Radio Radio
	Name  string // KnownRadio display name
	Port  string // serial port path
}

// AutoDetect scans available serial ports and returns the first radio it can
// open. It uses port-name heuristics to try the most likely radio type first:
//   - tty.SLAB_* → IC-7300 (Silicon Labs CP210x)
//   - tty.usbserial-* → FT-891 + DigiRig
//
// Returns an error if no radio is found on any port.
func AutoDetect() (AutoDetectResult, error) {
	ports := ScanPorts()
	for _, port := range ports {
		if !isLikelyRadio(port) {
			continue
		}
		preferred := preferredType(port)
		for _, kr := range KnownRadios {
			// If we have a heuristic match, skip non-matching types.
			if preferred != "" && kr.Type != preferred {
				continue
			}
			var r Radio
			var err error
			switch kr.Type {
			case "yaesu":
				r, err = OpenYaesu(port, kr.Baud)
			default:
				r, err = OpenIcom(port, kr.Baud)
			}
			if err == nil {
				return AutoDetectResult{Radio: r, Name: kr.Name, Port: port}, nil
			}
		}
	}
	return AutoDetectResult{}, fmt.Errorf("no radio detected — connect a radio and use the CAT button to configure")
}
