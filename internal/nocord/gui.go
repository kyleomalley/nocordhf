// Package nocord -"NocordHF" -is a Discord-style chat-focused UI for FT8
// receive and transmit. Three-column layout (mode rail / channel list / chat
// pane), modeled on Discord and traditional IRC clients.
//
// Distinct from internal/ui (the radio operator's full-instrument panel: tabs,
// waterfall, QSO state, decode list, map). The Nocord view treats decodes as
// chat lines and confines TX to two well-formed primitives -bare CQ or a
// directed call to a heard station -with no signal-report / RR73 / 73
// hand-off ceremony exposed to the user.
package nocord

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/kyleomalley/nocordhf/lib/adif"
	"github.com/kyleomalley/nocordhf/lib/callsign"
	"github.com/kyleomalley/nocordhf/lib/cat"
	"github.com/kyleomalley/nocordhf/lib/ft8"
	"github.com/kyleomalley/nocordhf/lib/hamdb"
	"github.com/kyleomalley/nocordhf/lib/logging"
	"github.com/kyleomalley/nocordhf/lib/lotw"
	mapview "github.com/kyleomalley/nocordhf/lib/mapview"
	"github.com/kyleomalley/nocordhf/lib/meshcore"

	"encoding/base64"
	"encoding/hex"
	"github.com/kyleomalley/nocordhf/lib/ntpcheck"
	"github.com/kyleomalley/nocordhf/lib/tqsl"
	"github.com/kyleomalley/nocordhf/lib/waterfall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Band is a single channel in the channel list -a HF amateur band that we
// can tune the radio to. The integer Hz is what we feed to the CAT driver.
type Band struct {
	Name string // "20m"
	Hz   uint64 // 14_074_000
}

// DefaultBands is the standard FT8 dial-frequency set. Mirrors the one in
// internal/ui/types.go but kept local so this package doesn't pull the
// heavyweight ui dependencies.
var DefaultBands = []Band{
	{"160m", 1_840_000},
	{"80m", 3_573_000},
	{"40m", 7_074_000},
	{"30m", 10_136_000},
	{"20m", 14_074_000},
	{"17m", 18_100_000},
	{"15m", 21_074_000},
	{"12m", 24_915_000},
	{"10m", 28_074_000},
}

// TxRequest mirrors ui.TxRequest's relevant subset. Built by the GUI when the
// operator types a CQ or a directed call; consumed by the decoder loop in main.
//
// Tune=true selects a non-FT8 path: generate a pure-carrier tone for
// ~3 s while keying PTT, suitable for adjusting an antenna tuner.
// When Tune is set, Callsign / Grid / RemoteCall are ignored and the
// 15-second slot countdown is skipped — tune transmissions don't have
// to align to FT8 boundaries.
type TxRequest struct {
	Callsign   string // operator's own call (set in profile)
	Grid       string // operator's grid
	RemoteCall string // empty for CQ; set when replying to a heard station
	// Tail, when non-empty alongside RemoteCall, replaces Grid in the
	// outbound message ("RemoteCall Callsign Tail" instead of
	// "RemoteCall Callsign Grid"). Used by the auto-progress path to
	// send sig reports / R+report / RR73 / 73 in response to inbound
	// directed messages without requiring a full QSO state machine.
	Tail string
	// AvoidPeriod, when set to 0 or 1, locks the TX out of that 15-s
	// slot period — runTX will wait one extra slot if the next boundary
	// would land in it. Use -1 for "no preference" (CQ, /tune, or
	// targets we've never decoded). Prevents accidentally TXing in the
	// same period as the station you're calling, where they'd be RXing
	// nothing because they're TXing too.
	AvoidPeriod int
	Tune        bool // pure-carrier tune transmission
	StopCh      chan struct{}
}

// heardSortMode picks how the HEARD sidebar orders its entries. Alpha is
// the default (predictable, like an IRC nick list); SNR sorts strongest
// first to surface the loudest stations on the band. The header label
// click cycles through these.
type heardSortMode int

const (
	heardSortAlpha heardSortMode = iota
	heardSortSNR
	heardSortRecent
)

// heardEntry is one HEARD-sidebar row: the most recent SNR for that
// callsign plus the wall-clock time we last decoded a transmission FROM
// that station. Stored in a map keyed by callsign so repeated decodes
// from the same operator just refresh the SNR rather than duplicating.
type heardEntry struct {
	snr      float64
	lastSeen time.Time
	lastCQ   time.Time // most recent slot we heard this op call CQ; zero if never
	// lastOTA is the most recent slot we heard this op transmit from
	// a portable / outdoor activity. lastOTAType is the *OTA program
	// name (POTA/SOTA/IOTA/WWFF/BOTA/LOTA/NOTA), or "PORTABLE" for a
	// /P /M /MM /AM suffix without an explicit programme. Empty when
	// never.
	lastOTA     time.Time
	lastOTAType string
}

const maxHeard = 200

// chatRow is one rendered line in the chat pane -either a real RX decode or
// a synthesised system message ("connected to 20m", "TX queued: CQ ...").
type chatRow struct {
	when      time.Time
	system    bool // styled differently
	tx        bool // our own TX echo
	addrUs    bool // message is addressed to us
	separator bool // thin divider drawn between RX cycles, no text content
	freqHz    float64
	snrDB     float64
	region    string // e.g. "NA-US", "EU-DE"
	text      string
	method    string // "" / "osd" / "a1" / etc.
	// confirmStatus flags whether the row's sender has a prior contact
	// with us on the current band (computed at AppendDecode time):
	//   0 = none, 1 = ADIF-logged, 2 = LoTW-confirmed
	confirmStatus int
	// txInProgress + txProgress drive the live "characters going green
	// while transmitting" effect on TX rows. While true, the row's
	// message is split: the first txProgress runes render green
	// (already on-air) and the remainder render grey (still pending).
	// Set to false once playback completes — row then renders fully
	// green like a normal finished-TX echo. Only meaningful on tx rows.
	txInProgress bool
	txProgress   int
	// txStart is the wall clock the audio began playing — used by the
	// 1 Hz status ticker to advance txProgress proportionally.
	txStart time.Time
	// MeshCore per-message delivery state. mcAckCRC is the
	// SentResult.ExpectedAckCRC the firmware returned at send time;
	// nonzero means we're tracking it. mcDelivery cycles
	// pending(1) → delivered(2) on PushSendConfirmed match, or
	// failed(3) on Send error / timeout. Zero on rows we don't
	// track (RX rows, system rows, FT8 rows).
	mcAckCRC   uint32
	mcSentAt   time.Time
	mcDelivery byte
	// mc marks the row as a MeshCore message, so formatRowSegments
	// renders the meta column with mesh SNR formatting (and skips
	// the FT8 region cell). Set by every mcAppendRow / mcAppend*
	// path; never set on FT8 rows.
	mc bool
	// mcSender is the bare sender name for an IRC-style right-
	// aligned column. Outbound rows set it to the operator's own
	// callsign; inbound rows set it to the contact display name
	// (DM) or the speaker extracted from the channel payload.
	// Empty for system / separator rows.
	mcSender string
	// mcPathLen / mcPath snapshot the receiving packet's route so
	// "Map Trace" works on historical messages instead of relying
	// on the volatile in-memory RxLog ring. Captured at incoming-
	// message append time by correlating with the matching RxLog
	// frame; persisted via chatRowToStored. Empty when no
	// correlation could be made (radio relaunched, ring rolled).
	mcPathLen byte
	mcPath    []byte
}

// MeshCore delivery-state values for chatRow.mcDelivery.
const (
	mcDeliveryNone      byte = 0
	mcDeliveryPending   byte = 1
	mcDeliveryDelivered byte = 2
	mcDeliveryFailed    byte = 3
)

// mcPendingTimeout caps how long we wait for a PushSendConfirmed
// match before flipping the row to "failed". The firmware's own
// est_timeout for a flood send is 30–60 s; 90 s leaves margin for a
// distant repeater hop without leaving the operator hanging.
const mcPendingTimeout = 90 * time.Second

// GUI is the NocordHF top-level window state. Everything the main loop needs
// to interact with the chat view is exposed via its methods; channel-style
// communication for the events that fire from background goroutines (decoder,
// audio capture, CAT).
type GUI struct {
	app    fyne.App
	window fyne.Window

	buildID string
	myCall  string
	myGrid  string

	// txCh is the outbound queue: GUI sends a TxRequest, main.go pulls and
	// triggers the encoder + player + PTT chain. Same shape as the legacy
	// ui.TxRequest channel -main.go converts.
	txCh chan TxRequest

	// tuneCh sends a frequency change request to the radio loop. Empty
	// channel selector = no CAT, ignored.
	tuneCh chan uint64

	mu           sync.Mutex
	current      Band      // selected channel/band
	rows         []chatRow // chat history, capped at maxRows
	lastChatSlot int64     // floored UTC second of the last decoded slot we appended; 0 = none yet

	// Per-slot decode counter for the status header. Counts how many
	// decodes have arrived for the current 15-second slot; resets to 0
	// when the slot rolls over. lastSlotDecodes preserves the previous
	// slot's final count so the header keeps showing it during the
	// silent gap between slot end and the first decode of the next.
	currentSlotDecodes int
	lastSlotDecodes    int
	currentSlotSec     int64
	// lastAutoReplyKey dedupes auto-progress replies within a single 15s
	// slot. Multiple Costas hits / OSD passes can deliver the same
	// decoded message more than once; without this guard we'd queue the
	// same R-report or RR73 several times back-to-back. Keyed by
	// "slotSec|RemoteCall|Tail"; cleared implicitly when the slot rolls.
	lastAutoReplyKey string
	// autoReply gates the AppendDecode → maybeAutoReply hook. When off,
	// the operator drives every TX manually (right-click Reply or
	// typing in the input). Persisted via the "auto_reply" preference
	// so the toggle survives restarts.
	autoReply bool
	// rosterStaleMinutes is how long a HEARD-list entry / map spot can
	// go without a fresh decode before sweepStaleRoster purges it.
	// 0 disables purging (entries live forever, like before this
	// setting existed). Persisted as the "roster_stale_minutes" pref;
	// editable in Settings → Profile.
	rosterStaleMinutes int
	// pendingRetries tracks auto-reply TXs we sent but haven't yet seen
	// a response to. Each entry's tail is re-queued every ~30 s (one
	// of their slots + one of ours) up to retryMaxAttempts; cleared the
	// moment we decode any inbound message addressed at us from that
	// call. Lets the auto-reply chain survive a missed RX without
	// requiring the user to manually re-click. Keyed by uppercase call.
	pendingRetries map[string]*pendingRetry
	// peerPeriods records the most-recently-observed TX period (0 = first
	// half of minute, 1 = second) for each station we've decoded. Used
	// to pick the OPPOSITE period when transmitting to them, so we
	// don't try to call them while they're talking. Updated on every
	// AppendDecode whose sender we can identify; capped implicitly by
	// chat history because callers never accumulate beyond what's been
	// recently heard. Map size is naturally bounded by band activity.
	peerPeriods map[string]int
	// heard tracks RX-only senders for the IRC-style HEARD sidebar and
	// input-box autocomplete. Keyed by callsign; value carries the latest
	// SNR plus the timestamp we last saw the call so we can sort newest
	// first. Capped at maxHeard entries (oldest evicted).
	heard     map[string]heardEntry
	heardSort heardSortMode

	// bandActivity returns the number of unique stations heard on `band`
	// over the recent activity window, or 0 if no data. Sourced from
	// pskreporter via main.go; nil = no live counts (band list shows
	// just "#20m" with no parenthetical).
	bandActivity func(band string) int

	// Fyne widgets, set during Build()
	chatList     *widget.List
	usersList    *widget.List
	bandList     *widget.List
	statusText   *canvas.Text
	input        *historyEntry
	bandLabel    *canvas.Text
	userCallText *canvas.Text // bottom-of-channel-column user bar -operator's callsign
	userGridText *canvas.Text // bottom-of-channel-column user bar -operator's grid
	scope        *scopePane   // rightmost column: waterfall + map
	sendBtn      *widget.Button
	// mcCharCount renders a "N/140" character counter to the right of
	// the input box, MeshCore-only. Goes amber within 10 of the cap
	// and red over it so the operator sees the limit before the
	// firmware silently drops the send. Built once in buildLayout;
	// shown/hidden by applySidebarForMode.
	mcCharCount *canvas.Text

	// Mode rail state. activeMode is one of "ft8" / "meshcore". The
	// chip references are kept on the struct so refreshModeRail can
	// repaint their backgrounds when the mode changes. Persisted as
	// the "active_mode" preference so the choice survives a restart.
	activeMode string
	ft8Chip    *fyne.Container
	meshChip   *fyne.Container

	// Channel-column swap. sidebarStack is the single child of the
	// channel column body; FT8 mode shows the bands list, MeshCore
	// mode swaps in mcSidebar (Contacts header + contacts list,
	// divider, Channels header + channels list). chanHeader is
	// retitled BANDS / MESHCORE on mode flip. usersCol is the
	// HEARD sidebar to the right of the chat list — hidden in
	// MeshCore mode (FT8 callsigns are irrelevant to mesh chat).
	// autoCheck is the FT8 auto-reply toggle in the input row —
	// hidden in MeshCore mode for the same reason.
	sidebarStack *fyne.Container
	mcSidebar    *fyne.Container
	chanHeader   *canvas.Text
	usersCol     *fyne.Container
	usersInner   fyne.CanvasObject // FT8 HEARD list, swapped out in MeshCore mode
	mcRosterPane *fyne.Container   // MeshCore roster (active-channel senders)
	mcRosterList *widget.List
	mcRosterHdr  *canvas.Text
	autoCheck    *widget.Check
	// chatHelpTap is the (?) icon next to the chat topic. Its
	// dialog covers FT8 colour conventions, badges, the auto-
	// progress chain, and FT8-specific keyboard shortcuts — all
	// irrelevant in MeshCore mode, so we hide the icon there.
	chatHelpTap *tappableContainer
	// chanChatScope is the outer HSplit that lets the operator drag
	// the chan column / chat-scope boundary. Stashed on the GUI so
	// applySidebarForMode can reset the offset on mode flip — FT8
	// gets a tight chan column (bands), MeshCore gets a wider one
	// (contact names).
	chanChatScope *container.Split

	// MeshCore session state. Owned by the GUI so the mode-rail
	// callbacks and the chat plumbing can both reach it. mcThreadHistory
	// is keyed by mcThreadID — "contact:<hex pubkey prefix>" or
	// "channel:<idx>" — and stores the per-conversation chat row
	// buffer. ft8RowsBackup snapshots g.rows on FT8 → MeshCore
	// transitions so we can restore the FT8 chat verbatim on the way
	// back; the active conversation's rows live in g.rows so the
	// existing chatList rendering path stays unchanged.
	mcMu             sync.Mutex
	mcClient         *meshcore.Client
	mcLog            *zap.SugaredLogger // wire trace → nocordhf-meshcore.log
	mcStore          *meshcore.Store    // persisted chat history → nocordhf-meshcore.db
	mcContacts       []meshcore.Contact
	mcChannels       []meshcore.Channel
	mcSelfInfo       meshcore.SelfInfo
	mcContactsList   *widget.List
	mcChannelsList   *widget.List
	mcContactsHeader *canvas.Text
	mcChannelsHeader *canvas.Text
	mcStatusText     *canvas.Text
	mcCurrentThread  string
	mcThreadHistory  map[string][]chatRow
	// mcContactsSortBy is the sidebar sort mode chosen via the
	// CONTACTS header menu (Recent / Name / Type). Persists in
	// memory only — defaults to Recent on every launch.
	mcContactsSortBy string
	// mcMentioned flags threads with at least one unread message
	// containing an @[<selfName>] mention since last read. Bumped
	// in the receive paths alongside mcUnread, cleared on thread
	// switch. Drives a stronger sidebar highlight (Slack "@you"
	// amber) than plain unread so directed call-outs in busy
	// channels stand out.
	mcMentioned map[string]bool
	// mcUnread tracks per-thread unread counts for the sidebar
	// badge. Keyed by mcThreadID; incremented on incoming messages
	// for non-active threads, zeroed when the operator selects
	// the thread. Slack-style "(n)" badge in the list rendering.
	mcUnread map[string]int
	// mcContactsLastMod is the largest Contact.LastMod we've seen
	// across the cached roster. Used as the `since` argument on
	// the next GetContactsSince call so each refresh pulls only
	// the delta instead of the full table.
	mcContactsLastMod time.Time
	// mcContactsRefreshTimer debounces advert-driven contact
	// refreshes. Without it, a busy mesh emitting many adverts
	// per minute would queue dozens of full-table dumps behind
	// callMu — at hundreds of contacts per dump, that flooded
	// the radio and pinned the command queue.
	mcContactsRefreshTimer *time.Timer
	// mcLastContactsFullWarn rate-limits the contacts-full
	// system warning so a thrashing radio doesn't drown the
	// chat in identical "contacts storage full" lines.
	mcLastContactsFullWarn time.Time
	// mcFavorites pins the operator's chosen contacts to the top
	// of the sidebar regardless of the active sort mode. Hydrated
	// from the bbolt store on connect; toggled via the contact
	// context menu's Favorite / Unfavorite item.
	mcFavorites map[meshcore.PubKey]bool
	// mcPendingByAck maps SentResult.ExpectedAckCRC to the
	// (thread, row index) of the chat row awaiting delivery
	// confirmation. PushSendConfirmed look-up flips the row to
	// Delivered; the per-second sweeper flips stale ones to Failed.
	mcPendingByAck map[uint32]mcPendingSend
	// mcAutoReconnectTimer is the one-shot timer scheduled by
	// scheduleMcAutoReconnect on EventDisconnected. Cleared on
	// manual disconnect so the operator's "stay disconnected"
	// intent isn't undermined by a stale timer firing later.
	mcAutoReconnectTimer *time.Timer
	// mcManualDisconnect is set when disconnectMeshcore runs from
	// a deliberate user action (Settings save, window close).
	// scheduleMcAutoReconnect respects this to avoid auto-reconnect
	// loops the operator just chose to break.
	mcManualDisconnect bool
	// mcConsecFails counts consecutive Failed DMs per contact pubkey
	// since their last successful Delivered. Reset on success and
	// after a manual / automatic CmdResetPath. Drives the
	// auto-path-reset recovery in mcSweepPending: when a contact's
	// counter hits mcAutoResetThreshold the radio's cached out_path
	// is dropped so the next DM re-discovers via FLOOD.
	mcConsecFails map[meshcore.PubKey]int
	// RxLog ring — every PushLogRxData event the firmware emits
	// goes here for display in the bottom-of-right-pane viewer.
	// Capped at maxMcRxLog so a busy mesh doesn't grow unbounded.
	mcRxLog       []mcRxLogEntry
	mcRxLogList   *widget.List
	mcRxLogPane   *fyne.Container
	mcRxLogHeader *canvas.Text
	ft8RowsBackup []chatRow
	mcStarted     bool

	// TX state: when a TX is in flight, sendBtn re-labels to "Stop" and
	// clicking it closes activeStopCh -the main TX loop watches this and
	// short-circuits the playback. Single TX at a time, so a single
	// channel suffices.
	txActive     bool
	activeStopCh chan struct{}

	// Magnification popup — two modes:
	//   - Preview (decodePopupPinned == false): driven by hover.
	//     Popup follows the cursor (positioned next to whichever
	//     decode box is under it). Hides when the cursor leaves all
	//     boxes. Lets the operator scan the band quickly.
	//   - Pinned (decodePopupPinned == true): set by clicking a
	//     decode box. Popup stays at the click position; hover
	//     events are ignored. Operator can mouse over to the popup's
	//     buttons without losing it. Click another box to re-pin to
	//     that one; click empty waterfall or [✕] to unpin.
	//
	// decodePopupPinPos remembers the click position so we can
	// re-show the popup at the same spot when a click on a different
	// decode box dismisses it (Fyne's PopUp auto-hides on any click
	// outside its own bounds).
	decodePopup        *widget.PopUp
	decodePopupContent *fyne.Container // body swapped in place on content updates
	decodePopupCall    string
	decodePopupPinPos  fyne.Position
	decodePopupPinned  bool

	// Hover-preview debounce: cursor motion across decode boxes can
	// fire many onHover events per second. Rebuilding + reshowing the
	// PopUp on each one is expensive (thousands of redraws while the
	// cursor sweeps). Instead, hover events stash the latest target
	// in decodePreviewPending and arm a timer; when it fires we render
	// the most recent target. Each new event resets the timer, so a
	// cursor that's still moving never causes a render.
	decodePreviewTimer   *time.Timer
	decodePreviewPending decodePreviewTarget

	// HEARD-row tooltip: small floating popup that shows the country
	// name when the operator hovers a row. Same persistent / debounce
	// pattern as decodePopup so the tooltip doesn't flicker when the
	// list re-binds the row template under the cursor.
	// HEARD-row tooltip rendered as a non-modal overlay container (NOT
	// widget.PopUp) so it doesn't capture mouse events — earlier the
	// PopUp version sat over the row and ate right-clicks intended
	// for the context menu.
	heardTooltip     fyne.CanvasObject
	heardTooltipHide *time.Timer
	heardTooltipCall string

	// Selection state: when the operator clicks a decode box, we
	// scroll the chat + HEARD lists to the matching call, blink-
	// highlight the row, and freeze auto-scroll on new chat rows so
	// the operator's selection doesn't get scrolled out of view.
	// chatScrollFrozenUntil is the wall-clock time until which
	// appendRow should suppress its auto-ScrollTo. Set whenever a
	// selection is made; cleared by user scrolling or after a
	// timeout.
	chatScrollFrozenUntil time.Time
	highlightedCall       string    // currently blink-highlighted call
	highlightUntil        time.Time // wall-clock end of blink window
	highlightTimer        *time.Timer

	// suppressChatSelectInput is set while we programmatically call
	// chatList.Select(idx) to scroll-to-row. Without it, the chatList
	// OnSelected handler would treat the synthetic selection like a
	// user click and overwrite the input box with the row's callsign.
	// Flipped on by scrollChatTo* helpers; the OnSelected handler
	// reads + clears it on entry.
	suppressChatSelectInput bool

	// suppressHeardSelectAction is the HEARD-list counterpart. The
	// HEARD OnSelected handler shows the magnification popup and
	// re-runs selectCall — both of which would recurse infinitely
	// when the synthetic Select comes from selectCall's own
	// scrollHeardToCall path. Set by scrollHeardToCall; OnSelected
	// reads + clears it on entry.
	suppressHeardSelectAction bool

	// QSO logging. adifWriter persists each completed contact to
	// nocordhf.adif; adifLog mirrors the on-disk file for in-memory
	// queries (worked-grid overlay, dup detection); qsoTracker watches
	// the RX/TX stream to detect when a contact is complete.
	adifWriter *adif.Writer
	adifLog    []adif.Record
	qso        *qsoTracker

	// LoTW state. lotwClient is constructed when credentials are saved
	// in the settings dialog; lotwQSLs holds the most recent sync's
	// QSL/QSO records and feeds the worked-grid overlay (yellow tint
	// for any LoTW QSO on the active band, red for confirmed QSL).
	lotwClient *lotw.Client
	lotwQSLs   []lotw.QSL

	// TQSL upload config + auto-upload toggle. When tqslAutoUpload is
	// true, every QSO logged by the qsoTracker is followed by a tqsl
	// invocation that signs the ADIF file and uploads to LoTW. Same
	// pattern as the legacy GUI: upload the running adif file on each
	// QSO close so partial logs go up promptly.
	tqslCfg        *tqsl.Config
	tqslAutoUpload bool
	// hamdb client + per-session in-flight dedupe. The first time we
	// hear a station's call we fire an async lookup; on success we
	// upgrade their map pin from coarse-grid placement to HamDB's
	// precise home coordinates (UpgradeSpotLocation handles the
	// portable-vs-home logic so we don't overwrite a station that's
	// transmitting from a grid different to their home QTH).
	hamdb          *hamdb.Client
	hamdbSubmitted map[string]struct{}

	// radio is the live CAT handle owned by main.go. Held here so the
	// Radio settings tab can hot-swap (Connect / Disconnect) the running
	// rig without restarting the app. nil-safe: if main.go never calls
	// SetRadio, the Radio tab degrades to read-only.
	radio *cat.AtomicRadio

	// ntpChecker measures clock drift against public NTP servers -the
	// single most important non-obvious failure mode for FT8 is clock
	// skew, so we surface it in the chat header. Lazy-started in Build()
	// so RX-only sessions still get the reading.
	ntpChecker *ntpcheck.Checker
}

// decodePreviewTarget is the data needed to render one preview frame.
// Stashed by the hover handler and consumed by the debounce timer.
type decodePreviewTarget struct {
	call      string
	slotStart time.Time
	freqHz    float64
	pos       fyne.Position
	end       bool // true → preview should hide instead of show
}

const maxRows = 500

// NewGUI builds the Nocord window for a Fyne app. buildID lands in the title
// bar so screenshots identify which build they came from. txCh and tuneCh
// are caller-owned; the GUI sends to them but never closes them.
func NewGUI(a fyne.App, buildID string, txCh chan TxRequest, tuneCh chan uint64) *GUI {
	g := &GUI{
		app:            a,
		buildID:        buildID,
		txCh:           txCh,
		tuneCh:         tuneCh,
		current:        DefaultBands[4], // 20m default
		peerPeriods:    make(map[string]int),
		pendingRetries: make(map[string]*pendingRetry),
		hamdbSubmitted: make(map[string]struct{}),
	}
	// HamDB client (on-disk cache + in-flight dedupe baked in). Best-
	// effort: if cache dir creation fails we degrade to coarse-prefix
	// map placement — log it once so the operator knows.
	if c, err := hamdb.New(); err == nil {
		g.hamdb = c
	} else if logging.L != nil {
		logging.L.Warnw("hamdb init failed", "err", err)
	}
	g.window = a.NewWindow("NocordHF [build=" + buildID + "]")
	g.window.Resize(fyne.NewSize(1100, 720))
	g.window.SetContent(g.buildLayout())
	// ESC anywhere on the window cancels every in-flight + queued TX
	// and wipes pending auto-reply retries. Fyne dispatches TypedKey
	// to the canvas only when no widget consumed the key first; the
	// chat input doesn't claim ESC, so this fires regardless of which
	// pane has focus.
	g.window.Canvas().SetOnTypedKey(func(ev *fyne.KeyEvent) {
		switch ev.Name {
		case fyne.KeyEscape:
			g.cancelAllSends("Esc", true)
		case fyne.KeyDelete, fyne.KeyBackspace:
			// Delete/Backspace at canvas level (i.e. no widget
			// claimed it — chat input swallows them when
			// focused). When the operator has a MeshCore
			// contact selected, treat as the prune shortcut.
			if focused := g.window.Canvas().Focused(); focused != nil {
				return
			}
			g.removeSelectedMcContact()
		}
	})
	return g
}

// SetProfile registers the operator's callsign and grid. CQ messages and
// directed-reply messages use these. Recentres the map on the new grid.
// Persists to fyne.Preferences so the values survive a relaunch -the
// settings dialog is the only writer, and main.go reads them at startup
// before applying CLI/env overrides.
// Safe to call from any goroutine.
func (g *GUI) SetProfile(myCall, myGrid string) {
	myCall = strings.ToUpper(strings.TrimSpace(myCall))
	myGrid = strings.ToUpper(strings.TrimSpace(myGrid))
	g.mu.Lock()
	g.myCall = myCall
	g.myGrid = myGrid
	scope := g.scope
	app := g.app
	g.mu.Unlock()
	if app != nil {
		prefs := app.Preferences()
		prefs.SetString("myCall", myCall)
		prefs.SetString("myGrid", myGrid)
	}
	g.refreshStatus()
	g.refreshUserBar()
	if scope != nil && myGrid != "" {
		scope.SetMyGrid(myGrid)
	}
	if g.qso != nil {
		g.qso.SetProfile(myCall, myGrid)
	}
}

// ApplySavedToggles reads the persisted decoder / map switches from
// fyne.Preferences and applies them to the live decoder and map widget.
// Called once from main.go after the GUI is built and before audio
// starts so the operator's last preferences are honoured even before
// they open the settings dialog. Defaults match the legacy GUI:
// worked-grid overlay ON, strict ITU filter ON.
func (g *GUI) ApplySavedToggles() {
	if g.app == nil {
		return
	}
	prefs := g.app.Preferences()
	overlay := prefs.BoolWithFallback("map_worked_overlay", true)
	itu := prefs.BoolWithFallback("itu_filter", true)
	auto := prefs.BoolWithFallback("auto_reply", false)
	ft8.SetITUFilterEnabled(itu)
	g.mu.Lock()
	g.autoReply = auto
	scope := g.scope
	g.mu.Unlock()
	if scope != nil && scope.mapWidget != nil {
		scope.mapWidget.SetShowWorkedOverlay(overlay)
	}
	// LoTW: if we have stored credentials, build the client and load
	// any cached QSL records immediately, then trigger an incremental
	// sync in the background. Cached records light up the map even if
	// the network sync is slow or fails.
	if user, pass := prefs.String("lotw_username"), prefs.String("lotw_password"); user != "" && pass != "" {
		if cli, err := lotw.New(user, pass); err == nil {
			g.mu.Lock()
			g.lotwClient = cli
			g.lotwQSLs = cli.LoadCached()
			g.mu.Unlock()
			if scope != nil && scope.mapWidget != nil {
				fyne.Do(func() { scope.mapWidget.Refresh() })
			}
			go g.runLoTWSync(user, pass, func(msg string) {
				g.AppendSystem("LoTW: " + msg)
			})
		}
	}
}

// LoadSavedProfile returns the (call, grid) pair persisted by a prior
// SetProfile call, or empty strings if none. main.go uses this so a CLI
// flag / env var can still override but the saved value is the default.
func (g *GUI) LoadSavedProfile() (call, grid string) {
	if g.app == nil {
		return "", ""
	}
	prefs := g.app.Preferences()
	return prefs.String("myCall"), prefs.String("myGrid")
}

// SetRadio hands main.go's AtomicRadio to the GUI so the Radio settings
// tab can swap it on Connect / Disconnect. Safe to call once at startup.
func (g *GUI) SetRadio(r *cat.AtomicRadio) {
	g.mu.Lock()
	g.radio = r
	g.mu.Unlock()
}

// LoadSavedRadio returns the persisted radio profile (type, port, baud)
// and ok=true if the user has configured one via Settings → Radio. main.go
// prefers this over auto-detect so an explicit choice survives reboots and
// a missing rig at launch is a graceful "no radio" rather than a stall on
// the auto-detect probe.
func (g *GUI) LoadSavedRadio() (rType, port string, baud int, ok bool) {
	if g.app == nil {
		return "", "", 0, false
	}
	prefs := g.app.Preferences()
	rType = prefs.String("radio_type")
	port = prefs.String("radio_port")
	baud = prefs.IntWithFallback("radio_baud", 0)
	if rType == "" || port == "" {
		return "", "", 0, false
	}
	if baud <= 0 {
		if kr, found := cat.RadioByType(rType); found {
			baud = kr.Baud
		}
	}
	return rType, port, baud, true
}

// refreshUserBar updates the bottom-of-sidebar user panel (callsign + grid).
// Safe from any goroutine; widget updates dispatched via fyne.Do.
func (g *GUI) refreshUserBar() {
	if g.userCallText == nil {
		return
	}
	g.mu.Lock()
	myCall := g.myCall
	myGrid := g.myGrid
	g.mu.Unlock()
	callDisplay := myCall
	if callDisplay == "" {
		callDisplay = "(no call)"
	}
	gridDisplay := myGrid
	if gridDisplay == "" {
		gridDisplay = "set in ⚙"
	}
	fyne.Do(func() {
		g.userCallText.Text = callDisplay
		g.userCallText.Refresh()
		g.userGridText.Text = gridDisplay
		g.userGridText.Refresh()
	})
}

// showSettings dispatches to the per-mode settings dialog. The gear
// icon shows whichever dialog matches the active mode chip — FT8
// gets the legacy radio / LoTW / TQSL tabs; MeshCore gets a focused
// dialog covering only the mesh device + advert profile. Keeps each
// dialog tight (every tab is relevant to the current mode) and
// guarantees per-mode prefs never bleed across (FT8 keeps its flat
// `radio_*` / `lotw_*` keys; MeshCore writes to namespaced
// `meshcore.*` keys).
func (g *GUI) showSettings() {
	g.mu.Lock()
	mode := g.activeMode
	g.mu.Unlock()
	if mode == "meshcore" {
		g.showMeshcoreSettings()
		return
	}
	g.showFT8Settings()
}

// showFT8Settings opens the FT8 settings dialog:
//
//   - Profile: operator callsign + grid (writes to fyne.Preferences;
//     SetProfile applies live).
//   - Radio: CAT type / port / baud / TX level.
//   - Map / Decoder: worked-grid overlay toggle, strict ITU filter on
//     weak decodes (both legacy toggles).
//   - LoTW: ARRL Logbook of the World credentials. On save we
//     instantiate lotw.Client, kick off a background sync, and feed
//     the results into the worked / confirmed grid map overlay.
//   - TQSL Upload: ARRL TrustedQSL binary configuration for
//     auto-uploading completed QSOs to LoTW.
//
// All values persist via fyne.Preferences across launches.
func (g *GUI) showFT8Settings() {
	g.mu.Lock()
	curCall := g.myCall
	curGrid := g.myGrid
	app := g.app
	scope := g.scope
	g.mu.Unlock()

	prefs := app.Preferences()

	// ── Profile tab ────────────────────────────────────────────────
	callEntry := widget.NewEntry()
	callEntry.SetText(curCall)
	callEntry.SetPlaceHolder("Your callsign")
	gridEntry := widget.NewEntry()
	gridEntry.SetText(curGrid)
	gridEntry.SetPlaceHolder("Your 4- or 6-char Maidenhead grid")
	rosterStaleEntry := widget.NewEntry()
	rosterStaleEntry.SetText(strconv.Itoa(prefs.IntWithFallback("roster_stale_minutes", 30)))
	rosterStaleEntry.SetPlaceHolder("30 (0 disables)")
	profileForm := widget.NewForm(
		widget.NewFormItem("Callsign", callEntry),
		widget.NewFormItem("Grid", gridEntry),
		widget.NewFormItem("Roster stale (min)", rosterStaleEntry),
	)

	// ── Map / Decoder tab ──────────────────────────────────────────
	overlayDefault := prefs.BoolWithFallback("map_worked_overlay", true)
	ituDefault := prefs.BoolWithFallback("itu_filter", true)
	overlayChk := widget.NewCheck("Tint worked grids on the map (DXCC overlay)", nil)
	overlayChk.SetChecked(overlayDefault)
	ituChk := widget.NewCheck("Strict ITU prefix filter on weak (OSD/AP) decodes", nil)
	ituChk.SetChecked(ituDefault)
	mapForm := widget.NewForm(
		widget.NewFormItem("Map", overlayChk),
		widget.NewFormItem("Decoder", ituChk),
	)

	// ── LoTW tab ───────────────────────────────────────────────────
	// Credentials are stored via fyne.Preferences (kept in the OS-
	// standard prefs path; on macOS that's a plist under
	// ~/Library/Preferences/fyne/com.nocordhf.app). On save we build
	// an lotw.Client and trigger a background sync; the results feed
	// the map's worked / confirmed grid tints.
	lotwUserEntry := widget.NewEntry()
	lotwUserEntry.SetText(prefs.String("lotw_username"))
	lotwUserEntry.SetPlaceHolder("ARRL LoTW username")
	lotwPassEntry := widget.NewPasswordEntry()
	lotwPassEntry.SetText(prefs.String("lotw_password"))
	lotwPassEntry.SetPlaceHolder("ARRL LoTW password")
	lotwStatus := canvas.NewText(g.lotwStatusLine(), color.RGBA{160, 165, 175, 255})
	lotwStatus.TextStyle = fyne.TextStyle{Monospace: true}
	lotwStatus.TextSize = 11
	lotwSyncBtn := widget.NewButton("Sync now", func() {
		// Apply credentials from the field state without closing the
		// dialog so the user can hit "Sync now" and see the result.
		u := strings.TrimSpace(lotwUserEntry.Text)
		p := strings.TrimSpace(lotwPassEntry.Text)
		if u == "" || p == "" {
			lotwStatus.Text = "Enter username + password first"
			lotwStatus.Refresh()
			return
		}
		go func() {
			g.runLoTWSync(u, p, func(msg string) {
				fyne.Do(func() {
					lotwStatus.Text = msg
					lotwStatus.Refresh()
				})
			})
		}()
	})
	lotwForm := widget.NewForm(
		widget.NewFormItem("Username", lotwUserEntry),
		widget.NewFormItem("Password", lotwPassEntry),
	)
	lotwTab := container.NewVBox(
		lotwForm,
		container.NewHBox(lotwSyncBtn, lotwStatus),
		widget.NewLabel("Credentials persist locally; sync runs in the background. Yellow tint = LoTW-known QSO on this band, red tint = LoTW-confirmed QSL."),
	)

	// ── TQSL (LoTW upload) tab ─────────────────────────────────────
	// Wraps the ARRL TrustedQSL binary to sign + upload the running
	// nocordhf.adif log on each completed QSO. The fields mirror the
	// legacy GUI's TQSL panel: binary path, station location
	// (a name configured inside TQSL itself), private-key password,
	// and an auto-upload toggle.
	tqslPathEntry := widget.NewEntry()
	tqslPathEntry.SetText(g.tqslCfg.BinaryPath)
	tqslPathEntry.SetPlaceHolder(tqsl.DefaultMacPath)
	tqslStationSelect := widget.NewSelect(nil, nil)
	tqslStationSelect.PlaceHolder = "Select station location"
	if locs, err := tqsl.StationLocations(); err == nil && len(locs) > 0 {
		tqslStationSelect.Options = locs
		if g.tqslCfg.StationLocation != "" {
			tqslStationSelect.SetSelected(g.tqslCfg.StationLocation)
		}
	}
	tqslCertPwEntry := widget.NewPasswordEntry()
	tqslCertPwEntry.SetText(g.tqslCfg.CertPassword)
	tqslCertPwEntry.SetPlaceHolder("Certificate private-key password (optional)")
	tqslAutoChk := widget.NewCheck("Auto-upload each QSO to LoTW on completion", nil)
	tqslAutoChk.SetChecked(g.tqslAutoUpload)
	tqslStatus := canvas.NewText("", color.RGBA{160, 165, 175, 255})
	tqslStatus.TextStyle = fyne.TextStyle{Monospace: true}
	tqslStatus.TextSize = 11
	if g.tqslCfg.Available() {
		tqslStatus.Text = "TQSL binary OK"
	} else {
		tqslStatus.Text = "TQSL binary not found at the configured path"
	}
	tqslTestBtn := widget.NewButton("Test", func() {
		cfg := &tqsl.Config{BinaryPath: tqslPathEntry.Text}
		if !cfg.Available() {
			tqslStatus.Text = "TQSL binary not found"
			tqslStatus.Refresh()
			return
		}
		if ver, err := cfg.Test(); err != nil {
			tqslStatus.Text = "TQSL test failed: " + err.Error()
		} else {
			tqslStatus.Text = "TQSL OK: " + ver
		}
		tqslStatus.Refresh()
	})
	tqslUploadBtn := widget.NewButton("Upload now", func() {
		cfg := &tqsl.Config{
			BinaryPath:      tqslPathEntry.Text,
			StationLocation: tqslStationSelect.Selected,
			CertPassword:    tqslCertPwEntry.Text,
		}
		if !cfg.Available() {
			tqslStatus.Text = "TQSL binary not found"
			tqslStatus.Refresh()
			return
		}
		path := ""
		if g.adifWriter != nil {
			path = g.adifWriter.Path()
		}
		if path == "" {
			tqslStatus.Text = "No ADIF log to upload yet"
			tqslStatus.Refresh()
			return
		}
		// TQSL refuses to sign a file that doesn't exist on disk yet.
		// nocordhf.adif is created lazily on the first QSO close, so
		// when the user opens settings before any contact has been
		// logged the file is missing. Surface that clearly instead of
		// passing the missing path to TQSL and showing its raw error.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			tqslStatus.Text = "No QSOs logged yet -nothing to upload"
			tqslStatus.Refresh()
			return
		}
		tqslStatus.Text = "Uploading…"
		tqslStatus.Refresh()
		go func() {
			err := cfg.Upload(path)
			fyne.Do(func() {
				if err != nil {
					tqslStatus.Text = "Upload failed: " + err.Error()
				} else {
					tqslStatus.Text = "Upload OK"
				}
				tqslStatus.Refresh()
			})
		}()
	})
	tqslForm := widget.NewForm(
		widget.NewFormItem("Binary path", tqslPathEntry),
		widget.NewFormItem("Station", tqslStationSelect),
		widget.NewFormItem("Cert password", tqslCertPwEntry),
		widget.NewFormItem("", tqslAutoChk),
	)
	tqslTab := container.NewVBox(
		tqslForm,
		container.NewHBox(tqslTestBtn, tqslUploadBtn, tqslStatus),
		widget.NewLabel("TQSL must already have your callsign certificate installed and at least one Station location configured. Auto-upload re-signs the running nocordhf.adif on every QSO completion."),
	)

	// ── Radio tab ──────────────────────────────────────────────────
	// Persisted radio profile: type ("icom" / "yaesu" / "" = none),
	// serial port path, and baud. On Save, the saved values are written
	// to prefs and (if "Connect now" was used) the live AtomicRadio is
	// swapped. Decouples startup from the radio: nocordhf launches even
	// with no rig attached, and the operator picks a profile when ready.
	const radioNone = "None (RX-only)"
	radioTypeOpts := append([]string{radioNone}, cat.RadioTypeNames()...)
	radioTypeSel := widget.NewSelect(radioTypeOpts, nil)
	curRadioType := prefs.String("radio_type")
	if kr, ok := cat.RadioByType(curRadioType); ok {
		radioTypeSel.SetSelected(kr.Name)
	} else {
		radioTypeSel.SetSelected(radioNone)
	}
	availablePorts := cat.ScanPorts()
	radioPortSel := widget.NewSelect(availablePorts, nil)
	radioPortSel.PlaceHolder = "Select serial port"
	if savedPort := prefs.String("radio_port"); savedPort != "" {
		radioPortSel.SetSelected(savedPort)
	}
	radioBaudEntry := widget.NewEntry()
	if b := prefs.IntWithFallback("radio_baud", 0); b > 0 {
		radioBaudEntry.SetText(fmt.Sprintf("%d", b))
	}
	radioBaudEntry.SetPlaceHolder("default for the selected radio")
	radioStatus := canvas.NewText("", color.RGBA{160, 165, 175, 255})
	radioStatus.TextStyle = fyne.TextStyle{Monospace: true}
	radioStatus.TextSize = 11
	g.mu.Lock()
	if g.radio != nil && g.radio.Inner() != nil {
		radioStatus.Text = "Connected"
	} else {
		radioStatus.Text = "Disconnected"
	}
	g.mu.Unlock()
	// Auto-fill baud when type changes if the user hasn't typed a custom value.
	radioTypeSel.OnChanged = func(name string) {
		if kr, ok := cat.RadioByName(name); ok {
			if strings.TrimSpace(radioBaudEntry.Text) == "" {
				radioBaudEntry.SetText(fmt.Sprintf("%d", kr.Baud))
			}
		}
	}
	radioRescanBtn := widget.NewButton("Rescan", func() {
		ports := cat.ScanPorts()
		radioPortSel.Options = ports
		radioPortSel.Refresh()
		if len(ports) == 0 {
			radioStatus.Text = "No serial ports found"
			radioStatus.Refresh()
		}
	})
	radioConnectBtn := widget.NewButton("Connect now", func() {
		g.mu.Lock()
		ar := g.radio
		g.mu.Unlock()
		if ar == nil {
			radioStatus.Text = "Radio control unavailable in this build"
			radioStatus.Refresh()
			return
		}
		name := radioTypeSel.Selected
		if name == "" || name == radioNone {
			ar.Swap(nil)
			radioStatus.Text = "Disconnected"
			radioStatus.Refresh()
			return
		}
		kr, ok := cat.RadioByName(name)
		if !ok {
			radioStatus.Text = "Unknown radio type"
			radioStatus.Refresh()
			return
		}
		port := radioPortSel.Selected
		if port == "" {
			radioStatus.Text = "Pick a serial port first"
			radioStatus.Refresh()
			return
		}
		baud := kr.Baud
		if s := strings.TrimSpace(radioBaudEntry.Text); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				baud = v
			}
		}
		r, err := cat.OpenByType(kr.Type, port, baud)
		if err != nil {
			radioStatus.Text = "Connect failed: " + err.Error()
			radioStatus.Refresh()
			return
		}
		ar.Swap(r)
		radioStatus.Text = fmt.Sprintf("Connected: %s on %s", kr.Name, port)
		radioStatus.Refresh()
		g.AppendSystem(fmt.Sprintf("radio: %s on %s", kr.Name, port))
	})
	// TX audio level slider. On USB-CODEC rigs the rig's ALC follows
	// the audio drive linearly, so this acts as a soft "TX power"
	// control: 0.02 (≈ -34 dBFS) at the bottom for a faint signal,
	// 0.5 (≈ -6 dBFS) at the top for full ALC drive. Default 0.18
	// is a conservative setting that keeps ALC happy while
	// still putting out useful power on most rigs.
	curTxLevel := g.TxLevel()
	txLevelLabel := canvas.NewText(fmt.Sprintf("%.2f (%.0f%%)", curTxLevel, curTxLevel*100/0.5), color.RGBA{200, 205, 215, 255})
	txLevelLabel.TextStyle = fyne.TextStyle{Monospace: true}
	txLevelLabel.TextSize = 11
	txLevelSlider := widget.NewSlider(0.02, 0.5)
	txLevelSlider.Step = 0.01
	txLevelSlider.SetValue(curTxLevel)
	txLevelSlider.OnChanged = func(v float64) {
		g.SetTxLevel(v)
		txLevelLabel.Text = fmt.Sprintf("%.2f (%.0f%%)", v, v*100/0.5)
		txLevelLabel.Refresh()
	}
	txLevelRow := container.NewBorder(nil, nil, nil, txLevelLabel, txLevelSlider)

	radioForm := widget.NewForm(
		widget.NewFormItem("Radio", radioTypeSel),
		widget.NewFormItem("Port", radioPortSel),
		widget.NewFormItem("Baud", radioBaudEntry),
		widget.NewFormItem("TX level", txLevelRow),
	)
	radioTab := container.NewVBox(
		radioForm,
		container.NewHBox(radioConnectBtn, radioRescanBtn, radioStatus),
		widget.NewLabel("Pick \"None\" to run RX-only. Save persists the choice; \"Connect now\" applies it to the running session without closing the dialog. TX level controls the audio drive into the rig's USB CODEC — adjust until the rig's ALC just kisses full scale on transmit."),
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("Profile", profileForm),
		container.NewTabItem("Radio", radioTab),
		container.NewTabItem("Map / Decoder", mapForm),
		container.NewTabItem("LoTW", lotwTab),
		container.NewTabItem("TQSL Upload", tqslTab),
	)
	d := dialog.NewCustomConfirm(
		"NocordHF settings", "Save", "Cancel",
		tabs,
		func(ok bool) {
			if !ok {
				return
			}
			// Persist all non-Profile tabs UNCONDITIONALLY. Earlier the
			// callsign-validation early-return at the bottom would
			// silently throw away the operator's Radio / Map / LoTW /
			// TQSL changes if they happened to also have a malformed
			// callsign in the Profile field. Now those tabs are
			// saved first; only Profile changes get gated on
			// validation.

			// Map / Decoder.
			prefs.SetBool("map_worked_overlay", overlayChk.Checked)
			prefs.SetBool("itu_filter", ituChk.Checked)
			ft8.SetITUFilterEnabled(ituChk.Checked)
			if scope != nil && scope.mapWidget != nil {
				scope.mapWidget.SetShowWorkedOverlay(overlayChk.Checked)
			}

			// Radio: type / port / baud. "None" clears the saved
			// profile so the next launch falls back to auto-detect.
			if rname := radioTypeSel.Selected; rname == "" || rname == radioNone {
				prefs.SetString("radio_type", "")
				prefs.SetString("radio_port", "")
				prefs.SetInt("radio_baud", 0)
			} else if kr, found := cat.RadioByName(rname); found {
				prefs.SetString("radio_type", kr.Type)
				prefs.SetString("radio_port", radioPortSel.Selected)
				baud := kr.Baud
				if s := strings.TrimSpace(radioBaudEntry.Text); s != "" {
					if v, err := strconv.Atoi(s); err == nil && v > 0 {
						baud = v
					}
				}
				prefs.SetInt("radio_baud", baud)
			}

			// LoTW: persist creds, build client, kick off a sync if
			// both fields were supplied. Empty fields clear the
			// stored creds and disable the LoTW client.
			lu := strings.TrimSpace(lotwUserEntry.Text)
			lp := strings.TrimSpace(lotwPassEntry.Text)
			prefs.SetString("lotw_username", lu)
			prefs.SetString("lotw_password", lp)
			if lu != "" && lp != "" {
				go g.runLoTWSync(lu, lp, func(msg string) {
					g.AppendSystem("LoTW: " + msg)
				})
			} else {
				g.mu.Lock()
				g.lotwClient = nil
				g.lotwQSLs = nil
				g.mu.Unlock()
				if scope != nil && scope.mapWidget != nil {
					scope.mapWidget.Refresh()
				}
			}

			// TQSL: binary path / station / cert password / auto-upload.
			tqslBinPath := strings.TrimSpace(tqslPathEntry.Text)
			tqslStation := tqslStationSelect.Selected
			tqslCertPw := tqslCertPwEntry.Text
			prefs.SetString("tqsl_path", tqslBinPath)
			prefs.SetString("tqsl_station", tqslStation)
			prefs.SetString("tqsl_cert_password", tqslCertPw)
			prefs.SetBool("tqsl_auto_upload", tqslAutoChk.Checked)
			g.mu.Lock()
			if g.tqslCfg == nil {
				g.tqslCfg = &tqsl.Config{}
			}
			g.tqslCfg.BinaryPath = tqslBinPath
			g.tqslCfg.StationLocation = tqslStation
			g.tqslCfg.CertPassword = tqslCertPw
			g.tqslAutoUpload = tqslAutoChk.Checked
			g.mu.Unlock()

			// Profile: validated last so an invalid callsign doesn't
			// throw away everything else above.
			newCall := strings.ToUpper(strings.TrimSpace(callEntry.Text))
			newGrid := strings.ToUpper(strings.TrimSpace(gridEntry.Text))
			// Roster stale-after threshold (minutes). Bad input falls
			// back to the previous value rather than rejecting the
			// whole save.
			if mins, err := strconv.Atoi(strings.TrimSpace(rosterStaleEntry.Text)); err == nil && mins >= 0 {
				prefs.SetInt("roster_stale_minutes", mins)
				g.mu.Lock()
				g.rosterStaleMinutes = mins
				g.mu.Unlock()
			}
			if newCall != "" && !isPlausibleCallsign(newCall) {
				dialog.ShowError(fmt.Errorf("%q does not look like a valid callsign; other settings have been saved", newCall), g.window)
				return
			}
			g.SetProfile(newCall, newGrid)
			g.AppendSystem(fmt.Sprintf("profile updated: %s | %s | overlay=%v | itu=%v",
				newCall, newGrid, overlayChk.Checked, ituChk.Checked))
		},
		g.window,
	)
	d.Resize(fyne.NewSize(520, 420))
	d.Show()
}

// showMeshcoreSettings opens the MeshCore-mode settings dialog —
// strictly mesh-relevant tabs (Device + Profile) so an FT8-only
// operator never sees mesh-specific knobs and vice versa. All values
// persist under the namespaced "meshcore.*" preferences and the
// Profile fields are pushed to the live device on save when the
// client is connected; otherwise they're applied on the next
// connect.
func (g *GUI) showMeshcoreSettings() {
	if g.app == nil {
		return
	}
	prefs := g.app.Preferences()

	// ── Device tab ────────────────────────────────────────────────
	// Transport picker at the top swaps between the USB sub-form
	// (board / port / baud) and the BLE sub-form (scan + pick a
	// peripheral). Each side persists its own state under the
	// matching meshcore.* prefs so flipping back and forth doesn't
	// lose the unselected side's pick.
	const (
		meshNone       = "None"
		labelUSB       = "USB Serial"
		labelBLE       = "Bluetooth"
		usbDescription = "USB-attached LoRa devboard running MeshCore companion firmware. Save persists the choice; Connect now applies it without closing the dialog."
		bleDescription = "BLE-connected LoRa devboard running MeshCore companion firmware. Scan finds peripherals advertising the MeshCore service. macOS prompts for Bluetooth permission on first scan."
	)
	deviceStatus := canvas.NewText("", color.RGBA{160, 165, 175, 255})
	deviceStatus.TextStyle = fyne.TextStyle{Monospace: true}
	deviceStatus.TextSize = 11
	g.mcMu.Lock()
	if g.mcClient != nil {
		deviceStatus.Text = "Connected"
	} else {
		deviceStatus.Text = "Not connected"
	}
	g.mcMu.Unlock()

	// USB sub-form ────────────────────────────────────────────────
	boardOpts := append([]string{meshNone}, meshcore.BoardNames()...)
	boardSel := widget.NewSelect(boardOpts, nil)
	if kb, ok := meshcore.BoardByType(prefs.String(mcPrefDeviceBoard)); ok {
		boardSel.SetSelected(kb.Name)
	} else {
		boardSel.SetSelected(meshNone)
	}
	// Filter the port list to plausible USB-CDC / USB-serial devices
	// so the operator isn't picking through Bluetooth pseudo-ports;
	// fall back to the unfiltered list if the heuristic excluded
	// every entry (lets an exotic adapter still be selectable).
	scanPortOpts := func() []string {
		all := meshcore.ScanPorts()
		filt := []string{}
		for _, p := range all {
			if meshcore.IsLikelyMeshCorePort(p) {
				filt = append(filt, p)
			}
		}
		if len(filt) == 0 {
			return all
		}
		return filt
	}
	portSel := widget.NewSelect(scanPortOpts(), nil)
	portSel.PlaceHolder = "Select USB serial port"
	if savedPort := prefs.String(mcPrefDevicePort); savedPort != "" {
		portSel.SetSelected(savedPort)
	}
	baudEntry := widget.NewEntry()
	if b := prefs.IntWithFallback(mcPrefDeviceBaud, 0); b > 0 {
		baudEntry.SetText(fmt.Sprintf("%d", b))
	}
	baudEntry.SetPlaceHolder(fmt.Sprintf("default %d", meshcore.DefaultBaud))
	boardSel.OnChanged = func(name string) {
		if kb, ok := meshcore.BoardByName(name); ok {
			if strings.TrimSpace(baudEntry.Text) == "" {
				baudEntry.SetText(fmt.Sprintf("%d", kb.Baud))
			}
		}
	}
	usbRescanBtn := widget.NewButton("Rescan", func() {
		opts := scanPortOpts()
		portSel.Options = opts
		portSel.Refresh()
		if len(opts) == 0 {
			deviceStatus.Text = "No USB serial ports found"
			deviceStatus.Refresh()
		}
	})
	usbForm := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("Board", boardSel),
			widget.NewFormItem("Port", portSel),
			widget.NewFormItem("Baud", baudEntry),
		),
		container.NewHBox(usbRescanBtn),
	)

	// BLE sub-form ────────────────────────────────────────────────
	bleSelectedAddr := prefs.String(mcPrefBLEAddress)
	bleSelectedName := prefs.String(mcPrefBLEDeviceName)
	bleDeviceLabel := widget.NewLabel(formatBLESelection(bleSelectedName, bleSelectedAddr))
	bleScanBtn := widget.NewButton("Scan…", func() {
		g.showBLEScanDialog(func(addr, name string) {
			bleSelectedAddr = addr
			bleSelectedName = name
			bleDeviceLabel.SetText(formatBLESelection(name, addr))
		})
	})
	bleClearBtn := widget.NewButton("Clear", func() {
		bleSelectedAddr = ""
		bleSelectedName = ""
		bleDeviceLabel.SetText(formatBLESelection("", ""))
	})
	bleForm := container.NewVBox(
		widget.NewForm(widget.NewFormItem("Selected device", bleDeviceLabel)),
		container.NewHBox(bleScanBtn, bleClearBtn),
	)

	// Transport picker + swappable sub-form ───────────────────────
	formStack := container.NewStack(usbForm)
	descLabel := widget.NewLabel(usbDescription)
	descLabel.Wrapping = fyne.TextWrapWord
	currentTransport := prefs.StringWithFallback(mcPrefTransport, mcTransportUSB)
	transportRadio := widget.NewRadioGroup([]string{labelUSB, labelBLE}, nil)
	transportRadio.Horizontal = true
	if currentTransport == mcTransportBLE {
		transportRadio.SetSelected(labelBLE)
		formStack.Objects = []fyne.CanvasObject{bleForm}
		descLabel.SetText(bleDescription)
	} else {
		transportRadio.SetSelected(labelUSB)
	}
	transportRadio.OnChanged = func(sel string) {
		switch sel {
		case labelBLE:
			currentTransport = mcTransportBLE
			formStack.Objects = []fyne.CanvasObject{bleForm}
			descLabel.SetText(bleDescription)
		default:
			currentTransport = mcTransportUSB
			formStack.Objects = []fyne.CanvasObject{usbForm}
			descLabel.SetText(usbDescription)
		}
		formStack.Refresh()
	}

	connectBtn := widget.NewButton("Connect now", func() {
		// Persist the live values for the active transport, then
		// disconnect + reconnect so the running session picks them
		// up without closing the dialog.
		prefs.SetString(mcPrefTransport, currentTransport)
		switch currentTransport {
		case mcTransportBLE:
			if bleSelectedAddr == "" {
				deviceStatus.Text = "Scan and pick a BLE device first"
				deviceStatus.Refresh()
				return
			}
			prefs.SetString(mcPrefBLEAddress, bleSelectedAddr)
			prefs.SetString(mcPrefBLEDeviceName, bleSelectedName)
		default:
			port := portSel.Selected
			if port == "" || boardSel.Selected == meshNone {
				deviceStatus.Text = "Pick a board + port first"
				deviceStatus.Refresh()
				return
			}
			baud := meshcore.DefaultBaud
			if kb, ok := meshcore.BoardByName(boardSel.Selected); ok {
				baud = kb.Baud
			}
			if s := strings.TrimSpace(baudEntry.Text); s != "" {
				if v, err := strconv.Atoi(s); err == nil && v > 0 {
					baud = v
				}
			}
			if kb, ok := meshcore.BoardByName(boardSel.Selected); ok {
				prefs.SetString(mcPrefDeviceBoard, kb.Type)
			}
			prefs.SetString(mcPrefDevicePort, port)
			prefs.SetInt(mcPrefDeviceBaud, baud)
		}
		g.disconnectMeshcore()
		g.connectMeshcore()
		deviceStatus.Text = "Connecting…"
		deviceStatus.Refresh()
	})

	transportRow := container.NewBorder(nil, nil, widget.NewLabel("Transport:"), nil, transportRadio)
	deviceTab := container.NewVBox(
		transportRow,
		formStack,
		container.NewHBox(connectBtn, deviceStatus),
		descLabel,
	)

	// ── Profile tab ───────────────────────────────────────────────
	nameEntry := widget.NewEntry()
	nameEntry.SetText(prefs.String(mcPrefProfileName))
	nameEntry.SetPlaceHolder("Display name shown to other mesh nodes")
	latEntry := widget.NewEntry()
	if v := prefs.FloatWithFallback(mcPrefProfileLat, 0); v != 0 {
		latEntry.SetText(strconv.FormatFloat(v, 'f', 6, 64))
	}
	latEntry.SetPlaceHolder("Latitude (decimal degrees, optional)")
	lonEntry := widget.NewEntry()
	if v := prefs.FloatWithFallback(mcPrefProfileLon, 0); v != 0 {
		lonEntry.SetText(strconv.FormatFloat(v, 'f', 6, 64))
	}
	lonEntry.SetPlaceHolder("Longitude (decimal degrees, optional)")
	profileStatus := canvas.NewText("", color.RGBA{160, 165, 175, 255})
	profileStatus.TextStyle = fyne.TextStyle{Monospace: true}
	profileStatus.TextSize = 11
	advertBtn := widget.NewButton("Send self-advert", func() {
		g.mcMu.Lock()
		client := g.mcClient
		g.mcMu.Unlock()
		if client == nil {
			profileStatus.Text = "Not connected"
			profileStatus.Refresh()
			return
		}
		go func() {
			if err := client.SendSelfAdvert(meshcore.SelfAdvertFlood); err != nil {
				fyne.Do(func() {
					profileStatus.Text = "Advert failed: " + err.Error()
					profileStatus.Refresh()
				})
				return
			}
			fyne.Do(func() {
				profileStatus.Text = "Advert sent"
				profileStatus.Refresh()
			})
		}()
	})
	manualAddCheck := widget.NewCheck("", nil)
	manualAddCheck.SetChecked(prefs.BoolWithFallback(mcPrefProfileManualAdd, false))
	// "Use radio GPS" snaps the manual lat/lon entries to whatever
	// the radio's GNSS chip last reported (T1000-E and similar
	// trackers). Convenient one-click capture; manual edits still
	// override.
	useGPSBtn := widget.NewButton("Use radio GPS", func() {
		g.mcMu.Lock()
		lat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
		lon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
		g.mcMu.Unlock()
		if lat == 0 && lon == 0 {
			profileStatus.Text = "no GPS fix from radio yet"
			profileStatus.Refresh()
			return
		}
		latEntry.SetText(strconv.FormatFloat(lat, 'f', 6, 64))
		lonEntry.SetText(strconv.FormatFloat(lon, 'f', 6, 64))
		profileStatus.Text = fmt.Sprintf("filled from radio GPS: %.6f, %.6f", lat, lon)
		profileStatus.Refresh()
	})
	pickMapBtn := widget.NewButton("Pick on map…", func() {
		g.showMcLocationPicker(latEntry, lonEntry, profileStatus)
	})
	profileForm := widget.NewForm(
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Latitude", latEntry),
		widget.NewFormItem("Longitude", lonEntry),
		widget.NewFormItem("Manual-add contacts", manualAddCheck),
	)
	profileTab := container.NewVBox(
		profileForm,
		container.NewHBox(useGPSBtn, pickMapBtn),
		container.NewHBox(advertBtn, profileStatus),
		widget.NewLabel("Advert name + location are broadcast to other mesh nodes when you Send self-advert (or on the firmware's periodic advert). Leave lat/lon blank to use whatever the radio's GPS reports; explicit values override the GPS."),
		widget.NewLabel("Manual-add: when checked, the radio stops auto-adding every node it hears. New peers must be added explicitly — useful on busy meshes where the contact table fills up."),
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("Device", deviceTab),
		container.NewTabItem("Profile", profileTab),
	)
	d := dialog.NewCustomConfirm(
		"MeshCore settings", "Save", "Cancel",
		tabs,
		func(ok bool) {
			if !ok {
				return
			}
			// Persist the active transport AND both sides' state.
			// Storing both sides means the operator can flip back to
			// the unused transport without re-picking the device.
			prefs.SetString(mcPrefTransport, currentTransport)
			// USB persistence — "None" clears the saved board so the
			// next launch starts disconnected on the USB side.
			if name := boardSel.Selected; name == "" || name == meshNone {
				prefs.SetString(mcPrefDeviceBoard, "")
				prefs.SetString(mcPrefDevicePort, "")
				prefs.SetInt(mcPrefDeviceBaud, 0)
			} else if kb, found := meshcore.BoardByName(name); found {
				prefs.SetString(mcPrefDeviceBoard, kb.Type)
				prefs.SetString(mcPrefDevicePort, portSel.Selected)
				baud := kb.Baud
				if s := strings.TrimSpace(baudEntry.Text); s != "" {
					if v, err := strconv.Atoi(s); err == nil && v > 0 {
						baud = v
					}
				}
				prefs.SetInt(mcPrefDeviceBaud, baud)
			}
			// BLE persistence.
			prefs.SetString(mcPrefBLEAddress, bleSelectedAddr)
			prefs.SetString(mcPrefBLEDeviceName, bleSelectedName)
			// Profile persistence + live push to the radio.
			advertName := strings.TrimSpace(nameEntry.Text)
			prefs.SetString(mcPrefProfileName, advertName)
			latF, _ := strconv.ParseFloat(strings.TrimSpace(latEntry.Text), 64)
			lonF, _ := strconv.ParseFloat(strings.TrimSpace(lonEntry.Text), 64)
			prefs.SetFloat(mcPrefProfileLat, latF)
			prefs.SetFloat(mcPrefProfileLon, lonF)
			manualAdd := manualAddCheck.Checked
			prefs.SetBool(mcPrefProfileManualAdd, manualAdd)
			g.mcMu.Lock()
			client := g.mcClient
			g.mcMu.Unlock()
			if client != nil {
				go func() {
					if advertName != "" {
						if err := client.SetAdvertName(advertName); err != nil {
							g.mcAppendSystem("SetAdvertName: " + err.Error())
						}
					}
					if latF != 0 || lonF != 0 {
						latE6, lonE6 := meshcore.LatLonToE6(latF, lonF)
						if err := client.SetAdvertLatLon(latE6, lonE6); err != nil {
							g.mcAppendSystem("SetAdvertLatLon: " + err.Error())
						}
					}
					if err := client.SetManualAddContacts(manualAdd); err != nil {
						g.mcAppendSystem("SetManualAddContacts: " + err.Error())
					}
				}()
			}
		},
		g.window,
	)
	d.Resize(fyne.NewSize(520, 360))
	d.Show()
}

// formatBLESelection returns the user-facing label shown next to
// "Selected device" in the MeshCore Device tab. Empty selection
// renders as a hint line; a real selection shows name + address so
// the operator can tell two same-named boards apart.
func formatBLESelection(name, addr string) string {
	switch {
	case addr == "":
		return "(none — tap Scan to find one)"
	case name == "":
		return addr
	default:
		return name + "  ·  " + addr
	}
}

// showBLEScanDialog runs a BLE scan and renders discovered MeshCore
// devices in a modal list. Tapping a row fires onSelect with the
// chosen address + display name and closes the dialog. Cancel
// dismisses without selecting. Scan runs in a goroutine so the UI
// stays responsive during the (~5 s) scan window.
func (g *GUI) showBLEScanDialog(onSelect func(addr, name string)) {
	status := canvas.NewText("Scanning…", color.RGBA{180, 185, 195, 255})
	status.TextStyle = fyne.TextStyle{Italic: true}
	status.TextSize = 11

	var (
		devices    []meshcore.DiscoveredBLEDevice
		devicesMu  sync.Mutex
		listWidget *widget.List
	)
	listWidget = widget.NewList(
		func() int {
			devicesMu.Lock()
			defer devicesMu.Unlock()
			return len(devices)
		},
		func() fyne.CanvasObject {
			t := canvas.NewText("", color.RGBA{220, 220, 230, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 12
			return container.NewPadded(t)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			padded := obj.(*fyne.Container)
			t := padded.Objects[0].(*canvas.Text)
			devicesMu.Lock()
			if id >= len(devices) {
				devicesMu.Unlock()
				return
			}
			d := devices[id]
			devicesMu.Unlock()
			label := d.Name
			if label == "" {
				label = "(unnamed)"
			}
			t.Text = fmt.Sprintf("%-24s  %4d dBm  %s", label, d.RSSI, d.Address)
			t.Refresh()
		},
	)

	// Holder so the OnSelected closure can dismiss the dialog.
	var d dialog.Dialog
	listWidget.OnSelected = func(id widget.ListItemID) {
		devicesMu.Lock()
		if id >= len(devices) {
			devicesMu.Unlock()
			return
		}
		picked := devices[id]
		devicesMu.Unlock()
		if onSelect != nil {
			onSelect(picked.Address, picked.Name)
		}
		if d != nil {
			d.Hide()
		}
	}

	body := container.NewBorder(nil, container.NewPadded(status), nil, nil, listWidget)
	d = dialog.NewCustomConfirm("MeshCore BLE devices", "Done", "Cancel", body, func(bool) {}, g.window)
	d.Resize(fyne.NewSize(480, 320))
	d.Show()

	go func() {
		found, err := meshcore.ScanBLE(meshcore.DefaultBLEScanDuration)
		fyne.Do(func() {
			devicesMu.Lock()
			devices = found
			devicesMu.Unlock()
			switch {
			case err != nil:
				status.Text = "Scan failed: " + err.Error()
			case len(found) == 0:
				status.Text = "No MeshCore devices found — make sure the device is powered on and advertising."
			default:
				status.Text = fmt.Sprintf("Found %d device(s) — tap one to select.", len(found))
			}
			status.Refresh()
			listWidget.Refresh()
		})
	}()
}

// lotwStatusLine returns a one-line summary of the LoTW client state for
// display in the settings dialog. Reflects whether credentials are set
// and how many QSLs the last sync produced.
func (g *GUI) lotwStatusLine() string {
	g.mu.Lock()
	cli := g.lotwClient
	n := len(g.lotwQSLs)
	g.mu.Unlock()
	if cli == nil || !cli.Configured() {
		return "Not configured"
	}
	if n == 0 {
		return "Configured (no QSLs synced yet)"
	}
	return fmt.Sprintf("%d QSL/QSO records cached", n)
}

// runLoTWSync (re)builds the LoTW client and kicks off a background
// sync. status is fired with progress lines so the caller can update
// either the dialog status label or the chat system stream. Safe to
// call repeatedly; replaces the existing client.
func (g *GUI) runLoTWSync(username, password string, status func(string)) {
	if status == nil {
		status = func(string) {}
	}
	cli, err := lotw.New(username, password)
	if err != nil {
		status("Init failed: " + err.Error())
		return
	}
	g.mu.Lock()
	g.lotwClient = cli
	scope := g.scope
	g.mu.Unlock()
	status("Syncing…")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := cli.Sync(ctx)
	if err != nil {
		status("Sync failed: " + err.Error())
		return
	}
	g.mu.Lock()
	g.lotwQSLs = res.QSLs
	g.mu.Unlock()
	if scope != nil && scope.mapWidget != nil {
		fyne.Do(func() { scope.mapWidget.Refresh() })
	}
	status(fmt.Sprintf("Synced: %d records (%d new)", len(res.QSLs), res.Fresh))
}

// CurrentBand returns the user-selected band (used by main.go to route audio
// frames and decode results to this view's chat pane).
func (g *GUI) CurrentBand() Band {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current
}

// SetBandActivity registers the callback used to render activity counts
// next to each band in the channel list (e.g. "#20m (123)"). Callback is
// expected to be cheap -it's invoked on every band-list redraw. Pass nil
// to clear. Safe from any goroutine.
func (g *GUI) SetBandActivity(fn func(band string) int) {
	g.mu.Lock()
	g.bandActivity = fn
	g.mu.Unlock()
	g.RefreshBandList()
}

// RefreshBandList triggers a redraw of the channel/band column. Called from
// the pskreporter refresh goroutine after new stats land.
func (g *GUI) RefreshBandList() {
	if g.bandList == nil {
		return
	}
	fyne.Do(func() { g.bandList.Refresh() })
}

// Window returns the underlying Fyne window so main.go can call Show/ShowAndRun.
func (g *GUI) Window() fyne.Window { return g.window }

// AppendDecode renders one decoder result as a chat line. Safe from any
// goroutine -the GUI thread re-reads via g.chatList.Refresh().
func (g *GUI) AppendDecode(d ft8.Decoded) {
	if d.Message.Text == "" {
		return
	}
	row := chatRow{
		when:   d.SlotStart,
		freqHz: d.Freq,
		snrDB:  d.SNR,
		text:   d.Message.Text,
		method: d.Method,
	}
	if remote := remoteCallFromMessage(d.Message.Text); remote != "" {
		row.region = callsign.ShortCode(remote)
	}
	// Mark rows whose SENDER is already in the log on the current
	// band so the operator can spot known calls / fresh DX at a
	// glance without scrolling through ADIF or the map.
	if sender := senderFromMessage(d.Message.Text); sender != "" {
		row.confirmStatus = g.confirmStatusForCall(sender)
		// Async HamDB lookup for accurate map placement. Idempotent
		// per session; cache hits apply inline.
		g.lookupHamdbAsync(sender)
	}
	// Mark messages addressed to us (first token == myCall) for highlighting.
	g.mu.Lock()
	myCall := g.myCall
	g.mu.Unlock()
	if myCall != "" {
		fields := strings.Fields(d.Message.Text)
		if len(fields) >= 1 && strings.EqualFold(fields[0], myCall) {
			row.addrUs = true
		}
	}
	// Track the SENDER for the HEARD sidebar -the operator who keyed up,
	// not the recipient of a directed call. senderFromMessage handles the
	// CQ vs directed distinction; we ignore <hashed> sender placeholders
	// since the actual call is unknown. Runs whether or not myCall is
	// set -RX-only sessions still build the list.
	if sender := senderFromMessage(d.Message.Text); sender != "" {
		isCQ := strings.HasPrefix(strings.ToUpper(strings.TrimSpace(d.Message.Text)), "CQ")
		otaType := messageIndicatesOTA(d.Message.Text)
		g.rememberHeard(sender, d.SNR, isCQ, otaType)
		// Record their TX period so we can pick the opposite when
		// calling them. Hashed senders ("<...>") are skipped — we'd
		// just be poisoning the map with placeholder keys.
		if !strings.HasPrefix(sender, "<") {
			period := int((d.SlotStart.Unix()/15)%2) & 1
			g.mu.Lock()
			g.peerPeriods[strings.ToUpper(sender)] = period
			g.mu.Unlock()
		}
	}
	// Emit a thin slot-separator row the first time we hear a decode from
	// a new 15-second slot, so RX cycles have visible breaks in the chat.
	// First decode after launch (lastChatSlot == 0) doesn't draw one —
	// nothing above to separate from.
	slotSec := d.SlotStart.Unix() - d.SlotStart.Unix()%15
	g.mu.Lock()
	emitSep := g.lastChatSlot != 0 && slotSec != g.lastChatSlot
	g.lastChatSlot = slotSec
	if slotSec != g.currentSlotSec {
		g.lastSlotDecodes = g.currentSlotDecodes
		g.currentSlotDecodes = 0
		g.currentSlotSec = slotSec
	}
	g.currentSlotDecodes++
	g.mu.Unlock()
	if emitSep {
		g.appendRow(chatRow{when: d.SlotStart, separator: true})
	}
	g.appendRow(row)
	// Feed the QSO tracker. If this decode closed an in-progress
	// contact (RR73 / 73 addressed to us), the tracker fires its
	// onLogged callback which persists the ADIF record + refreshes
	// the map overlay.
	if g.qso != nil {
		g.qso.FireRX(d.Message.Text, int(d.SNR), d.SlotStart)
	}
	// Auto-progress: when an inbound message addresses us with a sig
	// report / R-report / RR73 / grid, queue the right next-step reply
	// so the operator doesn't have to manually craft each leg of the
	// QSO. Stateless — purely a function of the message just heard —
	// but deduped per-slot to absorb Costas-hit duplicates.
	//
	// Diagnostic Info-log on every addrUs decode so post-mortem analysis
	// can tell whether the listener saw the message at all (BP decodes
	// otherwise leave no trace in the rescue log) and which path
	// followed: gated off, skipped (no recognized tail), deduped, or
	// queued.
	if row.addrUs {
		// Flip the map's QSO arc to point back at the caller.
		// SetQSOPartner with (0,0) lat/lon falls back to the spot DB
		// at draw time, so anyone we've previously decoded will have
		// a known position. The most recent of TX-echo / addr-us
		// decode wins — matches the "last call only" UX.
		sender := senderFromMessage(d.Message.Text)
		g.mu.Lock()
		scope := g.scope
		auto := g.autoReply
		g.mu.Unlock()
		if scope != nil && scope.mapWidget != nil && sender != "" {
			scope.mapWidget.SetQSOPartner(sender, 0, 0, false)
		}
		// They responded — drop any in-flight retry for this call so
		// sweepPendingRetries doesn't keep re-TXing the previous leg.
		// maybeAutoReply below will register a fresh entry for the
		// next QSO step if one applies.
		g.clearPendingRetry(sender)
		if logging.L != nil {
			logging.L.Infow("addr_us decode",
				"msg", d.Message.Text,
				"snr", int(d.SNR),
				"slot", d.SlotStart.Format("15:04:05"),
				"auto", auto,
				"my_call", myCall,
			)
		}
		if g.txCh != nil && myCall != "" && auto {
			g.maybeAutoReply(d, myCall, slotSec)
		}
	}
}

// maybeAutoReply builds and queues a reply TX for an inbound directed
// message addressed at us, if autoReplyTail recognises the trailing
// token. Caller has already established that d.Message.Text starts
// with myCall.
func (g *GUI) maybeAutoReply(d ft8.Decoded, myCall string, slotSec int64) {
	// Use senderFromMessage (returns fields[1] for "X Y Z") not
	// remoteCallFromMessage (returns fields[0] = the recipient = us, for
	// any message addressed at us). The previous wiring extracted our
	// own call as the "remote" and then bailed at the equality check
	// below, so auto-reply never fired for anything addressed to us.
	remote := senderFromMessage(d.Message.Text)
	if remote == "" || strings.EqualFold(remote, myCall) {
		if logging.L != nil {
			logging.L.Infow("auto-reply skipped: no remote call",
				"msg", d.Message.Text, "remote", remote)
		}
		return
	}
	tail := autoReplyTail(d.Message.Text, myCall, int(d.SNR))
	if tail == "" {
		if logging.L != nil {
			logging.L.Infow("auto-reply skipped: no recognised tail",
				"msg", d.Message.Text, "remote", remote)
		}
		return
	}
	key := fmt.Sprintf("%d|%s|%s", slotSec, strings.ToUpper(remote), tail)
	g.mu.Lock()
	if g.lastAutoReplyKey == key {
		g.mu.Unlock()
		if logging.L != nil {
			logging.L.Infow("auto-reply skipped: duplicate this slot",
				"key", key)
		}
		return
	}
	g.lastAutoReplyKey = key
	myGrid := g.myGrid
	g.mu.Unlock()

	req := TxRequest{
		Callsign:    myCall,
		Grid:        myGrid,
		RemoteCall:  remote,
		Tail:        tail,
		AvoidPeriod: g.peerPeriod(remote),
		StopCh:      make(chan struct{}),
	}
	select {
	case g.txCh <- req:
		g.AppendSystem(fmt.Sprintf("auto-reply queued: %s %s %s", remote, myCall, tail))
		if logging.L != nil {
			logging.L.Infow("auto-reply queued",
				"remote", remote, "tail", tail, "avoid_period", req.AvoidPeriod)
		}
		// Register / refresh the retry entry so the 1 Hz sweep
		// (sweepPendingRetries) re-queues this same TX up to
		// retryMaxAttempts times if the remote doesn't respond. Reset
		// counter when the tail changes (we've moved to the next QSO
		// step; the operator effectively ack'd the previous one).
		//
		// Skip "73" — that's the terminal courtesy reply after the
		// remote sent us RR73, so they've already closed; resending
		// 73 serves no protocol purpose and wastes a slot. (RR73 is
		// still retried: if they didn't hear it they need our ack to
		// finish their side of the QSO.)
		if tail != "73" {
			g.mu.Lock()
			ru := strings.ToUpper(remote)
			prev, ok := g.pendingRetries[ru]
			if ok && prev.tail == tail {
				prev.attempts++
				prev.lastSent = time.Now()
				prev.stopCh = req.StopCh
			} else {
				// Different tail (or first time): close the previous
				// in-flight TX (if any) so a stale retry sitting in
				// txCh from the prior QSO step doesn't fire after
				// the QSO has moved on.
				if ok {
					closeStopCh(prev.stopCh)
				}
				g.pendingRetries[ru] = &pendingRetry{
					tail:     tail,
					attempts: 1,
					lastSent: time.Now(),
					stopCh:   req.StopCh,
				}
			}
			g.mu.Unlock()
		}
	default:
		// TX queue full (operator has manual TXs pending). Drop silently;
		// they can manually click Reply if they want this one through.
		if logging.L != nil {
			logging.L.Warnw("auto-reply dropped: tx queue full",
				"remote", remote, "tail", tail)
		}
	}
}

// sweepPendingRetries walks the pendingRetries map and re-queues any
// auto-reply whose remote hasn't responded within retryWait, up to
// retryMaxAttempts. Called once a second by the status ticker.
//
// "Responded" is detected at AppendDecode time (see clearPendingRetry):
// any inbound from the entry's call addressed at us deletes the entry,
// so the sweep below only sees calls we're still waiting on.
func (g *GUI) sweepPendingRetries() {
	g.mu.Lock()
	myCall, myGrid := g.myCall, g.myGrid
	auto := g.autoReply
	if !auto || myCall == "" || g.txCh == nil {
		g.mu.Unlock()
		return
	}
	now := time.Now()
	type todo struct {
		remote, tail string
		avoid        int
	}
	var requeue []todo
	for call, p := range g.pendingRetries {
		if now.Sub(p.lastSent) < retryWait {
			continue
		}
		if p.attempts >= retryMaxAttempts {
			delete(g.pendingRetries, call)
			if logging.L != nil {
				logging.L.Infow("auto-reply gave up",
					"remote", call, "tail", p.tail, "attempts", p.attempts)
			}
			continue
		}
		requeue = append(requeue, todo{call, p.tail, g.peerPeriods[call]})
		p.attempts++
		p.lastSent = now
	}
	g.mu.Unlock()
	// Queue outside the lock — txCh send + AppendSystem can both block
	// briefly and we don't want to hold g.mu for them.
	for _, t := range requeue {
		avoid := t.avoid
		if _, ok := g.peerPeriods[t.remote]; !ok {
			avoid = -1
		}
		req := TxRequest{
			Callsign: myCall, Grid: myGrid,
			RemoteCall: t.remote, Tail: t.tail,
			AvoidPeriod: avoid,
			StopCh:      make(chan struct{}),
		}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("retry: %s %s %s", t.remote, myCall, t.tail))
			// Stash the new TX's stop channel so clearPendingRetry
			// can cancel this in-flight retry the moment the remote
			// responds — otherwise a stale retry already sitting in
			// txCh will fire after the QSO has moved on.
			g.mu.Lock()
			if p := g.pendingRetries[t.remote]; p != nil {
				closeStopCh(p.stopCh) // belt + braces against any prior in-flight
				p.stopCh = req.StopCh
			}
			g.mu.Unlock()
		default:
			// Queue full — leave the entry in pendingRetries for next sweep.
		}
	}
}

// advanceTxRows nudges the txProgress on every in-progress TX row by
// the proportion of the FT8 audio duration that's elapsed since the
// row was created. Called once a second by the status ticker; cheap
// (walks chat history, only mutates rows currently animating).
//
// When elapsed >= txAudioDuration the row is marked complete
// (txInProgress=false), at which point the renderer treats it as a
// normal finished-TX echo (full text rendered green).
func (g *GUI) advanceTxRows() {
	g.mu.Lock()
	now := time.Now()
	dirty := false
	for i := range g.rows {
		r := &g.rows[i]
		if !r.tx || !r.txInProgress {
			continue
		}
		runeLen := len([]rune(r.text))
		elapsed := now.Sub(r.txStart)
		if elapsed >= txAudioDuration {
			if r.txProgress != runeLen || r.txInProgress {
				r.txProgress = runeLen
				r.txInProgress = false
				dirty = true
			}
			continue
		}
		want := int(float64(runeLen) * elapsed.Seconds() / txAudioDuration.Seconds())
		if want > runeLen {
			want = runeLen
		}
		if want != r.txProgress {
			r.txProgress = want
			dirty = true
		}
	}
	chatList := g.chatList
	g.mu.Unlock()
	if dirty && chatList != nil {
		fyne.Do(func() { chatList.Refresh() })
	}
}

// sweepStaleRoster purges HEARD-list entries and map spots that haven't
// been refreshed within rosterStaleMinutes. Called once a second by the
// status ticker; cheap when the threshold isn't crossed (just walks the
// maps comparing timestamps). A 0-minute threshold disables the sweep
// so the operator can opt out via the Settings pref.
func (g *GUI) sweepStaleRoster() {
	g.mu.Lock()
	mins := g.rosterStaleMinutes
	g.mu.Unlock()
	if mins <= 0 {
		return
	}
	maxAge := time.Duration(mins) * time.Minute
	cutoff := time.Now().Add(-maxAge)

	g.mu.Lock()
	heardRemoved := 0
	for k, e := range g.heard {
		if !e.lastSeen.After(cutoff) {
			delete(g.heard, k)
			heardRemoved++
		}
	}
	scope := g.scope
	usersList := g.usersList
	g.mu.Unlock()

	spotsRemoved := 0
	if scope != nil && scope.mapWidget != nil {
		spotsRemoved = scope.mapWidget.RemoveStaleSpots(maxAge)
	}
	if heardRemoved > 0 || spotsRemoved > 0 {
		if logging.L != nil {
			logging.L.Infow("roster sweep",
				"heard_removed", heardRemoved,
				"spots_removed", spotsRemoved,
				"max_age_min", mins,
			)
		}
		if usersList != nil {
			fyne.Do(func() { usersList.Refresh() })
		}
		if scope != nil && scope.mapWidget != nil && spotsRemoved > 0 {
			fyne.Do(func() { scope.mapWidget.Refresh() })
		}
	}
}

// clearPendingRetry drops any in-flight retry for `call`. Called from
// AppendDecode when an inbound from `call` addressed at us arrives:
// they responded, so the previous TX leg succeeded and the next leg
// (if any) will register a fresh entry via maybeAutoReply.
// cancelAllSends aborts every TX in flight or queued: the actively-
// playing TX (closes its stop channel), every TxRequest still sitting
// in g.txCh (drained, each with its StopCh closed), and every
// pendingRetry entry (StopCh closed + map cleared).
//
// Called from the ESC key shortcut (verbose=true) and from the
// waterfall double-tap path (verbose=false). The double-tap is an
// implicit takeover — the operator clearly meant to retune — so a
// chat banner about it would just be noise.
func (g *GUI) cancelAllSends(reason string, verbose bool) {
	g.mu.Lock()
	active := g.activeStopCh
	pending := g.pendingRetries
	g.pendingRetries = make(map[string]*pendingRetry)
	g.mu.Unlock()

	closeStopCh(active)
	for _, p := range pending {
		closeStopCh(p.stopCh)
	}
	// Drain queued TxRequests. Non-blocking — stop as soon as the
	// channel is empty. Each drained request gets its StopCh closed
	// in case runTX picked it up between the drain and the close
	// (slot countdown will then exit before keying).
	drained := 0
	for {
		select {
		case req, ok := <-g.txCh:
			if !ok {
				goto done
			}
			closeStopCh(req.StopCh)
			drained++
		default:
			goto done
		}
	}
done:
	if verbose && (drained > 0 || len(pending) > 0 || active != nil) {
		g.AppendSystem(fmt.Sprintf("✕ all sends cancelled (%s)", reason))
	}
}

func (g *GUI) clearPendingRetry(call string) {
	if call == "" {
		return
	}
	key := strings.ToUpper(call)
	g.mu.Lock()
	if p, ok := g.pendingRetries[key]; ok {
		// Cancel the in-flight TX (if any) so a stale retry queued
		// before this response — sitting in txCh waiting its slot —
		// doesn't fire. runTX honours StopCh in both the slot
		// countdown and during playback, so this aborts cleanly
		// regardless of where the TX is in its lifecycle.
		closeStopCh(p.stopCh)
		delete(g.pendingRetries, key)
	}
	g.mu.Unlock()
}

// peerPeriod returns the most-recently-observed TX period (0 or 1) for
// `call`, or -1 if we've never decoded them. Callers feed the result
// into TxRequest.AvoidPeriod so the slot countdown skips that period.
func (g *GUI) peerPeriod(call string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.peerPeriods[strings.ToUpper(strings.TrimSpace(call))]
	if !ok {
		return -1
	}
	return p
}

// recentMsgFromCall walks chat history newest-to-oldest and returns the
// most recent RX row whose sender matches `call` and which addresses us.
// Used by the chat-side Reply affordances (right-click menu, profile
// popup) so they can advance a QSO past the first calling-with-grid
// stage without the click sites needing to thread message context.
//
// Returns ok=false when no such row exists in the visible history.
func (g *GUI) recentMsgFromCall(call string) (msgText string, snr int, ok bool) {
	upper := strings.ToUpper(strings.TrimSpace(call))
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.rows) - 1; i >= 0; i-- {
		r := g.rows[i]
		if r.system || r.tx || r.separator || !r.addrUs {
			continue
		}
		sender := senderFromMessage(r.text)
		if strings.EqualFold(sender, upper) {
			return r.text, int(r.snrDB), true
		}
	}
	return "", 0, false
}

// queueSmartReply is the chat-side analogue of maybeAutoReply: when the
// operator clicks Reply on a station we've been working, look up the
// last message they sent us and queue the right next-step trailer
// (R-report → RR73 → 73). Falls back to the existing first-call-with-
// grid behaviour (handleSubmit) when no advancing-step is implied.
func (g *GUI) queueSmartReply(call string) {
	g.mu.Lock()
	myCall := g.myCall
	g.mu.Unlock()
	if myCall == "" {
		g.handleSubmit(call) // hit the same "no profile" guard
		return
	}
	if msg, snr, ok := g.recentMsgFromCall(call); ok {
		if tail := autoReplyTail(msg, myCall, snr); tail != "" {
			g.input.SetText(call + " " + tail)
			g.handleSubmit(call + " " + tail)
			g.input.SetText("")
			return
		}
	}
	g.input.SetText(call)
	g.handleSubmit(call)
	g.input.SetText("")
}

// AppendSystem renders a synthesised line ("waiting for slot…", "TX done").
func (g *GUI) AppendSystem(text string) {
	g.appendRow(chatRow{when: time.Now(), system: true, text: text})
}

// AppendTxEcho records that we just transmitted; shows up in the chat with
// a TX-distinct style. Also feeds the QSO tracker so a directed call we
// initiated opens a contact and a closing 73 we send finalises one.
// txAudioDuration is the on-air length of one FT8 transmission
// (79 GFSK symbols × 160 ms). Used by the chat to interpolate the
// "characters going green while TXing" animation.
const txAudioDuration = 12640 * time.Millisecond

func (g *GUI) AppendTxEcho(msg string) {
	now := time.Now()
	// Created in-progress: the status ticker (advanceTxRows) walks
	// time.Since(txStart) and fills txProgress; the row renders the
	// transmitted prefix green and the pending suffix grey, then
	// flips fully green once the audio is done.
	g.appendRow(chatRow{
		when:         now,
		tx:           true,
		text:         msg,
		txInProgress: true,
		txStart:      now,
	})
	if g.qso != nil {
		g.qso.FireTX(msg, now)
	}
	// Update the map's QSO arc to point at whoever we just called.
	// CQ is broadcast — no specific destination — so skip those.
	// Coordinates default to (0,0): mapview falls back to the spot
	// database at draw time, so anyone we've previously decoded gets
	// an arc; cold calls to never-decoded stations silently get none.
	g.mu.Lock()
	scope := g.scope
	myCall := g.myCall
	g.mu.Unlock()
	if scope != nil && scope.mapWidget != nil {
		fields := strings.Fields(strings.ToUpper(strings.TrimSpace(msg)))
		if len(fields) >= 2 && fields[0] != "CQ" && !strings.EqualFold(fields[0], myCall) {
			scope.mapWidget.SetQSOPartner(fields[0], 0, 0, true)
		}
	}
	// Push the input-shorthand for this TX onto the chat-input
	// history so the operator can Up-arrow to recall it. Convert the
	// full TX message back to the shorthand handleSubmit accepts:
	//   "CQ X Y"               → "CQ"
	//   "CALL US grid"         → "CALL"        (initial directed call, our own grid)
	//   "CALL US TAIL"         → "CALL TAIL"   (sig report / R / RR73 / 73 / their grid)
	// Auto-replies and retries push the same way, so e.g. retrying a
	// directed call by arrow-Up + Enter Just Works.
	g.mu.Lock()
	myGrid := g.myGrid
	input := g.input
	g.mu.Unlock()
	if input != nil {
		fields := strings.Fields(strings.ToUpper(strings.TrimSpace(msg)))
		var shorthand string
		switch {
		case len(fields) == 0:
			// nothing to push
		case fields[0] == "CQ":
			shorthand = "CQ"
		case len(fields) >= 3:
			shorthand = fields[0]
			if !strings.EqualFold(fields[2], myGrid) {
				shorthand += " " + fields[2]
			}
		default:
			shorthand = fields[0]
		}
		input.push(shorthand)
	}
}

func (g *GUI) appendRow(r chatRow) {
	// Mode gate: in MeshCore mode the chat list is showing a MeshCore
	// thread, so anything coming from the FT8 stack — decodes, TX
	// echoes, AND background system notifications (LoTW sync, QSO
	// logged, radio status, etc.) — must NOT bleed into the live
	// view. Park it in ft8RowsBackup so the FT8 chat is intact when
	// the operator flips back. MeshCore-origin system messages go
	// through mcAppendSystem instead, never through this path.
	g.mu.Lock()
	if g.activeMode == "meshcore" {
		g.ft8RowsBackup = append(g.ft8RowsBackup, r)
		if len(g.ft8RowsBackup) > maxRows {
			g.ft8RowsBackup = g.ft8RowsBackup[len(g.ft8RowsBackup)-maxRows:]
		}
		g.mu.Unlock()
		return
	}
	// Insert in chronological-by-`when` order, not raw insertion order.
	// FT8 decodes arrive ~2 s after their slot ends — so an in-progress
	// TX row added at TX-start (when=now, e.g. 02:18:00) would normally
	// appear ABOVE the prior slot's decodes (when=02:17:45) that get
	// appended later. Walking back to the first row with `when` <= r.when
	// keeps the visible order correct: TX stays at the bottom and
	// late-arriving decodes slot into their proper slot above it.
	insertAt := len(g.rows)
	for insertAt > 0 && g.rows[insertAt-1].when.After(r.when) {
		insertAt--
	}
	if insertAt == len(g.rows) {
		g.rows = append(g.rows, r)
	} else {
		g.rows = append(g.rows, chatRow{})
		copy(g.rows[insertAt+1:], g.rows[insertAt:])
		g.rows[insertAt] = r
	}
	if len(g.rows) > maxRows {
		g.rows = g.rows[len(g.rows)-maxRows:]
	}
	n := len(g.rows)
	frozen := time.Now().Before(g.chatScrollFrozenUntil)
	g.mu.Unlock()
	if g.chatList != nil {
		fyne.Do(func() {
			g.chatList.Refresh()
			if !frozen {
				g.chatList.ScrollTo(n - 1)
			}
		})
	}
}

func (g *GUI) rememberHeard(call string, snr float64, isCQ bool, otaType string) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" || strings.HasPrefix(call, "<") {
		return
	}
	now := time.Now()
	g.mu.Lock()
	if g.heard == nil {
		g.heard = make(map[string]heardEntry)
	}
	entry := g.heard[call]
	entry.snr = snr
	entry.lastSeen = now
	if isCQ {
		entry.lastCQ = now
	}
	if otaType != "" {
		entry.lastOTA = now
		entry.lastOTAType = otaType
	}
	g.heard[call] = entry
	// Cap memory: when the map gets too large, drop the oldest half.
	if len(g.heard) > maxHeard {
		type kv struct {
			call string
			t    time.Time
		}
		all := make([]kv, 0, len(g.heard))
		for k, v := range g.heard {
			all = append(all, kv{k, v.lastSeen})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for i := 0; i < len(all)/2; i++ {
			delete(g.heard, all[i].call)
		}
	}
	g.mu.Unlock()
	if g.usersList != nil {
		fyne.Do(func() { g.usersList.Refresh() })
	}
}

// heardSnapshot returns the HEARD map flattened into a slice sorted by
// most-recently-seen first. Built fresh on every list redraw -small N
// (≤ maxHeard) keeps this trivially cheap. Decouples the list callbacks
// from the live map so they don't need to hold g.mu while drawing.
type heardRow struct {
	call        string
	snr         float64
	when        time.Time
	lastCQ      time.Time
	lastOTA     time.Time
	lastOTAType string
}

func (g *GUI) heardSnapshot() []heardRow {
	g.mu.Lock()
	mode := g.heardSort
	out := make([]heardRow, 0, len(g.heard))
	for c, e := range g.heard {
		out = append(out, heardRow{
			call: c, snr: e.snr, when: e.lastSeen,
			lastCQ: e.lastCQ, lastOTA: e.lastOTA, lastOTAType: e.lastOTAType,
		})
	}
	g.mu.Unlock()
	switch mode {
	case heardSortSNR:
		sort.Slice(out, func(i, j int) bool { return out[i].snr > out[j].snr })
	case heardSortRecent:
		sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	default: // heardSortAlpha
		sort.Slice(out, func(i, j int) bool { return out[i].call < out[j].call })
	}
	return out
}

// senderFromMessage extracts the operator who keyed the transmission
// (used for HEARD): for "CQ X …" the sender is X; for a directed
// message "DEST SENDER …" it's the second token. Returns "" when the
// sender is a hashed placeholder or the message has no recognisable
// callsign in the sender slot.
func senderFromMessage(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	var sender string
	if strings.EqualFold(fields[0], "CQ") {
		// "CQ MOD X GRID" or "CQ X GRID" — modifier set lives in
		// lib/ft8 so this stays aligned with the decoder.
		switch {
		case len(fields) >= 3 && ft8.IsCQModifier(fields[1]):
			sender = fields[2]
		case len(fields) >= 2:
			sender = fields[1]
		}
	} else if len(fields) >= 2 {
		sender = fields[1]
	}
	if sender == "" || strings.HasPrefix(sender, "<") {
		return ""
	}
	return strings.ToUpper(sender)
}

// refreshStatus repaints the chat-pane header with operator-facing status
// info that ISN'T shown elsewhere in the layout: UTC time, FT8 slot
// phase, NTP clock drift, and TX state. The band/call/grid live in the
// channel column so we don't repeat them here.
//
// Called from a 1 Hz ticker (see startStatusTicker) and any time TX
// state or profile changes.
func (g *GUI) refreshStatus() {
	if g.statusText == nil {
		return
	}
	g.mu.Lock()
	txActive := g.txActive
	checker := g.ntpChecker
	curSlot := g.currentSlotSec
	curCount := g.currentSlotDecodes
	prevCount := g.lastSlotDecodes
	mode := g.activeMode
	g.mu.Unlock()
	// MeshCore mode uses a different topic: there's no slot phase, no
	// per-slot decode count, no NTP gating — the relevant context is
	// which contact / channel the operator is currently looking at.
	if mode == "meshcore" {
		label := g.mcThreadLabel()
		statusColor := color.RGBA{170, 175, 185, 255}
		fyne.Do(func() {
			g.statusText.Text = label
			g.statusText.Color = statusColor
			g.statusText.Refresh()
		})
		return
	}

	now := time.Now().UTC()
	slotPhase := now.Second() % 15
	parts := []string{
		fmt.Sprintf("UTC %s", now.Format("15:04:05")),
		fmt.Sprintf("slot +%ds", slotPhase),
	}
	// "decodes" cell. curCount is the live count for the slot the
	// decoder is currently producing results for (increments as each
	// new decode lands), and prevCount is the previous slot's final
	// count. Auto-rolls in AppendDecode when the first decode of a
	// new slot arrives. We display straight from those — don't gate
	// on wall-clock slot equality, because the decoder finishes
	// ~2 s after a slot ends so the "current decoding slot" is
	// always 1 slot behind wall clock and was always reading 0.
	_ = curSlot
	parts = append(parts, fmt.Sprintf("rx: %d (last: %d)", curCount, prevCount))

	statusColor := color.RGBA{170, 175, 185, 255}
	if checker != nil {
		offset, valid := checker.Offset()
		switch {
		case !valid:
			parts = append(parts, "ntp: …")
		case absDur(offset) >= time.Second:
			parts = append(parts, fmt.Sprintf("!drift %+0.2fs", offset.Seconds()))
			statusColor = color.RGBA{220, 80, 80, 255}
		case absDur(offset) >= 500*time.Millisecond:
			parts = append(parts, fmt.Sprintf("drift %+0.2fs", offset.Seconds()))
			statusColor = color.RGBA{230, 160, 60, 255}
		default:
			parts = append(parts, fmt.Sprintf("drift %+0.2fs", offset.Seconds()))
		}
	}
	if txActive {
		parts = append(parts, "TX")
		statusColor = color.RGBA{80, 200, 120, 255}
	}

	fyne.Do(func() {
		g.statusText.Text = strings.Join(parts, " | ")
		g.statusText.Color = statusColor
		g.statusText.Refresh()
	})
}

// absDur returns |d| -saves an `if d < 0 { d = -d }` everywhere.
func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// showChatHelp opens a reference dialog covering chat colour
// conventions, the L/O badges, keyboard / mouse shortcuts, and the
// auto-progress chain. Triggered by the help icon next to the
// topic-bar status line. Mode-aware — MeshCore mode shows a
// mesh-specific reference (channel types, path overlay legend,
// crypto caveats) instead of the FT8 doc.
func (g *GUI) showChatHelp() {
	if g.window == nil {
		return
	}
	g.mu.Lock()
	mode := g.activeMode
	g.mu.Unlock()
	if mode == "meshcore" {
		g.showMcChatHelp()
		return
	}
	const md = `
## Topic bar

**rx N/M** — decodes in the current slot / previous slot.


## Chat colours

- **cyan** — message addressed to your call
- **orange** — one of your open QSO targets is busy with someone else
- **amber** — CQ from a heard station
- **green** — your transmission (splits green / grey while audio plays)


## Prior-contact badges

Right side of each chat row, and on the HEARD list.

- **L** (italic message) — LoTW-confirmed QSL on this band
- **O** — ADIF-logged QSO on this band, no LoTW yet


## Auto checkbox

Next to the chat input. When checked, an inbound directed at you
triggers the right next-step reply automatically:

grid → sig report → R+report → RR73 → 73

Retries up to 4 times, 30 seconds apart, if they don't respond.
**Esc** cancels everything in flight.


## Keyboard

- **Esc** — cancel ALL queued + in-flight TX
- **Enter** (in chat input) — queue what you typed
- **↑ / ↓** (in chat input) — recall previous TX from history


## Mouse

- click a decode box — select the call, scroll chat to it
- double-click waterfall — snap TX freq here AND cancel queued TX
- drag waterfall — live retune TX freq
- right-click any callsign — Profile / Reply / Copy / Open QRZ


## Chat input shorthand

- **CQ** — send a CQ
- **CALL** — first directed call to CALL (sends your grid)
- **CALL TAIL** — send to CALL with TAIL appended, where TAIL is a
  signal report (–10, +03), an R-report (R-10), RR73, 73, or a
  Maidenhead grid (DM13)
- **/tune** — pure-carrier tune (no FT8 modulation)
`
	rt := widget.NewRichTextFromMarkdown(md)
	rt.Wrapping = fyne.TextWrapWord
	scroll := container.NewScroll(rt)
	scroll.SetMinSize(fyne.NewSize(600, 620))
	dialog.ShowCustom("NocordHF — chat reference", "Close", scroll, g.window)
}

// showMcLocationPicker opens a modal with a fresh MapWidget so the
// operator can click anywhere to choose a lat/lon for their advert.
// Centres on the current entry values when present, otherwise the
// radio's GNSS-reported position, otherwise the map's default
// continental view. The OK button writes the latest pick into the
// supplied lat/lon entries; status echoes the click for confirmation.
func (g *GUI) showMcLocationPicker(latEntry, lonEntry *widget.Entry, statusText *canvas.Text) {
	// Pick a starting centre — manual values take priority, then
	// the radio's GNSS-derived position, then a fallback that the
	// MapWidget itself defaults to.
	startLat, _ := strconv.ParseFloat(strings.TrimSpace(latEntry.Text), 64)
	startLon, _ := strconv.ParseFloat(strings.TrimSpace(lonEntry.Text), 64)
	if startLat == 0 && startLon == 0 {
		g.mcMu.Lock()
		startLat = float64(g.mcSelfInfo.AdvLatE6) / 1e6
		startLon = float64(g.mcSelfInfo.AdvLonE6) / 1e6
		g.mcMu.Unlock()
	}
	picker := mapview.NewMapWidget("")
	if startLat != 0 || startLon != 0 {
		picker.SetSelfPosition(startLat, startLon)
		picker.FlyToRadius(startLat, startLon, 15)
	}
	preview := canvas.NewText("Click anywhere on the map to drop a pin.", color.RGBA{200, 205, 215, 255})
	preview.TextStyle = fyne.TextStyle{Monospace: true}
	preview.TextSize = 11
	var pickedLat, pickedLon float64
	var picked bool
	picker.SetOnMapTap(func(lat, lon float64) {
		pickedLat, pickedLon = lat, lon
		picked = true
		picker.SetSelfPosition(lat, lon)
		fyne.Do(func() {
			preview.Text = fmt.Sprintf("Pin: %.6f, %.6f  (click again to move; OK to apply)", lat, lon)
			preview.Refresh()
		})
	})
	body := container.NewBorder(nil, container.NewPadded(preview), nil, nil, picker)
	d := dialog.NewCustomConfirm(
		"Pick location", "OK", "Cancel",
		body,
		func(ok bool) {
			if !ok || !picked {
				return
			}
			latEntry.SetText(strconv.FormatFloat(pickedLat, 'f', 6, 64))
			lonEntry.SetText(strconv.FormatFloat(pickedLon, 'f', 6, 64))
			if statusText != nil {
				statusText.Text = fmt.Sprintf("picked: %.6f, %.6f", pickedLat, pickedLon)
				statusText.Refresh()
			}
		},
		g.window,
	)
	d.Resize(fyne.NewSize(720, 540))
	d.Show()
}

// showMcChatHelp opens the MeshCore-mode reference dialog —
// channel types, sidebar / map symbols, chat-row context menu
// actions, and the crypto caveats operators should know about.
// Triggered by the help icon in the chat header when MeshCore is
// the active mode.
func (g *GUI) showMcChatHelp() {
	const md = `
## Sidebar

**CONTACTS** — every node your radio has heard. Sort + Bulk delete via
the header menu (the firmware caps contacts at MAX_CONTACTS, typically a
few hundred — prune stale entries to free space).
Right-click a contact for **Info** or **Remove**. Delete / Backspace
also removes the selected contact.

**CHANNELS** — provisioned channel slots (max 32). The **+** menu
adds either a **Hashtag Channel** (community channel, key derived
from the name) or a **Private Channel** (operator pastes the
shared 16-byte secret). Right-click for **Info** or **Remove**.


## Channel types

- **Public** — firmware-default channel, key hardcoded as
  ` + "`PUBLIC_GROUP_PSK`" + `. Every MeshCore node has it.
- **Hashtag** — name starts with **#** (e.g. ` + "`#volcano`" + `, ` + "`#meshbud`" + `).
  Key derived as **SHA-256(name)[:16]**. The name itself IS the
  shared secret material — anyone with the channel name can
  decrypt traffic. **Treat as broadcast, not private.**
- **Private** — operator-defined, requires a 16-byte secret
  shared out of band (URL, QR, manual entry).


## Chat row layout

` + "`[time]   +SNR.NdB sender │ message`" + `

Right-click any row for **Info** (timestamp, sender, SNR, delivery
state, captured packet metadata) or **Map Trace** (animate the
route the message took on the map).


## Map symbols

- **◆ You** (yellow) — your station's broadcast position
- **● Repeater** (red) — infrastructure node
- **● Companion** (blue) — chat-capable end node
- **● Room** (green) — group endpoint
- **● Sensor** (orange) — telemetry source
- **── Route** (cyan line) — message-path overlay (lightning fade)
- **● Hop** (cyan dot) — known forwarder along a path
- **○ Unknown hop** (grey ring) — path-hash didn't match any contact

The latest route stays pinned at full alpha; older routes fade
over 5 s.


## RX log pane

Right-click any packet row for **Inspect** (parsed metadata + hex
dump), **Show path on map** (pin the route), or **Clear path**.


## Crypto reference

- Channel cipher: **AES-128-ECB** + **HMAC-SHA-256-truncated-to-2-bytes** MAC.
  Deterministic, no IV. Treat as obfuscation, not strong encryption.
- DMs: **X25519 ECDH** session key (Ed25519 → Curve25519). Firmware
  handles encryption — host never touches the keys.
- Path data: **cleartext** 1-byte hashes per hop.
- Replay protection: **monotonic timestamp per pubkey** — repeaters
  drop packets with an earlier timestamp than the last one seen
  from the same sender. NocordHF re-syncs the device clock hourly.


## Status bar

Active conversation name (DM contact or channel). FT8's slot / NTP
indicators are hidden — irrelevant here.
`
	rt := widget.NewRichTextFromMarkdown(md)
	rt.Wrapping = fyne.TextWrapWord
	scroll := container.NewScroll(rt)
	scroll.SetMinSize(fyne.NewSize(620, 640))
	dialog.ShowCustom("NocordHF — MeshCore reference", "Close", scroll, g.window)
}

// showCallContextMenu opens a small popup menu at the given canvas
// position with operator-relevant actions for `call`. isCQ controls
// whether the directed-call action reads "Reply" (we'd be answering a
// CQ) or "Call" (an unsolicited directed call). Used by all three
// surfaces -chat rows, HEARD list, waterfall decode boxes -so the
// experience is identical regardless of where the operator right-
// clicked.
func (g *GUI) showCallContextMenu(call string, isCQ bool, pos fyne.Position) {
	if g.window == nil || call == "" {
		return
	}
	directedLabel := "Call"
	if isCQ {
		directedLabel = "Reply"
	}
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("Profile", func() { g.showProfile(call, pos) }),
		fyne.NewMenuItem(directedLabel, func() {
			// Routes through queueSmartReply so a Reply on a sig-report
			// row sends R+report (etc.) instead of restarting the call.
			g.queueSmartReply(call)
		}),
		fyne.NewMenuItem("Copy callsign", func() {
			g.window.Clipboard().SetContent(call)
		}),
		fyne.NewMenuItem("Open QRZ", func() {
			_ = openURL(fmt.Sprintf("https://www.qrz.com/db/%s", call))
		}),
	}
	menu := fyne.NewMenu("", items...)
	pop := widget.NewPopUpMenu(menu, g.window.Canvas())
	pop.ShowAtPosition(pos)
}

// callIsCQ checks whether we've recently heard a CQ from this call.
// Drives the menu's Reply-vs-Call wording.
func (g *GUI) callIsCQ(call string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.heard[call]
	if !ok {
		return false
	}
	return !e.lastCQ.IsZero() && time.Since(e.lastCQ) <= 60*time.Second
}

// pinDecodePopup pins the magnification popup at screenPos for the
// given decode. Called from a deliberate single-click on a decode box.
// Replaces any existing preview popup. Subsequent calls (clicking a
// different box) update the body in place at the new pin location.
//
// The pinned popup ignores hover events until dismissed (empty-
// waterfall click), so the operator can mouse over to the action
// buttons without losing it.
func (g *GUI) pinDecodePopup(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position) {
	logging.L.Debugw("pinDecodePopup", "call", call)
	g.renderDecodePopup(call, slotStart, freqHz, screenPos, true /*pin*/)
}

// decodePreviewDebounce is how long hover events coalesce before
// rendering. Cursor sweeps fire onHover many times per second; without
// debounce we'd rebuild + reshow the popup on every pixel of motion.
// 100 ms is short enough to feel responsive when the cursor settles
// on a box, long enough to skip every intermediate target on a sweep.
const decodePreviewDebounce = 100 * time.Millisecond

// previewDecodePopup is called from hover events. It only stashes the
// target and (re)arms the debounce timer; the actual popup render
// happens in renderPendingPreview after the timer fires. Each event
// resets the timer, so a moving cursor never causes a render.
//
// No-op when a popup is currently pinned — the click commitment wins
// over hover until the operator dismisses.
func (g *GUI) previewDecodePopup(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position) {
	g.mu.Lock()
	logging.L.Debugw("previewDecodePopup",
		"call", call, "pinned", g.decodePopupPinned, "popup_nil", g.decodePopup == nil)
	if g.decodePopupPinned {
		g.mu.Unlock()
		return
	}
	// Same call as the currently-displayed preview? Skip — the
	// cursor is jittering inside the same box, no need to redraw.
	// We compare on call (rather than exact pixel position) because
	// every pixel of cursor motion within a box re-fires onHover.
	if g.decodePopup != nil && g.decodePopupCall == call && !g.decodePopupPinned {
		g.mu.Unlock()
		return
	}
	g.decodePreviewPending = decodePreviewTarget{
		call: call, slotStart: slotStart, freqHz: freqHz, pos: screenPos,
	}
	g.armPreviewTimerLocked()
	g.mu.Unlock()
}

// previewDecodePopupEnd is called when the cursor leaves all decode
// boxes. Same debounce path as previewDecodePopup so a quick
// out-then-back doesn't tear down + rebuild the popup.
func (g *GUI) previewDecodePopupEnd() {
	g.mu.Lock()
	if g.decodePopupPinned {
		g.mu.Unlock()
		return
	}
	g.decodePreviewPending = decodePreviewTarget{end: true}
	g.armPreviewTimerLocked()
	g.mu.Unlock()
}

// armPreviewTimerLocked (re)starts the debounce timer. Must hold g.mu.
func (g *GUI) armPreviewTimerLocked() {
	if g.decodePreviewTimer != nil {
		g.decodePreviewTimer.Stop()
	}
	g.decodePreviewTimer = time.AfterFunc(decodePreviewDebounce, g.renderPendingPreview)
}

// renderPendingPreview is the timer callback. Reads the latest pending
// target and either renders the preview popup or hides it.
func (g *GUI) renderPendingPreview() {
	g.mu.Lock()
	if g.decodePopupPinned {
		g.mu.Unlock()
		return
	}
	target := g.decodePreviewPending
	pop := g.decodePopup
	if target.end {
		g.decodePopup = nil
		g.decodePopupContent = nil
		g.decodePopupCall = ""
	}
	g.mu.Unlock()

	if target.end {
		if pop != nil {
			fyne.Do(func() { pop.Hide() })
		}
		return
	}
	g.renderDecodePopup(target.call, target.slotStart, target.freqHz, target.pos, false /*preview*/)
}

// renderDecodePopup is the shared show/update path. pin=true marks
// the popup pinned (subsequent hover events bypass it); pin=false
// keeps it in preview mode (next hover may move or hide it). The
// popup is recreated on every render so its position can change for
// preview mode; widget.NewPopUp's stale-content tracking is otherwise
// fragile across hide-then-reshow cycles.
func (g *GUI) renderDecodePopup(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position, pin bool) {
	if g.scope == nil || g.window == nil {
		return
	}

	body := g.buildDecodePopupBody(call, slotStart, freqHz)
	contentBox := container.NewStack(body)
	bg := canvas.NewRectangle(color.RGBA{30, 32, 38, 245})
	bg.StrokeColor = color.RGBA{90, 95, 105, 255}
	bg.StrokeWidth = 1
	wrapped := container.NewStack(bg, container.NewPadded(contentBox))
	newPop := widget.NewPopUp(wrapped, g.window.Canvas())

	g.mu.Lock()
	oldPop := g.decodePopup
	g.decodePopup = newPop
	g.decodePopupContent = contentBox
	g.decodePopupPinPos = screenPos
	g.decodePopupPinned = pin
	g.decodePopupCall = call
	g.mu.Unlock()

	fyne.Do(func() {
		if oldPop != nil {
			oldPop.Hide()
		}
		newPop.ShowAtPosition(screenPos)
	})
}

// showDecodePopup keeps the old name as a thin wrapper for the HEARD-
// list click path (the only remaining external caller); it pins
// since that's a deliberate user action equivalent to a box click.
func (g *GUI) showDecodePopup(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position) {
	g.pinDecodePopup(call, slotStart, freqHz, screenPos)
}

// buildDecodePopupBody constructs the inner widget tree for a given
// (call, slot, freq). Reused on first show and every subsequent
// in-place update.
func (g *GUI) buildDecodePopupBody(call string, slotStart time.Time, freqHz float64) fyne.CanvasObject {
	// Source slice request stays the same shape; the displayed
	// canvas.Image is what actually grows. SetMinSize below pushes
	// it to the popup's full available width with a generous height,
	// so the magnified slice — the whole point of the popup — fills
	// the panel instead of being squeezed beside the metadata.
	img := g.scope.MagnifiedSignalSlice(slotStart, freqHz, 180, 90)
	g.mu.Lock()
	heard, hasHeard := g.heard[call]
	g.mu.Unlock()

	cc := "  "
	if sc := callsign.ShortCode(call); len(sc) >= 2 {
		cc = sc[len(sc)-2:]
	}
	identText := canvas.NewText(fmt.Sprintf("%s  %s", cc, call), color.RGBA{220, 225, 235, 255})
	identText.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	identText.TextSize = 13
	freqText := canvas.NewText(fmt.Sprintf("%.0f Hz", freqHz), color.RGBA{170, 175, 185, 255})
	freqText.TextStyle = fyne.TextStyle{Monospace: true}
	freqText.TextSize = 10
	snrLine := ""
	if hasHeard {
		snrLine = fmt.Sprintf("SNR %+0.0f dB | %s ago",
			heard.snr, time.Since(heard.lastSeen).Round(time.Second))
	}
	snrText := canvas.NewText(snrLine, color.RGBA{170, 175, 185, 255})
	snrText.TextStyle = fyne.TextStyle{Monospace: true}
	snrText.TextSize = 10

	// Action buttons + metadata stack on the right; the magnified
	// image takes the full left side. Layout split is enforced via
	// HSplit so the operator gets a roughly 2/3 image, 1/3 chrome
	// ratio regardless of how the popup wrapper sizes itself. Image
	// dominates because the magnified slice is the whole point of
	// the popup.
	qrzBtn := widget.NewButton("QRZ", func() {
		_ = openURL(fmt.Sprintf("https://www.qrz.com/db/%s", call))
	})
	profileBtn := widget.NewButton("Profile", func() {
		g.showProfile(call, fyne.Position{})
	})
	callBtn := widget.NewButton("Call", func() {
		g.input.SetText(call)
		g.handleSubmit(call)
		g.input.SetText("")
		g.dismissDecodePopup()
	})
	// No explicit close button: hover-out hides the preview popup
	// automatically, and clicking empty waterfall dismisses a pinned
	// one. The `[✕]` was both redundant and a frequent target for
	// the "popup wants to redraw" interaction loop.
	rightCol := container.NewVBox(
		identText,
		freqText,
		snrText,
		qrzBtn,
		profileBtn,
		callBtn,
	)

	var imageWidget fyne.CanvasObject
	if img != nil {
		ci := canvas.NewImageFromImage(img)
		ci.FillMode = canvas.ImageFillContain
		// Min size sets the popup's overall size — HSplit will scale
		// children proportionally past this. ~260 wide on the left
		// side at 2/3 ≈ 390 popup width with 130-ish on the right.
		ci.SetMinSize(fyne.NewSize(260, 260))
		imageWidget = ci
	} else {
		imageWidget = canvas.NewRectangle(color.Transparent)
	}

	split := container.NewHSplit(imageWidget, rightCol)
	split.SetOffset(0.66) // image gets ~2/3, chrome ~1/3
	return split
}

// hideDecodePopup is a no-op. Kept for callers that may still send a
// hover-end signal. The popup is sticky and only dismisses on
// explicit click (close button, or empty waterfall).
func (g *GUI) hideDecodePopup() {}

// dismissDecodePopup tears down the magnification popup and clears
// the pinned flag so the next hover-preview can take over.
func (g *GUI) dismissDecodePopup() {
	logging.L.Debugw("dismissDecodePopup called")
	g.mu.Lock()
	pop := g.decodePopup
	g.decodePopup = nil
	g.decodePopupContent = nil
	g.decodePopupCall = ""
	g.decodePopupPinned = false
	g.mu.Unlock()
	if pop != nil {
		fyne.Do(func() { pop.Hide() })
	}
}

// showHeardTooltip displays a tiny "country" label near the cursor when
// the operator hovers a HEARD row. Implemented as a non-modal overlay
// container (added directly to the canvas overlays) so it does NOT
// intercept mouse events — earlier the widget.PopUp version sat over
// the row and ate right-clicks before they could reach the row's
// SecondaryTappable handler.
//
// Subsequent calls with a different call rebuild the label; same-call
// re-hovers are no-ops. Position updates come from updateHeardTooltipPos.
func (g *GUI) showHeardTooltip(call, country string) {
	if g.window == nil || country == "" {
		return
	}
	g.mu.Lock()
	if g.heardTooltipHide != nil {
		g.heardTooltipHide.Stop()
		g.heardTooltipHide = nil
	}
	if g.heardTooltipCall == call && g.heardTooltip != nil {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	g.removeHeardTooltipFromCanvas()

	t := canvas.NewText(country, color.RGBA{220, 225, 235, 255})
	t.TextStyle = fyne.TextStyle{Monospace: true}
	t.TextSize = 11
	bg := canvas.NewRectangle(color.RGBA{30, 32, 38, 240})
	bg.StrokeColor = color.RGBA{90, 95, 105, 255}
	bg.StrokeWidth = 1
	wrapped := container.NewStack(bg, container.NewPadded(t))
	wrapped.Resize(wrapped.MinSize())

	g.mu.Lock()
	g.heardTooltip = wrapped
	g.heardTooltipCall = call
	g.mu.Unlock()
	fyne.Do(func() {
		g.window.Canvas().Overlays().Add(wrapped)
	})
}

// updateHeardTooltipPos repositions the tooltip near the cursor. Called
// from the hoverRow MouseMoved handler so the tooltip tracks the
// pointer instead of being pinned to a fixed corner of the column.
func (g *GUI) updateHeardTooltipPos(absPos fyne.Position) {
	g.mu.Lock()
	tip := g.heardTooltip
	g.mu.Unlock()
	if tip == nil {
		return
	}
	fyne.Do(func() {
		// Offset so the tooltip doesn't sit directly under the pointer
		// (which would make it follow micro-jitter and visually crowd
		// the row text being inspected).
		tip.Move(fyne.NewPos(absPos.X+12, absPos.Y+8))
	})
}

// removeHeardTooltipFromCanvas pulls the tooltip off the canvas
// overlay stack if one is currently displayed. Safe to call when none
// is showing.
func (g *GUI) removeHeardTooltipFromCanvas() {
	g.mu.Lock()
	tip := g.heardTooltip
	g.mu.Unlock()
	if tip != nil && g.window != nil {
		fyne.Do(func() {
			g.window.Canvas().Overlays().Remove(tip)
		})
	}
}

// hideHeardTooltip schedules the tooltip to disappear after a short
// debounce so a rapid leave/enter (cursor jitter, list re-binding)
// doesn't tear it down and rebuild visibly.
func (g *GUI) hideHeardTooltip() {
	g.mu.Lock()
	if g.heardTooltipHide != nil {
		g.heardTooltipHide.Stop()
	}
	g.heardTooltipHide = time.AfterFunc(150*time.Millisecond, func() {
		g.mu.Lock()
		tip := g.heardTooltip
		g.heardTooltip = nil
		g.heardTooltipCall = ""
		g.heardTooltipHide = nil
		g.mu.Unlock()
		if tip != nil && g.window != nil {
			fyne.Do(func() {
				g.window.Canvas().Overlays().Remove(tip)
			})
		}
	})
	g.mu.Unlock()
}

// showProfile opens a Discord-style operator profile for the given
// callsign. Pulls everything available locally -country / continent
// from the callsign prefix table, distance + bearing from the operator
// grid, last-heard SNR and timestamp from the HEARD map, and recent
// decoded messages from chat history. Actions: Reply, Copy, Open QRZ.
func (g *GUI) showProfile(call string, _ fyne.Position) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return
	}
	// Country / continent.
	country := "-"
	continent := ""
	if ent, ok := callsign.Lookup(call); ok {
		country = ent.Name
		continent = continentFromShortCode(callsign.ShortCode(call))
	}
	profileCC := ""
	if sc := callsign.ShortCode(call); len(sc) >= 2 {
		profileCC = sc[len(sc)-2:]
	}

	// Heard stats.
	g.mu.Lock()
	heard, hasHeard := g.heard[call]
	myGrid := g.myGrid
	rowsCopy := make([]chatRow, len(g.rows))
	copy(rowsCopy, g.rows)
	g.mu.Unlock()
	snrLine := "not heard this session"
	lastSeenLine := ""
	if hasHeard {
		snrLine = fmt.Sprintf("Last SNR: %+0.0f dB", heard.snr)
		lastSeenLine = fmt.Sprintf("Last heard: %s ago",
			time.Since(heard.lastSeen).Round(time.Second))
	}

	// Recent decoded messages (last 6) from this sender.
	var msgs []string
	for i := len(rowsCopy) - 1; i >= 0 && len(msgs) < 12; i-- {
		r := rowsCopy[i]
		if r.system || r.tx || r.separator {
			continue
		}
		if senderFromMessage(r.text) != call {
			continue
		}
		ts := r.when.UTC().Format("15:04:05")
		msgs = append(msgs, fmt.Sprintf("%s  %+3.0f dB  %s", ts, r.snrDB, r.text))
	}

	// Header: country code + call (large) + country/continent.
	headCC := canvas.NewText(profileCC, color.RGBA{170, 175, 185, 255})
	headCC.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	headCC.TextSize = 16
	headCall := canvas.NewText(call, color.RGBA{220, 225, 235, 255})
	headCall.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	headCall.TextSize = 20
	subtitle := country
	if continent != "" {
		subtitle = continent + " | " + subtitle
	}
	headSub := canvas.NewText(subtitle, color.RGBA{160, 165, 175, 255})
	headSub.TextStyle = fyne.TextStyle{Monospace: true}
	headSub.TextSize = 11
	header := container.NewHBox(
		container.NewPadded(headCC),
		container.NewVBox(headCall, headSub),
	)

	// Stats: SNR, last seen, distance/bearing.
	statLines := []string{snrLine}
	if lastSeenLine != "" {
		statLines = append(statLines, lastSeenLine)
	}
	if myGrid != "" {
		if dist, bearing, ok := approxDistanceBearing(myGrid, call); ok {
			statLines = append(statLines, fmt.Sprintf("~%.0f km | bearing %.0f deg", dist, bearing))
		}
	}
	statsText := canvas.NewText(strings.Join(statLines, "\n"), color.RGBA{200, 205, 215, 255})
	statsText.TextStyle = fyne.TextStyle{Monospace: true}
	statsText.TextSize = 11

	// Recent messages: scrollable list rather than a wall of text. Last
	// 12 decodes from this sender, newest first. Click-tappable for
	// future enhancements (jump to chat row, etc.); for now selection
	// is just visual feedback.
	var recentBlock fyne.CanvasObject
	if len(msgs) > 0 {
		entries := msgs
		recentList := widget.NewList(
			func() int { return len(entries) },
			func() fyne.CanvasObject {
				t := canvas.NewText("", color.RGBA{200, 205, 215, 255})
				t.TextStyle = fyne.TextStyle{Monospace: true}
				t.TextSize = 10
				return container.NewPadded(t)
			},
			func(id widget.ListItemID, obj fyne.CanvasObject) {
				if id >= len(entries) {
					return
				}
				t := obj.(*fyne.Container).Objects[0].(*canvas.Text)
				t.Text = entries[id]
				t.Refresh()
			},
		)
		hdr := canvas.NewText("Recent decodes", color.RGBA{140, 145, 155, 255})
		hdr.TextStyle = fyne.TextStyle{Bold: true}
		hdr.TextSize = 11
		// Border with a fixed-min-height bg so the list always has room
		// for ~6 visible rows in the dialog.
		listSizer := canvas.NewRectangle(color.RGBA{0, 0, 0, 0})
		listSizer.SetMinSize(fyne.NewSize(0, 140))
		recentBlock = container.NewBorder(
			hdr, nil, nil, nil,
			container.NewStack(listSizer, recentList),
		)
	}

	// Actions.
	replyBtn := widget.NewButton("Reply (call this station)", func() {
		g.queueSmartReply(call)
	})
	copyBtn := widget.NewButton("Copy callsign", func() {
		g.window.Clipboard().SetContent(call)
	})
	qrzBtn := widget.NewButton("Open QRZ", func() {
		_ = openURL(fmt.Sprintf("https://www.qrz.com/db/%s", call))
	})
	actions := container.NewHBox(replyBtn, copyBtn, qrzBtn)

	body := container.NewVBox(header, widget.NewSeparator(), statsText)
	if recentBlock != nil {
		body.Add(widget.NewSeparator())
		body.Add(recentBlock)
	}
	body.Add(widget.NewSeparator())
	body.Add(actions)
	d := dialog.NewCustom("Operator profile: "+call, "Close", body, g.window)
	d.Resize(fyne.NewSize(460, 460))
	d.Show()
}

// continentFromShortCode pulls the leading "NA-"/"EU-"/etc. prefix off a
// ShortCode. Empty string for codes that don't have a continent prefix.
func continentFromShortCode(s string) string {
	if i := strings.Index(s, "-"); i > 0 {
		switch s[:i] {
		case "NA":
			return "North America"
		case "SA":
			return "South America"
		case "EU":
			return "Europe"
		case "AS":
			return "Asia"
		case "AF":
			return "Africa"
		case "OC":
			return "Oceania"
		case "AN":
			return "Antarctica"
		}
	}
	return ""
}

// approxDistanceBearing returns the great-circle distance (km) and
// initial bearing (deg, true) from the operator's grid to a station's
// home grid (looked up from the prefix table's lat/lon centroid). Not
// surveying-grade -the prefix table's coords are coarse country
// centroids -but good enough for a profile snapshot.
func approxDistanceBearing(myGrid, call string) (km, bearing float64, ok bool) {
	myLat, myLon, mok := gridToLatLon(myGrid)
	if !mok {
		return 0, 0, false
	}
	ent, eok := callsign.Lookup(call)
	if !eok {
		return 0, 0, false
	}
	d, b := haversineKm(myLat, myLon, ent.Lat, ent.Lon)
	return d, b, true
}

// gridToLatLon converts a 4- or 6-character Maidenhead grid square to
// approximate decimal lat/lon (centroid of the square). Mirrors the
// helper in internal/ui -small enough to inline here so the nocord
// package doesn't pull ui's heavy widget tree.
func gridToLatLon(g string) (lat, lon float64, ok bool) {
	g = strings.ToUpper(strings.TrimSpace(g))
	if len(g) < 4 {
		return 0, 0, false
	}
	A, B := int(g[0]-'A'), int(g[1]-'A')
	c, d := int(g[2]-'0'), int(g[3]-'0')
	if A < 0 || A > 17 || B < 0 || B > 17 || c < 0 || c > 9 || d < 0 || d > 9 {
		return 0, 0, false
	}
	lon = float64(A)*20 - 180 + float64(c)*2 + 1
	lat = float64(B)*10 - 90 + float64(d) + 0.5
	return lat, lon, true
}

// haversineKm returns great-circle distance (km) and initial bearing (deg true).
func haversineKm(lat1, lon1, lat2, lon2 float64) (km, bearing float64) {
	const R = 6371.0
	rad := func(x float64) float64 { return x * math.Pi / 180 }
	φ1, φ2 := rad(lat1), rad(lat2)
	dφ := rad(lat2 - lat1)
	dλ := rad(lon2 - lon1)
	a := math.Sin(dφ/2)*math.Sin(dφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	km = R * c
	y := math.Sin(dλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(dλ)
	bearing = math.Mod(math.Atan2(y, x)*180/math.Pi+360, 360)
	return km, bearing
}

// openURL launches the system browser at the given URL. Best-effort —
// failure is swallowed since the profile dialog stays useful even if
// QRZ can't be opened.
func openURL(u string) error {
	cmd := exec.Command("open", u) // macOS; other platforms can ship later
	return cmd.Start()
}

// lotwWorkedGridsOnActiveBand returns the set of 4-char grids LoTW
// shows we've worked on the current band (confirmed or not). Drives
// the map's yellow tint.
func (g *GUI) lotwWorkedGridsOnActiveBand() map[string]bool {
	g.mu.Lock()
	band := g.current.Name
	out := map[string]bool{}
	for _, q := range g.lotwQSLs {
		if !strings.EqualFold(q.Band, band) {
			continue
		}
		if len(q.Grid) < 4 {
			continue
		}
		out[strings.ToUpper(q.Grid[:4])] = true
	}
	g.mu.Unlock()
	return out
}

// lotwConfirmedGridsOnActiveBand returns the set of 4-char grids that
// LoTW has CONFIRMED (QSL_RCVD=Y) on the current band. Drives the red
// tint, drawn over the yellow.
func (g *GUI) lotwConfirmedGridsOnActiveBand() map[string]bool {
	g.mu.Lock()
	band := g.current.Name
	out := map[string]bool{}
	for _, q := range g.lotwQSLs {
		if !q.Confirmed {
			continue
		}
		if !strings.EqualFold(q.Band, band) {
			continue
		}
		if len(q.Grid) < 4 {
			continue
		}
		out[strings.ToUpper(q.Grid[:4])] = true
	}
	g.mu.Unlock()
	return out
}

// localWorkedGridsOnActiveBand returns the set of 4-character grid
// squares we've worked on the current band. Drives the map overlay
// blue tint. Empty result when the band hasn't been set or the log
// is empty.
func (g *GUI) localWorkedGridsOnActiveBand() map[string]bool {
	g.mu.Lock()
	band := g.current.Name
	out := map[string]bool{}
	for _, r := range g.adifLog {
		if !strings.EqualFold(r.Band, band) {
			continue
		}
		if len(r.TheirGrid) < 4 {
			continue
		}
		out[strings.ToUpper(r.TheirGrid[:4])] = true
	}
	g.mu.Unlock()
	return out
}

// lookupHamdbAsync kicks off a HamDB lookup for `call` so the map's
// pin can be upgraded from coarse-prefix placement to the operator's
// real home coordinates. Idempotent per session — repeated decodes
// from the same call don't re-fire the lookup.
//
// Fast-path: if the on-disk cache already has the record, we apply
// it inline (no network, no goroutine). Otherwise we spawn a goroutine
// that does the network call with a short context, then dispatches
// the upgrade on success.
//
// Skips hashed senders, our own call, anything shorter than 3 chars,
// and silently no-ops when the hamdb client failed to initialise.
func (g *GUI) lookupHamdbAsync(call string) {
	if g.hamdb == nil {
		return
	}
	call = strings.ToUpper(strings.TrimSpace(call))
	if len(call) < 3 || strings.HasPrefix(call, "<") {
		return
	}
	g.mu.Lock()
	if strings.EqualFold(call, g.myCall) {
		g.mu.Unlock()
		return
	}
	if _, seen := g.hamdbSubmitted[call]; seen {
		g.mu.Unlock()
		return
	}
	g.hamdbSubmitted[call] = struct{}{}
	scope := g.scope
	g.mu.Unlock()
	if scope == nil || scope.mapWidget == nil {
		return
	}
	apply := func(rec *hamdb.Record) {
		if rec == nil {
			return
		}
		lat, _ := strconv.ParseFloat(strings.TrimSpace(rec.Lat), 64)
		lon, _ := strconv.ParseFloat(strings.TrimSpace(rec.Lon), 64)
		if lat == 0 && lon == 0 {
			return
		}
		scope.mapWidget.UpgradeSpotLocation(call, strings.ToUpper(rec.Grid), lat, lon)
	}
	if rec, found, hasEntry := g.hamdb.LookupCached(call); hasEntry {
		if found {
			apply(rec)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		rec, err := g.hamdb.Lookup(ctx, call)
		if err != nil {
			return // ErrNotFound + network errors are both normal; cache absorbs both
		}
		apply(rec)
	}()
}

// confirmStatusForCall returns the strongest prior-contact level we
// have with `call` on the current band:
//
//	0 = never worked on this band
//	1 = QSO logged in our ADIF (but not yet LoTW-confirmed)
//	2 = LoTW QSL received (confirmed)
//
// Used by formatRowSegments to flag chat rows with a marker so the
// operator can see at-a-glance which calls are already in the log.
// Per-band so a worked-on-20m flag doesn't show up while we're on 40m.
func (g *GUI) confirmStatusForCall(call string) int {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	band := g.current.Name
	for _, q := range g.lotwQSLs {
		if q.Confirmed && strings.EqualFold(q.Band, band) && strings.EqualFold(q.Call, call) {
			return 2
		}
	}
	for _, r := range g.adifLog {
		if strings.EqualFold(r.Band, band) && strings.EqualFold(r.TheirCall, call) {
			return 1
		}
	}
	return 0
}

// workedStatusForCall classifies a callsign for map-pin colouring:
//
//	0 = not worked on this band  (default green)
//	1 = grid worked but not this call
//	2 = this exact call worked
//
// Mirrors the legacy ui's SetWorkedFunc contract so the existing
// MapWidget logic Just Works.
func (g *GUI) workedStatusForCall(call, grid string) int {
	call = strings.ToUpper(strings.TrimSpace(call))
	g.mu.Lock()
	defer g.mu.Unlock()
	band := g.current.Name
	gridKey := ""
	if len(grid) >= 4 {
		gridKey = strings.ToUpper(grid[:4])
	}
	gridSeen := false
	for _, r := range g.adifLog {
		if !strings.EqualFold(r.Band, band) {
			continue
		}
		if strings.EqualFold(r.TheirCall, call) {
			return 2
		}
		if gridKey != "" && len(r.TheirGrid) >= 4 &&
			strings.EqualFold(r.TheirGrid[:4], gridKey) {
			gridSeen = true
		}
	}
	if gridSeen {
		return 1
	}
	return 0
}

// selectCall is fired when the operator single-clicks a decode box on
// the waterfall. It locks chat auto-scroll for ~30 s, scrolls the
// chat + HEARD lists to the matching call, and starts a blink
// animation on the matching rows so the operator's eye is drawn to
// them. The blink runs for ~3 s before clearing.
func (g *GUI) selectCall(call string) {
	if call == "" {
		return
	}
	now := time.Now()
	g.mu.Lock()
	g.chatScrollFrozenUntil = now.Add(30 * time.Second)
	g.highlightedCall = call
	g.highlightUntil = now.Add(3 * time.Second)
	if g.highlightTimer != nil {
		g.highlightTimer.Stop()
	}
	g.mu.Unlock()
	// Scroll chat list to the most recent row from this sender (any
	// slot — the simpler "find the latest matching row" is more
	// useful than restricting to a specific slot for selection).
	g.scrollChatToCall(call)
	g.scrollHeardToCall(call)
	// Animate the blink: refresh both lists every 250 ms so the row
	// binder reads the alternating highlight state.
	tick := time.NewTicker(250 * time.Millisecond)
	go func() {
		defer tick.Stop()
		for range tick.C {
			g.mu.Lock()
			done := time.Now().After(g.highlightUntil)
			g.mu.Unlock()
			fyne.Do(func() {
				if g.chatList != nil {
					g.chatList.Refresh()
				}
				if g.usersList != nil {
					g.usersList.Refresh()
				}
			})
			if done {
				g.mu.Lock()
				g.highlightedCall = ""
				g.mu.Unlock()
				fyne.Do(func() {
					if g.chatList != nil {
						g.chatList.Refresh()
					}
					if g.usersList != nil {
						g.usersList.Refresh()
					}
				})
				return
			}
		}
	}()
}

// scrollChatToCall finds the most recent chat row from the given
// sender and scrolls / selects it. Used by selectCall.
func (g *GUI) scrollChatToCall(call string) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" || g.chatList == nil {
		return
	}
	g.mu.Lock()
	idx := -1
	for i := len(g.rows) - 1; i >= 0; i-- {
		r := g.rows[i]
		if r.system || r.tx || r.separator {
			continue
		}
		if senderFromMessage(r.text) == call {
			idx = i
			break
		}
	}
	g.mu.Unlock()
	if idx < 0 {
		return
	}
	fyne.Do(func() {
		g.mu.Lock()
		g.suppressChatSelectInput = true
		g.mu.Unlock()
		g.chatList.ScrollTo(idx)
		g.chatList.Select(idx)
	})
}

// scrollHeardToCall finds the call's row in the HEARD list and
// scrolls / selects it.
func (g *GUI) scrollHeardToCall(call string) {
	if g.usersList == nil {
		return
	}
	snap := g.heardSnapshot()
	for i, e := range snap {
		if strings.EqualFold(e.call, call) {
			fyne.Do(func() {
				g.mu.Lock()
				g.suppressHeardSelectAction = true
				g.mu.Unlock()
				g.usersList.ScrollTo(i)
				g.usersList.Select(i)
			})
			return
		}
	}
}

// shouldBlinkCall returns true if the row binder should render call
// in a blink-highlight state on this redraw. Alternates on/off every
// 250 ms while the highlight window is active.
func (g *GUI) shouldBlinkCall(call string) bool {
	g.mu.Lock()
	hl := g.highlightedCall
	until := g.highlightUntil
	g.mu.Unlock()
	if hl == "" || !strings.EqualFold(hl, call) || time.Now().After(until) {
		return false
	}
	// Phase: 4 cycles per second (250 ms steps), even = highlight on.
	return (time.Now().UnixMilli()/250)%2 == 0
}

// scrollChatToDecode finds the chat row matching (slotStart, call) and
// scrolls the chat list to it, selecting it briefly to flash a visual
// cue. Walks the rows in reverse so we hit the most recent matching
// decode if the same station has been heard repeatedly. No-op when no
// match is found.
func (g *GUI) scrollChatToDecode(slotStart time.Time, call string) {
	call = strings.ToUpper(strings.TrimSpace(call))
	slotSec := slotStart.Unix() - slotStart.Unix()%15
	g.mu.Lock()
	idx := -1
	for i := len(g.rows) - 1; i >= 0; i-- {
		r := g.rows[i]
		if r.system || r.tx || r.separator {
			continue
		}
		if r.when.Unix()-r.when.Unix()%15 != slotSec {
			continue
		}
		// Match by sender (HEARD/decode-box keying convention) so a
		// directed reply like "X Y RR73" doesn't false-match the
		// recipient's call.
		if call != "" && senderFromMessage(r.text) != call {
			continue
		}
		idx = i
		break
	}
	g.mu.Unlock()
	if idx < 0 || g.chatList == nil {
		return
	}
	fyne.Do(func() {
		g.mu.Lock()
		g.suppressChatSelectInput = true
		g.mu.Unlock()
		g.chatList.ScrollTo(idx)
		g.chatList.Select(idx)
	})
}

// startStatusTicker fires refreshStatus once a second so the UTC clock
// and slot phase tick in real time. Goroutine lives for the life of the
// GUI; the window-close path stops the process so we don't bother with
// a stop channel.
func (g *GUI) startStatusTicker() {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			g.refreshStatus()
			g.sweepPendingRetries()
			g.sweepStaleRoster()
			g.advanceTxRows()
			g.mcSweepPending()
		}
	}()
}

// buildLayout assembles the three-column Discord-style window. Returns the
// root container; SetContent attaches it to the window.
func (g *GUI) buildLayout() fyne.CanvasObject {
	// Hydrate active-mode from prefs before the chips are constructed
	// so the initial chip palette already reflects the persisted choice
	// and we don't paint FT8-active for one frame on a MeshCore launch.
	if g.app != nil {
		if m := g.app.Preferences().String("active_mode"); m == "ft8" || m == "meshcore" {
			g.activeMode = m
		}
	}
	if g.activeMode == "" {
		g.activeMode = "ft8"
	}
	// ── Far-left rail: mode selector ───────────────────────────────────
	// Discord shows a vertical strip of server icons here. We have two
	// modes — FT8 and MeshCore. Tap a chip to make it the active
	// mode; refreshModeRail repaints the chip palette so the active
	// one stands out (Discord blurple) and the inactive ones recede
	// (slate grey).
	g.ft8Chip = chip("FT8", modeChipFill("ft8", g.activeMode))
	g.meshChip = chip("MESH", modeChipFill("meshcore", g.activeMode))
	ft8Tap := newTappableContainer(g.ft8Chip, func() { g.setActiveMode("ft8") })
	meshTap := newTappableContainer(g.meshChip, func() { g.setActiveMode("meshcore") })
	modeRail := container.NewVBox(ft8Tap, meshTap)
	modeBg := canvas.NewRectangle(color.RGBA{32, 34, 37, 255})
	modeCol := container.NewStack(modeBg, container.NewPadded(modeRail))

	// ── Channel column: bands as #channels ─────────────────────────────
	g.bandList = widget.NewList(
		func() int { return len(DefaultBands) },
		func() fyne.CanvasObject {
			t := canvas.NewText("", color.RGBA{200, 200, 210, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 13
			return container.NewPadded(t)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			padded := obj.(*fyne.Container)
			t := padded.Objects[0].(*canvas.Text)
			b := DefaultBands[id]
			g.mu.Lock()
			selected := b.Name == g.current.Name
			fn := g.bandActivity
			g.mu.Unlock()
			label := "#" + b.Name
			if fn != nil {
				if n := fn(b.Name); n > 0 {
					label = fmt.Sprintf("#%-4s (%d)", b.Name, n)
				}
			}
			t.Text = label
			if selected {
				t.Color = color.RGBA{255, 255, 255, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			} else {
				t.Color = color.RGBA{170, 170, 175, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true}
			}
			t.Refresh()
		},
	)
	g.bandList.OnSelected = func(id widget.ListItemID) {
		b := DefaultBands[id]
		g.mu.Lock()
		g.current = b
		g.mu.Unlock()
		// Tune the radio if we have a CAT channel; non-blocking so a stuck
		// radio doesn't freeze the UI.
		if g.tuneCh != nil {
			select {
			case g.tuneCh <- b.Hz:
			default:
			}
		}
		g.AppendSystem(fmt.Sprintf("switched to #%s (%.3f MHz)", b.Name, float64(b.Hz)/1e6))
		g.refreshStatus()
		g.bandList.Refresh()
		if g.qso != nil {
			g.qso.SetActiveBand(b.Name, b.Hz)
		}
		// Wipe band-specific state so the new channel doesn't show
		// stale waterfall pixels, decode boxes, or map pins from the
		// band the operator just left.
		if g.scope != nil {
			g.scope.Reset()
			if g.scope.mapWidget != nil {
				g.scope.mapWidget.ClearSpots()
			}
		}
		// Also clear the per-band HEARD list — the call set is band-
		// specific and stale entries from the previous band confuse
		// the operator scanning the sidebar.
		g.mu.Lock()
		g.heard = map[string]heardEntry{}
		g.mu.Unlock()
		if g.usersList != nil {
			fyne.Do(func() { g.usersList.Refresh() })
		}
	}
	chanHeader := canvas.NewText("BANDS", color.RGBA{140, 140, 145, 255})
	chanHeader.TextSize = 11
	chanHeader.TextStyle = fyne.TextStyle{Bold: true}

	// ── User panel at the bottom of the channel column ─────────────────
	// Discord's bottom-of-sidebar user bar: avatar/handle on the left,
	// mute/headphone/gear icons on the right. We don't have voice state,
	// so just our callsign + grid (read-only display) + a gear that
	// opens the NocordHF settings dialog.
	g.userCallText = canvas.NewText("", color.RGBA{220, 220, 225, 255})
	g.userCallText.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	g.userCallText.TextSize = 12
	g.userGridText = canvas.NewText("", color.RGBA{140, 140, 150, 255})
	g.userGridText.TextStyle = fyne.TextStyle{Monospace: true}
	g.userGridText.TextSize = 10
	userText := container.NewVBox(g.userCallText, g.userGridText)
	gearBtn := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() {
		g.showSettings()
	})
	gearBtn.Importance = widget.LowImportance
	userBar := container.NewBorder(nil, nil, container.NewPadded(userText), gearBtn, nil)
	userBarBg := canvas.NewRectangle(color.RGBA{36, 38, 43, 255})
	userBarStack := container.NewStack(userBarBg, userBar)

	// sidebarStack swaps between the FT8 bands list and the MeshCore
	// Contacts/Channels sidebar based on activeMode. The chan-column
	// header (chanHeader) is also retitled by setActiveMode so the
	// "BANDS" label becomes "MESHCORE" when the mesh sidebar is up.
	g.sidebarStack = container.NewStack(g.bandList)
	chanCol := container.NewBorder(
		container.NewPadded(chanHeader), userBarStack, nil, nil,
		g.sidebarStack,
	)
	chanBg := canvas.NewRectangle(color.RGBA{47, 49, 54, 255})
	chanCol = container.NewStack(chanBg, chanCol)
	g.refreshUserBar()
	// Stash chanHeader on the GUI so setActiveMode can rename it
	// when the operator flips between FT8 and MeshCore.
	g.chanHeader = chanHeader

	// ── Chat pane: header + scrollable history + input ─────────────────
	g.statusText = canvas.NewText("UTC --:--:--", color.RGBA{170, 175, 185, 255})
	// Bold-only (no Monospace): Fyne's mono font lacks a true bold
	// variant on macOS so the topic bar rendered visually identical
	// to regular weight. The header isn't tabular so the proportional
	// face is fine here.
	g.statusText.TextStyle = fyne.TextStyle{Bold: true}
	g.statusText.TextSize = 11
	// Tiny help icon next to the topic line — opens an info dialog
	// covering chat colour conventions, badges, keyboard shortcuts,
	// and the auto-progress chain. Use canvas.Image (not a Button)
	// because Button's built-in padding makes the topic bar twice as
	// tall; the raw icon at the topic font size keeps the bar tight.
	helpImg := canvas.NewImageFromResource(theme.HelpIcon())
	helpImg.FillMode = canvas.ImageFillContain
	helpImg.SetMinSize(fyne.NewSize(13, 13))
	g.chatHelpTap = newTappableContainer(helpImg, func() { g.showChatHelp() })
	header := container.NewPadded(container.NewHBox(g.statusText, g.chatHelpTap))
	// Spin up the NTP checker + 1 Hz status ticker. The checker runs its
	// own goroutine internally; startStatusTicker drives refreshStatus so
	// the clock + slot phase tick in real time.
	g.ntpChecker = ntpcheck.New()
	g.ntpChecker.Start()
	g.startStatusTicker()
	headerBg := canvas.NewRectangle(color.RGBA{54, 57, 63, 255})
	headerStack := container.NewStack(headerBg, header)

	g.chatList = widget.NewList(
		func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.rows)
		},
		func() fyne.CanvasObject {
			// Three-segment row: timestamp at the chat font size (the
			// reader scans timestamps to orient in time, so it stays
			// prominent), then SNR + GEO at a smaller / dimmer size so
			// they recede, then the message at full chat size. System /
			// TX rows leave timestamp + meta empty and fill the message
			// cell with the styled-line text.
			//
			// Plus a trailing reply button shown only on CQ rows -the
			// operator can hit it to immediately call the station back
			// without typing the call into the input box.
			ts := canvas.NewText("", color.RGBA{170, 175, 185, 255})
			ts.TextStyle = fyne.TextStyle{Monospace: true}
			ts.TextSize = 10
			tsHover := newHoverTip(ts, "")
			meta := canvas.NewText("", color.RGBA{130, 135, 145, 255})
			meta.TextStyle = fyne.TextStyle{Monospace: true}
			meta.TextSize = 8
			// Message column = two canvas.Text widgets in an inner HBox.
			// Normal rows use only the first; in-progress TX rows split
			// the message between them — `msg` carries the already-
			// transmitted prefix (rendered green like a finished TX
			// echo) and `msgPending` carries the not-yet-transmitted
			// suffix in grey. Same monospace face + size so adjacent
			// characters line up pixel-for-pixel.
			msg := canvas.NewText("", color.RGBA{220, 220, 222, 255})
			msg.TextStyle = fyne.TextStyle{Monospace: true}
			msg.TextSize = 10
			msgPending := canvas.NewText("", color.RGBA{120, 124, 132, 255})
			msgPending.TextStyle = fyne.TextStyle{Monospace: true}
			msgPending.TextSize = 10
			// MeshCore-only fixed-width cells. snrSlot is RIGHT-
			// justified so "+14.0 dB" sits at the right edge of
			// its column; senderSlot is LEFT-justified so the
			// callsign starts immediately after — eliminating
			// the wasted gap between the two cells. The bar
			// still lands at a stable X (right edge of the
			// fixed-width senderSlot) so message text aligns
			// across rows. Both cells stay hidden on FT8 rows.
			snrText := canvas.NewText("", color.RGBA{140, 145, 155, 255})
			snrText.TextStyle = fyne.TextStyle{Monospace: true}
			snrText.TextSize = 9
			snrText.Alignment = fyne.TextAlignTrailing
			snrSlot := container.New(&fixedWidthLayout{width: 60}, snrText)
			snrSlot.Hide()
			senderText := canvas.NewText("", color.RGBA{180, 190, 205, 255})
			senderText.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			senderText.TextSize = 10
			senderText.Alignment = fyne.TextAlignLeading
			senderSlot := container.New(&fixedWidthLayout{width: 150}, senderText)
			senderSlot.Hide()
			// Vertical separator between the sender column and the
			// message body. widget.Separator is the theme-native
			// 1-px divider; in an HBox it renders vertically and
			// inherits the operator's theme colour, so it matches
			// other dividers in the UI without manual tinting.
			barSep := widget.NewSeparator()
			barSep.Hide()
			// msgSegments swaps in for msg/msgPending on MeshCore
			// rows whose text contains path-hash hex tokens that
			// resolve to known contacts — each token becomes an
			// inline link widget. inlineFlowLayout packs children
			// with no padding so plain text runs and links read as
			// one continuous line.
			msgSegments := container.New(inlineFlowLayout{})
			msgSegments.Hide()
			textRow := container.NewHBox(tsHover, meta, snrSlot, senderSlot, barSep, msg, msgPending, msgSegments)
			replyBtn := widget.NewButtonWithIcon("", theme.MailReplyIcon(), nil)
			replyBtn.Importance = widget.LowImportance
			replyBtn.Hide()
			// Prior-contact badge: single-letter "L" (LoTW QSL on this
			// band) or "O" (ADIF QSO logged but not confirmed). Lives
			// in slot 1 of the actions area, immediately left of the
			// reply button. Single character keeps every chat row left-
			// aligned at the same x-offset (no shift between marked
			// and unmarked rows).
			qsoBadge := canvas.NewText("", color.RGBA{200, 205, 215, 255})
			qsoBadge.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
			qsoBadge.TextSize = 10
			qsoBadge.Alignment = fyne.TextAlignCenter
			actions := container.New(&chatActionsLayout{slotWidth: 28, slots: 2},
				replyBtn, container.NewCenter(qsoBadge))
			padded := container.NewPadded(container.NewBorder(nil, nil, nil, actions, textRow))
			return newHoverRow(padded)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			h := obj.(*hoverRow)
			padded := h.inner.(*fyne.Container)
			border := padded.Objects[0].(*fyne.Container)
			textRow := border.Objects[0].(*fyne.Container)
			actions := border.Objects[1].(*fyne.Container)
			replyBtn := actions.Objects[0].(*widget.Button)
			qsoBadge := actions.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)
			tsHover := textRow.Objects[0].(*hoverTip)
			ts := tsHover.inner.(*canvas.Text)
			meta := textRow.Objects[1].(*canvas.Text)
			snrSlot := textRow.Objects[2].(*fyne.Container)
			snrText := snrSlot.Objects[0].(*canvas.Text)
			senderSlot := textRow.Objects[3].(*fyne.Container)
			senderText := senderSlot.Objects[0].(*canvas.Text)
			barSep := textRow.Objects[4].(*widget.Separator)
			msg := textRow.Objects[5].(*canvas.Text)
			msgPending := textRow.Objects[6].(*canvas.Text)
			msgSegments := textRow.Objects[7].(*fyne.Container)
			g.mu.Lock()
			if id >= len(g.rows) {
				g.mu.Unlock()
				return
			}
			r := g.rows[id]
			g.mu.Unlock()
			tsText, metaText, msgText := formatRowSegments(r)
			ts.Text = tsText
			tsHover.SetTooltip(formatHoverTime(r.when))
			// MeshCore rows split the meta column into two
			// fixed-width cells (SNR left-justified, sender
			// right-justified) plus a graphical bar before the
			// message. FT8 rows keep the legacy single meta cell
			// and hide the new cells entirely.
			if r.mc && !r.separator {
				meta.Text = ""
				snrText.Text = formatMcSnrCell(r.snrDB)
				senderText.Text = mcSenderOrStar(r.mcSender)
				snrText.Refresh()
				senderText.Refresh()
				snrSlot.Show()
				senderSlot.Show()
				barSep.Show()
			} else {
				meta.Text = metaText
				snrSlot.Hide()
				senderSlot.Hide()
				barSep.Hide()
			}
			// In-progress TX: split the message at the rune boundary
			// matching txProgress. msgPending picks up the rest in
			// grey so the operator can watch the line go green
			// character-by-character. Default for every other row:
			// full text in `msg`, msgPending empty.
			msg.Text = msgText
			msgPending.Text = ""
			if r.tx && r.txInProgress {
				runes := []rune(msgText)
				p := r.txProgress
				if p < 0 {
					p = 0
				}
				if p > len(runes) {
					p = len(runes)
				}
				msg.Text = string(runes[:p])
				msgPending.Text = string(runes[p:])
			}
			// MeshCore delivery state — append a glyph + status
			// word to the message, similar to the web client's
			// "Delivered" / "Sending…" / "Failed" footer. Only on
			// rows we tracked (mcDelivery != none); FT8 TX rows
			// stay untouched.
			if r.mcDelivery != mcDeliveryNone {
				switch r.mcDelivery {
				case mcDeliveryPending:
					msgPending.Text = "  (sending...)"
				case mcDeliveryDelivered:
					msgPending.Text = "  (delivered)"
				case mcDeliveryFailed:
					msgPending.Text = "  (failed)"
				}
			}
			isCQ := strings.HasPrefix(r.text, "CQ")
			switch {
			case r.separator:
				// Faint dim glyph used as a slot divider -picks up the
				// ts/meta cells being empty + a single-character message
				// drawn very dim. Reads as a horizontal break without
				// requiring a custom row template.
				msg.Color = color.RGBA{80, 82, 90, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true}
			case r.system:
				msg.Color = color.RGBA{140, 140, 150, 255}
				msg.TextStyle = fyne.TextStyle{Italic: true, Monospace: true}
			case r.tx:
				msg.Color = color.RGBA{80, 200, 120, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case r.addrUs:
				// Bright cyan — distinct from the CQ amber sitting right
				// next to it in busy bands so directed-at-us calls don't
				// get visually lost.
				msg.Color = color.RGBA{90, 220, 255, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case g.qso != nil && (g.qso.IsOpen(senderFromMessage(r.text)) ||
				g.qso.IsOpen(remoteCallFromMessage(r.text))):
				// One of our open-QSO targets is talking with (or being
				// called by) someone else. Warm orange flag so we notice
				// they're busy and may not respond to our pending TX.
				msg.Color = color.RGBA{255, 150, 60, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case isCQ:
				msg.Color = color.RGBA{255, 200, 50, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true}
			default:
				msg.Color = color.RGBA{220, 220, 222, 255}
				msg.TextStyle = fyne.TextStyle{Monospace: true}
			}
			// Blink-highlight if this row matches the selected call.
			if !r.system && !r.tx && !r.separator {
				if sender := senderFromMessage(r.text); sender != "" && g.shouldBlinkCall(sender) {
					msg.Color = color.RGBA{255, 240, 80, 255}
					msg.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
				}
			}
			// Prior-contact: "L" badge (LoTW QSL on this band) puts the
			// message in italics; "O" badge (ADIF-only QSO) leaves the
			// font upright. Skip on system / tx / separator rows where
			// the row's text isn't a station message.
			qsoBadge.Text = ""
			if !r.system && !r.tx && !r.separator {
				switch r.confirmStatus {
				case 2: // LoTW QSL
					qsoBadge.Text = "L"
					qsoBadge.Color = color.RGBA{120, 200, 120, 255}
					ts := msg.TextStyle
					ts.Italic = true
					msg.TextStyle = ts
				case 1: // ADIF QSO
					qsoBadge.Text = "O"
					qsoBadge.Color = color.RGBA{180, 180, 195, 255}
				}
			}
			qsoBadge.Refresh()
			// Reply button: visible only for CQ rows from a real (non-
			// hashed) callsign. Tapping it queues a directed call to
			// that station -equivalent to typing their call in the
			// input box and pressing Enter.
			if isCQ && !r.system && !r.tx && !r.separator {
				if remote := remoteCallFromMessage(r.text); remote != "" && !strings.HasPrefix(remote, "<") {
					replyBtn.OnTapped = func() {
						g.input.SetText(remote)
						g.handleSubmit(remote)
						g.input.SetText("")
					}
					replyBtn.Show()
				} else {
					replyBtn.OnTapped = nil
					replyBtn.Hide()
				}
			} else {
				replyBtn.OnTapped = nil
				replyBtn.Hide()
			}
			ts.Refresh()
			meta.Refresh()
			msg.Refresh()
			// MeshCore path-hash link detection. When the message
			// contains hex tokens (1/2/3-byte) that resolve against
			// the contact roster's pubkey prefixes, swap the
			// msg/msgPending pair for an inline-flow container of
			// plain runs + clickable link widgets. Skip rows where
			// rendering as segments would lose information: in-
			// progress TX (the rune-level progress split needs the
			// pending suffix) and rows with a delivery footer
			// (the "(delivered)" / "(sending...)" footer lives in msgPending).
			showSegments := false
			if r.mc && !r.separator && !(r.tx && r.txInProgress) && r.mcDelivery == mcDeliveryNone {
				g.mcMu.Lock()
				contactsCopy := append([]meshcore.Contact(nil), g.mcContacts...)
				g.mcMu.Unlock()
				if g.mcAttachHashLinks(msgSegments, msgText, msg.Color, msg.TextStyle, contactsCopy) {
					showSegments = true
				}
			}
			if showSegments {
				msg.Hide()
				msgPending.Hide()
				msgSegments.Show()
			} else {
				msgSegments.RemoveAll()
				msgSegments.Hide()
				msg.Show()
				msgPending.Show()
			}
			// MeshCore rows get their own right-click menu (Info +
			// Map Trace). FT8 rows route to the callsign-context
			// menu (Reply / QRZ / Profile / Copy) when they have a
			// recognisable callsign. System / TX-echo / separator
			// rows have no menu in either mode.
			switch {
			case r.separator || r.system:
				h.onSecondary = nil
			case r.mc:
				rowCopy := r // chat rows can mutate; freeze the snapshot for the closure
				h.onSecondary = func(pos fyne.Position) {
					g.showMcChatRowContextMenu(rowCopy, pos)
				}
			case !r.tx:
				if remote := senderFromMessage(r.text); remote != "" && !strings.HasPrefix(remote, "<") {
					rowIsCQ := isCQ
					h.onSecondary = func(pos fyne.Position) {
						g.showCallContextMenu(remote, rowIsCQ, pos)
					}
				} else {
					h.onSecondary = nil
				}
			default:
				h.onSecondary = nil
			}
		},
	)
	// Click a chat row to populate the input box with that station's call —
	// the FT8 equivalent of "Reply" on a Discord message. CQ rows route to
	// the calling station; directed-message rows route to whoever called
	// (the first non-myCall token). System / TX-echo / separator rows are
	// no-ops. After populating, focus the input so Enter sends.
	g.chatList.OnSelected = func(id widget.ListItemID) {
		g.mu.Lock()
		if id >= len(g.rows) {
			g.mu.Unlock()
			return
		}
		r := g.rows[id]
		// Suppress the input-population side-effect when the
		// selection was synthesised by scrollChatTo* (just to scroll
		// to the row). Without this, clicking a decode box on the
		// waterfall would write that call into the input box every
		// time, since selectCall scrolls the chat to the matching
		// row via Select(idx).
		suppress := g.suppressChatSelectInput
		g.suppressChatSelectInput = false
		g.mu.Unlock()
		g.chatList.UnselectAll()
		if suppress {
			return
		}
		if r.system || r.tx || r.separator {
			return
		}
		remote := remoteCallFromMessage(r.text)
		if remote == "" || strings.HasPrefix(remote, "<") {
			return
		}
		g.input.SetText(remote)
		g.window.Canvas().Focus(g.input)
	}

	g.input = newHistoryEntry()
	g.input.completer = g.mcAtCompleter
	g.input.OnSubmitted = func(s string) {
		g.handleSubmit(s)
		g.input.SetText("")
	}
	// MeshCore character counter — placed to the right of the input,
	// before the Send button. Hidden in FT8 mode since FT8's text
	// constraints are different and shorter. Updated on every
	// OnChanged (including history-recall SetText calls).
	g.mcCharCount = canvas.NewText(fmt.Sprintf("0/%d", meshcore.MaxTextLength), color.RGBA{140, 145, 155, 255})
	g.mcCharCount.TextStyle = fyne.TextStyle{Monospace: true}
	g.mcCharCount.TextSize = 10
	g.mcCharCount.Alignment = fyne.TextAlignTrailing
	g.mcCharCount.Hide()
	g.input.OnChanged = func(s string) {
		n := len(s)
		g.mcCharCount.Text = fmt.Sprintf("%d/%d", n, meshcore.MaxTextLength)
		switch {
		case n > meshcore.MaxTextLength:
			g.mcCharCount.Color = color.RGBA{240, 80, 80, 255} // red — over limit
		case n >= meshcore.MaxTextLength-10:
			g.mcCharCount.Color = color.RGBA{230, 170, 60, 255} // amber — close
		default:
			g.mcCharCount.Color = color.RGBA{140, 145, 155, 255} // dim — plenty of room
		}
		g.mcCharCount.Refresh()
	}
	g.sendBtn = widget.NewButton("Send", func() {
		g.mu.Lock()
		active := g.txActive
		stopCh := g.activeStopCh
		g.mu.Unlock()
		if active {
			// Hard-stop: close the per-TX stopCh; main's TX loop closes
			// the player's stop channel which silences playback and drops
			// PTT immediately.
			if stopCh != nil {
				select {
				case <-stopCh: // already closed
				default:
					close(stopCh)
				}
			}
			return
		}
		g.handleSubmit(g.input.Text)
		g.input.SetText("")
	})
	// Auto checkbox: when checked, an inbound message addressed to us
	// triggers the next-step reply automatically (sig report → R+report
	// → RR73 → 73). Read directly from prefs here (not g.autoReply)
	// because buildLayout runs inside NewGUI, before main.go calls
	// ApplySavedToggles — using g.autoReply would always render "off"
	// on the first frame regardless of what was previously saved.
	autoInitial := false
	if g.app != nil {
		autoInitial = g.app.Preferences().BoolWithFallback("auto_reply", false)
	}
	g.mu.Lock()
	g.autoReply = autoInitial
	g.mu.Unlock()
	g.autoCheck = widget.NewCheck("Auto", func(on bool) {
		g.mu.Lock()
		g.autoReply = on
		g.mu.Unlock()
		if g.app != nil {
			g.app.Preferences().SetBool("auto_reply", on)
		}
	})
	g.autoCheck.SetChecked(autoInitial)
	// Right edge of the input row: counter then Send. Counter is
	// padded so it doesn't kiss the entry's right border. The counter
	// stays hidden in FT8 mode (applySidebarForMode toggles it),
	// which collapses the HBox to just the button.
	inputRight := container.NewHBox(container.NewPadded(g.mcCharCount), g.sendBtn)
	inputRow := container.NewBorder(nil, nil, g.autoCheck, inputRight, g.input)
	inputBg := canvas.NewRectangle(color.RGBA{64, 68, 75, 255})
	inputStack := container.NewStack(inputBg, container.NewPadded(inputRow))

	// ── Users sidebar: IRC-style list of RX-only callsigns heard on band ─
	// Rendered to the right of the chat list. Click a name to populate the
	// input box, just like clicking a chat row. Header is a small tappable
	// label -click to cycle sort mode (alpha → SNR → recent).
	headerLabel := func(m heardSortMode) string {
		switch m {
		case heardSortSNR:
			return "HEARD | SNR"
		case heardSortRecent:
			return "HEARD | NEW"
		default:
			return "HEARD | A-Z"
		}
	}
	usersHdrText := canvas.NewText(headerLabel(heardSortAlpha), color.RGBA{170, 175, 185, 255})
	usersHdrText.TextStyle = fyne.TextStyle{Bold: true}
	usersHdrText.TextSize = 9
	usersHdrTap := newTextTap(usersHdrText, func() {
		g.mu.Lock()
		g.heardSort = (g.heardSort + 1) % 3
		mode := g.heardSort
		g.mu.Unlock()
		usersHdrText.Text = headerLabel(mode)
		usersHdrText.Refresh()
		if g.usersList != nil {
			g.usersList.Refresh()
		}
	})
	g.usersList = widget.NewList(
		func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.heard)
		},
		func() fyne.CanvasObject {
			// Four-segment row: [CQ slot] [OTA slot] [country slot]
			// CALL SNR. The CQ and OTA badges are wrapped in fixed-
			// width slots so toggling their visibility doesn't shift
			// the country circle, callsign, and SNR — those columns
			// stay at a fixed X regardless of which status badges are
			// present.
			//
			// The whole row is wrapped in a hoverRow (highlights the
			// matching map pin / waterfall box on hover, opens the
			// context menu on right-click). The flag-circle is in
			// its own hoverRow so the country tooltip ONLY appears
			// when the cursor is actually over the country circle.
			// CQ slot: pixel-art icon from the same badge catalog as
			// the OTA programmes, so map and roster show identical
			// CQ artwork. canvas.Image with nearest-neighbour scale
			// for sharp edges at the 18px slot size.
			cqImg := canvas.NewImageFromImage(mapview.BadgeImage("CQ"))
			cqImg.FillMode = canvas.ImageFillContain
			cqImg.ScaleMode = canvas.ImageScalePixels
			cqImg.SetMinSize(fyne.NewSize(18, 18))
			cqImg.Hide()
			cqSlot := container.New(&fixedWidthLayout{width: 22}, cqImg)
			// OTA slot: a canvas.Image that gets its source swapped
			// per row via mapview.BadgeImage(otaType). Image is
			// downscaled to ~18 px with nearest-neighbor for crisp
			// pixel-art rendering at small sizes.
			otaImg := canvas.NewImageFromImage(blankBadgeImage())
			otaImg.FillMode = canvas.ImageFillContain
			otaImg.ScaleMode = canvas.ImageScalePixels
			otaImg.SetMinSize(fyne.NewSize(18, 18))
			otaImg.Hide()
			otaSlot := container.New(&fixedWidthLayout{width: 22}, otaImg)
			// Country slot: native unicode flag emoji centred in a
			// fixed-width slot. Reverted from a colored-disc-with-
			// letters because the operating system already renders
			// flag emoji at the right size, and the actual flag is
			// what the user wants to see (the disc was reading as a
			// generic blob with text).
			flagText := canvas.NewText("", color.RGBA{220, 225, 235, 255})
			flagText.TextSize = 14
			flagText.Alignment = fyne.TextAlignCenter
			flagInner := container.New(&fixedWidthLayout{width: 22}, container.NewCenter(flagText))
			flagSlot := newHoverRow(flagInner)
			// Prior-contact slot: single-letter "L" / "O" badge that
			// mirrors the chat-row indicator. Same width as the other
			// status slots so the call column stays at a fixed x.
			qsoText := canvas.NewText("", color.RGBA{180, 180, 195, 255})
			qsoText.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
			qsoText.TextSize = 10
			qsoText.Alignment = fyne.TextAlignCenter
			qsoSlot := container.New(&fixedWidthLayout{width: 18}, container.NewCenter(qsoText))
			t := canvas.NewText("", color.RGBA{210, 215, 225, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 10
			return newHoverRow(container.NewHBox(cqSlot, otaSlot, qsoSlot, flagSlot, t))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			h := obj.(*hoverRow)
			row := h.inner.(*fyne.Container)
			cqSlot := row.Objects[0].(*fyne.Container)
			cqImg := cqSlot.Objects[0].(*canvas.Image)
			otaSlot := row.Objects[1].(*fyne.Container)
			otaImg := otaSlot.Objects[0].(*canvas.Image)
			qsoSlot := row.Objects[2].(*fyne.Container)
			qsoText := qsoSlot.Objects[0].(*fyne.Container).Objects[0].(*canvas.Text)
			flagSlot := row.Objects[3].(*hoverRow)
			flagInner := flagSlot.inner.(*fyne.Container)
			flagCenter := flagInner.Objects[0].(*fyne.Container)
			flagText := flagCenter.Objects[0].(*canvas.Text)
			t := row.Objects[4].(*canvas.Text)
			snap := g.heardSnapshot()
			if id >= len(snap) {
				h.onHoverIn = nil
				h.onHoverOut = nil
				h.onHoverMove = nil
				h.onSecondary = nil
				flagSlot.onHoverIn = nil
				flagSlot.onHoverOut = nil
				flagSlot.onHoverMove = nil
				return
			}
			e := snap[id]

			// Country slot: native unicode flag emoji for the call's
			// home entity. Falls back to the 2-letter ISO code for
			// entities without a real flag (Hawaii, Alaska, etc.)
			// or to two spaces when the code can't be inferred.
			flag := callsign.Flag(e.call)
			if flag == "" {
				if sc := callsign.ShortCode(e.call); len(sc) >= 2 {
					flag = sc[len(sc)-2:]
				} else {
					flag = "  "
				}
			}
			flagText.Text = flag
			flagText.Refresh()

			// CQ badge: visible while the CQ window is fresh.
			if !e.lastCQ.IsZero() && time.Since(e.lastCQ) <= 30*time.Second {
				cqImg.Show()
			} else {
				cqImg.Hide()
			}
			// OTA badge: pixel-art icon for the active program, drawn
			// from lib/mapview's BadgeImage so the same artwork
			// appears on the map. Hidden when no recent OTA has
			// been heard (or the program is unknown).
			if !e.lastOTA.IsZero() && time.Since(e.lastOTA) <= 5*time.Minute {
				if im := mapview.BadgeImage(e.lastOTAType); im != nil {
					otaImg.Image = im
					otaImg.Refresh()
					otaImg.Show()
				} else {
					otaImg.Hide()
				}
			} else {
				otaImg.Hide()
			}
			// Prior-contact badge: same letters as the chat row.
			// "L" for an LoTW QSL on the active band, "O" for an
			// ADIF-only QSO. Roster keeps the regular weight (no
			// italics) — the IRC-style monospace nick list reads
			// better when only the letter changes per row.
			switch g.confirmStatusForCall(e.call) {
			case 2:
				qsoText.Text = "L"
				qsoText.Color = color.RGBA{120, 200, 120, 255}
			case 1:
				qsoText.Text = "O"
				qsoText.Color = color.RGBA{180, 180, 195, 255}
			default:
				qsoText.Text = ""
			}
			qsoText.Refresh()
			t.Text = fmt.Sprintf("%-7s %+3.0f", e.call, e.snr)
			if g.shouldBlinkCall(e.call) {
				t.Color = color.RGBA{255, 240, 80, 255}
				t.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
			} else {
				t.Color = color.RGBA{210, 215, 225, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true}
			}
			t.Refresh()
			// Row-level hover: highlight matching map pin + waterfall
			// box. No country tooltip here — that lives on the flag
			// sub-widget so the tooltip only appears when the cursor
			// is actually over the country flag.
			call := e.call
			country := callsign.CountryName(call)
			h.onHoverIn = func() {
				if g.scope != nil {
					g.scope.SetHighlightCall(call)
				}
			}
			h.onHoverOut = func() {
				if g.scope != nil {
					g.scope.SetHighlightCall("")
				}
			}
			h.onHoverMove = nil
			rowIsCQ := !e.lastCQ.IsZero() && time.Since(e.lastCQ) <= 60*time.Second
			h.onSecondary = func(pos fyne.Position) {
				g.showCallContextMenu(call, rowIsCQ, pos)
			}
			// Flag-slot hover: country tooltip only.
			flagSlot.onHoverIn = func() {
				if country != "" {
					g.showHeardTooltip(call, country)
				}
			}
			flagSlot.onHoverOut = func() { g.hideHeardTooltip() }
			flagSlot.onHoverMove = func(absPos fyne.Position) {
				g.updateHeardTooltipPos(absPos)
			}
		},
	)
	g.usersList.OnSelected = func(id widget.ListItemID) {
		// Suppress the side-effect (popup + selectCall) when the
		// selection was synthesised by scrollHeardToCall. Without
		// this, the recursive selectCall → scrollHeardToCall → Select
		// → OnSelected → selectCall loop fires showDecodePopup
		// hundreds of times per second.
		g.mu.Lock()
		suppress := g.suppressHeardSelectAction
		g.suppressHeardSelectAction = false
		g.mu.Unlock()
		snap := g.heardSnapshot()
		if id >= len(snap) {
			return
		}
		call := snap[id].call
		g.usersList.UnselectAll()
		if suppress {
			return
		}
		// Single-click in the HEARD list mirrors single-click on a
		// waterfall decode box: show the magnification popup, scroll
		// + blink the matching chat row, freeze chat auto-scroll.
		// Falls back to a context-style menu if we don't have a
		// finalised decode for that call yet (operator clicked before
		// the next slot rolled in).
		if g.scope != nil {
			if d, ok := g.scope.LatestDecodeFor(call); ok {
				abs := fyne.CurrentApp().Driver().AbsolutePositionForObject(g.usersList)
				size := g.usersList.Size()
				g.showDecodePopup(call, d.slotStart, d.freqHz,
					fyne.NewPos(abs.X+size.Width-4, abs.Y+12))
			}
		}
		g.selectCall(call)
	}
	usersHdrBg := canvas.NewRectangle(color.RGBA{54, 57, 63, 255})
	usersHdrBg.SetMinSize(fyne.NewSize(0, 16))
	usersHdrStack := container.NewStack(usersHdrBg, container.NewPadded(usersHdrTap))
	usersBg := canvas.NewRectangle(color.RGBA{36, 38, 42, 255})
	g.usersInner = container.NewStack(usersBg, container.NewBorder(usersHdrStack, nil, nil, nil, g.usersList))
	// Force a fixed pixel width via a custom layout -Stack.MinSize from a
	// background rectangle doesn't reliably propagate up through Border's
	// east slot, leading the column to collapse and bleed visually into
	// the neighbouring HSplit pane.
	g.usersCol = container.New(&fixedWidthLayout{width: 170}, g.usersInner)

	chatCenter := container.NewBorder(headerStack, inputStack, nil, nil, g.chatList)
	chatCol := container.NewBorder(nil, nil, nil, g.usersCol, chatCenter)
	chatBg := canvas.NewRectangle(color.RGBA{40, 43, 48, 255})
	chatColStack := container.NewStack(chatBg, chatCol)

	// ── Scope column on the far right (waterfall + map) ────────────────
	// Map centres on the operator's grid if it's been set; otherwise the
	// MapWidget falls back to a default mid-North-America view.
	g.mu.Lock()
	myGrid := g.myGrid
	g.mu.Unlock()
	g.scope = newScopePane(myGrid)
	// If we launched into MeshCore mode, swap the right pane to the
	// map+RxLog VSplit before the layout becomes visible so the
	// operator never sees a one-frame flash of the FT8 split.
	if g.activeMode == "meshcore" {
		g.scope.SetMeshcoreLayout(g.buildMeshcoreRxLog())
	}

	// QSO logger: ADIF writer + in-memory log + state tracker. Reads
	// any pre-existing nocordhf.adif at startup so the worked-grid
	// overlay lights up immediately for prior contacts.
	g.adifWriter = adif.NewWriter("nocordhf.adif")
	g.qso = newQSOTracker()
	g.qso.SetProfile(g.myCall, g.myGrid)
	g.qso.SetActiveBand(g.current.Name, g.current.Hz)

	// TQSL config: same defaults the legacy GUI uses; values are
	// loaded from fyne.Preferences so they survive a relaunch.
	prefs := g.app.Preferences()
	g.tqslCfg = &tqsl.Config{
		BinaryPath:      prefs.StringWithFallback("tqsl_path", tqsl.DefaultMacPath),
		StationLocation: prefs.String("tqsl_station"),
		CertPassword:    prefs.String("tqsl_cert_password"),
	}
	g.tqslAutoUpload = prefs.BoolWithFallback("tqsl_auto_upload", false)
	g.rosterStaleMinutes = prefs.IntWithFallback("roster_stale_minutes", 30)
	if recs, err := adif.Read("nocordhf.adif"); err == nil && len(recs) > 0 {
		g.adifLog = append(g.adifLog, recs...)
	}
	g.qso.SetOnLogged(func(rec adif.Record) {
		// Persist + update in-memory log + refresh map overlay.
		// Backfill TheirGrid from the spot DB if the QSO didn't carry
		// one through the exchange — common when the contact opens on
		// a sig report rather than a CQ-with-grid. Without this the
		// map overlay can't tint the grid square blue because
		// localWorkedGridsOnActiveBand keys off rec.TheirGrid.
		if rec.TheirGrid == "" && g.scope != nil && g.scope.mapWidget != nil {
			if grid := g.scope.mapWidget.GetSpotGrid(rec.TheirCall); grid != "" {
				rec.TheirGrid = grid
			}
		}
		if err := g.adifWriter.Append(rec); err == nil {
			g.mu.Lock()
			g.adifLog = append(g.adifLog, rec)
			g.mu.Unlock()
		}
		g.AppendSystem(fmt.Sprintf("QSO logged: %s | %s | %s", rec.TheirCall, rec.TheirGrid, rec.Band))
		if g.scope != nil && g.scope.mapWidget != nil {
			g.scope.mapWidget.Refresh()
		}
		// Auto-upload to LoTW via TQSL when configured. Runs on a
		// background goroutine so we don't block the chat thread on
		// the tqsl process — uploads can take a couple of seconds.
		g.mu.Lock()
		auto := g.tqslAutoUpload
		cfg := g.tqslCfg
		path := ""
		if g.adifWriter != nil {
			path = g.adifWriter.Path()
		}
		g.mu.Unlock()
		if auto && cfg != nil && cfg.Available() && path != "" {
			go func() {
				if err := cfg.Upload(path); err != nil {
					g.AppendSystem("LoTW upload failed: " + err.Error())
				} else {
					g.AppendSystem("LoTW upload OK")
				}
			}()
		}
	})
	// Map overlay: blue tint for grids we've worked locally on the
	// active band, yellow for LoTW QSO records, red for LoTW QSLs.
	g.scope.mapWidget.SetLocalWorkedGridsFunc(g.localWorkedGridsOnActiveBand)
	g.scope.mapWidget.SetWorkedGridsFunc(g.lotwWorkedGridsOnActiveBand)
	g.scope.mapWidget.SetConfirmedGridsFunc(g.lotwConfirmedGridsOnActiveBand)
	g.scope.mapWidget.SetWorkedFunc(g.workedStatusForCall)
	g.scope.mapWidget.SetRecentCQFunc(g.callIsCQ)
	// Right-click a station dot on the map → same context menu as
	// the chat / HEARD / waterfall surfaces.
	g.scope.mapWidget.SetOnSpotSecondaryTap(func(call string, absPos fyne.Position) {
		g.showCallContextMenu(call, g.callIsCQ(call), absPos)
	})
	// Right-click a MeshCore node dot → Info / Open chat / Show
	// path. Only fires when the map has a mesh-node overlay (set
	// by mcSyncContactsToMap on MeshCore connect), so the FT8
	// surface keeps its single-callback behaviour.
	g.scope.mapWidget.SetOnMeshNodeSecondaryTap(func(pub [32]byte, absPos fyne.Position) {
		g.showMcMapNodeContextMenu(meshcore.PubKey(pub), absPos)
	})

	// Wire decode-box interactions:
	//   - double-click → scroll chat to the matching call;
	//   - right-click → operator context menu;
	//   - single-click on a box → pin the magnification popup at the
	//     click position and scroll/highlight the chat row;
	//   - single-click on empty waterfall → unpin + hide the popup;
	//   - hover over a box (when no popup is pinned) → show a live
	//     preview popup adjacent to the box; updates as the cursor
	//     moves between boxes; hides on hover-leave-all.
	g.scope.SetDecodeHooks(
		g.scrollChatToDecode,
		func(call string, pos fyne.Position) {
			g.showCallContextMenu(call, g.callIsCQ(call), pos)
		},
		func(call string, slotStart time.Time, freqHz float64, pos fyne.Position) {
			// Click → pin. Selection highlight is already set by
			// scope's onSelect before this callback fires; here we
			// pin the popup, scroll + blink-highlight the matching
			// chat row.
			g.pinDecodePopup(call, slotStart, freqHz, pos)
			g.selectCall(call)
		},
		g.dismissDecodePopup,
		func(call string, slotStart time.Time, freqHz float64, pos fyne.Position) {
			// Hover preview — only when nothing is currently pinned.
			g.previewDecodePopup(call, slotStart, freqHz, pos)
		},
		g.previewDecodePopupEnd,
	)

	// ── Compose four columns horizontally ──────────────────────────────
	// mode rail (fixed) | channels (drag) | chat | scope (drag)
	//
	// chanCol used to be a fixed-width column via SetMinSize, which
	// looked OK for FT8 ("20m", "40m") but cramped MeshCore contact
	// names. Wrapping it in an HSplit gives the operator a draggable
	// boundary so MeshCore "KO9OXR-T1000" rows fit comfortably while
	// FT8 mode can still snug the column down to taste.
	chatScope := container.NewHSplit(chatColStack, g.scope.container)
	chatScope.SetOffset(0.62)
	chanChatScope := container.NewHSplit(chanCol, chatScope)
	// Default offset positions chanCol around 140 px on a typical
	// 1100 px window. Operator drag adjusts as needed; mode flips
	// reset the offset via applySidebarForMode so MeshCore mode
	// always opens with enough room for contact names.
	chanChatScope.SetOffset(0.13)
	g.chanChatScope = chanChatScope
	root := container.NewBorder(nil, nil, modeCol, nil, chanChatScope)

	// Force the column widths to feel like Discord (mode rail thin,
	// chat takes the rest). chanCol's MinSize keeps the operator
	// from accidentally dragging the divider all the way closed.
	modeCol.Resize(fyne.NewSize(56, 720))
	modeBg.SetMinSize(fyne.NewSize(56, 0))
	chanBg.SetMinSize(fyne.NewSize(80, 0))

	// First-frame mode wiring. activeMode was hydrated from prefs at
	// the top of buildLayout; setActiveMode itself early-returns when
	// the requested mode equals the current one, so on a MeshCore
	// launch we apply the sidebar swap + scope flip + lazy connect
	// here instead.
	if g.activeMode == "meshcore" {
		g.applySidebarForMode()
		g.connectMeshcore()
	}

	return root
}

// SetWaterfallRow forwards a waterfall row to the embedded scope pane.
// Safe from any goroutine.
func (g *GUI) SetWaterfallRow(row waterfall.Row) {
	if g.scope != nil {
		g.scope.SetWaterfallRow(row)
	}
}

// AddSpots plots one batch of decoded stations on the map AND stamps
// signal-position markers into the waterfall. Caller passes the full
// per-slot decode set. Loopback decodes (myCall in fields[0]) are
// suppressed by the map widget.
func (g *GUI) AddSpots(results []ft8.Decoded) {
	if g.scope == nil {
		return
	}
	// Defense in depth: even if main.go's IsFT8Active gate is bypassed
	// (e.g. a slot in flight when the operator flips to MeshCore), no
	// FT8 spots reach the map / waterfall in MeshCore mode.
	if !g.IsFT8Active() {
		return
	}
	g.mu.Lock()
	myCall := g.myCall
	g.mu.Unlock()
	g.scope.AddSpots(results, myCall)
	g.scope.PaintDecodeMarkers(results)
}

// SetTxState toggles the Send button between Send and Stop. main calls this
// when entering TX (active=true, stopCh = the TxRequest's StopCh) and again
// when TX finishes (active=false). When active, clicking the button closes
// stopCh -the existing TX loop watches it and aborts playback + drops PTT.
func (g *GUI) SetTxState(active bool, stopCh chan struct{}) {
	g.mu.Lock()
	g.txActive = active
	g.activeStopCh = stopCh
	btn := g.sendBtn
	g.mu.Unlock()
	if btn == nil {
		return
	}
	fyne.Do(func() {
		if active {
			btn.SetText("Stop")
			btn.Importance = widget.DangerImportance
		} else {
			btn.SetText("Send")
			btn.Importance = widget.MediumImportance
		}
		btn.Refresh()
	})
}

// TxFreq returns the operator-selected TX audio frequency in Hz, set by
// clicking the waterfall. main reads this when encoding the next TX so
// the rig keys at the column the operator picked.
func (g *GUI) TxFreq() float64 {
	if g.scope == nil {
		return 1500
	}
	return g.scope.TxFreq()
}

// TxLevel returns the operator-selected TX audio amplitude in [0..1].
// Drives the encoder's level argument; on USB-CODEC rigs this maps
// roughly linearly to RF output via the rig's ALC, so the operator
// can tune it as a soft "TX power" control. Persisted in prefs;
// default 0.18 (≈ -15 dBFS, conservative to keep ALC happy).
func (g *GUI) TxLevel() float64 {
	if g.app == nil {
		return 0.18
	}
	return g.app.Preferences().FloatWithFallback("tx_level", 0.18)
}

// SetTxLevel persists a new TX level. Clamped to a sane range so a
// runaway slider can't blow the rig's ALC or drop transmissions to
// inaudibility.
func (g *GUI) SetTxLevel(v float64) {
	if g.app == nil {
		return
	}
	if v < 0.02 {
		v = 0.02
	}
	if v > 0.5 {
		v = 0.5
	}
	g.app.Preferences().SetFloat("tx_level", v)
}

// handleSubmit parses the input box and queues a TxRequest.
//
// Allowed inputs:
//
//	"CQ"                  → send "CQ <mycall> <mygrid>"
//	"<CALLSIGN>"          → start a directed call ("<them> <us> <grid>")
//	"<CALLSIGN> <TAIL>"   → directed message with explicit trailer
//	                        (TAIL ∈ ±NN | R±NN | RR73 | 73 | grid),
//	                        e.g. "VP2MAA -10" → "VP2MAA KO6IEH -10"
//	"/tune"               → pure-carrier tune transmission
//	"" (empty)            → ignore
//
// Anything else is rejected with an inline system message -keeps the user
// from accidentally transmitting free-form text on a digital QSO band.
func (g *GUI) handleSubmit(raw string) {
	g.mu.Lock()
	mode := g.activeMode
	g.mu.Unlock()
	if mode == "meshcore" {
		// MeshCore messages are free-form text — preserve case and
		// punctuation. Empty input is a no-op like FT8.
		text := strings.TrimRight(raw, "\r\n")
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		g.mcSendActiveThread(text)
		return
	}
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return
	}
	// Slash-command path: /tune emits a pure-carrier tune
	// transmission (no slot alignment, no FT8 modulation). Doesn't
	// require a callsign / grid since nothing is encoded.
	if s == "/TUNE" {
		req := TxRequest{Tune: true, StopCh: make(chan struct{})}
		select {
		case g.txCh <- req:
			g.AppendSystem("queued: tune (pure carrier)")
		default:
			g.AppendSystem("!TX queue full -try again")
		}
		return
	}
	g.mu.Lock()
	myCall, myGrid := g.myCall, g.myGrid
	g.mu.Unlock()
	if myCall == "" || myGrid == "" {
		g.AppendSystem("!profile not set -set MyCall / MyGrid before transmitting")
		return
	}
	tokens := strings.Fields(s)
	switch {
	case s == "CQ":
		req := TxRequest{
			Callsign: myCall, Grid: myGrid,
			AvoidPeriod: -1, // CQ broadcasts; no peer period to dodge
			StopCh:      make(chan struct{}),
		}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("queued: CQ %s %s", myCall, myGrid))
		default:
			g.AppendSystem("!TX queue full -try again")
		}
	case len(tokens) == 1 && isPlausibleCallsign(tokens[0]):
		req := TxRequest{
			Callsign: myCall, Grid: myGrid,
			RemoteCall:  tokens[0],
			AvoidPeriod: g.peerPeriod(tokens[0]),
			StopCh:      make(chan struct{}),
		}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("queued: directed call to %s", tokens[0]))
		default:
			g.AppendSystem("!TX queue full -try again")
		}
	case len(tokens) == 2 && isPlausibleCallsign(tokens[0]) && isPlausibleTail(tokens[1]):
		// Directed message with explicit trailer — operator typed
		// "VP2MAA -10" or "VP2MAA RR73". Same encoder path as a first
		// directed call; the Tail field replaces the grid.
		req := TxRequest{
			Callsign: myCall, Grid: myGrid,
			RemoteCall: tokens[0], Tail: tokens[1],
			AvoidPeriod: g.peerPeriod(tokens[0]),
			StopCh:      make(chan struct{}),
		}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("queued: %s %s %s", tokens[0], myCall, tokens[1]))
		default:
			g.AppendSystem("!TX queue full -try again")
		}
	default:
		g.AppendSystem(fmt.Sprintf("!%q is not \"CQ\", a valid callsign, or /tune -input rejected", raw))
	}
}

// isPlausibleTail reports whether tok is a valid trailing token for the
// "<them> <us> <tail>" directed-message shape: a sig report (±NN), R+sig
// (R±NN), the closing tokens RR73 / 73, or a Maidenhead grid. Used by
// the chat input parser so operators can manually advance a QSO by
// typing e.g. "VP2MAA -10" without reaching for buttons.
func isPlausibleTail(tok string) bool {
	tok = strings.ToUpper(tok)
	switch {
	case tok == "RR73", tok == "73":
		return true
	case len(tok) >= 3 && tok[0] == 'R' && (tok[1] == '+' || tok[1] == '-'):
		for _, c := range tok[2:] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	case len(tok) >= 2 && (tok[0] == '+' || tok[0] == '-'):
		for _, c := range tok[1:] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	case isGridLike(tok):
		return true
	}
	return false
}

// Run shows the window and blocks the main goroutine until the user closes
// the window. Caller must have spun up decoder/audio loops already.
func (g *GUI) Run() {
	g.window.ShowAndRun()
}

// formatRow produces the IRC-style line for one chat row.
//
//	15:37:15  +00 dB  NA-US   KR4NO VA7GEM -07
//	15:37:30   TX>  CQ KO6IEH DM13                (own TX, green)
//	15:38:01  |system: switched to #20m            (italic, grey)
func formatRow(r chatRow) string {
	if r.system {
		return fmt.Sprintf("       |%s", r.text)
	}
	t := r.when.UTC().Format("15:04:05")
	if r.tx {
		return fmt.Sprintf("%s  TX>  %s", t, r.text)
	}
	method := ""
	if r.method != "" {
		method = " ~" + r.method
	}
	region := r.region
	if region == "" {
		region = "-"
	}
	return fmt.Sprintf("%s  %+3.0f dB  %-7s %s%s",
		t, r.snrDB, region, r.text, method)
}

// formatRowSegments splits a chat row into (timestamp, meta, message)
// for FT8 rendering. MeshCore rows are formatted directly in the bind
// callback because they use a different set of widgets (split SNR +
// sender slots + graphical bar) — funneling them through this
// three-string helper would force string-padding tricks that misalign
// at non-monospace pixel boundaries.
func formatRowSegments(r chatRow) (ts, meta, msg string) {
	if r.separator {
		return "", "", strings.Repeat("─", 80)
	}
	if r.system {
		if r.mc {
			return "[" + r.when.UTC().Format("15:04:05") + "]", "", r.text
		}
		return "", "", fmt.Sprintf("* %s", r.text)
	}
	t := r.when.UTC().Format("15:04:05")
	if r.mc {
		// Cell contents are populated by the bind callback;
		// return ts only so the timestamp still renders.
		return "[" + t + "]", "", r.text
	}
	if r.tx {
		return t, " TX> ", r.text
	}
	region := r.region
	if region == "" {
		region = "-"
	}
	meta = fmt.Sprintf(" %+3.0f dB  %-7s ", r.snrDB, region)
	msg = r.text
	if r.method != "" {
		msg += " ~" + r.method
	}
	return t, meta, msg
}

// formatMcSnrCell renders the SNR cell for a MeshCore chat row.
// Empty when the firmware didn't report a value (older companion
// protocol < v3, or our own outbound TX) so the column collapses
// gracefully without a "+0.0 dB" placeholder.
func formatMcSnrCell(snr float64) string {
	if snr == 0 {
		return ""
	}
	return fmt.Sprintf("%+.1f dB", snr)
}

// mcSenderOrStar returns the IRC-style sender label, defaulting to
// "*" for system / unknown-sender rows.
func mcSenderOrStar(sender string) string {
	if sender == "" {
		return "*"
	}
	return sender
}

// chip renders a small filled rectangle with a label -used for the mode rail.
// tappableContainer wraps any CanvasObject so a tap fires onTap.
// Used for chip / icon visuals that aren't natively interactive.
// Implements fyne.Tappable on a BaseWidget so Fyne's pointer-event
// dispatch finds it and routes single-clicks here.
// historyEntry is the chat input box with shell-style Up/Down history
// recall. Every TX (manual or auto) gets pushed via push() — pressing
// Up walks backwards through that buffer and Down forward, with the
// in-progress draft preserved on first Up so Down at the bottom
// restores it. Dedupes consecutive identical pushes so a 4-attempt
// retry doesn't bloat the history with the same string.
type historyEntry struct {
	widget.Entry
	mu      sync.Mutex
	history []string
	cursor  int    // -1 = current draft, 0..len-1 = position in history
	saved   string // snapshot of the in-progress draft when navigation starts
	// completer is consulted on Tab to expand an @-mention partial
	// into a contact-name match. Returns matches sorted in the
	// preferred display order (typically alphabetical) so cycling is
	// stable. nil disables tab-completion (FT8 mode leaves it unset).
	completer func(prefix string) []string
	// tab tracks an in-progress mention cycle so repeated Tab presses
	// rotate through matches without re-querying the roster on each
	// keystroke. Reset when any non-Tab/Up/Down key arrives so a
	// regular edit cleanly drops out of cycle mode.
	tab tabCompletion
}

// tabCompletion is the in-progress @-mention cycle state. at is the
// '@' position in the text; inserted is the name we've currently
// substituted in (so the next Tab can locate and replace it).
type tabCompletion struct {
	active   bool
	at       int
	matches  []string
	idx      int
	inserted string
}

const historyMaxLen = 100

func newHistoryEntry() *historyEntry {
	e := &historyEntry{cursor: -1}
	e.ExtendBaseWidget(e)
	return e
}

func (e *historyEntry) push(s string) {
	if s == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := len(e.history); n > 0 && e.history[n-1] == s {
		return // skip consecutive duplicates (typical of retry sweeps)
	}
	e.history = append(e.history, s)
	if len(e.history) > historyMaxLen {
		e.history = e.history[len(e.history)-historyMaxLen:]
	}
	e.cursor = -1
}

// AcceptsTab claims the Tab key so the canvas focus traversal
// doesn't intercept it before TypedKey can run our @-mention
// completer. widget.Entry's default returns true only for multi-
// line entries; ours is single-line but needs Tab for completion.
func (e *historyEntry) AcceptsTab() bool { return true }

func (e *historyEntry) TypedKey(ev *fyne.KeyEvent) {
	switch ev.Name {
	case fyne.KeyUp:
		e.tab.active = false
		e.recall(-1)
	case fyne.KeyDown:
		e.tab.active = false
		e.recall(+1)
	case fyne.KeyTab:
		// Always swallow Tab — either it advances a mention
		// completion or it's a no-op. We override AcceptsTab so
		// focus-traversal can't reach the entry; falling through to
		// Entry.TypedKey would insert a literal tab character into
		// the chat draft, which is never what the operator wants.
		e.tryAtComplete()
	default:
		e.tab.active = false
		e.Entry.TypedKey(ev)
	}
}

// tryAtComplete handles the @-mention tab-complete cycle. Returns
// true when it consumed the Tab (cycled to the next match or
// inserted a new completion); false to let the caller fall through
// to the default Tab behaviour.
func (e *historyEntry) tryAtComplete() bool {
	if e.completer == nil {
		return false
	}
	text := e.Text
	cursor := e.CursorColumn
	if cursor > len(text) {
		cursor = len(text)
	}
	// Cycle path: still in the same mention slot we last completed.
	// inserted includes the trailing space, so cursor must sit just
	// past it.
	if e.tab.active && e.tab.at < len(text) && text[e.tab.at] == '@' &&
		cursor == e.tab.at+1+len(e.tab.inserted) && len(e.tab.matches) > 0 {
		e.tab.idx = (e.tab.idx + 1) % len(e.tab.matches)
		next := "[" + e.tab.matches[e.tab.idx] + "] "
		before := text[:e.tab.at+1]
		afterStart := e.tab.at + 1 + len(e.tab.inserted)
		var after string
		if afterStart <= len(text) {
			after = text[afterStart:]
		}
		e.tab.inserted = next
		newText := before + next + after
		e.SetText(newText)
		e.CursorColumn = len(before) + len(next)
		e.Refresh()
		return true
	}
	// Fresh completion: walk back from the cursor for the most recent
	// '@'; bail if we hit whitespace first (the cursor isn't inside a
	// mention token).
	at := -1
	for i := cursor - 1; i >= 0; i-- {
		c := text[i]
		if c == '@' {
			at = i
			break
		}
		if c == ' ' || c == '\t' || c == '\n' {
			return false
		}
	}
	if at < 0 {
		return false
	}
	prefix := text[at+1 : cursor]
	matches := e.completer(prefix)
	if len(matches) == 0 {
		return false
	}
	// Bracketed trailing-space form: "[Name] ". Brackets are the
	// MeshCore wire convention for mentions (rendered as a styled
	// highlight by upstream clients and by our own renderer, which
	// strips the brackets from display). Trailing space lets the
	// operator immediately type the message body without retyping
	// a separator. Cycling still works because the cursor is placed
	// past the space and inserted tracks the same "[name] " span
	// the next Tab will replace.
	first := "[" + matches[0] + "] "
	before := text[:at+1]
	after := text[cursor:]
	newText := before + first + after
	e.SetText(newText)
	e.CursorColumn = len(before) + len(first)
	e.Refresh()
	e.tab = tabCompletion{
		active:   true,
		at:       at,
		matches:  matches,
		idx:      0,
		inserted: first,
	}
	return true
}

// recall walks the history. dir=-1 = older (Up), dir=+1 = newer (Down).
// Past the newest entry restores the draft we snapshotted on first Up.
func (e *historyEntry) recall(dir int) {
	e.mu.Lock()
	if len(e.history) == 0 {
		e.mu.Unlock()
		return
	}
	if e.cursor < 0 {
		// First nav: snapshot whatever the operator was typing so
		// Down past the newest entry can restore it.
		e.saved = e.Text
		if dir < 0 {
			e.cursor = len(e.history) - 1
		} else {
			// Down with no nav in progress = no-op (nothing newer
			// than the draft).
			e.mu.Unlock()
			return
		}
	} else {
		e.cursor += dir
		if e.cursor < 0 {
			e.cursor = 0
		} else if e.cursor >= len(e.history) {
			e.cursor = -1
		}
	}
	var text string
	if e.cursor < 0 {
		text = e.saved
	} else {
		text = e.history[e.cursor]
	}
	e.mu.Unlock()
	e.SetText(text)
	e.CursorColumn = len(text)
	e.Refresh()
}

type tappableContainer struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onTap   func()
}

func newTappableContainer(content fyne.CanvasObject, onTap func()) *tappableContainer {
	t := &tappableContainer{content: content, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tappableContainer) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.content)
}

func (t *tappableContainer) Tapped(_ *fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

// circleBadge renders a colored circle with up to a few characters of
// text inside it — used as a visual marker in the HEARD roster + on
// the map. Shape is enforced via canvas.Circle (Fyne primitive that
// renders a perfect filled disk regardless of pixel density). The
// label is centred and clipped to the badge's diameter via a Stack.
//
// size = the circle's diameter in DIPs.
// blankBadgeImage returns a small fully-transparent RGBA used as the
// initial source for an HEARD-list OTA slot's canvas.Image. The
// binder swaps in the real BadgeImage when a row activates an OTA
// programme; this placeholder keeps the slot a valid image until then
// (canvas.NewImageFromImage requires a non-nil source).
func blankBadgeImage() *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, 1, 1))
}

func circleBadge(label string, bg color.Color, fg color.Color, size float32) *fyne.Container {
	// Transparent sizer rect enforces the diameter — canvas.Circle
	// itself has no SetMinSize, but a Stack adopts the largest
	// MinSize among its children.
	sizer := canvas.NewRectangle(color.Transparent)
	sizer.SetMinSize(fyne.NewSize(size, size))
	c := canvas.NewCircle(bg)
	c.StrokeWidth = 0
	t := canvas.NewText(label, fg)
	t.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	// Font size that fits 2 chars comfortably inside the diameter
	// without crowding. 0.55× the diameter is a good empirical fit
	// for the gomonobold rendering Fyne uses by default.
	t.TextSize = size * 0.55
	t.Alignment = fyne.TextAlignCenter
	return container.NewStack(sizer, c, container.NewCenter(t))
}

// otaBadgeStyle returns the label + colour pair for an OTA program.
// Same palette is used in the HEARD roster (circleBadge slot) and on
// the map (mapDrawCircleBadgeLabel), so the visual identity of each
// programme is consistent across both surfaces. Cartoony bright fills
// with white text — readable at the small badge sizes both views use.
//
// Returns ok=false for unknown types so the caller can hide the slot.
func otaBadgeStyle(otaType string) (label string, bg, fg color.RGBA, ok bool) {
	white := color.RGBA{255, 255, 255, 255}
	switch otaType {
	case "POTA":
		return "P", color.RGBA{60, 180, 75, 255}, white, true // park green
	case "SOTA":
		return "S", color.RGBA{220, 130, 30, 255}, white, true // summit amber
	case "WWFF":
		return "WF", color.RGBA{50, 200, 170, 255}, white, true // flora teal
	case "IOTA":
		return "I", color.RGBA{60, 130, 230, 255}, white, true // island blue
	case "BOTA":
		return "B", color.RGBA{90, 170, 240, 255}, white, true // beach sky
	case "LOTA":
		return "L", color.RGBA{240, 200, 60, 255}, white, true // lighthouse yellow
	case "NOTA":
		return "N", color.RGBA{180, 110, 200, 255}, white, true // nuns purple
	case "PORTABLE":
		return "/P", color.RGBA{160, 160, 160, 255}, white, true // generic grey
	}
	return "", color.RGBA{}, color.RGBA{}, false
}

// updateCircleBadge mutates a circleBadge in place — used by list
// row binders that need to re-colour / re-label the same persistent
// widget on every refresh, rather than rebuilding the whole row.
//
// Mirrors circleBadge's child layout: Stack of [sizer, circle,
// Center(text)]. Defensive on the type assertions so a future change
// to circleBadge fails loudly rather than silently no-op'ing.
func updateCircleBadge(badge *fyne.Container, label string, bg, fg color.Color) {
	if badge == nil || len(badge.Objects) < 3 {
		return
	}
	if c, ok := badge.Objects[1].(*canvas.Circle); ok {
		c.FillColor = bg
		c.Refresh()
	}
	if center, ok := badge.Objects[2].(*fyne.Container); ok && len(center.Objects) > 0 {
		if t, ok := center.Objects[0].(*canvas.Text); ok {
			t.Text = label
			t.Color = fg
			t.Refresh()
		}
	}
}

func chip(label string, fill color.Color) *fyne.Container {
	bg := canvas.NewRectangle(fill)
	bg.CornerRadius = 8
	t := canvas.NewText(label, color.White)
	t.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	t.TextSize = 11
	t.Alignment = fyne.TextAlignCenter
	return container.NewStack(bg, container.NewPadded(t))
}

// modeChipFill returns the background colour for a mode-rail chip
// based on whether it represents the active mode. Active = Discord
// blurple; inactive = slate grey so the eye lands on the active chip
// at a glance.
func modeChipFill(chipMode, activeMode string) color.Color {
	if chipMode == activeMode {
		return color.RGBA{88, 101, 242, 255} // Discord blurple
	}
	return color.RGBA{60, 60, 60, 255}
}

// setActiveMode flips the active-mode state, repaints the mode-rail
// chips, swaps the right-pane layout (FT8 = waterfall+map, MeshCore =
// map only) and the channel-column sidebar (FT8 = bands list,
// MeshCore = Contacts/Channels), snapshots the per-mode chat buffer
// so the chat list shows the correct history for the new mode, and
// persists the choice. Safe to call from any goroutine.
func (g *GUI) setActiveMode(mode string) {
	if mode != "ft8" && mode != "meshcore" {
		return
	}
	g.mu.Lock()
	prev := g.activeMode
	if prev == mode {
		g.mu.Unlock()
		return
	}
	g.activeMode = mode
	g.mu.Unlock()
	if g.app != nil {
		g.app.Preferences().SetString("active_mode", mode)
	}
	g.applyChatBufferForMode(prev)
	fyne.Do(func() {
		g.refreshModeRail()
		if g.scope != nil {
			if mode == "meshcore" {
				g.scope.SetMeshcoreLayout(g.buildMeshcoreRxLog())
			} else {
				g.scope.SetWaterfallVisible(true)
			}
		}
		g.applySidebarForMode()
		g.refreshStatus()
	})
	switch mode {
	case "meshcore":
		// Announcement goes through mcAppendSystem so it lands in
		// the now-active MeshCore view; AppendSystem would route to
		// ft8RowsBackup since the mode flip already completed.
		g.mcAppendSystem("mode: MeshCore (waterfall hidden, map active)")
		g.connectMeshcore()
	default:
		g.AppendSystem("mode: FT8")
	}
}

// IsFT8Active reports whether the operator is currently in FT8 mode
// (the only mode where the FT8 decoder pipeline should run). main.go
// uses this as a gate so the per-slot decode + spot-ingest path
// short-circuits in MeshCore mode — saves a few hundred ms of CPU
// per slot and stops new FT8 stations from popping onto the map
// while the operator is on a mesh thread.
func (g *GUI) IsFT8Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeMode == "" || g.activeMode == "ft8"
}

// refreshModeRail repaints the FT8 / MeshCore chip backgrounds based
// on g.activeMode. Must be called from the UI goroutine.
func (g *GUI) refreshModeRail() {
	repaint := func(c *fyne.Container, mode string) {
		if c == nil || len(c.Objects) == 0 {
			return
		}
		bg, ok := c.Objects[0].(*canvas.Rectangle)
		if !ok {
			return
		}
		bg.FillColor = modeChipFill(mode, g.activeMode)
		bg.Refresh()
	}
	repaint(g.ft8Chip, "ft8")
	repaint(g.meshChip, "meshcore")
}

// MeshCore preference keys. Namespaced under "meshcore." so they
// can never collide with FT8's flat-key prefs and so a future
// per-mode settings reorganisation can grep them in one place.
//
// Transport selects the link layer: "usb" (default) reads serial
// device prefs (board/port/baud); "ble" reads BLE device prefs
// (address + display name). Storing both transports' state lets the
// operator flip between them without losing the other side's pick.
const (
	mcPrefTransport        = "meshcore.transport"
	mcPrefDeviceBoard      = "meshcore.device.board"
	mcPrefDevicePort       = "meshcore.device.port"
	mcPrefDeviceBaud       = "meshcore.device.baud"
	mcPrefBLEAddress       = "meshcore.ble.address"
	mcPrefBLEDeviceName    = "meshcore.ble.device_name"
	mcPrefProfileName      = "meshcore.profile.name"
	mcPrefProfileLat       = "meshcore.profile.lat"
	mcPrefProfileLon       = "meshcore.profile.lon"
	mcPrefProfileManualAdd = "meshcore.profile.manual_add"
	// Auto-reconnect interval in MINUTES. 0 disables. Default 5 min
	// is a battery-conscious choice for BLE-attached trackers like
	// the T1000-E — aggressive reconnect would keep the radio's
	// BLE radio active and drain the cell. macOS sleep/wake is the
	// common case where reconnect is needed.
	mcPrefAutoReconnectMin    = "meshcore.auto_reconnect_minutes"
	mcDefaultAutoReconnectMin = 5
)

// MeshCore transport identifiers as persisted in mcPrefTransport.
const (
	mcTransportUSB = "usb"
	mcTransportBLE = "ble"
)

// ─── MeshCore mode plumbing ───────────────────────────────────────
//
// The MeshCore mode shares one chat-list widget and one input field
// with FT8. Mode and thread switching swap g.rows out from under that
// widget — the per-conversation buffer for the active thread lives in
// g.rows so the existing rendering / scrolling / hover code paths
// don't need to know about the conversation model. The "previous"
// FT8 buffer is stashed in ft8RowsBackup; per-thread MeshCore buffers
// in mcThreadHistory.

// mcPendingSend ties an outbound message's expected-ack CRC to the
// chat row that's waiting for confirmation. Stored in
// GUI.mcPendingByAck, which the PushSendConfirmed handler looks up
// to flip the row to Delivered, and which the per-second sweeper
// scans to flip stale rows to Failed. recipient is the destination
// pubkey for DMs (zero for channel sends) — used to track
// consecutive failures per contact for the auto-path-reset feature.
type mcPendingSend struct {
	thread    string
	rowIdx    int
	sentAt    time.Time
	recipient meshcore.PubKey
}

// mcAutoResetThreshold is the number of consecutive Failed DMs to
// the same contact that triggers an automatic CmdResetPath. Two
// failures takes ~3 minutes (2 × mcPendingTimeout = 2 × 90s) before
// the auto-recovery kicks in — long enough to avoid resetting on
// transient noise, short enough that an operator isn't stuck
// silently retrying a dead path forever.
const mcAutoResetThreshold = 2

// mcRxLogEntry is one row in the RxLog viewer — a single mesh
// packet the radio decoded off-air, surfaced via PushLogRxData.
// Mirrors the fields the web client's RxLogPage shows. raw holds
// the exact packet bytes so the inspect modal can show a hex dump.
type mcRxLogEntry struct {
	when    time.Time
	route   string  // "FLOOD" / "DIRECT" / "TRANSPORT_*"
	payload string  // "TXT_MSG" / "ADVERT" / "PATH" / …
	hops    int     // path-hash count
	snr     float64 // dBm-ish (raw_snr / 4)
	rssi    int     // dBm
	raw     []byte  // verbatim packet bytes for the inspect modal
	packet  meshcore.Packet
}

// maxMcRxLog caps the in-memory RxLog ring. ~200 entries is enough
// to keep ~20 minutes of a moderately busy mesh visible without the
// list rendering churn from longer slices becoming noticeable.
const maxMcRxLog = 200

// mcThreadID returns the map key for a per-conversation chat buffer.
// kind is "contact" or "channel"; id is the lowercase hex pubkey
// prefix for contacts or the decimal channel index for channels.
func mcThreadID(kind, id string) string { return kind + ":" + id }

// mcContactThreadID is the convenience version for a Contact —
// derives the 6-byte pubkey prefix as lowercase hex.
func mcContactThreadID(c meshcore.Contact) string {
	return mcThreadID("contact", fmt.Sprintf("%x", c.PubKey[:6]))
}

// mcContactIcon returns the icon resource for a contact's advertised
// type. Chat contacts use Fyne's built-in person silhouette; the
// other three types use the custom SVGs in assets/.
func mcContactIcon(t meshcore.AdvType) fyne.Resource {
	switch t {
	case meshcore.AdvTypeRepeater:
		return MeshRepeaterResource
	case meshcore.AdvTypeRoom:
		return MeshRoomResource
	case meshcore.AdvTypeSensor:
		return MeshSensorResource
	default:
		return theme.AccountIcon()
	}
}

// mcChannelThreadID is the convenience version for a Channel.
func mcChannelThreadID(c meshcore.Channel) string {
	return mcThreadID("channel", fmt.Sprintf("%d", c.Index))
}

// mcChannelLabel returns the display label for a channel + a flag
// indicating whether the firmware-assigned name already starts with
// "#" (the operator-named public/group channels). The flag drives
// icon selection: hash glyph for #-prefixed names, repeater glyph
// for plain names like "Public" that the firmware seeds. The label
// preserves an existing "#" prefix verbatim instead of stacking a
// second one — matches the convention in the upstream web client.
func mcChannelLabel(c meshcore.Channel) (label string, hasHashPrefix bool) {
	name := c.Name
	if name == "" {
		name = fmt.Sprintf("ch%d", c.Index)
	}
	if strings.HasPrefix(name, "#") {
		return name, true
	}
	return name, false
}

// mcThreadLabel returns a user-facing label for the active thread —
// "DM with KO9OXR" for a contact, "#general" for a channel, or
// "MeshCore" when nothing is selected. Used by refreshStatus to fill
// the topic bar in MeshCore mode.
func (g *GUI) mcThreadLabel() string {
	g.mcMu.Lock()
	thread := g.mcCurrentThread
	contacts := g.mcContacts
	channels := g.mcChannels
	g.mcMu.Unlock()
	if thread == "" {
		return "MeshCore"
	}
	for _, c := range contacts {
		if mcContactThreadID(c) == thread {
			name := c.AdvName
			if name == "" {
				name = fmt.Sprintf("%x", c.PubKey[:4])
			}
			return "DM with " + name
		}
	}
	for _, ch := range channels {
		if mcChannelThreadID(ch) == thread {
			label, _ := mcChannelLabel(ch)
			return label
		}
	}
	return "MeshCore"
}

// buildMeshcoreSidebar lazily constructs the Contacts / Channels
// sidebar that replaces the bands list in MeshCore mode. Idempotent —
// returns the cached container on subsequent calls so list-data
// pointers stay stable across mode flips.
func (g *GUI) buildMeshcoreSidebar() *fyne.Container {
	if g.mcSidebar != nil {
		return g.mcSidebar
	}
	g.mcContactsHeader = canvas.NewText("CONTACTS  (0)", color.RGBA{140, 140, 145, 255})
	g.mcContactsHeader.TextSize = 11
	g.mcContactsHeader.TextStyle = fyne.TextStyle{Bold: true}
	g.mcChannelsHeader = canvas.NewText("CHANNELS  (0)", color.RGBA{140, 140, 145, 255})
	g.mcChannelsHeader.TextSize = 11
	g.mcChannelsHeader.TextStyle = fyne.TextStyle{Bold: true}

	g.mcContactsList = widget.NewList(
		func() int {
			g.mcMu.Lock()
			defer g.mcMu.Unlock()
			return len(g.mcContacts)
		},
		func() fyne.CanvasObject {
			// Row template: [star][icon-slot][name]. Star is a
			// tappableContainer-wrapped canvas.Image — outline
			// (gray) for non-favourite, solid (warm yellow) when
			// favourited. The bind callback swaps the underlying
			// SVG resource per-row. Tapping the star toggles
			// favourite without selecting the row (the surrounding
			// hoverRow's onTap still handles selection on clicks
			// anywhere else).
			star := canvas.NewImageFromResource(StarResource)
			star.FillMode = canvas.ImageFillContain
			star.SetMinSize(fyne.NewSize(14, 14))
			starTap := newTappableContainer(star, nil) // bound per-row
			starSlot := container.New(&fixedWidthLayout{width: 18}, starTap)
			icon := canvas.NewImageFromResource(theme.AccountIcon())
			icon.FillMode = canvas.ImageFillContain
			icon.SetMinSize(fyne.NewSize(16, 16))
			iconSlot := container.New(&fixedWidthLayout{width: 20}, icon)
			leading := container.NewHBox(starSlot, iconSlot)
			t := canvas.NewText("", color.RGBA{200, 200, 210, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 13
			row := newHoverRow(container.NewPadded(container.NewBorder(nil, nil, leading, nil, t)))
			row.onTap = func() { g.mcContactsList.Select(row.listIdx) }
			row.onSecondary = func(absPos fyne.Position) {
				g.showMcContactContextMenu(row.listIdx, absPos)
			}
			return row
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*hoverRow)
			padded := row.inner.(*fyne.Container)
			border := padded.Objects[0].(*fyne.Container)
			leading := border.Objects[1].(*fyne.Container)
			starSlot := leading.Objects[0].(*fyne.Container)
			starTap := starSlot.Objects[0].(*tappableContainer)
			star := starTap.content.(*canvas.Image)
			iconSlot := leading.Objects[1].(*fyne.Container)
			icon := iconSlot.Objects[0].(*canvas.Image)
			t := border.Objects[0].(*canvas.Text)
			row.listIdx = id
			g.mcMu.Lock()
			if id >= len(g.mcContacts) {
				g.mcMu.Unlock()
				return
			}
			ct := g.mcContacts[id]
			active := mcContactThreadID(ct) == g.mcCurrentThread
			g.mcMu.Unlock()
			name := ct.AdvName
			if name == "" {
				name = fmt.Sprintf("%x", ct.PubKey[:4])
			}
			icon.Resource = mcContactIcon(ct.Type)
			icon.Refresh()
			if g.mcIsFavorite(ct.PubKey) {
				star.Resource = StarFilledResource
			} else {
				star.Resource = StarResource
			}
			star.Refresh()
			pub := ct.PubKey
			starTap.onTap = func() { g.mcToggleFavorite(pub) }
			thread := mcContactThreadID(ct)
			unread := g.mcUnreadCount(thread)
			mentioned := g.mcIsMentioned(thread)
			label := name
			if unread > 0 {
				if mentioned {
					label = fmt.Sprintf("%s  (@%d)", label, unread)
				} else {
					label = fmt.Sprintf("%s  (%d)", label, unread)
				}
			}
			t.Text = label
			switch {
			case active:
				t.Color = color.RGBA{255, 255, 255, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case mentioned:
				// Mentioned, not selected — warm amber, the "you got
				// pinged" colour. Stronger pull than plain unread so
				// directed call-outs in busy channels stand out.
				t.Color = color.RGBA{255, 195, 80, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case unread > 0:
				// Unread, not selected — bright cyan + bold to
				// match Slack's "you have new messages" cue.
				t.Color = color.RGBA{120, 220, 255, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			default:
				t.Color = color.RGBA{200, 200, 210, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true}
			}
			t.Refresh()
		},
	)
	g.mcContactsList.OnSelected = func(id widget.ListItemID) {
		g.mcMu.Lock()
		if id >= len(g.mcContacts) {
			g.mcMu.Unlock()
			return
		}
		ct := g.mcContacts[id]
		g.mcMu.Unlock()
		g.mcSwitchThread(mcContactThreadID(ct))
		if g.mcChannelsList != nil {
			g.mcChannelsList.UnselectAll()
		}
	}

	g.mcChannelsList = widget.NewList(
		func() int {
			g.mcMu.Lock()
			defer g.mcMu.Unlock()
			return len(g.mcChannels)
		},
		func() fyne.CanvasObject {
			// Same row template shape as the contacts list:
			// fixed-width icon slot + monospace name. Icon is
			// swapped per-row in the bind callback (hash glyph
			// for #-prefixed names, repeater glyph for the
			// public / unnamed channels). Wrapped in a hoverRow
			// so right-click opens an Info / Remove menu —
			// listIdx is set in bind so the menu callback can
			// look up the underlying channel without capturing
			// stale values.
			icon := canvas.NewImageFromResource(MeshHashResource)
			icon.FillMode = canvas.ImageFillContain
			icon.SetMinSize(fyne.NewSize(16, 16))
			iconSlot := container.New(&fixedWidthLayout{width: 20}, icon)
			t := canvas.NewText("", color.RGBA{200, 200, 210, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 13
			row := newHoverRow(container.NewPadded(container.NewBorder(nil, nil, iconSlot, nil, t)))
			row.onTap = func() { g.mcChannelsList.Select(row.listIdx) }
			row.onSecondary = func(absPos fyne.Position) {
				g.showMcChannelContextMenu(row.listIdx, absPos)
			}
			return row
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*hoverRow)
			padded := row.inner.(*fyne.Container)
			border := padded.Objects[0].(*fyne.Container)
			iconSlot := border.Objects[1].(*fyne.Container)
			icon := iconSlot.Objects[0].(*canvas.Image)
			t := border.Objects[0].(*canvas.Text)
			row.listIdx = id
			g.mcMu.Lock()
			if id >= len(g.mcChannels) {
				g.mcMu.Unlock()
				return
			}
			ch := g.mcChannels[id]
			active := mcChannelThreadID(ch) == g.mcCurrentThread
			g.mcMu.Unlock()
			label, hasHashPrefix := mcChannelLabel(ch)
			thread := mcChannelThreadID(ch)
			unread := g.mcUnreadCount(thread)
			mentioned := g.mcIsMentioned(thread)
			if unread > 0 {
				if mentioned {
					label = fmt.Sprintf("%s  (@%d)", label, unread)
				} else {
					label = fmt.Sprintf("%s  (%d)", label, unread)
				}
			}
			t.Text = label
			if hasHashPrefix {
				icon.Resource = MeshHashResource
			} else {
				icon.Resource = MeshRepeaterResource
			}
			icon.Refresh()
			switch {
			case active:
				t.Color = color.RGBA{255, 255, 255, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case mentioned:
				// Channel has an unread @[<self>] — warm amber for
				// the directed call-out, same convention as the
				// contacts list and the inline mention render.
				t.Color = color.RGBA{255, 195, 80, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			case unread > 0:
				t.Color = color.RGBA{120, 220, 255, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			default:
				t.Color = color.RGBA{200, 200, 210, 255}
				t.TextStyle = fyne.TextStyle{Monospace: true}
			}
			t.Refresh()
		},
	)
	g.mcChannelsList.OnSelected = func(id widget.ListItemID) {
		g.mcMu.Lock()
		if id >= len(g.mcChannels) {
			g.mcMu.Unlock()
			return
		}
		ch := g.mcChannels[id]
		g.mcMu.Unlock()
		g.mcSwitchThread(mcChannelThreadID(ch))
		if g.mcContactsList != nil {
			g.mcContactsList.UnselectAll()
		}
	}

	g.mcStatusText = canvas.NewText("", color.RGBA{160, 165, 175, 255})
	g.mcStatusText.TextStyle = fyne.TextStyle{Italic: true}
	g.mcStatusText.TextSize = 10

	divider := canvas.NewRectangle(color.RGBA{60, 63, 70, 255})
	divider.SetMinSize(fyne.NewSize(0, 1))

	// Channels header gets a "+" add-channel button on the right.
	// Tapping it opens an Add Channel dialog (name + base64 secret)
	// — popup menu offers "Add Hashtag Channel" (auto-derives the
	// 16-byte secret from the #-prefixed name) and "Add Private
	// Channel" (operator pastes a shared secret out of band).
	// Both backends find the first empty slot and call SetChannel.
	addBtn := widget.NewButtonWithIcon("", theme.ContentAddIcon(), nil)
	addBtn.Importance = widget.LowImportance
	addBtn.OnTapped = func() {
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(addBtn)
		pos.Y += addBtn.Size().Height
		menu := fyne.NewMenu("",
			fyne.NewMenuItem("Add Hashtag Channel…", func() { g.showMcAddHashtagChannelDialog() }),
			fyne.NewMenuItem("Add Private Channel…", func() { g.showMcAddPrivateChannelDialog() }),
		)
		widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), pos)
	}
	channelsHeader := container.NewBorder(
		nil, nil,
		container.NewPadded(g.mcChannelsHeader),
		addBtn, nil,
	)

	// Contacts header gets a popup menu (sort + bulk delete) on the
	// right edge. Single button keeps the header tight; the popup
	// hosts the rare actions so the common case (scroll the list)
	// stays uncluttered.
	contactsMenuBtn := widget.NewButtonWithIcon("", theme.MoreVerticalIcon(), nil)
	contactsMenuBtn.Importance = widget.LowImportance
	contactsMenuBtn.OnTapped = func() {
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(contactsMenuBtn)
		pos.Y += contactsMenuBtn.Size().Height
		menu := fyne.NewMenu("",
			fyne.NewMenuItem("Sort by Recent", func() { g.mcSetContactsSortBy(mcContactsSortRecent) }),
			fyne.NewMenuItem("Sort by Name", func() { g.mcSetContactsSortBy(mcContactsSortName) }),
			fyne.NewMenuItem("Sort by Type", func() { g.mcSetContactsSortBy(mcContactsSortType) }),
			fyne.NewMenuItem("Sort by Distance", func() { g.mcSetContactsSortBy(mcContactsSortDistance) }),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Bulk delete…", func() { g.showMcBulkDeleteDialog() }),
		)
		widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), pos)
	}
	contactsHeader := container.NewBorder(
		nil, nil,
		container.NewPadded(g.mcContactsHeader),
		contactsMenuBtn, nil,
	)

	// Two scrollable lists stacked vertically with proportional weight
	// — contacts get the top half, channels the bottom. fixedHeight
	// won't work here because Fyne lists need a parent that reports a
	// real height. NewVSplit with a 0.6 default keeps both lists usable
	// at typical sidebar widths.
	listsSplit := container.NewVSplit(
		container.NewBorder(contactsHeader, nil, nil, nil, g.mcContactsList),
		container.NewBorder(channelsHeader, nil, nil, nil, g.mcChannelsList),
	)
	listsSplit.SetOffset(0.6)

	g.mcSidebar = container.NewBorder(
		nil,
		container.NewPadded(g.mcStatusText),
		nil, nil,
		container.NewBorder(nil, divider, nil, nil, listsSplit),
	)
	return g.mcSidebar
}

// mcRefreshLists repaints the contacts/channels lists + their header
// counts. Safe from any goroutine; UI mutations are dispatched via
// fyne.Do.
func (g *GUI) mcRefreshLists() {
	if g.mcContactsList == nil || g.mcChannelsList == nil {
		return
	}
	g.mcMu.Lock()
	nc := len(g.mcContacts)
	nch := len(g.mcChannels)
	g.mcMu.Unlock()
	fyne.Do(func() {
		if g.mcContactsHeader != nil {
			g.mcContactsHeader.Text = fmt.Sprintf("CONTACTS  (%d)", nc)
			g.mcContactsHeader.Refresh()
		}
		if g.mcChannelsHeader != nil {
			g.mcChannelsHeader.Text = fmt.Sprintf("CHANNELS  (%d)", nch)
			g.mcChannelsHeader.Refresh()
		}
		g.mcContactsList.Refresh()
		g.mcChannelsList.Refresh()
	})
}

// mcSetStatus writes a one-line italic note under the meshcore
// sidebar. Used for "connecting…", "no device configured", and
// transient error reporting.
func (g *GUI) mcSetStatus(msg string) {
	if g.mcStatusText == nil {
		return
	}
	fyne.Do(func() {
		g.mcStatusText.Text = msg
		g.mcStatusText.Refresh()
	})
}

// mcSwitchThread snapshots the current g.rows into mcThreadHistory
// (under the previous thread key) and swaps in the requested thread's
// buffer. Empty buffer if the thread has no history yet. Repaints the
// chat list and the topic bar.
func (g *GUI) mcSwitchThread(newThread string) {
	g.mu.Lock()
	prev := g.mcCurrentThread
	if prev == newThread {
		g.mu.Unlock()
		return
	}
	if g.mcThreadHistory == nil {
		g.mcThreadHistory = map[string][]chatRow{}
	}
	if prev != "" {
		// Snapshot the outgoing thread's rows. Copy so subsequent
		// mutations to g.rows don't bleed back into the saved buffer.
		snap := make([]chatRow, len(g.rows))
		copy(snap, g.rows)
		g.mcThreadHistory[prev] = snap
	}
	g.mcCurrentThread = newThread
	hist := g.mcThreadHistory[newThread]
	if hist == nil {
		g.rows = nil
	} else {
		g.rows = make([]chatRow, len(hist))
		copy(g.rows, hist)
	}
	g.mu.Unlock()
	// Selecting a thread reads it — drop any unread badge for it.
	g.mcClearUnread(newThread)
	fyne.Do(func() {
		if g.chatList != nil {
			g.chatList.Refresh()
			if n := len(g.rows); n > 0 {
				g.chatList.ScrollTo(n - 1)
			}
		}
		g.mcRefreshLists()
		g.refreshStatus()
	})
	g.mcRefreshRoster()
}

// mcAppendSystem surfaces a MeshCore-origin system notification —
// connect / disconnect / send-failure / mode-switch — in the
// MeshCore chat. Drops the message silently if the operator isn't
// currently viewing MeshCore (these are ephemeral status updates;
// persisting them per-thread would surface stale "connected" lines
// when the operator flips back days later). Always safe to call
// from any goroutine; UI mutations are dispatched via fyne.Do.
func (g *GUI) mcAppendSystem(text string) {
	g.mu.Lock()
	if g.activeMode != "meshcore" {
		g.mu.Unlock()
		return
	}
	g.rows = append(g.rows, chatRow{
		when:   time.Now().UTC(),
		system: true,
		text:   text,
	})
	g.trimRows()
	n := len(g.rows)
	g.mu.Unlock()
	if g.chatList != nil {
		fyne.Do(func() {
			g.chatList.Refresh()
			g.chatList.ScrollTo(n - 1)
		})
	}
}

// mcContactsRefreshDelay debounces advert-driven contact refreshes.
// Adverts within this window are coalesced into a single
// GetContactsSince call against the cached lastMod, so a 200-node
// mesh chattering at 1 advert/sec produces ~1 refresh per window
// instead of 200. Bumped from 3 s to 30 s after a 14 h overnight
// session showed ~17 refreshes/hr — the radio was constantly busy
// satisfying these instead of operator-initiated commands. 30 s
// trades sidebar freshness for command responsiveness.
const mcContactsRefreshDelay = 30 * time.Second

// mcCurrentRoster scans the active MeshCore thread's chat rows and
// returns the unique senders ordered most-recent-first. Used to
// populate the MeshCore-mode roster column with channel members the
// operator has actually heard from. mcSender is set explicitly on
// every MeshCore row at receive / send time, so this is a flat
// dedup walk.
func (g *GUI) mcCurrentRoster() []string {
	g.mu.Lock()
	thread := g.mcCurrentThread
	rows := append([]chatRow(nil), g.rows...)
	g.mu.Unlock()
	if thread == "" || !strings.HasPrefix(thread, "channel:") {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.system || r.separator || r.mcSender == "" {
			continue
		}
		if seen[r.mcSender] {
			continue
		}
		seen[r.mcSender] = true
		out = append(out, r.mcSender)
	}
	return out
}

// MeshCore contact-sort modes persisted on the GUI. Each maps to a
// less-than function used by sortMcContacts.
const (
	mcContactsSortRecent   = "recent"   // newest LastAdvert first (default)
	mcContactsSortName     = "name"     // alphabetical by AdvName, case-insensitive
	mcContactsSortType     = "type"     // by AdvType (Repeater / Room / Sensor / Chat), then name
	mcContactsSortDistance = "distance" // closest to operator first; unknown-position contacts sink to bottom
)

// mcContactsSortMode returns the operator's chosen sort, defaulting
// to Recent on first use. Threaded into every sort call so a single
// mode change updates the sidebar uniformly without per-callsite
// branching.
func (g *GUI) mcContactsSortMode() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mcContactsSortBy == "" {
		return mcContactsSortRecent
	}
	return g.mcContactsSortBy
}

// mcSetContactsSortBy updates the sidebar sort, re-sorts the
// roster in place, and refreshes the lists. Called from the
// CONTACTS header menu items (Sort by Recent / Name / Type).
func (g *GUI) mcSetContactsSortBy(mode string) {
	g.mu.Lock()
	g.mcContactsSortBy = mode
	g.mu.Unlock()
	g.mcMu.Lock()
	g.sortMcContactsLocked(g.mcContacts, mode)
	g.mcMu.Unlock()
	g.mcRefreshLists()
}

// sortMcContactsLocked sorts the contact slice in place per the
// given mode then partitions favourites to the top. Caller must
// hold mcMu — the distance sort reads g.mcSelfInfo and the
// favourites partition reads g.mcFavorites.
func (g *GUI) sortMcContactsLocked(contacts []meshcore.Contact, mode string) {
	defer g.partitionFavoritesLocked(contacts)
	if mode != mcContactsSortDistance {
		sortMcContacts(contacts, mode)
		return
	}
	// Distance sort needs the operator's reference position. Fall
	// back to recency-sort when the radio hasn't supplied a
	// SelfInfo position (otherwise everything would be sorted by
	// "distance from null island" which is just longitude order).
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	if selfLat == 0 && selfLon == 0 {
		sortMcContacts(contacts, mcContactsSortRecent)
		return
	}
	sort.SliceStable(contacts, func(i, j int) bool {
		di, oki := distanceMiles(selfLat, selfLon, contacts[i].LatDegrees(), contacts[i].LonDegrees(), contacts[i].AdvLatE6, contacts[i].AdvLonE6)
		dj, okj := distanceMiles(selfLat, selfLon, contacts[j].LatDegrees(), contacts[j].LonDegrees(), contacts[j].AdvLatE6, contacts[j].AdvLonE6)
		// Contacts without a position go to the bottom of the list.
		if oki != okj {
			return oki
		}
		return di < dj
	})
}

func sortMcContacts(contacts []meshcore.Contact, mode string) {
	switch mode {
	case mcContactsSortName:
		sort.SliceStable(contacts, func(i, j int) bool {
			return strings.ToLower(contacts[i].AdvName) < strings.ToLower(contacts[j].AdvName)
		})
	case mcContactsSortType:
		sort.SliceStable(contacts, func(i, j int) bool {
			if contacts[i].Type != contacts[j].Type {
				return contacts[i].Type < contacts[j].Type
			}
			return strings.ToLower(contacts[i].AdvName) < strings.ToLower(contacts[j].AdvName)
		})
	default: // mcContactsSortRecent
		sort.SliceStable(contacts, func(i, j int) bool {
			return contacts[i].LastAdvert.After(contacts[j].LastAdvert)
		})
	}
}

// partitionFavoritesLocked reorders the slice in place so every
// favourited contact lands ahead of every non-favourited one,
// preserving the relative order of each partition (i.e. respects
// whatever sort just ran). Caller must hold mcMu.
func (g *GUI) partitionFavoritesLocked(contacts []meshcore.Contact) {
	if len(g.mcFavorites) == 0 || len(contacts) < 2 {
		return
	}
	favs := make([]meshcore.Contact, 0, len(contacts))
	rest := make([]meshcore.Contact, 0, len(contacts))
	for _, c := range contacts {
		if g.mcFavorites[c.PubKey] {
			favs = append(favs, c)
		} else {
			rest = append(rest, c)
		}
	}
	copy(contacts, append(favs, rest...))
}

// mcIsFavorite returns whether a pubkey is on the favourites
// shortlist. Safe from any goroutine.
func (g *GUI) mcIsFavorite(pub meshcore.PubKey) bool {
	g.mcMu.Lock()
	defer g.mcMu.Unlock()
	return g.mcFavorites[pub]
}

// mcToggleFavorite flips the favourite flag for a contact, persists
// the change to the bbolt store, re-sorts the sidebar (so the
// contact jumps to / from the favourites partition at the top),
// and refreshes the list rendering.
func (g *GUI) mcToggleFavorite(pub meshcore.PubKey) {
	g.mcMu.Lock()
	if g.mcFavorites == nil {
		g.mcFavorites = map[meshcore.PubKey]bool{}
	}
	on := !g.mcFavorites[pub]
	if on {
		g.mcFavorites[pub] = true
	} else {
		delete(g.mcFavorites, pub)
	}
	store := g.mcStore
	g.sortMcContactsLocked(g.mcContacts, g.mcContactsSortMode())
	g.mcMu.Unlock()
	if store != nil {
		if err := store.SetFavorite(pub, on); err != nil {
			g.mcAppendSystem("favorite save failed: " + err.Error())
		}
	}
	g.mcRefreshLists()
}

// distanceMiles is a great-circle distance via the haversine
// formula. Returns (miles, true) when both endpoints have a
// non-zero broadcast position; (0, false) when the contact's
// position is unset so the caller can sink it to the bottom of
// the sort.
func distanceMiles(lat1, lon1, lat2, lon2 float64, contactLatE6, contactLonE6 int32) (float64, bool) {
	if contactLatE6 == 0 && contactLonE6 == 0 {
		return 0, false
	}
	const earthRadiusMiles = 3958.8
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMiles * c, true
}

// showMcContactContextMenu opens a right-click popup next to a
// contact row in the sidebar. Items: Info (display the contact
// record) and Remove (call CmdRemoveContact then refresh the
// roster).
func (g *GUI) showMcContactContextMenu(visibleIdx int, absPos fyne.Position) {
	g.mcMu.Lock()
	if visibleIdx < 0 || visibleIdx >= len(g.mcContacts) {
		g.mcMu.Unlock()
		return
	}
	ct := g.mcContacts[visibleIdx]
	g.mcMu.Unlock()
	canvas := g.window.Canvas()
	favLabel := "Favorite"
	if g.mcIsFavorite(ct.PubKey) {
		favLabel = "Unfavorite"
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem(favLabel, func() { g.mcToggleFavorite(ct.PubKey) }),
		fyne.NewMenuItem("Info", func() { g.showMcContactInfoDialog(ct) }),
		fyne.NewMenuItem("Reset path", func() { g.confirmResetMcPath(ct) }),
		fyne.NewMenuItem("Remove", func() { g.confirmRemoveMcContact(ct) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, canvas, absPos)
}

// confirmResetMcPath asks before issuing CmdResetPath. The
// confirmation matters because the operator is asking the radio to
// throw away its learned next-hop sequence — the next DM will
// re-discover via FLOOD, which is slower and noisier on the
// mesh. Useful when the cached path has gone stale (DMs to a
// reachable contact stop arriving while channel sends still work)
// and the operator wants to force re-routing.
func (g *GUI) confirmResetMcPath(ct meshcore.Contact) {
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	dialog.ShowConfirm("Reset path?",
		fmt.Sprintf("Forget the cached route to %s? The next DM will re-discover via FLOOD.", display),
		func(ok bool) {
			if !ok {
				return
			}
			g.mcMu.Lock()
			client := g.mcClient
			g.mcMu.Unlock()
			if client == nil {
				g.mcAppendSystem("!not connected")
				return
			}
			// Clear the auto-reset counter so the next failed DM
			// starts a fresh count rather than potentially
			// triggering an immediate second reset.
			g.mu.Lock()
			delete(g.mcConsecFails, ct.PubKey)
			g.mu.Unlock()
			go func() {
				if err := client.ResetPath(ct.PubKey); err != nil {
					g.mcAppendSystem("reset path failed: " + err.Error())
					return
				}
				g.mcAppendSystem(fmt.Sprintf("path reset for %s — next DM will FLOOD", display))
			}()
		}, g.window)
}

// showMcMapNodeContextMenu opens a popup at absPos for the MeshCore
// node identified by pub. Looks up the matching contact in the
// roster and offers Info / Open chat / Show path on map. No-op if
// the pubkey doesn't resolve (e.g. the contact was deleted between
// hit-test and click).
func (g *GUI) showMcMapNodeContextMenu(pub meshcore.PubKey, absPos fyne.Position) {
	g.mcMu.Lock()
	var ct meshcore.Contact
	found := false
	for i := range g.mcContacts {
		if g.mcContacts[i].PubKey == pub {
			ct = g.mcContacts[i]
			found = true
			break
		}
	}
	g.mcMu.Unlock()
	if !found {
		return
	}
	favLabel := "Favorite"
	if g.mcIsFavorite(pub) {
		favLabel = "Unfavorite"
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem(favLabel, func() { g.mcToggleFavorite(pub) }),
		fyne.NewMenuItem("Info", func() { g.showMcContactInfoDialog(ct) }),
		fyne.NewMenuItem("Open chat", func() {
			g.mcSwitchThread(mcContactThreadID(ct))
			if g.mcChannelsList != nil {
				g.mcChannelsList.UnselectAll()
			}
			g.mcMu.Lock()
			selectIdx := -1
			for i := range g.mcContacts {
				if g.mcContacts[i].PubKey == pub {
					selectIdx = i
					break
				}
			}
			g.mcMu.Unlock()
			if selectIdx >= 0 && g.mcContactsList != nil {
				g.mcContactsList.Select(selectIdx)
			}
		}),
		fyne.NewMenuItem("Show last path", func() {
			pkt, ok := g.findMcRxLogPacketForContact(ct)
			if !ok {
				g.mcAppendSystem("no captured path for this node yet — wait for a message or advert")
				return
			}
			g.mcMu.Lock()
			nodes := g.buildPathFromPacketLocked(pkt)
			g.mcMu.Unlock()
			mw := g.scopeMapWidget()
			if mw == nil || len(nodes) < 2 {
				return
			}
			mw.AppendMessagePath(nodes)
		}),
		fyne.NewMenuItem("Reset path", func() { g.confirmResetMcPath(ct) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), absPos)
}

// mcAttachHashLinks rebuilds the inline-flow container from text,
// returning true when at least one inline element (path-hash link
// OR @-mention) was attached. Plain runs render as canvas.Text in
// plainColor + plainStyle so they match the swapped-out msg
// widget; path-hash tokens become mcHashLink instances; mentions
// render as bold styled text (Slack-blue for others, amber for
// the operator's own name). Returns false (with the container
// untouched) when nothing notable, so the caller falls through to
// the legacy single-text path.
func (g *GUI) mcAttachHashLinks(msgSegments *fyne.Container, text string, plainColor color.Color, plainStyle fyne.TextStyle, contacts []meshcore.Contact) bool {
	g.mcMu.Lock()
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()
	segs := mcParseChatSegments(text, contacts, selfName)
	if len(segs) == 0 {
		return false
	}
	msgSegments.RemoveAll()
	for _, s := range segs {
		switch {
		case s.url != "":
			href := s.url
			link := newMcHashLink(s.text,
				func() { g.openExternalURL(href) },
				func(pos fyne.Position) { g.showMcURLContextMenu(href, pos) },
			)
			msgSegments.Add(link)
		case s.link:
			pub := s.pub
			link := newMcHashLink(s.text,
				func() { g.mcFlyToPubKey(pub) },
				func(pos fyne.Position) { g.showMcMapNodeContextMenu(pub, pos) },
			)
			msgSegments.Add(link)
		case s.mention:
			t := canvas.NewText(s.text, mcMentionColor(s.mentionSelf))
			t.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
			t.TextSize = 10
			msgSegments.Add(t)
		default:
			t := canvas.NewText(s.text, plainColor)
			t.TextStyle = plainStyle
			t.TextSize = 10
			msgSegments.Add(t)
		}
	}
	// Force a layout pass so the inline-flow children's MinSize
	// propagates up to the textRow HBox parent. Without this,
	// dynamically-added canvas.Text + mcHashLink children sometimes
	// render at zero size on first paint (the parent HBox cached
	// the previous MinSize when msgSegments was empty).
	msgSegments.Refresh()
	return true
}

// mcMentionColor returns the rendering colour for an @-mention
// segment. Mentions of the operator's own name use a brighter
// amber/orange (Slack "@you" style) so directed call-outs in busy
// channels jump out; mentions of anyone else use a subdued cyan-
// blue closer to Slack's neutral mention colour.
func mcMentionColor(self bool) color.RGBA {
	if self {
		return color.RGBA{255, 195, 80, 255} // warm amber: "you got pinged"
	}
	return color.RGBA{120, 180, 245, 255} // cool blue: "someone pinged someone"
}

// mcAtCompleter returns contact-name candidates whose normalised
// names start with prefix (case-insensitive), sorted alphabetically
// for stable Tab cycling. Empty prefix returns every contact —
// pressing @<Tab> with nothing typed cycles the whole roster.
// Returns nil outside MeshCore mode so FT8 chat keeps Tab as the
// default focus-shift key.
func (g *GUI) mcAtCompleter(prefix string) []string {
	g.mu.Lock()
	if g.activeMode != "meshcore" {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	g.mcMu.Lock()
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	g.mcMu.Unlock()
	if len(contacts) == 0 {
		return nil
	}
	p := strings.ToLower(prefix)
	out := make([]string, 0, len(contacts))
	for _, c := range contacts {
		name := c.AdvName
		if name == "" {
			continue
		}
		if p == "" || strings.HasPrefix(strings.ToLower(name), p) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// openExternalURL hands a URL to the OS's default browser via
// Fyne's app.OpenURL. Surfaces a system line on parse / launch
// failure so the operator knows the click went somewhere even
// when nothing visible happens.
func (g *GUI) openExternalURL(rawURL string) {
	if g.app == nil {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		g.mcAppendSystem("invalid URL: " + err.Error())
		return
	}
	if err := g.app.OpenURL(u); err != nil {
		g.mcAppendSystem("could not open URL: " + err.Error())
	}
}

// showMcURLContextMenu pops up a right-click menu on a URL link
// offering Open and Copy. Open mirrors the left-click behaviour
// (handy when the operator's mouse hand is already on the right
// button); Copy puts the raw URL on the clipboard so the operator
// can paste it elsewhere without mousing across the whole link.
func (g *GUI) showMcURLContextMenu(rawURL string, absPos fyne.Position) {
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Open in browser", func() { g.openExternalURL(rawURL) }),
		fyne.NewMenuItem("Copy URL", func() {
			if g.window != nil {
				g.window.Clipboard().SetContent(rawURL)
			}
		}),
	)
	widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), absPos)
}

// mcFlyToPubKey centres the map on the contact's broadcast position.
// Surfaces a system line (without changing the map) when the
// pubkey doesn't match any contact or when the contact hasn't
// broadcast a position yet — both common: path hashes routinely
// reference relays the operator hasn't met directly, and many
// chat-only contacts never share lat/lon.
func (g *GUI) mcFlyToPubKey(pub meshcore.PubKey) {
	g.mcMu.Lock()
	var ct meshcore.Contact
	found := false
	for i := range g.mcContacts {
		if g.mcContacts[i].PubKey == pub {
			ct = g.mcContacts[i]
			found = true
			break
		}
	}
	g.mcMu.Unlock()
	if !found {
		g.mcAppendSystem(fmt.Sprintf("path hash %02x: contact unknown", pub[0]))
		return
	}
	if ct.AdvLatE6 == 0 && ct.AdvLonE6 == 0 {
		display := ct.AdvName
		if display == "" {
			display = fmt.Sprintf("%02x", pub[0])
		}
		g.mcAppendSystem(fmt.Sprintf("%s: no broadcast position yet", display))
		return
	}
	mw := g.scopeMapWidget()
	if mw == nil {
		return
	}
	mw.FlyTo(ct.LatDegrees(), ct.LonDegrees())
}

// findMcRxLogPacketForContact returns the most recent RxLog packet
// that correlates to a chat row received from the given contact.
// We can't match packets to senders by inspecting the raw packet
// (path bytes are next-hop hashes, not originator IDs), so we go
// the other way: find the contact's most recent inbound chat row,
// then reuse the packet-correlation path used by the chat-row
// right-click handler.
func (g *GUI) findMcRxLogPacketForContact(ct meshcore.Contact) (meshcore.Packet, bool) {
	thread := mcContactThreadID(ct)
	g.mcMu.Lock()
	hist := g.mcThreadHistory[thread]
	var lastInbound *chatRow
	for i := len(hist) - 1; i >= 0; i-- {
		if !hist[i].tx {
			lastInbound = &hist[i]
			break
		}
	}
	g.mcMu.Unlock()
	if lastInbound == nil {
		return meshcore.Packet{}, false
	}
	return g.findMcRxLogPacketForRow(*lastInbound)
}

// showMcContactInfoDialog opens a read-only modal showing the
// contact's pubkey, type, last-heard time, and lat/lon when
// known. Useful when the operator is deciding whether to prune.
func (g *GUI) showMcContactInfoDialog(ct meshcore.Contact) {
	body := strings.Builder{}
	fmt.Fprintf(&body, "Name:        %s\n", ct.AdvName)
	fmt.Fprintf(&body, "Type:        %s\n", ct.Type)
	fmt.Fprintf(&body, "PubKey:      %s\n", hex.EncodeToString(ct.PubKey[:]))
	fmt.Fprintf(&body, "Last advert: %s\n", ct.LastAdvert.Local().Format("2006-01-02 15:04:05"))
	if ct.AdvLatE6 != 0 || ct.AdvLonE6 != 0 {
		fmt.Fprintf(&body, "Location:    %.6f, %.6f\n", ct.LatDegrees(), ct.LonDegrees())
	}
	entry := widget.NewMultiLineEntry()
	entry.SetText(body.String())
	entry.TextStyle = fyne.TextStyle{Monospace: true}
	entry.Wrapping = fyne.TextWrapOff
	d := dialog.NewCustom("Contact info", "Close", container.NewPadded(entry), g.window)
	d.Resize(fyne.NewSize(620, 240))
	d.Show()
}

// confirmRemoveMcContact prompts before issuing CmdRemoveContact —
// useful when the operator is pruning the roster to free space
// (the firmware caps contacts at MAX_CONTACTS, typically a few
// hundred per board). On success refreshes the local cache and
// the map overlay.
func (g *GUI) confirmRemoveMcContact(ct meshcore.Contact) {
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	dialog.ShowConfirm("Remove contact?",
		fmt.Sprintf("Delete %s from the radio's contact table?", display),
		func(ok bool) {
			if !ok {
				return
			}
			g.mcMu.Lock()
			client := g.mcClient
			g.mcMu.Unlock()
			if client == nil {
				g.mcAppendSystem("!not connected")
				return
			}
			go func() {
				if err := client.RemoveContact(ct.PubKey); err != nil {
					g.mcAppendSystem("remove contact failed: " + err.Error())
					return
				}
				// Drop from local roster + refresh derivative
				// surfaces (sidebar list, map overlay).
				g.mcMu.Lock()
				kept := g.mcContacts[:0]
				for _, c := range g.mcContacts {
					if c.PubKey != ct.PubKey {
						kept = append(kept, c)
					}
				}
				g.mcContacts = kept
				g.mcMu.Unlock()
				g.mcRefreshLists()
				g.mcSyncContactsToMap()
				g.mcAppendSystem(fmt.Sprintf("removed contact %s", display))
			}()
		}, g.window)
}

// showMcBulkDeleteDialog opens a checklist of every contact in the
// roster so the operator can prune many at once. Helpful when the
// firmware's MAX_CONTACTS limit is approaching and a single
// right-click-Remove per stale node would take forever. Three
// quick-pick buttons select stale subsets (>7d, >30d, never-heard);
// individual checkboxes let the operator tune the selection. Hit
// Remove to issue CmdRemoveContact in sequence; failures are
// surfaced via mcAppendSystem and don't stop the loop.
func (g *GUI) showMcBulkDeleteDialog() {
	g.mcMu.Lock()
	client := g.mcClient
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("!not connected")
		return
	}
	if len(contacts) == 0 {
		g.mcAppendSystem("contact roster is empty — nothing to prune")
		return
	}
	// Sort oldest-first so the worst stale entries are at the top
	// of the list — operator can scroll a short distance and
	// approve a chunk.
	sort.SliceStable(contacts, func(i, j int) bool {
		return contacts[i].LastAdvert.Before(contacts[j].LastAdvert)
	})
	checks := make([]*widget.Check, len(contacts))
	rows := make([]fyne.CanvasObject, 0, len(contacts))
	now := time.Now()
	for i, c := range contacts {
		name := c.AdvName
		if name == "" {
			name = fmt.Sprintf("(unnamed %x)", c.PubKey[:4])
		}
		age := mcContactAgeLabel(now, c.LastAdvert)
		label := fmt.Sprintf("%-22s  %s  ·  %s", name, c.Type, age)
		checks[i] = widget.NewCheck(label, nil)
		rows = append(rows, checks[i])
	}
	listBody := container.NewVBox(rows...)
	scroll := container.NewScroll(listBody)
	scroll.SetMinSize(fyne.NewSize(520, 360))
	status := canvas.NewText("0 selected", color.RGBA{160, 165, 175, 255})
	status.TextStyle = fyne.TextStyle{Italic: true}
	status.TextSize = 11
	updateStatus := func() {
		n := 0
		for _, c := range checks {
			if c.Checked {
				n++
			}
		}
		status.Text = fmt.Sprintf("%d selected", n)
		status.Refresh()
	}
	for _, c := range checks {
		c.OnChanged = func(bool) { updateStatus() }
	}
	pickStaleOlderThan := func(d time.Duration) {
		for i, ct := range contacts {
			checks[i].SetChecked(!ct.LastAdvert.IsZero() && now.Sub(ct.LastAdvert) > d)
		}
		updateStatus()
	}
	pickNeverHeard := func() {
		for i, ct := range contacts {
			checks[i].SetChecked(ct.LastAdvert.IsZero())
		}
		updateStatus()
	}
	pickBrokenTimestamps := func() {
		// Either bucket the "broken" tag in mcContactAgeLabel
		// catches: future timestamps (clock-set-wrong nodes) and
		// pre-RTC-sync timestamps (more than 5 years old).
		for i, ct := range contacts {
			if ct.LastAdvert.IsZero() {
				checks[i].SetChecked(false)
				continue
			}
			delta := now.Sub(ct.LastAdvert)
			checks[i].SetChecked(delta < 0 || delta > 5*365*24*time.Hour)
		}
		updateStatus()
	}
	pickClear := func() {
		for _, c := range checks {
			c.SetChecked(false)
		}
		updateStatus()
	}
	presets := container.NewHBox(
		widget.NewButton("Stale > 7d", func() { pickStaleOlderThan(7 * 24 * time.Hour) }),
		widget.NewButton("Stale > 30d", func() { pickStaleOlderThan(30 * 24 * time.Hour) }),
		widget.NewButton("Never heard", pickNeverHeard),
		widget.NewButton("Broken timestamps", pickBrokenTimestamps),
		widget.NewButton("Clear", pickClear),
	)
	body := container.NewBorder(presets, status, nil, nil, scroll)
	dialog.ShowCustomConfirm("Bulk remove contacts", "Remove", "Cancel", body,
		func(ok bool) {
			if !ok {
				return
			}
			pruneList := make([]meshcore.Contact, 0, len(contacts))
			for i, c := range checks {
				if c.Checked {
					pruneList = append(pruneList, contacts[i])
				}
			}
			if len(pruneList) == 0 {
				return
			}
			go func() {
				removed := 0
				for _, ct := range pruneList {
					if err := client.RemoveContact(ct.PubKey); err != nil {
						g.mcAppendSystem(fmt.Sprintf("remove %s failed: %s", ct.AdvName, err.Error()))
						continue
					}
					removed++
				}
				// Drop pruned entries from the local cache in
				// one pass instead of re-fetching everything.
				prunedKeys := make(map[meshcore.PubKey]bool, len(pruneList))
				for _, ct := range pruneList {
					prunedKeys[ct.PubKey] = true
				}
				g.mcMu.Lock()
				kept := g.mcContacts[:0]
				for _, c := range g.mcContacts {
					if !prunedKeys[c.PubKey] {
						kept = append(kept, c)
					}
				}
				g.mcContacts = kept
				g.mcMu.Unlock()
				g.mcRefreshLists()
				g.mcSyncContactsToMap()
				g.mcAppendSystem(fmt.Sprintf("bulk remove: %d/%d contacts removed", removed, len(pruneList)))
			}()
		}, g.window)
}

// mcContactAgeLabel renders a contact's LastAdvert timestamp as a
// short human-readable age, with explicit "broken" labels for the
// two failure modes operators see in the wild:
//   - LastAdvert is zero → "never heard" (firmware never received an advert)
//   - LastAdvert is in the future → "future timestamp (broken)"
//   - LastAdvert is more than 5 years in the past → "ancient (broken)"
//     (almost always a node booted with no RTC sync, broadcasting a
//     near-zero unix timestamp; the firmware propagates whatever the
//     advert claimed.)
//
// Either "broken" tag is the operator's signal that the contact is
// junk and a safe bulk-delete candidate.
func mcContactAgeLabel(now, lastAdvert time.Time) string {
	if lastAdvert.IsZero() {
		return "never heard"
	}
	delta := now.Sub(lastAdvert)
	switch {
	case delta < 0:
		return "future timestamp (broken)"
	case delta > 5*365*24*time.Hour:
		return "ancient (broken)"
	default:
		return fmt.Sprintf("%s ago", trimDuration(delta))
	}
}

// trimDuration returns a compact human-readable age string for the
// bulk-delete dialog ("3d", "5h", "12m", "23s") so wide rows stay
// readable without dragging the dialog wider.
func trimDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// removeSelectedMcContact is the keyboard-shortcut entry point —
// fires from a window-level Delete / Backspace handler when the
// MeshCore mode + a contact is selected. No-op when nothing is
// selected so accidentally hitting Delete in the chat input
// doesn't trigger the prompt.
func (g *GUI) removeSelectedMcContact() {
	g.mu.Lock()
	mode := g.activeMode
	thread := g.mcCurrentThread
	g.mu.Unlock()
	if mode != "meshcore" || !strings.HasPrefix(thread, "contact:") {
		return
	}
	g.mcMu.Lock()
	var picked *meshcore.Contact
	for i := range g.mcContacts {
		if mcContactThreadID(g.mcContacts[i]) == thread {
			c := g.mcContacts[i]
			picked = &c
			break
		}
	}
	g.mcMu.Unlock()
	if picked == nil {
		return
	}
	g.confirmRemoveMcContact(*picked)
}

// showMcChannelContextMenu pops up a right-click menu next to a
// channel row in the MeshCore sidebar. Items: Info (display the
// channel record) and Remove (zero out the slot via SetChannel).
// visibleIdx is the row's position in g.mcChannels — looked up at
// menu-firing time so a roster mutation between hover and click
// doesn't reach a stale entry.
func (g *GUI) showMcChannelContextMenu(visibleIdx int, absPos fyne.Position) {
	g.mcMu.Lock()
	if visibleIdx < 0 || visibleIdx >= len(g.mcChannels) {
		g.mcMu.Unlock()
		return
	}
	ch := g.mcChannels[visibleIdx]
	g.mcMu.Unlock()
	canvas := g.window.Canvas()
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Info", func() { g.showMcChannelInfoDialog(ch) }),
		fyne.NewMenuItem("Remove", func() { g.confirmRemoveMcChannel(ch) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, canvas, absPos)
}

// showMcChannelInfoDialog opens a read-only modal showing the
// channel slot's index, name and base64-encoded secret. Lets the
// operator copy the secret to share the channel with another mesh
// participant.
func (g *GUI) showMcChannelInfoDialog(ch meshcore.Channel) {
	body := strings.Builder{}
	fmt.Fprintf(&body, "Index:   %d\n", ch.Index)
	fmt.Fprintf(&body, "Name:    %s\n", ch.Name)
	fmt.Fprintf(&body, "Secret:  %s\n", base64.StdEncoding.EncodeToString(ch.Secret[:]))
	fmt.Fprintf(&body, "         %s\n", hex.EncodeToString(ch.Secret[:]))
	entry := widget.NewMultiLineEntry()
	entry.SetText(body.String())
	entry.TextStyle = fyne.TextStyle{Monospace: true}
	entry.Wrapping = fyne.TextWrapOff
	d := dialog.NewCustom("Channel info", "Close", container.NewPadded(entry), g.window)
	d.Resize(fyne.NewSize(560, 220))
	d.Show()
}

// confirmRemoveMcChannel asks the operator to confirm a channel
// removal then calls SetChannel against an empty name + zero
// secret, which the firmware treats as "clear this slot".
func (g *GUI) confirmRemoveMcChannel(ch meshcore.Channel) {
	label, _ := mcChannelLabel(ch)
	dialog.ShowConfirm("Remove channel?",
		fmt.Sprintf("Clear channel slot %d (%s)?", ch.Index, label),
		func(ok bool) {
			if !ok {
				return
			}
			g.mcMu.Lock()
			client := g.mcClient
			g.mcMu.Unlock()
			if client == nil {
				g.mcAppendSystem("!not connected")
				return
			}
			go func() {
				var zero [16]byte
				if err := client.SetChannel(ch.Index, "", zero); err != nil {
					g.mcAppendSystem("remove channel failed: " + err.Error())
					return
				}
				// Re-pull the channel table so the sidebar drops the slot.
				channels, _ := client.GetChannels(31)
				g.mcMu.Lock()
				g.mcChannels = channels
				g.mcMu.Unlock()
				g.mcRefreshLists()
			}()
		}, g.window)
}

// showMcChatRowContextMenu pops up the right-click menu for a
// MeshCore chat row. Items: Info (timestamp, sender, SNR, delivery
// state + matched packet metadata) and Map Trace (animate the path
// the message took, when the row matches an entry still in the
// RxLog ring).
func (g *GUI) showMcChatRowContextMenu(r chatRow, absPos fyne.Position) {
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Info", func() { g.showMcChatRowInfo(r) }),
		fyne.NewMenuItem("Map Trace", func() { g.showMcChatRowMapTrace(r) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), absPos)
}

// showMcChatRowInfo opens a read-only modal with everything we
// know about a single chat row — the in-memory state we kept on
// chatRow plus, when we can correlate it, the parsed Packet from
// the RxLog ring (route + payload type + hops). Useful for
// debugging "why did this message route through there" without
// scrolling the full RxLog pane.
func (g *GUI) showMcChatRowInfo(r chatRow) {
	body := strings.Builder{}
	fmt.Fprintf(&body, "Time:        %s\n", r.when.Local().Format("2006-01-02 15:04:05"))
	if r.mcSender != "" {
		fmt.Fprintf(&body, "Sender:      %s\n", r.mcSender)
	}
	if r.tx {
		body.WriteString("Direction:   outbound (TX)\n")
	} else {
		body.WriteString("Direction:   inbound (RX)\n")
	}
	if r.snrDB != 0 {
		fmt.Fprintf(&body, "SNR:         %+.1f dB\n", r.snrDB)
	}
	if r.mcAckCRC != 0 {
		fmt.Fprintf(&body, "Ack CRC:     0x%08x\n", r.mcAckCRC)
		fmt.Fprintf(&body, "Delivery:    %s\n", mcDeliveryStateLabel(r.mcDelivery))
	}
	body.WriteString("\nMessage:\n")
	body.WriteString(r.text)
	body.WriteString("\n")
	// Prefer the path snapshot persisted on the row (captured at
	// receive time, reloaded from bbolt across relaunches). Fall
	// back to live RxLog correlation only when the row predates the
	// persisted-path schema or arrived without a matching frame in
	// the ring at append time. Either source surfaces under the
	// same "Captured packet" header so Info reads consistently.
	switch {
	case r.mcPathLen != 0 || len(r.mcPath) > 0:
		pkt := meshcore.Packet{PathLen: r.mcPathLen, Path: r.mcPath}
		body.WriteString("\n--- Captured packet (persisted) ---\n")
		fmt.Fprintf(&body, "Hops:        %d\n", pkt.HopCount())
		if len(pkt.Path) > 0 {
			fmt.Fprintf(&body, "Path bytes:  %x\n", pkt.Path)
		}
	case !r.tx:
		if pkt, ok := g.findMcRxLogPacketForRow(r); ok {
			body.WriteString("\n--- Captured packet (live ring) ---\n")
			fmt.Fprintf(&body, "Route:       %s\n", pkt.RouteType())
			fmt.Fprintf(&body, "Payload:     %s\n", pkt.PayloadType())
			fmt.Fprintf(&body, "Hops:        %d\n", pkt.HopCount())
			if len(pkt.Path) > 0 {
				fmt.Fprintf(&body, "Path bytes:  %x\n", pkt.Path)
			}
		} else {
			body.WriteString("\n(no captured packet — RxLog ring may have aged out and row predates persisted-path schema)\n")
		}
	}
	entry := widget.NewMultiLineEntry()
	entry.SetText(body.String())
	entry.TextStyle = fyne.TextStyle{Monospace: true}
	entry.Wrapping = fyne.TextWrapOff
	d := dialog.NewCustom("Message info", "Close", container.NewPadded(entry), g.window)
	d.Resize(fyne.NewSize(560, 360))
	d.Show()
}

// showMcChatRowMapTrace animates the route a chat-row message took
// across the mesh. Prefers the path snapshot persisted on the row
// (captured at incoming-append time and reloaded from bbolt across
// relaunches) and falls back to live RxLog correlation for rows
// that predate the persisted-path schema or that arrived without a
// matching frame in the ring. No-op with a friendly system line
// when neither source has a path.
func (g *GUI) showMcChatRowMapTrace(r chatRow) {
	var pkt meshcore.Packet
	if r.mcPathLen != 0 || len(r.mcPath) > 0 {
		pkt = meshcore.Packet{PathLen: r.mcPathLen, Path: r.mcPath}
	} else {
		var ok bool
		pkt, ok = g.findMcRxLogPacketForRow(r)
		if !ok {
			g.mcAppendSystem("no captured path for this message — try a more recent one")
			return
		}
	}
	g.mcMu.Lock()
	nodes := g.buildPathFromPacketLocked(pkt)
	g.mcMu.Unlock()
	mw := g.scopeMapWidget()
	if mw == nil || len(nodes) < 2 {
		return
	}
	mw.AppendMessagePath(nodes)
}

// mcCapturePathFromRxLog stamps the row with the route bytes from
// the matching RxLog frame, when one is in the ring. Mutates row
// in place so the caller's subsequent mcAppendRow persists the
// path along with the message text. Silently leaves an empty path
// when no correlation lands; the right-click trace falls back to
// the live RxLog ring for those.
func (g *GUI) mcCapturePathFromRxLog(row *chatRow) {
	if row == nil || row.tx {
		return
	}
	pkt, ok := g.findMcRxLogPacketForRow(*row)
	if !ok {
		return
	}
	row.mcPathLen = pkt.PathLen
	if len(pkt.Path) > 0 {
		row.mcPath = append([]byte(nil), pkt.Path...)
	}
}

// findMcRxLogPacketForRow correlates a chat row with the RxLog
// entry that produced it — same payload type (TXT_MSG / GRP_TXT)
// and timestamp within ±5 s. Returns the closest match. Skips
// outbound TX rows (we never receive our own send via PushLogRxData).
func (g *GUI) findMcRxLogPacketForRow(r chatRow) (meshcore.Packet, bool) {
	if r.tx {
		return meshcore.Packet{}, false
	}
	g.mcMu.Lock()
	defer g.mcMu.Unlock()
	if len(g.mcRxLog) == 0 {
		return meshcore.Packet{}, false
	}
	bestDelta := 5 * time.Second
	bestIdx := -1
	for i := range g.mcRxLog {
		e := g.mcRxLog[i]
		pt := e.packet.PayloadType()
		if pt != meshcore.PayloadTxtMsg && pt != meshcore.PayloadGrpTxt {
			continue
		}
		delta := r.when.Sub(e.when)
		if delta < 0 {
			delta = -delta
		}
		if delta < bestDelta {
			bestDelta = delta
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return meshcore.Packet{}, false
	}
	return g.mcRxLog[bestIdx].packet, true
}

// mcDeliveryStateLabel renders the chatRow.mcDelivery enum into a
// human label for the Info dialog.
func mcDeliveryStateLabel(d byte) string {
	switch d {
	case mcDeliveryPending:
		return "Pending"
	case mcDeliveryDelivered:
		return "Delivered"
	case mcDeliveryFailed:
		return "Failed"
	default:
		return "(none)"
	}
}

// nextEmptyChannelSlot scans the firmware's 32-slot channel table
// for the first index not currently occupied. Returns (idx, true)
// or (0, false) when the table is full. Both add-dialog flows use
// this so they share the same allocation policy + the same
// "channel table full" early-out.
func (g *GUI) nextEmptyChannelSlot() (uint8, bool) {
	g.mcMu.Lock()
	used := make(map[uint8]bool, len(g.mcChannels))
	for _, ch := range g.mcChannels {
		used[ch.Index] = true
	}
	g.mcMu.Unlock()
	for i := uint8(0); i <= 31; i++ {
		if !used[i] {
			return i, true
		}
	}
	return 0, false
}

// addMcChannel is the shared backend for both add-channel dialogs.
// Issues SetChannel against the operator's chosen slot, re-pulls
// the channel table, and posts a system-line confirmation. Runs the
// SetChannel call on a fresh goroutine so the dialog can dismiss
// immediately without blocking the UI thread on radio I/O.
func (g *GUI) addMcChannel(idx uint8, name string, secret [16]byte) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("!not connected")
		return
	}
	go func() {
		if err := client.SetChannel(idx, name, secret); err != nil {
			g.mcAppendSystem("add channel failed: " + err.Error())
			return
		}
		channels, _ := client.GetChannels(31)
		g.mcMu.Lock()
		g.mcChannels = channels
		g.mcMu.Unlock()
		g.mcRefreshLists()
		g.mcAppendSystem(fmt.Sprintf("channel %s added in slot %d", name, idx))
	}()
}

// showMcAddHashtagChannelDialog joins a community hashtag channel —
// the operator types just the name (with or without the leading
// "#") and we derive the 16-byte AES-128 secret as SHA-256(name)[:16].
// This is the same convention the iOS / Flutter MeshCore clients
// use to auto-join channels like #volcano without a separate
// secret-exchange step.
func (g *GUI) showMcAddHashtagChannelDialog() {
	idx, ok := g.nextEmptyChannelSlot()
	if !ok {
		g.mcAppendSystem("!channel table full — remove an unused channel first")
		return
	}
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("!not connected — open a MeshCore device first")
		return
	}
	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("e.g. volcano  (the # is added for you)")
	derivedHint := canvas.NewText("", color.RGBA{120, 200, 240, 255})
	derivedHint.TextStyle = fyne.TextStyle{Italic: true}
	derivedHint.TextSize = 11
	nameEntry.OnChanged = func(s string) {
		full := normaliseHashtagName(s)
		if full == "" {
			derivedHint.Text = ""
		} else {
			secret := meshcore.DeriveHashtagChannelSecret(full)
			derivedHint.Text = fmt.Sprintf("→ %s   secret %s", full, hex.EncodeToString(secret[:]))
		}
		derivedHint.Refresh()
	}
	form := widget.NewForm(
		widget.NewFormItem("Slot", widget.NewLabel(fmt.Sprintf("%d", idx))),
		widget.NewFormItem("Name", nameEntry),
	)
	warning := widget.NewLabel("Treat hashtag channels as broadcast, not private. Anyone with the channel name can derive the same key (SHA-256 of name) and decrypt traffic — there's no per-recipient encryption.")
	warning.Wrapping = fyne.TextWrapWord
	dialog.ShowCustomConfirm("Add hashtag channel", "Join", "Cancel",
		container.NewVBox(form,
			derivedHint,
			widget.NewLabel("Hashtag channels (#volcano, #meshbud, …) derive the channel secret from the name itself. Typing the name is enough to join — every node using that name shares the same key."),
			warning,
		),
		func(ok bool) {
			if !ok {
				return
			}
			full := normaliseHashtagName(nameEntry.Text)
			if full == "" {
				g.mcAppendSystem("!channel name is required")
				return
			}
			secret := meshcore.DeriveHashtagChannelSecret(full)
			g.addMcChannel(idx, full, secret)
		},
		g.window,
	)
	_ = client // keep the connection check above visible to the type checker
}

// showMcAddPrivateChannelDialog joins a private (non-hashtag) channel
// — the operator pastes the 16-byte AES-128 shared secret that the
// channel creator distributed out of band. The secret accepts both
// base64 and hex encodings to match what other MeshCore tools emit.
func (g *GUI) showMcAddPrivateChannelDialog() {
	idx, ok := g.nextEmptyChannelSlot()
	if !ok {
		g.mcAppendSystem("!channel table full — remove an unused channel first")
		return
	}
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("!not connected — open a MeshCore device first")
		return
	}
	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("display name (no leading #)")
	secretEntry := widget.NewEntry()
	secretEntry.SetPlaceHolder("16-byte shared secret (base64 or hex)")
	form := widget.NewForm(
		widget.NewFormItem("Slot", widget.NewLabel(fmt.Sprintf("%d", idx))),
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Secret", secretEntry),
	)
	dialog.ShowCustomConfirm("Add private channel", "Add", "Cancel",
		container.NewVBox(form,
			widget.NewLabel("Private channels are keyed by an arbitrary 16-byte AES-128 secret distributed out of band by the channel creator. Accepts base64 or hex. (For community hashtag channels, use Add Hashtag Channel — the secret comes from the name automatically.)"),
		),
		func(ok bool) {
			if !ok {
				return
			}
			name := strings.TrimSpace(nameEntry.Text)
			secretRaw := strings.TrimSpace(secretEntry.Text)
			if name == "" {
				g.mcAppendSystem("!channel name is required")
				return
			}
			secretBytes, err := base64.StdEncoding.DecodeString(secretRaw)
			if err != nil {
				if hb, herr := hex.DecodeString(strings.ReplaceAll(secretRaw, " ", "")); herr == nil {
					secretBytes = hb
				} else {
					g.mcAppendSystem("!secret must be base64 (or hex) of 16 bytes — " + err.Error())
					return
				}
			}
			if len(secretBytes) != 16 {
				g.mcAppendSystem(fmt.Sprintf("!secret decoded to %d bytes, expected 16", len(secretBytes)))
				return
			}
			var secret [16]byte
			copy(secret[:], secretBytes)
			g.addMcChannel(idx, name, secret)
		},
		g.window,
	)
	_ = client
}

// normaliseHashtagName trims whitespace, ensures the name starts
// with exactly one "#", and rejects an empty string. Lets the
// operator type "volcano" or "#volcano" interchangeably and have
// the channel land with the canonical "#volcano" name.
func normaliseHashtagName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for strings.HasPrefix(s, "#") {
		s = s[1:]
	}
	if s == "" {
		return ""
	}
	return "#" + s
}

// buildMeshcoreRoster lazily constructs the column shown to the
// right of the chat in MeshCore mode — header + scrollable list of
// channel senders. Idempotent; returns the cached pane on repeat
// calls so list-data pointers stay stable across mode flips.
func (g *GUI) buildMeshcoreRoster() *fyne.Container {
	if g.mcRosterPane != nil {
		return g.mcRosterPane
	}
	g.mcRosterHdr = canvas.NewText("ROSTER  (0)", color.RGBA{140, 140, 145, 255})
	g.mcRosterHdr.TextSize = 11
	g.mcRosterHdr.TextStyle = fyne.TextStyle{Bold: true}
	g.mcRosterList = widget.NewList(
		func() int { return len(g.mcCurrentRoster()) },
		func() fyne.CanvasObject {
			t := canvas.NewText("", color.RGBA{210, 215, 225, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 12
			return container.NewPadded(t)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			padded := obj.(*fyne.Container)
			t := padded.Objects[0].(*canvas.Text)
			roster := g.mcCurrentRoster()
			if id >= len(roster) {
				return
			}
			t.Text = roster[id]
			t.Refresh()
		},
	)
	bg := canvas.NewRectangle(color.RGBA{36, 38, 43, 255})
	g.mcRosterPane = container.NewStack(
		bg,
		container.NewBorder(container.NewPadded(g.mcRosterHdr), nil, nil, nil, g.mcRosterList),
	)
	return g.mcRosterPane
}

// mcRefreshRoster repaints the roster header count + list when the
// active thread or its message buffer changes, and hides the entire
// roster column on 1:1 contact threads (where "who's been chatty"
// is trivially "you and them" — no useful list). Safe from any
// goroutine; UI mutations are dispatched via fyne.Do.
func (g *GUI) mcRefreshRoster() {
	if g.mcRosterList == nil {
		return
	}
	g.mu.Lock()
	thread := g.mcCurrentThread
	g.mu.Unlock()
	hideRoster := strings.HasPrefix(thread, "contact:")
	count := len(g.mcCurrentRoster())
	fyne.Do(func() {
		if g.usersCol != nil {
			if hideRoster {
				g.usersCol.Hide()
			} else {
				g.usersCol.Show()
			}
		}
		if g.mcRosterHdr != nil {
			g.mcRosterHdr.Text = fmt.Sprintf("ROSTER  (%d)", count)
			g.mcRosterHdr.Refresh()
		}
		g.mcRosterList.Refresh()
	})
}

// chatRowToStored projects a chatRow into the persistence shape,
// translating the in-memory delivery enum into the boolean flags
// the store uses for forward-compatibility.
func chatRowToStored(r chatRow) meshcore.StoredMessage {
	msg := meshcore.StoredMessage{
		When:     r.when,
		Text:     r.text,
		Outgoing: r.tx,
		AckCRC:   r.mcAckCRC,
		SNR:      r.snrDB,
		Sender:   r.mcSender,
		PathLen:  r.mcPathLen,
	}
	if len(r.mcPath) > 0 {
		msg.Path = append([]byte(nil), r.mcPath...)
	}
	switch r.mcDelivery {
	case mcDeliveryDelivered:
		msg.Delivered = true
	case mcDeliveryFailed:
		msg.Failed = true
	}
	return msg
}

// storedToChatRow rebuilds a chatRow from a StoredMessage. The
// addrUs flag is set for incoming contact messages so the chat
// renderer applies the bright-cyan "addressed at us" style — same
// behaviour as messages arriving live.
func storedToChatRow(thread string, m meshcore.StoredMessage) chatRow {
	r := chatRow{
		when:      m.When,
		text:      m.Text,
		tx:        m.Outgoing,
		mcAckCRC:  m.AckCRC,
		snrDB:     m.SNR,
		mc:        true,
		mcSender:  m.Sender,
		mcPathLen: m.PathLen,
	}
	if len(m.Path) > 0 {
		r.mcPath = append([]byte(nil), m.Path...)
	}
	if !m.Outgoing && strings.HasPrefix(thread, "contact:") {
		r.addrUs = true
	}
	if m.AckCRC != 0 {
		switch {
		case m.Delivered:
			r.mcDelivery = mcDeliveryDelivered
		case m.Failed:
			r.mcDelivery = mcDeliveryFailed
		default:
			r.mcDelivery = mcDeliveryPending
		}
	}
	return r
}

// mcPersist writes a chatRow to the store under the given thread.
// No-op if persistence is disabled, the row is a system /
// separator (ephemeral), or the entry has no usable timestamp.
func (g *GUI) mcPersist(thread string, r chatRow) {
	if r.system || r.separator || r.when.IsZero() {
		return
	}
	g.mcMu.Lock()
	store := g.mcStore
	g.mcMu.Unlock()
	if store == nil {
		return
	}
	if err := store.Append(thread, chatRowToStored(r)); err != nil {
		if mcLog := g.mcLog; mcLog != nil {
			mcLog.Warnw("meshcore store append", "thread", thread, "err", err)
		}
	}
}

// mcBumpUnread increments the unread counter for a thread when
// it's not the live view. Called from the receive paths so the
// sidebar shows a badge for inbound messages the operator hasn't
// seen yet. Caller-side g.mu may be held; this method does its
// own locking.
func (g *GUI) mcBumpUnread(thread string) {
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	if live {
		g.mu.Unlock()
		return
	}
	if g.mcUnread == nil {
		g.mcUnread = map[string]int{}
	}
	g.mcUnread[thread]++
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcClearUnread zeros the unread counter and mention flag for a
// thread — called from mcSwitchThread when the operator selects it.
func (g *GUI) mcClearUnread(thread string) {
	g.mu.Lock()
	if g.mcUnread != nil {
		delete(g.mcUnread, thread)
	}
	if g.mcMentioned != nil {
		delete(g.mcMentioned, thread)
	}
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcUnreadCount reads the current unread count for a thread.
// Locked-callers can pass true via g.mu being already held — but
// it's cheap enough to just take the lock.
func (g *GUI) mcUnreadCount(thread string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mcUnread == nil {
		return 0
	}
	return g.mcUnread[thread]
}

// mcIsMentioned returns whether the thread has at least one
// unread @[<selfName>] mention since last read. Stronger signal
// than plain unread — drives the warm-amber sidebar highlight
// reserved for directed call-outs.
func (g *GUI) mcIsMentioned(thread string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mcMentioned[thread]
}

// mcMarkMentioned flips the per-thread mention flag on. Caller is
// responsible for skipping the live-thread case (a mention you're
// already looking at isn't unread). Also clears on mcClearUnread.
func (g *GUI) mcMarkMentioned(thread string) {
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	if live {
		g.mu.Unlock()
		return
	}
	if g.mcMentioned == nil {
		g.mcMentioned = map[string]bool{}
	}
	g.mcMentioned[thread] = true
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcTextMentionsSelf returns true when the message body contains
// an @[<selfName>] mention (case-insensitive). selfName comes from
// the connected SelfInfo.Name; empty self skips matching so
// pre-AppStart messages don't false-positive.
func mcTextMentionsSelf(text, selfName string) bool {
	if selfName == "" || text == "" {
		return false
	}
	for _, m := range mcMentionRe.FindAllStringSubmatch(text, -1) {
		if strings.EqualFold(m[1], selfName) {
			return true
		}
	}
	return false
}

// mcSyncContactsToMap rebuilds the map's MeshCore-node overlay
// from the current contact roster. Filters contacts that haven't
// broadcast a position. Safe from any goroutine; UI-side Refresh
// is dispatched by the map widget.
func (g *GUI) mcSyncContactsToMap() {
	mw := g.scopeMapWidget()
	if mw == nil {
		return
	}
	g.mcMu.Lock()
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	g.mcMu.Unlock()
	nodes := make([]mapview.MeshNode, 0, len(contacts))
	for _, c := range contacts {
		if c.AdvLatE6 == 0 && c.AdvLonE6 == 0 {
			continue
		}
		nodes = append(nodes, mapview.MeshNode{
			Name:   c.AdvName,
			PubKey: [32]byte(c.PubKey),
			Lat:    c.LatDegrees(),
			Lon:    c.LonDegrees(),
			Type:   int(c.Type),
		})
	}
	mw.SetMeshNodes(nodes)
}

// scheduleMcContactsRefresh starts (or restarts) the debounce
// timer. When it fires, doMcContactsRefresh runs against the most
// recent client + lastMod. Safe from any goroutine.
func (g *GUI) scheduleMcContactsRefresh(client *meshcore.Client) {
	g.mcMu.Lock()
	if g.mcContactsRefreshTimer != nil {
		g.mcContactsRefreshTimer.Stop()
	}
	g.mcContactsRefreshTimer = time.AfterFunc(mcContactsRefreshDelay, func() {
		g.doMcContactsRefresh(client)
	})
	g.mcMu.Unlock()
}

// doMcContactsRefresh fetches the contacts that have changed since
// the last refresh and merges them into mcContacts. New contacts
// are appended; existing ones are updated in place. Updates
// mcContactsLastMod so the next refresh is even tighter.
func (g *GUI) doMcContactsRefresh(client *meshcore.Client) {
	g.mcMu.Lock()
	if client == nil || g.mcClient != client {
		// Client was swapped or closed since the timer was
		// scheduled — nothing useful to do with this stale ref.
		g.mcMu.Unlock()
		return
	}
	since := g.mcContactsLastMod
	g.mcMu.Unlock()
	delta, err := client.GetContactsSince(since)
	if err != nil {
		return
	}
	if len(delta) == 0 {
		return
	}
	g.mcMu.Lock()
	byKey := make(map[meshcore.PubKey]int, len(g.mcContacts))
	for i, c := range g.mcContacts {
		byKey[c.PubKey] = i
	}
	for _, c := range delta {
		if idx, ok := byKey[c.PubKey]; ok {
			g.mcContacts[idx] = c
		} else {
			byKey[c.PubKey] = len(g.mcContacts)
			g.mcContacts = append(g.mcContacts, c)
		}
		if c.LastMod.After(g.mcContactsLastMod) {
			g.mcContactsLastMod = c.LastMod
		}
	}
	g.sortMcContactsLocked(g.mcContacts, g.mcContactsSortMode())
	g.mcMu.Unlock()
	g.mcRefreshLists()
	g.mcSyncContactsToMap()
}

// buildMeshcoreRxLog lazily constructs the RxLog viewer pane that
// sits beneath the map in MeshCore mode. Idempotent — returns the
// cached container so repeated mode flips don't rebuild list state.
func (g *GUI) buildMeshcoreRxLog() *fyne.Container {
	if g.mcRxLogPane != nil {
		return g.mcRxLogPane
	}
	g.mcRxLogHeader = canvas.NewText("RX LOG  (0)", color.RGBA{140, 140, 145, 255})
	g.mcRxLogHeader.TextSize = 11
	g.mcRxLogHeader.TextStyle = fyne.TextStyle{Bold: true}

	g.mcRxLogList = widget.NewList(
		func() int {
			g.mcMu.Lock()
			defer g.mcMu.Unlock()
			return len(g.mcRxLog)
		},
		func() fyne.CanvasObject {
			t := canvas.NewText("", color.RGBA{200, 205, 215, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 11
			// hoverTip surfaces the row's full local datetime on
			// hover — the visible "15:04:05" prefix drops the
			// date, which matters when scrolling back through
			// hours / days of traffic.
			tip := newHoverTip(container.NewPadded(t), "")
			row := newHoverRow(tip)
			row.onTap = func() { g.mcRxLogList.Select(row.listIdx) }
			row.onSecondary = func(absPos fyne.Position) {
				g.showRxLogContextMenu(row.listIdx, absPos)
			}
			return row
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*hoverRow)
			tip := row.inner.(*hoverTip)
			padded := tip.inner.(*fyne.Container)
			t := padded.Objects[0].(*canvas.Text)
			g.mcMu.Lock()
			if id >= len(g.mcRxLog) {
				g.mcMu.Unlock()
				return
			}
			// Newest at BOTTOM (chronological, like a chat) so
			// the operator can read the log top-down without
			// re-anchoring to the latest line. Row id maps
			// directly to slice index — autoscroll-on-append
			// keeps the most-recent line in view.
			e := g.mcRxLog[id]
			g.mcMu.Unlock()
			t.Text = fmt.Sprintf("%s  %-8s %-9s  %dh  SNR %4.1f  RSSI %4d",
				e.when.Format("15:04:05"), e.route, e.payload, e.hops, e.snr, e.rssi)
			t.Refresh()
			tip.SetTooltip(formatHoverTime(e.when))
			// Stash the row index so the secondary-tap handler
			// can fish out the entry without the closure
			// capturing a stale value.
			row.listIdx = id
		},
	)
	g.mcRxLogList.OnSelected = func(id widget.ListItemID) {
		g.showRxLogInspectByIdx(id)
		g.mcRxLogList.UnselectAll()
	}

	bg := canvas.NewRectangle(color.RGBA{30, 32, 38, 255})
	g.mcRxLogPane = container.NewStack(
		bg,
		container.NewBorder(
			container.NewPadded(g.mcRxLogHeader),
			nil, nil, nil,
			g.mcRxLogList,
		),
	)
	return g.mcRxLogPane
}

// showRxLogContextMenu pops up a right-click menu on a row in the
// RxLog viewer. Inspect opens the parsed-metadata + hex-dump
// modal; Show path on map plots the route the packet traversed
// using contact-roster lookups for each path-hash hop. Clear path
// removes the most recent path overlay so the operator can de-clutter
// without flipping modes.
func (g *GUI) showRxLogContextMenu(visibleIdx int, absPos fyne.Position) {
	g.mcMu.Lock()
	if visibleIdx < 0 || visibleIdx >= len(g.mcRxLog) {
		g.mcMu.Unlock()
		return
	}
	g.mcMu.Unlock()
	canvas := g.window.Canvas()
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Inspect", func() { g.showRxLogInspectByIdx(visibleIdx) }),
		fyne.NewMenuItem("Show path on map", func() { g.mcShowPathForRxLog(visibleIdx) }),
		fyne.NewMenuItem("Clear path", func() {
			if mw := g.scopeMapWidget(); mw != nil {
				mw.ClearMessagePath()
			}
		}),
	)
	widget.ShowPopUpMenuAtPosition(menu, canvas, absPos)
}

// mcShowPathForRxLog draws the route the indexed RxLog packet took
// through the mesh, on the map. Each path hash in the packet's
// path field maps to a contact via pubkey-prefix match (firmware:
// Identity::copyHashTo just copies the first PATH_HASH_SIZE bytes
// of pubkey). Hops we don't know about become placeholder dots
// interpolated between the nearest known endpoints; our own
// position closes the chain at the receiver end.
func (g *GUI) mcShowPathForRxLog(visibleIdx int) {
	g.mcMu.Lock()
	if visibleIdx < 0 || visibleIdx >= len(g.mcRxLog) {
		g.mcMu.Unlock()
		return
	}
	entry := g.mcRxLog[visibleIdx]
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	g.mcMu.Unlock()

	pkt := entry.packet
	hashSize := int(pkt.PathLen>>6) + 1
	hashCount := int(pkt.PathLen & 0x3F)
	if pkt.PathLen == 0xFF {
		hashCount = 0 // direct, no path
	}
	nodes := make([]mapview.MessagePathNode, 0, hashCount+1)
	// Walk the path in transmit order — each hash is the prefix
	// of a forwarder's pubkey.
	for h := 0; h < hashCount && h*hashSize+hashSize <= len(pkt.Path); h++ {
		hashBytes := pkt.Path[h*hashSize : h*hashSize+hashSize]
		var matched *meshcore.Contact
		for i := range contacts {
			if matchPathHash(contacts[i].PubKey[:], hashBytes) {
				c := contacts[i]
				matched = &c
				break
			}
		}
		if matched != nil && (matched.AdvLatE6 != 0 || matched.AdvLonE6 != 0) {
			nodes = append(nodes, mapview.MessagePathNode{
				Name: matched.AdvName,
				Lat:  matched.LatDegrees(),
				Lon:  matched.LonDegrees(),
			})
		} else {
			// Unknown hop or contact with no advertised
			// position. Mark as placeholder; the position is
			// filled in below by interpolation against known
			// endpoints.
			nodes = append(nodes, mapview.MessagePathNode{
				Name:        fmt.Sprintf("%x?", hashBytes),
				Placeholder: true,
			})
		}
	}
	// Close the chain with the operator's own position when
	// we know it. Without it the polyline can't reach the
	// receiver end; without contacts at all the operator just
	// sees nothing on the map.
	if selfLat != 0 || selfLon != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: g.mcSelfInfo.Name,
			Lat:  selfLat,
			Lon:  selfLon,
		})
	}
	mcInterpolatePathPlaceholders(nodes)
	mw := g.scopeMapWidget()
	if mw == nil {
		return
	}
	mw.SetMessagePath(nodes)
}

// mcAnimateIncomingChannel finds the most recent packet in mcRxLog
// that looks like the channel message we just decoded and fires
// its path animation. Correlation is fuzzy: we look back ~5 s for
// a GRP_TXT or TXT_MSG packet, picking the newest. False matches
// are visually harmless — the path animation is informational, not
// authoritative. The mesh-state lock is released before calling
// into the map widget so a slow UI Refresh can't stall the
// receive goroutine that's holding mesh-state critical data.
func (g *GUI) mcAnimateIncomingChannel(_ int8) {
	g.mcMu.Lock()
	if len(g.mcRxLog) == 0 {
		g.mcMu.Unlock()
		return
	}
	cutoff := time.Now().Add(-5 * time.Second)
	var pkt meshcore.Packet
	found := false
	for i := len(g.mcRxLog) - 1; i >= 0; i-- {
		e := g.mcRxLog[i]
		if e.when.Before(cutoff) {
			break
		}
		pt := e.packet.PayloadType()
		if pt == meshcore.PayloadGrpTxt || pt == meshcore.PayloadTxtMsg {
			pkt = e.packet
			found = true
			break
		}
	}
	if !found {
		g.mcMu.Unlock()
		return
	}
	nodes := g.buildPathFromPacketLocked(pkt)
	g.mcMu.Unlock()
	mw := g.scopeMapWidget()
	if mw == nil || len(nodes) < 2 {
		return
	}
	mw.AppendMessagePath(nodes)
}

// mcAnimateOutgoingContact draws a single path from our advertised
// location to the destination contact. No reveal-of-hops — the
// outbound route is unknown — but the lightning fade still gives
// the operator a confirmation that traffic flew.
func (g *GUI) mcAnimateOutgoingContact(ct meshcore.Contact) {
	mw := g.scopeMapWidget()
	if mw == nil {
		return
	}
	g.mcMu.Lock()
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()
	if (selfLat == 0 && selfLon == 0) || (ct.AdvLatE6 == 0 && ct.AdvLonE6 == 0) {
		return
	}
	mw.AppendMessagePath([]mapview.MessagePathNode{
		{Name: selfName, Lat: selfLat, Lon: selfLon},
		{Name: ct.AdvName, Lat: ct.LatDegrees(), Lon: ct.LonDegrees()},
	})
}

// mcAnimateOutgoingChannel fires one path-fade per known channel
// roster member, fanning outward from our location. Captures the
// "broadcast went here" intuition without claiming knowledge of the
// actual on-air route.
func (g *GUI) mcAnimateOutgoingChannel(roster []string) {
	mw := g.scopeMapWidget()
	if mw == nil || len(roster) == 0 {
		return
	}
	g.mcMu.Lock()
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	selfName := g.mcSelfInfo.Name
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	g.mcMu.Unlock()
	if selfLat == 0 && selfLon == 0 {
		return
	}
	byName := make(map[string]meshcore.Contact, len(contacts))
	for _, c := range contacts {
		byName[c.AdvName] = c
	}
	batch := make([][]mapview.MessagePathNode, 0, len(roster))
	for _, name := range roster {
		if name == selfName {
			continue
		}
		c, ok := byName[name]
		if !ok || (c.AdvLatE6 == 0 && c.AdvLonE6 == 0) {
			continue
		}
		batch = append(batch, []mapview.MessagePathNode{
			{Name: selfName, Lat: selfLat, Lon: selfLon},
			{Name: c.AdvName, Lat: c.LatDegrees(), Lon: c.LonDegrees()},
		})
	}
	if len(batch) > 0 {
		mw.AppendMessagePaths(batch)
	}
}

// buildPathFromPacketLocked walks a packet's path-hash field and
// returns the resolved sequence of MessagePathNodes (matched
// contacts + interpolated placeholders + our position). Caller
// must hold g.mcMu — used by the auto-animate paths to avoid
// re-acquiring the lock.
func (g *GUI) buildPathFromPacketLocked(pkt meshcore.Packet) []mapview.MessagePathNode {
	hashSize := int(pkt.PathLen>>6) + 1
	hashCount := int(pkt.PathLen & 0x3F)
	if pkt.PathLen == 0xFF {
		hashCount = 0
	}
	nodes := make([]mapview.MessagePathNode, 0, hashCount+1)
	for h := 0; h < hashCount && h*hashSize+hashSize <= len(pkt.Path); h++ {
		hashBytes := pkt.Path[h*hashSize : h*hashSize+hashSize]
		var matched *meshcore.Contact
		for i := range g.mcContacts {
			if matchPathHash(g.mcContacts[i].PubKey[:], hashBytes) {
				matched = &g.mcContacts[i]
				break
			}
		}
		if matched != nil && (matched.AdvLatE6 != 0 || matched.AdvLonE6 != 0) {
			nodes = append(nodes, mapview.MessagePathNode{
				Name: matched.AdvName,
				Lat:  matched.LatDegrees(),
				Lon:  matched.LonDegrees(),
			})
		} else {
			nodes = append(nodes, mapview.MessagePathNode{
				Name:        fmt.Sprintf("%x?", hashBytes),
				Placeholder: true,
			})
		}
	}
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	if selfLat != 0 || selfLon != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: g.mcSelfInfo.Name,
			Lat:  selfLat,
			Lon:  selfLon,
		})
	}
	mcInterpolatePathPlaceholders(nodes)
	return nodes
}

// matchPathHash returns true when the leading bytes of pubKey
// match hash. PATH_HASH_SIZE is firmware-side fixed (default 1).
func matchPathHash(pubKey, hash []byte) bool {
	if len(hash) > len(pubKey) {
		return false
	}
	for i := 0; i < len(hash); i++ {
		if pubKey[i] != hash[i] {
			return false
		}
	}
	return true
}

// mcInterpolatePathPlaceholders fills in lat/lon for placeholder
// nodes by linear-interpolating between the nearest known endpoints
// on either side. Lets unknown hops still appear on the map between
// the contacts we DO know rather than collapsing to (0, 0).
func mcInterpolatePathPlaceholders(nodes []mapview.MessagePathNode) {
	for i := range nodes {
		if !nodes[i].Placeholder {
			continue
		}
		// Find nearest known anchor before i.
		left := -1
		for j := i - 1; j >= 0; j-- {
			if !nodes[j].Placeholder && (nodes[j].Lat != 0 || nodes[j].Lon != 0) {
				left = j
				break
			}
		}
		right := -1
		for j := i + 1; j < len(nodes); j++ {
			if !nodes[j].Placeholder && (nodes[j].Lat != 0 || nodes[j].Lon != 0) {
				right = j
				break
			}
		}
		switch {
		case left >= 0 && right >= 0:
			frac := float64(i-left) / float64(right-left)
			nodes[i].Lat = nodes[left].Lat + (nodes[right].Lat-nodes[left].Lat)*frac
			nodes[i].Lon = nodes[left].Lon + (nodes[right].Lon-nodes[left].Lon)*frac
		case left >= 0:
			nodes[i].Lat = nodes[left].Lat
			nodes[i].Lon = nodes[left].Lon
		case right >= 0:
			nodes[i].Lat = nodes[right].Lat
			nodes[i].Lon = nodes[right].Lon
		}
	}
}

// showRxLogInspect opens the inspect modal for the entry under the
// given hoverRow. Wraps showRxLogInspectByIdx so the secondary-tap
// callback doesn't have to look up the index itself.
func (g *GUI) showRxLogInspect(row *hoverRow) {
	if row == nil {
		return
	}
	g.showRxLogInspectByIdx(row.listIdx)
}

// showRxLogInspectByIdx opens a modal showing the parsed metadata
// + a hex dump of the raw packet bytes for the RxLog entry at the
// given visible-list index. Visible index 0 = newest packet, so we
// translate to the underlying slice order before reading.
func (g *GUI) showRxLogInspectByIdx(visibleIdx int) {
	g.mcMu.Lock()
	if visibleIdx < 0 || visibleIdx >= len(g.mcRxLog) {
		g.mcMu.Unlock()
		return
	}
	entry := g.mcRxLog[visibleIdx]
	g.mcMu.Unlock()

	// Build a multi-line monospace dump. Mirrors the web client's
	// RxLogPage detail view: header line + per-payload-type fields
	// + a hex+ASCII dump of the raw bytes for forensic copy/paste.
	var b strings.Builder
	fmt.Fprintf(&b, "Time:        %s\n", entry.when.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Route:       %s\n", entry.route)
	fmt.Fprintf(&b, "Payload:     %s\n", entry.payload)
	fmt.Fprintf(&b, "Hops:        %d\n", entry.hops)
	fmt.Fprintf(&b, "SNR / RSSI:  %.1f dB / %d dBm\n", entry.snr, entry.rssi)
	fmt.Fprintf(&b, "Header:      0x%02x  (route=%d, type=%d, ver=%d)\n",
		entry.packet.Header,
		entry.packet.RouteType(),
		entry.packet.PayloadType(),
		entry.packet.PayloadVersion(),
	)
	if entry.packet.TransportCode1 != 0 || entry.packet.TransportCode2 != 0 {
		fmt.Fprintf(&b, "TransportCodes: %04x %04x\n",
			entry.packet.TransportCode1, entry.packet.TransportCode2)
	}
	fmt.Fprintf(&b, "PathLen byte: 0x%02x  (hashSize=%d, hashCount=%d)\n",
		entry.packet.PathLen,
		int(entry.packet.PathLen>>6)+1,
		int(entry.packet.PathLen&0x3F))
	if len(entry.packet.Path) > 0 {
		fmt.Fprintf(&b, "Path:        %x\n", entry.packet.Path)
	}
	fmt.Fprintf(&b, "Payload len: %d bytes\n", len(entry.packet.Payload))
	fmt.Fprintf(&b, "Raw len:     %d bytes\n\n", len(entry.raw))
	b.WriteString(formatHexDump(entry.raw))

	textArea := widget.NewMultiLineEntry()
	textArea.SetText(b.String())
	textArea.TextStyle = fyne.TextStyle{Monospace: true}
	textArea.Wrapping = fyne.TextWrapOff
	scroller := container.NewScroll(textArea)
	scroller.SetMinSize(fyne.NewSize(560, 360))

	d := dialog.NewCustom("Inspect mesh packet", "Close", scroller, g.window)
	d.Resize(fyne.NewSize(620, 420))
	d.Show()
}

// formatHexDump returns a classic 16-bytes-per-row hex + printable
// ASCII dump of b. Trailing partial rows pad cleanly so the ASCII
// gutter stays aligned.
func formatHexDump(b []byte) string {
	if len(b) == 0 {
		return "(empty)\n"
	}
	var out strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		row := b[off:end]
		fmt.Fprintf(&out, "%04x  ", off)
		for i := 0; i < 16; i++ {
			if i < len(row) {
				fmt.Fprintf(&out, "%02x ", row[i])
			} else {
				out.WriteString("   ")
			}
			if i == 7 {
				out.WriteByte(' ')
			}
		}
		out.WriteString(" |")
		for _, c := range row {
			if c >= 0x20 && c < 0x7F {
				out.WriteByte(c)
			} else {
				out.WriteByte('.')
			}
		}
		out.WriteString("|\n")
	}
	return out.String()
}

// mcAppendRxLogEntry buffers one parsed PushLogRxData event and
// refreshes the RxLog viewer. Caps mcRxLog at maxMcRxLog (newest
// wins). Safe from any goroutine.
func (g *GUI) mcAppendRxLogEntry(ev meshcore.EventRxLog) {
	g.mcMu.Lock()
	g.mcRxLog = append(g.mcRxLog, mcRxLogEntry{
		when:    time.Now(),
		route:   ev.Packet.RouteType().String(),
		payload: ev.Packet.PayloadType().String(),
		hops:    ev.Packet.HopCount(),
		snr:     ev.SNR,
		rssi:    ev.RSSI,
		raw:     ev.Raw,
		packet:  ev.Packet,
	})
	if len(g.mcRxLog) > maxMcRxLog {
		g.mcRxLog = g.mcRxLog[len(g.mcRxLog)-maxMcRxLog:]
	}
	n := len(g.mcRxLog)
	g.mcMu.Unlock()
	if g.mcRxLogList != nil {
		fyne.Do(func() {
			if g.mcRxLogHeader != nil {
				g.mcRxLogHeader.Text = fmt.Sprintf("RX LOG  (%d)", n)
				g.mcRxLogHeader.Refresh()
			}
			g.mcRxLogList.Refresh()
			// Newest at the BOTTOM of the list now (chronological,
			// reads top-down). Scroll-to-bottom keeps the latest
			// arrival in view as the log grows.
			g.mcRxLogList.ScrollToBottom()
		})
	}
}

// mcAppendTrackedTx appends an outbound TX row to the thread, marks
// it Pending, and registers the AckCRC so PushSendConfirmed can flip
// it to Delivered. ackCRC == 0 falls through to a plain mcAppendRow
// (firmware sometimes returns 0 for messages it doesn't track).
// recipient is the destination contact's pubkey for DMs (zero for
// channel sends) — recorded so the per-contact failure counter can
// trigger an auto-reset when consecutive sends to the same
// contact time out.
func (g *GUI) mcAppendTrackedTx(thread string, r chatRow, ackCRC uint32, recipient meshcore.PubKey) {
	if ackCRC == 0 {
		g.mcAppendRow(thread, r)
		return
	}
	r.mcAckCRC = ackCRC
	r.mcSentAt = time.Now()
	r.mcDelivery = mcDeliveryPending
	g.mcAppendRow(thread, r)
	// Look up the just-appended row's index in whichever buffer
	// it landed in (live g.rows when MeshCore mode + matching
	// thread, else the per-thread history). Register that
	// (thread, idx) under the AckCRC for the SendConfirmed
	// handler.
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	idx := -1
	if live {
		idx = len(g.rows) - 1
	} else if hist := g.mcThreadHistory[thread]; len(hist) > 0 {
		idx = len(hist) - 1
	}
	if g.mcPendingByAck == nil {
		g.mcPendingByAck = map[uint32]mcPendingSend{}
	}
	if idx >= 0 {
		g.mcPendingByAck[ackCRC] = mcPendingSend{
			thread:    thread,
			rowIdx:    idx,
			sentAt:    r.mcSentAt,
			recipient: recipient,
		}
	}
	g.mu.Unlock()
}

// mcMarkDelivered finds the pending row for the given AckCRC and
// flips it to Delivered. Called from runMeshcoreEvents on every
// PushSendConfirmed. No-op if the AckCRC is unknown (firmware can
// emit a confirm for a message we sent before launch, or after
// retention dropped the row).
func (g *GUI) mcMarkDelivered(ackCRC uint32) {
	if ackCRC == 0 {
		return
	}
	g.mu.Lock()
	pend, ok := g.mcPendingByAck[ackCRC]
	if ok {
		delete(g.mcPendingByAck, ackCRC)
		// A successful delivery proves the cached path is alive,
		// so the auto-reset counter for this contact resets to
		// zero — only future *consecutive* failures should count.
		var zero meshcore.PubKey
		if pend.recipient != zero && g.mcConsecFails != nil {
			delete(g.mcConsecFails, pend.recipient)
		}
	}
	g.mu.Unlock()
	if !ok {
		return
	}
	g.mcUpdateRowDelivery(pend.thread, pend.rowIdx, mcDeliveryDelivered)
}

// mcUpdateRowDelivery mutates a row's delivery state in whichever
// buffer holds it (live g.rows for the active thread, else the
// per-thread history). Re-persists the row so a Pending → Delivered
// transition isn't lost on relaunch. Refreshes the chat list when
// the change is visible.
func (g *GUI) mcUpdateRowDelivery(thread string, idx int, state byte) {
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	var snapshot chatRow
	updated := false
	if live {
		if idx >= 0 && idx < len(g.rows) {
			g.rows[idx].mcDelivery = state
			snapshot = g.rows[idx]
			updated = true
		}
	} else {
		if hist, ok := g.mcThreadHistory[thread]; ok && idx >= 0 && idx < len(hist) {
			hist[idx].mcDelivery = state
			g.mcThreadHistory[thread] = hist
			snapshot = hist[idx]
			updated = true
		}
	}
	g.mu.Unlock()
	if updated {
		// Same-key write overwrites the original Pending entry
		// in the store (the When key drives the bbolt key).
		g.mcPersist(thread, snapshot)
	}
	if live && updated && g.chatList != nil {
		fyne.Do(func() { g.chatList.Refresh() })
	}
}

// mcSweepPending walks mcPendingByAck and flips any row that's been
// waiting longer than mcPendingTimeout to Failed. Called from the
// 1 Hz status ticker so timeouts appear without an extra goroutine.
// Each Failed DM also bumps a per-recipient counter; once a contact
// hits mcAutoResetThreshold consecutive failures the radio's cached
// out_path for that contact is auto-cleared so the next DM
// re-discovers via FLOOD — saves the operator a manual right-click
// when a once-good route bit-rots.
func (g *GUI) mcSweepPending() {
	now := time.Now()
	g.mu.Lock()
	if len(g.mcPendingByAck) == 0 {
		g.mu.Unlock()
		return
	}
	var zero meshcore.PubKey
	stale := []mcPendingSend{}
	autoReset := []meshcore.PubKey{}
	for ack, pend := range g.mcPendingByAck {
		if now.Sub(pend.sentAt) < mcPendingTimeout {
			continue
		}
		stale = append(stale, pend)
		delete(g.mcPendingByAck, ack)
		if pend.recipient == zero {
			continue // channel send — no per-contact path to reset
		}
		if g.mcConsecFails == nil {
			g.mcConsecFails = map[meshcore.PubKey]int{}
		}
		g.mcConsecFails[pend.recipient]++
		if g.mcConsecFails[pend.recipient] >= mcAutoResetThreshold {
			autoReset = append(autoReset, pend.recipient)
			delete(g.mcConsecFails, pend.recipient)
		}
	}
	client := g.mcClient
	g.mu.Unlock()
	for _, pend := range stale {
		g.mcUpdateRowDelivery(pend.thread, pend.rowIdx, mcDeliveryFailed)
	}
	if client == nil {
		return
	}
	for _, pub := range autoReset {
		// Resolve the contact name for the system line — best-effort,
		// fall back to a hex prefix when the contact has been
		// removed since the send.
		g.mcMu.Lock()
		display := fmt.Sprintf("%x", pub[:6])
		for i := range g.mcContacts {
			if g.mcContacts[i].PubKey == pub && g.mcContacts[i].AdvName != "" {
				display = g.mcContacts[i].AdvName
				break
			}
		}
		g.mcMu.Unlock()
		go func(p meshcore.PubKey, name string) {
			if err := client.ResetPath(p); err != nil {
				g.mcAppendSystem(fmt.Sprintf("auto-reset path for %s failed: %s", name, err.Error()))
				return
			}
			g.mcAppendSystem(fmt.Sprintf("auto-reset stale path for %s after %d failed DMs — next send will FLOOD", name, mcAutoResetThreshold))
		}(pub, display)
	}
}

// mcAppendRow appends a chat row to a thread's buffer. The row goes
// into g.rows (the live chat-list view) only when MeshCore mode is
// active AND the thread matches the operator's current selection;
// otherwise it lands in mcThreadHistory so the conversation is
// preserved without disturbing whatever the operator is currently
// looking at — including the FT8 view if they've flipped modes since
// the last selection.
func (g *GUI) mcAppendRow(thread string, r chatRow) {
	g.mu.Lock()
	if g.mcThreadHistory == nil {
		g.mcThreadHistory = map[string][]chatRow{}
	}
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	if live {
		g.rows = append(g.rows, r)
		g.trimRows()
	} else {
		hist := append(g.mcThreadHistory[thread], r)
		if len(hist) > maxRows {
			hist = hist[len(hist)-maxRows:]
		}
		g.mcThreadHistory[thread] = hist
	}
	g.mu.Unlock()
	g.mcPersist(thread, r)
	if live && g.chatList != nil {
		fyne.Do(func() {
			g.chatList.Refresh()
			if n := len(g.rows); n > 0 {
				g.chatList.ScrollTo(n - 1)
			}
		})
	}
}

// trimRows enforces the maxRows cap on g.rows; callers must hold g.mu.
// Mirrors the inline cap from appendRow without requiring an extra
// row-append round trip.
func (g *GUI) trimRows() {
	if len(g.rows) > maxRows {
		g.rows = g.rows[len(g.rows)-maxRows:]
	}
}

// meshcoreLogger returns the dedicated MeshCore wire-trace logger,
// lazy-opening nocordhf-meshcore.log on first use. Always at Debug
// level (the dedicated file is opt-in by being separate from the
// main log). Returns nil only if the file fails to open, in which
// case callers fall back to the package-level logger.
func (g *GUI) meshcoreLogger() *zap.SugaredLogger {
	g.mcMu.Lock()
	defer g.mcMu.Unlock()
	if g.mcLog != nil {
		return g.mcLog
	}
	l, err := logging.NewFileLogger("nocordhf-meshcore.log", g.buildID, zapcore.DebugLevel)
	if err != nil {
		// Surface the failure once via the main log; subsequent
		// connectMeshcore calls will just see g.mcLog == nil and
		// skip wiring without further noise.
		if logging.L != nil {
			logging.L.Warnw("meshcore log open failed", "err", err)
		}
		return nil
	}
	g.mcLog = l
	return l
}

// connectMeshcore opens the configured MeshCore device, runs AppStart,
// pulls contacts + channels, and spawns the events goroutine. Safe to
// call from any goroutine; idempotent if a client is already open.
// Pure no-op if no device is configured (the operator can still flip
// to MeshCore mode to read the saved-but-empty sidebar).
func (g *GUI) connectMeshcore() {
	g.mcMu.Lock()
	if g.mcClient != nil {
		g.mcMu.Unlock()
		return
	}
	// Any connect attempt — automatic OR triggered by the auto-
	// reconnect timer firing — clears the manual-disconnect flag
	// so a future link drop is again eligible for auto-reconnect.
	g.mcManualDisconnect = false
	if g.mcAutoReconnectTimer != nil {
		g.mcAutoReconnectTimer.Stop()
		g.mcAutoReconnectTimer = nil
	}
	g.mcMu.Unlock()
	if g.app == nil {
		return
	}
	prefs := g.app.Preferences()
	transport := prefs.StringWithFallback(mcPrefTransport, mcTransportUSB)
	advertName := strings.TrimSpace(prefs.String(mcPrefProfileName))
	advertLatF := prefs.FloatWithFallback(mcPrefProfileLat, 0)
	advertLonF := prefs.FloatWithFallback(mcPrefProfileLon, 0)

	// Resolve transport-specific connect parameters up front so the
	// goroutine below has a single Open call without mode branching
	// inside its body.
	type connectPlan struct {
		open  func() (*meshcore.Client, error)
		label string
	}
	var plan connectPlan
	switch transport {
	case mcTransportBLE:
		address := strings.TrimSpace(prefs.String(mcPrefBLEAddress))
		if address == "" {
			g.mcSetStatus("no BLE device — configure in Settings → MeshCore")
			return
		}
		name := prefs.String(mcPrefBLEDeviceName)
		if name == "" {
			name = address
		}
		plan = connectPlan{
			label: "BLE " + name,
			open:  func() (*meshcore.Client, error) { return meshcore.OpenBLE(address, 0) },
		}
	default:
		port := strings.TrimSpace(prefs.String(mcPrefDevicePort))
		if port == "" {
			g.mcSetStatus("no USB device — configure in Settings → MeshCore")
			return
		}
		baud := prefs.IntWithFallback(mcPrefDeviceBaud, meshcore.DefaultBaud)
		plan = connectPlan{
			label: port,
			open:  func() (*meshcore.Client, error) { return meshcore.OpenSerial(port, baud) },
		}
	}
	g.mcSetStatus("connecting on " + plan.label + "…")
	go func() {
		client, err := plan.open()
		if err != nil {
			g.mcSetStatus("open failed: " + err.Error())
			g.mcAppendSystem("open failed: " + err.Error())
			if mcLog := g.meshcoreLogger(); mcLog != nil {
				mcLog.Warnw("open failed", "transport", transport, "label", plan.label, "err", err)
			}
			return
		}
		// Pipe every meshcore tx/rx/push frame into a dedicated
		// nocordhf-meshcore.log so the operator can `tail -f` it
		// without hunting through the main app log. Always at
		// Debug level regardless of -debug — the dedicated file is
		// opt-in by being separate, so we want full traffic when
		// it's active.
		if mcLog := g.meshcoreLogger(); mcLog != nil {
			client.SetLogger(mcLog)
			mcLog.Infow("meshcore connected", "transport", transport, "label", plan.label)
		}
		// Open the persistent message store + load any saved
		// history into mcThreadHistory before the events
		// goroutine starts emitting new rows. Failure is
		// non-fatal — the operator just loses history persistence
		// for this session.
		g.mcMu.Lock()
		hadStore := g.mcStore != nil
		g.mcMu.Unlock()
		if !hadStore {
			if store, err := meshcore.OpenStore("nocordhf-meshcore.db"); err != nil {
				g.mcAppendSystem("history store unavailable: " + err.Error())
			} else {
				g.mcMu.Lock()
				g.mcStore = store
				if favs, ferr := store.LoadFavorites(); ferr == nil {
					g.mcFavorites = favs
				}
				g.mcMu.Unlock()
				if all, err := store.LoadAll(maxRows); err == nil && len(all) > 0 {
					g.mcMu.Lock()
					if g.mcThreadHistory == nil {
						g.mcThreadHistory = map[string][]chatRow{}
					}
					var loaded int
					for thread, msgs := range all {
						rows := make([]chatRow, 0, len(msgs))
						for _, m := range msgs {
							rows = append(rows, storedToChatRow(thread, m))
						}
						g.mcThreadHistory[thread] = rows
						loaded += len(rows)
					}
					g.mcMu.Unlock()
					g.mcAppendSystem(fmt.Sprintf("loaded %d messages from history (%d threads)", loaded, len(all)))
				}
			}
		}
		info, err := client.AppStart("nocordhf")
		if err != nil {
			g.mcSetStatus("AppStart failed: " + err.Error())
			g.mcAppendSystem("AppStart failed: " + err.Error())
			_ = client.Close()
			return
		}
		// Best-effort clock sync — many companion boards have no RTC,
		// so without this the per-message senderTimestamp is
		// nonsense for the first send. Errors are non-fatal.
		_ = client.SetDeviceTime(time.Now())
		// Push the manual-add-contacts toggle if it differs from
		// the radio's current state. Critical on busy meshes where
		// auto-add fills the contacts table to MAX_CONTACTS and
		// the firmware starts thrashing on evictions.
		manualAdd := prefs.BoolWithFallback(mcPrefProfileManualAdd, false)
		currentManual := info.ManualAddContacts != 0
		if manualAdd != currentManual {
			if err := client.SetManualAddContacts(manualAdd); err != nil {
				g.mcAppendSystem("SetManualAddContacts: " + err.Error())
			}
		}
		// Repeaters drop packets whose senderTimestamp is earlier
		// than the most recent timestamp seen from that pubkey
		// (per the MeshCore crypto reference). A long-running
		// session whose RTC drifts ahead of wall-clock would
		// silently start having sends rejected, so re-sync
		// hourly while this client is the live one.
		go g.runMeshcoreClockSync(client)
		// Push saved profile to the radio. Best-effort — if the
		// operator hasn't set a name yet we leave the firmware
		// default in place; lat/lon at exact 0,0 (Null Island) is
		// treated as "unset" for the same reason.
		if advertName != "" && advertName != info.Name {
			_ = client.SetAdvertName(advertName)
		}
		if advertLatF != 0 || advertLonF != 0 {
			latE6, lonE6 := meshcore.LatLonToE6(advertLatF, advertLonF)
			if latE6 != info.AdvLatE6 || lonE6 != info.AdvLonE6 {
				_ = client.SetAdvertLatLon(latE6, lonE6)
			}
		}
		contacts, err := client.GetContacts()
		if err != nil {
			g.mcSetStatus("GetContacts failed: " + err.Error())
			g.mcAppendSystem("GetContacts failed: " + err.Error())
		}
		channels, _ := client.GetChannels(31)
		// Seed the LastMod cursor from the initial contact dump
		// so subsequent advert-driven refreshes only pull deltas.
		var latestMod time.Time
		for _, c := range contacts {
			if c.LastMod.After(latestMod) {
				latestMod = c.LastMod
			}
		}
		g.mcMu.Lock()
		g.mcClient = client
		g.mcSelfInfo = info
		g.mcContacts = contacts
		g.mcContactsLastMod = latestMod
		g.mcChannels = channels
		g.mcStarted = true
		// Sort under the lock so distance mode reads the
		// freshly-assigned mcSelfInfo for its reference position.
		g.sortMcContactsLocked(g.mcContacts, g.mcContactsSortMode())
		g.mcMu.Unlock()
		g.mcRefreshLists()
		g.mcSyncContactsToMap()
		// Fly the map to the operator's broadcast location with a
		// ~50 mi (~80 km) radius so nearby nodes are visible
		// immediately. Falls back to the map's default centre if
		// the operator hasn't set their location yet.
		if mw := g.scopeMapWidget(); mw != nil && (info.AdvLatE6 != 0 || info.AdvLonE6 != 0) {
			lat := float64(info.AdvLatE6) / 1e6
			lon := float64(info.AdvLonE6) / 1e6
			fyne.Do(func() {
				// Pin the diamond to the firmware-reported
				// position (GPS-derived on T1000-E etc.)
				// instead of the FT8-grid centroid.
				mw.SetSelfPosition(lat, lon)
				mw.FlyToRadius(lat, lon, 50)
			})
		}
		g.mcSetStatus(fmt.Sprintf("connected — %s", info.Name))
		g.mcAppendSystem(fmt.Sprintf("connected: %s (%d contacts, %d channels)", info.Name, len(contacts), len(channels)))
		go g.runMeshcoreEvents(client)
	}()
}

// disconnectMeshcore closes the client cleanly. Called from
// Settings → Save (after a device pick change) and window-close.
// Mode-rail flipping leaves the client open so a quick
// FT8 ↔ MeshCore round-trip doesn't tear down the session. The
// persistence store is closed here too so bbolt's file lock is
// released before the next process tries to open it. Sets
// mcManualDisconnect so any pending auto-reconnect timer no-ops
// when it fires — the operator's intent is "stay disconnected".
func (g *GUI) disconnectMeshcore() {
	g.mcMu.Lock()
	c := g.mcClient
	store := g.mcStore
	g.mcClient = nil
	g.mcStore = nil
	g.mcStarted = false
	g.mcManualDisconnect = true
	if g.mcAutoReconnectTimer != nil {
		g.mcAutoReconnectTimer.Stop()
		g.mcAutoReconnectTimer = nil
	}
	g.mcMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	if store != nil {
		_ = store.Close()
	}
}

// scheduleMcAutoReconnect arms a one-shot timer to retry
// connectMeshcore after the operator-configured interval (default
// mcDefaultAutoReconnectMin minutes). 0 disables. macOS sleep/wake
// is the most common failure that drops the BLE link silently;
// without this the operator has to remember to manually re-pick
// the radio in Settings every time the laptop wakes. The interval
// is intentionally NOT zero by default — battery-powered trackers
// like the T1000-E pay a real cost for an aggressive reconnect
// loop, and a 5-minute drift between "I closed the lid" and
// "messages flow again" is acceptable for most use.
func (g *GUI) scheduleMcAutoReconnect() {
	if g.app == nil {
		return
	}
	mins := g.app.Preferences().IntWithFallback(mcPrefAutoReconnectMin, mcDefaultAutoReconnectMin)
	if mins <= 0 {
		return
	}
	g.mcMu.Lock()
	if g.mcManualDisconnect {
		g.mcMu.Unlock()
		return
	}
	if g.mcAutoReconnectTimer != nil {
		g.mcAutoReconnectTimer.Stop()
	}
	delay := time.Duration(mins) * time.Minute
	g.mcAutoReconnectTimer = time.AfterFunc(delay, func() {
		g.mcMu.Lock()
		// Bail if a manual reconnect already happened or the
		// operator manually disconnected since the timer armed.
		if g.mcClient != nil || g.mcManualDisconnect {
			g.mcMu.Unlock()
			return
		}
		g.mcMu.Unlock()
		g.mcAppendSystem(fmt.Sprintf("auto-reconnecting after %d min idle", mins))
		fyne.Do(func() { g.connectMeshcore() })
	})
	g.mcMu.Unlock()
	g.mcAppendSystem(fmt.Sprintf("link dropped — auto-reconnect in %d min (Settings → MeshCore to change)", mins))
}

// runMeshcoreClockSync re-issues SetDeviceTime once an hour while
// the supplied client is still the active one. Long-running
// sessions whose device clock drifts ahead of wall-clock would
// otherwise have sends silently dropped by repeaters that enforce
// monotonic per-pubkey timestamps. Self-exits when the client
// changes (operator reconnect / disconnect) so multiple
// connect-disconnect cycles don't accumulate stray syncers.
func (g *GUI) runMeshcoreClockSync(client *meshcore.Client) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		g.mcMu.Lock()
		current := g.mcClient
		g.mcMu.Unlock()
		if current != client {
			return
		}
		_ = client.SetDeviceTime(time.Now())
	}
}

// runMeshcoreEvents pumps the Client's event channel until it closes.
// Translates push events into chat rows / sidebar updates. Drains
// MsgWaiting via SyncNextMessage in a loop so we keep up with bursty
// inbound traffic.
func (g *GUI) runMeshcoreEvents(client *meshcore.Client) {
	for ev := range client.Events() {
		switch e := ev.(type) {
		case meshcore.EventMsgWaiting:
			for {
				res, err := client.SyncNextMessage()
				if err != nil {
					// Surface decode / transport failures so silent
					// dropped messages aren't a mystery. Don't loop
					// forever on a persistent error — break out.
					g.mcAppendSystem("sync message failed: " + err.Error())
					break
				}
				if res.NoMore {
					break
				}
				// SyncNextMessage already emits ContactMessage /
				// ChannelMessage events for the result, so the
				// matching case branches below handle the chat
				// append. Nothing to do here.
			}
		case meshcore.EventContactMessage:
			g.mcAppendIncomingContact(e.ContactMessage)
		case meshcore.EventChannelMessage:
			g.mcAppendIncomingChannel(e.ChannelMessage)
		case meshcore.EventSendConfirmed:
			// Firmware confirms one of our outbound contact
			// messages reached its destination. Flip the matching
			// chat row from Pending to Delivered.
			g.mcMarkDelivered(e.AckCode)
		case meshcore.EventRxLog:
			// Raw mesh packet decoded off-air. Feed the RxLog
			// viewer; in MeshCore mode the operator can see live
			// traffic + SNR/RSSI without leaving the app.
			g.mcAppendRxLogEntry(e)
		case meshcore.EventContactsFull:
			// Hardware contacts table at MAX_CONTACTS — the
			// firmware will start dropping new adverts AND
			// commands slow dramatically as it churns evictions
			// (observed ~10 s round-trips on a thrashing radio).
			// Rate-limit the system-line spam to once every 5
			// minutes so the warning is visible without burying
			// real chat content.
			g.mcMu.Lock()
			now := time.Now()
			suppress := now.Sub(g.mcLastContactsFullWarn) < 5*time.Minute
			if !suppress {
				g.mcLastContactsFullWarn = now
			}
			g.mcMu.Unlock()
			if !suppress {
				g.mcAppendSystem("contacts storage full — open the contacts menu and choose Bulk delete to prune. Commands may be slow until the table has room.")
			}
		case meshcore.EventContactDeleted:
			// Firmware evicted a contact on its own (oldest-
			// out auto-add). Drop from the local cache so the
			// sidebar + map mirror the radio's state.
			g.mcMu.Lock()
			kept := g.mcContacts[:0]
			for _, c := range g.mcContacts {
				if c.PubKey != e.PublicKey {
					kept = append(kept, c)
				}
			}
			g.mcContacts = kept
			g.mcMu.Unlock()
			g.mcRefreshLists()
			g.mcSyncContactsToMap()
		case meshcore.EventPathUpdated:
			// Firmware learned (or refreshed) a route to this
			// contact. Surface it so the operator can correlate
			// "I just did Reset path" with "the path locked in
			// again" — and so silent path bit-rot recovery is
			// visible after an auto-reset cycle. Triggers a
			// debounced contact refresh so Contact.OutPathLen +
			// OutPath get pulled in fresh.
			g.mcMu.Lock()
			display := fmt.Sprintf("%x", e.PublicKey[:6])
			for i := range g.mcContacts {
				if g.mcContacts[i].PubKey == e.PublicKey && g.mcContacts[i].AdvName != "" {
					display = g.mcContacts[i].AdvName
					break
				}
			}
			g.mcMu.Unlock()
			g.mcAppendSystem("path updated for " + display)
			g.scheduleMcContactsRefresh(client)
		case meshcore.EventAdvert:
			// New / updated advert — schedule a debounced delta
			// refresh. A naive full GetContacts on every advert
			// flooded the radio (one busy mesh dumped 32k contact
			// records over the wire across 113 redundant calls).
			// Coalesce adverts within the debounce window into a
			// single GetContactsSince(lastMod) that returns just
			// the changed records.
			g.scheduleMcContactsRefresh(client)
		case meshcore.EventDisconnected:
			g.mcMu.Lock()
			g.mcClient = nil
			g.mcStarted = false
			g.mcMu.Unlock()
			msg := "disconnected"
			if e.Err != nil {
				msg += ": " + e.Err.Error()
			}
			g.mcSetStatus("disconnected")
			g.mcAppendSystem(msg)
			g.scheduleMcAutoReconnect()
			return
		}
	}
}

// mcAppendIncomingContact converts a received ContactMessage into a
// chat row in that contact's thread. Looks up the sender by 6-byte
// pubkey prefix; an unknown sender is shown by hex prefix so the
// operator can still see traffic from a contact the radio hasn't
// added yet.
func (g *GUI) mcAppendIncomingContact(m meshcore.ContactMessage) {
	g.mcMu.Lock()
	var senderName string
	var thread string
	for _, c := range g.mcContacts {
		if c.PubKey.Prefix() == m.SenderPrefix {
			senderName = c.AdvName
			thread = mcContactThreadID(c)
			break
		}
	}
	g.mcMu.Unlock()
	if thread == "" {
		hex := fmt.Sprintf("%x", m.SenderPrefix[:])
		thread = mcThreadID("contact", hex)
		senderName = hex
	}
	when := m.SenderTime
	if when.IsZero() {
		when = time.Now().UTC()
	}
	row := chatRow{
		when:     when,
		text:     m.Text,
		addrUs:   true,
		snrDB:    m.SNR,
		mc:       true,
		mcSender: senderName,
	}
	g.mcCapturePathFromRxLog(&row)
	g.mcAppendRow(thread, row)
	g.mcBumpUnread(thread)
	// DMs are directed-at-us by definition; mark mentioned so the
	// sidebar gets the heavier "you got pinged" highlight, not just
	// the regular unread tint.
	g.mcMarkMentioned(thread)
}

// mcAppendIncomingChannel converts a received ChannelMessage into a
// chat row in that channel's thread. The wire payload format is
// "name: text" — surfaced verbatim.
//
// Channel routing is messy because the firmware's ChannelMessage
// envelope's "channel_idx" byte is undocumented in upstream
// meshcore.js (annotated literally "reserved (0 for now, ie.
// 'public')"). Different firmware versions appear to populate it
// differently — some set the slot index, some set the channel
// hash (SHA-256(secret)[0]), some leave it at 0. Rather than
// guess wrong, we try BOTH common interpretations: first the slot
// index match, then the SHA-256 hash match. Whichever resolves to
// a configured channel wins. A debug log records the wire byte
// alongside both interpretations so a mismatch can be diagnosed
// from logs without UI guesswork.
func (g *GUI) mcAppendIncomingChannel(m meshcore.ChannelMessage) {
	wireByte := byte(m.ChannelIndex)
	g.mcMu.Lock()
	var thread string
	var matchedBy string
	// Pass 1: the byte is the slot index.
	for _, ch := range g.mcChannels {
		if ch.Index == wireByte {
			thread = mcChannelThreadID(ch)
			matchedBy = "slot"
			break
		}
	}
	// Pass 2: the byte is SHA-256(secret)[0] — the channel hash
	// the firmware uses on the air to tag GRP_TXT packets.
	if thread == "" {
		for _, ch := range g.mcChannels {
			if meshcore.ChannelHash(ch.Secret) == wireByte {
				thread = mcChannelThreadID(ch)
				matchedBy = "hash"
				break
			}
		}
	}
	g.mcMu.Unlock()
	if g.mcLog != nil {
		g.mcLog.Debugw("channel message routing",
			"wire_byte", fmt.Sprintf("0x%02x", wireByte),
			"matched_by", matchedBy,
			"thread", thread,
		)
	}
	if thread == "" {
		// No interpretation matched — park under a synthetic
		// thread keyed by the wire byte so the message is at
		// least preserved (visible via thread-history search /
		// debug). Better than silently dropping.
		thread = mcThreadID("channel", fmt.Sprintf("unknown:%02x", wireByte))
	}
	when := m.SenderTime
	if when.IsZero() {
		when = time.Now().UTC()
	}
	// Channel payloads are formatted "name: message" by the
	// firmware. Split that here so the sender lands in mcSender
	// (right-aligned column) and the bar separator falls cleanly
	// between metadata and content.
	sender, body := splitMcChannelPayload(m.Text)
	row := chatRow{
		when:     when,
		text:     body,
		snrDB:    m.SNR,
		mc:       true,
		mcSender: sender,
	}
	g.mcCapturePathFromRxLog(&row)
	g.mcAppendRow(thread, row)
	g.mcBumpUnread(thread)
	// Channel messages get the mention bump only if the body
	// contains an @[<selfName>] for us — otherwise plain unread.
	g.mcMu.Lock()
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()
	if mcTextMentionsSelf(body, selfName) {
		g.mcMarkMentioned(thread)
	}
	g.mcRefreshRoster()
	// Lightning-strike the route this message took to reach us,
	// correlated with the most recent matching PushLogRxData
	// frame. False matches are visually harmless — the path
	// animation is informational, not authoritative.
	g.mcAnimateIncomingChannel(m.ChannelIndex)
}

// splitMcChannelPayload separates the "name: message" channel
// envelope. Returns ("", text) when the format isn't recognised so
// the message body still reaches the chat with sender shown as "*".
func splitMcChannelPayload(text string) (sender, body string) {
	if i := strings.Index(text, ": "); i > 0 {
		return text[:i], text[i+2:]
	}
	return "", text
}

// mcSendActiveThread sends a text message to the currently selected
// thread. Returns a friendly system error message via AppendSystem on
// failure. Does nothing (with a hint) if no thread is selected or the
// client isn't open.
func (g *GUI) mcSendActiveThread(text string) {
	g.mcMu.Lock()
	client := g.mcClient
	thread := g.mcCurrentThread
	contacts := g.mcContacts
	channels := g.mcChannels
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("!not connected — configure in Settings → MeshCore")
		return
	}
	if thread == "" {
		g.mcAppendSystem("!pick a contact or channel from the sidebar first")
		return
	}
	now := time.Now().UTC()
	switch {
	case strings.HasPrefix(thread, "contact:"):
		var prefix meshcore.PubKeyPrefix
		var displayName string
		var recipient meshcore.Contact
		var found bool
		for _, c := range contacts {
			if mcContactThreadID(c) == thread {
				prefix = c.PubKey.Prefix()
				displayName = c.AdvName
				recipient = c
				found = true
				break
			}
		}
		if !found {
			g.mcAppendSystem("!selected contact no longer in roster")
			return
		}
		go func() {
			res, err := client.SendContactMessage(prefix, now, text)
			if err != nil {
				g.mcAppendSystem("send failed: " + err.Error())
				return
			}
			g.mcAppendTrackedTx(thread, chatRow{
				when:     now,
				text:     text,
				tx:       true,
				mc:       true,
				mcSender: selfName,
			}, res.ExpectedAckCRC, recipient.PubKey)
			_ = displayName // recipient context comes from the active thread header
			// Animate a lightning-strike out to the recipient.
			g.mcAnimateOutgoingContact(recipient)
		}()
	case strings.HasPrefix(thread, "channel:"):
		var idx uint8
		var label string
		var found bool
		for _, ch := range channels {
			if mcChannelThreadID(ch) == thread {
				idx = ch.Index
				label, _ = mcChannelLabel(ch)
				found = true
				break
			}
		}
		if !found {
			g.mcAppendSystem("!selected channel no longer in list")
			return
		}
		roster := g.mcCurrentRoster()
		go func() {
			res, err := client.SendChannelMessage(idx, now, text)
			if err != nil {
				g.mcAppendSystem("send failed: " + err.Error())
				return
			}
			g.mcAppendTrackedTx(thread, chatRow{
				when:     now,
				text:     text,
				tx:       true,
				mc:       true,
				mcSender: selfName,
			}, res.ExpectedAckCRC, meshcore.PubKey{})
			// Lightning-fan out to every roster member with a
			// known position. We don't know the actual on-air
			// route (broadcast packet), so visualise as a "this
			// went here" radial pulse.
			g.mcAnimateOutgoingChannel(roster)
			_ = label // channel context comes from the active thread header
		}()
	}
}

// applySidebarForMode swaps the channel-column body between the FT8
// bands list and the MeshCore Contacts/Channels sidebar, hides /
// shows the FT8-only chat chrome (HEARD sidebar, Auto auto-reply
// checkbox), and clears / restores FT8-only map data (DXCC worked
// overlay, FT8 spot pins, QSO partner arc) so the map in MeshCore
// mode shows the bare basemap rather than stale FT8 spots.
// Lazy-builds the MeshCore sidebar on first switch.
func (g *GUI) applySidebarForMode() {
	if g.sidebarStack == nil {
		return
	}
	mw := g.scopeMapWidget()
	if g.activeMode == "meshcore" {
		sb := g.buildMeshcoreSidebar()
		g.sidebarStack.Objects = []fyne.CanvasObject{sb}
		if g.chanHeader != nil {
			g.chanHeader.Text = "MESHCORE"
			g.chanHeader.Refresh()
		}
		if g.usersCol != nil {
			// Replace the FT8 HEARD list with the MeshCore
			// roster (per-channel senders). Keep the column
			// visible — operators want to see who's been chatty.
			roster := g.buildMeshcoreRoster()
			g.usersCol.Objects = []fyne.CanvasObject{roster}
			g.usersCol.Refresh()
			g.usersCol.Show()
		}
		if g.autoCheck != nil {
			g.autoCheck.Hide()
		}
		if g.mcCharCount != nil {
			g.mcCharCount.Show()
		}
		if g.chatHelpTap != nil {
			// Help icon stays visible in MeshCore mode now —
			// showChatHelp branches by activeMode and shows the
			// MeshCore reference there.
			g.chatHelpTap.Show()
		}
		if g.chanChatScope != nil {
			// Wider chan column (~22% of the window) so MeshCore
			// contact names like "KO9OXR-T1000" don't truncate.
			g.chanChatScope.SetOffset(0.22)
		}
		if mw != nil {
			mw.SetShowWorkedOverlay(false)
			mw.SetShowLegend(false)
			mw.SetShowMeshcoreLegend(true)
			mw.SetShowGrids(false)
			mw.SetHighlight("")
			mw.ClearSpots()
			mw.ClearQSOPartner()
			mw.Refresh()
		}
	} else {
		g.sidebarStack.Objects = []fyne.CanvasObject{g.bandList}
		if g.chanHeader != nil {
			g.chanHeader.Text = "BANDS"
			g.chanHeader.Refresh()
		}
		if g.usersCol != nil {
			// Restore the FT8 HEARD list as the column's content.
			if g.usersInner != nil {
				g.usersCol.Objects = []fyne.CanvasObject{g.usersInner}
				g.usersCol.Refresh()
			}
			g.usersCol.Show()
		}
		if g.autoCheck != nil {
			g.autoCheck.Show()
		}
		if g.mcCharCount != nil {
			g.mcCharCount.Hide()
		}
		if g.chatHelpTap != nil {
			g.chatHelpTap.Show()
		}
		if g.chanChatScope != nil {
			// Tight chan column for FT8 — band labels are short.
			g.chanChatScope.SetOffset(0.13)
		}
		if mw != nil && g.app != nil {
			// Restore the operator's saved overlay preference, the
			// FT8 OTA legend, and the Maidenhead grid lines. Drop
			// the MeshCore node overlay + legend so the FT8 view
			// isn't littered with mesh peers. New FT8 spots will
			// repopulate from the next decode slot once the
			// decoder un-pauses.
			overlay := g.app.Preferences().BoolWithFallback("map_worked_overlay", true)
			mw.SetShowWorkedOverlay(overlay)
			mw.SetShowLegend(true)
			mw.SetShowMeshcoreLegend(false)
			mw.SetShowGrids(true)
			mw.ClearMeshNodes()
			mw.ClearSelfPosition() // diamond falls back to myGrid centroid
			mw.Refresh()
		}
	}
	g.sidebarStack.Refresh()
}

// scopeMapWidget returns the embedded MapWidget if the scope pane is
// up, nil otherwise. Wrapper so the few callers that need the map
// pointer don't reach through scopePane internals every time.
func (g *GUI) scopeMapWidget() *mapview.MapWidget {
	if g.scope == nil {
		return nil
	}
	return g.scope.mapWidget
}

// applyChatBufferForMode snapshots the outgoing mode's chat history
// and restores the incoming mode's buffer. FT8 mode owns
// ft8RowsBackup as a single buffer; MeshCore mode owns one buffer per
// thread, keyed by mcCurrentThread (empty when nothing is selected).
func (g *GUI) applyChatBufferForMode(prevMode string) {
	g.mu.Lock()
	switch prevMode {
	case "ft8":
		g.ft8RowsBackup = g.rows
	case "meshcore":
		if g.mcThreadHistory == nil {
			g.mcThreadHistory = map[string][]chatRow{}
		}
		if g.mcCurrentThread != "" {
			snap := make([]chatRow, len(g.rows))
			copy(snap, g.rows)
			g.mcThreadHistory[g.mcCurrentThread] = snap
		}
	}
	switch g.activeMode {
	case "ft8":
		g.rows = g.ft8RowsBackup
		g.ft8RowsBackup = nil
	case "meshcore":
		if g.mcCurrentThread != "" && g.mcThreadHistory[g.mcCurrentThread] != nil {
			hist := g.mcThreadHistory[g.mcCurrentThread]
			g.rows = make([]chatRow, len(hist))
			copy(g.rows, hist)
		} else {
			g.rows = nil
		}
	}
	g.mu.Unlock()
	if g.chatList != nil {
		fyne.Do(func() {
			g.chatList.Refresh()
			if n := len(g.rows); n > 0 {
				g.chatList.ScrollTo(n - 1)
			}
		})
	}
}

// remoteCallFromMessage extracts the "remote operator" callsign from a
// decoded FT8 message. For "CQ X Y" returns X; for "X Y Z" returns Y when
// the first token is a hash placeholder, X otherwise. Empty if neither
// position has a recognisable callsign.
//
// Local copy; matches the helper in internal/ui without dragging that
// package's heavyweight deps into the Nocord build.
func remoteCallFromMessage(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "CQ") {
		// "CQ MOD X GRID" or "CQ X GRID". The modifier set (DX, NA,
		// EU, …, POTA, SOTA, numeric zones, …) is owned by lib/ft8 so
		// the decoder and this reply-target parser stay in sync.
		if len(fields) >= 3 && ft8.IsCQModifier(fields[1]) {
			return fields[2]
		}
		if len(fields) >= 2 {
			return fields[1]
		}
		return ""
	}
	if len(fields) >= 1 {
		return fields[0]
	}
	return ""
}

// isPlausibleCallsign returns true if s has the rough shape of an amateur
// callsign: at least one letter and at least one digit, length 3–10, only
// uppercase letters / digits / "/". Defensive -keeps obvious typos out
// of the TX queue without an exhaustive ITU-prefix check.
// messageIndicatesOTA classifies a decoded FT8 message for portable /
// outdoor activity. Returns the program name when one is recognised:
//
//   - POTA / SOTA / IOTA / WWFF / BOTA / LOTA / NOTA — explicit FT8
//     "CQ <PROG> CALL GRID" form (n28 reservation for named
//     activities). The program name comes through verbatim.
//   - PORTABLE — a /P /M /MM /AM suffix on a callsign-shaped token,
//     no explicit program. Conventional ham shorthand for
//     portable/mobile/maritime mobile/aeronautical mobile.
//
// Returns "" for system messages or anything that doesn't fit.
func messageIndicatesOTA(text string) string {
	fields := strings.Fields(strings.ToUpper(text))
	if len(fields) >= 2 && fields[0] == "CQ" {
		switch fields[1] {
		case "POTA", "SOTA", "IOTA", "WWFF", "BOTA", "LOTA", "NOTA":
			return fields[1]
		}
	}
	for _, f := range fields {
		i := strings.Index(f, "/")
		if i <= 0 || i == len(f)-1 {
			continue
		}
		suffix := f[i+1:]
		switch suffix {
		case "P", "M", "MM", "AM":
			pre := f[:i]
			hasL, hasD := false, false
			for _, r := range pre {
				if r >= 'A' && r <= 'Z' {
					hasL = true
				}
				if r >= '0' && r <= '9' {
					hasD = true
				}
			}
			if hasL && hasD {
				return "PORTABLE"
			}
		}
	}
	return ""
}

func isPlausibleCallsign(s string) bool {
	if len(s) < 3 || len(s) > 10 {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
			hasLetter = true
		case c >= '0' && c <= '9':
			hasDigit = true
		case c == '/':
			// portable / mobile suffix
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}

// textTap wraps a canvas.Text in a tappable widget so a small label can act
// as a button without the bulky default button chrome / minimum height.
type textTap struct {
	widget.BaseWidget
	text  *canvas.Text
	onTap func()
}

func newTextTap(text *canvas.Text, onTap func()) *textTap {
	t := &textTap{text: text, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

func (t *textTap) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.text)
}

func (t *textTap) Tapped(_ *fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

// chatActionsLayout reserves a fixed strip on the right side of a chat row
// for inline action buttons (reply, future: log QSO, etc.). Each child
// occupies a fixed-width slot; slot 0 is the rightmost. Hidden children
// keep their slot reserved so visible buttons never shift around as
// different rows show or hide actions.
type chatActionsLayout struct {
	slotWidth float32
	slots     int
}

func (l *chatActionsLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for i, o := range objs {
		// slot index 0 = rightmost; place each at (rightEdge - (i+1)*slotWidth).
		x := size.Width - float32(i+1)*l.slotWidth
		o.Move(fyne.NewPos(x, 0))
		o.Resize(fyne.NewSize(l.slotWidth, size.Height))
	}
}

func (l *chatActionsLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(l.slotWidth*float32(l.slots), 0)
}

// fixedWidthLayout pins its single child to a fixed pixel width, full parent
// height. Used to give Border slots an unambiguous size when SetMinSize on
// a nested rectangle doesn't propagate cleanly.
type fixedWidthLayout struct {
	width float32
}

func (l *fixedWidthLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) == 0 {
		return
	}
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(fyne.NewSize(l.width, size.Height))
}

func (l *fixedWidthLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(l.width, 0)
}

// hoverRow wraps a row's content with hover hooks so the HEARD list can
// highlight the corresponding station on the map and waterfall when the
// pointer is over it. Tap is delegated to the surrounding widget.List
// via OnSelected (we don't need a tap handler here).
type hoverRow struct {
	widget.BaseWidget
	inner       fyne.CanvasObject
	onHoverIn   func()
	onHoverOut  func()
	onHoverMove func(absPos fyne.Position) // cursor moved while hovering
	onSecondary func(pos fyne.Position)    // right-click → context menu
	// listIdx is set by MeshCore list bind callbacks (RxLog rows,
	// channel rows) so the row's onSecondary handler can look up
	// the underlying entry without capturing a stale closure
	// value. Unused by other hoverRow consumers; harmless when zero.
	listIdx int
	// onTap fires on left-click. Required when hoverRow wraps a
	// widget.List row template, because hoverRow's
	// SecondaryTappable impl stops Fyne bubbling pointer events
	// to the parent List (so OnSelected would never fire).
	// Callers set this to invoke the same logic as OnSelected,
	// typically `list.Select(row.listIdx)`.
	onTap func()
}

var _ fyne.SecondaryTappable = (*hoverRow)(nil)
var _ fyne.Tappable = (*hoverRow)(nil)

func (h *hoverRow) TappedSecondary(ev *fyne.PointEvent) {
	if h.onSecondary != nil {
		h.onSecondary(ev.AbsolutePosition)
	}
}

// Tapped — left-click handler. hoverRow exists primarily to add
// right-click support to widgets that don't natively expose it; it
// also has to handle left-click here because once the widget
// implements SecondaryTappable, Fyne stops bubbling pointer events
// to the parent (and so a containing widget.List would never see
// the tap that should fire its OnSelected). onTap is set per-bind
// by callers that need that behaviour (channels / contacts / RxLog
// list rows). If onTap is nil the click is silently dropped — fine
// for chat rows where selection is irrelevant.
func (h *hoverRow) Tapped(*fyne.PointEvent) {
	if h.onTap != nil {
		h.onTap()
	}
}

func newHoverRow(inner fyne.CanvasObject) *hoverRow {
	h := &hoverRow{inner: inner}
	h.ExtendBaseWidget(h)
	return h
}

func (h *hoverRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(h.inner)
}

func (h *hoverRow) MouseIn(*desktop.MouseEvent) {
	if h.onHoverIn != nil {
		h.onHoverIn()
	}
}

func (h *hoverRow) MouseMoved(ev *desktop.MouseEvent) {
	if h.onHoverMove != nil {
		h.onHoverMove(ev.AbsolutePosition)
	}
}

func (h *hoverRow) MouseOut() {
	if h.onHoverOut != nil {
		h.onHoverOut()
	}
}

// hoverTip wraps any CanvasObject and pops a small floating label
// near the cursor on MouseIn — used to surface the exact local
// datetime behind compact "HH:MM:SS" timestamps in chat / RxLog
// rows. The wrapped child is rendered unchanged; only hover events
// are intercepted. Tooltip text is mutable so list-row recycling
// can update it per bind.
//
// Implementation notes that matter:
//
//   - Show is debounced by hoverTipDelay so a brief mouse-pass
//     doesn't flash a tooltip in the operator's face.
//   - The tooltip floats as a primitive container in the canvas
//     overlay stack rather than via widget.PopUp. PopUp installs a
//     dismiss-on-tap handler that consumes ANY click on the canvas
//     while it's visible — that ate the right-clicks on RxLog rows
//     before. Primitive overlays are click-through.
//   - The overlay does NOT follow the cursor (MouseMoved is a no-
//     op): stationary placement avoids racing into the operator's
//     right-click target after they've decided where to click.
type hoverTip struct {
	widget.BaseWidget
	inner   fyne.CanvasObject
	tooltip string
	timer   *time.Timer
	overlay fyne.CanvasObject
	// seq invalidates in-flight debounce timers when MouseIn /
	// MouseOut state changes. Read + written from UI-thread paths
	// only (timer callback re-enters via fyne.Do).
	seq uint64
}

// hoverTipDelay is how long the cursor must rest over a tooltip
// target before the popup appears. 400ms is the same threshold
// macOS / VSCode use — long enough to skip drive-by hovers, short
// enough that intentional inspection feels responsive.
const hoverTipDelay = 400 * time.Millisecond

func newHoverTip(inner fyne.CanvasObject, tooltip string) *hoverTip {
	h := &hoverTip{inner: inner, tooltip: tooltip}
	h.ExtendBaseWidget(h)
	return h
}

func (h *hoverTip) SetTooltip(s string) { h.tooltip = s }

func (h *hoverTip) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(h.inner)
}

var _ desktop.Hoverable = (*hoverTip)(nil)

func (h *hoverTip) MouseIn(ev *desktop.MouseEvent) {
	if h.tooltip == "" {
		return
	}
	if h.timer != nil {
		h.timer.Stop()
	}
	h.seq++
	mySeq := h.seq
	pos := ev.AbsolutePosition.Add(fyne.NewPos(12, 18))
	h.timer = time.AfterFunc(hoverTipDelay, func() {
		fyne.Do(func() {
			if h.seq != mySeq || h.tooltip == "" || h.overlay != nil {
				return
			}
			h.showOverlay(pos)
		})
	})
}

func (h *hoverTip) MouseMoved(*desktop.MouseEvent) {
	// Intentional no-op: see hoverTip godoc — moving the overlay
	// chases the cursor into the click target.
}

func (h *hoverTip) MouseOut() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.seq++ // invalidate any in-flight timer that already fired
	h.hideOverlay()
}

func (h *hoverTip) showOverlay(pos fyne.Position) {
	cv := fyne.CurrentApp().Driver().CanvasForObject(h)
	if cv == nil {
		return
	}
	label := canvas.NewText(h.tooltip, color.RGBA{230, 232, 240, 255})
	label.TextStyle = fyne.TextStyle{Monospace: true}
	label.TextSize = 11
	bg := canvas.NewRectangle(color.RGBA{40, 42, 48, 235})
	bg.StrokeColor = color.RGBA{90, 92, 100, 255}
	bg.StrokeWidth = 1
	box := container.NewStack(bg, container.NewPadded(label))
	box.Resize(box.MinSize())
	box.Move(pos)
	cv.Overlays().Add(box)
	h.overlay = box
}

func (h *hoverTip) hideOverlay() {
	if h.overlay == nil {
		return
	}
	if cv := fyne.CurrentApp().Driver().CanvasForObject(h); cv != nil {
		cv.Overlays().Remove(h.overlay)
	}
	h.overlay = nil
}

// formatHoverTime renders a Time as a long, locale-readable
// string used in tooltip popups. Local zone with explicit short
// zone name so operators travelling across TZs see the correct
// wall-clock + offset (e.g. "Mon Jan 2, 2026 15:04:05 PST").
func formatHoverTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("Mon Jan 2, 2006 15:04:05 MST")
}

// mcHashSegment is one piece of a parsed chat-row text — a plain
// run, a path-hash link, or an @-mention. Path hashes are 1-, 2-,
// or 3-byte prefixes of a contact pubkey (the firmware uses the
// first PATH_HASH_SIZE bytes — 1 by default — to identify hops in
// Packet.Path). Tokens that don't resolve to a known contact stay
// in plain runs so common English hex-looking words ("be", "ad",
// "decade") aren't underlined. Mentions match the @[Name] wire
// convention used by upstream MeshCore clients (web, iOS) — the
// brackets are wire-format only and stripped when rendering.
type mcHashSegment struct {
	text        string          // visible characters (mentions: "@Name" without brackets)
	link        bool            // true when token resolves to a contact (path-hash link)
	mention     bool            // true when this is an @[Name] mention
	mentionSelf bool            // true when the mention targets the operator's own advert name
	url         string          // populated for http(s) URL spans — render as clickable link
	pub         meshcore.PubKey // populated when link == true OR mention resolved to a contact
}

// mcURLRe matches http:// and https:// URLs. The character class
// stops at whitespace; trailing punctuation (.,;:!?)]) commonly
// hugs URLs in prose ("check https://example.com.") so we strip
// it after matching to keep the link targeting the actual page,
// not page-plus-period.
var mcURLRe = regexp.MustCompile(`https?://\S+`)

// mcURLTrimTrailing is the set of punctuation characters peeled
// off the right end of a URL match before the link is rendered.
const mcURLTrimTrailing = ".,;:!?)]\"'>"

// mcMentionRe matches the "@[Name]" wire convention. Names can
// contain anything except a literal closing bracket so multi-word
// or punctuated handles (rare on MeshCore but possible) survive.
var mcMentionRe = regexp.MustCompile(`@\[([^\]]+)\]`)

// mcHashSeriesRe matches a comma-separated SERIES of 2/4/6 hex
// digits — at least two tokens. The series form is the only one we
// auto-link, because a single 2-hex token ("be", "78", "ad") is
// indistinguishable from ordinary English / numeric text and would
// false-positive constantly. A series like "df,b7,43" or
// "df, b7, 43" is unambiguous: nothing in normal chat looks like
// that. Whitespace between commas is allowed so manually-typed
// lists still match.
var mcHashSeriesRe = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?:[0-9a-f]{2}){0,2}(?:\s*,\s*[0-9a-f]{2}(?:[0-9a-f]{2}){0,2})+\b`)

// mcHashTokenInSeriesRe pulls one hex token (and the byte offsets
// of just the hex characters) out of a series substring. Used after
// mcHashSeriesRe locates a series so we can emit each token as its
// own link/plain segment with the comma separators preserved as
// plain runs between them.
var mcHashTokenInSeriesRe = regexp.MustCompile(`(?i)[0-9a-f]{2}(?:[0-9a-f]{2}){0,2}`)

// mcParseChatSegments is the unified inline-segment parser used by
// the chat row binder. It splits text into plain runs, @[Name]
// mentions (rendered without brackets, styled), and path-hash
// links (existing behaviour). Mentions are extracted first as the
// outermost frames; hash-link detection then runs on the plain
// runs between mentions, so a mention can't accidentally hide
// inside a hex series (or vice versa). selfName is the operator's
// own advert name — when a mention's target matches selfName the
// segment is flagged mentionSelf so the renderer can highlight it
// (Slack "@you" style). Returns nil when nothing notable was
// found, letting the caller fall back to the plain canvas.Text
// path.
func mcParseChatSegments(text string, contacts []meshcore.Contact, selfName string) []mcHashSegment {
	if text == "" {
		return nil
	}
	urlLocs := mcURLRe.FindAllStringIndex(text, -1)
	mentionLocs := mcMentionRe.FindAllStringSubmatchIndex(text, -1)
	if len(urlLocs) == 0 && len(mentionLocs) == 0 {
		// No URLs / mentions — fall through to hash-only parsing.
		return mcParseHashLinks(text, contacts)
	}
	var out []mcHashSegment
	cursor := 0
	// emitMentionsAndPlain runs mention extraction over a plain
	// substring, then hands any remaining un-mentioned text to
	// hash-link parsing. Used for the gaps BETWEEN URL matches so
	// URLs are the outermost frames and mentions / hash series
	// don't accidentally swallow URL characters.
	emitMentionsAndPlain := func(s string) {
		if s == "" {
			return
		}
		if len(mentionLocs) == 0 {
			// No mentions in the original text — go straight to
			// hash-link parsing for this slice.
			if hashSegs := mcParseHashLinks(s, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: s})
			}
			return
		}
		// Mentions exist somewhere in the original text; rescan
		// just this slice for them so the offsets stay local.
		subMentions := mcMentionRe.FindAllStringSubmatchIndex(s, -1)
		if len(subMentions) == 0 {
			if hashSegs := mcParseHashLinks(s, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: s})
			}
			return
		}
		subCursor := 0
		emitPlainHashes := func(p string) {
			if p == "" {
				return
			}
			if hashSegs := mcParseHashLinks(p, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: p})
			}
		}
		for _, m := range subMentions {
			if m[0] > subCursor {
				emitPlainHashes(s[subCursor:m[0]])
			}
			name := s[m[2]:m[3]]
			seg := mcHashSegment{text: "@" + name, mention: true}
			if selfName != "" && strings.EqualFold(name, selfName) {
				seg.mentionSelf = true
			}
			for i := range contacts {
				if strings.EqualFold(contacts[i].AdvName, name) {
					seg.pub = contacts[i].PubKey
					break
				}
			}
			out = append(out, seg)
			subCursor = m[1]
		}
		if subCursor < len(s) {
			emitPlainHashes(s[subCursor:])
		}
	}
	// Walk URL matches as outermost frames. Plain text between
	// URLs goes through mention + hash extraction.
	for _, u := range urlLocs {
		if u[0] > cursor {
			emitMentionsAndPlain(text[cursor:u[0]])
		}
		raw := text[u[0]:u[1]]
		// Peel trailing punctuation that's almost certainly part
		// of the surrounding prose, not the URL.
		trim := strings.TrimRight(raw, mcURLTrimTrailing)
		out = append(out, mcHashSegment{text: trim, url: trim})
		// If we trimmed any tail, emit it as plain so the visible
		// text matches the input character-for-character.
		if tail := raw[len(trim):]; tail != "" {
			out = append(out, mcHashSegment{text: tail})
		}
		cursor = u[1]
	}
	if cursor < len(text) {
		emitMentionsAndPlain(text[cursor:])
	}
	// If nothing in the result is a link / mention / URL, the
	// parser effectively did no work — let the plain-text path
	// handle it.
	for _, s := range out {
		if s.link || s.mention || s.url != "" {
			return out
		}
	}
	return nil
}

// mcParseHashLinks splits text into plain + link segments. Only
// path-hash *series* (≥2 hex tokens joined by commas) get scanned;
// individual tokens that resolve against the contacts slice (via
// pubkey-prefix match of 1/2/3 bytes) become links, the rest stay
// in plain runs. The roster is passed in (not read off
// g.mcContacts) so callers can avoid re-locking on every row
// binding.
func mcParseHashLinks(text string, contacts []meshcore.Contact) []mcHashSegment {
	if text == "" || len(contacts) == 0 {
		return nil
	}
	seriesLocs := mcHashSeriesRe.FindAllStringIndex(text, -1)
	if len(seriesLocs) == 0 {
		return nil
	}
	resolve := func(token string) (meshcore.PubKey, bool) {
		decoded, err := hex.DecodeString(token)
		if err != nil {
			return meshcore.PubKey{}, false
		}
		for i := range contacts {
			pk := contacts[i].PubKey
			if len(decoded) > len(pk) {
				continue
			}
			equal := true
			for j, b := range decoded {
				if pk[j] != b {
					equal = false
					break
				}
			}
			if equal {
				return pk, true
			}
		}
		return meshcore.PubKey{}, false
	}

	var out []mcHashSegment
	cursor := 0
	anyLink := false
	for _, sLoc := range seriesLocs {
		series := text[sLoc[0]:sLoc[1]]
		tokenLocs := mcHashTokenInSeriesRe.FindAllStringIndex(series, -1)
		if len(tokenLocs) < 2 {
			continue
		}
		// Emit any plain text between the previous cursor and this
		// series.
		if sLoc[0] > cursor {
			out = append(out, mcHashSegment{text: text[cursor:sLoc[0]]})
		}
		// Walk the series: token, separator, token, separator, …
		seriesCursor := 0
		for _, tLoc := range tokenLocs {
			if tLoc[0] > seriesCursor {
				out = append(out, mcHashSegment{text: series[seriesCursor:tLoc[0]]})
			}
			token := series[tLoc[0]:tLoc[1]]
			if pub, ok := resolve(token); ok {
				out = append(out, mcHashSegment{text: token, link: true, pub: pub})
				anyLink = true
			} else {
				out = append(out, mcHashSegment{text: token})
			}
			seriesCursor = tLoc[1]
		}
		if seriesCursor < len(series) {
			out = append(out, mcHashSegment{text: series[seriesCursor:]})
		}
		cursor = sLoc[1]
	}
	if !anyLink {
		// All tokens looked like a series but none resolved to a
		// contact — render the original text untouched so we don't
		// gratuitously segment a non-routing comma list.
		return nil
	}
	if cursor < len(text) {
		out = append(out, mcHashSegment{text: text[cursor:]})
	}
	return out
}

// mcHashLink is a tappable canvas-text widget rendering a single
// path-hash token in cyan with an underline. Left-click flies the
// map to the linked contact's broadcast position; right-click pops
// the same Info / Open chat / Show last path menu used by the map's
// node right-click handler.
type mcHashLink struct {
	widget.BaseWidget
	text        string
	color       color.Color
	onTap       func()
	onSecondary func(absPos fyne.Position)
}

func newMcHashLink(text string, onTap func(), onSecondary func(fyne.Position)) *mcHashLink {
	h := &mcHashLink{
		text:        text,
		color:       color.RGBA{120, 200, 255, 255},
		onTap:       onTap,
		onSecondary: onSecondary,
	}
	h.ExtendBaseWidget(h)
	return h
}

var (
	_ fyne.Tappable          = (*mcHashLink)(nil)
	_ fyne.SecondaryTappable = (*mcHashLink)(nil)
	_ desktop.Cursorable     = (*mcHashLink)(nil)
)

func (h *mcHashLink) Tapped(_ *fyne.PointEvent) {
	if h.onTap != nil {
		h.onTap()
	}
}

func (h *mcHashLink) TappedSecondary(ev *fyne.PointEvent) {
	if h.onSecondary != nil {
		h.onSecondary(ev.AbsolutePosition)
	}
}

func (h *mcHashLink) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (h *mcHashLink) CreateRenderer() fyne.WidgetRenderer {
	t := canvas.NewText(h.text, h.color)
	t.TextStyle = fyne.TextStyle{Monospace: true}
	t.TextSize = 10
	u := canvas.NewLine(h.color)
	u.StrokeWidth = 1
	return &mcHashLinkRenderer{label: t, underline: u}
}

type mcHashLinkRenderer struct {
	label     *canvas.Text
	underline *canvas.Line
}

func (r *mcHashLinkRenderer) Layout(size fyne.Size) {
	r.label.Resize(size)
	r.label.Move(fyne.NewPos(0, 0))
	min := r.label.MinSize()
	r.underline.Position1 = fyne.NewPos(0, min.Height-1)
	r.underline.Position2 = fyne.NewPos(min.Width, min.Height-1)
}

func (r *mcHashLinkRenderer) MinSize() fyne.Size { return r.label.MinSize() }

func (r *mcHashLinkRenderer) Refresh() {
	r.label.Refresh()
	r.underline.Refresh()
}

func (r *mcHashLinkRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.label, r.underline}
}

func (r *mcHashLinkRenderer) Destroy() {}

// inlineFlowLayout packs children left-to-right with no padding so
// adjacent canvas.Text widgets render as one continuous line. Used
// for the segmented chat message column when path-hash links are
// inlined alongside plain text.
type inlineFlowLayout struct{}

func (inlineFlowLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		sz := o.MinSize()
		w += sz.Width
		if sz.Height > h {
			h = sz.Height
		}
	}
	return fyne.NewSize(w, h)
}

func (inlineFlowLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	var x float32
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		sz := o.MinSize()
		o.Resize(fyne.NewSize(sz.Width, size.Height))
		o.Move(fyne.NewPos(x, 0))
		x += sz.Width
	}
}
