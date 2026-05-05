package meshcore

// Client is the high-level companion-radio interface: drive
// request/response calls and stream asynchronous push events. One
// Client per physical device. The reference implementation we mirror
// is liamcottle/meshcore.js (src/connection/connection.js); the wire
// protocol is documented in the firmware at meshcore-dev/MeshCore.
//
// The Client is transport-agnostic: it operates on application-level
// payloads and lets a Transport deal with whatever framing the link
// requires. OpenSerial wraps a USB / UART connection (`<` 0x3c / `>`
// 0x3e framing); OpenBLE wraps a Nordic-UART-Service GATT connection
// (each characteristic notification IS one frame).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultRequestTimeout caps how long a single request waits for the
// matching response(s) before returning context.DeadlineExceeded.
// Bumped to 30 s after observing real-world BLE congestion at ~10 s
// per round-trip on long-running sessions where the firmware was
// thrashing on a full contacts table — the previous 10 s value
// caused legitimate sends to fail "deadline exceeded" while the
// radio was just slow, not wedged. Truly wedged radios will still
// surface within 30 s rather than hanging the GUI forever.
const DefaultRequestTimeout = 30 * time.Second

// Logger is the optional sink for diagnostic logs. Matches the
// Sugar-style key/value variadics used by lib/logging so the GUI can
// pass its existing *zap.SugaredLogger directly. Implementations that
// don't care can fall back to NopLogger (the default).
type Logger interface {
	Debugw(msg string, keysAndValues ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
}

// NopLogger is a Logger that drops everything. Used as the default
// so callers that don't set a logger don't dereference nil.
type NopLogger struct{}

func (NopLogger) Debugw(string, ...interface{}) {}
func (NopLogger) Infow(string, ...interface{})  {}
func (NopLogger) Warnw(string, ...interface{})  {}

// Client owns a single MeshCore companion-firmware device.
type Client struct {
	transport Transport

	// callMu serialises outbound commands so only one waiter is
	// registered at a time. The reader dispatches sync responses to
	// the active waiter via respCh; pushes go to events independently.
	callMu sync.Mutex
	respMu sync.Mutex
	respCh chan Frame

	events  chan Event
	closing chan struct{}
	closed  bool
	closeMu sync.Mutex

	// log is the diagnostic sink. Default NopLogger; replaced via
	// SetLogger by the GUI to pipe meshcore tx/rx/push events into
	// the project's nocordhf.log when -debug is set.
	logMu sync.RWMutex
	log   Logger
}

// OpenSerial opens a USB / UART connection to a MeshCore companion-
// firmware device, starts the read loop, and returns a Client ready
// for AppStart / GetContacts / GetChannels / Send* calls.
//
// The caller must call Close exactly once when done — failing to do
// so leaks the read goroutine and the OS serial handle.
func OpenSerial(portPath string, baud int) (*Client, error) {
	t, err := newSerialTransport(portPath, baud)
	if err != nil {
		return nil, err
	}
	return newClient(t), nil
}

// OpenBLE connects to a MeshCore companion device over BLE GATT,
// starts the read loop, and returns a Client. address comes from
// ScanBLE (or a previously-saved string from Address.String()) — on
// macOS this is a CoreBluetooth peripheral UUID, on Linux the MAC.
//
// The caller must call Close exactly once when done.
func OpenBLE(address string, timeout time.Duration) (*Client, error) {
	t, err := newBLETransport(address, timeout)
	if err != nil {
		return nil, err
	}
	return newClient(t), nil
}

// newClient wires a Transport into the dispatcher. Used by OpenSerial,
// OpenBLE, and tests that want to inject a mock transport.
func newClient(t Transport) *Client {
	c := &Client{
		transport: t,
		events:    make(chan Event, 64),
		closing:   make(chan struct{}),
		log:       NopLogger{},
	}
	go c.readLoop()
	return c
}

// SetLogger swaps the diagnostic sink. Safe to call from any goroutine
// at any time. Pass NopLogger{} to silence.
func (c *Client) SetLogger(l Logger) {
	if l == nil {
		l = NopLogger{}
	}
	c.logMu.Lock()
	c.log = l
	c.logMu.Unlock()
}

func (c *Client) logger() Logger {
	c.logMu.RLock()
	defer c.logMu.RUnlock()
	return c.log
}

// Events returns the asynchronous event stream — adverts, send
// confirmations, msg-waiting pushes, decoded contact/channel
// messages, and the final EventDisconnected when the Client closes.
// The channel buffers 64 events; when full, oldest pushes are
// dropped silently to keep the reader responsive.
func (c *Client) Events() <-chan Event { return c.events }

// Close stops the read goroutine and releases the serial port.
// Idempotent; safe to call from any goroutine.
func (c *Client) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closing)
	c.closeMu.Unlock()
	return c.transport.Close()
}

