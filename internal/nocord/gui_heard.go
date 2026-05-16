package nocord

// gui_heard.go — HEARD-sidebar plumbing: the in-memory roster of
// callsigns we've decoded on the active band, the floating tooltip
// shown on row hover (country + worked-history), and the small
// glue helpers (scroll-to-call, blink-on-highlight, worked
// summary against ADIF + LoTW).
//
// Methods that touch HEARD *and* other surfaces (sweepStaleRoster
// also prunes map spots; selectCall drives chat/waterfall;
// confirmStatusForCall is read by chat rows; workedStatusForCall
// is consumed by the map widget) deliberately stay in gui.go so
// the cross-feature coupling is visible in one place.

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
)

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

// heardRow is the materialised view of a heardEntry — flattened from
// the keyed map into a slice the list widget can iterate by index.
// Built fresh by heardSnapshot on every refresh; cheap at maxHeard.
type heardRow struct {
	call        string
	snr         float64
	when        time.Time
	lastCQ      time.Time
	lastOTA     time.Time
	lastOTAType string
}

// rememberHeard records that we just decoded a transmission FROM call.
// Caps the in-memory roster at maxHeard, dropping the oldest half once
// the cap is exceeded. Triggers a list redraw on the UI thread.
func (g *GUI) rememberHeard(call string, snr float64, isCQ bool, otaType string) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" || strings.HasPrefix(call, "<") {
		return
	}
	// Last-line-of-defence callsign-shape gate: senderFromMessage
	// already filters CQ modifiers (ASIA / BY / numeric zones / …),
	// but any future change to that path could leak grid squares,
	// reports (R-18), or sign-offs (RR73) into the roster. Require
	// at least one letter + one digit — eliminates the obvious
	// non-callsign tokens without an exhaustive ITU-prefix table.
	if !isPlausibleCallsign(call) {
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

// heardSnapshot returns the HEARD map flattened into a slice sorted
// per the active heardSort mode. Built fresh on every list redraw —
// small N (≤ maxHeard) keeps this trivially cheap. Decouples the
// list callbacks from the live map so they don't need to hold g.mu
// while drawing.
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

// showHeardTooltip pops a floating overlay near the HEARD row the
// cursor is hovering — country first, then a multi-line worked-
// history summary so the operator can decide whether to chase or
// skip without leaving the HEARD list. Idempotent: re-hovering the
// same call within the debounce window is a no-op (avoids overlay
// thrash during cursor jitter on a single row).
func (g *GUI) showHeardTooltip(call, country string) {
	if g.window == nil {
		return
	}
	worked := g.workedSummaryForCall(call)
	if country == "" && worked == "" {
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

	bodyLines := make([]string, 0, 4)
	if country != "" {
		bodyLines = append(bodyLines, country)
	}
	if worked != "" {
		bodyLines = append(bodyLines, strings.Split(worked, "\n")...)
	}
	textCol := color.RGBA{220, 225, 235, 255}
	rows := make([]fyne.CanvasObject, 0, len(bodyLines))
	for i, line := range bodyLines {
		t := canvas.NewText(line, textCol)
		t.TextStyle = fyne.TextStyle{Monospace: true}
		t.TextSize = 11
		// Subdued colour for everything past the country header so
		// the eye lands on the country first; worked-history reads
		// as supporting detail.
		if i > 0 {
			t.Color = color.RGBA{180, 185, 195, 255}
		}
		rows = append(rows, t)
	}
	bg := canvas.NewRectangle(color.RGBA{30, 32, 38, 240})
	bg.StrokeColor = color.RGBA{90, 95, 105, 255}
	bg.StrokeWidth = 1
	wrapped := container.NewStack(bg, container.NewPadded(container.NewVBox(rows...)))
	wrapped.Resize(wrapped.MinSize())

	g.mu.Lock()
	g.heardTooltip = wrapped
	g.heardTooltipCall = call
	g.mu.Unlock()
	fyne.Do(func() {
		g.window.Canvas().Overlays().Add(wrapped)
	})
}

// updateHeardTooltipPos repositions the tooltip near the cursor.
// Called from the hoverRow MouseMoved handler so the tooltip tracks
// the pointer instead of being pinned to a fixed corner of the
// column.
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
// overlay stack if one is currently displayed. Safe to call when
// none is showing.
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

// workedSummaryForCall returns a short multi-line description of
// the operator's QSO history with the given call: total contact
// count, most-recent date + band, and LoTW-confirmed bands.
// Returns "Not worked before" when nothing matches. Used by the
// HEARD-list hover tooltip so the operator can decide at a glance
// whether to chase a station (new one) or skip (already in the
// log on this band, or LoTW-confirmed).
//
// All scans are in-memory against g.adifLog + g.lotwQSLs.
func (g *GUI) workedSummaryForCall(call string) string {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return ""
	}
	g.mu.Lock()
	type bandHit struct {
		band string
		when time.Time
	}
	var hits []bandHit
	for _, r := range g.adifLog {
		if strings.EqualFold(r.TheirCall, call) {
			hits = append(hits, bandHit{band: r.Band, when: r.TimeOn})
		}
	}
	confirmedBands := map[string]bool{}
	for _, q := range g.lotwQSLs {
		if q.Confirmed && strings.EqualFold(q.Call, call) {
			confirmedBands[strings.ToUpper(q.Band)] = true
		}
	}
	g.mu.Unlock()
	if len(hits) == 0 {
		return "Not worked before"
	}
	// Most-recent first.
	var mostRecent bandHit
	for _, h := range hits {
		if h.when.After(mostRecent.when) {
			mostRecent = h
		}
	}
	bandsSet := map[string]bool{}
	for _, h := range hits {
		bandsSet[strings.ToUpper(h.band)] = true
	}
	bandList := make([]string, 0, len(bandsSet))
	for b := range bandsSet {
		bandList = append(bandList, b)
	}
	sort.Strings(bandList)
	out := fmt.Sprintf("Worked %d×", len(hits))
	if !mostRecent.when.IsZero() {
		out += fmt.Sprintf(" — last %s on %s",
			mostRecent.when.Format("2006-01-02"),
			strings.ToUpper(mostRecent.band))
	}
	if len(bandList) > 1 {
		out += fmt.Sprintf("\nBands: %s", strings.Join(bandList, ", "))
	}
	if len(confirmedBands) > 0 {
		confirmedList := make([]string, 0, len(confirmedBands))
		for b := range confirmedBands {
			confirmedList = append(confirmedList, b)
		}
		sort.Strings(confirmedList)
		out += fmt.Sprintf("\nLoTW: confirmed on %s", strings.Join(confirmedList, ", "))
	} else {
		out += "\nLoTW: not confirmed"
	}
	return out
}

// scrollHeardToCall finds the row matching call in the live HEARD
// snapshot and scrolls the usersList widget to it, selecting briefly
// to flash a visual cue. Sets suppressHeardSelectAction so the
// resulting OnSelected callback doesn't re-fire the magnification
// popup + recurse back into selectCall.
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
