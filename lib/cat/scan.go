package cat

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

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
//
// Strategy:
//  1. For each likely port, try the heuristic-preferred type first
//     (e.g. SLAB_* → Icom, usbserial-* → Yaesu) with a long enough
//     probe timeout to give cold-key-up rigs (notably the FT-891 via
//     DigiRig) time to reply.
//  2. If the preferred type doesn't reply, fall through and try the
//     OTHER known types on the same port — heuristics get the easy
//     cases right but mislabel adapters often enough that a fallback
//     pass is worth the extra few seconds on first launch.
//
// The probe timeout is intentionally generous (2s) because the cost
// of false-negative is the operator having to manually pick the
// radio in Settings, while the cost of false-positive (declaring a
// non-radio adapter as a radio) is way worse — sends bogus CAT
// commands at the operator's actual radio if it shares a USB hub.
func AutoDetect() (AutoDetectResult, error) {
	const probeTimeout = 2 * time.Second
	tryOpen := func(port string, kr KnownRadio) (Radio, error) {
		switch kr.Type {
		case "yaesu":
			return OpenYaesu(port, kr.Baud)
		default:
			return OpenIcom(port, kr.Baud)
		}
	}
	tryPort := func(port string, kr KnownRadio) (Radio, bool) {
		r, err := tryOpen(port, kr)
		if err != nil {
			return nil, false
		}
		if probe, ok := r.(interface {
			VerifyResponse(time.Duration) error
		}); ok {
			if perr := probe.VerifyResponse(probeTimeout); perr != nil {
				_ = r.Close()
				return nil, false
			}
		}
		return r, true
	}

	ports := ScanPorts()
	for _, port := range ports {
		if !isLikelyRadio(port) {
			continue
		}
		preferred := preferredType(port)
		// Pass 1: heuristic-preferred type first.
		for _, kr := range KnownRadios {
			if preferred != "" && kr.Type != preferred {
				continue
			}
			if r, ok := tryPort(port, kr); ok {
				return AutoDetectResult{Radio: r, Name: kr.Name, Port: port}, nil
			}
		}
		// Pass 2: fall back to every other type the heuristic didn't
		// suggest. Catches the case where (for example) the DigiRig's
		// FTDI adapter enumerates with an unexpected name and the
		// preferredType heuristic guessed wrong.
		for _, kr := range KnownRadios {
			if preferred != "" && kr.Type == preferred {
				continue // already tried in pass 1
			}
			if r, ok := tryPort(port, kr); ok {
				return AutoDetectResult{Radio: r, Name: kr.Name, Port: port}, nil
			}
		}
	}
	return AutoDetectResult{}, fmt.Errorf("no radio detected — connect a radio and use the CAT button to configure")
}

// OpenByType opens a radio of the named type on the given port and baud.
// Type strings match KnownRadio.Type ("icom", "yaesu"). Used by the saved-
// profile path in main.go and the Radio settings tab so a known config can
// be re-opened without rerunning AutoDetect.
func OpenByType(t, port string, baud int) (Radio, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "icom":
		return OpenIcom(port, baud)
	case "yaesu":
		return OpenYaesu(port, baud)
	default:
		return nil, fmt.Errorf("unknown radio type %q", t)
	}
}
