package nocord

// scope.go — the rightmost column of the NocordHF window: waterfall on top,
// map on bottom, separated by a draggable horizontal divider that scales
// both panes proportionally. NOT a separate OS window — embedded as the
// fourth column of the main layout (mode rail / channels / chat / scope).
//
// Operator can drag the divider up to give the map more room or down to
// give the waterfall more room. Closing/hiding requires zero plumbing —
// the column is just another container.CanvasObject.

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"github.com/kyleomalley/nocordhf/lib/ft8"
	ui "github.com/kyleomalley/nocordhf/lib/mapview"
	"github.com/kyleomalley/nocordhf/lib/waterfall"
)

const (
	scopeWFWidth = 400

	// rowsPerSlot — at 12 kHz sample rate / 512 stride the waterfall
	// processor emits ~23.4375 rows/sec; one FT8 slot is 15 s ≈ 352 rows.
	// We round to 360 so an integer number of slots map cleanly to image
	// rows; this is the *data* resolution per slot.
	rowsPerSlot = 360

	// Per-slot display-height range. The waterfall scales each slot
	// continuously between slotPxMin and slotPxMax to fill the pane; once
	// the pane has room for another slot at slotPxMin it snaps to one
	// more slot rather than stretching past slotPxMax. Net feel: smooth
	// scaling within a slot count, hard snap when adding a slot.
	slotPxMin = 110
	slotPxMax = 200

	// Snap-to-slots range: the waterfall always displays an integer number
	// of FT8 slots, snapped from the live pane height. Going below 2 makes
	// the slot timeline useless; above 12 the history runs out anyway.
	minDisplayedSlots = 2
	maxDisplayedSlots = 12

	scopeFreqMaxHz = 3600.0 // X axis spans 0..3600 Hz across scopeWFWidth pixels

	// FT8 occupies 8 tones × 6.25 Hz = 50 Hz of spectrum. Drawing a TX
	// indicator that's a box of this width gives the operator a visually
	// honest representation of what they'll occupy when they key —
	// matching what they see for received signals.
	scopeFT8BWHz = 50.0

	// freqAxisHeight is the docked frequency-axis strip's vertical
	// extent (replaces the old SCOPE header). Tall enough for one
	// line of tick labels and a TX marker line below them.
	freqAxisHeight = 28

	// Time strip on the left of the waterfall: dedicated narrow column
	// that holds crisp canvas.Text timestamps positioned by the layout to
	// align with each slot boundary in the rasterised image.
	scopeTimeStripWidth = 64

	// maxDecodeRects bounds the decode-box overlay's rectangle pool. The
	// pool is allocated once at construction; Layout only repositions /
	// shows / hides entries — it MUST NOT call Add or Remove, since
	// Fyne re-runs Layout in response and that would recurse to a stack
	// overflow. Capacity is comfortably above any realistic per-window
	// decode count (typical band: ≤ 30 simultaneous signals × a few
	// visible slots).
	maxDecodeRects = 256
)

// scopePane holds the waterfall + map column. Built once per GUI; main feeds
// the waterfall by calling SetWaterfallRow as the audio capturer produces
// rows, and AddSpots as decoded stations land. Reuses ui.MapWidget — the
// same map widget the legacy nocordhf GUI uses, which gives us tile fetch,
// drag/zoom, spot pins, hover-info, and worked-grid overlay for free.
// slotBoundary records where a 15-second UTC slot start was painted in the
// waterfall image, in image-row coordinates (0=top, wfHeight-1=bottom).
// Each new waterfall row scrolls every boundary's row index up by one;
// boundaries that scroll off the top are dropped.
type slotBoundary struct {
	row  int
	when time.Time
}

// pendingDecode is a decode whose hollow box hasn't been finalised yet
// because we're waiting for the slot to fully scroll past — only at the
// next slot boundary do we know the full row range the signal occupied.
type pendingDecode struct {
	freqHz    float64
	call      string
	snr       float64
	slotStart time.Time // explicit slot-start so flushing late doesn't mis-key
}

// finalizedDecode is a decoded signal that has been promoted from pending
// into the live overlay set. Position is keyed to the slot's start time
// (UTC) — at render time we look up that slot in the current boundaries
// list to figure out its current pixel-row range, so the box scrolls in
// lockstep with the waterfall image. Boxes drop off when their slot has
// scrolled off the bottom of the visible waterfall.
type finalizedDecode struct {
	slotStart time.Time
	freqHz    float64
	call      string // sender, used to highlight the box on HEARD-row hover
}

type scopePane struct {
	mu        sync.Mutex
	wfImg     *image.RGBA
	wfCanvas  *canvas.Image
	mapWidget *ui.MapWidget
	container *fyne.Container // root — added to the main layout (NewStack of bg + child)
	bg        *canvas.Rectangle
	// wfWithAxis is the waterfall + freq-axis pair that becomes the
	// VSplit's leading object in FT8 mode. wfMapSplit is the live
	// VSplit; rebuilt on every flip back to FT8 mode (Fyne doesn't
	// reattach a Split's Trailing cleanly after we reparent it).
	wfWithAxis *fyne.Container
	wfMapSplit *container.Split

	// Snap-to-slots state. The waterfall image is sized to an integer number
	// of FT8 slots and rebuilt when the live pane height crosses a slot
	// boundary. wfHeight is always displayedSlots * rowsPerSlot.
	displayedSlots int
	wfHeight       int

	// Row history (newest at front). Populated by SetWaterfallRow; consumed
	// when the image is resized to repaint the visible window. Capped at
	// maxDisplayedSlots*rowsPerSlot rows so memory stays bounded.
	history []waterfall.Row

	// Slot-boundary book-keeping. boundaries holds at most a handful of
	// recent boundary positions in image-row coords; lastSlotSec tracks
	// the most recent UTC second we recognised as a 15-s boundary so we
	// don't paint duplicates while sitting on the same boundary row.
	boundaries  []slotBoundary
	lastSlotSec int64

	// Time-strip overlay: a container holding one canvas.Text per visible
	// slot boundary, positioned by timeStripLayout. Crisp at any pane
	// height — no rasterised text to squish.
	timeStrip       *fyne.Container
	timeStripLayout *timeStripLayout

	// Decodes from the most recently completed slot, drawn as hollow
	// boxes when the next slot's boundary rolls in (so the box covers
	// the whole signal range, not just the bottom few rows).
	pending []pendingDecode

	// Finalised decodes from past slots, drawn as canvas.Rectangle
	// overlays (not baked into the waterfall image). Each entry's pixel
	// position is recomputed every layout pass from the live boundaries
	// list so the boxes scroll in lockstep with the waterfall and stay
	// crisp at any zoom.
	decodes       []finalizedDecode
	decodeOverlay *fyne.Container
	decodeBoxPool []*decodeBox

	// Hooks fired by interactive decodeBox widgets. Set by the GUI so
	// the scope package stays free of GUI-package types.
	//
	// onDecodeSelect fires on a single-click that landed on a decode
	// box; arg is the call. The GUI uses it to show the magnification
	// popup at the click position and to scroll/highlight the chat row.
	//
	// onDecodeDeselect fires on a single-click that landed on EMPTY
	// waterfall. Used by the GUI to dismiss the magnification popup.
	//
	// onDecodeHover fires when the cursor moves onto a decode box
	// (and again, with a different decode, when it moves to a
	// different one). onDecodeHoverEnd fires when the cursor leaves
	// all decode boxes. The GUI uses these for the live "preview"
	// popup that follows the cursor when no popup is pinned; clicks
	// pin a popup, after which hover events are ignored by the GUI.
	onDecodeDoubleTap func(slotStart time.Time, call string)
	onDecodeContext   func(call string, screenPos fyne.Position)
	onDecodeSelect    func(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position)
	onDecodeDeselect  func()
	onDecodeHover     func(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position)
	onDecodeHoverEnd  func()

	// highlightCall identifies a callsign whose decode boxes should
	// render in a brighter highlight colour. Set when the operator
	// hovers a HEARD-list row. highlightClear debounces the
	// "clear-on-MouseOut" path so a rapid leave-then-enter (Fyne list
	// re-binding the row template, cursor jitter on row boundaries)
	// doesn't drop and re-add the highlight visibly.
	highlightCall  string
	highlightClear *time.Timer

	// Hover state: the (call, slot, freq) tuple under the cursor right
	// now, used to paint the hovered box in a soft highlight as visual
	// feedback. Distinct from highlightCall (HEARD-row hover, applies
	// to every box for that call) and from selectedCall (click-active,
	// stronger highlight + fill).
	hoverCall      string
	hoverSlotStart time.Time
	hoverFreqHz    float64

	// Selection state (click-to-pick). One specific decode box is
	// "active" at a time and renders in a brighter yellow with a
	// saturated fill so the operator can see at a glance which decode
	// the magnification popup is showing.
	selectedCall      string
	selectedSlotStart time.Time
	selectedFreqHz    float64

	// TX-bandwidth indicator: a translucent box matching FT8's 50 Hz
	// occupied spectrum, centred on the operator's selected TX freq.
	// Operator clicks the waterfall to move it; the live value is what
	// the encoder picks up at TX time. The box lives inside an overlay
	// container with a custom Layout that recomputes pixel geometry from
	// the live container size — so the box auto-tracks every VSplit drag
	// and window resize without any manual refresh plumbing.
	txFreqHz       float64
	txBox          *canvas.Rectangle
	txOverlay      *fyne.Container
	wfWithOverlay  fyne.CanvasObject // the tappable+overlay stack added to VSplit
	onTxFreqChange func(hz float64)  // callback for main.go to read latest TX freq

	// Frequency-axis strip docked under the waterfall. Replaces the
	// old "SCOPE | TX 1234 Hz" header. Shows tick labels at 500-Hz
	// intervals so the operator can read the X-axis like a graph,
	// plus a triangle + label marking the live TX frequency.
	freqAxis      *fyne.Container
	freqAxisMark  *canvas.Text      // moves with txFreqHz, shows "1234 Hz"
	freqAxisCaret *canvas.Rectangle // small caret beneath the marker
}

