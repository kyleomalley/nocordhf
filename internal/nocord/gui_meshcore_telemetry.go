package nocord

// gui_meshcore_telemetry.go — operator-triggered sensor telemetry
// queries. Right-click an AdvTypeSensor (or any contact, the
// menu doesn't gate hard) → "Query telemetry" issues
// CmdSendTelemetryReq; the response arrives later as
// EventTelemetryResponse with LPP-decoded readings, which we
// render as a system message in the contact's thread.
//
// Single-line summary on success ("battery=78%, temp=22.4°C,
// humidity=45.5%, GPS=33.02,-117.07") so the operator can scan
// without expanding anything. Unknown LPP types fall through to
// "ch3/0x77=4 bytes 0x..." so the partial parse stays visible.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// mcPendingTelemetry tracks one in-flight telemetry request,
// correlating PushTelemetryResponse by SenderPrefix. We could in
// principle have multiple operator-issued requests in flight at
// once (e.g. polling several sensors), so the map keys on the
// destination pubkey prefix the response will carry.
type mcPendingTelemetry struct {
	contact meshcore.Contact
	startAt time.Time
	expires time.Time
}

var (
	mcTelemetryMu       sync.Mutex
	mcTelemetryByPrefix = map[meshcore.PubKeyPrefix]*mcPendingTelemetry{}
	mcTelemetryMaxAge   = 90 * time.Second
)

// requestMcContactTelemetry fires a telemetry query at the given
// contact. Surfaces a system message either way (queued / error /
// not connected) so the operator gets immediate feedback.
func (g *GUI) requestMcContactTelemetry(ct meshcore.Contact) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("telemetry: not connected")
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	if ct.Type != meshcore.AdvTypeSensor {
		// Non-sensor nodes won't respond. Surface this rather
		// than silently issuing a request that times out.
		g.mcAppendSystem(fmt.Sprintf("telemetry: %s isn't a sensor — request likely to time out", display))
	}
	pending := &mcPendingTelemetry{
		contact: ct,
		startAt: time.Now(),
		expires: time.Now().Add(mcTelemetryMaxAge),
	}
	prefix := ct.PubKey.Prefix()
	mcTelemetryMu.Lock()
	mcTelemetryByPrefix[prefix] = pending
	mcTelemetryMu.Unlock()
	go func() {
		sent, err := client.SendTelemetryReq(ct.PubKey)
		if err != nil {
			mcTelemetryMu.Lock()
			delete(mcTelemetryByPrefix, prefix)
			mcTelemetryMu.Unlock()
			g.mcAppendSystem(fmt.Sprintf("telemetry %s: %s", display, err.Error()))
			return
		}
		if sent.EstTimeoutMilli > 0 {
			est := time.Duration(sent.EstTimeoutMilli) * time.Millisecond
			if est > mcTelemetryMaxAge {
				mcTelemetryMu.Lock()
				pending.expires = time.Now().Add(est)
				mcTelemetryMu.Unlock()
			}
		}
		g.mcAppendSystem(fmt.Sprintf("telemetry %s queued (~%ds)", display, sent.EstTimeoutMilli/1000))
	}()
}

// handleMcTelemetryResponse fires from the events goroutine on a
// PushTelemetryResponse. Matches by sender prefix; ignores
// unsolicited replies (could be a different host app sharing the
// radio, or a stale response post-timeout).
func (g *GUI) handleMcTelemetryResponse(ev meshcore.EventTelemetryResponse) {
	mcTelemetryMu.Lock()
	p, ok := mcTelemetryByPrefix[ev.SenderPrefix]
	if ok {
		delete(mcTelemetryByPrefix, ev.SenderPrefix)
	}
	mcTelemetryMu.Unlock()
	if !ok {
		gcMcTelemetry()
		return
	}
	display := p.contact.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", p.contact.PubKey[:6])
	}
	if len(ev.Readings) == 0 {
		g.mcAppendSystem(fmt.Sprintf("telemetry %s: empty payload (%d bytes)",
			display, len(ev.Raw)))
		return
	}
	parts := make([]string, 0, len(ev.Readings))
	for _, r := range ev.Readings {
		parts = append(parts, formatLPPReading(r))
	}
	elapsed := time.Since(p.startAt).Round(time.Millisecond)
	g.mcAppendSystem(fmt.Sprintf("telemetry %s (%s): %s",
		display, elapsed, strings.Join(parts, ", ")))
}

