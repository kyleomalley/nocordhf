package nocord

// gui_meshcore_trace.go — operator-triggered MeshCore traceroute.
// Right-click contact → "Trace path" sends CmdSendTracePath along
// the contact's cached OutPath. Each repeater the trace passes
// appends its path-hash + observed SNR to the response, which the
// firmware delivers asynchronously as PushTraceData. We correlate
// by the random tag we picked at request time.
//
// Distinct from "Map Trace" on a chat row: that infers the path
// from a captured message's PushLogRxData (passive observation,
// inferred SNR); this one is an active probe that returns each
// hop's actual measured signal level.

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"fyne.io/fyne/v2"

	"github.com/kyleomalley/nocordhf/lib/mapview"
	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// mcPendingTrace holds the in-flight state for a single operator-
// triggered trace. The events goroutine drops the entry once the
// PushTraceData with the matching tag arrives, or after the
// firmware-reported timeout elapses (whichever comes first).
type mcPendingTrace struct {
	tag     uint32
	contact meshcore.Contact
	startAt time.Time
	expires time.Time
}

// mcTraceMu guards the pending-trace map. Lives at package scope
// so traceMcContactPath + the EventTraceData handler share one
// authoritative table without each having to look up g.* fields.
var (
	mcTraceMu     sync.Mutex
	mcTraceByTag  = map[uint32]*mcPendingTrace{}
	mcTraceMaxAge = 90 * time.Second // hard floor in case the firmware estTimeout is missing
)

// traceMcContactPath issues a CmdSendTracePath along the
// contact's cached OutPath. No-op when the contact has no cached
// path (firmware doesn't know how to route there yet — operator
// should send a DM first to force discovery). The trace result
// arrives later as an EventTraceData; the handler lights up the
// hops on the map with per-leg SNR labels.
func (g *GUI) traceMcContactPath(ct meshcore.Contact) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("trace path: not connected")
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	if ct.OutPathLen <= 0 {
		g.mcAppendSystem(fmt.Sprintf("no cached path to %s — send a DM first to force discovery, then try Trace again", display))
		return
	}
	pathBytes := append([]byte(nil), ct.OutPath[:ct.OutPathLen]...)

	// Random tag for correlation. 32-bit random from crypto/rand is
	// fine — collision odds with another in-flight trace are
	// vanishingly small at the rate operators issue these.
	var tagBytes [4]byte
	if _, err := rand.Read(tagBytes[:]); err != nil {
		// Fall back to a wall-clock-derived tag. Worse entropy
		// but never zero; the firmware doesn't care about
		// uniqueness across processes.
		binary.LittleEndian.PutUint32(tagBytes[:], uint32(time.Now().UnixNano()))
	}
	tag := binary.LittleEndian.Uint32(tagBytes[:])

	pending := &mcPendingTrace{
		tag:     tag,
		contact: ct,
		startAt: time.Now(),
		expires: time.Now().Add(mcTraceMaxAge),
	}
	mcTraceMu.Lock()
	mcTraceByTag[tag] = pending
	mcTraceMu.Unlock()

	go func() {
		sent, err := client.SendTracePath(tag, 0, pathBytes)
		if err != nil {
			mcTraceMu.Lock()
			delete(mcTraceByTag, tag)
			mcTraceMu.Unlock()
			g.mcAppendSystem(fmt.Sprintf("trace %s: %s", display, err.Error()))
			return
		}
		// Use the firmware's estTimeout when present — it knows
		// the actual on-air budget for the path's hop count. Cap
		// at our hard floor so a degenerate 0 doesn't make us
		// expire the trace immediately.
		if sent.EstTimeoutMilli > 0 {
			est := time.Duration(sent.EstTimeoutMilli) * time.Millisecond
			if est > mcTraceMaxAge {
				mcTraceMu.Lock()
				pending.expires = time.Now().Add(est)
				mcTraceMu.Unlock()
			}
		}
		g.mcAppendSystem(fmt.Sprintf("trace %s queued (%d hops, ~%ds)",
			display, ct.OutPathLen, sent.EstTimeoutMilli/1000))
	}()
}

