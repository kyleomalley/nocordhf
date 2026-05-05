package mapview

import (
	"image"
	"image/color"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/kyleomalley/nocordhf/lib/callsign"
	"github.com/kyleomalley/nocordhf/lib/ft8"
	"github.com/kyleomalley/nocordhf/lib/logging"
)

// spotEntry is one decoded station plotted on the map.
type spotEntry struct {
	call    string
	grid    string
	lat     float64
	lon     float64
	snr     float64
	seen    time.Time
	otaType string // "POTA", "SOTA", "WWFF", "IOTA", "BOTA", "LOTA", "NOTA" — empty if none
	precise bool   // true when lat/lon came from HamDB (home address), not the grid centre
}

// otaKeywords maps FT8 message tokens to OTA programme names.
// Activators typically call "CQ POTA W6ABC DM13" etc.
var otaKeywords = map[string]string{
	"POTA": "POTA", // Parks on the Air
	"SOTA": "SOTA", // Summits on the Air
	"WWFF": "WWFF", // World Wide Flora & Fauna
	"IOTA": "IOTA", // Islands on the Air
	"BOTA": "BOTA", // Beaches on the Air
	"LOTA": "LOTA", // Lighthouses on the Air
	"NOTA": "NOTA", // Nuns on the Air
}

// MapWidget is a zoomable, pannable ham radio station map backed by
// CartoDB Dark Matter slippy-map tiles (Web Mercator projection).
// Scroll wheel zooms toward the cursor; click-drag pans.
// Default view is centred on North America.
type MapWidget struct {
	widget.BaseWidget

	// Viewport (guarded by mu).
	mu        sync.Mutex
	centerLon float64
	centerLat float64
	zoom      float64

	// Spots (guarded by spotsMu).
	spotsMu sync.RWMutex
	spots   map[string]spotEntry
	myGrid  string
	myLat   float64
	myLon   float64
	// myOverrideValid pins the operator's diamond to an exact
	// lat/lon supplied via SetSelfPosition, overriding the
	// myGrid centroid. MeshCore mode reads SelfInfo.AdvLat/Lon
	// from the device (GPS-derived on T1000-E and similar
	// boards) and pushes it here so the diamond shows actual
	// position rather than the FT8 6-char-grid centroid (which
	// can be tens of miles off).
	myOverrideValid bool
	myOverrideLat   float64
	myOverrideLon   float64
	hoverCall       string // callsign to highlight; empty = none

	// QSO propagation arc (guarded by spotsMu).
	// qsoCall is the active QSO partner's callsign; empty = no arc.
	// qsoLat/qsoLon are the explicit partner coordinates; (0,0) falls back to
	// the spots database at draw time. qsoTxPhase=true bows the arc upward
	// (we are transmitting), false bows it downward (we are receiving).
	qsoCall    string
	qsoLat     float64
	qsoLon     float64
	qsoTxPhase bool
	// lastLoggedQSOKey dedupes the diagnostic Info log inside draw()
	// so we only emit a "qso-arc" line when the partner or phase
	// actually changes — without it, every Refresh would spam.
	lastLoggedQSOKey string

	tiles              *tileCache
	raster             *canvas.Raster
	onSpotTap          func(call string)                       // called when the user taps a station dot
	onSpotSecondaryTap func(call string, absPos fyne.Position) // called when the user right-clicks a station dot
	// onMeshNodeSecondaryTap fires for right-clicks on a MeshCore
	// node dot. The pubkey identifies the contact (caller looks it
	// up in its roster) and absPos lets the GUI place a popup menu.
	onMeshNodeSecondaryTap func(pub [32]byte, absPos fyne.Position)
	// onMapTap fires for any left-tap that doesn't land on a known
	// station dot or mesh node — gives the caller the unprojected
	// geographic point. Used by the Profile-settings location
	// picker to let the operator click anywhere on the map to set
	// their advert lat/lon.
	onMapTap func(lat, lon float64)

	// recentCQFn returns true when the GUI has heard the call transmit
	// a CQ recently. Used to render a green "CQ" badge next to the
	// callsign label on the map. nil-safe: when nil, no CQ markers
	// are drawn.
	recentCQFn func(call string) bool

	// workedFn classifies a spot for coloring: 0 = unworked, 1 = worked in same
	// grid square, 2 = worked this exact callsign. Set once at startup; read-only
	// during rendering so no additional locking is needed.
	workedFn func(call, grid string) int

	// localWorkedGridsFn returns the set of 4-char grid squares we've logged
	// locally (ADIF) on the active band but that aren't yet represented in our
	// LoTW data. Renders as blue — "we worked them, LoTW hasn't caught up".
	localWorkedGridsFn func() map[string]bool

	// workedGridsFn returns the set of 4-char grid squares already worked on the
	// currently-selected band (uppercase keys). Called at draw time when the
	// worked-grids overlay is enabled. Set once at startup.
	workedGridsFn func() map[string]bool

	// confirmedGridsFn returns the set of 4-char grid squares LoTW-confirmed on
	// the active band. Drawn on top of workedGridsFn's tint so confirmations
	// stand out from plain worked contacts. Set once at startup.
	confirmedGridsFn func() map[string]bool

	// showWorkedOverlay toggles the red tint over worked 4-char grid squares.
	showWorkedOverlay bool
	// hideLegend suppresses the bottom-left OTA / status legend
	// overlay. Defaults to false so existing FT8 callers see it
	// unchanged; the MeshCore mode flips this to true since the FT8
	// OTA glyphs (POTA / SOTA / WWFF / …) don't apply to mesh
	// traffic and a mesh-specific legend will replace them later.
	hideLegend bool
	// hideGrids suppresses both Maidenhead grid systems (field
	// 20°×10° and square 2°×1°), their tick labels, and the
	// worked-tile tint overlay. MeshCore mode wants a clean
	// basemap — grids only mean something for HF DX work.
	hideGrids bool
	// showMeshcoreLegend draws a small bottom-right swatch
	// explaining the node-type colours and path overlay style.
	// Independent of hideLegend (which gates the FT8 OTA
	// legend) so MeshCore mode shows mesh-relevant content
	// instead of being a blank basemap.
	showMeshcoreLegend bool
	// meshNodes is the set of MeshCore peers to plot — fed from
	// the contact roster's broadcast lat/lon. Nil / empty means
	// nothing extra rendered. Independent of the FT8 spot table.
	meshNodes []MeshNode
	// messagePaths holds in-flight animated message-path overlays.
	// Each entry has its own startedAt clock so the renderer
	// computes a "lightning-strike" reveal (segments appear
	// progressively) followed by an alpha fade — old paths drop
	// off naturally when their alpha hits zero. Multiple
	// concurrent paths overlay so a busy mesh's traffic is
	// readable as a swarm of fading arcs.
	messagePaths             []activeMessagePath
	messagePathTickerRunning bool // single ticker pumps refreshes for the whole set
}

// activeMessagePath is one animated path overlay. Stored on the
// MapWidget; refreshed by the animation ticker until the entry's
// total elapsed exceeds mcPathTotalDuration. The newest appended
// path is marked persistent — its reveal still animates, but it
// holds full alpha indefinitely (the previous persistent path is
// demoted to fading at append time). This gives the operator a
// stable view of the most recent route while a busy mesh's older
// strikes still fade naturally.
type activeMessagePath struct {
	nodes      []MessagePathNode
	startedAt  time.Time
	persistent bool
}

// MessagePathNode is one waypoint along a received-message route.
// Lat/Lon are float-degrees. Placeholder = true means the hop's
// pubkey-prefix didn't match any contact in the roster, so the
// renderer draws it as a smaller faded dot at an interpolated
// position between known endpoints.
type MessagePathNode struct {
	Name        string
	Lat, Lon    float64
	Placeholder bool
}

// MeshNode is one peer to plot on the map in MeshCore mode. Type
// is an integer that mirrors meshcore.AdvType (1=Chat, 2=Repeater,
// 3=Room, 4=Sensor); we store it as int here to avoid an import
// cycle from mapview to lib/meshcore.
type MeshNode struct {
	Name   string
	PubKey [32]byte
	Lat    float64
	Lon    float64
	Type   int
}

// MeshNode type identifiers — kept in sync with lib/meshcore.AdvType.
const (
	MeshNodeChat     = 1
	MeshNodeRepeater = 2
	MeshNodeRoom     = 3
	MeshNodeSensor   = 4
)

// NewMapWidget creates a map widget centred on the operator's grid at a
// regional zoom (~1/8 of the world's longitude span visible at startup).
func NewMapWidget(myGrid string) *MapWidget {
	// Default centre: mid-North America, in case grid is empty.
	cLat, cLon := 38.0, -98.0
	if lat, lon, ok := gridToLatLon(myGrid); ok {
		cLat, cLon = lat, lon
	}
	m := &MapWidget{
		spots:             make(map[string]spotEntry),
		myGrid:            myGrid,
		centerLon:         cLon,
		centerLat:         cLat,
		zoom:              8,
		showWorkedOverlay: true,
	}
	m.myLat, m.myLon, _ = gridToLatLon(myGrid)
	m.tiles = newTileCache(func() {
		fyne.Do(func() { m.raster.Refresh() })
	})
	m.raster = canvas.NewRaster(m.draw)
	m.ExtendBaseWidget(m)
	return m
}

// CreateRenderer implements fyne.Widget.
func (m *MapWidget) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(m.raster)
}

// MinSize implements fyne.Widget.
func (m *MapWidget) MinSize() fyne.Size { return fyne.NewSize(200, 120) }

// Scrolled implements fyne.Scrollable — zoom toward/away from the cursor.
func (m *MapWidget) Scrolled(ev *fyne.ScrollEvent) {
	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	if w == 0 || h == 0 {
		return
	}
	m.mu.Lock()

	// Current tile geometry. Event positions are DIP, so we compute tile
	// geometry in DIP units too (not m.pixW which is post-scale device px).
	ppd := w * m.zoom / 360.0
	tz, tNF, tSPx := tileGeom(ppd)
	_ = tz

	// Geographic point under the cursor (before zoom).
	mx := float64(ev.Position.X) - w/2
	my := float64(ev.Position.Y) - h/2
	cMX := lonToTileX(m.centerLon, tNF)
	cMY := latToTileY(m.centerLat, tNF)
	cursorLon := tileXToLon(cMX+mx/tSPx, tNF)
	cursorLat := tileYToLat(cMY+my/tSPx, tNF)

	// Apply zoom factor.
	factor := 1.15
	if ev.Scrolled.DY < 0 {
		factor = 1.0 / 1.15
	}
	m.zoom *= factor
	if m.zoom < 0.9 {
		m.zoom = 0.9
	}
	if m.zoom > 131072 { // ~tile z=17
		m.zoom = 131072
	}

	// New tile geometry.
	newPPD := w * m.zoom / 360.0
	_, newTNF, newTSPx := tileGeom(newPPD)

	// Shift centre so the geographic point under the cursor stays fixed.
	newCursorMX := lonToTileX(cursorLon, newTNF)
	newCursorMY := latToTileY(cursorLat, newTNF)
	newCMX := newCursorMX - mx/newTSPx
	newCMY := newCursorMY - my/newTSPx

	m.centerLon = tileXToLon(newCMX, newTNF)
	m.centerLat = tileYToLat(newCMY, newTNF)
	if m.centerLat > 85.0511 {
		m.centerLat = 85.0511
	}
	if m.centerLat < -85.0511 {
		m.centerLat = -85.0511
	}
	m.mu.Unlock()
	m.refresh()
}

// Dragged implements fyne.Draggable — pan the map.
func (m *MapWidget) Dragged(ev *fyne.DragEvent) {
	w := float64(m.raster.Size().Width)
	m.mu.Lock()
	if w > 0 {
		ppd := w * m.zoom / 360.0
		_, tNF, tSPx := tileGeom(ppd)

		cMX := lonToTileX(m.centerLon, tNF)
		cMY := latToTileY(m.centerLat, tNF)
		cMX -= float64(ev.Dragged.DX) / tSPx
		cMY -= float64(ev.Dragged.DY) / tSPx

		m.centerLon = tileXToLon(cMX, tNF)
		m.centerLat = tileYToLat(cMY, tNF)
		if m.centerLat > 85.0511 {
			m.centerLat = 85.0511
		}
		if m.centerLat < -85.0511 {
			m.centerLat = -85.0511
		}
	}
	m.mu.Unlock()
	m.refresh()
}

// DragEnd implements fyne.Draggable.
func (m *MapWidget) DragEnd() {}

// Refresh overrides BaseWidget.Refresh so external callers also force the
// raster to regenerate — the default BaseWidget path only repaints the
// renderer tree and leaves the cached raster image intact.
func (m *MapWidget) Refresh() {
	m.BaseWidget.Refresh()
	if m.raster != nil {
		m.refresh()
	}
}

// refresh is the always-safe entry point for raster repaints. Many
// callers (setter methods invoked from contact-roster refresh
// goroutines, the path-animation ticker, BLE / serial event
// handlers) sit OFF the Fyne UI thread, and Fyne 2.7 strictly
// enforces "UI mutations on the UI thread" via the
// "*** Error in Fyne call thread" guard. fyne.Do schedules the
// refresh onto the UI thread; calling it from the UI thread itself
// is a cheap no-op-equivalent. Use this everywhere instead of
// m.raster.Refresh() directly.
func (m *MapWidget) refresh() {
	if m.raster == nil {
		return
	}
	fyne.Do(func() { m.raster.Refresh() })
}

// SetOnSpotTap registers a callback invoked when the user taps a station dot.
func (m *MapWidget) SetOnSpotTap(fn func(call string)) {
	m.onSpotTap = fn
}

