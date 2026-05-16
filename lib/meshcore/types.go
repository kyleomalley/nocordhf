package meshcore

import "time"

// PubKey is a 32-byte ed25519 public key. Used in full as a contact's
// stable identifier and truncated to a 6-byte prefix when addressing
// outgoing text messages on the wire.
type PubKey [32]byte

// PubKeyPrefix is the first 6 bytes of a PubKey. The companion
// firmware identifies inbound text messages and addresses outbound
// ones by prefix only — full keys travel only in advertisements and
// the GetContacts dump.
type PubKeyPrefix [6]byte

// Prefix returns the first 6 bytes of a PubKey for use as an
// addressing handle on outgoing SendTxtMsg frames.
func (k PubKey) Prefix() PubKeyPrefix {
	var p PubKeyPrefix
	copy(p[:], k[:6])
	return p
}

// Contact mirrors a single record in the radio's contact table. Lat
// and Lon are encoded by the firmware as fixed-point integers (degrees
// × 1e6); LatDegrees / LonDegrees provide the float view callers
// usually want.
type Contact struct {
	PubKey     PubKey
	Type       AdvType
	Flags      byte
	OutPathLen int8
	OutPath    [64]byte
	AdvName    string
	LastAdvert time.Time
	AdvLatE6   int32
	AdvLonE6   int32
	LastMod    time.Time
}

// LatDegrees returns the contact's last-advertised latitude in
// floating-point degrees. Zero when the contact never advertised a
// position.
func (c *Contact) LatDegrees() float64 { return float64(c.AdvLatE6) / 1e6 }

// LonDegrees returns the contact's last-advertised longitude in
// floating-point degrees. Zero when the contact never advertised a
// position.
func (c *Contact) LonDegrees() float64 { return float64(c.AdvLonE6) / 1e6 }

// Channel mirrors one entry in the radio's channel table. Index is
// the slot number used to address SendChannelTxtMsg / GetChannel.
// Secret is the 16-byte symmetric key the radio uses to encrypt
// traffic on this channel — kept opaque on the host side; we never
// need to decrypt locally because the firmware does it for us.
type Channel struct {
	Index  uint8
	Name   string
	Secret [16]byte
}

// ContactMessage is a text message addressed at us by another contact,
// surfaced through a SyncNextMessage poll or a MsgWaiting push.
// SenderTime is the wall clock the originating device claimed when it
// signed the frame — useful for ordering but not authoritative. SNR
// is the link signal-to-noise the radio reported alongside this
// message in companion protocol v3+ responses; zero on older firmware.
type ContactMessage struct {
	SenderPrefix PubKeyPrefix
	PathLen      uint8
	TxtType      TxtType
	SenderTime   time.Time
	Text         string
	SNR          float64
}

// ChannelMessage is a text message addressed at a channel we're
// subscribed to. ChannelIndex matches Channel.Index. SNR is the link
// signal-to-noise the radio reported alongside this message in
// companion protocol v3+ responses; zero on older firmware.
type ChannelMessage struct {
	ChannelIndex int8
	PathLen      uint8 // 0xFF if delivered via direct route, otherwise hop count
	TxtType      TxtType
	SenderTime   time.Time
	Text         string
	SNR          float64
}

// SelfInfo is the radio's response to AppStart — identity + radio
// configuration we need before doing anything else. The TxPower and
// MaxTxPower units are firmware-specific (typically dBm).
type SelfInfo struct {
	Type              byte
	TxPower           byte
	MaxTxPower        byte
	PublicKey         PubKey
	AdvLatE6          int32
	AdvLonE6          int32
	ManualAddContacts byte
	// RadioFreqKHz is the carrier frequency in **kHz** (not Hz). The
	// firmware's selfInfo packet and SetRadioParams wire format both
	// use kHz for frequency; only BW is in Hz. Verified against
	// meshcore-web's SettingsPage.vue (radioFreq / 1000 → MHz).
	RadioFreqKHz uint32
	RadioBwHz    uint32
	RadioSF      byte
	RadioCR      byte
	Name         string
}

// DeviceInfo is the radio's response to CmdDeviceQuery — firmware
// build identity for display in a Status / About panel. None of
// these fields gate behaviour; they're informational only.
type DeviceInfo struct {
	FirmwareVersion   int8   // protocol-version-aligned integer (1, 2, 3, …)
	FirmwareBuildDate string // 12-byte cstring like "19 Feb 2025"
	ManufacturerModel string // remainder of frame, e.g. "RAK4631"
}

// CoreStats is the radio's response to GetStats(StatsTypeCore).
// Lightweight battery + uptime + queue snapshot; cheap enough to
// poll on a Status tab without bothering the firmware.
type CoreStats struct {
	BatteryMilliVolts uint16
	UptimeSecs        uint32
	QueueLen          uint8 // pending outbound packets in the firmware's TX queue
}

// RadioStats is the radio's response to GetStats(StatsTypeRadio).
// Air-time accounting + last-packet link quality. Useful for
// diagnosing "why are sends slow" (high TX/RX air seconds = busy
// channel) and "why is range bad" (noiseFloor + lastSnr).
type RadioStats struct {
	NoiseFloor int16   // dBm
	LastRSSI   int8    // dBm of the most recently decoded packet
	LastSNR    float64 // dB; firmware reports int8 quarter-dB, divided by 4
	TxAirSecs  uint32
	RxAirSecs  uint32
}