// ─── Read loop ────────────────────────────────────────────────────

func (c *Client) readLoop() {
	var lastErr error
	defer func() {
		c.emit(EventDisconnected{Err: lastErr})
		close(c.events)
	}()
	recv := c.transport.Receive()
	for {
		select {
		case <-c.closing:
			return
		case payload, ok := <-recv:
			if !ok {
				// Transport closed underneath us — usually because
				// the operator unplugged the device or the BLE link
				// dropped. Surface as a clean disconnect; the
				// upstream Client.Close path covers explicit shutdown.
				return
			}
			c.dispatch(payload)
		}
	}
}

// dispatch routes one application-level payload to the active waiter
// (sync responses 0x00–0x7F) or to the events stream (pushes 0x80+).
// Drops sync responses with no waiter and async events with no consumer.
func (c *Client) dispatch(payload []byte) {
	if len(payload) == 0 {
		return
	}
	code := payload[0]
	c.logger().Debugw("meshcore rx", "code", fmt.Sprintf("0x%02x", code), "len", len(payload))
	if code >= 0x80 {
		c.handlePush(PushCode(code), payload[1:])
		return
	}
	c.respMu.Lock()
	ch := c.respCh
	c.respMu.Unlock()
	if ch == nil {
		// No active waiter — drop. This is normal when the firmware
		// emits a stray Ok / Err during reconnect or after a
		// timed-out call; we can't usefully attribute it anywhere.
		return
	}
	// Wrap the payload as a Frame for the existing handler signature.
	// Marker is always FrameIncoming on the dispatch path so handlers
	// don't need to inspect it; the value is here only for symmetry
	// with the wire-level abstraction.
	frame := Frame{Marker: FrameIncoming, Payload: payload}
	select {
	case ch <- frame:
	default:
		// Waiter buffer full — should not happen with our buffer
		// sizing but drop rather than block the reader.
	}
}

func (c *Client) handlePush(code PushCode, payload []byte) {
	c.logger().Debugw("meshcore push", "code", fmt.Sprintf("0x%02x", byte(code)), "len", len(payload))
	switch code {
	case PushAdvert:
		if len(payload) >= 32 {
			ev := EventAdvert{}
			copy(ev.PublicKey[:], payload[:32])
			c.emit(ev)
		}
	case PushNewAdvert:
		if len(payload) >= 32 {
			ev := EventAdvert{Manual: true}
			copy(ev.PublicKey[:], payload[:32])
			c.emit(ev)
		}
	case PushPathUpdated:
		if len(payload) >= 32 {
			ev := EventPathUpdated{}
			copy(ev.PublicKey[:], payload[:32])
			c.emit(ev)
		}
	case PushSendConfirmed:
		if len(payload) >= 8 {
			c.emit(EventSendConfirmed{
				AckCode:     binary.LittleEndian.Uint32(payload[0:4]),
				RoundTripMs: binary.LittleEndian.Uint32(payload[4:8]),
			})
		}
	case PushMsgWaiting:
		c.emit(EventMsgWaiting{})
	case PushContactsFull:
		c.emit(EventContactsFull{})
	case PushContactDeleted:
		if len(payload) >= 32 {
			ev := EventContactDeleted{}
			copy(ev.PublicKey[:], payload[:32])
			c.emit(ev)
		}
	case PushLogRxData:
		// Layout: int8 lastSnr (firmware units, /4 to dBm) +
		// int8 lastRssi + remaining bytes = the raw mesh packet.
		// Web client parses the same way (see RxLogPage.vue).
		if len(payload) < 2 {
			return
		}
		ev := EventRxLog{
			SNR:  float64(int8(payload[0])) / 4,
			RSSI: int(int8(payload[1])),
		}
		raw := payload[2:]
		ev.Raw = make([]byte, len(raw))
		copy(ev.Raw, raw)
		if pkt, err := PacketFromBytes(raw); err == nil {
			ev.Packet = pkt
		}
		c.emit(ev)
	default:
		// Other pushes (RawData, LoginSuccess, StatusResponse, …)
		// aren't surfaced yet. The chat-focused MVP only needs the
		// codes above; later UI can grow more event types without
		// changing this dispatcher.
	}
}