// SetOnSpotSecondaryTap registers a callback invoked on right-click/secondary
// tap of a station dot. absPos is the AbsolutePosition of the click, suitable
// for positioning a popup menu.
func (m *MapWidget) SetOnSpotSecondaryTap(fn func(call string, absPos fyne.Position)) {
	m.onSpotSecondaryTap = fn
}

// SetOnMeshNodeSecondaryTap registers a callback invoked on right-click of a
// MeshCore node dot. The pubkey lets the GUI look up the matching contact
// (display name, type, last-heard etc.) without the map widget needing to
// know about meshcore.Contact.
func (m *MapWidget) SetOnMeshNodeSecondaryTap(fn func(pub [32]byte, absPos fyne.Position)) {
	m.onMeshNodeSecondaryTap = fn
}

// SetRecentCQFunc installs a callback the renderer uses to decide
// whether to draw a green "CQ" badge next to a station label. The fn
// is consulted on every redraw, so the GUI's notion of "recent" can
// change without reseeding the map's spot list.
func (m *MapWidget) SetRecentCQFunc(fn func(call string) bool) {
	m.recentCQFn = fn
}

// SetWorkedFunc installs a function that classifies each spot for coloring.
// It is called once per spot per render with the station's callsign and grid.
// Return values: 0 = never worked, 1 = worked someone in the same grid square,
// 2 = worked this exact callsign.
func (m *MapWidget) SetWorkedFunc(fn func(call, grid string) int) {
	m.workedFn = fn
}

// SetLocalWorkedGridsFunc installs a callback that returns the set of 4-char
// grid squares worked locally (ADIF log) on the active band but not yet
// reflected in LoTW. Drawn as a blue tint beneath the yellow/red overlays.
func (m *MapWidget) SetLocalWorkedGridsFunc(fn func() map[string]bool) {
	m.localWorkedGridsFn = fn
}

// SetWorkedGridsFunc installs a callback that returns the set of 4-char grid
// squares (uppercase) worked on the currently-selected band. The overlay is
// re-queried on every render.
func (m *MapWidget) SetWorkedGridsFunc(fn func() map[string]bool) {
	m.workedGridsFn = fn
}

// SetConfirmedGridsFunc installs a callback that returns the set of 4-char
// grid squares (uppercase) LoTW-confirmed on the active band.
func (m *MapWidget) SetConfirmedGridsFunc(fn func() map[string]bool) {
	m.confirmedGridsFn = fn
}

// SetShowWorkedOverlay toggles the red tint over worked 4-char grid squares.
func (m *MapWidget) SetShowWorkedOverlay(on bool) {
	m.mu.Lock()
	changed := m.showWorkedOverlay != on
	m.showWorkedOverlay = on
	m.mu.Unlock()
	if changed {
		m.refresh()
	}
}

// SetShowLegend toggles the bottom-left OTA / status legend overlay.
// FT8 mode wants it on (default); MeshCore mode wants it off because
// the legend's POTA / SOTA / WWFF / portable glyphs don't apply to
// mesh contacts.
func (m *MapWidget) SetShowLegend(on bool) {
	m.mu.Lock()
	changed := m.hideLegend == on
	m.hideLegend = !on
	m.mu.Unlock()
	if changed {
		m.refresh()
	}
}

// SetShowMeshcoreLegend toggles the bottom-right swatch explaining
// MeshCore node-type colours (Repeater / Companion / Room / Sensor)
// and the path overlay's "real hop / unknown hop / route line"
// glyphs. MeshCore mode shows it; FT8 mode hides it.
func (m *MapWidget) SetShowMeshcoreLegend(on bool) {
	m.mu.Lock()
	changed := m.showMeshcoreLegend != on
	m.showMeshcoreLegend = on
	m.mu.Unlock()
	if changed {
		m.refresh()
	}
}

// SetShowGrids toggles the Maidenhead grid lines (field + square),
// their labels, and the worked-tile tint. FT8 mode wants them on
// (default); MeshCore mode wants them off — grid squares are an
// HF-DX construct, irrelevant on the mesh.
func (m *MapWidget) SetShowGrids(on bool) {
	m.mu.Lock()
	changed := m.hideGrids == on
	m.hideGrids = !on
	m.mu.Unlock()
	if changed {
		m.refresh()
	}
}

// SetMeshNodes replaces the MeshCore-node overlay set. Pass nil or
// an empty slice to clear. Nodes with lat=lon=0 are ignored on
// render so contacts that haven't broadcast a position don't pile
// up in the Atlantic.
func (m *MapWidget) SetMeshNodes(nodes []MeshNode) {
	m.mu.Lock()
	m.meshNodes = nodes
	m.mu.Unlock()
	m.refresh()
}

// ClearMeshNodes is a convenience wrapper for SetMeshNodes(nil).
// Called on FT8-mode return so the FT8 view stays uncluttered.
func (m *MapWidget) ClearMeshNodes() { m.SetMeshNodes(nil) }

// Animation timings for message-path overlays. mcPathRevealTotal is
// the FIXED wall-clock budget the entire path's reveal animation
// uses, regardless of hop count — a 2-hop path and an 8-hop path
// both finish in mcPathRevealTotal. Per-segment time is divided
// evenly (mcPathRevealTotal / segCount) so longer paths just draw
// each segment faster instead of a uniformly long sweep that scales
// with hops. Within each segment the arc reveals progressively
// (the line "draws itself" rather than popping in), and an
// arrowhead at the destination signals direction. After the reveal
// completes the alpha fades to zero over FadeDuration.
const (
	mcPathRevealTotal   = 2 * time.Second
	mcPathFadeDuration  = 5 * time.Second
	mcPathMaxConcurrent = 12 // cap so a busy mesh doesn't wedge the renderer
)

// AppendMessagePath adds an animated path overlay with its own age
// clock. Multiple paths may be in flight at once — each fades
// independently. Older paths beyond mcPathMaxConcurrent are dropped.
// No explicit Refresh — the animation ticker (started here when not
// already running) drives the next paint, which avoids a spike of
// N synchronous Refresh calls when a fan-out send queues a path
// per roster member.
func (m *MapWidget) AppendMessagePath(nodes []MessagePathNode) {
	m.AppendMessagePaths([][]MessagePathNode{nodes})
}

// AppendMessagePaths is the batch form. Useful for outbound channel
// sends that need one path-fade per roster member — call once with
// every endpoint instead of N separate AppendMessagePath calls,
// each of which would otherwise schedule its own UI refresh. The
// last entry of the batch becomes the persistent path (held at full
// alpha until the next AppendMessagePaths call); previously
// persistent paths are demoted to fading from now.
func (m *MapWidget) AppendMessagePaths(batch [][]MessagePathNode) {
	now := time.Now()
	added := false
	m.mu.Lock()
	// Demote any existing persistent path to a fading one,
	// rebasing its clock so the fade starts now (not from when
	// it was first appended).
	for i := range m.messagePaths {
		if m.messagePaths[i].persistent {
			m.messagePaths[i].persistent = false
			// Rebase so the fade starts immediately rather than from
			// the moment the path first appeared. Since the reveal
			// budget is fixed (mcPathRevealTotal), all paths share
			// the same lifetime regardless of hop count.
			m.messagePaths[i].startedAt = now.Add(-mcPathRevealTotal)
		}
	}
	// Append the new batch. Track the index of the last valid
	// entry so we can mark it persistent after the loop.
	lastIdx := -1
	for _, nodes := range batch {
		if len(nodes) < 2 {
			continue
		}
		cp := append([]MessagePathNode(nil), nodes...)
		m.messagePaths = append(m.messagePaths, activeMessagePath{
			nodes:     cp,
			startedAt: now,
		})
		lastIdx = len(m.messagePaths) - 1
		added = true
	}
	if lastIdx >= 0 {
		m.messagePaths[lastIdx].persistent = true
	}
	if len(m.messagePaths) > mcPathMaxConcurrent {
		m.messagePaths = m.messagePaths[len(m.messagePaths)-mcPathMaxConcurrent:]
	}
	m.mu.Unlock()
	if added {
		m.ensureMessagePathTicker()
	}
}

// SetMessagePath is the legacy single-path API kept for the
// right-click "Show path on map" action — clears any in-flight
// animations and pins the supplied path with a fresh clock so it
// fades like the auto-fired ones.
func (m *MapWidget) SetMessagePath(nodes []MessagePathNode) {
	m.mu.Lock()
	m.messagePaths = nil
	m.mu.Unlock()
	m.AppendMessagePath(nodes)
}

// ClearMessagePath drops every in-flight animation immediately.
func (m *MapWidget) ClearMessagePath() {
	m.mu.Lock()
	m.messagePaths = nil
	m.mu.Unlock()
	m.refresh()
}

// ensureMessagePathTicker spins up a 30 Hz refresh loop while any
// path animation is active. Self-stops when the active set drains.
// Idempotent — multiple calls during a busy burst share one ticker.
func (m *MapWidget) ensureMessagePathTicker() {
	m.mu.Lock()
	if m.messagePathTickerRunning {
		m.mu.Unlock()
		return
	}
	m.messagePathTickerRunning = true
	m.mu.Unlock()
	go func() {
		// 30 Hz — needed for the arc-draw + arrowhead animation
		// to look smooth (the line's leading edge advances along
		// a quadratic Bezier; at 15 Hz the head visibly hops).
		// Each tick prunes expired entries then issues exactly
		// one Refresh, so the cost is bounded regardless of how
		// many concurrent paths are in flight.
		t := time.NewTicker(33 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			m.mu.Lock()
			now := time.Now()
			kept := m.messagePaths[:0]
			anyAnimating := false
			for _, p := range m.messagePaths {
				if p.persistent {
					// Persistent path stays forever (until the
					// next AppendMessagePaths demotes it).
					kept = append(kept, p)
					// Still animating during its reveal phase.
					if now.Sub(p.startedAt) < mcPathRevealTotal {
						anyAnimating = true
					}
					continue
				}
				if now.Sub(p.startedAt) < mcPathRevealTotal+mcPathFadeDuration {
					kept = append(kept, p)
					anyAnimating = true
				}
			}
			m.messagePaths = kept
			// Stop the ticker once everything has finished
			// animating — the persistent path is static after
			// its reveal completes, so re-rendering at 15 Hz
			// would be pure waste. AppendMessagePaths spins the
			// ticker back up the next time something new arrives.
			if !anyAnimating {
				m.messagePathTickerRunning = false
			}
			done := !anyAnimating
			m.mu.Unlock()
			// raster.Refresh must run on the Fyne UI thread.
			// Calling it from this background ticker goroutine
			// directly trips the "*** Error in Fyne call thread"
			// guard at higher tick rates (30 Hz). fyne.Do
			// schedules the refresh onto the UI thread.
			fyne.Do(func() { m.raster.Refresh() })
			if done {
				return
			}
		}
	}()
}

// Tapped implements fyne.Tappable. Hit-tests visible station dots
// first; if no spot is within range and an onMapTap callback is
// registered, falls through to the geographic-tap path with the
// unprojected lat/lon under the cursor.
func (m *MapWidget) Tapped(ev *fyne.PointEvent) {
	if m.onSpotTap != nil {
		if call := m.hitTestSpot(ev.Position); call != "" {
			m.onSpotTap(call)
			return
		}
	}
	if m.onMapTap != nil {
		if lat, lon, ok := m.unproject(ev.Position); ok {
			m.onMapTap(lat, lon)
		}
	}
}

// SetOnMapTap registers a callback fired when the user taps an
// empty area of the map (no station dot nearby). Lat/lon are
// decimal degrees in the standard WGS-84 frame. Pass nil to
// disable. The Profile-settings location picker uses this to let
// the operator click anywhere to set their advert position.
func (m *MapWidget) SetOnMapTap(fn func(lat, lon float64)) {
	m.onMapTap = fn
}

// unproject converts a screen pixel position into geographic
// lat/lon by inverting the same Web-Mercator projection the
// renderer uses. Returns ok=false when the raster hasn't sized
// yet (no viewport to project against).
func (m *MapWidget) unproject(pos fyne.Position) (lat, lon float64, ok bool) {
	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	if w == 0 || h == 0 {
		return 0, 0, false
	}
	m.mu.Lock()
	zoom := m.zoom
	centerLon := m.centerLon
	centerLat := m.centerLat
	m.mu.Unlock()
	ppd := w * zoom / 360.0
	_, tNF, tSPx := tileGeom(ppd)
	cMX := lonToTileX(centerLon, tNF)
	cMY := latToTileY(centerLat, tNF)
	mx := float64(pos.X) - w/2
	my := float64(pos.Y) - h/2
	lon = tileXToLon(cMX+mx/tSPx, tNF)
	lat = tileYToLat(cMY+my/tSPx, tNF)
	return lat, normLon(lon), true
}

// TappedSecondary implements fyne.SecondaryTappable. Right-click on a station
// dot invokes onSpotSecondaryTap, used to open the profile viewer. If no FT8
// spot is hit we fall through to the MeshCore node overlay so the operator
// can right-click a peer to inspect / DM / show its path.
func (m *MapWidget) TappedSecondary(ev *fyne.PointEvent) {
	if m.onSpotSecondaryTap != nil {
		if call := m.hitTestSpot(ev.Position); call != "" {
			m.onSpotSecondaryTap(call, ev.AbsolutePosition)
			return
		}
	}
	if m.onMeshNodeSecondaryTap != nil {
		if pub, ok := m.hitTestMeshNode(ev.Position); ok {
			m.onMeshNodeSecondaryTap(pub, ev.AbsolutePosition)
		}
	}
}

