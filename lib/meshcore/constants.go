package meshcore

// Companion-radio protocol constants. Byte values mirror the upstream
// reference implementation in liamcottle/meshcore.js (src/constants.js)
// and the firmware in meshcore-dev/MeshCore. Treat these as wire
// contracts — bumping a value silently breaks compatibility with every
// device on the network.

// SupportedCompanionProtocolVersion is the AppStart protocol version
// we identify ourselves as supporting. Stays at 1 until upstream cuts
// a new major.
const SupportedCompanionProtocolVersion = 1

// Serial framing markers. Every frame on the wire is
// [marker:1][length:uint16-LE][payload:length].
const (
	FrameIncoming byte = 0x3E // ">" — radio → host
	FrameOutgoing byte = 0x3C // "<" — host → radio
)

// CommandCode identifies a host → radio request. Stored as the first
// payload byte of an outgoing frame.
type CommandCode byte

const (
	CmdAppStart          CommandCode = 1
	CmdSendTxtMsg        CommandCode = 2
	CmdSendChannelTxtMsg CommandCode = 3
	CmdGetContacts       CommandCode = 4
	CmdGetDeviceTime     CommandCode = 5
	CmdSetDeviceTime     CommandCode = 6
	CmdSendSelfAdvert    CommandCode = 7
	CmdSetAdvertName     CommandCode = 8
	CmdAddUpdateContact  CommandCode = 9
	CmdSyncNextMessage   CommandCode = 10
	CmdSetRadioParams    CommandCode = 11
	CmdSetTxPower        CommandCode = 12
	CmdResetPath         CommandCode = 13
	CmdSetAdvertLatLon   CommandCode = 14
	CmdRemoveContact     CommandCode = 15
	CmdShareContact      CommandCode = 16
	CmdExportContact     CommandCode = 17
	CmdImportContact     CommandCode = 18
	CmdReboot            CommandCode = 19
	CmdGetBatteryVoltage CommandCode = 20
	CmdDeviceQuery       CommandCode = 22
	CmdExportPrivateKey  CommandCode = 23
	CmdImportPrivateKey  CommandCode = 24
	CmdSendRawData       CommandCode = 25
	CmdGetChannel        CommandCode = 31
	CmdSetChannel        CommandCode = 32
	CmdSetOtherParams    CommandCode = 38
	CmdSendTelemetryReq  CommandCode = 39
	CmdSendBinaryReq     CommandCode = 50
	CmdSetFloodScope     CommandCode = 54
	CmdGetStats          CommandCode = 56
	CmdSendChannelData   CommandCode = 62
)

// ResponseCode identifies a radio → host response. Stored as the
// first payload byte of an incoming frame. Disjoint from PushCode so
// the dispatcher only needs to inspect the first byte.
type ResponseCode byte

const (
	RespOk                ResponseCode = 0
	RespErr               ResponseCode = 1
	RespContactsStart     ResponseCode = 2
	RespContact           ResponseCode = 3
	RespEndOfContacts     ResponseCode = 4
	RespSelfInfo          ResponseCode = 5
	RespSent              ResponseCode = 6
	RespContactMsgRecv    ResponseCode = 7 // companion protocol < v3
	RespChannelMsgRecv    ResponseCode = 8 // companion protocol < v3
	RespCurrTime          ResponseCode = 9
	RespNoMoreMessages    ResponseCode = 10
	RespExportContact     ResponseCode = 11
	RespBatteryVoltage    ResponseCode = 12
	RespDeviceInfo        ResponseCode = 13
	RespPrivateKey        ResponseCode = 14
	RespDisabled          ResponseCode = 15
	RespContactMsgRecvV3  ResponseCode = 16 // companion protocol >= v3 — adds SNR + 2 reserved bytes
	RespChannelMsgRecvV3  ResponseCode = 17 // companion protocol >= v3 — adds SNR + 2 reserved bytes
	RespChannelInfo       ResponseCode = 18
	RespSignStart         ResponseCode = 19
	RespSignature         ResponseCode = 20
	RespCustomVars        ResponseCode = 21
	RespAdvertPath        ResponseCode = 22
	RespTuningParams      ResponseCode = 23
	RespStats             ResponseCode = 24
	RespAutoAddConfig     ResponseCode = 25
	RespChannelDataRecv   ResponseCode = 27
	RespDefaultFloodScope ResponseCode = 28
)

// PushCode identifies an asynchronous radio → host event (no
// matching host request). Same wire slot as ResponseCode but in a
// disjoint range starting at 0x80.
type PushCode byte

const (
	PushAdvert            PushCode = 0x80
	PushPathUpdated       PushCode = 0x81
	PushSendConfirmed     PushCode = 0x82
	PushMsgWaiting        PushCode = 0x83
	PushRawData           PushCode = 0x84
	PushLoginSuccess      PushCode = 0x85
	PushLoginFail         PushCode = 0x86
	PushStatusResponse    PushCode = 0x87
	PushLogRxData         PushCode = 0x88
	PushTraceData         PushCode = 0x89
	PushNewAdvert         PushCode = 0x8A
	PushTelemetryResponse PushCode = 0x8B
	PushBinaryResponse    PushCode = 0x8C
	PushContactDeleted    PushCode = 0x8F // firmware evicted oldest contact
	PushContactsFull      PushCode = 0x90 // contacts storage at MAX_CONTACTS
)

// ErrCode subtypes returned in the second byte of a RespErr frame.
// Mirrors ERR_CODE_* in MeshCore firmware (examples/companion_radio).
type ErrCode byte

const (
	ErrUnsupportedCmd ErrCode = 1
	ErrNotFound       ErrCode = 2
	ErrTableFull      ErrCode = 3 // contacts/channels storage exhausted
	ErrBadState       ErrCode = 4
	ErrFileIO         ErrCode = 5
	ErrIllegalArg     ErrCode = 6
)

// AdvType is the contact / advertisement type carried in an advert
// app-data flags byte (low 4 bits).
type AdvType byte

const (
	AdvTypeNone     AdvType = 0
	AdvTypeChat     AdvType = 1
	AdvTypeRepeater AdvType = 2
	AdvTypeRoom     AdvType = 3
	AdvTypeSensor   AdvType = 4
)

// String returns a stable identifier suitable for logging or UI.
func (a AdvType) String() string {
	switch a {
	case AdvTypeNone:
		return "NONE"
	case AdvTypeChat:
		return "CHAT"
	case AdvTypeRepeater:
		return "REPEATER"
	case AdvTypeRoom:
		return "ROOM"
	case AdvTypeSensor:
		return "SENSOR"
	default:
		return "UNKNOWN"
	}
}

// SelfAdvertType selects the propagation scope for a SendSelfAdvert
// command. ZeroHop stays on the immediate channel; Flood is rebroadcast
// across the mesh.
type SelfAdvertType byte

const (
	SelfAdvertZeroHop SelfAdvertType = 0
	SelfAdvertFlood   SelfAdvertType = 1
)

// TxtType selects the body interpretation of a text message. Plain is
// the default chat traffic; CliData carries machine-parseable command
// output; SignedPlain is plain text with an attached signature.
type TxtType byte

const (
	TxtPlain       TxtType = 0
	TxtCliData     TxtType = 1
	TxtSignedPlain TxtType = 2
)