// formatLPPReading renders one decoded LPP record for the chat
// system-message line. Type-specific formatting picks reasonable
// precision per measurement: battery / humidity to whole percent,
// temperature to one decimal, voltage to two, GPS as lat/lon
// truncated to four decimals.
func formatLPPReading(r meshcore.LPPReading) string {
	name := lppTypeName(r.Type)
	switch v := r.Value.(type) {
	case float64:
		// Pick decimal precision per type so labels read
		// naturally (no "78.0%" for a battery, no "22°C" with
		// no decimal for temp).
		switch r.Type {
		case meshcore.LPPRelativeHumidity, meshcore.LPPPercentage:
			return fmt.Sprintf("%s=%.0f%s", name, v, r.Unit)
		case meshcore.LPPVoltage, meshcore.LPPCurrent:
			return fmt.Sprintf("%s=%.2f %s", name, v, r.Unit)
		default:
			return fmt.Sprintf("%s=%.1f %s", name, v, strings.TrimSpace(r.Unit))
		}
	case [3]float64:
		if r.Type == meshcore.LPPGPS {
			return fmt.Sprintf("%s=%.4f,%.4f,%.0fm", name, v[0], v[1], v[2])
		}
		return fmt.Sprintf("%s=%.2f,%.2f,%.2f %s", name, v[0], v[1], v[2], r.Unit)
	case uint32:
		if r.Unit != "" {
			return fmt.Sprintf("%s=%d %s", name, v, r.Unit)
		}
		return fmt.Sprintf("%s=%d", name, v)
	case bool:
		return fmt.Sprintf("%s=%t", name, v)
	default:
		// Unrecognised — show channel + hex so partial data isn't
		// silently dropped.
		return fmt.Sprintf("ch%d/0x%02x=%x", r.Channel, byte(r.Type), r.Raw)
	}
}

// lppTypeName returns a short human label for known LPP types.
// Falls back to "ch%d/0x%02x" for unrecognised, which the caller
// is expected to override with its own raw-bytes formatting.
func lppTypeName(t meshcore.LPPType) string {
	switch t {
	case meshcore.LPPDigitalInput:
		return "digital_in"
	case meshcore.LPPDigitalOutput:
		return "digital_out"
	case meshcore.LPPAnalogInput:
		return "analog_in"
	case meshcore.LPPAnalogOutput:
		return "analog_out"
	case meshcore.LPPGenericSensor:
		return "sensor"
	case meshcore.LPPLuminosity:
		return "lux"
	case meshcore.LPPPresence:
		return "presence"
	case meshcore.LPPTemperature:
		return "temp"
	case meshcore.LPPRelativeHumidity:
		return "humidity"
	case meshcore.LPPAccelerometer:
		return "accel"
	case meshcore.LPPBarometricPressure:
		return "pressure"
	case meshcore.LPPVoltage:
		return "voltage"
	case meshcore.LPPCurrent:
		return "current"
	case meshcore.LPPFrequency:
		return "freq"
	case meshcore.LPPPercentage:
		return "battery"
	case meshcore.LPPAltitude:
		return "altitude"
	case meshcore.LPPUnixTime:
		return "time"
	case meshcore.LPPGPS:
		return "gps"
	default:
		return fmt.Sprintf("type%d", byte(t))
	}
}

// gcMcTelemetry drops pending entries whose deadline has passed.
// Same pattern as gcMcTraces — cheap walk run opportunistically
// from the response handler when an unsolicited prefix arrives.
func gcMcTelemetry() {
	now := time.Now()
	mcTelemetryMu.Lock()
	defer mcTelemetryMu.Unlock()
	for k, p := range mcTelemetryByPrefix {
		if now.After(p.expires) {
			delete(mcTelemetryByPrefix, k)
		}
	}
}
