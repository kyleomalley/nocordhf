//go:build darwin

package meshcore

import "tinygo.org/x/bluetooth"

// bleDeviceLooksZero reports whether a freshly-returned Device value
// is the all-zero "phantom" the macOS CoreBluetooth backend
// occasionally hands back from Connect (no error reported, but the
// per-peripheral CBPeripheral handle never resolved). Calling
// DiscoverServices on such a Device dereferences a nil internal
// pointer inside tinygo's gattc_darwin.go.
//
// macOS Address is a single UUID, so the zero check reduces to
// "is the UUID all zeros". Real peripherals always carry a non-
// zero CoreBluetooth UUID after Connect.
func bleDeviceLooksZero(d bluetooth.Device) bool {
	return d.Address.UUID == bluetooth.UUID{}
}
