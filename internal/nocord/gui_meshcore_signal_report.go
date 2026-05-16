package nocord

// gui_meshcore_signal_report.go — "Send Signal Report" action.
// Right-click any incoming chat message and we post a one-line
// report about it to the currently-active channel: the @sender,
// the comma-separated hash-prefix path, hop count, SNR, RSSI,
// and the time the packet was received.
//
// Path extraction prefers the message body when the sender is a
// bot that publishes its route as "XX: name" lines (Volcano-bot
// and similar — the firmware's PathLen for DIRECT-routed replies
// reads as 1 hop even when the actual route was 4+ hops, because
// the Path field on a DIRECT packet only carries the sender's
// stored OutPath stamp, not the per-forwarder accumulation that
// FLOOD packets produce). Falls back to the firmware-captured
// Path bytes for messages without an embedded route table.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// sendMcSignalReportForRow formats a signal-quality report for
// the given incoming chat row and posts it to the currently-
// active channel.
func (g *GUI) sendMcSignalReportForRow(r chatRow) {
	if r.tx || r.system || r.separator {
		g.mcAppendSystem("signal report: pick an inbound message — outbound / system rows have nothing to report")
		return
	}
	g.mcMu.Lock()
	client := g.mcClient
	thread := g.mcCurrentThread
	channels := append([]meshcore.Channel(nil), g.mcChannels...)
	selfName := g.mcSelfInfo.Name
	rxLog := append([]mcRxLogEntry(nil), g.mcRxLog...)
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("signal report: not connected")
		return
	}
	if !strings.HasPrefix(thread, "channel:") {
		g.mcAppendSystem("signal report: switch to a channel first — reports post to the active channel")
		return
	}
	var chIdx uint8
	var chFound bool
	for _, ch := range channels {
		if mcChannelThreadID(ch) == thread {
			chIdx = ch.Index
			chFound = true
			break
		}
	}
	if !chFound {
		g.mcAppendSystem("signal report: active channel no longer in list")
		return
	}
	name := strings.TrimSpace(r.mcSender)
	if name == "" {
		name = "unknown"
	}
	// Prefer RxLog values for SNR + RSSI when an entry is still
	// in the ring (firmware-reported, usually more accurate than
	// the higher-level message event's SNR which is sometimes 0).
	// Fall back to the chat row's persisted snrDB for older rows.
	rxEntry, haveRx := mcRxLogEntryNearTime(rxLog, r.when)
	var snr float64
	var rssi int
	hasSNR := false
	hasRSSI := false
	if haveRx {
		snr = rxEntry.snr
		rssi = rxEntry.rssi
		hasSNR = true
		hasRSSI = true
	}
	if !hasSNR && r.snrDB != 0 {
		snr = r.snrDB
		hasSNR = true
	}
	// Path extraction: prefer the body's "XX: name" route table
	// when present (more accurate for DIRECT replies), otherwise
	// use what the firmware captured for this packet.
	path, pathSource := pickReportPath(r)
	report := formatMcSignalReport(name, r.when, snr, rssi, hasSNR, hasRSSI, path, pathSource)
	now := time.Now().UTC()
	roster := g.mcCurrentRoster()
	go func() {
		res, err := client.SendChannelMessage(chIdx, now, report)
		if err != nil {
			g.mcAppendSystem("signal report send failed: " + err.Error())
			return
		}
		g.mcAppendTrackedTx(thread, chatRow{
			when:     now,
			text:     report,
			tx:       true,
			mc:       true,
			mcSender: selfName,
		}, res.ExpectedAckCRC, meshcore.PubKey{})
		g.mcAnimateOutgoingChannel(roster)
	}()
}

