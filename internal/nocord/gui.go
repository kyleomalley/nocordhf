// Package nocord -"NocordHF" -is a Discord-style chat-focused UI for FT8/FT4
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
	"image/color"
	"math"
	"os"
	"os/exec"
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
	"github.com/kyleomalley/nocordhf/lib/lotw"
	"github.com/kyleomalley/nocordhf/lib/ntpcheck"
	"github.com/kyleomalley/nocordhf/lib/tqsl"
	"github.com/kyleomalley/nocordhf/lib/waterfall"
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
	Tune       bool   // pure-carrier tune transmission
	StopCh     chan struct{}
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
}

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
	input        *widget.Entry
	bandLabel    *canvas.Text
	userCallText *canvas.Text // bottom-of-channel-column user bar -operator's callsign
	userGridText *canvas.Text // bottom-of-channel-column user bar -operator's grid
	scope        *scopePane   // rightmost column: waterfall + map
	sendBtn      *widget.Button

	// TX state: when a TX is in flight, sendBtn re-labels to "Stop" and
	// clicking it closes activeStopCh -the main TX loop watches this and
	// short-circuits the playback. Single TX at a time, so a single
	// channel suffices.
	txActive     bool
	activeStopCh chan struct{}

	// decodePopup is the floating widget shown while the operator hovers
	// a decode box on the waterfall. Held on the GUI so successive
	// hovers replace the existing popup instead of stacking. A pending
	// hide timer debounces MouseOut → MouseIn back-to-back events so
	// the popup doesn't flash while crossing between adjacent boxes.
	// decodePopup is the floating widget shown while hovering a decode
	// box. Persistent: once shown, the popup stays at its initial
	// position; subsequent hovers on different boxes refresh the
	// magnified-image + text content in place rather than relocating
	// the panel. The first hover after a hide picks the position
	// relative to the box that was hovered.
	decodePopup        *widget.PopUp
	decodePopupContent *fyne.Container // VBox swapped in place on content updates
	decodePopupHide    *time.Timer
	decodePopupCall    string

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

const maxRows = 500

// NewGUI builds the Nocord window for a Fyne app. buildID lands in the title
// bar so screenshots identify which build they came from. txCh and tuneCh
// are caller-owned; the GUI sends to them but never closes them.
func NewGUI(a fyne.App, buildID string, txCh chan TxRequest, tuneCh chan uint64) *GUI {
	g := &GUI{
		app:     a,
		buildID: buildID,
		txCh:    txCh,
		tuneCh:  tuneCh,
		current: DefaultBands[4], // 20m default
	}
	g.window = a.NewWindow("NocordHF [build=" + buildID + "]")
	g.window.Resize(fyne.NewSize(1100, 720))
	g.window.SetContent(g.buildLayout())
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
	ft8.SetITUFilterEnabled(itu)
	g.mu.Lock()
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

// showSettings opens a tabbed settings dialog with three panes:
//
//   - Profile: operator callsign + grid (writes to fyne.Preferences;
//     SetProfile applies live).
//   - Map / Decoder: worked-grid overlay toggle, strict ITU filter on
//     weak decodes (both legacy toggles).
//   - LoTW: ARRL Logbook of the World credentials. On save we
//     instantiate lotw.Client, kick off a background sync, and feed
//     the results into the worked / confirmed grid map overlay.
//
// All values persist via fyne.Preferences across launches.
func (g *GUI) showSettings() {
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
	profileForm := widget.NewForm(
		widget.NewFormItem("Callsign", callEntry),
		widget.NewFormItem("Grid", gridEntry),
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
		g.AppendSystem(fmt.Sprintf("ⓘ radio: %s on %s", kr.Name, port))
	})
	// TX audio level slider. On USB-CODEC rigs the rig's ALC follows
	// the audio drive linearly, so this acts as a soft "TX power"
	// control: 0.02 (≈ -34 dBFS) at the bottom for a faint signal,
	// 0.5 (≈ -6 dBFS) at the top for full ALC drive. Default 0.18
	// is the conservative ft8m8 setting that keeps ALC happy while
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
		g.rememberHeard(sender, d.SNR, isCQ)
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
}