// DoubleTapped implements fyne.DoubleTappable. Re-centres the map on the
// double-clicked geographic point and zooms in by 25%.
func (m *MapWidget) DoubleTapped(ev *fyne.PointEvent) {
	const factor = 1.25

	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	if w == 0 || h == 0 {
		logging.L.Debugw("map DoubleTapped ignored (zero viewport)",
			"x", ev.Position.X, "y", ev.Position.Y)
		return
	}
	m.mu.Lock()

	ppd := w * m.zoom / 360.0
	_, tNF, tSPx := tileGeom(ppd)

	// Geographic point under the cursor.
	mx := float64(ev.Position.X) - w/2
	my := float64(ev.Position.Y) - h/2
	cMX := lonToTileX(m.centerLon, tNF)
	cMY := latToTileY(m.centerLat, tNF)
	cursorLon := tileXToLon(cMX+mx/tSPx, tNF)
	cursorLat := tileYToLat(cMY+my/tSPx, tNF)

	// Re-centre on that point.
	oldLat, oldLon, oldZoom := m.centerLat, m.centerLon, m.zoom
	m.centerLon = cursorLon
	m.centerLat = cursorLat

	// Zoom in.
	m.zoom *= factor
	if m.zoom > 131072 {
		m.zoom = 131072
	}

	if m.centerLat > 85.0511 {
		m.centerLat = 85.0511
	}
	if m.centerLat < -85.0511 {
		m.centerLat = -85.0511
	}
	newLat, newLon, newZoom := m.centerLat, m.centerLon, m.zoom
	m.mu.Unlock()
	logging.L.Infow("map DoubleTapped",
		"px", ev.Position.X, "py", ev.Position.Y,
		"from_lat", oldLat, "from_lon", oldLon, "from_zoom", oldZoom,
		"to_lat", newLat, "to_lon", newLon, "to_zoom", newZoom)
	m.refresh()
}

// hitTestSpot returns the callsign of the station dot or label under pos,
// preferring the nearest dot within 12 px and falling back to the label
// bounding box if no dot qualifies. Mirrors the draw-loop layout including
// stacked-spot vertical spreading so multi-station cells stay clickable.
func (m *MapWidget) hitTestSpot(pos fyne.Position) string {
	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	if w == 0 || h == 0 {
		return ""
	}

	m.mu.Lock()
	zoom := m.zoom
	centerLon := m.centerLon
	centerLat := m.centerLat
	m.mu.Unlock()

	ppd := w * zoom / 360.0
	_, tNF, tSPx := tileGeom(ppd)
	cMX := lonToTileX(centerLon, tNF)
	cMY := latToTileY(centerLat, tNF)

	toX := func(lon float64) int {
		return int((lonToTileX(wrapLon(lon, centerLon), tNF)-cMX)*tSPx + w/2)
	}
	toY := func(lat float64) int {
		return int((latToTileY(lat, tNF)-cMY)*tSPx + h/2)
	}

	tx := float64(pos.X)
	ty := float64(pos.Y)

	m.spotsMu.RLock()
	defer m.spotsMu.RUnlock()

	// Group by on-screen pixel so stacked spots share the same dot position —
	// identical grouping to the draw loop.
	type pixGroup struct{ spots []spotEntry }
	groups := map[[2]int]*pixGroup{}
	for _, s := range m.spots {
		key := [2]int{toX(s.lon), toY(s.lat)}
		if grp, ok := groups[key]; ok {
			grp.spots = append(grp.spots, s)
		} else {
			groups[key] = &pixGroup{spots: []spotEntry{s}}
		}
	}

	var (
		best      string
		bestScore = math.MaxFloat64
	)
	// Use the same font sizing as rendering so label bounds match visually.
	_, adv, ascent := callsignFont(ppd)
	const rowSpacing = 14
	dotHitR2 := 12.0 * 12.0

	for key, grp := range groups {
		n := len(grp.spots)
		cx, cy := key[0], key[1]
		spacing := rowSpacing
		if n > 6 {
			if cellPixH := int(ppd); cellPixH > 0 {
				spacing = cellPixH / (n + 1)
			}
			if spacing < 5 {
				spacing = 5
			}
		}
		firstOffset := -(n - 1) * spacing / 2

		for i, s := range grp.spots {
			y := cy + firstOffset + i*spacing
			x := cx

			// Dot proximity (preferred).
			dx := tx - float64(x)
			dy := ty - float64(y)
			if d2 := dx*dx + dy*dy; d2 <= dotHitR2 {
				if d2 < bestScore {
					bestScore = d2
					best = s.call
				}
				continue
			}

			// Label bounding box. The draw loop places the label at
			// (x+gap, y+ascent/2) when zoomed in (ppd>=45), else above the
			// dot with a tiny font.
			if ppd >= 45 {
				gap := 6
				if ascent > 10 {
					gap = ascent / 2
				}
				labelW := len(s.call) * adv
				lx := x + gap
				if lx+labelW > int(w)-2 {
					lx = x - gap - labelW
				}
				lxMin := lx - 2
				lxMax := lx + labelW + 2
				lyMin := y + ascent/2 - ascent - 2
				lyMax := y + ascent/2 + 2
				if int(tx) >= lxMin && int(tx) <= lxMax &&
					int(ty) >= lyMin && int(ty) <= lyMax {
					// Score label hits slightly worse than dot hits so a
					// nearby dot still wins.
					score := dotHitR2 + 1
					if score < bestScore {
						bestScore = score
						best = s.call
					}
				}
			} else {
				// Tiny-font branch: 5px wide glyphs, ~7px tall, centered above.
				lw := len(s.call)*5 - 1
				labelY := y - 14
				if s.otaType == "" {
					labelY = y - 9
				}
				lx := x - lw/2
				lxMin := lx - 2
				lxMax := lx + lw + 2
				lyMin := labelY - 9
				lyMax := labelY + 2
				if int(tx) >= lxMin && int(tx) <= lxMax &&
					int(ty) >= lyMin && int(ty) <= lyMax {
					score := dotHitR2 + 1
					if score < bestScore {
						bestScore = score
						best = s.call
					}
				}
			}
		}
	}
	return best
}

// hitTestMeshNode returns the pubkey of the MeshCore node dot under pos,
// preferring the nearest dot within 12 px. Mirrors the draw-loop projection
// (toX/toY use the same wrapLon-aware path used in the renderer's mesh-node
// pass) so right-click hits land on the node the operator actually sees.
func (m *MapWidget) hitTestMeshNode(pos fyne.Position) ([32]byte, bool) {
	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	if w == 0 || h == 0 {
		return [32]byte{}, false
	}

	m.mu.Lock()
	zoom := m.zoom
	centerLon := m.centerLon
	centerLat := m.centerLat
	nodes := append([]MeshNode(nil), m.meshNodes...)
	m.mu.Unlock()

	if len(nodes) == 0 {
		return [32]byte{}, false
	}

	ppd := w * zoom / 360.0
	_, tNF, tSPx := tileGeom(ppd)
	cMX := lonToTileX(centerLon, tNF)
	cMY := latToTileY(centerLat, tNF)

	toX := func(lon float64) int {
		return int((lonToTileX(wrapLon(lon, centerLon), tNF)-cMX)*tSPx + w/2)
	}
	toY := func(lat float64) int {
		return int((latToTileY(lat, tNF)-cMY)*tSPx + h/2)
	}

	tx := float64(pos.X)
	ty := float64(pos.Y)
	const dotHitR2 = 12.0 * 12.0

	var (
		bestPub   [32]byte
		bestFound bool
		bestScore = math.MaxFloat64
	)
	for _, n := range nodes {
		if n.Lat == 0 && n.Lon == 0 {
			continue
		}
		dx := tx - float64(toX(n.Lon))
		dy := ty - float64(toY(n.Lat))
		if d2 := dx*dx + dy*dy; d2 <= dotHitR2 && d2 < bestScore {
			bestScore = d2
			bestPub = n.PubKey
			bestFound = true
		}
	}
	return bestPub, bestFound
}

// FlyTo centres the map on the given lat/lon. If the map is very zoomed out
// (zoom < 15) it also zooms in to a useful level so the target is visible.
// The destination longitude is normalised to the nearest wrap copy of the
// current centre so panning past ±180° doesn't cause a jarring jump.
func (m *MapWidget) FlyTo(lat, lon float64) {
	m.mu.Lock()
	m.centerLat = lat
	m.centerLon = wrapLon(lon, m.centerLon)
	if m.zoom < 15 {
		m.zoom = 20
	}
	m.mu.Unlock()
	m.refresh()
}

// FlyToRadius centres the map on the given lat/lon and sets zoom so
// roughly the requested radius (in statute miles) is visible from
// centre to the shorter window edge. Used by MeshCore mode to open
// the map zoomed to ~50 miles around the operator's broadcast
// position rather than the default mid-North-America regional view.
//
// The pixels-per-degree formula matches the rest of the widget:
// ppd = w * zoom / 360. We solve for zoom against a radius
// converted to degrees of latitude (1° ≈ 69 mi). Falls back to a
// conservative default zoom if the widget hasn't been laid out yet
// (no width to compute against).
func (m *MapWidget) FlyToRadius(lat, lon, miles float64) {
	if miles <= 0 {
		m.FlyTo(lat, lon)
		return
	}
	size := m.raster.Size()
	w := float64(size.Width)
	h := float64(size.Height)
	short := w
	if h > 0 && h < w {
		short = h
	}
	radiusDeg := miles / 69.0 // 1° latitude ≈ 69 statute miles
	m.mu.Lock()
	m.centerLat = lat
	m.centerLon = wrapLon(lon, m.centerLon)
	if short > 0 && radiusDeg > 0 {
		// We want centre→edge to span radiusDeg, so the visible
		// span across the short axis is 2*radiusDeg degrees:
		// short / (2*radiusDeg) = ppd; zoom = ppd * 360 / w.
		ppd := short / (2 * radiusDeg)
		m.zoom = ppd * 360.0 / w
		if m.zoom < 0.9 {
			m.zoom = 0.9
		}
		if m.zoom > 131072 {
			m.zoom = 131072
		}
	} else if m.zoom < 15 {
		m.zoom = 20
	}
	m.mu.Unlock()
	m.refresh()
}

// GetSpotLocation returns the lat/lon of a spotted callsign, or false if unknown.
func (m *MapWidget) GetSpotLocation(call string) (lat, lon float64, ok bool) {
	m.spotsMu.RLock()
	defer m.spotsMu.RUnlock()
	for _, s := range m.spots {
		if strings.EqualFold(s.call, call) {
			return s.lat, s.lon, true
		}
	}
	return 0, 0, false
}

// GetSpotGrid returns the grid associated with the most recent spot of call,
// or "" if the call hasn't been spotted.
func (m *MapWidget) GetSpotGrid(call string) string {
	m.spotsMu.RLock()
	defer m.spotsMu.RUnlock()
	for _, s := range m.spots {
		if strings.EqualFold(s.call, call) {
			return s.grid
		}
	}
	return ""
}

// UpgradeSpotLocation refines the map pin for call with HamDB-sourced
// coordinates. Behaviour depends on what the station is currently transmitting:
//
//   - spot has no grid (coarse prefix placement): upgrade freely.
//   - spot has a grid whose 4-char field matches hamdbGrid: upgrade to HamDB's
//     precise home coordinates (they're in their home grid).
//   - spot has a grid that disagrees with hamdbGrid: station is portable —
//     do NOT overwrite; display the grid they're actively sending.
//
// Does nothing if the call isn't currently spotted.
func (m *MapWidget) UpgradeSpotLocation(call, hamdbGrid string, lat, lon float64) {
	m.spotsMu.Lock()
	s, ok := m.spots[call]
	if !ok {
		m.spotsMu.Unlock()
		return
	}
	if s.grid != "" {
		if len(hamdbGrid) < 4 || len(s.grid) < 4 ||
			!strings.EqualFold(s.grid[:4], hamdbGrid[:4]) {
			// Portable station — trust the message grid.
			m.spotsMu.Unlock()
			return
		}
	}
	s.lat = lat
	s.lon = lon
	s.precise = true
	if hamdbGrid != "" {
		s.grid = hamdbGrid
	}
	m.spots[call] = s
	m.spotsMu.Unlock()
	fyne.Do(func() { m.raster.Refresh() })
}

// SetHighlight marks a callsign for visual emphasis on the map.
// Pass an empty string to clear the highlight.
func (m *MapWidget) SetHighlight(call string) {
	m.spotsMu.Lock()
	changed := m.hoverCall != call
	m.hoverCall = call
	m.spotsMu.Unlock()
	if changed {
		m.refresh()
	}
}

// UpdateMyGrid refreshes the operator's own location marker.
func (m *MapWidget) UpdateMyGrid(grid string) {
	m.spotsMu.Lock()
	m.myGrid = grid
	m.myLat, m.myLon, _ = gridToLatLon(grid)
	m.spotsMu.Unlock()
	m.refresh()
}

// SetSelfPosition pins the operator's diamond to an explicit
// lat/lon, overriding the myGrid-derived centroid. MeshCore mode
// uses this to show the firmware-reported (often GPS-derived)
// position rather than the coarser FT8 6-char-grid square.
// Lat=0 && Lon=0 clears the override and falls back to myGrid.
func (m *MapWidget) SetSelfPosition(lat, lon float64) {
	m.spotsMu.Lock()
	if lat == 0 && lon == 0 {
		m.myOverrideValid = false
	} else {
		m.myOverrideValid = true
		m.myOverrideLat = lat
		m.myOverrideLon = lon
	}
	m.spotsMu.Unlock()
	m.refresh()
}

// ClearSelfPosition is a convenience wrapper to drop the override
// and fall back to the myGrid centroid (FT8 mode).
func (m *MapWidget) ClearSelfPosition() { m.SetSelfPosition(0, 0) }