// newScopePane constructs the embedded scope column. The returned
// CanvasObject is what the main layout puts as its fourth column.
// myGrid is used to centre the initial map view on the operator's location.
func newScopePane(myGrid string) *scopePane {
	initialSlots := minDisplayedSlots
	s := &scopePane{
		mapWidget:      ui.NewMapWidget(myGrid),
		lastSlotSec:    -1,
		displayedSlots: initialSlots,
		wfHeight:       initialSlots * rowsPerSlot,
	}
	s.wfImg = image.NewRGBA(image.Rect(0, 0, scopeWFWidth, s.wfHeight))
	for i := range s.wfImg.Pix {
		if i%4 == 3 {
			s.wfImg.Pix[i] = 255 // alpha
		}
	}
	// Seed past-slot boundaries so the grid renders immediately, even
	// before any audio rows arrive. Without this the operator opens the
	// app to a blank black pane until a full slot's worth of FFTs have
	// been processed (~15 s).
	s.seedPastBoundariesLocked(time.Now().UTC())

	s.wfCanvas = canvas.NewImageFromImage(s.wfImg)
	s.wfCanvas.FillMode = canvas.ImageFillStretch
	s.wfCanvas.SetMinSize(fyne.NewSize(scopeWFWidth, float32(slotPxMin*minDisplayedSlots)))

	// Time-strip overlay: holds canvas.Text labels positioned by a custom
	// layout that maps each boundary's pixel row to its Y in the strip.
	// Background rectangle keeps the column visually distinct even when
	// empty.
	stripBg := canvas.NewRectangle(color.RGBA{22, 24, 28, 255})
	s.timeStripLayout = &timeStripLayout{scope: s, bg: stripBg}
	s.timeStrip = container.New(s.timeStripLayout, stripBg)

	// TX-bandwidth overlay: a 50-Hz-wide box (the actual FT8 occupied
	// spectrum) centred on the operator's TX freq. Semi-transparent red
	// fill with crisper border so it reads against any waterfall
	// background. Default 1500 Hz (FT8 audio centre); operator clicks
	// the waterfall to retune.
	s.txFreqHz = 1500.0
	s.txBox = canvas.NewRectangle(color.RGBA{255, 80, 80, 80})
	s.txBox.StrokeColor = color.RGBA{255, 120, 120, 230}
	s.txBox.StrokeWidth = 1
	// Custom layout: Fyne calls Layout() with the live container size on
	// every parent resize, so the box geometry is always recomputed from
	// the current pixel/Hz ratio — no manual Refresh wiring required.
	s.txOverlay = container.New(&txOverlayLayout{scope: s}, s.txBox)

	// Decode-box overlay: a fixed-size pool of interactive decodeBox
	// widgets (maxDecodeRects). Layout repositions / shows / hides
	// entries from s.decodes — it MUST NOT call Add or Remove, since
	// Fyne re-runs Layout in response, which would recurse forever.
	s.decodeBoxPool = make([]*decodeBox, maxDecodeRects)
	overlayChildren := make([]fyne.CanvasObject, maxDecodeRects)
	for i := range s.decodeBoxPool {
		b := newDecodeBox(s)
		s.decodeBoxPool[i] = b
		overlayChildren[i] = b
	}
	s.decodeOverlay = container.New(&decodeOverlayLayout{scope: s}, overlayChildren...)

	// Snap-to-slots layout: places the time strip on the west and the
	// waterfall + tx overlay on the east, both sized to the snapped
	// integer-slot height so labels and rule lines stay aligned no matter
	// how tall the parent pane gets.
	overlayStack := container.NewStack(s.wfCanvas, s.decodeOverlay, s.txOverlay)
	tappable := newWaterfallTap(overlayStack, nil)
	// Click model:
	//   - Single click on a decode box → "select" it: pin the
	//     magnification popup + open a context menu so the operator
	//     can act on it (Profile / Reply / Copy / QRZ).
	//   - Single click on empty waterfall → no-op.
	//   - Double click anywhere → tune TX freq to that cursor x.
	//   - Right click on a decode box → context menu.
	//   - Hover → soft yellow highlight on the box under the cursor;
	//     no popup, no GUI callback. Pure visual feedback so the
	//     operator can see what their cursor is hitting before they
	//     commit to a click.
	//   - Single click on a decode box → selection (strong yellow +
	//     fill) + GUI callback that opens the magnification popup at
	//     the click position and scrolls/highlights the chat row.
	//   - Single click on empty waterfall → clear selection + dismiss
	//     the popup.
	//
	// Routing all pointer interactions through the stable waterfallTap
	// (rather than per-decode widgets that move every audio row) keeps
	// fyne's mouse-event dispatch from pairing down/up events across
	// recycled decodeBox widgets.
	tappable.onSelect = func(local fyne.Position) {
		size := tappable.Size()
		d, ok := s.decodeAtPixel(local, size)
		if !ok {
			s.clearSelection()
			if cb := s.getDecodeDeselectHook(); cb != nil {
				cb()
			}
			return
		}
		s.setSelection(d)
		if cb := s.getDecodeSelectHook(); cb != nil {
			abs := fyne.CurrentApp().Driver().AbsolutePositionForObject(tappable)
			screenPos := fyne.NewPos(abs.X+local.X+12, abs.Y+local.Y+12)
			cb(d.call, d.slotStart, d.freqHz, screenPos)
		}
	}
	// Shared "x → TX freq" handler used by both double-click (snap-
	// once) and drag (continuous sweep). Centralised so the two
	// gestures stay in lockstep on bounds + decode-box guard.
	setTxFromX := func(local fyne.Position) {
		size := tappable.Size()
		if size.Width <= 0 {
			return
		}
		// Don't retune TX frequency when the gesture landed on a
		// decode box. Operators were accidentally moving their TX
		// freq while trying to interact with a station they'd
		// selected — a fast double-click on the box looks identical
		// to "double-click empty water to retune" without this
		// check, and the same applies to drag-starts that originate
		// inside a box.
		if _, ok := s.decodeAtPixel(local, size); ok {
			return
		}
		xFrac := float64(local.X / size.Width)
		if xFrac < 0 {
			xFrac = 0
		}
		if xFrac > 1 {
			xFrac = 1
		}
		s.SetTxFreq(xFrac * scopeFreqMaxHz)
	}
	// Double-tap and drag both retune the TX cursor without touching
	// queued / in-flight TX state — moving around the waterfall is
	// just frequency selection, not a takeover gesture. Esc is the
	// only explicit cancel.
	tappable.onDouble = setTxFromX
	tappable.onDrag = setTxFromX
	tappable.onSecondary = func(local fyne.Position, abs fyne.Position) {
		size := tappable.Size()
		if d, ok := s.decodeAtPixel(local, size); ok {
			if cb := s.getDecodeContextHook(); cb != nil {
				cb(d.call, abs)
			}
		}
	}
	tappable.onHover = func(local fyne.Position) {
		size := tappable.Size()
		d, ok := s.decodeAtPixel(local, size)
		if !ok {
			s.setHover(finalizedDecode{})
			if cb := s.getDecodeHoverEndHook(); cb != nil {
				cb()
			}
			return
		}
		// Box-highlight feedback (pure visual, scope-internal).
		s.setHover(d)
		// Live preview hook for the GUI: it positions the popup near
		// the box and updates content as the cursor moves between
		// boxes. The GUI ignores this when a popup is pinned.
		if cb := s.getDecodeHoverHook(); cb != nil {
			abs := fyne.CurrentApp().Driver().AbsolutePositionForObject(tappable)
			screenPos := fyne.NewPos(abs.X+local.X+12, abs.Y+local.Y+12)
			cb(d.call, d.slotStart, d.freqHz, screenPos)
		}
	}
	tappable.onHoverEnd = func() {
		s.setHover(finalizedDecode{})
		if cb := s.getDecodeHoverEndHook(); cb != nil {
			cb()
		}
	}
	s.wfWithOverlay = container.New(&waterfallSnapLayout{scope: s}, s.timeStrip, tappable)

	// Frequency-axis strip docked under the waterfall. Replaces the
	// old "SCOPE | TX 1234 Hz" header — same vertical real estate
	// but now serves the X axis: tick labels at 500-Hz intervals
	// across the visible range, plus a marker that follows the live
	// TX freq. The strip's children are managed by freqAxisLayout
	// which positions everything from the live container width.
	axisBg := canvas.NewRectangle(color.RGBA{22, 24, 28, 255})
	axisBg.SetMinSize(fyne.NewSize(0, freqAxisHeight))
	tickLabels := []*canvas.Text{}
	for hz := 500; hz <= int(scopeFreqMaxHz); hz += 500 {
		t := canvas.NewText(fmt.Sprintf("%d", hz), color.RGBA{140, 145, 155, 255})
		t.TextStyle = fyne.TextStyle{Monospace: true}
		t.TextSize = 9
		t.Alignment = fyne.TextAlignCenter
		tickLabels = append(tickLabels, t)
	}
	mark := canvas.NewText(fmt.Sprintf("%.0f Hz", s.txFreqHz), color.RGBA{255, 200, 80, 255})
	mark.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
	mark.TextSize = 10
	mark.Alignment = fyne.TextAlignCenter
	s.freqAxisMark = mark
	caret := canvas.NewRectangle(color.RGBA{255, 200, 80, 255})
	s.freqAxisCaret = caret
	axisChildren := []fyne.CanvasObject{axisBg}
	for _, t := range tickLabels {
		axisChildren = append(axisChildren, t)
	}
	axisChildren = append(axisChildren, caret, mark)
	s.freqAxis = container.New(&freqAxisLayout{scope: s, ticks: tickLabels}, axisChildren...)

	// Stack waterfall on top + axis strip on bottom, then VSplit
	// between that pair and the map. The axis strip rides with the
	// waterfall through the VSplit drag; the map gets the bottom.
	s.wfWithAxis = container.NewBorder(nil, s.freqAxis, nil, nil, s.wfWithOverlay)
	s.wfMapSplit = container.NewVSplit(s.wfWithAxis, s.mapWidget)
	s.wfMapSplit.SetOffset(0.55) // slightly more waterfall by default

	s.bg = canvas.NewRectangle(color.RGBA{30, 32, 38, 255})
	s.container = container.NewStack(s.bg, s.wfMapSplit)
	return s
}

