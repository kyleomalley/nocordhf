package nocord

// gui_meshcore_unread.go — per-thread unread + mention bookkeeping
// for the MeshCore sidebar. Drives the cyan-bold "you have new
// messages" badge on contact / channel rows and the warmer amber
// "@you was pinged" highlight when an inbound contains a mention.
//
// Live-thread guard: incrementing for the thread the operator is
// already viewing would be visually noisy (number ticks up while
// the message is already on screen), so the bump methods early-out
// when activeMode == "meshcore" AND mcCurrentThread == thread.

// mcBumpUnread increments the unread counter for a thread when
// it's not the live view. Called from the receive paths so the
// sidebar shows a badge for inbound messages the operator hasn't
// seen yet.
func (g *GUI) mcBumpUnread(thread string) {
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	if live {
		g.mu.Unlock()
		return
	}
	if g.mcUnread == nil {
		g.mcUnread = map[string]int{}
	}
	g.mcUnread[thread]++
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcClearUnread zeros the unread counter and mention flag for a
// thread — called from mcSwitchThread when the operator selects it.
func (g *GUI) mcClearUnread(thread string) {
	g.mu.Lock()
	if g.mcUnread != nil {
		delete(g.mcUnread, thread)
	}
	if g.mcMentioned != nil {
		delete(g.mcMentioned, thread)
	}
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcClearAllUnread wipes every per-thread unread counter and
// mention flag in one shot. Bound to the Contacts header menu's
// "Mark all as read" item — useful after a long absence when
// "10 unread" badges everywhere aren't actionable.
func (g *GUI) mcClearAllUnread() {
	g.mu.Lock()
	g.mcUnread = nil
	g.mcMentioned = nil
	g.mu.Unlock()
	g.mcRefreshLists()
}

// mcUnreadCount reads the current unread count for a thread.
func (g *GUI) mcUnreadCount(thread string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mcUnread == nil {
		return 0
	}
	return g.mcUnread[thread]
}

// mcIsMentioned returns whether the thread has at least one
// unread @[<selfName>] mention since last read. Stronger signal
// than plain unread — drives the warm-amber sidebar highlight
// reserved for directed call-outs.
func (g *GUI) mcIsMentioned(thread string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mcMentioned[thread]
}

// mcMarkMentioned flips the per-thread mention flag on. Caller is
// responsible for skipping the live-thread case (a mention you're
// already looking at isn't unread). Also clears on mcClearUnread.
func (g *GUI) mcMarkMentioned(thread string) {
	g.mu.Lock()
	live := g.activeMode == "meshcore" && g.mcCurrentThread == thread
	if live {
		g.mu.Unlock()
		return
	}
	if g.mcMentioned == nil {
		g.mcMentioned = map[string]bool{}
	}
	g.mcMentioned[thread] = true
	g.mu.Unlock()
	g.mcRefreshLists()
}