// RemoveStaleSpots drops every spot whose `seen` timestamp is older than
// maxAge. Returns the number of spots removed so the caller can decide
// whether to trigger a redraw (and whether to log it). Caller should
// invoke Refresh() if n > 0.
//
// Used by the GUI's roster-staleness sweep so the map doesn't keep
// painting stations that haven't been heard for a long time.
func (m *MapWidget) RemoveStaleSpots(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	m.spotsMu.Lock()
	defer m.spotsMu.Unlock()
	n := 0
	for k, s := range m.spots {
		if !s.seen.After(cutoff) {
			delete(m.spots, k)
			n++
		}
	}
	return n
}

// SetQSOPartner records the active QSO partner for the propagation arc.
// lat/lon (0,0) means "look up from the spot database at draw time."
// txPhase=true → arc bows upward (we TX); false → bows downward (we RX).
func (m *MapWidget) SetQSOPartner(call string, lat, lon float64, txPhase bool) {
	m.spotsMu.Lock()
	m.qsoCall = strings.ToUpper(call)
	m.qsoLat = lat
	m.qsoLon = lon
	m.qsoTxPhase = txPhase
	m.spotsMu.Unlock()
	fyne.Do(func() { m.raster.Refresh() })
}

// SetQSOTxPhase flips the arc direction without changing the partner.
// true = we are TX-ing (arc bows up), false = we are RX-ing (bows down).
// No-ops if no QSO partner is set.
func (m *MapWidget) SetQSOTxPhase(txPhase bool) {
	m.spotsMu.Lock()
	changed := m.qsoCall != "" && m.qsoTxPhase != txPhase
	m.qsoTxPhase = txPhase
	m.spotsMu.Unlock()
	if changed {
		fyne.Do(func() { m.raster.Refresh() })
	}
}

// ClearQSOPartner removes the propagation arc.
func (m *MapWidget) ClearQSOPartner() {
	m.spotsMu.Lock()
	had := m.qsoCall != ""
	m.qsoCall = ""
	m.qsoLat, m.qsoLon = 0, 0
	m.spotsMu.Unlock()
	if had {
		fyne.Do(func() { m.raster.Refresh() })
	}
}

// ClearSpots removes every station pin and any active QSO arc. Called
// on band change so the map only shows current-band activity.
func (m *MapWidget) ClearSpots() {
	m.spotsMu.Lock()
	m.spots = map[string]spotEntry{}
	m.qsoCall = ""
	m.qsoLat, m.qsoLon = 0, 0
	m.hoverCall = ""
	m.spotsMu.Unlock()
	fyne.Do(func() { m.raster.Refresh() })
}

// AddSpots parses decoded FT8 messages and adds stations to the map.
// When a message contains a grid square it is used for precise placement;
// otherwise the callsign prefix is looked up in the entity table for a
// representative country/region location.
func (m *MapWidget) AddSpots(results []ft8.Decoded, myCall string) {
	changed := false
	m.spotsMu.Lock()
	for _, d := range results {
		call, grid, otaType, hasCall := parseSpotMsg(d.Message.Text, myCall)
		if !hasCall || call == "" {
			continue
		}
		var lat, lon float64
		if lat2, lon2, ok := gridToLatLon(grid); ok {
			lat, lon = lat2, lon2
		} else if ent, ok := callsign.Lookup(call); ok {
			lat, lon = ent.Lat, ent.Lon
			grid = "" // no grid; placed by prefix only
		} else {
			continue // can't place this station at all
		}
		precise := false
		// If we already have a HamDB-precise pin and the new decode's 4-char
		// grid still matches our stored grid, keep the precise coords — the
		// station is transmitting from home, we shouldn't snap back to the
		// grid centre just because a fresh CQ came in.
		if prev, ok := m.spots[call]; ok && prev.precise && grid != "" &&
			len(prev.grid) >= 4 && len(grid) >= 4 &&
			strings.EqualFold(prev.grid[:4], grid[:4]) {
			lat, lon = prev.lat, prev.lon
			precise = true
		}
		m.spots[call] = spotEntry{
			call: call, grid: grid,
			lat: lat, lon: lon,
			snr: d.SNR, seen: time.Now(),
			otaType: otaType,
			precise: precise,
		}
		changed = true
	}
	cutoff := time.Now().Add(-60 * time.Minute)
	for k, s := range m.spots {
		if s.seen.Before(cutoff) {
			delete(m.spots, k)
		}
	}
	m.spotsMu.Unlock()
	if changed {
		fyne.Do(func() { m.raster.Refresh() })
	}
}