// SetWaterfallVisible swaps the right-pane layout between the full
// waterfall+map VSplit (FT8 mode) and a map-only view (MeshCore mode).
// Called by the GUI on mode-rail selection. The map widget itself is
// the same instance in both layouts — we just reparent it between the
// VSplit and the bare stack so worked-grid overlay state, spot pins,
// QSO partner arc, etc. all carry through a mode flip.
func (s *scopePane) SetWaterfallVisible(show bool) {
	if s == nil || s.container == nil {
		return
	}
	if show {
		// Rebuild the VSplit with the same children — Fyne doesn't
		// like reusing a Split whose Trailing was reparented, so a
		// fresh Split is the cleanest way back from map-only.
		s.wfMapSplit = container.NewVSplit(s.wfWithAxis, s.mapWidget)
		s.wfMapSplit.SetOffset(0.55)
		s.container.Objects = []fyne.CanvasObject{s.bg, s.wfMapSplit}
	} else {
		s.container.Objects = []fyne.CanvasObject{s.bg, s.mapWidget}
	}
	s.container.Refresh()
}

// SetMeshcoreLayout swaps the right pane into an RxLog-on-top +
// map-on-bottom VSplit. The RxLog sits where the waterfall lives
// in FT8 mode (top of the scope pane); the map takes the larger
// bottom panel. Pass nil for top to fall back to a bare map.
// The "top" arg is conventionally the packet log but the helper
// stays generic so future surfaces can swap in.
func (s *scopePane) SetMeshcoreLayout(top fyne.CanvasObject) {
	if s == nil || s.container == nil {
		return
	}
	if top == nil {
		s.container.Objects = []fyne.CanvasObject{s.bg, s.mapWidget}
	} else {
		split := container.NewVSplit(top, s.mapWidget)
		// Top pane gets ~35% (compact log header), map ~65%
		// (matches the FT8 waterfall:map weighting feel and
		// gives geographic context plenty of room).
		split.SetOffset(0.35)
		s.container.Objects = []fyne.CanvasObject{s.bg, split}
	}
	s.container.Refresh()
}

// AddSpots plots decoded stations as pins on the map. Forwards directly to
// the underlying ui.MapWidget which already has all the parsing /
// grid-resolution / pin-rendering logic. myCall is used to suppress our
// own loopback decodes from showing up as foreign spots.
func (s *scopePane) AddSpots(results []ft8.Decoded, myCall string) {
	if s.mapWidget == nil {
		return
	}
	s.mapWidget.AddSpots(results, myCall)
}

// SetMyGrid recentres the map on the operator's location after a profile
// change. Operator's home pin and bearing-from-home calculations rely on
// this being current.
func (s *scopePane) SetMyGrid(grid string) {
	if s.mapWidget == nil {
		return
	}
	s.mapWidget.UpdateMyGrid(grid)
}

// Reset clears all band-specific scope state — row history, decode
// boxes, slot boundaries, and the rendered waterfall image — and re-
// seeds synthetic boundaries from the current wall-clock so the slot
// grid stays visible. Called when the operator switches channels so
// the new band's display isn't contaminated with audio from the
// previous one.
func (s *scopePane) Reset() {
	s.mu.Lock()
	s.history = nil
	s.boundaries = s.boundaries[:0]
	s.decodes = s.decodes[:0]
	s.pending = s.pending[:0]
	s.lastSlotSec = -1
	// Wipe the rendered image (alpha kept at 255 for the black bg).
	if s.wfImg != nil {
		for i := range s.wfImg.Pix {
			if i%4 == 3 {
				s.wfImg.Pix[i] = 255
			} else {
				s.wfImg.Pix[i] = 0
			}
		}
	}
	s.seedPastBoundariesLocked(time.Now().UTC())
	canvasImg := s.wfCanvas
	timeStrip := s.timeStrip
	overlay := s.decodeOverlay
	s.mu.Unlock()
	if canvasImg != nil {
		fyne.Do(func() {
			canvasImg.Refresh()
			if timeStrip != nil {
				timeStrip.Refresh()
			}
			if overlay != nil {
				overlay.Refresh()
			}
		})
	}
}

// SetTxFreq updates the operator's intended TX frequency (Hz, audio centre)
// and moves the on-waterfall TX-line marker. Click-on-waterfall handler
// also calls this. Notifies main via the OnTxFreqChange callback so the
// encoder picks up the new freq on the next TX.
func (s *scopePane) SetTxFreq(hz float64) {
	if hz < 0 {
		hz = 0
	}
	if hz > scopeFreqMaxHz {
		hz = scopeFreqMaxHz
	}
	s.mu.Lock()
	s.txFreqHz = hz
	cb := s.onTxFreqChange
	overlay := s.txOverlay
	axis := s.freqAxis
	mark := s.freqAxisMark
	s.mu.Unlock()
	fyne.Do(func() {
		if overlay != nil {
			overlay.Refresh()
		}
		if mark != nil {
			mark.Text = fmt.Sprintf("%.0f Hz", hz)
			mark.Refresh()
		}
		// Layout depends on the new mark position; trigger a relayout
		// so the marker + caret slide to the new X.
		if axis != nil {
			axis.Refresh()
		}
	})
	if cb != nil {
		cb(hz)
	}
}

// TxFreq returns the current operator-selected TX frequency in Hz (audio
// centre). main reads this when encoding the next TX.
func (s *scopePane) TxFreq() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txFreqHz
}

// SetOnTxFreqChange registers a callback fired whenever the operator clicks
// the waterfall to retune. Used by main.go to keep its TxOffsetHz in sync.
func (s *scopePane) SetOnTxFreqChange(fn func(hz float64)) {
	s.mu.Lock()
	s.onTxFreqChange = fn
	s.mu.Unlock()
}

// txOverlayLayout positions the TX-bandwidth box from the live overlay
// size. Fyne calls Layout on every parent resize (VSplit drag, window
// resize), so the box pixel width and X position always match the current
// Hz/pixel ratio without any manual refresh plumbing.
type txOverlayLayout struct {
	scope *scopePane
}

func (l *txOverlayLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) == 0 || size.Width <= 0 || size.Height <= 0 {
		return
	}
	box := objs[0]
	l.scope.mu.Lock()
	hz := l.scope.txFreqHz
	l.scope.mu.Unlock()
	hzPerPx := scopeFreqMaxHz / float64(size.Width)
	w := float32(scopeFT8BWHz / hzPerPx)
	if w < 4 {
		w = 4 // minimum legible width on a very narrow pane
	}
	cx := float32(hz/scopeFreqMaxHz) * size.Width
	box.Move(fyne.NewPos(cx-w/2, 0))
	box.Resize(fyne.NewSize(w, size.Height))
}

func (l *txOverlayLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(0, 0) // overlay sizes itself to its parent stack
}

// freqAxisLayout positions the frequency-axis strip's children:
//
//	objs[0]      = background rectangle (covers the full strip)
//	objs[1..N-3] = tick labels (one per 500-Hz tick), pre-ordered
//	objs[N-2]    = TX caret rectangle
//	objs[N-1]    = TX marker text
//
// Tick labels are clamped on the left/right edges so the leftmost and
// rightmost don't get clipped against the strip border. The TX marker
// + caret slide horizontally to track scope.txFreqHz.
type freqAxisLayout struct {
	scope *scopePane
	ticks []*canvas.Text
}

func (l *freqAxisLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 3 || size.Width <= 0 || size.Height <= 0 {
		return
	}
	bg := objs[0]
	bg.Move(fyne.NewPos(0, 0))
	bg.Resize(size)

	// The waterfall above us reserves scopeTimeStripWidth pixels on the
	// left for the slot-time strip; the actual 0..scopeFreqMaxHz axis
	// spans only the area to the RIGHT of that strip. Mirror the same
	// offset here so tick labels and the TX caret line up with the
	// frequency column they refer to. Without this, the axis maps
	// 0..3600 Hz across the full parent width and everything sits ~64
	// px too far left.
	stripW := float32(scopeTimeStripWidth)
	if stripW > size.Width {
		stripW = size.Width
	}
	axisW := size.Width - stripW

	// Tick row sits on the top half, marker on the bottom half. Caret
	// is a 1-px-wide vertical bar between the two so the operator's
	// eye can trace marker → caret tip → waterfall column.
	tickY := float32(0)
	caretTopY := float32(11)
	caretH := float32(5)
	markerY := float32(15)
	pxPerHz := axisW / float32(scopeFreqMaxHz)

	for i, t := range l.ticks {
		// Tick label: centred at its frequency, clamped so the first
		// and last don't fall off the edges.
		hz := float32((i + 1) * 500) // l.ticks[0] = 500 Hz, etc.
		cx := stripW + hz*pxPerHz
		ts := t.MinSize()
		x := cx - ts.Width/2
		if x < stripW {
			x = stripW
		}
		if x+ts.Width > size.Width {
			x = size.Width - ts.Width
		}
		t.Move(fyne.NewPos(x, tickY))
		t.Resize(ts)
	}

	// TX marker + caret. Only valid if both the second-to-last and
	// last objects are the caret + marker — guarded by the slice len
	// check above.
	caret := objs[len(objs)-2]
	mark := objs[len(objs)-1]
	l.scope.mu.Lock()
	hz := l.scope.txFreqHz
	l.scope.mu.Unlock()
	cx := stripW + float32(hz/scopeFreqMaxHz)*axisW
	caret.Move(fyne.NewPos(cx, caretTopY))
	caret.Resize(fyne.NewSize(2, caretH))
	if mt, ok := mark.(*canvas.Text); ok {
		ms := mt.MinSize()
		x := cx - ms.Width/2
		if x < stripW {
			x = stripW
		}
		if x+ms.Width > size.Width {
			x = size.Width - ms.Width
		}
		mark.Move(fyne.NewPos(x, markerY))
		mark.Resize(ms)
	}
}

