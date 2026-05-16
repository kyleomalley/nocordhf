package nocord

// gui_meshcore_rxlog.go — RX LOG viewer pane that sits beneath
// the map in MeshCore mode. Renders one row per
// PushLogRxData-decoded mesh packet; right-click opens an
// Inspect dialog (parsed metadata + hex dump) or kicks the
// path-on-map renderer. The (?) icon in the header opens a
// data-driven trace-routing reference dialog sampled from the
// in-memory ring.
//
// Path-rendering helpers that DRAW on the map for the operator's
// path/animation requests intentionally stay in gui.go /
// gui_meshcore_path.go — they're map-side rendering, not
// RxLog-viewer concerns. The viewer just hands off via
// mcShowPathForRxLog when the right-click "Show path on map"
// item fires.

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/kyleomalley/nocordhf/lib/logging"
	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

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
			// Two halves: a fixed metadata text on the left
			// (timestamp / route / payload / hops×hashSize /
			// SNR / RSSI / sender) and a horizontal flow on the
			// right for the path hashes. Each path hash renders
			// as a clickable mcHashLink — left-click flies the
			// map to that contact, matching the chat-row
			// inline-hash-link behaviour for "see this hop on
			// the map without leaving the RX LOG".
			meta := canvas.NewText("", color.RGBA{200, 205, 215, 255})
			meta.TextStyle = fyne.TextStyle{Monospace: true}
			meta.TextSize = 10
			pathFlow := container.NewHBox()
			body := container.NewHBox(container.NewPadded(meta), pathFlow)
			tip := newHoverTip(body, "")
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
			body := tip.inner.(*fyne.Container)
			padded := body.Objects[0].(*fyne.Container)
			meta := padded.Objects[0].(*canvas.Text)
			pathFlow := body.Objects[1].(*fyne.Container)
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
			contacts := append([]meshcore.Contact(nil), g.mcContacts...)
			g.mcMu.Unlock()
			hashSize := int(e.packet.PathLen>>6) + 1
			if e.packet.PathLen == 0xFF {
				hashSize = 0
			}
			// Sender nickname from the LAST path-hash byte —
			// best-effort "previous repeater that handed it
			// to us" cue.
			senderTag := "—"
			if hashSize > 0 && len(e.packet.Path) >= hashSize {
				lastHash := e.packet.Path[len(e.packet.Path)-hashSize:]
				if matched, _ := resolvePathHopHash(contacts, lastHash, 0, 0); matched != nil && matched.AdvName != "" {
					senderTag = matched.AdvName
					if len(senderTag) > 12 {
						senderTag = senderTag[:12]
					}
				} else {
					senderTag = fmt.Sprintf("%x?", lastHash)
				}
			}
			meta.Text = fmt.Sprintf("%s %-3s %-6s %dh×%dB %5.1f %4d %-12s",
				e.when.Format("15:04:05"),
				routeShort(e.route),
				payloadShort(e.payload),
				e.hops,
				hashSize,
				e.snr,
				e.rssi,
				senderTag,
			)
			meta.Refresh()
			// Rebuild the per-hop path flow: comma-separated
			// hex tokens, each a clickable mcHashLink when it
			// resolves to a known contact (left-click flies the
			// map there). Unresolved hops render as dim plain
			// text so the operator can still see them but can't
			// click to fly. Matches the chat-row inline-hash
			// behaviour for "see this hop on the map".
			pathFlow.RemoveAll()
			if hashSize > 0 && len(e.packet.Path) > 0 {
				hopCount := len(e.packet.Path) / hashSize
				dimCol := color.RGBA{140, 145, 155, 255}
				for h := 0; h < hopCount; h++ {
					if h > 0 {
						sep := canvas.NewText(",", dimCol)
						sep.TextStyle = fyne.TextStyle{Monospace: true}
						sep.TextSize = 10
						pathFlow.Add(sep)
					}
					hashBytes := e.packet.Path[h*hashSize : (h+1)*hashSize]
					tokenText := fmt.Sprintf("%x", hashBytes)
					matched, _ := resolvePathHopHash(contacts, hashBytes, 0, 0)
					if matched != nil {
						pub := matched.PubKey
						link := newMcHashLink(tokenText,
							func() { g.mcFlyToPubKey(pub) },
							nil,
						)
						pathFlow.Add(link)
					} else {
						unknownTok := canvas.NewText(tokenText, dimCol)
						unknownTok.TextStyle = fyne.TextStyle{Monospace: true}
						unknownTok.TextSize = 10
						pathFlow.Add(unknownTok)
					}
				}
			}
			pathFlow.Refresh()
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
	// Top-right (?) button that opens a dialog explaining how the
	// trace-route animation works, the firmware's path-hash
	// configuration we observed in recent packets, and the
	// fundamental 1-byte collision limitation. Placed in the
	// RxLog header (right corner of the top half of the MeshCore
	// right column) since that's where the data driving the
	// trace originates.
	helpBtn := widget.NewButtonWithIcon("", theme.QuestionIcon(), g.showMcTraceHelpDialog)
	helpBtn.Importance = widget.LowImportance
	header := container.NewBorder(
		nil, nil,
		container.NewPadded(g.mcRxLogHeader),
		helpBtn, nil,
	)
	// Column-label row aligned with the row format below
	// ("%s %-3s %-6s %dh×%dB %5.1f %4d %-12s"). Width-padded to
	// match the meta string positions so the header sits over
	// the right columns even though TextSize is the same.
	colHdr := canvas.NewText("TIME     RT  PAY    HOPS   SNR RSSI SENDER       PATH", color.RGBA{120, 125, 135, 255})
	colHdr.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
	colHdr.TextSize = 9
	g.mcRxLogPane = container.NewStack(
		bg,
		container.NewBorder(
			container.NewVBox(header, container.NewPadded(colHdr)),
			nil, nil, nil,
			g.mcRxLogList,
		),
	)
	return g.mcRxLogPane
}