// draw is the canvas.Raster pixel callback.
func (m *MapWidget) draw(w, h int) image.Image {
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	m.mu.Lock()
	centerLon := m.centerLon
	centerLat := m.centerLat
	zoom := m.zoom
	showWorked := m.showWorkedOverlay
	showGrids := !m.hideGrids
	m.mu.Unlock()

	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// ppd = pixels per degree of longitude at the Mercator equator.
	ppd := float64(w) * zoom / 360.0
	tileZoom, tileNF, tileScreenPx := tileGeom(ppd)
	tileN := 1 << tileZoom

	// Mercator tile-coordinate of the viewport centre.
	centerMX := lonToTileX(centerLon, tileNF)
	centerMY := latToTileY(centerLat, tileNF)

	// Screen-coordinate converters (Web Mercator).
	toX := func(lon float64) int {
		return int((lonToTileX(lon, tileNF)-centerMX)*tileScreenPx + float64(w)/2)
	}
	toY := func(lat float64) int {
		// Web Mercator is only defined up to ~±85.0511°. Clamp so stations at
		// extreme latitudes (e.g. Arctic drift expeditions transmitting grids
		// like HR27 at ~87.5°N) render at the top/bottom edge of the map
		// instead of vanishing off-screen or jittering in NaN territory.
		if lat > 85.0511 {
			lat = 85.0511
		} else if lat < -85.0511 {
			lat = -85.0511
		}
		return int((latToTileY(lat, tileNF)-centerMY)*tileScreenPx + float64(h)/2)
	}
	onScreen := func(x, y int) bool {
		return x >= 0 && x < w && y >= 0 && y < h
	}
	// toXW converts a geographic longitude to a screen X, choosing the
	// nearest wrapped copy to the current centre so that spots, arcs, and
	// the own-station marker are correct after panning past ±180°.
	toXW := func(lon float64) int {
		return toX(wrapLon(lon, centerLon))
	}

	// Dark ocean background — shown while tiles are loading.
	bg := color.RGBA{10, 28, 52, 255}
	pix := img.Pix
	for off := 0; off < len(pix); off += 4 {
		pix[off] = bg.R
		pix[off+1] = bg.G
		pix[off+2] = bg.B
		pix[off+3] = bg.A
	}

	// Blit map tiles covering the viewport.
	halfTW := float64(w) / (2 * tileScreenPx)
	halfTH := float64(h) / (2 * tileScreenPx)
	txMin := int(math.Floor(centerMX - halfTW))
	txMax := int(math.Ceil(centerMX + halfTW))
	tyMin := int(math.Floor(centerMY - halfTH))
	tyMax := int(math.Ceil(centerMY + halfTH))
	if tyMin < 0 {
		tyMin = 0
	}
	if tyMax >= tileN {
		tyMax = tileN - 1
	}
	sz := int(math.Ceil(tileScreenPx)) + 1 // rendered pixel size of one tile
	for ty := tyMin; ty <= tyMax; ty++ {
		for tx := txMin; tx <= txMax; tx++ {
			tileImg := m.tiles.get(tileZoom, ((tx%tileN)+tileN)%tileN, ty)
			if tileImg == nil {
				continue // bg colour already painted
			}
			sx := int(math.Round((float64(tx)-centerMX)*tileScreenPx + float64(w)/2))
			sy := int(math.Round((float64(ty)-centerMY)*tileScreenPx + float64(h)/2))
			blitTile(img, tileImg, sx, sy, sz)
		}
	}

	// Read spots snapshot now so we can suppress grid labels in occupied cells.
	m.spotsMu.RLock()
	spots := make([]spotEntry, 0, len(m.spots))
	for _, s := range m.spots {
		spots = append(spots, s)
	}
	hoverCall := m.hoverCall
	myGrid := m.myGrid
	myLat := m.myLat
	myLon := m.myLon
	if m.myOverrideValid {
		// MeshCore mode supplies an exact lat/lon (firmware-
		// reported, often GPS-derived) — preferred over the
		// FT8-grid centroid.
		myLat = m.myOverrideLat
		myLon = m.myOverrideLon
		myGrid = "x" // non-empty so the downstream draw block fires
	}
	qsoCall := m.qsoCall
	qsoLat := m.qsoLat
	qsoLon := m.qsoLon
	qsoTxPhase := m.qsoTxPhase
	m.spotsMu.RUnlock()

	// Build set of occupied 4-char grid squares to suppress overlapping labels.
	occupiedGrids := map[string]bool{}
	for _, s := range spots {
		key := strings.ToUpper(s.grid)
		if len(key) > 4 {
			key = key[:4]
		}
		occupiedGrids[key] = true
	}

	// Visible lon/lat extent — used to clip the square grid iteration.
	tyTop := centerMY - halfTH - 1
	tyBot := centerMY + halfTH + 1
	if tyTop < 0 {
		tyTop = 0
	}
	if tyBot > tileNF {
		tyBot = tileNF
	}
	visLonMin := tileXToLon(centerMX-halfTW-1, tileNF)
	visLonMax := tileXToLon(centerMX+halfTW+1, tileNF)
	visLatMax := tileYToLat(tyTop, tileNF)
	visLatMin := tileYToLat(tyBot, tileNF)

	// ── Maidenhead grid overlay ───────────────────────────────────────────────
	//
	// ppd thresholds (pixels per degree longitude):
	//   ppd <  7  : field only (2-char, 20°×10°)
	//   ppd >= 7  : + square lines (4-char, 2°×1°)
	//   ppd >= 45 : + square labels
	//
	// Whole block is gated on showGrids — MeshCore mode hides it
	// since grid squares are an HF-DX construct.
	face := basicfont.Face7x13

	if showGrids {
		// Field grid (20°×10°) — always visible in HF mode.
		// Vertical lines are iterated over the extended visible-longitude range so
		// they continue to appear after the map has been panned past ±180°.
		fieldCol := color.RGBA{18, 45, 82, 255}
		fieldLabelCol := color.RGBA{180, 210, 255, 255}
		lnStart20 := math.Floor(visLonMin/20) * 20
		for lon := lnStart20; lon <= visLonMax+20; lon += 20 {
			x := toX(lon)
			if x >= 0 && x < w {
				mapVline(img, x, 0, h-1, fieldCol)
			}
		}
		for i := 0; i <= 18; i++ {
			lat := -90.0 + float64(i)*10
			y := toY(lat)
			if y >= 0 && y < h {
				mapHline(img, 0, w-1, y, fieldCol)
			}
		}
		// Field labels: shown when squares are NOT labeled.
		if ppd < 45 {
			for lon := lnStart20; lon < visLonMax; lon += 20 {
				col := int(math.Floor((normLon(lon) + 180) / 20))
				if col < 0 || col >= 18 {
					continue
				}
				cx := toX(lon + 10)
				for row := 0; row < 18; row++ {
					lbl := string([]byte{byte('A' + col), byte('A' + row)})
					cy := toY(-85.0+float64(row)*10) + 5
					if onScreen(cx, cy) {
						mapDrawTextOutlined(img, lbl, cx-6, cy, fieldLabelCol, face)
					}
				}
			}
		}
	}

	// Grid-status overlay — three-colour tint on 4-char squares for the active
	// band: red for LoTW-confirmed (QSL), yellow for LoTW-known QSOs with no
	// QSL yet, blue for contacts we have locally but LoTW doesn't reflect.
	// Drawn beneath the square grid lines so boundaries stay visible.
	if showGrids && showWorked && ppd >= 7 {
		var lotwWorked, confirmed, local map[string]bool
		if m.workedGridsFn != nil {
			lotwWorked = m.workedGridsFn()
		}
		if m.confirmedGridsFn != nil {
			confirmed = m.confirmedGridsFn()
		}
		if m.localWorkedGridsFn != nil {
			local = m.localWorkedGridsFn()
		}
		if len(lotwWorked) > 0 || len(confirmed) > 0 || len(local) > 0 {
			workedTint := color.RGBA{230, 200, 40, 100}   // yellow: LoTW QSO (no QSL)
			confirmedTint := color.RGBA{220, 40, 40, 110} // red: LoTW-confirmed (QSL)
			localTint := color.RGBA{80, 140, 230, 110}    // blue: local-only QSO
			lonStart := math.Floor(visLonMin/2) * 2
			latStart := math.Floor(visLatMin/1) * 1
			for lon := lonStart; lon <= visLonMax; lon += 2 {
				cLon := normLon(lon)
				col := int(math.Floor((cLon + 180) / 20))
				if col < 0 || col >= 18 {
					continue
				}
				sqLon := int(math.Floor((cLon+180)/2)) % 10
				if sqLon < 0 {
					continue
				}
				for lat := latStart; lat <= visLatMax; lat++ {
					row := int(math.Floor((lat + 90) / 10))
					if row < 0 || row >= 18 {
						continue
					}
					sqLat := int(math.Floor(lat+90)) % 10
					if sqLat < 0 {
						continue
					}
					lbl := string([]byte{
						byte('A' + col), byte('A' + row),
						byte('0' + sqLon), byte('0' + sqLat),
					})
					var tint color.RGBA
					switch {
					case confirmed[lbl]:
						tint = confirmedTint
					case lotwWorked[lbl]:
						tint = workedTint
					case local[lbl]:
						tint = localTint
					default:
						continue
					}
					x0 := toX(lon)
					x1 := toX(lon + 2)
					y0 := toY(lat + 1)
					y1 := toY(lat)
					mapFillRect(img, x0, y0, x1, y1, tint)
				}
			}
		}
	}

	// Square grid (2°×1°).
	if showGrids && ppd >= 7 {
		sqCol := color.RGBA{14, 36, 66, 255}
		sqLabelCol := color.RGBA{180, 210, 255, 255}
		lonStart := math.Floor(visLonMin/2) * 2
		latStart := math.Floor(visLatMin/1) * 1
		// Vertical lines every 2°.
		for lon := lonStart; lon <= visLonMax; lon += 2 {
			if math.Mod(lon, 20) == 0 {
				continue // already drawn by field grid
			}
			x := toX(lon)
			if x >= 0 && x < w {
				mapVline(img, x, 0, h-1, sqCol)
			}
		}
		// Horizontal lines every 1°.
		for lat := latStart; lat <= visLatMax; lat++ {
			if math.Mod(lat, 10) == 0 {
				continue
			}
			y := toY(lat)
			if y >= 0 && y < h {
				mapHline(img, 0, w-1, y, sqCol)
			}
		}
		// Square labels (4-char) when cells are at least ~90px wide.
		if ppd >= 45 {
			for lon := lonStart; lon < visLonMax; lon += 2 {
				for lat := latStart; lat < visLatMax; lat++ {
					cLon := normLon(lon)
					col := int(math.Floor((cLon + 180) / 20))
					row := int(math.Floor((lat + 90) / 10))
					if col < 0 || col >= 18 || row < 0 || row >= 18 {
						continue
					}
					sqLon := int(math.Floor((cLon+180)/2)) % 10
					sqLat := int(math.Floor(lat+90)) % 10
					if sqLon < 0 || sqLat < 0 {
						continue
					}
					lbl := string([]byte{
						byte('A' + col), byte('A' + row),
						byte('0' + sqLon), byte('0' + sqLat),
					})
					// Hide label when a station is plotted in this cell.
					if occupiedGrids[lbl] {
						continue
					}
					cx := toX(lon + 1)
					cy := toY(lat+0.5) + 4
					if onScreen(cx-14, cy-10) && onScreen(cx+14, cy+2) {
						mapDrawTextOutlined(img, lbl, cx-14, cy, sqLabelCol, face)
					}
				}
			}
		}
	}

	// ── QSO propagation arc ──────────────────────────────────────────────────
	// Drawn under spots so station dots/labels remain visible.
	if qsoCall != "" && myGrid != "" {
		var arcX, arcY int
		var arcOK bool
		if qsoLon != 0 || qsoLat != 0 {
			arcX, arcY = toXW(qsoLon), toY(qsoLat)
			arcOK = true
		} else {
			for _, s := range spots {
				if strings.EqualFold(s.call, qsoCall) {
					arcX, arcY = toXW(s.lon), toY(s.lat)
					arcOK = true
					break
				}
			}
		}
		// Diagnostic: log only when the call/phase changes so we don't
		// spam every redraw. Tells us whether SetQSOPartner is firing
		// and whether the partner was findable in the spot DB.
		key := qsoCall
		if qsoTxPhase {
			key += "|tx"
		} else {
			key += "|rx"
		}
		if key != m.lastLoggedQSOKey {
			m.lastLoggedQSOKey = key
			if logging.L != nil {
				logging.L.Infow("qso-arc",
					"call", qsoCall,
					"tx_phase", qsoTxPhase,
					"resolved", arcOK,
					"spots", len(spots),
					"my_grid_set", myGrid != "",
				)
			}
		}
		if arcOK {
			mapDrawQSOArc(img, toXW(myLon), toY(myLat), arcX, arcY, qsoTxPhase)
		}
	} else if qsoCall == "" && m.lastLoggedQSOKey != "" {
		m.lastLoggedQSOKey = ""
	}

	// Group spots that share the same on-screen pixel so they can be spread
	// vertically instead of stacking on top of each other.
	type pixGroup struct{ spots []spotEntry }
	groups := map[[2]int]*pixGroup{}
	for _, s := range spots {
		key := [2]int{toXW(s.lon), toY(s.lat)}
		if grp, ok := groups[key]; ok {
			grp.spots = append(grp.spots, s)
		} else {
			groups[key] = &pixGroup{spots: []spotEntry{s}}
		}
	}

	// Stable iteration: a Go map randomises iteration order, so without
	// sorting the groups (and the spots within each group) the same
	// cluster gets a different visual arrangement every redraw — labels
	// dance around as the user pans/zooms. Sort by pixel key for groups
	// and by callsign within each group so positions are deterministic.
	groupKeys := make([][2]int, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		if groupKeys[i][0] != groupKeys[j][0] {
			return groupKeys[i][0] < groupKeys[j][0]
		}
		return groupKeys[i][1] < groupKeys[j][1]
	})
	for _, grp := range groups {
		sort.Slice(grp.spots, func(i, j int) bool { return grp.spots[i].call < grp.spots[j].call })
	}

	const rowSpacing = 14
	for _, key := range groupKeys {
		grp := groups[key]
		n := len(grp.spots)
		cx, cy := key[0], key[1]

		// Layout selection by crowding:
		//   n ≤ 4   → vertical stack (legacy behaviour, cleanest for sparse cells)
		//   n  > 4  → concentric rings centred on the cell so the
		//             cluster stays inside the DXCC square boundary
		//             instead of spilling out as a tall column.
		spacing := rowSpacing
		var ringRadii []int      // pixel radius of each ring; len == #rings
		var ringCount []int      // spots in each ring (filled inner-out)
		var ringStartA []float64 // starting angle (radians) for each ring
		useRings := n > 4
		if useRings {
			cellPixH := int(ppd) // 1° lat ≈ ppd pixels; one square is 1° tall
			if cellPixH < 24 {
				cellPixH = 24
			}
			maxR := cellPixH/2 - 4
			if maxR < 6 {
				maxR = 6
			}
			// Pack: 1 in centre, 6 on first ring, 12 on second, etc.
			// Stop when we've allocated enough slots for n spots.
			ringCount = []int{1}
			placed := 1
			ringIdx := 1
			for placed < n {
				slots := 6 * ringIdx
				ringCount = append(ringCount, slots)
				placed += slots
				ringIdx++
			}
			rings := len(ringCount)
			ringRadii = make([]int, rings)
			ringStartA = make([]float64, rings)
			for r := 0; r < rings; r++ {
				if r == 0 {
					ringRadii[r] = 0
				} else {
					ringRadii[r] = maxR * r / (rings - 1)
					if ringRadii[r] < 6 {
						ringRadii[r] = 6
					}
				}
				// Stagger ring starting angles so labels on adjacent rings
				// don't all line up along the same azimuth.
				ringStartA[r] = float64(r) * (math.Pi / 6)
			}
		} else if n > 6 {
			cellPixH := int(ppd)
			if cellPixH > 0 {
				spacing = cellPixH / (n + 1)
			}
			if spacing < 5 {
				spacing = 5
			}
		}

		totalSpan := (n - 1) * spacing
		firstOffset := -totalSpan / 2

		for i, s := range grp.spots {
			var x, y int
			if useRings {
				// Find the ring this spot lands in.
				idx := i
				ring := 0
				for ring < len(ringCount) && idx >= ringCount[ring] {
					idx -= ringCount[ring]
					ring++
				}
				if ring == 0 {
					x, y = cx, cy
				} else {
					slots := ringCount[ring]
					theta := ringStartA[ring] + 2*math.Pi*float64(idx)/float64(slots)
					x = cx + int(float64(ringRadii[ring])*math.Cos(theta))
					y = cy + int(float64(ringRadii[ring])*math.Sin(theta))
				}
			} else {
				dy := firstOffset + i*spacing
				x = cx
				y = cy + dy
			}
			if !onScreen(x, y) {
				continue
			}
			highlighted := hoverCall != "" && strings.EqualFold(s.call, hoverCall)
			dotR := 3
			workedStatus := 0
			if m.workedFn != nil {
				workedStatus = m.workedFn(s.call, s.grid)
			}
			dotC := spotColour(workedStatus, s.seen)
			labelCol := color.RGBA{210, 210, 210, 220}
			if highlighted {
				dotR = 6
				dotC = color.RGBA{255, 255, 80, 255}
				labelCol = color.RGBA{255, 255, 80, 255}
			}
			mapDrawDot(img, x, y, dotR, dotC)
			isRecentCQ := m.recentCQFn != nil && m.recentCQFn(s.call)
			// At zoomed-out levels we render the pixel-art badge
			// directly above the station dot (no room for the
			// callsign-adjacent layout used at high zoom). Zoomed-in
			// levels paint the same badge next to the callsign label
			// — see the inner block below — so we skip this one when
			// the side-of-callsign badge will appear.
			if s.otaType != "" && ppd < 45 {
				drawBadgeIcon(img, x, y-16, s.otaType, 24)
			}
			if ppd >= 45 {
				// Zoomed in: scaled font to the right of the dot. At very
				// high zoom we use a larger TrueType face so labels stay
				// readable and clickable. CQ + OTA badges sit
				// immediately to the LEFT of the callsign label so the
				// reader's eye picks up status before the call.
				lblFace, adv, ascent := callsignFont(ppd)
				labelW := len(s.call) * adv
				gap := 6
				if ascent > 10 {
					gap = ascent / 2
				}
				lx := x + gap
				if lx+labelW > w-2 {
					lx = x - gap - labelW
				}
				if lx < 2 {
					lx = 2
				}
				ly := y + ascent/2

				// Status badges sit in a fixed strip immediately to
				// the left of the callsign label. Order from nearest
				// the call to farthest: [CQ?] [OTA?] CALLSIGN.
				// Both badges share the same vertical centre line,
				// matched against the callsign's text baseline.
				badgeGap := 2
				iconSize := 24 // px diameter of each callsign-side badge
				badgeY := ly - iconSize/2 - 4
				cursor := lx - badgeGap
				if isRecentCQ {
					cursor -= iconSize
					if drawBadgeIcon(img, cursor+iconSize/2, badgeY+iconSize/2, "CQ", iconSize) {
						cursor -= badgeGap
					} else {
						cursor += iconSize
					}
				}
				if s.otaType != "" {
					cursor -= iconSize
					if drawBadgeIcon(img, cursor+iconSize/2, badgeY+iconSize/2, s.otaType, iconSize) {
						cursor -= badgeGap
					} else {
						cursor += iconSize
					}
				}
				mapDrawTextOutlined(img, s.call, lx, ly, labelCol, lblFace)
			} else {
				// Zoomed out: tiny font centered above the dot.
				lw := len(s.call)*5 - 1
				labelY := y - 24 // clears the 2×-scale OTA icon
				if s.otaType == "" {
					labelY = y - 9
				}
				if labelY < 2 {
					labelY = y + 9
				}
				lx := x - lw/2
				if lx < 2 {
					lx = 2
				}
				if lx+lw > w-2 {
					lx = w - 2 - lw
				}
				mapDrawTinyTextOutlined(img, s.call, lx, labelY, labelCol)
			}
		}
	}

	// Operator's own station — yellow diamond.
	if myGrid != "" {
		x := toXW(myLon)
		y := toY(myLat)
		if onScreen(x-6, y-6) || onScreen(x+6, y+6) {
			mapDrawDiamond(img, x, y, 6, color.RGBA{255, 220, 0, 255})
		}
	}

	if !m.hideLegend {
		mapDrawLegend(img, w, h)
	}
	if m.showMeshcoreLegend {
		mapDrawMeshcoreLegend(img, w, h)
	}

	// MeshCore node overlay — one colored dot per peer that
	// broadcast a position. Type drives colour so the operator can
	// scan the topology at a glance: repeaters in red (the
	// infrastructure), rooms in green (group endpoints), sensors
	// in orange (telemetry sources), chat peers in blue. Drawn
	// after the legend so labels can sit on top of grid lines.
	m.mu.Lock()
	nodes := append([]MeshNode(nil), m.meshNodes...)
	m.mu.Unlock()
	if len(nodes) > 0 {
		labelCol := color.RGBA{220, 230, 245, 255}
		for _, n := range nodes {
			if n.Lat == 0 && n.Lon == 0 {
				continue
			}
			x := toX(n.Lon)
			y := toY(n.Lat)
			if !onScreen(x, y) {
				continue
			}
			var c color.RGBA
			switch n.Type {
			case MeshNodeRepeater:
				c = color.RGBA{220, 60, 60, 255}
			case MeshNodeRoom:
				c = color.RGBA{60, 200, 100, 255}
			case MeshNodeSensor:
				c = color.RGBA{230, 160, 40, 255}
			default:
				c = color.RGBA{80, 150, 240, 255}
			}
			mapDrawDot(img, x, y, 5, c)
			if n.Name != "" {
				mapDrawTinyTextOutlined(img, n.Name, x+8, y+4, labelCol)
			}
		}
	}

	// MeshCore message-path overlay — animated cyan polylines, one
	// per active path. Each path "lights up" segment-by-segment
	// (relay-style) then fades to invisible over mcPathFadeDuration.
	// Multiple concurrent overlays so a busy mesh's traffic reads
	// as overlapping fading strikes rather than a single sticky line.
	m.mu.Lock()
	paths := append([]activeMessagePath(nil), m.messagePaths...)
	m.mu.Unlock()
	if len(paths) > 0 {
		now := time.Now()
		for _, p := range paths {
			drawAnimatedMessagePath(img, p, now, toX, toY, onScreen)
		}
	}

	return img
}