// AppendSystem renders a synthesised line ("waiting for slot…", "TX done").
func (g *GUI) AppendSystem(text string) {
	g.appendRow(chatRow{when: time.Now(), system: true, text: text})
}

// AppendTxEcho records that we just transmitted; shows up in the chat with
// a TX-distinct style. Also feeds the QSO tracker so a directed call we
// initiated opens a contact and a closing 73 we send finalises one.
func (g *GUI) AppendTxEcho(msg string) {
	now := time.Now()
	g.appendRow(chatRow{when: now, tx: true, text: msg})
	if g.qso != nil {
		g.qso.FireTX(msg, now)
	}
}

func (g *GUI) appendRow(r chatRow) {
	g.mu.Lock()
	g.rows = append(g.rows, r)
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

func (g *GUI) rememberHeard(call string, snr float64, isCQ bool) {
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
	call   string
	snr    float64
	when   time.Time
	lastCQ time.Time
}

func (g *GUI) heardSnapshot() []heardRow {
	g.mu.Lock()
	mode := g.heardSort
	out := make([]heardRow, 0, len(g.heard))
	for c, e := range g.heard {
		out = append(out, heardRow{call: c, snr: e.snr, when: e.lastSeen, lastCQ: e.lastCQ})
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
		// "CQ MOD X GRID" or "CQ X GRID"
		switch {
		case len(fields) >= 3 && (fields[1] == "DX" || fields[1] == "POTA" || fields[1] == "SOTA" || fields[1] == "TEST"):
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
	g.mu.Unlock()

	now := time.Now().UTC()
	slotPhase := now.Second() % 15
	parts := []string{
		fmt.Sprintf("UTC %s", now.Format("15:04:05")),
		fmt.Sprintf("slot +%ds", slotPhase),
	}
	// "decodes" cell: live count for the current slot if any decodes
	// have landed, otherwise the previous slot's final count so the
	// header is informative during the silent gap.
	nowSlot := now.Unix() - now.Unix()%15
	switch {
	case curCount > 0 && curSlot == nowSlot:
		parts = append(parts, fmt.Sprintf("rx %d", curCount))
	case prevCount > 0:
		parts = append(parts, fmt.Sprintf("rx %d (prev)", prevCount))
	}

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
			g.input.SetText(call)
			g.handleSubmit(call)
			g.input.SetText("")
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

// showDecodePopup renders / updates the floating panel that shows a
// magnified slice of the waterfall around the hovered/selected decode
// box plus the station's identity / SNR.
//
// The panel is anchored to a FIXED position — the top-left corner of
// the SCOPE pane — and only its contents change as the operator
// hovers different boxes. Earlier versions tried to position the
// popup next to whichever box was hovered, but fyne's coordinate
// system across nested HSplit / VSplit / Border containers made the
// reported "absolute" position unstable, so the popup would jump
// around (and sometimes appear far outside the visible scope pane).
// The fixed anchor avoids the coordinate-translation pitfalls
// entirely; the popup is always in the same place.
//
// The screenPos argument is kept for API compatibility but only used
// the very first time we have to ShowAtPosition (Fyne's PopUp won't
// render until ShowAtPosition / Show is called once).
func (g *GUI) showDecodePopup(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position) {
	g.mu.Lock()
	if g.decodePopupHide != nil {
		g.decodePopupHide.Stop()
		g.decodePopupHide = nil
	}
	sameCall := g.decodePopupCall == call && g.decodePopup != nil && g.decodePopup.Visible()
	popupOpen := g.decodePopup != nil
	g.mu.Unlock()
	if sameCall {
		return
	}
	if g.scope == nil || g.window == nil {
		return
	}
	// Compute a fixed anchor: top-left of the scope container in
	// canvas-absolute coordinates. Stable across the whole session;
	// the only thing that moves it is window resize.
	anchor := fyne.NewPos(0, 0)
	if g.scope.container != nil {
		anchor = fyne.CurrentApp().Driver().AbsolutePositionForObject(g.scope.container)
		anchor = fyne.NewPos(anchor.X+8, anchor.Y+30)
	}
	// screenPos isn't trusted; replace with our fixed anchor. The
	// parameter stays in the signature so existing callers (HEARD
	// list click, scope hover hook) don't need to know about this.
	_ = screenPos
	screenPos = anchor

	img := g.scope.MagnifiedSignalSlice(slotStart, freqHz, 180, 90)
	g.mu.Lock()
	heard, hasHeard := g.heard[call]
	g.mu.Unlock()
	cc := "  "
	if sc := callsign.ShortCode(call); len(sc) >= 2 {
		cc = sc[len(sc)-2:]
	}
	identity := fmt.Sprintf("%s  %s", cc, call)
	snrLine := ""
	if hasHeard {
		snrLine = fmt.Sprintf("SNR %+0.0f dB | %s ago",
			heard.snr, time.Since(heard.lastSeen).Round(time.Second))
	}
	freqLine := fmt.Sprintf("%.0f Hz", freqHz)

	identText := canvas.NewText(identity, color.RGBA{220, 225, 235, 255})
	identText.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	identText.TextSize = 12
	freqText := canvas.NewText(freqLine, color.RGBA{170, 175, 185, 255})
	freqText.TextStyle = fyne.TextStyle{Monospace: true}
	freqText.TextSize = 10
	snrText := canvas.NewText(snrLine, color.RGBA{170, 175, 185, 255})
	snrText.TextStyle = fyne.TextStyle{Monospace: true}
	snrText.TextSize = 10

	var bodyChildren []fyne.CanvasObject
	if img != nil {
		ci := canvas.NewImageFromImage(img)
		ci.FillMode = canvas.ImageFillContain
		ci.SetMinSize(fyne.NewSize(110, 220))
		bodyChildren = []fyne.CanvasObject{ci, identText, freqText, snrText}
	} else {
		bodyChildren = []fyne.CanvasObject{identText, freqText, snrText}
	}

	g.mu.Lock()
	if popupOpen && g.decodePopupContent != nil {
		// Update the existing panel in place. Replacing the VBox's
		// children + refreshing leaves the popup parked at its
		// current position — no bounce, no flicker. If the popup
		// happens to be hidden (operator dismissed it some other
		// way) re-show it at the supplied screenPos so subsequent
		// hovers actually surface a visible popup.
		g.decodePopupContent.Objects = bodyChildren
		content := g.decodePopupContent
		pop := g.decodePopup
		g.decodePopupCall = call
		g.mu.Unlock()
		fyne.Do(func() {
			content.Refresh()
			if pop != nil && !pop.Visible() {
				pop.ShowAtPosition(screenPos)
			}
		})
		return
	}
	g.mu.Unlock()

	body := container.NewVBox(bodyChildren...)
	bg := canvas.NewRectangle(color.RGBA{30, 32, 38, 240})
	bg.StrokeColor = color.RGBA{90, 95, 105, 255}
	bg.StrokeWidth = 1
	wrapped := container.NewStack(bg, container.NewPadded(body))

	g.mu.Lock()
	if g.decodePopup != nil {
		g.decodePopup.Hide()
	}
	pop := widget.NewPopUp(wrapped, g.window.Canvas())
	g.decodePopup = pop
	g.decodePopupContent = body
	g.decodePopupCall = call
	g.mu.Unlock()
	pop.ShowAtPosition(screenPos)
}

// hideDecodePopup is a no-op. Earlier versions auto-dismissed the
// magnification popup on hover-out, but the popup was never reliably
// stable because Fyne's pointer-event dispatch races with the
// per-row decodeBox repositioning. We now leave the popup visible
// indefinitely once shown — its contents update in place when the
// operator hovers a different decode box, and the popup is dismissed
// only by clicking its inline Close button (added in showDecodePopup).
func (g *GUI) hideDecodePopup() {
	// Intentionally empty.
}

// dismissDecodePopup tears down the magnification popup. Called from
// the popup's own Close button.
func (g *GUI) dismissDecodePopup() {
	g.mu.Lock()
	pop := g.decodePopup
	g.decodePopup = nil
	g.decodePopupContent = nil
	g.decodePopupCall = ""
	if g.decodePopupHide != nil {
		g.decodePopupHide.Stop()
		g.decodePopupHide = nil
	}
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
		g.input.SetText(call)
		g.handleSubmit(call)
		g.input.SetText("")
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
		}
	}()
}

// buildLayout assembles the three-column Discord-style window. Returns the
// root container; SetContent attaches it to the window.
func (g *GUI) buildLayout() fyne.CanvasObject {
	// ── Far-left rail: mode selector ───────────────────────────────────
	// Discord shows a vertical strip of server icons here. We have one
	// active mode ("FT8") and one greyed-out future mode ("FT4"); chips
	// rather than icons keep the layout cheap and obvious.
	ft8Chip := chip("FT8", color.RGBA{88, 101, 242, 255}) // Discord blurple
	ft4Chip := chip("FT4", color.RGBA{60, 60, 60, 255})   // disabled grey
	modeRail := container.NewVBox(ft8Chip, ft4Chip)
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

	chanCol := container.NewBorder(
		container.NewPadded(chanHeader), userBarStack, nil, nil,
		g.bandList,
	)
	chanBg := canvas.NewRectangle(color.RGBA{47, 49, 54, 255})
	chanCol = container.NewStack(chanBg, chanCol)
	g.refreshUserBar()

	// ── Chat pane: header + scrollable history + input ─────────────────
	g.statusText = canvas.NewText("UTC --:--:--", color.RGBA{170, 175, 185, 255})
	g.statusText.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	g.statusText.TextSize = 9
	header := container.NewPadded(g.statusText)
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
			meta := canvas.NewText("", color.RGBA{130, 135, 145, 255})
			meta.TextStyle = fyne.TextStyle{Monospace: true}
			meta.TextSize = 8
			msg := canvas.NewText("", color.RGBA{220, 220, 222, 255})
			msg.TextStyle = fyne.TextStyle{Monospace: true}
			msg.TextSize = 10
			textRow := container.NewHBox(ts, meta, msg)
			replyBtn := widget.NewButtonWithIcon("", theme.MailReplyIcon(), nil)
			replyBtn.Importance = widget.LowImportance
			replyBtn.Hide()
			// Action area: a fixed-width strip on the right side of the
			// row. Each potential button has a reserved slot so adding a
			// second action later (e.g. "log QSO", "ignore") doesn't
			// shift the reply button around. Slot 0 = rightmost.
			actions := container.New(&chatActionsLayout{slotWidth: 28, slots: 1}, replyBtn)
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
			ts := textRow.Objects[0].(*canvas.Text)
			meta := textRow.Objects[1].(*canvas.Text)
			msg := textRow.Objects[2].(*canvas.Text)
			g.mu.Lock()
			if id >= len(g.rows) {
				g.mu.Unlock()
				return
			}
			r := g.rows[id]
			g.mu.Unlock()
			tsText, metaText, msgText := formatRowSegments(r)
			ts.Text = tsText
			meta.Text = metaText
			msg.Text = msgText
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
				msg.Color = color.RGBA{255, 200, 80, 255}
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
			// Right-click any non-system / non-tx / non-separator row
			// with a recognisable callsign to open the operator profile.
			if !r.system && !r.tx && !r.separator {
				if remote := senderFromMessage(r.text); remote != "" && !strings.HasPrefix(remote, "<") {
					rowIsCQ := isCQ
					h.onSecondary = func(pos fyne.Position) {
						g.showCallContextMenu(remote, rowIsCQ, pos)
					}
				} else {
					h.onSecondary = nil
				}
			} else {
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
		g.mu.Unlock()
		g.chatList.UnselectAll()
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

	g.input = widget.NewEntry()
	g.input.SetPlaceHolder("Type CQ, a callsign, or /tune; then Enter")
	g.input.OnSubmitted = func(s string) {
		g.handleSubmit(s)
		g.input.SetText("")
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
	inputRow := container.NewBorder(nil, nil, nil, g.sendBtn, g.input)
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
			// Three-segment row: tiny green "CQ" tag, country flag, and
			// "CALL SNR" text. The whole row is wrapped in a hoverRow
			// (highlights the matching map pin / waterfall box on
			// hover, opens the context menu on right-click). The
			// flag-only slot is wrapped in its OWN hoverRow so the
			// country tooltip ONLY appears when the cursor is actually
			// over the flag — keeps the tooltip from popping for
			// every cursor move across the row text.
			cq := canvas.NewText("CQ", color.RGBA{80, 200, 120, 255})
			cq.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
			cq.TextSize = 10
			cq.Hide()
			cqSlot := container.New(&fixedWidthLayout{width: 22}, cq)
			flagText := canvas.NewText("", color.RGBA{220, 225, 235, 255})
			flagText.TextSize = 14
			flagText.Alignment = fyne.TextAlignCenter
			flagInner := container.New(&fixedWidthLayout{width: 24}, container.NewCenter(flagText))
			flagSlot := newHoverRow(flagInner)
			t := canvas.NewText("", color.RGBA{210, 215, 225, 255})
			t.TextStyle = fyne.TextStyle{Monospace: true}
			t.TextSize = 10
			return newHoverRow(container.NewHBox(cqSlot, flagSlot, t))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			h := obj.(*hoverRow)
			row := h.inner.(*fyne.Container)
			cqSlot := row.Objects[0].(*fyne.Container)
			cq := cqSlot.Objects[0].(*canvas.Text)
			flagSlot := row.Objects[1].(*hoverRow)
			flagInner := flagSlot.inner.(*fyne.Container)
			flagText := flagInner.Objects[0].(*fyne.Container).Objects[0].(*canvas.Text)
			t := row.Objects[2].(*canvas.Text)
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
			flag := callsign.Flag(e.call)
			if flag == "" {
				// Fall back to country code for entities without a
				// real ISO-3166 trailer (Hawaii, Alaska, etc.).
				if sc := callsign.ShortCode(e.call); len(sc) >= 2 {
					flag = sc[len(sc)-2:]
				} else {
					flag = "  "
				}
			}
			flagText.Text = flag
			flagText.Refresh()
			if !e.lastCQ.IsZero() && time.Since(e.lastCQ) <= 30*time.Second {
				cq.Show()
			} else {
				cq.Hide()
			}
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
		snap := g.heardSnapshot()
		if id >= len(snap) {
			return
		}
		call := snap[id].call
		g.usersList.UnselectAll()
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
	usersInner := container.NewStack(usersBg, container.NewBorder(usersHdrStack, nil, nil, nil, g.usersList))
	// Force a fixed pixel width via a custom layout -Stack.MinSize from a
	// background rectangle doesn't reliably propagate up through Border's
	// east slot, leading the column to collapse and bleed visually into
	// the neighbouring HSplit pane.
	usersCol := container.New(&fixedWidthLayout{width: 170}, usersInner)

	chatCenter := container.NewBorder(headerStack, inputStack, nil, nil, g.chatList)
	chatCol := container.NewBorder(nil, nil, nil, usersCol, chatCenter)
	chatBg := canvas.NewRectangle(color.RGBA{40, 43, 48, 255})
	chatColStack := container.NewStack(chatBg, chatCol)

	// ── Scope column on the far right (waterfall + map) ────────────────
	// Map centres on the operator's grid if it's been set; otherwise the
	// MapWidget falls back to a default mid-North-America view.
	g.mu.Lock()
	myGrid := g.myGrid
	g.mu.Unlock()
	g.scope = newScopePane(myGrid)

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
	// Load both nocordhf.adif (our own future writes) and any pre-
	// existing ft8m8.adif from the legacy GUI in the same working
	// directory. Means the operator's full QSO history lights up the
	// map overlay on first launch, instead of starting blank until
	// they finish their first NocordHF contact. De-dup is unnecessary
	// for overlay purposes (worked-grids is a set).
	for _, path := range []string{"nocordhf.adif", "ft8m8.adif"} {
		if recs, err := adif.Read(path); err == nil && len(recs) > 0 {
			g.adifLog = append(g.adifLog, recs...)
		}
	}
	g.qso.SetOnLogged(func(rec adif.Record) {
		// Persist + update in-memory log + refresh map overlay.
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
	// Right-click a station dot on the map → same context menu as
	// the chat / HEARD / waterfall surfaces.
	g.scope.mapWidget.SetOnSpotSecondaryTap(func(call string, absPos fyne.Position) {
		g.showCallContextMenu(call, g.callIsCQ(call), absPos)
	})

	// Wire decode-box interactions: double-click → scroll chat;
	// hover → magnified signal popup; right-click → operator profile.
	// Hover handlers stay nil for now until the popup widget below is
	// constructed; double-click + context are safe to wire up front.
	g.scope.SetDecodeHooks(
		g.scrollChatToDecode,
		func(call string, slotStart time.Time, freqHz float64, _ float64, pos fyne.Position) {
			g.showDecodePopup(call, slotStart, freqHz, pos)
		},
		func() { g.hideDecodePopup() },
		func(call string, pos fyne.Position) {
			g.showCallContextMenu(call, g.callIsCQ(call), pos)
		},
		g.selectCall,
	)

	// ── Compose four columns horizontally ──────────────────────────────
	// mode rail | channels | chat | scope
	leftPair := container.NewBorder(nil, nil, modeCol, nil, chanCol)
	// chat + scope split: HSplit so the operator can drag the boundary
	// to give one or the other more room. Default 0.62 leaves the scope
	// column ~38% of the right pane (~400 px on a 1100 px wide window).
	chatScope := container.NewHSplit(chatColStack, g.scope.container)
	chatScope.SetOffset(0.62)
	root := container.NewBorder(nil, nil, leftPair, nil, chatScope)

	// Force the column widths to feel like Discord (mode rail thin,
	// channel column moderate, chat takes the rest).
	modeCol.Resize(fyne.NewSize(56, 720))
	modeBg.SetMinSize(fyne.NewSize(56, 0))
	chanCol.Resize(fyne.NewSize(180, 720))
	chanBg.SetMinSize(fyne.NewSize(180, 0))

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
//	"CQ"               → send "CQ <mycall> <mygrid>"
//	"<CALLSIGN>"       → start a directed call to <CALLSIGN>
//	"" (empty)         → ignore
//
// Anything else is rejected with an inline system message -keeps the user
// from accidentally transmitting free-form text on a digital QSO band.
func (g *GUI) handleSubmit(raw string) {
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
	switch {
	case s == "CQ":
		req := TxRequest{Callsign: myCall, Grid: myGrid, StopCh: make(chan struct{})}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("queued: CQ %s %s", myCall, myGrid))
		default:
			g.AppendSystem("!TX queue full -try again")
		}
	case isPlausibleCallsign(s):
		req := TxRequest{Callsign: myCall, Grid: myGrid, RemoteCall: s, StopCh: make(chan struct{})}
		select {
		case g.txCh <- req:
			g.AppendSystem(fmt.Sprintf("queued: directed call to %s", s))
		default:
			g.AppendSystem("!TX queue full -try again")
		}
	default:
		g.AppendSystem(fmt.Sprintf("!%q is not \"CQ\", a valid callsign, or /tune -input rejected", raw))
	}
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

// formatRowSegments splits a chat row into (timestamp, meta, message).
// Timestamp renders at the chat font size so the operator can scan
// times normally; meta (SNR + GEO) renders smaller and dimmer so it
// recedes; message keeps full chat-font weight. System rows leave both
// timestamp and meta empty; TX rows fill timestamp + tag in meta.
func formatRowSegments(r chatRow) (ts, meta, msg string) {
	if r.separator {
		// Long em-dash run renders as a thin horizontal break without
		// requiring a special row template.
		return "", "", strings.Repeat("─", 80)
	}
	if r.system {
		return "", "", fmt.Sprintf("* %s", r.text)
	}
	t := r.when.UTC().Format("15:04:05")
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

// chip renders a small filled rectangle with a label -used for the mode rail.
func chip(label string, fill color.Color) *fyne.Container {
	bg := canvas.NewRectangle(fill)
	bg.CornerRadius = 8
	t := canvas.NewText(label, color.White)
	t.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	t.TextSize = 11
	t.Alignment = fyne.TextAlignCenter
	return container.NewStack(bg, container.NewPadded(t))
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
		// "CQ MOD X GRID" or "CQ X GRID"
		if len(fields) >= 3 && (fields[1] == "DX" || fields[1] == "POTA" || fields[1] == "SOTA" || fields[1] == "TEST") {
			return fields[1+1]
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
}

var _ fyne.SecondaryTappable = (*hoverRow)(nil)

func (h *hoverRow) TappedSecondary(ev *fyne.PointEvent) {
	if h.onSecondary != nil {
		h.onSecondary(ev.AbsolutePosition)
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