// showMcTraceHelpDialog opens a read-only dialog explaining how
// the lightning trace-route animation on the map works, what the
// firmware's path-hash configuration looks like in recent
// traffic, and why 1-byte hashes occasionally pick the "wrong"
// repeater on dense meshes. Data-driven where possible: hash
// size + hop distribution are sampled from the live mcRxLog
// ring so the operator sees what their radio is actually
// experiencing right now.
func (g *GUI) showMcTraceHelpDialog() {
	g.mcMu.Lock()
	log := append([]mcRxLogEntry(nil), g.mcRxLog...)
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	g.mcMu.Unlock()

	// Sample observed hash sizes + hop counts from the RxLog
	// ring. Skip the 0xFF "no path" sentinel since it carries
	// no hop info.
	hashSizeHistogram := map[int]int{}
	hopHistogram := map[int]int{}
	var sampledPackets int
	for _, e := range log {
		if e.packet.PathLen == 0xFF {
			continue
		}
		hashSize := int(e.packet.PathLen>>6) + 1
		hopCount := int(e.packet.PathLen & 0x3F)
		hashSizeHistogram[hashSize]++
		hopHistogram[hopCount]++
		sampledPackets++
	}

	// Count current potential collisions: how many of our
	// contacts share a 1-byte leading-byte prefix with another
	// contact? Quick O(N) histogram of first bytes.
	firstByteCount := map[byte]int{}
	for _, c := range contacts {
		firstByteCount[c.PubKey[0]]++
	}
	collidingFirstBytes := 0
	collidingContacts := 0
	for _, n := range firstByteCount {
		if n > 1 {
			collidingFirstBytes++
			collidingContacts += n
		}
	}

	var hashSizeLine string
	if sampledPackets == 0 {
		hashSizeLine = "(no recent packets sampled yet — connect and listen for a minute)"
	} else {
		parts := make([]string, 0, len(hashSizeHistogram))
		for sz := 1; sz <= 4; sz++ {
			if n, ok := hashSizeHistogram[sz]; ok {
				parts = append(parts, fmt.Sprintf("%dB ×%d", sz, n))
			}
		}
		hashSizeLine = strings.Join(parts, ",  ") + fmt.Sprintf("   (from %d packets)", sampledPackets)
	}

	hopParts := make([]string, 0)
	for hops := 0; hops <= 8; hops++ {
		if n, ok := hopHistogram[hops]; ok {
			hopParts = append(hopParts, fmt.Sprintf("%dh×%d", hops, n))
		}
	}
	hopLine := strings.Join(hopParts, ", ")
	if hopLine == "" {
		hopLine = "(no packets with path data sampled yet)"
	}

	body := container.NewVBox(
		wrappedLabel("HOW IT WORKS"),
		wrappedLabel(
			"When the radio receives a mesh packet, the packet's `path` field carries one short hash per repeater hop — the leading bytes of each forwarder's pubkey. NocordHF walks that path and animates each hop on the map, matching each hash against your contacts roster to plot named nodes (and interpolating positions for hops you don't know yet)."),
		wrappedLabel("WIRE FORMAT"),
		wrappedLabel(
			"The packet's PathLen byte encodes two things: top 2 bits = hash size per hop (1, 2, 3, or 4 bytes), bottom 6 bits = hop count (0-63). 0xFF is the \"direct, no path captured\" sentinel."),
		wrappedLabel("OBSERVED IN YOUR TRAFFIC"),
		wrappedLabel("  Hash size distribution:  "+hashSizeLine),
		wrappedLabel("  Hop count distribution:   "+hopLine),
		wrappedLabel("LIMITATION: 1-BYTE COLLISIONS"),
		wrappedLabel(
			"The firmware default is 1 byte per hash — 256 possible values. With dozens of repeaters in earshot, two repeaters whose pubkeys share the same leading byte are indistinguishable from the path field alone. The protocol supports up to 4-byte hashes but firmware mostly uses 1."),
		wrappedLabel(fmt.Sprintf(
			"  Right now you have %d contacts; %d distinct leading bytes collide across %d contacts (~%d%% chance a hop hash is ambiguous).",
			len(contacts), collidingFirstBytes, collidingContacts, percentInt(collidingContacts, len(contacts)))),
		wrappedLabel("HOW NOCORDHF DISAMBIGUATES"),
		wrappedLabel(
			"When a hash matches multiple contacts, NocordHF prefers (1) the Repeater type (path hops are almost always repeaters), then (2) the repeater geographically closest to the previously-resolved hop. Failing both, it falls back to any match so a path still renders rather than collapsing to placeholders. Collisions are logged at debug level in nocordhf.log as `path-hash collision`."),
		wrappedLabel("WHAT WE CAN'T FIX HOST-SIDE"),
		wrappedLabel(
			"The hash size is chosen by the SENDER's firmware when it builds the packet; we can only work with what's on the wire. Until the MeshCore firmware default changes to wider hashes, occasional misattribution on dense meshes is fundamental — the disambiguation rules above are a best-effort heuristic."),
	)
	scroll := container.NewVScroll(body)
	scroll.SetMinSize(fyne.NewSize(560, 460))
	dialog.ShowCustom("About trace routing", "Close", scroll, g.window)
}

