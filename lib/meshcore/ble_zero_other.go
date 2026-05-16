//go:build !darwin

package meshcore

import "tinygo.org/x/bluetooth"

// bleDeviceLooksZero is the non-darwin stub. The "Connect returned
// a zero Device" path is specific to tinygo's CoreBluetooth backend
// — the Linux (BlueZ DBus) and Windows (WinRT) backends report
// connection failures through the err return cleanly.
//
// Returns false unconditionally; the panic-recover in
// newBLETransport remains the safety net for anything weird that
// surfaces in a future tinygo release on these platforms.
//
// (Whole-Device == zero comparison isn't an option here — the
// Linux Device struct embeds a dbus.BusObject interface which makes
// the type non-comparable, so the static check has to be platform-
// gated either way.)
func bleDeviceLooksZero(d bluetooth.Device) bool {
	return false
}
