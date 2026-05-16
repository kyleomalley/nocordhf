package nocord

// gui_meshcore_path.go — path/route resolution + map-trace
// rendering for MeshCore. Walks a packet's path-hash bytes (1
// byte per hop by firmware default; up to 4 bytes per hop is
// protocol-supported via the PathLen top-2-bits encoding),
// matches each hash against the operator's contact roster with
// repeater-preferred + geographic-continuity disambiguation,
// and turns the result into mapview.MessagePathNode slices the
// map widget can animate.
//
// Three entry points:
//   - showMcChatRowMapTrace (right-click chat row → Map Trace) —
//     dispatches between persisted path / RxLog correlation /
//     outbound OutPath depending on the row's direction.
//   - showMcOutboundChatRowMapTrace — outbound-DM specialisation
//     that walks the destination contact's cached OutPath.
//   - mcCapturePathFromRxLog — receive-time helper that stamps
//     the path bytes onto a chat row at append time so the
//     trace works on historical messages reloaded from bbolt.

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kyleomalley/nocordhf/lib/mapview"
	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// showMcChatRowMapTrace animates the route a chat-row message took
// across the mesh. Three sources of path data, tried in order:
//
//  1. Row's persisted mcPathLen/mcPath snapshot — set at incoming-
//     append time from the matching RxLog frame, and reloaded
//     from bbolt across relaunches.
//  2. Live RxLog correlation — for inbound rows that predate the
//     persisted-path schema or whose original RxLog entry hasn't
//     yet rolled out of the 200-entry ring.
//  3. Destination contact's cached OutPath — for OUTBOUND DM rows
//     (which have no inbound packet to correlate against). The
//     firmware stores the route it used to send to each contact
//     in Contact.OutPath; rendering that path is exactly the
//     trace the operator wants for a "what route did my DM
//     take?" question.
//
// Outbound channel messages and TX rows with no destination context
// (no active DM thread) fall through to a friendly system line.
func (g *GUI) showMcChatRowMapTrace(r chatRow) {
	var pkt meshcore.Packet
	switch {
	case r.mcPathLen != 0 || len(r.mcPath) > 0:
		pkt = meshcore.Packet{PathLen: r.mcPathLen, Path: r.mcPath}
	case !r.tx:
		// Inbound row without a persisted path — try the live
		// RxLog ring.
		var ok bool
		pkt, ok = g.findMcRxLogPacketForRow(r)
		if !ok {
			g.mcAppendSystem("no captured path for this message — try a more recent one")
			return
		}
	default:
		// Outbound row — pull the cached path from the
		// destination contact (only meaningful for DMs).
		g.showMcOutboundChatRowMapTrace(r)
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
}

// showMcOutboundChatRowMapTrace renders the route an outbound DM
// took, sourced from the destination contact's firmware-cached
// OutPath. Outbound channel messages have no single destination
// (FLOOD to everyone) so this only fires for contact threads.
//
// Node order is [self, hop1, …, hopN, destination] — the reverse
// of the inbound case, since the packet leaves us and arrives at
// the contact. If OutPathLen <= 0, the firmware has no cached
// path and the next send will FLOOD; tell the operator so they
// know there's no specific route to draw.
func (g *GUI) showMcOutboundChatRowMapTrace(r chatRow) {
	g.mcMu.Lock()
	thread := g.mcCurrentThread
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()
	if !strings.HasPrefix(thread, "contact:") {
		g.mcAppendSystem("trace path only available for DMs — channel messages flood to all listeners")
		return
	}
	prefixHex := thread[len("contact:"):]
	prefix, err := hex.DecodeString(prefixHex)
	if err != nil {
		g.mcAppendSystem("trace path: malformed thread id " + thread)
		return
	}
	var dest *meshcore.Contact
	for i := range contacts {
		if len(prefix) <= len(contacts[i].PubKey) &&
			bytesPrefixEqual(contacts[i].PubKey[:], prefix) {
			dest = &contacts[i]
			break
		}
	}
	if dest == nil {
		g.mcAppendSystem("trace path: destination contact not found in roster")
		return
	}
	display := dest.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", dest.PubKey[:6])
	}
	if dest.OutPathLen <= 0 {
		g.mcAppendSystem(fmt.Sprintf("no cached path to %s — your radio will FLOOD on next send (right-click contact → Reset path to force re-discovery)", display))
		return
	}
	pathBytes := dest.OutPath[:dest.OutPathLen]
	// Build the node sequence walking from us outward. Each byte
	// of OutPath is a 1-byte path-hash hop. resolvePathHopHash
	// disambiguates 1-byte collisions using the prior hop's
	// geography as anchor; self is the initial anchor.
	nodes := make([]mapview.MessagePathNode, 0, int(dest.OutPathLen)+2)
	if selfLat != 0 || selfLon != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: selfName,
			Lat:  selfLat,
			Lon:  selfLon,
		})
	}
	prevLat, prevLon := selfLat, selfLon
	for h := 0; h < len(pathBytes); h++ {
		hashBytes := pathBytes[h : h+1]
		matched, _ := resolvePathHopHash(contacts, hashBytes, prevLat, prevLon)
		if matched != nil && (matched.AdvLatE6 != 0 || matched.AdvLonE6 != 0) {
			nodes = append(nodes, mapview.MessagePathNode{
				Name: matched.AdvName,
				Lat:  matched.LatDegrees(),
				Lon:  matched.LonDegrees(),
			})
			prevLat, prevLon = matched.LatDegrees(), matched.LonDegrees()
		} else {
			nodes = append(nodes, mapview.MessagePathNode{
				Name:        fmt.Sprintf("%x?", hashBytes),
				Placeholder: true,
			})
		}
	}
	// Destination at the end — known position (or placeholder if
	// the contact never broadcast lat/lon).
	if dest.AdvLatE6 != 0 || dest.AdvLonE6 != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: dest.AdvName,
			Lat:  dest.LatDegrees(),
			Lon:  dest.LonDegrees(),
		})
	} else {
		nodes = append(nodes, mapview.MessagePathNode{
			Name:        dest.AdvName,
			Placeholder: true,
		})
	}
	mcInterpolatePathPlaceholders(nodes)
	mw := g.scopeMapWidget()
	if mw == nil || len(nodes) < 2 {
		return
	}
	mw.AppendMessagePath(nodes)
	g.mcAppendSystem(fmt.Sprintf("traced outbound DM path to %s (%d hop%s)", display, dest.OutPathLen, plural(int(dest.OutPathLen))))
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
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	nodes := make([]mapview.MessagePathNode, 0, hashCount+1)
	prevLat, prevLon := selfLat, selfLon
	for h := 0; h < hashCount && h*hashSize+hashSize <= len(pkt.Path); h++ {
		hashBytes := pkt.Path[h*hashSize : h*hashSize+hashSize]
		matched, nMatches := resolvePathHopHash(g.mcContacts, hashBytes, prevLat, prevLon)
		if nMatches > 1 && g.mcLog != nil {
			g.mcLog.Debugw("path-hash collision",
				"hash", fmt.Sprintf("%x", hashBytes),
				"matches", nMatches,
				"picked", func() string {
					if matched != nil {
						return matched.AdvName
					}
					return "(none)"
				}(),
			)
		}
		if matched != nil && (matched.AdvLatE6 != 0 || matched.AdvLonE6 != 0) {
			prevLat, prevLon = matched.LatDegrees(), matched.LonDegrees()
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

// resolvePathHopHash picks the best contact match for one path-hash
// hop in a packet's path field. Handles the 1-byte-collision case
// that's frequent on busy meshes: the firmware's default path-hash
// width is 1 byte (256 distinct values) so two repeaters whose
// pubkeys share the same leading byte are indistinguishable to a
// naive first-match scan. Disambiguation rules, in order:
//
//  1. Prefer an exact-match repeater (path hops are essentially
//     always repeaters; matching a Chat contact whose pubkey
//     happens to share the leading byte is almost always wrong).
//  2. Among multiple repeaters, prefer one whose advertised
//     position is closer to the existing path's geographic
//     trajectory — encoded by neighbour, the previous resolved
//     hop. Caller passes prevLat/prevLon as the anchor; pass 0,0
//     when there's no prior position to anchor on.
//  3. Fall through to the first match of any type so a non-zero
//     path always renders something rather than collapsing to
//     placeholders just because no repeater matches.
//
// Returns (best, allMatchCount). allMatchCount > 1 indicates a
// genuine collision the caller may want to log for diagnostics.
func resolvePathHopHash(contacts []meshcore.Contact, hashBytes []byte, prevLat, prevLon float64) (best *meshcore.Contact, allMatchCount int) {
	type candidate struct {
		idx  int
		dist float64
		hasD bool
	}
	var repeaters, others []candidate
	for i := range contacts {
		if !matchPathHash(contacts[i].PubKey[:], hashBytes) {
			continue
		}
		allMatchCount++
		c := candidate{idx: i}
		if (prevLat != 0 || prevLon != 0) && (contacts[i].AdvLatE6 != 0 || contacts[i].AdvLonE6 != 0) {
			d, ok := distanceMiles(prevLat, prevLon,
				contacts[i].LatDegrees(), contacts[i].LonDegrees(),
				contacts[i].AdvLatE6, contacts[i].AdvLonE6)
			c.dist, c.hasD = d, ok
		}
		if contacts[i].Type == meshcore.AdvTypeRepeater {
			repeaters = append(repeaters, c)
		} else {
			others = append(others, c)
		}
	}
	pick := func(set []candidate) *meshcore.Contact {
		if len(set) == 0 {
			return nil
		}
		// Prefer the candidate closest to the path's prior anchor
		// when distance data is available; fall back to the first.
		bestIdx := set[0].idx
		bestDist := set[0].dist
		bestHas := set[0].hasD
		for _, c := range set[1:] {
			if c.hasD && (!bestHas || c.dist < bestDist) {
				bestIdx, bestDist, bestHas = c.idx, c.dist, c.hasD
			}
		}
		return &contacts[bestIdx]
	}
	if r := pick(repeaters); r != nil {
		return r, allMatchCount
	}
	return pick(others), allMatchCount
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

// bytesPrefixEqual returns true when prefix matches the leading
// bytes of full. Used in contact lookup by 6-byte thread-id
// prefix (the standard MeshCore addressing handle for DMs).
func bytesPrefixEqual(full, prefix []byte) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if full[i] != prefix[i] {
			return false
		}
	}
	return true
}

// plural returns "s" when n != 1 — small grammar helper to avoid
// "1 hops" / "2 hop" awkwardness in the system message.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