// PacketStats is the radio's response to GetStats(StatsTypePackets).
// Cumulative since boot; flood vs direct breakdown helps spot a
// repeater that's chewing through everyone's airtime.
type PacketStats struct {
	Recv        uint32
	Sent        uint32
	NSentFlood  uint32
	NSentDirect uint32
	NRecvFlood  uint32
	NRecvDirect uint32
	NRecvErrors uint32 // optional in older firmwares; 0 when absent
	HasRecvErrs bool   // false when the trailing field was missing
}

// SentResult is the radio's synchronous acknowledgement of a Send*
// command — Result < 0 indicates a queue / radio error; the ack and
// timeout fields let us correlate the eventual SendConfirmed push.
type SentResult struct {
	Result          int8
	ExpectedAckCRC  uint32
	EstTimeoutMilli uint32
}

// Event is the union type carried on Client.Events(). One concrete
// event per push / async response code; callers type-switch on the
// concrete type. Synchronous responses (RespOk, RespContact,
// RespEndOfContacts, RespChannelInfo, …) drive request methods
// directly and are not republished as Events.
type Event interface{ isMeshcoreEvent() }

// EventAdvert fires when an advertisement reaches us — either passively
// because the radio's "auto add contacts" mode is on (PushAdvert) or
// pending operator approval when manual mode is on (PushNewAdvert,
// indicated by Manual=true).
//
// Pending is non-nil only when Manual=true: PushNewAdvert delivers a
// full Contact-shaped record (name, type, lat/lon, last-advert
// timestamp), so callers can surface the advert without admitting it
// to the firmware's contacts table. PushAdvert ships only the pubkey
// — the firmware has already persisted it, so the rich data is
// available via GetContacts.
type EventAdvert struct {
	PublicKey PubKey
	Manual    bool // true if delivered as PushNewAdvert (operator must approve)
	Pending   *Contact
}

func (EventAdvert) isMeshcoreEvent() {}

// EventPathUpdated fires when the radio learns a new direct path to
// the named contact. Useful for refreshing routing UI.
type EventPathUpdated struct {
	PublicKey PubKey
}

func (EventPathUpdated) isMeshcoreEvent() {}

// EventSendConfirmed fires when an outbound text message has been
// acknowledged by the destination. AckCode matches the value returned
// in the SentResult.ExpectedAckCRC of the originating send.
type EventSendConfirmed struct {
	AckCode     uint32
	RoundTripMs uint32
}

func (EventSendConfirmed) isMeshcoreEvent() {}

// EventMsgWaiting fires when the radio buffers an inbound message we
// haven't drained yet. Trigger a Client.SyncNextMessage to pull the
// payload — that delivers either an EventContactMessage or an
// EventChannelMessage.
type EventMsgWaiting struct{}

func (EventMsgWaiting) isMeshcoreEvent() {}

// EventContactMessage is an inbound contact-addressed text message
// drained via SyncNextMessage.
type EventContactMessage struct{ ContactMessage }

func (EventContactMessage) isMeshcoreEvent() {}

// EventChannelMessage is an inbound channel-addressed text message
// drained via SyncNextMessage.
type EventChannelMessage struct{ ChannelMessage }

func (EventChannelMessage) isMeshcoreEvent() {}

// EventTraceData fires when the firmware delivers a PushTraceData
// frame — the reply to a previously-sent CmdSendTracePath request.
// Tag is the uint32 the host picked when issuing the trace; callers
// correlate against their pending-trace table to drop responses for
// traces they didn't initiate (a neighbouring app could in
// principle issue overlapping traces).
//
// PathHashes is the actual hop sequence the trace traversed (1
// byte per hop, same encoding as a normal Packet.Path); PathSNRs is
// the matching per-hop SNR (firmware quarter-dB units already
// converted to plain dB by the parser). LastSNR is the SNR of the
// final hop back to us.
type EventTraceData struct {
	Tag        uint32
	AuthCode   uint32
	Flags      byte
	PathHashes []byte
	PathSNRs   []float64
	LastSNR    float64
}

func (EventTraceData) isMeshcoreEvent() {}

// EventDisconnected fires once when the read goroutine exits — either
// the operator disconnected the device or the serial port returned an
// unrecoverable error. Err is nil on a clean Client.Close.
type EventDisconnected struct{ Err error }

func (EventDisconnected) isMeshcoreEvent() {}

// EventContactsFull fires when the firmware reports the contacts
// table is at its hardware-imposed limit (MAX_CONTACTS in the
// firmware — typically a few hundred entries depending on board).
// Subsequent inbound adverts will be dropped until the operator
// removes a contact via Client.RemoveContact.
type EventContactsFull struct{}

func (EventContactsFull) isMeshcoreEvent() {}

// EventContactDeleted fires when the firmware evicts a contact on
// its own (e.g. the oldest entry to make room for a new advert
// when the operator hasn't enabled manual-add mode). Carries the
// pubkey of the deleted contact so the client can drop it from
// the local roster + chat history.
type EventContactDeleted struct {
	PublicKey PubKey
}

func (EventContactDeleted) isMeshcoreEvent() {}

// EventRxLog fires for every packet the radio decodes off-air. The
// firmware sends these as PushLogRxData; we parse the embedded
// Packet so consumers (the RxLog viewer) can show route + payload
// type, hop count, SNR/RSSI without re-parsing the wire bytes.
type EventRxLog struct {
	SNR    float64 // last_snr / 4 (firmware units)
	RSSI   int     // last_rssi (dBm)
	Raw    []byte  // raw packet bytes (verbatim, for forensic dump)
	Packet Packet  // parsed Packet — zero value if Raw failed to decode
}

func (EventRxLog) isMeshcoreEvent() {}