func (l *freqAxisLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(0, freqAxisHeight)
}

// SetWaterfallRow paints one waterfall row into the scope's RGBA image and
// refreshes the canvas. Each row scrolls the previous content down by one
// pixel (copy rows 0..H-2 → 1..H-1 from the bottom up to avoid overlap,
// then paint the new row into row 0).
//
// Side effects on top of the basic blit:
//   - The row is appended to history so the image can be re-rasterised
//     when the snap-to-slots layout grows or shrinks the slot count.
//   - When the row's UTC second crosses a 15-second slot boundary, the
//     boundary is recorded (timestamps are rendered as crisp text overlays
//     by timeStripLayout) and the just-ended slot's pending decodes are
//     flashed as hollow boxes onto the waterfall.
//
// Safe from any goroutine; widget refresh dispatched via fyne.Do.
func (s *scopePane) SetWaterfallRow(row waterfall.Row) {
	if len(row.Pixels) == 0 {
		return
	}
	s.mu.Lock()

	// Append to history (newest at the front of the slice; trim to a
	// bounded ring so memory stays predictable).
	historyCap := maxDisplayedSlots * rowsPerSlot
	s.history = append([]waterfall.Row{row}, s.history...)
	if len(s.history) > historyCap {
		s.history = s.history[:historyCap]
	}

	wfH := s.wfHeight
	wfStride := s.wfImg.Stride
	// Scroll the image down by one row. Copy from the bottom up so the
	// dst-after-src overlap doesn't clobber unread source rows.
	for y := wfH - 1; y > 0; y-- {
		copy(s.wfImg.Pix[y*wfStride:(y+1)*wfStride], s.wfImg.Pix[(y-1)*wfStride:y*wfStride])
	}
	// Increment existing boundary rows to track scrolling; drop any that
	// have scrolled off the bottom of the image.
	kept := s.boundaries[:0]
	for _, b := range s.boundaries {
		b.row++
		if b.row < wfH {
			kept = append(kept, b)
		}
	}
	s.boundaries = kept

	// Paint new waterfall row at the top. Nearest-neighbor map of
	// 1228 source bins → scopeWFWidth output pixels.
	top := 0
	nSrc := len(row.Pixels)
	for x := 0; x < scopeWFWidth; x++ {
		srcIdx := x * (nSrc - 1) / (scopeWFWidth - 1)
		if srcIdx >= nSrc {
			srcIdx = nSrc - 1
		}
		c := row.Pixels[srcIdx]
		off := top + x*4
		s.wfImg.Pix[off+0] = c.R
		s.wfImg.Pix[off+1] = c.G
		s.wfImg.Pix[off+2] = c.B
		s.wfImg.Pix[off+3] = 255
	}

	// Slot-boundary detection. FT8 slots tick on UTC seconds 0/15/30/45.
	// We mark the first row whose floored 15-s second differs from the
	// last one we recorded.
	if !row.Time.IsZero() {
		slotSec := row.Time.Unix() - row.Time.Unix()%15
		if slotSec != s.lastSlotSec {
			s.lastSlotSec = slotSec
			boundaryTime := time.Unix(slotSec, 0).UTC()
			s.boundaries = append(s.boundaries, slotBoundary{
				row:  0,
				when: boundaryTime,
			})
			// Faint horizontal rule across the waterfall row.
			ruleCol := color.RGBA{180, 180, 200, 110}
			for x := 0; x < scopeWFWidth; x++ {
				off := top + x*4
				s.wfImg.Pix[off+0] = blendChan(s.wfImg.Pix[off+0], ruleCol.R, ruleCol.A)
				s.wfImg.Pix[off+1] = blendChan(s.wfImg.Pix[off+1], ruleCol.G, ruleCol.A)
				s.wfImg.Pix[off+2] = blendChan(s.wfImg.Pix[off+2], ruleCol.B, ruleCol.A)
			}
			// Stamp pending decode boxes from the slot that just ended.
			s.flushPendingBoxesLocked()
		}
	}

	timeStrip := s.timeStrip
	decodeOverlay := s.decodeOverlay
	s.mu.Unlock()
	if s.wfCanvas != nil {
		fyne.Do(func() {
			s.wfCanvas.Refresh()
			if timeStrip != nil {
				timeStrip.Refresh()
			}
			if decodeOverlay != nil {
				decodeOverlay.Refresh()
			}
		})
	}
}

// seedPastBoundariesLocked walks the current waterfall image and stamps
// synthetic slot-boundary marks (rule line + boundary entry) at every row
// that would correspond to a UTC :00/:15/:30/:45 boundary, working
// backwards from `now`. Result: when the app first opens or the pane is
// resized, the slot grid is already drawn across the whole pane instead
// of taking 15 s of live audio to fill in.
//
// Caller must hold s.mu.
func (s *scopePane) seedPastBoundariesLocked(now time.Time) {
	if s.wfHeight <= 0 || s.wfImg == nil {
		return
	}
	// Row 0 (top) is "now"; each subsequent row is one stride older.
	// Seconds per row = 15 s / rowsPerSlot.
	secPerRow := 15.0 / float64(rowsPerSlot)
	stride := s.wfImg.Stride
	ruleCol := color.RGBA{180, 180, 200, 110}

	// Drop any synthetic boundaries we previously seeded; live ones get
	// re-emitted as audio arrives.
	s.boundaries = s.boundaries[:0]

	// Find the most recent boundary at or before `now`.
	nowSec := now.Unix()
	mostRecentBoundarySec := nowSec - nowSec%15
	mostRecentBoundary := time.Unix(mostRecentBoundarySec, 0).UTC()
	// Distance in seconds from `now` back to that boundary.
	dt := now.Sub(mostRecentBoundary).Seconds()
	row := int(dt / secPerRow)

	for row < s.wfHeight {
		boundaryTime := mostRecentBoundary
		s.boundaries = append(s.boundaries, slotBoundary{row: row, when: boundaryTime})
		off := row * stride
		for x := 0; x < scopeWFWidth; x++ {
			p := off + x*4
			s.wfImg.Pix[p+0] = blendChan(s.wfImg.Pix[p+0], ruleCol.R, ruleCol.A)
			s.wfImg.Pix[p+1] = blendChan(s.wfImg.Pix[p+1], ruleCol.G, ruleCol.A)
			s.wfImg.Pix[p+2] = blendChan(s.wfImg.Pix[p+2], ruleCol.B, ruleCol.A)
		}
		mostRecentBoundary = mostRecentBoundary.Add(-15 * time.Second)
		row += rowsPerSlot
	}
	s.lastSlotSec = mostRecentBoundarySec
}

// seedPastBoundariesBelowLocked fills synthetic slot-grid rules into the
// region of the current waterfall image starting at startRow (inclusive),
// working downward to the bottom. Used after a resize when the row
// history doesn't cover the whole image yet — gives the operator a
// uniform slot grid instead of a black tail.
//
// The first synthetic row's timestamp is computed by stepping back from
// `now` by `startRow * secPerRow`, so the synthetic and live grids stay
// aligned at their join.
//
// Caller must hold s.mu.
func (s *scopePane) seedPastBoundariesBelowLocked(startRow int, now time.Time) {
	if s.wfImg == nil || startRow >= s.wfHeight {
		return
	}
	secPerRow := 15.0 / float64(rowsPerSlot)
	stride := s.wfImg.Stride
	ruleCol := color.RGBA{180, 180, 200, 110}

	// Time corresponding to startRow.
	rowTime := now.Add(-time.Duration(float64(startRow) * secPerRow * float64(time.Second)))
	// Most recent UTC :00/:15/:30/:45 boundary at or before rowTime.
	rowSec := rowTime.Unix()
	boundarySec := rowSec - rowSec%15
	boundary := time.Unix(boundarySec, 0).UTC()
	dt := rowTime.Sub(boundary).Seconds()
	row := startRow + int(dt/secPerRow)

	// Skip slot times that already have a live boundary recorded —
	// otherwise the dual entries make decode-overlay row lookups
	// pick the wrong (synthetic) row and boxes land on the wrong
	// vertical band of the waterfall.
	existing := make(map[int64]bool, len(s.boundaries))
	for _, b := range s.boundaries {
		existing[b.when.Unix()] = true
	}
	for row < s.wfHeight {
		if !existing[boundary.Unix()] {
			s.boundaries = append(s.boundaries, slotBoundary{row: row, when: boundary})
			off := row * stride
			for x := 0; x < scopeWFWidth; x++ {
				p := off + x*4
				s.wfImg.Pix[p+0] = blendChan(s.wfImg.Pix[p+0], ruleCol.R, ruleCol.A)
				s.wfImg.Pix[p+1] = blendChan(s.wfImg.Pix[p+1], ruleCol.G, ruleCol.A)
				s.wfImg.Pix[p+2] = blendChan(s.wfImg.Pix[p+2], ruleCol.B, ruleCol.A)
			}
		}
		boundary = boundary.Add(-15 * time.Second)
		row += rowsPerSlot
	}
}