// routeShort abbreviates a RouteType string so it fits the new
// tighter RX LOG row layout. FLOOD / DIRECT are kept readable;
// the TRANSPORT_* variants get a two-letter prefix.
func routeShort(s string) string {
	switch s {
	case "FLOOD":
		return "FLD"
	case "DIRECT":
		return "DIR"
	case "TRANSPORT_FLOOD":
		return "TFL"
	case "TRANSPORT_DIRECT":
		return "TDR"
	default:
		if len(s) > 4 {
			return s[:4]
		}
		return s
	}
}

// payloadShort abbreviates a PayloadType string for the tight
// row layout. Preserves uniqueness so the operator can still tell
// types apart at a glance.
func payloadShort(s string) string {
	switch s {
	case "TXT_MSG":
		return "TXT"
	case "GRP_TXT":
		return "GRP"
	case "GRP_DATA":
		return "GRPDAT"
	case "ADVERT":
		return "ADV"
	case "PATH":
		return "PATH"
	case "TRACE":
		return "TRACE"
	case "ACK":
		return "ACK"
	case "REQ":
		return "REQ"
	case "RESPONSE":
		return "RESP"
	case "ANON_REQ":
		return "ANON"
	case "RAW_CUSTOM":
		return "RAW"
	default:
		if len(s) > 7 {
			return s[:7]
		}
		return s
	}
}

// percentInt returns an integer percentage of n/total, guarded
// against divide-by-zero for empty contact rosters.
func percentInt(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

// showRxLogContextMenu pops up a right-click menu on a row in the
// RxLog viewer. Inspect opens the parsed-metadata + hex-dump
// modal; Show path on map plots the route the packet traversed
// using contact-roster lookups for each path-hash hop. Clear path
// removes the most recent path overlay so the operator can de-clutter
// without flipping modes.
func (g *GUI) showRxLogContextMenu(visibleIdx int, absPos fyne.Position) {
	if logging.L != nil {
		logging.L.Debugw("showRxLogContextMenu enter", "visibleIdx", visibleIdx)
	}
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