// drawAnimatedMessagePath renders one animated path. Each hop pair
// is drawn as a quadratic-Bezier arc that "draws itself" from the
// previous node toward the next, with a small arrowhead at the
// destination once a segment completes. The whole path's reveal
// budget is mcPathRevealTotal regardless of hop count — divided
// evenly between segments so longer paths just animate each leg
// faster. After the reveal completes the alpha fades to zero over
// mcPathFadeDuration. Persistent paths hold full alpha forever.
func drawAnimatedMessagePath(img *image.RGBA, p activeMessagePath, now time.Time,
	toX func(float64) int, toY func(float64) int, onScreen func(int, int) bool,
) {
	segCount := len(p.nodes) - 1
	if segCount < 1 {
		return
	}
	elapsed := now.Sub(p.startedAt)
	revealTotal := mcPathRevealTotal
	perSeg := revealTotal / time.Duration(segCount)
	// Fractional reveal: integer part = fully-completed segments,
	// fractional part = progress along the in-flight segment.
	fractionalReveal := float64(elapsed) / float64(perSeg)
	if maxR := float64(segCount); fractionalReveal > maxR {
		fractionalReveal = maxR
	}
	fullSegs := int(fractionalReveal)
	partialProg := fractionalReveal - float64(fullSegs)

	alpha := 1.0
	if !p.persistent && elapsed > revealTotal {
		fadeProgress := float64(elapsed-revealTotal) / float64(mcPathFadeDuration)
		if fadeProgress >= 1 {
			return
		}
		alpha = 1 - fadeProgress
	}
	scaled := func(c color.RGBA) color.RGBA {
		return color.RGBA{c.R, c.G, c.B, byte(float64(c.A) * alpha)}
	}
	pathCol := scaled(color.RGBA{60, 200, 230, 220})
	arrowCol := scaled(color.RGBA{120, 220, 240, 255})
	dotCol := scaled(color.RGBA{120, 220, 240, 255})
	placeholderCol := scaled(color.RGBA{160, 165, 175, 200})
	labelCol := scaled(color.RGBA{200, 230, 245, 255})

	// Project every node up-front so arc + dot rendering use the
	// same screen coordinates.
	type pt struct{ x, y int }
	pts := make([]pt, len(p.nodes))
	for i, n := range p.nodes {
		pts[i] = pt{toX(n.Lon), toY(n.Lat)}
	}

	// Fully-revealed segments: full arc + arrowhead at destination.
	for i := 0; i < fullSegs; i++ {
		drawPathArc(img, pts[i].x, pts[i].y, pts[i+1].x, pts[i+1].y, 1.0, pathCol)
		dx, dy := arcEndTangent(pts[i].x, pts[i].y, pts[i+1].x, pts[i+1].y, 1.0)
		drawArrowhead(img, pts[i+1].x, pts[i+1].y, dx, dy, arrowCol)
	}
	// In-flight segment: partial arc, no arrowhead until it
	// completes (the next tick promotes it to fullSegs).
	if fullSegs < segCount && partialProg > 0 {
		i := fullSegs
		drawPathArc(img, pts[i].x, pts[i].y, pts[i+1].x, pts[i+1].y, partialProg, pathCol)
	}

	// Dots for every node we've reached so far. Source dot of the
	// in-flight segment is the destination dot of the previous
	// segment (or the path origin); both are drawn here.
	upTo := fullSegs
	if partialProg > 0 && fullSegs < segCount {
		upTo = fullSegs // source already shown by previous loop pass; destination not yet
	}
	for i := 0; i <= upTo && i < len(p.nodes); i++ {
		n := p.nodes[i]
		x, y := pts[i].x, pts[i].y
		if !onScreen(x, y) {
			continue
		}
		if n.Placeholder {
			mapDrawDot(img, x, y, 3, placeholderCol)
		} else {
			mapDrawDot(img, x, y, 5, dotCol)
		}
		if n.Name != "" {
			mapDrawTinyTextOutlined(img, n.Name, x+8, y+4, labelCol)
		}
	}
}

// drawPathArc renders a quadratic-Bezier arc from (x0,y0) to (x1,y1)
// up to the fractional reveal `progress` ∈ (0, 1]. The control
// point sits perpendicular to the chord at its midpoint, with bow
// magnitude scaled to the chord length so short hops bow gently
// and long hops curve more visibly. Drawn as a polyline of small
// straight segments — sample density scales with chord length so
// short arcs don't burn samples and long arcs don't go faceted.
func drawPathArc(img *image.RGBA, x0, y0, x1, y1 int, progress float64, c color.RGBA) {
	if progress <= 0 {
		return
	}
	if progress > 1 {
		progress = 1
	}
	fx0, fy0 := float64(x0), float64(y0)
	fx1, fy1 := float64(x1), float64(y1)
	dx, dy := fx1-fx0, fy1-fy0
	length := math.Hypot(dx, dy)
	if length < 1 {
		return
	}
	// Perpendicular unit vector (rotate chord 90° counter-clockwise
	// in screen coords — y grows downward, so this consistently bows
	// arcs upward when reading left-to-right).
	px, py := -dy/length, dx/length
	bow := length * 0.18
	if bow > 32 {
		bow = 32
	}
	if bow < 6 {
		bow = 6
	}
	cx := (fx0+fx1)/2 + px*bow
	cy := (fy0+fy1)/2 + py*bow
	// Sample density: ~one sample every 4 px of arc length, capped
	// so a giant continent-spanning hop doesn't stall the renderer.
	samples := int(length/4) + 8
	if samples > 80 {
		samples = 80
	}
	prevX, prevY := x0, y0
	for i := 1; i <= samples; i++ {
		t := float64(i) / float64(samples) * progress
		u := 1 - t
		// Quadratic Bezier: B(t) = (1-t)² P0 + 2(1-t)t C + t² P1
		x := u*u*fx0 + 2*u*t*cx + t*t*fx1
		y := u*u*fy0 + 2*u*t*cy + t*t*fy1
		ix, iy := int(x), int(y)
		mapDrawLine(img, prevX, prevY, ix, iy, c)
		prevX, prevY = ix, iy
	}
}

// arcEndTangent returns the direction vector of the arc at its
// final sampled point (just shy of progress==1). Used to orient
// arrowheads so they point along the arrival vector rather than
// the chord.
func arcEndTangent(x0, y0, x1, y1 int, progress float64) (dx, dy float64) {
	fx0, fy0 := float64(x0), float64(y0)
	fx1, fy1 := float64(x1), float64(y1)
	chordX, chordY := fx1-fx0, fy1-fy0
	length := math.Hypot(chordX, chordY)
	if length < 1 {
		return 0, 0
	}
	px, py := -chordY/length, chordX/length
	bow := length * 0.18
	if bow > 32 {
		bow = 32
	}
	if bow < 6 {
		bow = 6
	}
	cx := (fx0+fx1)/2 + px*bow
	cy := (fy0+fy1)/2 + py*bow
	// Quadratic Bezier derivative at t=1: B'(1) = 2(P1 - C).
	t := progress
	if t > 1 {
		t = 1
	}
	tx := 2 * (1 - t) * (cx - fx0)
	ty := 2 * (1 - t) * (cy - fy0)
	tx += 2 * t * (fx1 - cx)
	ty += 2 * t * (fy1 - cy)
	tlen := math.Hypot(tx, ty)
	if tlen < 1 {
		return 0, 0
	}
	return tx / tlen, ty / tlen
}

// drawArrowhead renders a small filled-look V at (x,y) opening in
// the (-dx,-dy) direction (so the V's tip sits at (x,y) and points
// where dx,dy says the line was going). Drawn with two short lines
// rather than a filled polygon — keeps it lightweight and matches
// the existing line-based rendering style.
func drawArrowhead(img *image.RGBA, x, y int, dx, dy float64, c color.RGBA) {
	if dx == 0 && dy == 0 {
		return
	}
	const (
		arrowLen   = 8.0  // px back from the tip
		arrowAngle = 0.45 // radians off the line — ~26°, looks balanced
	)
	cosA, sinA := math.Cos(arrowAngle), math.Sin(arrowAngle)
	// Backwards vector along the line.
	bx, by := -dx, -dy
	// Rotate ±arrowAngle.
	x1 := bx*cosA - by*sinA
	y1 := bx*sinA + by*cosA
	x2 := bx*cosA + by*sinA
	y2 := -bx*sinA + by*cosA
	mapDrawLine(img, x, y, x+int(x1*arrowLen), y+int(y1*arrowLen), c)
	mapDrawLine(img, x, y, x+int(x2*arrowLen), y+int(y2*arrowLen), c)
}

// ── Web Mercator helpers ──────────────────────────────────────────────────────

// tileGeom derives the tile zoom level, tileNF (2^zoom as float), and the
// screen-pixel size of one 256-px tile from ppd (pixels per degree longitude).
func tileGeom(ppd float64) (tileZoom int, tileNF, tileScreenPx float64) {
	tileZoom = int(math.Log2(ppd * 360.0 / 256.0))
	if tileZoom < 0 {
		tileZoom = 0
	}
	if tileZoom > 18 {
		tileZoom = 18
	}
	tileNF = float64(int(1) << tileZoom)
	tileScreenPx = ppd * 360.0 / tileNF
	return
}

// lonToTileX converts a longitude to a fractional tile-x coordinate.
func lonToTileX(lon, tileN float64) float64 {
	return (lon + 180.0) / 360.0 * tileN
}

// latToTileY converts a latitude to a fractional tile-y coordinate.
func latToTileY(lat, tileN float64) float64 {
	latR := lat * math.Pi / 180.0
	return (1.0 - math.Log(math.Tan(math.Pi/4.0+latR/2.0))/math.Pi) / 2.0 * tileN
}

// tileXToLon converts a fractional tile-x coordinate back to longitude.
func tileXToLon(tx, tileN float64) float64 {
	return tx/tileN*360.0 - 180.0
}

// tileYToLat converts a fractional tile-y coordinate back to latitude.
func tileYToLat(ty, tileN float64) float64 {
	n := math.Pi - 2.0*math.Pi*ty/tileN
	return 180.0 / math.Pi * math.Atan(0.5*(math.Exp(n)-math.Exp(-n)))
}

// wrapLon returns the longitude equivalent to lon (mod 360°) that is nearest
// to centerLon. This keeps spots/arcs visible when the map has been panned
// past the antimeridian (±180°).
func wrapLon(lon, centerLon float64) float64 {
	diff := lon - centerLon
	diff -= math.Round(diff/360) * 360
	return centerLon + diff
}

// normLon folds an arbitrary longitude into the canonical [-180, 180) range.
func normLon(lon float64) float64 {
	return lon - math.Floor((lon+180)/360)*360
}

// blitTile copies a 256×256 tile into dst at (dstX, dstY) scaled to dstSize×dstSize.
// Uses nearest-neighbour scaling; clips to dst bounds automatically.
func blitTile(dst *image.RGBA, tile *image.RGBA, dstX, dstY, dstSize int) {
	const srcSize = 256
	b := dst.Bounds()
	dstPix := dst.Pix
	dstStride := dst.Stride
	tilePix := tile.Pix
	tileStride := tile.Stride
	for dy := 0; dy < dstSize; dy++ {
		py := dstY + dy
		if py < b.Min.Y || py >= b.Max.Y {
			continue
		}
		sy := dy * srcSize / dstSize
		dstRow := py * dstStride
		tileRow := sy * tileStride
		for dx := 0; dx < dstSize; dx++ {
			px := dstX + dx
			if px < b.Min.X || px >= b.Max.X {
				continue
			}
			sx := dx * srcSize / dstSize
			dOff := dstRow + px*4
			sOff := tileRow + sx*4
			dstPix[dOff] = tilePix[sOff]
			dstPix[dOff+1] = tilePix[sOff+1]
			dstPix[dOff+2] = tilePix[sOff+2]
			dstPix[dOff+3] = tilePix[sOff+3]
		}
	}
}

// gridToLatLon converts a 4- or 6-char Maidenhead grid to the centre lat/lon.
// 4-char (e.g. DM13): ~2°×1° cell, ±100 km precision.
// 6-char (e.g. DM13ab): ~5'×2.5' cell, ±5 km precision.
func gridToLatLon(grid string) (lat, lon float64, ok bool) {
	g := strings.ToUpper(strings.TrimSpace(grid))
	if len(g) < 4 {
		return 0, 0, false
	}
	if g[0] < 'A' || g[0] > 'R' || g[1] < 'A' || g[1] > 'R' {
		return 0, 0, false
	}
	if g[2] < '0' || g[2] > '9' || g[3] < '0' || g[3] > '9' {
		return 0, 0, false
	}
	lon = float64(g[0]-'A')*20 - 180 + float64(g[2]-'0')*2
	lat = float64(g[1]-'A')*10 - 90 + float64(g[3]-'0')
	if len(g) >= 6 && g[4] >= 'A' && g[4] <= 'X' && g[5] >= 'A' && g[5] <= 'X' {
		lon += float64(g[4]-'A')*2.0/24.0 + 1.0/24.0
		lat += float64(g[5]-'A')*1.0/24.0 + 0.5/24.0
	} else {
		lon += 1
		lat += 0.5
	}
	return lat, lon, true
}

func isGridSquare(s string) bool {
	_, _, ok := gridToLatLon(s)
	return ok
}

func parseSpotMsg(text, myCall string) (call, grid, otaType string, ok bool) {
	mine := strings.ToUpper(myCall)
	for _, tok := range strings.Fields(strings.ToUpper(text)) {
		// Filter FT8 protocol keywords BEFORE the grid check — "RR73" passes
		// isGridSquare() (R, R, digit, digit) and would be mis-plotted at
		// lon=175°, lat=83° if we don't catch it here first.
		switch tok {
		case "CQ", "DE", "DX", "73", "RR73", "RRR", "TNX", "TU", "R":
			continue
		}
		if ota, isOTA := otaKeywords[tok]; isOTA {
			otaType = ota
			continue
		}
		if isGridSquare(tok) {
			grid = tok
			continue
		}
		switch tok {
		case "":
			continue
		}
		if tok == mine {
			continue
		}
		if len(tok) >= 3 && len(tok) <= 10 {
			hasL, hasD := false, false
			for _, c := range tok {
				if c >= 'A' && c <= 'Z' {
					hasL = true
				}
				if c >= '0' && c <= '9' {
					hasD = true
				}
			}
			if hasL && hasD {
				call = tok
			}
		}
	}
	return call, grid, otaType, call != "" && grid != ""
}