func (c *Client) emit(ev Event) {
	select {
	case c.events <- ev:
	default:
		// Drop silently — see Events doc comment.
	}
}

// ─── Request helpers ──────────────────────────────────────────────

// call sends one command frame and invokes handler for each response
// frame the reader delivers, until handler returns done=true or the
// context expires. Holds callMu so only one in-flight call exists at
// a time; the reader is allowed to keep running and routing pushes
// independently.
func (c *Client) call(ctx context.Context, payload []byte, handler func(Frame) (done bool, err error)) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	ch := make(chan Frame, 32)
	c.respMu.Lock()
	c.respCh = ch
	c.respMu.Unlock()
	defer func() {
		c.respMu.Lock()
		c.respCh = nil
		c.respMu.Unlock()
	}()
	cmdCode := byte(0)
	if len(payload) > 0 {
		cmdCode = payload[0]
	}
	c.logger().Debugw("meshcore tx", "cmd", fmt.Sprintf("0x%02x", cmdCode), "len", len(payload))
	if err := c.transport.Send(payload); err != nil {
		c.logger().Warnw("meshcore tx failed", "cmd", fmt.Sprintf("0x%02x", cmdCode), "err", err)
		// Transport write failures mean the link is dead — common
		// after a macOS sleep/wake cycle on BLE, since the OS tears
		// down the GATT connection without our driver noticing
		// until the next write. Close the transport here so the
		// read loop's Receive channel closes, the readLoop exits,
		// and EventDisconnected is emitted exactly once. Without
		// this the GUI would sit on a stale "connected" status
		// forever and every subsequent send would also time out.
		_ = c.transport.Close()
		return fmt.Errorf("meshcore: write: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closing:
			return errors.New("meshcore: client closed")
		case frame := <-ch:
			done, err := handler(frame)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// callWithTimeout wraps call with the package's default timeout when
// the caller doesn't have a context to thread.
func (c *Client) callWithTimeout(payload []byte, handler func(Frame) (bool, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultRequestTimeout)
	defer cancel()
	return c.call(ctx, payload, handler)
}

// errFromFrame returns a typed error if the frame is a RespErr or
// RespDisabled. Used by handlers that share the same exit logic.
// RespErr's sub-code byte is decoded into a human-readable label
// (TABLE_FULL, NOT_FOUND, etc.) so callers can surface useful UX
// messages without manually translating each code.
func errFromFrame(frame Frame) error {
	if len(frame.Payload) == 0 {
		return ErrShortPayload
	}
	switch ResponseCode(frame.Payload[0]) {
	case RespErr:
		sub := byte(0)
		if len(frame.Payload) >= 2 {
			sub = frame.Payload[1]
		}
		return fmt.Errorf("meshcore: device returned Err (%s)", errCodeLabel(sub))
	case RespDisabled:
		return errors.New("meshcore: command disabled by device")
	}
	return nil
}

func errCodeLabel(sub byte) string {
	switch ErrCode(sub) {
	case ErrUnsupportedCmd:
		return "unsupported command"
	case ErrNotFound:
		return "not found"
	case ErrTableFull:
		return "storage full — remove an entry first"
	case ErrBadState:
		return "bad state"
	case ErrFileIO:
		return "file I/O error"
	case ErrIllegalArg:
		return "illegal argument"
	default:
		return fmt.Sprintf("code=0x%02x", sub)
	}
}

// ─── High-level commands ──────────────────────────────────────────

// AppStart issues the protocol handshake. Most companion devices won't
// respond to anything else until this completes. appName is opaque —
// the firmware logs it but doesn't gate behaviour on it.
func (c *Client) AppStart(appName string) (SelfInfo, error) {
	payload := make([]byte, 0, 16+len(appName))
	payload = append(payload, byte(CmdAppStart))
	payload = append(payload, byte(SupportedCompanionProtocolVersion))
	payload = append(payload, make([]byte, 6)...) // reserved
	payload = append(payload, []byte(appName)...)
	var info SelfInfo
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		if ResponseCode(frame.Payload[0]) != RespSelfInfo {
			return false, nil
		}
		parsed, err := parseSelfInfo(frame.Payload[1:])
		if err != nil {
			return true, err
		}
		info = parsed
		return true, nil
	})
	return info, err
}

// GetContacts streams the full contact table from the radio. Returns
// an empty slice (not an error) when the table is empty.
func (c *Client) GetContacts() ([]Contact, error) {
	return c.GetContactsSince(time.Time{})
}

