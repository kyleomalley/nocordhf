package nocord

// gui_meshcore_pending.go — pending-advert plumbing. When a radio
// is in manual-add-contacts mode (the default per-type filter
// keeps most types out), advert packets arrive as PushNewAdvert
// events carrying the full Contact-shaped payload. We stash those
// records here so they appear on the map as hollow rings + in the
// PENDING ADVERTS sidebar where the operator can promote, discard,
// or permanently block them.
//
// The auto-promote fast-path lives in mcRecordPendingAdvert: when
// the per-type auto-add prefs include the advert's AdvType, we
// skip the pending-bucket round-trip and call AddUpdateContact
// inline so the contact lands in the radio's table the same way
// it would have under radio-side auto-add.

import (
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// mcRecordPendingAdvert decides what to do with an advert the
// firmware delivered via PushNewAdvert (radio is in manual-add
// mode so the firmware DIDN'T persist it). The per-type auto-add
// prefs are consulted: if the type is checked, we promote
// immediately (call AddUpdateContact via the wire); otherwise we
// upsert the record into the pending bucket so it shows up in the
// PENDING ADVERTS sidebar / map ring for operator review.
//
// Skips entries whose pubkey is on the blocklist OR already in
// the contacts table — promoting an already-admitted contact
// would no-op on the radio but we'd waste a wire round-trip.
func (g *GUI) mcRecordPendingAdvert(c meshcore.Contact) {
	g.mcMu.Lock()
	if g.mcBlockedAdverts[c.PubKey] {
		g.mcMu.Unlock()
		return
	}
	for _, existing := range g.mcContacts {
		if existing.PubKey == c.PubKey {
			g.mcMu.Unlock()
			return
		}
	}
	// Per-type auto-promote: if the operator has the type
	// checked, push to the radio's contacts table now and skip
	// the pending entry entirely.
	autoTypes := g.mcAutoAddTypesLocked()
	if autoTypes[c.Type] {
		client := g.mcClient
		g.mcMu.Unlock()
		if client == nil {
			return
		}
		go func() {
			if err := client.AddUpdateContact(c); err != nil {
				g.mcAppendSystem("auto-add " + c.AdvName + ": " + err.Error())
				return
			}
			name := c.AdvName
			if name == "" {
				name = fmt.Sprintf("%x", c.PubKey[:4])
			}
			g.mcAppendSystem("added contact (auto): " + name)
			g.mcMu.Lock()
			cl := g.mcClient
			g.mcMu.Unlock()
			if cl != nil {
				g.scheduleMcContactsRefresh(cl)
			}
		}()
		return
	}
	if g.mcPendingAdverts == nil {
		g.mcPendingAdverts = map[meshcore.PubKey]meshcore.StoredPendingAdvert{}
	}
	now := time.Now().UTC()
	prev, hadPrev := g.mcPendingAdverts[c.PubKey]
	rec := meshcore.StoredPendingAdvert{
		PubKey:     c.PubKey,
		Type:       c.Type,
		Flags:      c.Flags,
		OutPathLen: c.OutPathLen,
		AdvName:    c.AdvName,
		AdvLatE6:   c.AdvLatE6,
		AdvLonE6:   c.AdvLonE6,
		LastAdvert: c.LastAdvert,
		LastSeen:   now,
	}
	// OutPath is fixed-width 64 bytes; persist only the meaningful
	// prefix indicated by OutPathLen so JSON stays compact.
	if c.OutPathLen > 0 && int(c.OutPathLen) <= len(c.OutPath) {
		rec.OutPath = append(rec.OutPath, c.OutPath[:c.OutPathLen]...)
	}
	if hadPrev && !prev.FirstSeen.IsZero() {
		rec.FirstSeen = prev.FirstSeen
	} else {
		rec.FirstSeen = now
	}
	g.mcPendingAdverts[c.PubKey] = rec
	store := g.mcStore
	g.mcMu.Unlock()
	if store != nil {
		if err := store.SavePendingAdvert(rec); err != nil {
			g.mcAppendSystem("save pending advert: " + err.Error())
		}
	}
	g.mcSyncContactsToMap()
	g.mcRefreshLists()
}

// showMcPendingAdvertContextMenu opens the right-click menu for a
// map ring (pending advert that hasn't been promoted to a real
// contact). Offers Promote (calls AddUpdateContact + clears the
// pending entry on success), Discard (drops the entry locally
// without ever telling the firmware), and Block (drops + adds to
// the persistent blocklist).
func (g *GUI) showMcPendingAdvertContextMenu(p meshcore.StoredPendingAdvert, absPos fyne.Position) {
	name := p.AdvName
	if name == "" {
		name = fmt.Sprintf("%x", p.PubKey[:4])
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Pending advert: "+name, func() {}),
		fyne.NewMenuItem("Add as Contact", func() { g.promoteMcPendingAdvert(p) }),
		fyne.NewMenuItem("Discard", func() { g.discardMcPendingAdvert(p.PubKey) }),
		fyne.NewMenuItem("Block (permanent)", func() { g.blockMcPendingAdvert(p) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, g.window.Canvas(), absPos)
}

// blockMcPendingAdvert silences a pubkey permanently — drops the
// current pending entry AND adds the pubkey to the persistent
// block list so future re-advertisements get filtered before
// reaching the in-memory map. Use to stop spam without having to
// hit Discard every time the node re-advertises (which on busy
// meshes is constant).
func (g *GUI) blockMcPendingAdvert(p meshcore.StoredPendingAdvert) {
	g.mcMu.Lock()
	if g.mcBlockedAdverts == nil {
		g.mcBlockedAdverts = map[meshcore.PubKey]bool{}
	}
	g.mcBlockedAdverts[p.PubKey] = true
	delete(g.mcPendingAdverts, p.PubKey)
	store := g.mcStore
	g.mcMu.Unlock()
	if store != nil {
		if err := store.BlockAdvert(p.PubKey); err != nil {
			g.mcAppendSystem("block save: " + err.Error())
		}
		_ = store.DeletePendingAdvert(p.PubKey)
	}
	name := p.AdvName
	if name == "" {
		name = fmt.Sprintf("%x", p.PubKey[:4])
	}
	g.mcAppendSystem("blocked advert: " + name + " (will not reappear)")
	g.mcSyncContactsToMap()
	g.mcRefreshLists()
}

// promoteMcPendingAdvert sends AddUpdateContact to the radio with
// the pending advert's record. On success, removes the pending
// entry locally + from the bbolt store and triggers a contacts
// refresh so the new contact appears in the sidebar / map. On
// failure, leaves the pending entry in place so the operator can
// try again (the radio's contacts table may be full).
func (g *GUI) promoteMcPendingAdvert(p meshcore.StoredPendingAdvert) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("promote: not connected")
		return
	}
	go func() {
		if err := client.AddUpdateContact(p.AsContact()); err != nil {
			g.mcAppendSystem("promote " + p.AdvName + ": " + err.Error())
			return
		}
		// Optimistic local-state update: inject the equivalent
		// Contact into mcContacts BEFORE deleting from pending
		// AND before scheduleMcContactsRefresh has a chance to
		// catch up. Without this the map node disappears for
		// up to mcContactsRefreshDelay (30s) — the sync renders
		// from mcContacts ∪ mcPendingAdverts, and during that
		// window the pubkey is in neither. mcSyncContactsToMap
		// already de-duplicates pending entries against contacts,
		// so once the next real refresh lands and confirms the
		// contact, nothing changes visually.
		g.mcMu.Lock()
		already := false
		for _, existing := range g.mcContacts {
			if existing.PubKey == p.PubKey {
				already = true
				break
			}
		}
		if !already {
			ct := p.AsContact()
			ct.LastMod = time.Now().UTC()
			g.mcContacts = append(g.mcContacts, ct)
			g.sortMcContactsLocked(g.mcContacts, g.mcContactsSortMode())
		}
		delete(g.mcPendingAdverts, p.PubKey)
		store := g.mcStore
		g.mcMu.Unlock()
		if store != nil {
			_ = store.DeletePendingAdvert(p.PubKey)
		}
		name := p.AdvName
		if name == "" {
			name = fmt.Sprintf("%x", p.PubKey[:4])
		}
		g.mcAppendSystem("added contact: " + name)
		g.scheduleMcContactsRefresh(client)
		g.mcSyncContactsToMap()
		g.mcRefreshLists()
	}()
}

// discardMcPendingAdvert drops a pending advert locally. The
// firmware never knew about it, so there's nothing to tell the
// radio. Useful for spam / known-bad nodes the operator wants to
// stop seeing on the map until they advertise again (at which
// point the entry comes back unless auto-add was meanwhile
// turned on, which would admit them as contacts directly).
func (g *GUI) discardMcPendingAdvert(pub meshcore.PubKey) {
	g.mcMu.Lock()
	delete(g.mcPendingAdverts, pub)
	store := g.mcStore
	g.mcMu.Unlock()
	if store != nil {
		_ = store.DeletePendingAdvert(pub)
	}
	g.mcSyncContactsToMap()
	g.mcRefreshLists()
}
