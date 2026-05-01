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
	spotsMu   sync.RWMutex
	spots     map[string]spotEntry
	myGrid    string
	myLat     float64
	myLon     float64
	hoverCall string // callsign to highlight; empty = none

	// QSO propagation arc (guarded by spotsMu).
	// qsoCall is the active QSO partner's callsign; empty = no arc.
	// qsoLat/qsoLon are the explicit partner coordinates; (0,0) falls back to
	// the spots database at draw time. qsoTxPhase=true bows the arc upward
	// (we are transmitting), false bows it downward (we are receiving).
	qsoCall    string
	qsoLat     float64
	qsoLon     float64
	qsoTxPhase bool

	tiles              *tileCache
	raster             *canvas.Raster
	onSpotTap          func(call string)                       // called when the user taps a station dot
	onSpotSecondaryTap func(call string, absPos fyne.Position) // called when the user right-clicks a station dot

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
}

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
	m.raster.Refresh()
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
	m.raster.Refresh()
}

// DragEnd implements fyne.Draggable.
func (m *MapWidget) DragEnd() {}

// Refresh overrides BaseWidget.Refresh so external callers also force the
// raster to regenerate — the default BaseWidget path only repaints the
// renderer tree and leaves the cached raster image intact.
func (m *MapWidget) Refresh() {
	m.BaseWidget.Refresh()
	if m.raster != nil {
		m.raster.Refresh()
	}
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
		m.raster.Refresh()
	}
}

// Tapped implements fyne.Tappable. It hit-tests all visible station dots and
// calls onSpotTap with the nearest callsign within 12 pixels.
func (m *MapWidget) Tapped(ev *fyne.PointEvent) {
	if m.onSpotTap == nil {
		return
	}
	if call := m.hitTestSpot(ev.Position); call != "" {
		m.onSpotTap(call)
	}
}

// TappedSecondary implements fyne.SecondaryTappable. Right-click on a station
// dot invokes onSpotSecondaryTap, used to open the profile viewer.
func (m *MapWidget) TappedSecondary(ev *fyne.PointEvent) {
	if m.onSpotSecondaryTap == nil {
		return
	}
	if call := m.hitTestSpot(ev.Position); call != "" {
		m.onSpotSecondaryTap(call, ev.AbsolutePosition)
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
	m.raster.Refresh()
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
	m.raster.Refresh()
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
		m.raster.Refresh()
	}
}

// UpdateMyGrid refreshes the operator's own location marker.
func (m *MapWidget) UpdateMyGrid(grid string) {
	m.spotsMu.Lock()
	m.myGrid = grid
	m.myLat, m.myLon, _ = gridToLatLon(grid)
	m.spotsMu.Unlock()
	m.raster.Refresh()
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
	face := basicfont.Face7x13

	// Field grid (20°×10°) — always visible.
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

	// Grid-status overlay — three-colour tint on 4-char squares for the active
	// band: red for LoTW-confirmed (QSL), yellow for LoTW-known QSOs with no
	// QSL yet, blue for contacts we have locally but LoTW doesn't reflect.
	// Drawn beneath the square grid lines so boundaries stay visible.
	if showWorked && ppd >= 7 {
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
	if ppd >= 7 {
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
		if arcOK {
			mapDrawQSOArc(img, toXW(myLon), toY(myLat), arcX, arcY, qsoTxPhase)
		}
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

	mapDrawLegend(img, w, h)

	return img
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

	// Filled dot at the destination end to mark the target station.
	mapDrawDot(img, ex, ey, 5, c)
	mapDrawDot(img, ex, ey, 2, color.RGBA{255, 255, 255, 220})
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