// blendChan does an 8-bit alpha-blend of src over dst with the given alpha.
func blendChan(dst, src, a uint8) uint8 {
	return uint8((int(src)*int(a) + int(dst)*(255-int(a))) / 255)
}

// flushPendingBoxesLocked promotes each queued decode into the finalised
// overlay set, keyed to the slot's UTC start time. Called under
// s.mu.Lock() at every new slot boundary. The decodeOverlayLayout looks
// up the slot's current pixel position on each render so the box scrolls
// with the waterfall — no pixels are baked in.
func (s *scopePane) flushPendingBoxesLocked() {
	if len(s.pending) == 0 {
		return
	}
	// Use each pending decode's own recorded slotStart rather than
	// guessing from the boundary list — the decoder can take longer
	// than one 15-second slot to finish, so by the time we flush the
	// "previous boundary" may already be two slots stale and the
	// box would key to a slot whose history rows are completely
	// different signals.
	for _, d := range s.pending {
		if d.freqHz <= 0 || d.freqHz > scopeFreqMaxHz {
			continue
		}
		slotStart := d.slotStart
		if slotStart.IsZero() && len(s.boundaries) >= 2 {
			slotStart = s.boundaries[len(s.boundaries)-2].when
		}
		s.decodes = append(s.decodes, finalizedDecode{
			slotStart: slotStart,
			freqHz:    d.freqHz,
			call:      strings.ToUpper(d.call),
		})
	}
	s.pending = s.pending[:0]
	// Cap memory: history can hold a fair few slots' worth of decodes,
	// but anything older than the visible window will get culled by the
	// overlay layout anyway. A loose cap keeps the slice from creeping.
	const decodeCap = 256
	if len(s.decodes) > decodeCap {
		s.decodes = s.decodes[len(s.decodes)-decodeCap:]
	}
}

// decodeBox is a PURELY visual widget used in the decode overlay. It
// renders a hollow rectangle (yellow by default, brighter when the
// matching callsign is highlighted) and nothing else.
//
// Earlier this widget owned hover/tap/right-click handlers, but the
// boxes move every audio row (~24×/s) as the waterfall scrolls; fyne's
// pointer-event dispatch can pair a mouse-down on one moved box with a
// mouse-up on a different one, producing flaky right-clicks and hover
// flicker. All pointer interactions for the waterfall now route
// through the parent waterfallTap (which is stable across frames),
// which hit-tests the cursor against the live decode set.
//
// The struct retains call/slotStart/freqHz fields so the layout's
// content updates carry the per-box metadata that hit-testing reads
// back from s.decodes.
type decodeBox struct {
	widget.BaseWidget
	rect      *canvas.Rectangle
	scope     *scopePane
	call      string
	slotStart time.Time
	freqHz    float64
}

func newDecodeBox(scope *scopePane) *decodeBox {
	r := canvas.NewRectangle(color.RGBA{0, 0, 0, 0})
	r.StrokeColor = color.RGBA{255, 245, 120, 220}
	r.StrokeWidth = 1
	b := &decodeBox{rect: r, scope: scope}
	b.ExtendBaseWidget(b)
	b.Hide()
	return b
}

func (b *decodeBox) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(b.rect)
}

// decodeOverlayLayout draws one canvas.Rectangle per finalised decode,
// positioned by looking up the box's slotStart in the current boundaries
// list. Boxes whose slot has scrolled off-screen are skipped (the
// underlying entries get pruned lazily as the boundary list rolls over).
type decodeOverlayLayout struct {
	scope *scopePane
}

func (l *decodeOverlayLayout) Layout(_ []fyne.CanvasObject, size fyne.Size) {
	s := l.scope
	if size.Width <= 0 || size.Height <= 0 {
		return
	}
	s.mu.Lock()
	wfH := s.wfHeight
	if wfH <= 0 {
		s.mu.Unlock()
		return
	}
	// Build a quick lookup: slotStart → row. Synthetic past-boundary
	// seeding can leave duplicate entries for the same slot in the
	// boundaries slice (one synthetic at startup + one live as audio
	// rolled in); pick the smallest row (newest in the image), which
	// is the live one. Boundary rows climb as the waterfall scrolls,
	// so each render uses the live values.
	rowOf := make(map[int64]int, len(s.boundaries))
	for _, b := range s.boundaries {
		key := b.when.Unix()
		if existing, ok := rowOf[key]; !ok || b.row < existing {
			rowOf[key] = b.row
		}
	}
	pxPerRow := size.Height / float32(wfH)
	pxPerHz := size.Width / float32(scopeFreqMaxHz)
	halfHz := float32(scopeFT8BWHz) / 2

	type rect struct {
		x, y, w, h float32
		hot        bool // matches highlightCall — render in highlight colour (HEARD-row hover)
		hovered    bool // cursor is currently over this box
		selected   bool // click-active selection
		d          finalizedDecode
	}
	hot := s.highlightCall
	hovCall := s.hoverCall
	hovSlot := s.hoverSlotStart
	hovFreq := s.hoverFreqHz
	selCall := s.selectedCall
	selSlot := s.selectedSlotStart
	selFreq := s.selectedFreqHz
	// Cull decodes by absolute age rather than by what's currently in
	// s.boundaries. The boundary list shrinks when the pane is small,
	// so culling against it would discard decodes that the operator
	// could still bring back into view by enlarging the waterfall.
	// Hold maxDisplayedSlots+1 slots' worth so a freshly-grown pane
	// re-shows boxes that scrolled out of view earlier.
	keepCutoffSec := time.Now().Unix() - int64(maxDisplayedSlots+1)*15
	rects := make([]rect, 0, len(s.decodes))
	keep := s.decodes[:0]
	for _, d := range s.decodes {
		dSec := d.slotStart.Unix()
		if dSec < keepCutoffSec {
			continue // older than maxDisplayedSlots+1 slots; genuinely stale
		}
		keep = append(keep, d)
		// In the waterfall image, newer audio is at smaller row numbers
		// (top). A boundary recorded at time T appears at the top of the
		// image at time T and scrolls down. So slot T's audio sits
		// BETWEEN boundary T+15 (above, smaller row) and boundary T
		// (below, larger row) — the slot's top row is the NEXT slot's
		// boundary, and the bottom row is its OWN boundary minus 1.
		// Looking up the next slot's boundary positions the box
		// correctly; falling back to topRow = ownBoundary - rowsPerSlot
		// covers the case where the next-slot boundary doesn't exist
		// yet (decode came in before any newer boundary).
		ownRow, ok := rowOf[dSec]
		if !ok {
			continue // not visible this frame, but keep the entry
		}
		topRow := ownRow - rowsPerSlot
		if nextRow, ok := rowOf[dSec+15]; ok {
			topRow = nextRow
		}
		if topRow < 0 {
			topRow = 0
		}
		yTop := float32(topRow) * pxPerRow
		yBot := float32(ownRow) * pxPerRow
		if yBot > size.Height {
			yBot = size.Height
		}
		if yBot-yTop < 2 {
			continue
		}
		// d.freqHz is the LOWEST tone of the FT8 signal (sync-block base);
		// the actual occupied bandwidth is [freqHz, freqHz + scopeFT8BWHz].
		// Centre the box on the midpoint so it lines up with the visible
		// streak rather than appearing shifted ~25 Hz to the left of it.
		cx := float32(d.freqHz+float64(scopeFT8BWHz)/2) * pxPerHz
		x0 := cx - halfHz*pxPerHz
		w := halfHz * 2 * pxPerHz
		if w < 4 {
			w = 4
		}
		matchExact := func(c string, sl time.Time, f float64) bool {
			return c != "" && d.call == c && d.slotStart.Equal(sl) && d.freqHz == f
		}
		rects = append(rects, rect{
			x: x0, y: yTop, w: w, h: yBot - yTop,
			hot:      hot != "" && d.call == hot,
			hovered:  matchExact(hovCall, hovSlot, hovFreq),
			selected: matchExact(selCall, selSlot, selFreq),
			d:        d,
		})
		if len(rects) >= len(s.decodeBoxPool) {
			break // pool exhausted; remaining decodes silently drop this frame
		}
	}
	s.decodes = keep
	pool := s.decodeBoxPool
	s.mu.Unlock()

	// Reposition + rebind handlers on pre-allocated pool entries. Never
	// Add/Remove — that triggers a Fyne layout pass which recurses back
	// into us.
	defaultStroke := color.RGBA{255, 245, 120, 220}
	hotStroke := color.RGBA{255, 235, 60, 255}
	hotFill := color.RGBA{255, 235, 60, 70}
	// Hover: visual feedback only, softer than selected. Lets the
	// operator see what the cursor is over without committing.
	hoverStroke := color.RGBA{255, 245, 120, 255}
	hoverFill := color.RGBA{255, 235, 60, 40}
	// Selected: deliberate click-active highlight, strongest of the
	// three so the operator knows which decode the popup refers to.
	// Wins over hovered + hot when overlapping.
	selectedStroke := color.RGBA{255, 255, 80, 255}
	selectedFill := color.RGBA{255, 235, 60, 130}
	clearFill := color.RGBA{0, 0, 0, 0}
	for i, rc := range rects {
		b := pool[i]
		b.Move(fyne.NewPos(rc.x, rc.y))
		b.Resize(fyne.NewSize(rc.w, rc.h))
		b.rect.Resize(fyne.NewSize(rc.w, rc.h))
		switch {
		case rc.selected:
			b.rect.StrokeColor = selectedStroke
			b.rect.StrokeWidth = 3
			b.rect.FillColor = selectedFill
		case rc.hovered:
			b.rect.StrokeColor = hoverStroke
			b.rect.StrokeWidth = 2
			b.rect.FillColor = hoverFill
		case rc.hot:
			b.rect.StrokeColor = hotStroke
			b.rect.StrokeWidth = 2
			b.rect.FillColor = hotFill
		default:
			b.rect.StrokeColor = defaultStroke
			b.rect.StrokeWidth = 1
			b.rect.FillColor = clearFill
		}
		b.rect.Refresh()
		b.call = rc.d.call
		b.slotStart = rc.d.slotStart
		b.freqHz = rc.d.freqHz
		b.Show()
	}
	for i := len(rects); i < len(pool); i++ {
		pool[i].Hide()
	}
}

