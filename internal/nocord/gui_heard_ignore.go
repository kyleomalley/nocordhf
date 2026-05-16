package nocord

// gui_heard_ignore.go — persistent ignore-list for HEARD entries.
// Operator-curated set of callsigns we silently drop from the
// roster + waterfall decode boxes. Useful for shutting up
// contest beacons, propagation-poke stations, or chronic
// repeaters of the same fragment that the operator's seen a
// thousand times.
//
// Stored in prefs as a comma-separated string for simplicity —
// the operator population is typically <20 ignored calls, so a
// fancy keyed structure is overkill.

import (
	"strings"
)

const prefHeardIgnored = "heard_ignored"

// loadHeardIgnored hydrates the in-memory ignore set from the
// comma-separated pref. Idempotent; called from rememberHeard
// on first miss so a fresh launch picks up the saved list
// without needing an explicit init step in the GUI bootstrap.
func (g *GUI) loadHeardIgnored() {
	if g.app == nil {
		return
	}
	raw := g.app.Preferences().StringWithFallback(prefHeardIgnored, "")
	set := map[string]bool{}
	for _, c := range strings.Split(raw, ",") {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c != "" {
			set[c] = true
		}
	}
	g.mu.Lock()
	g.heardIgnored = set
	g.mu.Unlock()
}

// isHeardIgnored reports whether call is on the operator's
// ignore list. Lazy-loads on first read so callers don't have
// to remember to bootstrap.
func (g *GUI) isHeardIgnored(call string) bool {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return false
	}
	g.mu.Lock()
	if g.heardIgnored == nil {
		g.mu.Unlock()
		g.loadHeardIgnored()
		g.mu.Lock()
	}
	defer g.mu.Unlock()
	return g.heardIgnored[call]
}

// setHeardIgnored adds (on=true) or removes (on=false) call
// from the ignore set, persists the change, and — when adding —
// purges any existing HEARD entry so the operator sees
// immediate effect rather than waiting for the next decode
// cycle to clear the row.
func (g *GUI) setHeardIgnored(call string, on bool) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return
	}
	g.mu.Lock()
	if g.heardIgnored == nil {
		g.heardIgnored = map[string]bool{}
	}
	if on {
		g.heardIgnored[call] = true
		// Purge any live HEARD entry so the row disappears now.
		// Subsequent decodes will be filtered before they get
		// re-added.
		if g.heard != nil {
			delete(g.heard, call)
		}
	} else {
		delete(g.heardIgnored, call)
	}
	// Re-serialise to the pref. Sort isn't necessary but keeps
	// the on-disk form stable for any future diag-bundle reader.
	list := make([]string, 0, len(g.heardIgnored))
	for c := range g.heardIgnored {
		list = append(list, c)
	}
	g.mu.Unlock()
	if g.app != nil {
		g.app.Preferences().SetString(prefHeardIgnored, strings.Join(list, ","))
	}
	if g.usersList != nil {
		// Schedule a refresh so the row drop is visible
		// immediately. Safe from any goroutine.
		g.usersList.Refresh()
	}
}
