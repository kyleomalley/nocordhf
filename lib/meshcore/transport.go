package meshcore

// Transport is the link-layer abstraction over which Client speaks
// the companion-radio protocol. Two implementations ship today:
// serialTransport (USB / UART, with `<` 0x3c / `>` 0x3e framing) and
// bleTransport (Nordic UART Service GATT, where each characteristic
// notification IS one frame).
//
// Implementations move application-level payloads — the same bytes
// the firmware sees as the body of a companion-protocol message,
// starting with a CommandCode (host → radio) or a ResponseCode /
// PushCode (radio → host). Any per-transport framing is hidden.
type Transport interface {
	// Send writes one application-level payload to the device.
	// Implementations apply whatever framing the transport requires.
	Send(payload []byte) error

	// Receive returns a stream of application-level payloads received
	// from the device. The channel closes when the transport closes,
	// signalling Client.readLoop to emit EventDisconnected and exit.
	Receive() <-chan []byte

	// Close releases the transport's resources. Idempotent — Client
	// may call Close from multiple paths (operator disconnect, read
	// error, app shutdown) and each must be safe.
	Close() error
}