func (l *decodeOverlayLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(0, 0)
}

// MagnifiedSignalSlice extracts a small RGBA image showing the waterfall
// pixels that correspond to one decoded slot at one frequency.
//
// The image is returned at the SOURCE pixel resolution — one image
// column per FFT bin in the requested freq window, one image row per
// history row in the slot. Callers are expected to scale it to display
// size via canvas.Image.SetMinSize + ImageFillContain rather than
// pre-stretching here, so Fyne's interpolation produces a clean upscale
// instead of the blocky / stretched look you get by upsampling a small
// source ourselves with nearest-neighbour. The outW / outH parameters
// are accepted for API stability but are no longer authoritative.
//
// Slot location uses the boundaries table (which is what the on-screen
// waterfall image keys off) rather than re-walking history.Time —
// history-row timestamps drift relative to the slot boundary as audio
// buffering shifts, and previously that drift made the popup show the
// wrong slot's audio. The boundary table is authoritative because it's
// what the visible boxes line up with.
func (s *scopePane) MagnifiedSignalSlice(slotStart time.Time, freqHz float64, outW, outH int) *image.RGBA {
	_ = outW
	_ = outH
	slotSec := slotStart.Unix() - slotStart.Unix()%15
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return nil
	}
	// Slot T's audio occupies the rows BETWEEN boundary T+15 (top,
	// smaller row) and boundary T (bottom, larger row) in the waterfall
	// image. Look up both boundaries, preferring smallest row when
	// duplicates exist (synthetic + live).
	ownRow := -1
	for _, b := range s.boundaries {
		if b.when.Unix() != slotSec {
			continue
		}
		if ownRow == -1 || b.row < ownRow {
			ownRow = b.row
		}
	}
	if ownRow < 0 {
		return nil
	}
	topRow := ownRow - rowsPerSlot
	for _, b := range s.boundaries {
		if b.when.Unix() != slotSec+15 {
			continue
		}
		if b.row < topRow || topRow < 0 {
			topRow = b.row
		}
	}
	if topRow < 0 {
		topRow = 0
	}
	if topRow >= len(s.history) {
		return nil
	}
	startIdx := topRow
	endIdx := ownRow - 1
	if endIdx >= len(s.history) {
		endIdx = len(s.history) - 1
	}
	srcRows := endIdx - startIdx + 1
	if srcRows <= 0 {
		return nil
	}
	// Frequency window: ±halfHz around the CENTRE of the FT8 signal.
	// freqHz is the lowest tone (sync-block base); the actual occupied
	// band is [freqHz, freqHz + scopeFT8BWHz], so we centre the window
	// on freqHz + scopeFT8BWHz/2 to match the box drawn on the
	// waterfall. Source bins span 0..waterfall.NumBins for
	// 0..waterfall.FreqMax Hz.
	const halfHz = 60.0
	centerHz := freqHz + float64(scopeFT8BWHz)/2
	lo := centerHz - halfHz
	hi := centerHz + halfHz
	if lo < 0 {
		lo = 0
	}
	if hi > waterfall.FreqMax {
		hi = waterfall.FreqMax
	}
	binLo := int(lo / waterfall.FreqMax * float64(waterfall.NumBins))
	binHi := int(hi / waterfall.FreqMax * float64(waterfall.NumBins))
	if binHi <= binLo {
		return nil
	}
	srcCols := binHi - binLo

	// One image column per FFT bin and one image row per history row.
	// history[startIdx] is the NEWEST row in the slot (history is
	// stored newest-first); writing it to output row 0 (top) makes the
	// popup read top→bottom = newest→oldest, matching the live
	// waterfall's orientation. Earlier we walked endIdx → startIdx,
	// which produced an upside-down magnification relative to the
	// boxes the user was hovering.
	out := image.NewRGBA(image.Rect(0, 0, srcCols, srcRows))
	for y := 0; y < srcRows; y++ {
		row := s.history[startIdx+y]
		if len(row.Pixels) < binHi {
			continue
		}
		base := y * out.Stride
		for x := 0; x < srcCols; x++ {
			srcCol := binLo + x
			if srcCol >= len(row.Pixels) {
				srcCol = len(row.Pixels) - 1
			}
			c := row.Pixels[srcCol]
			off := base + x*4
			out.Pix[off+0] = c.R
			out.Pix[off+1] = c.G
			out.Pix[off+2] = c.B
			out.Pix[off+3] = 255
		}
	}
	return out
}

// SetDecodeHooks registers UI-side handlers fired by interactive decode
// boxes. Pass nil for any handler to disable that interaction.
//
//   - onDouble: invoked on double-click; used to scroll the chat to the
//     row that matches (slotStart, call).
//   - onHover / onHoverEnd: invoked on mouse-in / mouse-out; used to
//     show a popup with a magnified signal slice + signal info.
//   - onContext: invoked on right-click; used to open the operator
//     profile dialog for the call under the cursor.
//
// All handlers run on the Fyne main goroutine.
//
// Hover is intentionally NOT a hook: hover-only feedback (the box
// highlight) is rendered internally by scope; the GUI only learns
// about decode interactions via deliberate actions (double-click,
// right-click, single-click on a box, or single-click on empty
// waterfall = deselect).
func (s *scopePane) SetDecodeHooks(
	onDouble func(slotStart time.Time, call string),
	onContext func(call string, screenPos fyne.Position),
	onSelect func(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position),
	onDeselect func(),
	onHover func(call string, slotStart time.Time, freqHz float64, screenPos fyne.Position),
	onHoverEnd func(),
) {
	s.mu.Lock()
	s.onDecodeDoubleTap = onDouble
	s.onDecodeContext = onContext
	s.onDecodeSelect = onSelect
	s.onDecodeDeselect = onDeselect
	s.onDecodeHover = onHover
	s.onDecodeHoverEnd = onHoverEnd
	s.mu.Unlock()
}

// Hook accessors used by waterfallTap callbacks. Reading via these
// helpers means the callbacks pull the freshest registered hook on
// every event without holding the scope mutex during dispatch.
func (s *scopePane) getDecodeDoubleHook() func(time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeDoubleTap
}
func (s *scopePane) getDecodeContextHook() func(string, fyne.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeContext
}
func (s *scopePane) getDecodeSelectHook() func(string, time.Time, float64, fyne.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeSelect
}
func (s *scopePane) getDecodeDeselectHook() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeDeselect
}
func (s *scopePane) getDecodeHoverHook() func(string, time.Time, float64, fyne.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeHover
}
func (s *scopePane) getDecodeHoverEndHook() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDecodeHoverEnd
}

// setHover marks d as the hovered decode (or clears the hover state
// if d.call is empty) and refreshes the overlay so the box paints
// in its hover style. Called by the waterfallTap onHover handler.
func (s *scopePane) setHover(d finalizedDecode) {
	s.mu.Lock()
	if s.hoverCall == d.call && s.hoverSlotStart.Equal(d.slotStart) && s.hoverFreqHz == d.freqHz {
		s.mu.Unlock()
		return
	}
	s.hoverCall = d.call
	s.hoverSlotStart = d.slotStart
	s.hoverFreqHz = d.freqHz
	overlay := s.decodeOverlay
	s.mu.Unlock()
	if overlay != nil {
		fyne.Do(func() { overlay.Refresh() })
	}
}

// setSelection marks d as the click-active decode and refreshes the
// overlay so the box paints in its selected style.
func (s *scopePane) setSelection(d finalizedDecode) {
	s.mu.Lock()
	s.selectedCall = d.call
	s.selectedSlotStart = d.slotStart
	s.selectedFreqHz = d.freqHz
	overlay := s.decodeOverlay
	s.mu.Unlock()
	if overlay != nil {
		fyne.Do(func() { overlay.Refresh() })
	}
}

// clearSelection drops the click-active decode (no box renders in the
// selected style after this call).
func (s *scopePane) clearSelection() {
	s.mu.Lock()
	s.selectedCall = ""
	s.selectedSlotStart = time.Time{}
	s.selectedFreqHz = 0
	overlay := s.decodeOverlay
	s.mu.Unlock()
	if overlay != nil {
		fyne.Do(func() { overlay.Refresh() })
	}
}

// LatestDecodeFor returns the most recently finalised decode for the
// given callsign, or zero + false if none. Used by the HEARD list to
// open the magnification popup on a single-click without requiring
// the operator to first hover the corresponding waterfall box.
func (s *scopePane) LatestDecodeFor(call string) (finalizedDecode, bool) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return finalizedDecode{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.decodes) - 1; i >= 0; i-- {
		if s.decodes[i].call == call {
			return s.decodes[i], true
		}
	}
	return finalizedDecode{}, false
}