// handleMcTraceData fires from the events goroutine when a
// PushTraceData arrives. Matches by tag against the pending
// table, animates the hops on the map with per-hop SNR labels,
// drops the entry on success. Ignores tags we never issued
// (could be a different host app sharing the radio).
func (g *GUI) handleMcTraceData(ev meshcore.EventTraceData) {
	mcTraceMu.Lock()
	p, ok := mcTraceByTag[ev.Tag]
	if ok {
		delete(mcTraceByTag, ev.Tag)
	}
	mcTraceMu.Unlock()
	if !ok {
		// Sweep stale entries while we're here so a long-dropped
		// trace doesn't leak forever.
		gcMcTraces()
		return
	}
	contact := p.contact
	g.mcMu.Lock()
	contacts := append([]meshcore.Contact(nil), g.mcContacts...)
	selfLat := float64(g.mcSelfInfo.AdvLatE6) / 1e6
	selfLon := float64(g.mcSelfInfo.AdvLonE6) / 1e6
	selfName := g.mcSelfInfo.Name
	g.mcMu.Unlock()

	display := contact.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", contact.PubKey[:6])
	}

	// Build a node list mirroring the outbound-trace path layout:
	// [self, hop1 (SNR), …, hopN (SNR), destination]. Each hop's
	// SNR is appended to the resolved name so the map label reads
	// e.g. "K6XYZ (-3.5 dB)".
	nodes := make([]mapview.MessagePathNode, 0, len(ev.PathHashes)+2)
	if selfLat != 0 || selfLon != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: selfName,
			Lat:  selfLat,
			Lon:  selfLon,
		})
	}
	prevLat, prevLon := selfLat, selfLon
	for i, h := range ev.PathHashes {
		hashBytes := []byte{h}
		matched, _ := resolvePathHopHash(contacts, hashBytes, prevLat, prevLon)
		label := fmt.Sprintf("%x?", hashBytes)
		var lat, lon float64
		placeholder := true
		if matched != nil {
			if matched.AdvName != "" {
				label = matched.AdvName
			}
			if matched.AdvLatE6 != 0 || matched.AdvLonE6 != 0 {
				lat = matched.LatDegrees()
				lon = matched.LonDegrees()
				placeholder = false
				prevLat, prevLon = lat, lon
			}
		}
		if i < len(ev.PathSNRs) {
			label = fmt.Sprintf("%s (%.1f dB)", label, ev.PathSNRs[i])
		}
		nodes = append(nodes, mapview.MessagePathNode{
			Name:        label,
			Lat:         lat,
			Lon:         lon,
			Placeholder: placeholder,
		})
	}
	// Destination at the end, with the final-hop SNR appended.
	destLabel := contact.AdvName
	if destLabel == "" {
		destLabel = fmt.Sprintf("%x", contact.PubKey[:6])
	}
	destLabel = fmt.Sprintf("%s (%.1f dB)", destLabel, ev.LastSNR)
	if contact.AdvLatE6 != 0 || contact.AdvLonE6 != 0 {
		nodes = append(nodes, mapview.MessagePathNode{
			Name: destLabel,
			Lat:  contact.LatDegrees(),
			Lon:  contact.LonDegrees(),
		})
	} else {
		nodes = append(nodes, mapview.MessagePathNode{
			Name:        destLabel,
			Placeholder: true,
		})
	}
	mcInterpolatePathPlaceholders(nodes)
	elapsed := time.Since(p.startAt).Round(time.Millisecond)
	g.mcAppendSystem(fmt.Sprintf("trace %s: %d hops in %s (last SNR %.1f dB)",
		display, len(ev.PathHashes), elapsed, ev.LastSNR))
	if mw := g.scopeMapWidget(); mw != nil && len(nodes) >= 2 {
		fyne.Do(func() { mw.AppendMessagePath(nodes) })
	}
}

// gcMcTraces drops pending entries whose deadline has passed.
// Cheap walk; runs only when a non-matching PushTraceData arrives
// or when traceMcContactPath issues a new trace (caller hooks
// via this entry point).
func gcMcTraces() {
	now := time.Now()
	mcTraceMu.Lock()
	defer mcTraceMu.Unlock()
	for tag, p := range mcTraceByTag {
		if now.After(p.expires) {
			delete(mcTraceByTag, tag)
		}
	}
}
