package nocord

// gui_meshcore_lifecycle.go — MeshCore session lifecycle and
// long-running background watchers tied to a live client:
// disconnect, auto-reconnect, periodic clock sync, and the
// battery-low monitor.
//
// connectMeshcore + runMeshcoreEvents deliberately stay in gui.go
// because they entangle with multiple feature surfaces (sidebar
// hydration, contacts refresh, history store wiring, the central
// event-dispatch switch). The smaller per-task watchers + the
// teardown path move here so the lifecycle story reads in one
// file.

import (
	"fmt"
	"time"

	"fyne.io/fyne/v2"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// MeshCore battery-warning thresholds — Li-ion 1S pack reference.
// Warn at 3500 mV (~10% remaining); rearm at 3700 mV (~30%) to
// avoid rapid retrigger when voltage hovers around the threshold
// (battery voltage often dips under load and recovers seconds
// later). 0 mV is treated as "mains powered / unsupported" and
// skipped.
const (
	mcBatteryWarnMillivolts  = 3500
	mcBatteryRearmMillivolts = 3700
	mcBatteryPollInterval    = 5 * time.Minute
)

// runMeshcoreBatteryWatch polls the radio's CoreStats every few
// minutes looking for the battery voltage crossing below the
// warn threshold. Fires exactly one system message per dip — the
// "armed" flag rearms only after voltage rises past the rearm
// threshold (hysteresis). Exits when the client argument is no
// longer the live mcClient (i.e. after disconnect / reconnect)
// so a fresh connect spawns a fresh watcher without overlap.
func (g *GUI) runMeshcoreBatteryWatch(client *meshcore.Client) {
	armed := true
	t := time.NewTicker(mcBatteryPollInterval)
	defer t.Stop()
	check := func() bool {
		g.mcMu.Lock()
		current := g.mcClient
		g.mcMu.Unlock()
		if current != client {
			return false // newer client took over
		}
		mv, err := client.GetCoreStats()
		if err != nil {
			return true // transient — try again next tick
		}
		v := mv.BatteryMilliVolts
		if v == 0 {
			return true // mains powered or unsupported
		}
		if armed && v <= mcBatteryWarnMillivolts {
			g.mcAppendSystem(fmt.Sprintf("battery low: %s — consider charging the radio", formatBattery(v)))
			armed = false
		} else if !armed && v >= mcBatteryRearmMillivolts {
			armed = true
		}
		return true
	}
	for range t.C {
		if !check() {
			return
		}
	}
}

// disconnectMeshcore closes the client cleanly. Called from
// Settings → Save (after a device pick change) and window-close.
// Mode-rail flipping leaves the client open so a quick
// FT8 ↔ MeshCore round-trip doesn't tear down the session. The
// persistence store is closed here too so bbolt's file lock is
// released before the next process tries to open it. Sets
// mcManualDisconnect so any pending auto-reconnect timer no-ops
// when it fires — the operator's intent is "stay disconnected".
func (g *GUI) disconnectMeshcore() {
	g.mcMu.Lock()
	c := g.mcClient
	store := g.mcStore
	g.mcClient = nil
	g.mcStore = nil
	g.mcStarted = false
	g.mcManualDisconnect = true
	if g.mcAutoReconnectTimer != nil {
		g.mcAutoReconnectTimer.Stop()
		g.mcAutoReconnectTimer = nil
	}
	g.mcMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	if store != nil {
		_ = store.Close()
	}
}

// scheduleMcAutoReconnect arms a one-shot timer to retry
// connectMeshcore after the operator-configured interval (default
// mcDefaultAutoReconnectMin minutes). 0 disables. macOS sleep/wake
// is the most common failure that drops the BLE link silently;
// without this the operator has to remember to manually re-pick
// the radio in Settings every time the laptop wakes. The interval
// is intentionally NOT zero by default — battery-powered trackers
// like the T1000-E pay a real cost for an aggressive reconnect
// loop, and a 5-minute drift between "I closed the lid" and
// "messages flow again" is acceptable for most use.
func (g *GUI) scheduleMcAutoReconnect() {
	if g.app == nil {
		return
	}
	mins := g.app.Preferences().IntWithFallback(mcPrefAutoReconnectMin, mcDefaultAutoReconnectMin)
	if mins <= 0 {
		return
	}
	g.mcMu.Lock()
	if g.mcManualDisconnect {
		g.mcMu.Unlock()
		return
	}
	if g.mcAutoReconnectTimer != nil {
		g.mcAutoReconnectTimer.Stop()
	}
	delay := time.Duration(mins) * time.Minute
	g.mcAutoReconnectTimer = time.AfterFunc(delay, func() {
		g.mcMu.Lock()
		// Bail if a manual reconnect already happened or the
		// operator manually disconnected since the timer armed.
		if g.mcClient != nil || g.mcManualDisconnect {
			g.mcMu.Unlock()
			return
		}
		g.mcMu.Unlock()
		g.mcAppendSystem(fmt.Sprintf("auto-reconnecting after %d min idle", mins))
		fyne.Do(func() { g.connectMeshcore() })
	})
	g.mcMu.Unlock()
	g.mcAppendSystem(fmt.Sprintf("link dropped — auto-reconnect in %d min (Settings → MeshCore to change)", mins))
}

// runMeshcoreTrackerAdvert fires SendSelfAdvert(Flood) at the
// operator-configured interval while client remains the live
// session. Intended for trackers / portable radios that move
// faster than the firmware's own auto-advert cycle, so peers
// see fresh GPS position promptly. 0 (the default) disables —
// stationary radios shouldn't re-advert on a schedule.
//
// Self-exits when the client changes (operator reconnect /
// disconnect) so multiple connect-disconnect cycles don't
// accumulate stray tickers.
func (g *GUI) runMeshcoreTrackerAdvert(client *meshcore.Client) {
	if g.app == nil {
		return
	}
	mins := g.app.Preferences().IntWithFallback(mcPrefTrackerAdvertMin, 0)
	if mins <= 0 {
		return
	}
	g.mcAppendSystem(fmt.Sprintf("tracker re-advert: every %d min", mins))
	t := time.NewTicker(time.Duration(mins) * time.Minute)
	defer t.Stop()
	for range t.C {
		g.mcMu.Lock()
		current := g.mcClient
		g.mcMu.Unlock()
		if current != client {
			return
		}
		if err := client.SendSelfAdvert(meshcore.SelfAdvertFlood); err != nil {
			g.mcAppendSystem("tracker re-advert: " + err.Error())
			// Don't bail on transient errors; the firmware's BLE
			// link can hiccup briefly under load. Next tick tries
			// again. If the client itself goes away the
			// current-client check above will exit cleanly.
			continue
		}
	}
}

// runMeshcoreClockSync re-issues SetDeviceTime once an hour while
// the supplied client is still the active one. Long-running
// sessions whose device clock drifts ahead of wall-clock would
// otherwise have sends silently dropped by repeaters that enforce
// monotonic per-pubkey timestamps. Self-exits when the client
// changes (operator reconnect / disconnect) so multiple
// connect-disconnect cycles don't accumulate stray syncers.
func (g *GUI) runMeshcoreClockSync(client *meshcore.Client) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		g.mcMu.Lock()
		current := g.mcClient
		g.mcMu.Unlock()
		if current != client {
			return
		}
		_ = client.SetDeviceTime(time.Now())
	}
}