// decodeAtPixel hit-tests the cursor's local position against the
// live finalised decode set. Two-pass match:
//
//  1. Strict box hit (with a slopPx pad on every edge so a cursor
//     resting right at the stroke still counts).
//  2. If no strict hit AND the cursor is inside a slot's vertical
//     band, fall back to the nearest decode in that slot (by Δx)
//     within nearestPx pixels. This catches "I aimed at that signal
//     but the box is only 4 px wide" misses.
//
// Newer decodes win over older ones on overlap — iterate the slice
// from the back. Routing all hover / click hit-testing through this
// helper (rather than per-box event handlers) keeps interaction
// stable across the per-row waterfall scroll.
func (s *scopePane) decodeAtPixel(local fyne.Position, size fyne.Size) (finalizedDecode, bool) {
	if size.Width <= 0 || size.Height <= 0 {
		return finalizedDecode{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wfHeight <= 0 || len(s.decodes) == 0 {
		return finalizedDecode{}, false
	}
	const slopPx float32 = 3     // forgiveness around each box edge
	const nearestPx float32 = 14 // max horizontal distance for nearest-fallback
	pxPerRow := size.Height / float32(s.wfHeight)
	pxPerHz := size.Width / float32(scopeFreqMaxHz)
	halfHz := float32(scopeFT8BWHz) / 2
	rowOf := make(map[int64]int, len(s.boundaries))
	for _, b := range s.boundaries {
		key := b.when.Unix()
		if existing, ok := rowOf[key]; !ok || b.row < existing {
			rowOf[key] = b.row
		}
	}

	type cand struct {
		d  finalizedDecode
		dx float32
	}
	var nearest cand
	nearestDx := nearestPx + 1

	for i := len(s.decodes) - 1; i >= 0; i-- {
		d := s.decodes[i]
		ownRow, ok := rowOf[d.slotStart.Unix()]
		if !ok {
			continue
		}
		topRow := ownRow - rowsPerSlot
		if next, ok := rowOf[d.slotStart.Unix()+15]; ok {
			topRow = next
		}
		if topRow < 0 {
			topRow = 0
		}
		yTop := float32(topRow)*pxPerRow - slopPx
		yBot := float32(ownRow)*pxPerRow + slopPx
		if local.Y < yTop || local.Y > yBot {
			continue
		}
		cx := float32(d.freqHz+float64(scopeFT8BWHz)/2) * pxPerHz
		x0 := cx - halfHz*pxPerHz - slopPx
		x1 := cx + halfHz*pxPerHz + slopPx
		if local.X >= x0 && local.X <= x1 {
			return d, true
		}
		// Track nearest-by-Δx for the fallback pass; only consider
		// boxes whose vertical band already contains the cursor.
		dx := local.X - cx
		if dx < 0 {
			dx = -dx
		}
		if dx < nearestDx {
			nearestDx = dx
			nearest = cand{d: d, dx: dx}
		}
	}
	if nearestDx <= nearestPx {
		return nearest.d, true
	}
	return finalizedDecode{}, false
}

// SetHighlightCall marks a callsign whose decode boxes should render in a
// brighter "selected" colour. Pass "" to clear. Also forwards to the map
// widget so the operator's hover lights up the corresponding pin and any
// matching decode-overlay box at the same time.
//
// Safe from any goroutine; widget refresh dispatched via fyne.Do.
func (s *scopePane) SetHighlightCall(call string) {
	call = strings.ToUpper(call)
	s.mu.Lock()
	// Cancel any pending clear; this call either re-asserts a highlight
	// (so we don't want to drop it) or replaces it with a different
	// call.
	if s.highlightClear != nil {
		s.highlightClear.Stop()
		s.highlightClear = nil
	}
	if call == "" {
		// Defer the clear so a rapid hover-out followed by a hover-in
		// (which fyne's list re-binder can produce when the cursor sits
		// still on a row that's being redrawn) doesn't visibly drop
		// the highlight.
		curr := s.highlightCall
		s.highlightClear = time.AfterFunc(120*time.Millisecond, func() {
			s.mu.Lock()
			if s.highlightCall != curr {
				s.mu.Unlock()
				return
			}
			s.highlightCall = ""
			overlay := s.decodeOverlay
			mapW := s.mapWidget
			s.highlightClear = nil
			s.mu.Unlock()
			if mapW != nil {
				mapW.SetHighlight("")
			}
			if overlay != nil {
				fyne.Do(func() { overlay.Refresh() })
			}
		})
		s.mu.Unlock()
		return
	}
	changed := s.highlightCall != call
	s.highlightCall = call
	overlay := s.decodeOverlay
	mapW := s.mapWidget
	s.mu.Unlock()
	if !changed {
		return
	}
	if mapW != nil {
		mapW.SetHighlight(call)
	}
	if overlay != nil {
		fyne.Do(func() { overlay.Refresh() })
	}
}

// PaintDecodeMarkers queues this slot's decodes for hollow-box rendering.
// The boxes are not drawn here — they're stamped by SetWaterfallRow when
// the next slot boundary rolls in, so each box covers the full row range
// occupied by the signal rather than just the most recent few rows.
//
// Safe from any goroutine. Replaces an earlier implementation that stamped
// 3-row markers immediately; that version sat at the bottom of the image
// and scrolled away within a second, which the operator couldn't see at a
// glance.
func (s *scopePane) PaintDecodeMarkers(decodes []ft8.Decoded) {
	if len(decodes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Dedup within the slot: rescue / OSD passes can return the same
	// signal at slightly-different frequencies (e.g. one decode at
	// 1500 Hz and a duplicate at 1503 Hz). Treat anything within
	// ½ × scopeFT8BWHz of an existing pending decode for the same
	// call as a duplicate so we don't draw overlapping boxes.
	dupTolHz := float64(scopeFT8BWHz) / 2
	for _, d := range decodes {
		call := firstCallsign(d.Message.Text)
		// Floor SlotStart to the 15s boundary so dedup matches decodes
		// whose timestamps differ by a few ms within the same slot.
		slotSec := d.SlotStart.Unix() - d.SlotStart.Unix()%15
		slotStart := time.Unix(slotSec, 0).UTC()
		dup := false
		for _, p := range s.pending {
			if p.call != call {
				continue
			}
			if !p.slotStart.Equal(slotStart) {
				continue
			}
			if abs64(p.freqHz-d.Freq) < dupTolHz {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.pending = append(s.pending, pendingDecode{
			freqHz:    d.Freq,
			call:      call,
			snr:       float64(d.SNR),
			slotStart: slotStart,
		})
	}
	// Flush immediately. Each pending now carries its own slotStart,
	// so we don't have to wait for the next slot boundary to finalise
	// the boxes (which would mis-key whenever a decode took longer
	// than 15 s). Also gives the operator immediate visual feedback
	// instead of a 15-second delay before boxes appear.
	s.flushPendingBoxesLocked()
	overlay := s.decodeOverlay
	go func() {
		if overlay != nil {
			fyne.Do(func() { overlay.Refresh() })
		}
	}()
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// firstCallsign extracts the most informative callsign from a decoded
// message text. For "CQ X Y" returns X; otherwise returns the second
// token if it looks like a callsign (the more interesting party), else
// the first token.
func firstCallsign(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "CQ") && len(fields) >= 2 {
		return strings.ToUpper(fields[1])
	}
	if len(fields) >= 2 && looksLikeCall(fields[1]) {
		return strings.ToUpper(fields[1])
	}
	return strings.ToUpper(fields[0])
}

// looksLikeCall is a coarse filter — we only need to distinguish callsigns
// from grids/reports for label-priority purposes. Two-or-three-letter
// prefix + digit + suffix is enough.
func looksLikeCall(s string) bool {
	if len(s) < 4 || len(s) > 10 {
		return false
	}
	hasDigit := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			hasDigit = true
		} else if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && r != '/' {
			return false
		}
	}
	return hasDigit
}

// waterfallTap is a tappable + hoverable + secondary-tappable wrapper
// around the waterfall canvas stack. ALL pointer interactions for the
// waterfall route through this single widget rather than the per-decode
// rectangle pool entries — those decodeBox widgets move every frame as
// new audio rows arrive, and fyne's mouse-event dispatch can pair a
// down-event on one moved box with an up-event on a different one,
// producing flaky right-clicks and hover flicker.
//
// All click handlers receive the click's local pixel position; the
// callbacks then do their own decode hit-testing against the live
// (slot, freq) state.
type waterfallTap struct {
	widget.BaseWidget
	inner       fyne.CanvasObject
	onTap       func(xFrac float64)          // legacy single-tap → x fraction (unused now)
	onSelect    func(localPos fyne.Position) // single-tap → select decode under cursor
	onSecondary func(localPos fyne.Position, abs fyne.Position)
	onDouble    func(localPos fyne.Position)
	onDrag      func(localPos fyne.Position) // drag → live TX-frequency drag
	onHover     func(localPos fyne.Position) // mouse-in or mouse-move
	onHoverEnd  func()
}

var _ fyne.Tappable = (*waterfallTap)(nil)
var _ fyne.SecondaryTappable = (*waterfallTap)(nil)
var _ fyne.DoubleTappable = (*waterfallTap)(nil)
var _ fyne.Draggable = (*waterfallTap)(nil)
var _ desktop.Hoverable = (*waterfallTap)(nil)

func newWaterfallTap(inner fyne.CanvasObject, onTap func(xFrac float64)) *waterfallTap {
	w := &waterfallTap{inner: inner, onTap: onTap}
	w.ExtendBaseWidget(w)
	return w
}

func (w *waterfallTap) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(w.inner)
}

func (w *waterfallTap) MinSize() fyne.Size { return w.inner.MinSize() }

func (w *waterfallTap) Tapped(ev *fyne.PointEvent) {
	if w.onSelect != nil {
		w.onSelect(ev.Position)
	}
	if w.onTap != nil {
		size := w.Size()
		if size.Width <= 0 {
			return
		}
		xFrac := float64(ev.Position.X / size.Width)
		if xFrac < 0 {
			xFrac = 0
		}
		if xFrac > 1 {
			xFrac = 1
		}
		w.onTap(xFrac)
	}
}

func (w *waterfallTap) TappedSecondary(ev *fyne.PointEvent) {
	if w.onSecondary != nil {
		w.onSecondary(ev.Position, ev.AbsolutePosition)
	}
}

func (w *waterfallTap) DoubleTapped(ev *fyne.PointEvent) {
	if w.onDouble != nil {
		w.onDouble(ev.Position)
	}
}

func (w *waterfallTap) MouseIn(ev *desktop.MouseEvent) {
	if w.onHover != nil {
		w.onHover(ev.Position)
	}
}

func (w *waterfallTap) MouseMoved(ev *desktop.MouseEvent) {
	if w.onHover != nil {
		w.onHover(ev.Position)
	}
}

// Dragged fires on every pointer-move while a button is held. We use it
// for live TX-frequency tuning so the operator can sweep across the
// waterfall and watch the caret follow the cursor without releasing
// (the existing double-tap path is still there for "snap once").
//
// DragEvent.Position is in widget-local coordinates, same as Tapped /
// DoubleTapped — onDrag re-uses the freq math from onDouble.
func (w *waterfallTap) Dragged(ev *fyne.DragEvent) {
	if w.onDrag != nil {
		w.onDrag(ev.Position)
	}
}

// DragEnd is required by fyne.Draggable but we have no per-gesture
// state to release: each Dragged call already updated the live TX freq.
func (w *waterfallTap) DragEnd() {}

func (w *waterfallTap) MouseOut() {
	if w.onHoverEnd != nil {
		w.onHoverEnd()
	}
}

// waterfallSnapLayout snaps the waterfall display to an integer number of
// FT8 slots. On every parent resize Fyne calls Layout with the live pane
// size; we compute the slot count that fits, rebuild the underlying image
// to that exact pixel height (so 1:1 stretch maps cleanly), and re-rasterise
// the most recent rows from the history ring.
//
// This is what gives the waterfall its "snap" feel — the operator drags the
// VSplit and the display jumps to 2/3/4/... slots rather than smearing.
type waterfallSnapLayout struct {
	scope *scopePane
}

func (l *waterfallSnapLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 2 {
		return
	}
	// Snap rule: pick the largest N where N × slotPxMin ≤ paneHeight, so
	// we only add a slot once there's room for it at minimum height.
	// Within that N, stretch each slot continuously up to slotPxMax —
	// gives a smooth-resize feel until the next snap kicks in.
	wantSlots := int(size.Height) / slotPxMin
	if wantSlots < minDisplayedSlots {
		wantSlots = minDisplayedSlots
	}
	if wantSlots > maxDisplayedSlots {
		wantSlots = maxDisplayedSlots
	}
	l.scope.resizeToSlots(wantSlots)

	// Each slot scales between slotPxMin and slotPxMax. wfH = clamped
	// pane height; cap at slotPxMax × wantSlots so we don't stretch past
	// the snap-up threshold (which would just look stretched-then-empty).
	wfH := size.Height
	maxH := float32(wantSlots * slotPxMax)
	if wfH > maxH {
		wfH = maxH
	}
	stripW := float32(scopeTimeStripWidth)
	if stripW > size.Width {
		stripW = size.Width
	}
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(fyne.NewSize(stripW, wfH))
	objs[1].Move(fyne.NewPos(stripW, 0))
	objs[1].Resize(fyne.NewSize(size.Width-stripW, wfH))
}

func (l *waterfallSnapLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(scopeWFWidth+scopeTimeStripWidth, float32(slotPxMin*minDisplayedSlots))
}

// resizeToSlots resizes wfImg to wantSlots*rowsPerSlot rows and re-rasterises
// from history. Boundary rows are recomputed by walking history with the
// same slot-detection rule SetWaterfallRow uses.
//
// No-op when wantSlots already matches displayedSlots — the hot path on
// every layout call.
func (s *scopePane) resizeToSlots(wantSlots int) {
	s.mu.Lock()
	if wantSlots == s.displayedSlots {
		s.mu.Unlock()
		return
	}
	s.displayedSlots = wantSlots
	s.wfHeight = wantSlots * rowsPerSlot

	newImg := image.NewRGBA(image.Rect(0, 0, scopeWFWidth, s.wfHeight))
	// Black background (alpha = 255).
	for i := 3; i < len(newImg.Pix); i += 4 {
		newImg.Pix[i] = 255
	}
	stride := newImg.Stride

	// Walk history newest→oldest (history[0] is newest). Row 0 of the
	// image is the newest row; row wfHeight-1 the oldest visible.
	n := len(s.history)
	if n > s.wfHeight {
		n = s.wfHeight
	}
	s.boundaries = s.boundaries[:0]
	lastSlotSec := int64(-1)
	for i := 0; i < n; i++ {
		row := s.history[i]
		nSrc := len(row.Pixels)
		off := i * stride
		for x := 0; x < scopeWFWidth; x++ {
			srcIdx := x * (nSrc - 1) / (scopeWFWidth - 1)
			if srcIdx >= nSrc {
				srcIdx = nSrc - 1
			}
			c := row.Pixels[srcIdx]
			newImg.Pix[off+x*4+0] = c.R
			newImg.Pix[off+x*4+1] = c.G
			newImg.Pix[off+x*4+2] = c.B
			newImg.Pix[off+x*4+3] = 255
		}
	}
	// Boundary recovery: walk history oldest→newest so the "first row of
	// a new slot" rule matches the live path. Record positions in image
	// coords (0 = newest).
	for i := n - 1; i >= 0; i-- {
		row := s.history[i]
		if row.Time.IsZero() {
			continue
		}
		slotSec := row.Time.Unix() - row.Time.Unix()%15
		if slotSec != lastSlotSec {
			lastSlotSec = slotSec
			s.boundaries = append(s.boundaries, slotBoundary{
				row:  i,
				when: time.Unix(slotSec, 0).UTC(),
			})
			// Faint horizontal rule, matching SetWaterfallRow.
			ruleCol := color.RGBA{180, 180, 200, 110}
			off := i * stride
			for x := 0; x < scopeWFWidth; x++ {
				p := off + x*4
				newImg.Pix[p+0] = blendChan(newImg.Pix[p+0], ruleCol.R, ruleCol.A)
				newImg.Pix[p+1] = blendChan(newImg.Pix[p+1], ruleCol.G, ruleCol.A)
				newImg.Pix[p+2] = blendChan(newImg.Pix[p+2], ruleCol.B, ruleCol.A)
			}
		}
	}
	s.lastSlotSec = lastSlotSec
	s.wfImg = newImg
	// If the new image extends past the end of our row history, fill the
	// uncovered tail with synthetic slot-grid lines so the operator sees
	// a uniform slot grid even before audio arrives or before the buffer
	// has caught up to the bigger image.
	if n < s.wfHeight {
		s.seedPastBoundariesBelowLocked(n, time.Now().UTC())
	}
	canvasImg := s.wfCanvas
	timeStrip := s.timeStrip
	decodeOverlay := s.decodeOverlay
	s.mu.Unlock()

	if canvasImg != nil {
		canvasImg.Image = newImg
		fyne.Do(func() {
			canvasImg.Refresh()
			if timeStrip != nil {
				timeStrip.Refresh()
			}
			if decodeOverlay != nil {
				decodeOverlay.Refresh()
			}
		})
	}
}

// timeStripLayout positions one canvas.Text per slot boundary at its row's Y
// in the strip. Re-runs whenever the strip is refreshed (i.e. on every new
// row, since SetWaterfallRow refreshes it). Reuses pooled text widgets to
// avoid churn.
type timeStripLayout struct {
	scope *scopePane
	bg    *canvas.Rectangle
	pool  []*canvas.Text
}

func (l *timeStripLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if size.Width <= 0 || size.Height <= 0 {
		return
	}
	// Background fills the strip.
	l.bg.Move(fyne.NewPos(0, 0))
	l.bg.Resize(size)

	l.scope.mu.Lock()
	wfH := l.scope.wfHeight
	bounds := make([]slotBoundary, len(l.scope.boundaries))
	copy(bounds, l.scope.boundaries)
	l.scope.mu.Unlock()

	if wfH <= 0 {
		return
	}
	pxPerRow := size.Height / float32(wfH)

	// Grow pool to fit. Pool entries are added to the parent container by
	// the layout-as-side-effect pattern: we manage them via Add/Remove on
	// the timeStrip container.
	parent := l.scope.timeStrip
	for len(l.pool) < len(bounds) {
		t := canvas.NewText("", color.RGBA{210, 220, 240, 255})
		t.TextSize = 11
		t.TextStyle = fyne.TextStyle{Monospace: true}
		l.pool = append(l.pool, t)
		if parent != nil {
			parent.Add(t)
		}
	}
	// Hide unused pool entries by clearing their text and parking them
	// off-screen.
	for i := len(bounds); i < len(l.pool); i++ {
		l.pool[i].Text = ""
		l.pool[i].Move(fyne.NewPos(-1000, -1000))
		l.pool[i].Refresh()
	}

	for i, b := range bounds {
		t := l.pool[i]
		t.Text = b.when.Format("15:04:05")
		// Center the text vertically on the boundary row, clamped so the
		// glyph stays inside the strip.
		y := float32(b.row) * pxPerRow
		yPos := y - 7
		if yPos < 0 {
			yPos = 0
		}
		if yPos+14 > size.Height {
			yPos = size.Height - 14
		}
		t.Move(fyne.NewPos(4, yPos))
		t.Resize(fyne.NewSize(size.Width-4, 14))
		t.Refresh()
	}
}

func (l *timeStripLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(scopeTimeStripWidth, float32(slotPxMin*minDisplayedSlots))
}