// spotColour returns the dot color for a station spot.
// workedStatus: 0 = unworked (green), 1 = worked in same grid (blue), 2 = worked this call (red).
// Brightness fades over 60 minutes to a 25% minimum.
func spotColour(workedStatus int, seen time.Time) color.RGBA {
	age := time.Since(seen).Minutes()
	bright := math.Max(0.25, 1-age/60)
	b := func(v float64) uint8 { return uint8(v * bright) }
	switch workedStatus {
	case 2: // worked this call — red
		return color.RGBA{b(220), b(50), b(50), 255}
	case 1: // worked in same grid — blue
		return color.RGBA{b(60), b(110), b(230), 255}
	default: // unworked — green
		return color.RGBA{0, b(210), b(80), 255}
	}
}

// --- drawing primitives ---

func mapDrawDot(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	b := img.Bounds()
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				x, y := cx+dx, cy+dy
				if x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}
}

// otaBadgeStyle returns the short label + bg/fg colour pair for one
// of the recognised *OTA programmes. Same palette as the HEARD
// roster's per-program badge so the operator sees the same visual
// identity in both views (green park / amber summit / teal flora /
// etc). Returns ok=false for unrecognised types.
func otaBadgeStyle(otaType string) (label string, bg, fg color.RGBA, ok bool) {
	white := color.RGBA{255, 255, 255, 255}
	switch otaType {
	case "POTA":
		return "P", color.RGBA{60, 180, 75, 255}, white, true
	case "SOTA":
		return "S", color.RGBA{220, 130, 30, 255}, white, true
	case "WWFF":
		return "WF", color.RGBA{50, 200, 170, 255}, white, true
	case "IOTA":
		return "I", color.RGBA{60, 130, 230, 255}, white, true
	case "BOTA":
		return "B", color.RGBA{90, 170, 240, 255}, white, true
	case "LOTA":
		return "L", color.RGBA{240, 200, 60, 255}, white, true
	case "NOTA":
		return "N", color.RGBA{180, 110, 200, 255}, white, true
	case "PORTABLE":
		return "/P", color.RGBA{160, 160, 160, 255}, white, true
	}
	return "", color.RGBA{}, color.RGBA{}, false
}

// mapDrawCircleBadgeLabel draws a filled circle with up to 3 chars of
// tinyFont text centred inside. Used for the per-station status
// badges (CQ / OTA) drawn just to the left of a callsign label on the
// zoomed-in map view.
func mapDrawCircleBadgeLabel(img *image.RGBA, cx, cy, r int, label string, bg, fg color.RGBA) {
	mapDrawDot(img, cx, cy, r, bg)
	// tinyFont chars are 4 wide × 5 tall (with 1-px advance gap → 5 px
	// per char); pixelWidth excludes the trailing gap.
	if label == "" {
		return
	}
	const charW = 5
	const charH = 5
	w := len(label)*charW - 1
	tx := cx - w/2
	ty := cy - charH/2
	mapDrawTinyTextOutlined(img, label, tx, ty, fg)
}

func mapDrawDiamond(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	b := img.Bounds()
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if mapAbs(dx)+mapAbs(dy) <= r {
				x, y := cx+dx, cy+dy
				if x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}
}

func mapDrawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	bnd := img.Bounds()
	dx := mapAbs(x1 - x0)
	sx := 1
	if x0 > x1 {
		sx = -1
	}
	dy := -mapAbs(y1 - y0)
	sy := 1
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		if x0 >= bnd.Min.X && x0 < bnd.Max.X && y0 >= bnd.Min.Y && y0 < bnd.Max.Y {
			img.SetRGBA(x0, y0, c)
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func mapVline(img *image.RGBA, x, y0, y1 int, c color.RGBA) {
	b := img.Bounds()
	if x < b.Min.X || x >= b.Max.X {
		return
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if y1 >= b.Max.Y {
		y1 = b.Max.Y - 1
	}
	for y := y0; y <= y1; y++ {
		img.SetRGBA(x, y, c)
	}
}

func mapHline(img *image.RGBA, x0, x1, y int, c color.RGBA) {
	b := img.Bounds()
	if y < b.Min.Y || y >= b.Max.Y {
		return
	}
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if x1 >= b.Max.X {
		x1 = b.Max.X - 1
	}
	for x := x0; x <= x1; x++ {
		img.SetRGBA(x, y, c)
	}
}

// mapFillRect alpha-blends c over img across the rectangle (x0,y0)–(x1,y1).
// Coordinates are auto-normalised and clipped to img's bounds.
func mapFillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	b := img.Bounds()
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if x1 > b.Max.X-1 {
		x1 = b.Max.X - 1
	}
	if y1 > b.Max.Y-1 {
		y1 = b.Max.Y - 1
	}
	if x0 > x1 || y0 > y1 {
		return
	}
	a := uint32(c.A)
	if a == 0 {
		return
	}
	rSrc := uint32(c.R) * a
	gSrc := uint32(c.G) * a
	bSrc := uint32(c.B) * a
	inv := 255 - a
	for y := y0; y <= y1; y++ {
		off := img.PixOffset(x0, y)
		for x := x0; x <= x1; x++ {
			dr := uint32(img.Pix[off])
			dg := uint32(img.Pix[off+1])
			db := uint32(img.Pix[off+2])
			img.Pix[off] = uint8((rSrc + dr*inv) / 255)
			img.Pix[off+1] = uint8((gSrc + dg*inv) / 255)
			img.Pix[off+2] = uint8((bSrc + db*inv) / 255)
			img.Pix[off+3] = 255
			off += 4
		}
	}
}

// callsignFont returns a font.Face suitable for spot labels at the given ppd
// (pixels per degree). At typical zooms it returns the 13-px bitmap face; at
// high zooms it returns a larger TrueType face so callsigns remain readable
// and clickable. Faces are cached per size.
var (
	bigFontMu    sync.Mutex
	bigFontBase  *opentype.Font
	bigFontCache = map[int]font.Face{}
)

func callsignFont(ppd float64) (face font.Face, advance int, ascent int) {
	size := 0
	switch {
	case ppd >= 8000:
		size = 22
	case ppd >= 2000:
		size = 18
	case ppd >= 500:
		size = 15
	}
	if size == 0 {
		return basicfont.Face7x13, 7, 10
	}

	bigFontMu.Lock()
	defer bigFontMu.Unlock()
	if f, ok := bigFontCache[size]; ok {
		adv := int(font.MeasureString(f, "W").Ceil())
		return f, adv, f.Metrics().Ascent.Ceil()
	}
	if bigFontBase == nil {
		parsed, err := opentype.Parse(gomonobold.TTF)
		if err != nil {
			return basicfont.Face7x13, 7, 10
		}
		bigFontBase = parsed
	}
	f, err := opentype.NewFace(bigFontBase, &opentype.FaceOptions{
		Size:    float64(size),
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return basicfont.Face7x13, 7, 10
	}
	bigFontCache[size] = f
	adv := int(font.MeasureString(f, "W").Ceil())
	return f, adv, f.Metrics().Ascent.Ceil()
}

func mapDrawText(img *image.RGBA, text string, x, y int, c color.RGBA, face font.Face) {
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(text)
}

// mapDrawTextOutlined renders text with a dark outline for legibility on any
// background, then draws the label colour on top twice (x and x+1) for a
// simulated bold weight.
func mapDrawTextOutlined(img *image.RGBA, text string, x, y int, c color.RGBA, face font.Face) {
	outline := color.RGBA{0, 0, 0, 200}
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			mapDrawText(img, text, x+dx, y+dy, outline, face)
		}
	}
	mapDrawText(img, text, x+1, y, c, face)
	mapDrawText(img, text, x, y, c, face)
}