// pickReportPath returns the route to display in the report and a
// short tag identifying where the bytes came from. The firmware-
// captured Path is authoritative for the actual on-air route of
// THIS packet (FLOOD accumulates each forwarder; DIRECT stamps
// the sender's OutPath). The body-extracted "XX: name" list is
// the bot's contact-table broadcast — its known peers, NOT this
// packet's route — so we never use it as the path. When the bot
// happens to also publish a peer table in the body, we tack the
// hash list onto the source tag as "table:dd,ee,ff" so the
// reader knows both pieces without being misled.
func pickReportPath(r chatRow) (path []string, source string) {
	hashSize := 0
	if r.mcPathLen != 0 && r.mcPathLen != 0xFF {
		hashSize = int(r.mcPathLen>>6) + 1
	}
	if hashSize == 0 || len(r.mcPath) < hashSize {
		// No firmware-reported path. If the body has a peer table
		// fall back to printing it so the report isn't blank, but
		// flag it as a table not a route.
		if peers := extractRouteHashesFromBody(r.text); len(peers) > 0 {
			return peers, "bot peers"
		}
		return nil, ""
	}
	hops := len(r.mcPath) / hashSize
	out := make([]string, 0, hops)
	for h := 0; h < hops; h++ {
		out = append(out, fmt.Sprintf("%x", r.mcPath[h*hashSize:(h+1)*hashSize]))
	}
	src := "rx"
	if r.mcHeader != 0 {
		switch (meshcore.Packet{Header: r.mcHeader}).RouteType() {
		case meshcore.RouteFlood, meshcore.RouteTransportFlood:
			src = "FLOOD"
		case meshcore.RouteDirect, meshcore.RouteTransportDirect:
			src = "DIRECT"
		}
	}
	// If the body also carries a peer table, append it as
	// supplementary context so the receiver can compare what THIS
	// copy traversed against the bot's broader known mesh.
	if peers := extractRouteHashesFromBody(r.text); len(peers) > 0 {
		src += " | peers " + strings.Join(peers, ",")
	}
	return out, src
}

// routeLineRE matches a single "XX: name" line in a bot's known-
// route payload. Two-hex prefix, colon, space, then the label.
// We require at least 2 matches before treating the body as a
// route table — random text containing one such line shouldn't
// hijack the path extraction.
var routeLineRE = regexp.MustCompile(`(?m)^([0-9A-Fa-f]{2}):\s+.+$`)

// extractRouteHashesFromBody pulls the lowercase 2-hex prefixes
// out of a "XX: name" route-table broadcast in source order.
// Returns nil when fewer than 2 such lines are present so plain
// chat text (which might coincidentally have a "AB: " prefix)
// doesn't masquerade as a route.
func extractRouteHashesFromBody(body string) []string {
	matches := routeLineRE.FindAllStringSubmatch(body, -1)
	if len(matches) < 2 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}

// formatMcSignalReport renders the one-line report. Truncates to
// the MeshCore 140-byte cap with an ellipsis marker; the metadata
// fields come first so they survive truncation if any field bloats.
func formatMcSignalReport(name string, when time.Time, snr float64, rssi int, hasSNR, hasRSSI bool, pathTokens []string, pathSource string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s | ", name)
	if len(pathTokens) > 0 {
		pathStr := strings.Join(pathTokens, ",")
		if pathSource != "" {
			fmt.Fprintf(&b, "%s (%d hops, %s) | ", pathStr, len(pathTokens), pathSource)
		} else {
			fmt.Fprintf(&b, "%s (%d hops) | ", pathStr, len(pathTokens))
		}
	} else {
		b.WriteString("direct (0 hops) | ")
	}
	if hasSNR {
		fmt.Fprintf(&b, "SNR: %.2f dB | ", snr)
	}
	if hasRSSI {
		fmt.Fprintf(&b, "RSSI: %d dBm | ", rssi)
	}
	fmt.Fprintf(&b, "Received at: %s", when.Local().Format("15:04:05"))
	out := b.String()
	if len(out) > meshcore.MaxTextLength {
		out = out[:meshcore.MaxTextLength-1] + "…"
	}
	return out
}

// mcRxLogEntryNearTime returns the RxLog entry closest in time
// to t (within ±5 s) along with ok=true, or a zero entry +
// ok=false when nothing qualifies. Used to enrich a chat row
// with the firmware-reported SNR / RSSI pair when the RxLog
// frame that produced it is still in the ring.
func mcRxLogEntryNearTime(rxLog []mcRxLogEntry, t time.Time) (mcRxLogEntry, bool) {
	bestDelta := 5 * time.Second
	bestIdx := -1
	for i := range rxLog {
		e := rxLog[i]
		d := e.when.Sub(t)
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			bestDelta = d
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return mcRxLogEntry{}, false
	}
	return rxLog[bestIdx], true
}
