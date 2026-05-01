package nocord

// qso_log.go — minimal QSO bookkeeping for NocordHF.
//
// NocordHF intentionally has a constrained TX model (just "CQ" or a
// directed first-call), so we don't try to drive the full FT8
// handshake the way internal/ui does. Instead we passively detect
// when a contact completes from the RX stream:
//
//   - openContact: created when we first hear a station addressing
//     myCall after we sent them a directed call OR after we sent CQ.
//     Captures their grid and SNR as they appear on subsequent
//     transmissions in the same exchange.
//   - completion trigger: any later message of the form
//     "MYCALL THEIR_CALL RR73" or "MYCALL THEIR_CALL 73" (received
//     OR transmitted) flushes the open contact to an ADIF record and
//     calls the configured writer + map-overlay refresh hook.
//
// The result is a working ADIF + worked-grid overlay that lights up
// DXCC squares the same way the legacy nocordhf GUI does, without
// inheriting that GUI's TX-state-machine complexity. Calls that
// disappear before closing (other op walks away) age out after
// qsoStaleAfter so the open-contact map stays bounded.

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/adif"
)

const qsoStaleAfter = 10 * time.Minute

// openContact tracks the running state of an in-progress QSO with one
// remote operator. Fields are filled in opportunistically as the chat
// stream brings them in; on close we hand the populated record to
// adif.Writer.Append.
type openContact struct {
	theirCall  string
	theirGrid  string
	rstSent    int // our measurement of their signal (we'd have TX'd them this report)
	rstRcvd    int // their measurement of us (their last R±NN to us)
	freqMHz    float64
	band       string
	startTime  time.Time
	lastUpdate time.Time
}

// qsoTracker holds the live in-progress contact map plus a small
// callback set so the GUI can wire ADIF persistence, in-memory log
// updates, and worked-grid overlay refresh without this file pulling
// the GUI struct in. All public methods are goroutine-safe.
type qsoTracker struct {
	mu sync.Mutex

	open map[string]*openContact // keyed by uppercase callsign

	// myCall / myGrid / activeBand / activeFreqMHz are kept in sync by
	// the GUI through the corresponding setters. The tracker uses them
	// as defaults when promoting an open contact to a finalised ADIF
	// record.
	myCall        string
	myGrid        string
	activeBand    string
	activeFreqMHz float64

	// onLogged is fired (off the caller's goroutine) once an ADIF
	// record has been persisted. The GUI uses it to refresh the map's
	// worked-grid overlay and append a "QSO logged" system message
	// to the chat.
	onLogged func(adif.Record)
}

func newQSOTracker() *qsoTracker {
	return &qsoTracker{open: map[string]*openContact{}}
}

// SetProfile / SetActiveBand mirror the GUI's profile + channel state
// into the tracker so the next finalised QSO carries correct
// STATION_CALLSIGN, MY_GRIDSQUARE, BAND, FREQ values.
func (t *qsoTracker) SetProfile(myCall, myGrid string) {
	t.mu.Lock()
	t.myCall = strings.ToUpper(strings.TrimSpace(myCall))
	t.myGrid = strings.ToUpper(strings.TrimSpace(myGrid))
	t.mu.Unlock()
}
func (t *qsoTracker) SetActiveBand(name string, hz uint64) {
	t.mu.Lock()
	t.activeBand = name
	t.activeFreqMHz = float64(hz) / 1e6
	t.mu.Unlock()
}
func (t *qsoTracker) SetOnLogged(fn func(adif.Record)) {
	t.mu.Lock()
	t.onLogged = fn
	t.mu.Unlock()
}

// IsOpen reports whether a contact with `call` is currently in progress
// (we sent it a directed call but haven't logged the QSO yet, so it's
// still in t.open). Used by the chat renderer to flag rows where one
// of our open targets is now talking with someone else — a "they're
// busy" cue so the operator notices the target may not respond to us.
func (t *qsoTracker) IsOpen(call string) bool {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.open[call]
	return ok
}