// mapDrawBitmap plots a set of pixel offsets relative to (cx,cy). scale >= 1
// expands each source pixel into a scale×scale block, keeping the icon's
// design-time shape crisp at larger sizes (plain integer upscale — no
// interpolation, so pixel art stays sharp).
func mapDrawBitmap(img *image.RGBA, cx, cy int, pixels [][2]int, c color.RGBA, scale int) {
	if scale < 1 {
		scale = 1
	}
	b := img.Bounds()
	for _, p := range pixels {
		baseX := cx + p[0]*scale
		baseY := cy + p[1]*scale
		for dy := 0; dy < scale; dy++ {
			for dx := 0; dx < scale; dx++ {
				x, y := baseX+dx, baseY+dy
				if x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}
}

// OTA icon bitmaps — pixel offsets from (cx, cy).

// POTA (Parks on the Air) — pine tree.
var bitmapPOTA = [][2]int{
	{0, -5},
	{-1, -4}, {0, -4}, {1, -4},
	{-2, -3}, {-1, -3}, {0, -3}, {1, -3}, {2, -3},
	{0, -2}, {0, -1},
}

// SOTA (Summits on the Air) — mountain peak.
var bitmapSOTA = [][2]int{
	{0, -5},
	{-1, -4}, {0, -4}, {1, -4},
	{-2, -3}, {-1, -3}, {0, -3}, {1, -3}, {2, -3},
	{-3, -2}, {-2, -2}, {-1, -2}, {0, -2}, {1, -2}, {2, -2}, {3, -2},
}

// WWFF (World Wide Flora & Fauna) — leaf with stem.
var bitmapWWFF = [][2]int{
	{0, -5},
	{-1, -4}, {0, -4}, {1, -4},
	{-1, -3}, {0, -3}, {1, -3},
	{-1, -2}, {0, -2}, {1, -2},
	{0, -1}, {0, 0},
}

// IOTA (Islands on the Air) — island mound over waves.
var bitmapIOTA = [][2]int{
	{-1, -4}, {0, -4}, {1, -4},
	{-2, -3}, {-1, -3}, {0, -3}, {1, -3}, {2, -3},
}
var bitmapIOTAWave = [][2]int{ // drawn in lighter blue
	{-2, -2}, {0, -2}, {2, -2},
	{-1, -1}, {1, -1},
}

// BOTA (Beaches on the Air) — waves.
var bitmapBOTA = [][2]int{
	{-2, -4}, {0, -4}, {2, -4},
	{-1, -3}, {1, -3},
	{-2, -2}, {0, -2}, {2, -2},
	{-1, -1}, {1, -1},
}

// LOTA (Lighthouses on the Air) — tower with light beam.
var bitmapLOTA = [][2]int{
	{-2, -5}, {0, -5}, {2, -5}, // beam
	{0, -4}, // top
	{0, -3}, // shaft
	{-1, -2}, {0, -2}, {1, -2},
	{-1, -1}, {0, -1}, {1, -1},
	{-2, 0}, {-1, 0}, {0, 0}, {1, 0}, {2, 0}, // base
}

// NOTA (Nuns on the Air) — latin cross.
var bitmapNOTA = [][2]int{
	{0, -5},
	{0, -4},
	{-2, -3}, {-1, -3}, {0, -3}, {1, -3}, {2, -3}, // crossbar
	{0, -2},
	{0, -1},
	{0, 0},
}

// otaLegendEntries defines display order and short labels for the legend.
var otaLegendEntries = []struct{ key, label string }{
	{"POTA", "Parks"},
	{"SOTA", "Summits"},
	{"WWFF", "Flora & Fauna"},
	{"IOTA", "Islands"},
	{"BOTA", "Beaches"},
	{"LOTA", "Lighthouses"},
	{"NOTA", "Nuns"},
}

// mapDrawOTAIcon draws the appropriate small icon for a spotted OTA station.
// (cx,cy) should be the station dot position; the icon is drawn above it.
// scale >= 1 grows the icon uniformly — used at 2× on the map so *OTA markers
// pop against dense spot clusters, and at 1× in the legend so its box layout
// isn't distorted.
func mapDrawOTAIcon(img *image.RGBA, cx, cy int, otaType string, scale int) {
	switch otaType {
	case "POTA":
		mapDrawBitmap(img, cx, cy, bitmapPOTA, color.RGBA{60, 210, 80, 255}, scale)
	case "SOTA":
		mapDrawBitmap(img, cx, cy, bitmapSOTA, color.RGBA{220, 150, 50, 255}, scale)
	case "WWFF":
		mapDrawBitmap(img, cx, cy, bitmapWWFF, color.RGBA{50, 210, 180, 255}, scale)
	case "IOTA":
		mapDrawBitmap(img, cx, cy, bitmapIOTA, color.RGBA{200, 180, 100, 255}, scale)
		mapDrawBitmap(img, cx, cy, bitmapIOTAWave, color.RGBA{80, 160, 240, 255}, scale)
	case "BOTA":
		mapDrawBitmap(img, cx, cy, bitmapBOTA, color.RGBA{80, 180, 240, 255}, scale)
	case "LOTA":
		mapDrawBitmap(img, cx, cy, bitmapLOTA, color.RGBA{240, 220, 100, 255}, scale)
	case "NOTA":
		mapDrawBitmap(img, cx, cy, bitmapNOTA, color.RGBA{200, 150, 220, 255}, scale)
	}
}

// mapDrawQSOArc draws a quadratic bezier arc between the operator's station
// and the QSO partner's screen position.
//
//	txPhase=true  → arc bows upward  (cyan)  — we are transmitting to them
//	txPhase=false → arc bows downward (amber) — we are receiving from them
func mapDrawQSOArc(img *image.RGBA, sx, sy, ex, ey int, txPhase bool) {
	dx := float64(ex - sx)
	dy := float64(ey - sy)
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist < 8 {
		return
	}

	// Bow height: 1/3 of chord length, clamped to [28, 140] px.
	bow := dist / 3
	if bow < 28 {
		bow = 28
	}
	if bow > 140 {
		bow = 140
	}

	cx := float64(sx+ex) / 2
	cy := float64(sy+ey) / 2
	if txPhase {
		cy -= bow
	} else {
		cy += bow
	}

	var c color.RGBA
	if txPhase {
		c = color.RGBA{0, 210, 255, 210} // cyan — TX
	} else {
		c = color.RGBA{255, 195, 40, 210} // amber — RX
	}

	// Quadratic bezier rendered as a polyline, drawn twice for 2px thickness.
	const steps = 120
	for yOff := 0; yOff <= 1; yOff++ {
		fsy := float64(sy + yOff)
		fey := float64(ey + yOff)
		fcy := cy + float64(yOff)
		px, py := float64(sx), fsy
		for i := 1; i <= steps; i++ {
			t := float64(i) / steps
			t1 := 1 - t
			nx := t1*t1*float64(sx) + 2*t*t1*cx + t*t*float64(ex)
			ny := t1*t1*fsy + 2*t*t1*fcy + t*t*fey
			mapDrawLine(img, int(px), int(py), int(nx), int(ny), c)
			px, py = nx, ny
		}
	}

	// Arrowhead at the destination, oriented along the bezier tangent
	// at t=1 — `2*(P2 - P1)` for a quadratic, so the unit vector is
	// (ex,ey) − (cx,cy) normalised. txPhase=true (we TX) → arrow points
	// at the partner; txPhase=false (we RX) → arrow points back at us.
	tipX, tipY := float64(ex), float64(ey)
	tanX, tanY := tipX-cx, tipY-cy
	if !txPhase {
		// RX: flip so the arrow points toward our station instead.
		tanX, tanY = -tanX, -tanY
		tipX, tipY = float64(sx), float64(sy)
	}
	tlen := math.Sqrt(tanX*tanX + tanY*tanY)
	if tlen > 0 {
		const arrowLen, arrowHalfW = 14.0, 7.0
		ux, uy := tanX/tlen, tanY/tlen
		bcx := tipX - arrowLen*ux
		bcy := tipY - arrowLen*uy
		// Perpendicular to the tangent.
		px, py := -uy, ux
		ax := int(tipX)
		ay := int(tipY)
		bx := int(bcx + arrowHalfW*px)
		by := int(bcy + arrowHalfW*py)
		cxi := int(bcx - arrowHalfW*px)
		cyi := int(bcy - arrowHalfW*py)
		mapFillTriangle(img, ax, ay, bx, by, cxi, cyi, c)
	}
}

// mapFillTriangle scan-fills a small triangle using sign-of-edge tests
// over the bounding box. Only used for QSO-arc arrowheads (≤16 px on
// a side) so the O(area) cost is negligible.
func mapFillTriangle(img *image.RGBA, ax, ay, bx, by, cx, cy int, col color.RGBA) {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	minX, maxX := ax, ax
	if bx < minX {
		minX = bx
	}
	if cx < minX {
		minX = cx
	}
	if bx > maxX {
		maxX = bx
	}
	if cx > maxX {
		maxX = cx
	}
	minY, maxY := ay, ay
	if by < minY {
		minY = by
	}
	if cy < minY {
		minY = cy
	}
	if by > maxY {
		maxY = by
	}
	if cy > maxY {
		maxY = cy
	}
	if minX < 0 {
		minX = 0
	}
	if minY < 0 {
		minY = 0
	}
	if maxX >= w {
		maxX = w - 1
	}
	if maxY >= h {
		maxY = h - 1
	}
	edge := func(x1, y1, x2, y2, x3, y3 int) int {
		return (x1-x3)*(y2-y3) - (x2-x3)*(y1-y3)
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d1 := edge(x, y, ax, ay, bx, by)
			d2 := edge(x, y, bx, by, cx, cy)
			d3 := edge(x, y, cx, cy, ax, ay)
			hasNeg := d1 < 0 || d2 < 0 || d3 < 0
			hasPos := d1 > 0 || d2 > 0 || d3 > 0
			if !(hasNeg && hasPos) {
				img.SetRGBA(x, y, col)
			}
		}
	}
}

// mapDrawMeshcoreLegend draws a compact bottom-right swatch
// explaining the MeshCore overlay's colour code: one row per
// node-type dot (Repeater / Companion / Room / Sensor) plus a
// "Route" line and "Unknown hop" placeholder. Lives in the
// opposite corner from the FT8 OTA legend so the two never
// collide visually if both modes' legends were ever shown
// simultaneously.
func mapDrawMeshcoreLegend(img *image.RGBA, w, h int) {
	type legendEntry struct {
		label    string
		dotColor color.RGBA
		dotR     int  // dot radius
		lineSeg  bool // true → render as a line segment instead of a dot
		ringOnly bool // true → outline (placeholder hop)
		diamond  bool // true → diamond (operator's own station)
	}
	entries := []legendEntry{
		{label: "You", dotColor: color.RGBA{255, 220, 0, 255}, dotR: 5, diamond: true},
		{label: "Repeater", dotColor: color.RGBA{220, 60, 60, 255}, dotR: 5},
		{label: "Companion", dotColor: color.RGBA{80, 150, 240, 255}, dotR: 5},
		{label: "Room", dotColor: color.RGBA{60, 200, 100, 255}, dotR: 5},
		{label: "Sensor", dotColor: color.RGBA{230, 160, 40, 255}, dotR: 5},
		{label: "Route", dotColor: color.RGBA{60, 200, 230, 220}, lineSeg: true},
		{label: "Hop", dotColor: color.RGBA{120, 220, 240, 255}, dotR: 5},
		{label: "Unknown hop", dotColor: color.RGBA{160, 165, 175, 200}, dotR: 3, ringOnly: true},
	}

	const (
		rowH        = 18
		swatchX     = 14 // centre of the dot/line column inside the box
		textX       = 30
		padX        = 8
		padY        = 6
		glyphW      = 7
		rightGutter = 8
	)
	maxLabelChars := 0
	for _, e := range entries {
		if len(e.label) > maxLabelChars {
			maxLabelChars = len(e.label)
		}
	}
	boxW := textX + maxLabelChars*glyphW + rightGutter
	boxH := len(entries)*rowH + padY*2

	// Bottom-right corner — opposite of the FT8 OTA legend so
	// they don't fight for the same real estate.
	bx := w - boxW - padX
	by := h - boxH - padX

	bg := color.RGBA{5, 15, 35, 210}
	border := color.RGBA{40, 80, 130, 255}
	for y := by; y <= by+boxH; y++ {
		for x := bx; x <= bx+boxW; x++ {
			if x < 0 || x >= w || y < 0 || y >= h {
				continue
			}
			if x == bx || x == bx+boxW || y == by || y == by+boxH {
				img.SetRGBA(x, y, border)
			} else {
				img.SetRGBA(x, y, bg)
			}
		}
	}

	face := basicfont.Face7x13
	labelCol := color.RGBA{200, 215, 240, 255}
	for i, e := range entries {
		iy := by + padY + i*rowH + rowH/2
		switch {
		case e.diamond:
			mapDrawDiamond(img, bx+swatchX, iy, e.dotR, e.dotColor)
		case e.lineSeg:
			// Short horizontal stroke showing the route colour.
			x0 := bx + swatchX - 6
			x1 := bx + swatchX + 6
			mapDrawLine(img, x0, iy, x1, iy, e.dotColor)
			// Slight thickness — second pass one pixel below.
			mapDrawLine(img, x0, iy+1, x1, iy+1, e.dotColor)
		case e.ringOnly:
			// Ring outline communicates "placeholder / unknown".
			r := e.dotR + 1
			for dx := -r; dx <= r; dx++ {
				for dy := -r; dy <= r; dy++ {
					d2 := dx*dx + dy*dy
					if d2 >= (r-1)*(r-1) && d2 <= r*r {
						img.SetRGBA(bx+swatchX+dx, iy+dy, e.dotColor)
					}
				}
			}
		default:
			mapDrawDot(img, bx+swatchX, iy, e.dotR, e.dotColor)
		}
		mapDrawTextOutlined(img, e.label, bx+textX, iy+4, labelCol, face)
	}
}

// mapDrawLegend draws the OTA icon legend in the bottom-left corner.
// Compact layout: small (16 px) badge + label per row, tight rows.
func mapDrawLegend(img *image.RGBA, w, h int) {
	const (
		badgeSize = 16
		rowH      = badgeSize + 2
		iconX     = badgeSize/2 + 6
		textX     = badgeSize + 12
		padX      = 8
		padY      = 6
		// basicfont.Face7x13 advances 7 px per glyph; right-edge gutter
		// keeps the longest label off the border.
		glyphW      = 7
		rightGutter = 8
	)
	maxLabelChars := 0
	for _, e := range otaLegendEntries {
		n := len(e.key) + 1 + len(e.label) // "POTA Parks"
		if n > maxLabelChars {
			maxLabelChars = n
		}
	}
	boxW := textX + maxLabelChars*glyphW + rightGutter
	boxH := len(otaLegendEntries)*rowH + padY*2
	bx := padX
	by := h - boxH - padX

	bg := color.RGBA{5, 15, 35, 210}
	border := color.RGBA{40, 80, 130, 255}

	for y := by; y <= by+boxH; y++ {
		for x := bx; x <= bx+boxW; x++ {
			if x < 0 || x >= w || y < 0 || y >= h {
				continue
			}
			if x == bx || x == bx+boxW || y == by || y == by+boxH {
				img.SetRGBA(x, y, border)
			} else {
				img.SetRGBA(x, y, bg)
			}
		}
	}

	face := basicfont.Face7x13
	labelCol := color.RGBA{200, 215, 240, 255}
	for i, e := range otaLegendEntries {
		iy := by + padY + i*rowH + rowH/2
		drawBadgeIcon(img, bx+iconX, iy, e.key, badgeSize)
		mapDrawTextOutlined(img, e.key+" "+e.label, bx+textX, iy+4, labelCol, face)
	}
}

// tinyFont is a 4×5 bitmap font for callsign labels.
// Each entry is [5]uint8; bits 3:0 of each byte are the 4 pixel columns of
// one row (bit 3 = leftmost). Character advance = 5 px (4 wide + 1 gap).
var tinyFont = map[rune][5]uint8{
	' ': {0, 0, 0, 0, 0},
	'A': {6, 9, 15, 9, 9},
	'B': {14, 9, 14, 9, 14},
	'C': {7, 8, 8, 8, 7},
	'D': {14, 9, 9, 9, 14},
	'E': {15, 8, 14, 8, 15},
	'F': {15, 8, 14, 8, 8},
	'G': {7, 8, 11, 9, 7},
	'H': {9, 9, 15, 9, 9},
	'I': {14, 4, 4, 4, 14},
	'J': {7, 1, 1, 9, 6},
	'K': {9, 10, 12, 10, 9},
	'L': {8, 8, 8, 8, 15},
	'M': {9, 15, 9, 9, 9},
	'N': {9, 13, 11, 9, 9},
	'O': {6, 9, 9, 9, 6},
	'P': {14, 9, 14, 8, 8},
	'Q': {6, 9, 9, 11, 7},
	'R': {14, 9, 14, 10, 9},
	'S': {7, 8, 6, 1, 14},
	'T': {15, 4, 4, 4, 4},
	'U': {9, 9, 9, 9, 6},
	'V': {9, 9, 9, 6, 6},
	'W': {9, 9, 13, 11, 9},
	'X': {9, 6, 6, 6, 9},
	'Y': {9, 9, 6, 4, 4},
	'Z': {15, 1, 2, 4, 15},
	'0': {6, 9, 9, 9, 6},
	'1': {2, 6, 2, 2, 7},
	'2': {6, 9, 1, 6, 15},
	'3': {6, 1, 6, 1, 6},
	'4': {9, 9, 15, 1, 1},
	'5': {15, 8, 14, 1, 14},
	'6': {6, 8, 14, 9, 6},
	'7': {15, 1, 2, 4, 4},
	'8': {6, 9, 6, 9, 6},
	'9': {6, 9, 7, 1, 6},
	'/': {1, 2, 4, 4, 8},
	'-': {0, 0, 14, 0, 0},
}

// mapDrawTinyText renders text with the 4×5 tinyFont.
// (x,y) is the top-left corner of the first character.
func mapDrawTinyText(img *image.RGBA, text string, x, y int, c color.RGBA) {
	b := img.Bounds()
	cx := x
	for _, ch := range strings.ToUpper(text) {
		rows, ok := tinyFont[ch]
		if !ok {
			cx += 5
			continue
		}
		for row, mask := range rows {
			for col := 0; col < 4; col++ {
				if mask&(1<<uint(3-col)) != 0 {
					px, py := cx+col, y+row
					if px >= b.Min.X && px < b.Max.X && py >= b.Min.Y && py < b.Max.Y {
						img.SetRGBA(px, py, c)
					}
				}
			}
		}
		cx += 5
	}
}

// mapDrawTinyTextOutlined renders tiny text with a 1-pixel dark outline.
func mapDrawTinyTextOutlined(img *image.RGBA, text string, x, y int, c color.RGBA) {
	outline := color.RGBA{0, 0, 0, 180}
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			mapDrawTinyText(img, text, x+dx, y+dy, outline)
		}
	}
	mapDrawTinyText(img, text, x, y, c)
}

func mapAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