// GetContactsSince streams the subset of the contact table that has
// changed since the given timestamp. Pass a zero `since` to dump
// everything (the firmware reads "no since field" as "give me all
// contacts"). The companion firmware tracks each contact's LastMod;
// we use the largest LastMod observed in a previous fetch as the
// next `since` value, turning what would otherwise be repeated
// full-table dumps into incremental deltas.
func (c *Client) GetContactsSince(since time.Time) ([]Contact, error) {
	payload := []byte{byte(CmdGetContacts)}
	if !since.IsZero() {
		payload = appendUint32LE(payload, uint32(since.Unix()))
	}
	var contacts []Contact
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		switch ResponseCode(frame.Payload[0]) {
		case RespContactsStart:
			return false, nil
		case RespContact:
			ct, err := parseContact(frame.Payload[1:])
			if err != nil {
				return false, nil
			}
			contacts = append(contacts, ct)
			return false, nil
		case RespEndOfContacts:
			return true, nil
		}
		return false, nil
	})
	return contacts, err
}

// GetChannel fetches one channel slot by index. Returns a typed error
// when the index isn't configured (firmware replies Err) so callers
// can iterate idx=0,1,… and stop on the first not-found.
func (c *Client) GetChannel(idx uint8) (Channel, error) {
	payload := []byte{byte(CmdGetChannel), idx}
	var ch Channel
	var found bool
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		if ResponseCode(frame.Payload[0]) != RespChannelInfo {
			return false, nil
		}
		parsed, err := parseChannelInfo(frame.Payload[1:])
		if err != nil {
			return true, err
		}
		ch = parsed
		found = true
		return true, nil
	})
	if err != nil {
		return Channel{}, err
	}
	if !found {
		return Channel{}, fmt.Errorf("meshcore: no ChannelInfo for idx=%d", idx)
	}
	return ch, nil
}

// GetChannels iterates GetChannel from idx=0 upward up to maxIdx and
// returns every CONFIGURED slot. The companion firmware happily
// answers GetChannel for every slot in its table — empty slots return
// a record with an all-zero secret + a placeholder name like "ch3".
// Filtering on the secret keeps the sidebar limited to channels the
// operator has actually set up.
func (c *Client) GetChannels(maxIdx uint8) ([]Channel, error) {
	out := []Channel{}
	for i := uint8(0); i <= maxIdx; i++ {
		ch, err := c.GetChannel(i)
		if err != nil {
			// Real RespErr from the firmware = no more slots
			// addressable. Stop iterating; downstream slots
			// wouldn't be reachable anyway.
			break
		}
		if !channelConfigured(ch) {
			c.logger().Debugw("meshcore channel skipped (unconfigured)", "idx", i, "name", ch.Name)
			if i == 255 {
				break
			}
			continue
		}
		out = append(out, ch)
		if i == 255 {
			break // avoid uint8 overflow on a fully-populated table
		}
	}
	return out, nil
}

// channelConfigured returns true if the channel slot has a non-zero
// 16-byte secret. The firmware leaves the secret all-zeros for slots
// the operator hasn't configured (the public channel uses a
// well-known non-zero key, so it survives this filter).
func channelConfigured(ch Channel) bool {
	for _, b := range ch.Secret {
		if b != 0 {
			return true
		}
	}
	return false
}

// MaxTextLength is the conservative on-air limit (in bytes) for one
// chat message body. The actual cap depends on the radio mode (LoRa
// SF / BW / coderate) and on the encrypted-channel framing
// overhead, but 140 bytes is the value the upstream MeshCore web
// client + iOS app enforce so messages fit a single LoRa packet
// across every standard preset. Sends longer than this are silently
// dropped by the firmware (no RespOk, no error code — the cmd
// times out 30s later), so we reject pre-flight with ErrTextTooLong
// to give the operator an immediate, actionable signal.
const MaxTextLength = 140

// ErrTextTooLong is returned by SendChannelMessage / SendContactMessage
// when the message body exceeds MaxTextLength. Surfaced as a system
// line in the GUI so the operator can shorten + retry without
// waiting for the firmware-silent-drop timeout.
var ErrTextTooLong = errors.New("meshcore: message text exceeds " + itoaConst(MaxTextLength) + " bytes")

