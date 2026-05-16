package nocord

// gui_meshcore_status.go — operator-triggered repeater status
// queries. Right-click a repeater or room-server contact →
// "Query repeater status" issues CmdSendStatusReq; the firmware
// delivers the response asynchronously as PushStatusResponse →
// EventStatusResponse. We render the decoded RepeaterStats as
// a multi-line system message: battery, uptime, queue, noise,
// last RSSI/SNR, packet counters.
//
// Pattern mirrors the telemetry flow: correlation by sender
// pubkey prefix, expiry from the firmware's estTimeoutMilli.

import (
	"fmt"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

type mcPendingStatus struct {
	contact meshcore.Contact
	startAt time.Time
	expires time.Time
}

var (
	mcStatusMu       sync.Mutex
	mcStatusByPrefix = map[meshcore.PubKeyPrefix]*mcPendingStatus{}
	mcStatusMaxAge   = 90 * time.Second
)

// requestMcContactStatus fires a status query at the given
// contact. Surfaces a system message either way (queued / error
// / not connected) so the operator gets immediate feedback. The
// pre-check warns when the target's AdvType isn't Repeater or
// Room — those are the only types the firmware answers, so a
// query against a Chat contact will just time out.
func (g *GUI) requestMcContactStatus(ct meshcore.Contact) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("status: not connected")
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	if ct.Type != meshcore.AdvTypeRepeater && ct.Type != meshcore.AdvTypeRoom {
		g.mcAppendSystem(fmt.Sprintf("status: %s isn't a repeater / room — request likely to time out", display))
	}
	pending := &mcPendingStatus{
		contact: ct,
		startAt: time.Now(),
		expires: time.Now().Add(mcStatusMaxAge),
	}
	prefix := ct.PubKey.Prefix()
	mcStatusMu.Lock()
	mcStatusByPrefix[prefix] = pending
	mcStatusMu.Unlock()
	go func() {
		sent, err := client.SendStatusReq(ct.PubKey)
		if err != nil {
			mcStatusMu.Lock()
			delete(mcStatusByPrefix, prefix)
			mcStatusMu.Unlock()
			g.mcAppendSystem(fmt.Sprintf("status %s: %s", display, err.Error()))
			return
		}
		if sent.EstTimeoutMilli > 0 {
			est := time.Duration(sent.EstTimeoutMilli) * time.Millisecond
			if est > mcStatusMaxAge {
				mcStatusMu.Lock()
				pending.expires = time.Now().Add(est)
				mcStatusMu.Unlock()
			}
		}
		g.mcAppendSystem(fmt.Sprintf("status %s queued (~%ds)", display, sent.EstTimeoutMilli/1000))
	}()
}

// handleMcStatusResponse fires from the events goroutine on a
// PushStatusResponse. Matches by sender prefix; ignores
// unsolicited replies. Renders the decoded RepeaterStats as a
// multi-line system message so the operator can see all of it
// at once.
func (g *GUI) handleMcStatusResponse(ev meshcore.EventStatusResponse) {
	mcStatusMu.Lock()
	p, ok := mcStatusByPrefix[ev.SenderPrefix]
	if ok {
		delete(mcStatusByPrefix, ev.SenderPrefix)
	}
	mcStatusMu.Unlock()
	if !ok {
		gcMcStatus()
		return
	}
	display := p.contact.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", p.contact.PubKey[:6])
	}
	if !ev.StatsOK {
		g.mcAppendSystem(fmt.Sprintf("status %s: unexpected payload (%d bytes raw)",
			display, len(ev.Raw)))
		return
	}
	s := ev.Stats
	elapsed := time.Since(p.startAt).Round(time.Millisecond)
	// SNR field is firmware quarter-dB units.
	snrDB := float64(s.LastSNR) / 4
	g.mcAppendSystem(fmt.Sprintf(
		"status %s (%s):\n  battery=%s  uptime=%s  tx_queue=%d  noise=%d dBm\n  last_rssi=%d dBm  last_snr=%.1f dB  err_events=%d\n  packets recv=%d sent=%d (flood %d/%d, direct %d/%d)\n  total_air=%s  dups direct=%d flood=%d",
		display, elapsed,
		formatBattery(s.BatteryMilliVolts),
		formatUptime(s.TotalUpTimeSecs),
		s.TxQueueLen,
		s.NoiseFloor,
		s.LastRSSI, snrDB, s.ErrEvents,
		s.PacketsRecv, s.PacketsSent,
		s.NRecvFlood, s.NSentFlood,
		s.NRecvDirect, s.NSentDirect,
		formatUptime(s.TotalAirTimeSecs),
		s.NDirectDups, s.NFloodDups,
	))
}

// gcMcStatus drops pending entries whose deadline has passed.
// Same pattern as gcMcTraces / gcMcTelemetry.
func gcMcStatus() {
	now := time.Now()
	mcStatusMu.Lock()
	defer mcStatusMu.Unlock()
	for k, p := range mcStatusByPrefix {
		if now.After(p.expires) {
			delete(mcStatusByPrefix, k)
		}
	}
}
