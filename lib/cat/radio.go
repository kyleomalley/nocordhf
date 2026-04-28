// Package cat provides CAT (Computer Aided Transceiver) control for
// amateur radios. Supported protocols:
//   - Icom CI-V (IC-7300)
//   - Yaesu NewCAT (FT-891)
package cat

// Radio is the interface for radio CAT control.
// Implementations handle different radio models and protocols.
type Radio interface {
	// Frequency returns the current VFO frequency in Hz.
	// Returns 0 if no frequency has been received yet.
	Frequency() uint64

	// SetFrequency sets the VFO to the specified frequency in Hz.
	SetFrequency(freq uint64) error

	// PTTOn keys the transmitter.
	PTTOn() error

	// PTTOff unkeys the transmitter.
	PTTOff() error

	// SplitOn enables split TX/RX mode.
	SplitOn() error

	// SplitOff disables split TX/RX mode.
	SplitOff() error

	// ReadMeters queries the radio for current meter readings.
	// Results are accessed via SMeter/Power/SWR/ALC.
	ReadMeters()

	// SMeter returns the last S-meter reading (0–255).
	SMeter() uint32

	// Power returns the last RF power output reading (0–255).
	Power() uint32

	// SWR returns the last SWR reading (0–255).
	SWR() uint32

	// ALC returns the last ALC reading (0–255).
	ALC() uint32

	// Close stops background operations and closes the connection.
	Close() error
}