// itoaConst formats a small constant int into its decimal string at
// init time. Used to bake MaxTextLength into ErrTextTooLong's
// message without strconv (avoiding an import just for the var
// initialisation).
func itoaConst(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// SendContactMessage sends a plain text message addressed at a contact
// identified by their 6-byte pubkey prefix (use Contact.PubKey.Prefix()).
// senderTime is stamped into the frame; pass time.Now() unless you're
// resending an old draft.
func (c *Client) SendContactMessage(prefix PubKeyPrefix, senderTime time.Time, text string) (SentResult, error) {
	if len(text) > MaxTextLength {
		return SentResult{}, ErrTextTooLong
	}
	payload := make([]byte, 0, 1+1+1+4+6+len(text))
	payload = append(payload, byte(CmdSendTxtMsg))
	payload = append(payload, byte(TxtPlain))
	payload = append(payload, 0) // attempt
	payload = appendUint32LE(payload, uint32(senderTime.Unix()))
	payload = append(payload, prefix[:]...)
	payload = append(payload, []byte(text)...)
	var res SentResult
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		switch ResponseCode(frame.Payload[0]) {
		case RespSent:
			parsed, err := parseSent(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			res = parsed
			return true, nil
		case RespOk:
			// Some firmware variants ack a contact send with bare
			// Ok instead of Sent (no AckCRC body). Treat as success;
			// delivery state stays pending without a tracking key.
			return true, nil
		}
		return false, nil
	})
	return res, err
}

// SendChannelMessage sends a plain text message to the given channel
// index. The radio formats the wire payload as "name: text" so callers
// pass the bare message body without prefixing their own name.
//
// Channel sends are broadcast — the firmware acks the operator's
// command with bare RespOk (no SentResult body) because there's no
// per-destination delivery confirmation to track. The returned
// SentResult is therefore zero-valued for channel sends; the caller
// shouldn't try to correlate it with a PushSendConfirmed event.
// Older firmware variants that return RespSent are also accepted.
func (c *Client) SendChannelMessage(channelIdx uint8, senderTime time.Time, text string) (SentResult, error) {
	if len(text) > MaxTextLength {
		return SentResult{}, ErrTextTooLong
	}
	payload := make([]byte, 0, 1+1+1+4+len(text))
	payload = append(payload, byte(CmdSendChannelTxtMsg))
	payload = append(payload, byte(TxtPlain))
	payload = append(payload, channelIdx)
	payload = appendUint32LE(payload, uint32(senderTime.Unix()))
	payload = append(payload, []byte(text)...)
	var res SentResult
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		switch ResponseCode(frame.Payload[0]) {
		case RespOk:
			return true, nil
		case RespSent:
			parsed, err := parseSent(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			res = parsed
			return true, nil
		}
		return false, nil
	})
	return res, err
}

// SyncMessageResult is the discriminated union returned by
// SyncNextMessage — exactly one of Contact / Channel is populated, or
// NoMore is true to indicate the radio has no buffered messages left.
type SyncMessageResult struct {
	Contact *ContactMessage
	Channel *ChannelMessage
	NoMore  bool
}