// FireRX is the convenience wrapper used by GUI.AppendDecode: feeds
// the tracker and, if the message closed a QSO, fires onLogged
// synchronously. Returns true if a record was produced.
func (t *qsoTracker) FireRX(msg string, theirSNR int, decodedAt time.Time) bool {
	rec, ok := t.ObserveRX(msg, theirSNR, decodedAt)
	if !ok {
		return false
	}
	t.mu.Lock()
	cb := t.onLogged
	t.mu.Unlock()
	if cb != nil {
		cb(rec)
	}
	return true
}

// FireTX mirrors FireRX for our own transmissions (we may close a QSO
// by sending 73 / RR73 ourselves).
func (t *qsoTracker) FireTX(msg string, txAt time.Time) bool {
	rec, ok := t.ObserveTX(msg, txAt)
	if !ok {
		return false
	}
	t.mu.Lock()
	cb := t.onLogged
	t.mu.Unlock()
	if cb != nil {
		cb(rec)
	}
	return true
}

// ObserveRX is called for every successfully decoded message. If the
// message implies a step in a QSO with us, the tracker updates / opens
// / closes the corresponding entry. Returns the finalised record (and
// true) when the message closed a QSO so the caller can persist it.
func (t *qsoTracker) ObserveRX(msg string, theirSNR int, decodedAt time.Time) (adif.Record, bool) {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(msg)))
	if len(fields) < 2 {
		return adif.Record{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gcLocked(decodedAt)

	myCall := t.myCall
	if myCall == "" || fields[0] != myCall {
		return adif.Record{}, false
	}
	their := fields[1]
	if !looksLikeCallsignFast(their) {
		return adif.Record{}, false
	}

	c, wasOpen := t.open[their], t.open[their] != nil
	if c == nil {
		c = &openContact{
			theirCall: their,
			startTime: decodedAt,
			band:      t.activeBand,
			freqMHz:   t.activeFreqMHz,
		}
		t.open[their] = c
	}
	c.lastUpdate = decodedAt

	// Look at the trailing token. Possibilities:
	//   GRID    (e.g. "DM13") — first reply after our directed call
	//   ±NN     (e.g. "-12")   — their SNR report of our signal
	//   R±NN    (e.g. "R-12")  — roger + SNR
	//   RR73 / 73             — closing
	for i := 2; i < len(fields); i++ {
		tok := fields[i]
		switch {
		case tok == "RR73" || tok == "73":
			// Don't re-fire onLogged on a repeat 73/RR73 from them:
			// the QSO was already finalised on the first one and
			// removed from t.open. Without this, multiple Costas hits
			// of the same closing message produce duplicate uploads.
			if !wasOpen {
				delete(t.open, their)
				return adif.Record{}, false
			}
			rec := t.finaliseLocked(c, decodedAt)
			delete(t.open, their)
			return rec, true
		case isGridLike(tok):
			c.theirGrid = tok
		case strings.HasPrefix(tok, "R") && len(tok) >= 3 && (tok[1] == '+' || tok[1] == '-'):
			if v, err := strconv.Atoi(tok[1:]); err == nil {
				c.rstRcvd = v
			}
		case (tok[0] == '+' || tok[0] == '-') && len(tok) >= 2:
			if v, err := strconv.Atoi(tok); err == nil {
				c.rstRcvd = v
			}
		}
	}
	if c.rstSent == 0 && theirSNR != 0 {
		c.rstSent = theirSNR
	}
	return adif.Record{}, false
}

// ObserveTX is called whenever we transmit. Used to seed the open
// contact (so a CQ-reply scenario already has us listed when their
// first reply arrives) and to detect us closing the QSO with our own
// 73 / RR73.
func (t *qsoTracker) ObserveTX(msg string, txAt time.Time) (adif.Record, bool) {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(msg)))
	if len(fields) < 2 {
		return adif.Record{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gcLocked(txAt)

	// "CQ ..." doesn't open or close anything.
	if fields[0] == "CQ" {
		return adif.Record{}, false
	}
	their := fields[0]
	if !looksLikeCallsignFast(their) {
		return adif.Record{}, false
	}
	c, wasOpen := t.open[their], t.open[their] != nil
	if c == nil {
		c = &openContact{
			theirCall: their,
			startTime: txAt,
			band:      t.activeBand,
			freqMHz:   t.activeFreqMHz,
		}
		t.open[their] = c
	}
	c.lastUpdate = txAt

	// Walk our trailing tokens looking for a closing 73 / RR73.
	for i := 2; i < len(fields); i++ {
		tok := fields[i]
		if tok == "73" || tok == "RR73" {
			// Don't double-fire onLogged when we re-send 73/RR73
			// (manually after auto-reply, or auto-reply re-firing on
			// repeated decodes): the QSO was already finalised on the
			// first one and removed from t.open. If it isn't there
			// now, there's nothing meaningful to log — skip.
			if !wasOpen {
				delete(t.open, their)
				return adif.Record{}, false
			}
			rec := t.finaliseLocked(c, txAt)
			delete(t.open, their)
			return rec, true
		}
		// Capture our own R±NN (which doubles as RST_SENT — what we
		// reported to them) when the tracker is being driven only off
		// chat / TX echos.
		if strings.HasPrefix(tok, "R") && len(tok) >= 3 && (tok[1] == '+' || tok[1] == '-') {
			if v, err := strconv.Atoi(tok[1:]); err == nil {
				c.rstSent = v
			}
		} else if (tok[0] == '+' || tok[0] == '-') && len(tok) >= 2 {
			if v, err := strconv.Atoi(tok); err == nil {
				c.rstSent = v
			}
		}
	}
	return adif.Record{}, false
}

// finaliseLocked converts an openContact into an ADIF record stamped
// with current operator profile values. Caller holds t.mu.
func (t *qsoTracker) finaliseLocked(c *openContact, closedAt time.Time) adif.Record {
	band := c.band
	if band == "" {
		band = t.activeBand
	}
	freq := c.freqMHz
	if freq == 0 {
		freq = t.activeFreqMHz
	}
	start := c.startTime
	if start.IsZero() {
		start = closedAt
	}
	return adif.Record{
		TheirCall:   c.theirCall,
		TheirGrid:   c.theirGrid,
		Mode:        "FT8",
		RSTSent:     c.rstSent,
		RSTRcvd:     c.rstRcvd,
		TimeOn:      start,
		TimeOff:     closedAt,
		Band:        band,
		FreqMHz:     freq,
		StationCall: t.myCall,
		MyGrid:      t.myGrid,
	}
}

// gcLocked drops openContact entries that have been silent for
// qsoStaleAfter so the map stays bounded. Caller holds t.mu.
func (t *qsoTracker) gcLocked(now time.Time) {
	for k, c := range t.open {
		if now.Sub(c.lastUpdate) > qsoStaleAfter {
			delete(t.open, k)
		}
	}
}

// isGridLike matches a 4- or 6-character Maidenhead grid. Used by the
// QSO tracker to opportunistically grab their grid out of decoded
// messages — looks like "DM13" or "DM13ab" on its own as a token.
func isGridLike(s string) bool {
	if len(s) != 4 && len(s) != 6 {
		return false
	}
	if s[0] < 'A' || s[0] > 'R' || s[1] < 'A' || s[1] > 'R' {
		return false
	}
	if s[2] < '0' || s[2] > '9' || s[3] < '0' || s[3] > '9' {
		return false
	}
	if len(s) == 6 && (s[4] < 'A' || s[4] > 'X' || s[5] < 'A' || s[5] > 'X') {
		return false
	}
	return true
}

// looksLikeCallsignFast is a coarse callsign filter used by the QSO
// tracker. Same shape rule as isPlausibleCallsign in gui.go but kept
// local so this file doesn't depend on layout helpers.
func looksLikeCallsignFast(s string) bool {
	if len(s) < 3 || len(s) > 10 {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
			hasLetter = true
		case c >= '0' && c <= '9':
			hasDigit = true
		case c == '/':
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}