// SyncNextMessage drains one buffered inbound message from the
// radio. Loop until NoMore=true to fully drain after a PushMsgWaiting.
func (c *Client) SyncNextMessage() (SyncMessageResult, error) {
	payload := []byte{byte(CmdSyncNextMessage)}
	var out SyncMessageResult
	err := c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		switch ResponseCode(frame.Payload[0]) {
		case RespContactMsgRecv:
			msg, err := parseContactMsg(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			out.Contact = &msg
			c.emit(EventContactMessage{ContactMessage: msg})
			return true, nil
		case RespContactMsgRecvV3:
			// V3 wire layout = 1 byte SNR×4 + 2 reserved + v2 payload.
			msg, err := parseContactMsgV3(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			out.Contact = &msg
			c.emit(EventContactMessage{ContactMessage: msg})
			return true, nil
		case RespChannelMsgRecv:
			msg, err := parseChannelMsg(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			out.Channel = &msg
			c.emit(EventChannelMessage{ChannelMessage: msg})
			return true, nil
		case RespChannelMsgRecvV3:
			msg, err := parseChannelMsgV3(frame.Payload[1:])
			if err != nil {
				return true, err
			}
			out.Channel = &msg
			c.emit(EventChannelMessage{ChannelMessage: msg})
			return true, nil
		case RespNoMoreMessages:
			out.NoMore = true
			return true, nil
		}
		return false, nil
	})
	return out, err
}

// SetDeviceTime synchronises the radio's clock with the host. Called
// once on connect — many MeshCore boards lack an RTC, so the
// sender-timestamp on outbound frames is bogus until we set this.
func (c *Client) SetDeviceTime(t time.Time) error {
	payload := append([]byte{byte(CmdSetDeviceTime)}, le32(uint32(t.Unix()))...)
	return c.callWithTimeout(payload, func(frame Frame) (bool, error) {
		if e := errFromFrame(frame); e != nil {
			return true, e
		}
		if ResponseCode(frame.Payload[0]) == RespOk || ResponseCode(frame.Payload[0]) == RespCurrTime {
			return true, nil
		}
		return false, nil
	})
}

// SetAdvertName sets the operator-facing display name the radio
// includes in its advert frames. Other nodes see this as the
// contact's `AdvName`. The firmware silently truncates anything past
// ~31 bytes — keep names short for visual clarity in remote UIs.
func (c *Client) SetAdvertName(name string) error {
	payload := append([]byte{byte(CmdSetAdvertName)}, []byte(name)...)
	return c.callWithTimeout(payload, awaitOk)
}

// SetAdvertLatLon sets the lat/lon broadcast in our advert. Values
// are firmware-encoded fixed-point integers (degrees × 1e6) — use the
// LatLonToE6 helper to convert from float degrees.
func (c *Client) SetAdvertLatLon(latE6, lonE6 int32) error {
	payload := make([]byte, 0, 1+8)
	payload = append(payload, byte(CmdSetAdvertLatLon))
	payload = appendInt32LE(payload, latE6)
	payload = appendInt32LE(payload, lonE6)
	return c.callWithTimeout(payload, awaitOk)
}

// SetChannel writes a channel slot — name + 16-byte AES-128 shared
// secret. Pass an empty name + zero secret to clear the slot
// (firmware treats that as "deconfigured", so subsequent
// GetChannels filters skip it). The "public key" prompt other
// MeshCore clients show on channel-add is the same shared secret;
// it's not asymmetric, just frequently shared out of band.
func (c *Client) SetChannel(channelIdx uint8, name string, secret [16]byte) error {
	payload := make([]byte, 0, 1+1+32+16)
	payload = append(payload, byte(CmdSetChannel))
	payload = append(payload, channelIdx)
	// Name is a 32-byte fixed-width null-padded cstring.
	var nameBuf [32]byte
	copy(nameBuf[:], name)
	payload = append(payload, nameBuf[:]...)
	payload = append(payload, secret[:]...)
	return c.callWithTimeout(payload, awaitOk)
}

// RemoveContact deletes a contact from the radio's table by
// public key. The firmware drops any pending messages and frees
// the slot for a new advert.
func (c *Client) RemoveContact(pub PubKey) error {
	payload := append([]byte{byte(CmdRemoveContact)}, pub[:]...)
	return c.callWithTimeout(payload, awaitOk)
}

// ResetPath clears the radio's stored next-hop route for a contact.
// DMs to that contact then re-discover the path via FLOOD on the
// next send — useful when the cached route has gone stale (a relay
// dropped off the mesh, the recipient moved out of the previously-
// learned path's coverage, etc.) and direct sends keep failing.
// Wire payload is just the contact's full pubkey.
func (c *Client) ResetPath(pub PubKey) error {
	payload := append([]byte{byte(CmdResetPath)}, pub[:]...)
	return c.callWithTimeout(payload, awaitOk)
}

// SetManualAddContacts toggles the firmware's auto-add-contacts
// behaviour. When false (default), every advert the radio hears
// gets added to the contacts table — fine for sparse meshes, but
// on busy networks the table fills (MAX_CONTACTS) and the
// firmware starts thrashing on evictions, slowing every command.
// When true, new adverts arrive as PushNewAdvert events instead
// (surfaced via EventAdvert with Manual=true) and the operator
// must explicitly approve each one before it consumes a slot.
func (c *Client) SetManualAddContacts(on bool) error {
	val := byte(0)
	if on {
		val = 1
	}
	payload := []byte{byte(CmdSetOtherParams), val}
	return c.callWithTimeout(payload, awaitOk)
}

// SendSelfAdvert broadcasts a fresh self-advertisement on the mesh.
// Call after changing the advert name / lat / lon so peers see the
// update without waiting for the periodic auto-advert. Pass
// SelfAdvertFlood for cross-mesh propagation, SelfAdvertZeroHop to
// stay on the local channel.
func (c *Client) SendSelfAdvert(t SelfAdvertType) error {
	payload := []byte{byte(CmdSendSelfAdvert), byte(t)}
	return c.callWithTimeout(payload, awaitOk)
}

// awaitOk is the response handler shared by every Set* command —
// completes on Ok, propagates Err verbatim, ignores anything else.
func awaitOk(frame Frame) (bool, error) {
	if e := errFromFrame(frame); e != nil {
		return true, e
	}
	if ResponseCode(frame.Payload[0]) == RespOk {
		return true, nil
	}
	return false, nil
}

// LatLonToE6 converts float-degrees lat/lon to the int32 1e6 fixed-
// point form expected by SetAdvertLatLon. Out-of-range values are
// clamped to the int32 limits so a typo doesn't wrap.
func LatLonToE6(lat, lon float64) (int32, int32) {
	clamp := func(d float64) int32 {
		v := d * 1e6
		if v > 2147483647 {
			return 2147483647
		}
		if v < -2147483648 {
			return -2147483648
		}
		return int32(v)
	}
	return clamp(lat), clamp(lon)
}

// ─── Parsers ──────────────────────────────────────────────────────

func parseSelfInfo(b []byte) (SelfInfo, error) {
	r := newBufReader(b)
	var info SelfInfo
	var err error
	info.Type, err = r.byte_()
	if err != nil {
		return info, err
	}
	info.TxPower, err = r.byte_()
	if err != nil {
		return info, err
	}
	info.MaxTxPower, err = r.byte_()
	if err != nil {
		return info, err
	}
	pk, err := r.bytes(32)
	if err != nil {
		return info, err
	}
	copy(info.PublicKey[:], pk)
	if info.AdvLatE6, err = r.int32LE(); err != nil {
		return info, err
	}
	if info.AdvLonE6, err = r.int32LE(); err != nil {
		return info, err
	}
	if _, err = r.bytes(3); err != nil { // reserved
		return info, err
	}
	if info.ManualAddContacts, err = r.byte_(); err != nil {
		return info, err
	}
	if info.RadioFreqHz, err = r.uint32LE(); err != nil {
		return info, err
	}
	if info.RadioBwHz, err = r.uint32LE(); err != nil {
		return info, err
	}
	if info.RadioSF, err = r.byte_(); err != nil {
		return info, err
	}
	if info.RadioCR, err = r.byte_(); err != nil {
		return info, err
	}
	info.Name = r.remainingString()
	return info, nil
}

func parseContact(b []byte) (Contact, error) {
	r := newBufReader(b)
	var ct Contact
	pk, err := r.bytes(32)
	if err != nil {
		return ct, err
	}
	copy(ct.PubKey[:], pk)
	t, err := r.byte_()
	if err != nil {
		return ct, err
	}
	ct.Type = AdvType(t)
	if ct.Flags, err = r.byte_(); err != nil {
		return ct, err
	}
	pathLen, err := r.byte_()
	if err != nil {
		return ct, err
	}
	ct.OutPathLen = int8(pathLen)
	path, err := r.bytes(64)
	if err != nil {
		return ct, err
	}
	copy(ct.OutPath[:], path)
	name, err := r.cstring(32)
	if err != nil {
		return ct, err
	}
	ct.AdvName = name
	lastAdv, err := r.uint32LE()
	if err != nil {
		return ct, err
	}
	ct.LastAdvert = epochSecs(lastAdv)
	if ct.AdvLatE6, err = r.int32LE(); err != nil {
		return ct, err
	}
	if ct.AdvLonE6, err = r.int32LE(); err != nil {
		return ct, err
	}
	lastMod, err := r.uint32LE()
	if err != nil {
		return ct, err
	}
	ct.LastMod = epochSecs(lastMod)
	return ct, nil
}

func parseChannelInfo(b []byte) (Channel, error) {
	r := newBufReader(b)
	idx, err := r.byte_()
	if err != nil {
		return Channel{}, err
	}
	name, err := r.cstring(32)
	if err != nil {
		return Channel{}, err
	}
	secret, err := r.bytes(16)
	if err != nil {
		return Channel{}, err
	}
	var ch Channel
	ch.Index = idx
	ch.Name = name
	copy(ch.Secret[:], secret)
	return ch, nil
}

func parseSent(b []byte) (SentResult, error) {
	r := newBufReader(b)
	var s SentResult
	res, err := r.byte_()
	if err != nil {
		return s, err
	}
	s.Result = int8(res)
	if s.ExpectedAckCRC, err = r.uint32LE(); err != nil {
		return s, err
	}
	if s.EstTimeoutMilli, err = r.uint32LE(); err != nil {
		return s, err
	}
	return s, nil
}

func parseContactMsg(b []byte) (ContactMessage, error) {
	r := newBufReader(b)
	var msg ContactMessage
	prefix, err := r.bytes(6)
	if err != nil {
		return msg, err
	}
	copy(msg.SenderPrefix[:], prefix)
	if msg.PathLen, err = r.byte_(); err != nil {
		return msg, err
	}
	t, err := r.byte_()
	if err != nil {
		return msg, err
	}
	msg.TxtType = TxtType(t)
	ts, err := r.uint32LE()
	if err != nil {
		return msg, err
	}
	msg.SenderTime = epochSecs(ts)
	msg.Text = r.remainingString()
	return msg, nil
}

// parseContactMsgV3 decodes the companion-protocol-v3 layout the
// firmware emits when app_target_ver >= 3. Adds 3 bytes of header
// (int8 SNR×4 + 2 reserved) before the v2 ContactMsgRecv payload.
// See examples/companion_radio/MyMesh.cpp::queueMessage in the
// firmware repo.
func parseContactMsgV3(b []byte) (ContactMessage, error) {
	r := newBufReader(b)
	snr, err := r.byte_()
	if err != nil {
		return ContactMessage{}, err
	}
	if _, err = r.bytes(2); err != nil { // reserved1, reserved2
		return ContactMessage{}, err
	}
	msg, err := parseContactMsg(r.b[r.i:])
	if err != nil {
		return msg, err
	}
	msg.SNR = float64(int8(snr)) / 4
	return msg, nil
}

func parseChannelMsg(b []byte) (ChannelMessage, error) {
	r := newBufReader(b)
	var msg ChannelMessage
	idx, err := r.byte_()
	if err != nil {
		return msg, err
	}
	msg.ChannelIndex = int8(idx)
	if msg.PathLen, err = r.byte_(); err != nil {
		return msg, err
	}
	t, err := r.byte_()
	if err != nil {
		return msg, err
	}
	msg.TxtType = TxtType(t)
	ts, err := r.uint32LE()
	if err != nil {
		return msg, err
	}
	msg.SenderTime = epochSecs(ts)
	msg.Text = r.remainingString()
	return msg, nil
}

// parseChannelMsgV3 — same V3 envelope as parseContactMsgV3 wrapped
// around the v2 ChannelMsgRecv payload.
func parseChannelMsgV3(b []byte) (ChannelMessage, error) {
	r := newBufReader(b)
	snr, err := r.byte_()
	if err != nil {
		return ChannelMessage{}, err
	}
	if _, err = r.bytes(2); err != nil {
		return ChannelMessage{}, err
	}
	msg, err := parseChannelMsg(r.b[r.i:])
	if err != nil {
		return msg, err
	}
	msg.SNR = float64(int8(snr)) / 4
	return msg, nil
}

// ─── Small binary helpers ─────────────────────────────────────────

type bufReader struct {
	b []byte
	i int
}

func newBufReader(b []byte) *bufReader { return &bufReader{b: b} }

func (r *bufReader) byte_() (byte, error) {
	if r.i >= len(r.b) {
		return 0, ErrShortPayload
	}
	v := r.b[r.i]
	r.i++
	return v, nil
}

func (r *bufReader) bytes(n int) ([]byte, error) {
	if r.i+n > len(r.b) {
		return nil, ErrShortPayload
	}
	v := r.b[r.i : r.i+n]
	r.i += n
	return v, nil
}

func (r *bufReader) uint32LE() (uint32, error) {
	v, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(v), nil
}

func (r *bufReader) int32LE() (int32, error) {
	v, err := r.uint32LE()
	return int32(v), err
}

// cstring reads a null-terminated string laid out in a fixed-size
// max-len buffer. Matches BufferReader.readCString from the JS lib —
// the firmware always writes this many bytes, with the first 0x00
// marking the end of meaningful text.
func (r *bufReader) cstring(maxLen int) (string, error) {
	v, err := r.bytes(maxLen)
	if err != nil {
		return "", err
	}
	for i, b := range v {
		if b == 0 {
			return string(v[:i]), nil
		}
	}
	return string(v), nil
}

// remainingString returns the rest of the buffer as a UTF-8 string.
// Matches BufferReader.readString.
func (r *bufReader) remainingString() string {
	if r.i >= len(r.b) {
		return ""
	}
	v := r.b[r.i:]
	r.i = len(r.b)
	return string(v)
}

func appendUint32LE(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendInt32LE(b []byte, v int32) []byte {
	return appendUint32LE(b, uint32(v))
}

func le32(v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return tmp[:]
}

func epochSecs(v uint32) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(int64(v), 0).UTC()
}
