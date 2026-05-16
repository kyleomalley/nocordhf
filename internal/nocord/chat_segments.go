package nocord

// chat_segments.go — parser that turns a chat-row text payload into
// a typed segment slice the renderer can walk: plain runs, @[Name]
// mentions, path-hash links, http(s) URLs, geo coordinate pairs
// (rendered as Google Maps URLs), and contact-card URLs
// (mc://contact/<base64>).
//
// Lives in its own file because (a) it has no GUI state — pure
// functions over text + a contacts slice — and (b) chat parsing
// is exercised by parse_test.go as a unit, so keeping it next to
// its tests is a navigability win.

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

// mcHashSegment is one piece of a parsed chat-row text — a plain
// run, a path-hash link, or an @-mention. Path hashes are 1-, 2-,
// or 3-byte prefixes of a contact pubkey (the firmware uses the
// first PATH_HASH_SIZE bytes — 1 by default — to identify hops in
// Packet.Path). Tokens that don't resolve to a known contact stay
// in plain runs so common English hex-looking words ("be", "ad",
// "decade") aren't underlined. Mentions match the @[Name] wire
// convention used by upstream MeshCore clients (web, iOS) — the
// brackets are wire-format only and stripped when rendering.
type mcHashSegment struct {
	text        string          // visible characters (mentions: "@Name" without brackets)
	link        bool            // true when token resolves to a contact (path-hash link)
	mention     bool            // true when this is an @[Name] mention
	mentionSelf bool            // true when the mention targets the operator's own advert name
	url         string          // populated for http(s) URL spans — render as clickable link
	pub         meshcore.PubKey // populated when link == true OR mention resolved to a contact
	// card is non-nil when this segment represents a decoded
	// mc://contact/<base64> contact-share. Renderer shows it as
	// a clickable pill (name + type icon + "Add contact") instead
	// of the URL string.
	card *meshcore.ContactCard
}

// mcURLRe matches http:// and https:// URLs. The character class
// stops at whitespace; trailing punctuation (.,;:!?)]) commonly
// hugs URLs in prose ("check https://example.com.") so we strip
// it after matching to keep the link targeting the actual page,
// not page-plus-period.
var mcURLRe = regexp.MustCompile(`https?://\S+`)

// mcContactCardRe matches the in-channel contact-share URL form
// — mc://contact/<base64-url-safe-no-padding>. Detected as an
// outermost frame in the chat-segment parser so it renders as a
// chat pill (name + add-contact button) rather than as a raw URL.
// Allows base64url alphabet plus the closing whitespace boundary.
var mcContactCardRe = regexp.MustCompile(`mc://contact/[A-Za-z0-9_-]+`)

// mcURLTrimTrailing is the set of punctuation characters peeled
// off the right end of a URL match before the link is rendered.
const mcURLTrimTrailing = ".,;:!?)]\"'>"

// mcMentionRe matches the "@[Name]" wire convention. Names can
// contain anything except a literal closing bracket so multi-word
// or punctuated handles (rare on MeshCore but possible) survive.
var mcMentionRe = regexp.MustCompile(`@\[([^\]]+)\]`)

// mcGeoRe matches "lat, lon" decimal-degree pairs that frequently
// show up in #wardriving and similar location-share channels —
// e.g. "34.14289, -118.03159" or "-33.86, 151.21". Both numbers
// MUST have a decimal point so we don't false-positive on ordinary
// "1, 2" enumerations or message-id pairs. Range validity (lat in
// [-90,90], lon in [-180,180]) is enforced by mcParseGeoLink after
// the regex match — the regex itself is permissive on digit count
// to avoid having to encode the bounds in regex form.
var mcGeoRe = regexp.MustCompile(`(?:^|[\s(\[])(-?\d{1,3}\.\d+)\s*,\s*(-?\d{1,3}\.\d+)(?:[\s).,;:!?\]]|$)`)

// mcParseGeoLink validates a regex match against the lat/lon
// bounds. Returns the visible text (without the leading/trailing
// boundary character the regex captured) and a Google Maps URL
// when the values are within range. Returns "", "" otherwise so
// the caller leaves the run as plain text.
func mcParseGeoLink(match []int, text string) (visible, href string, start, end int) {
	// Group 1 = lat, group 2 = lon. The full match (group 0)
	// includes the boundary chars on either side; we only want
	// the lat,lon span to be the visible link text.
	latStr := text[match[2]:match[3]]
	lonStr := text[match[4]:match[5]]
	lat, errLat := strconv.ParseFloat(latStr, 64)
	lon, errLon := strconv.ParseFloat(lonStr, 64)
	if errLat != nil || errLon != nil {
		return "", "", 0, 0
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return "", "", 0, 0
	}
	visible = text[match[2]:match[5]]
	href = fmt.Sprintf("https://www.google.com/maps?q=%s,%s", latStr, lonStr)
	return visible, href, match[2], match[5]
}

// mcHashSeriesRe matches a comma-separated SERIES of 2/4/6 hex
// digits — at least two tokens. The series form is the only one we
// auto-link, because a single 2-hex token ("be", "78", "ad") is
// indistinguishable from ordinary English / numeric text and would
// false-positive constantly. A series like "df,b7,43" or
// "df, b7, 43" is unambiguous: nothing in normal chat looks like
// that. Whitespace between commas is allowed so manually-typed
// lists still match.
var mcHashSeriesRe = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?:[0-9a-f]{2}){0,2}(?:\s*,\s*[0-9a-f]{2}(?:[0-9a-f]{2}){0,2})+\b`)

// mcHashTokenInSeriesRe pulls one hex token (and the byte offsets
// of just the hex characters) out of a series substring. Used after
// mcHashSeriesRe locates a series so we can emit each token as its
// own link/plain segment with the comma separators preserved as
// plain runs between them.
var mcHashTokenInSeriesRe = regexp.MustCompile(`(?i)[0-9a-f]{2}(?:[0-9a-f]{2}){0,2}`)

// mcParseChatSegments is the unified inline-segment parser used by
// the chat row binder. It splits text into plain runs, @[Name]
// mentions (rendered without brackets, styled), and path-hash
// links (existing behaviour). Mentions are extracted first as the
// outermost frames; hash-link detection then runs on the plain
// runs between mentions, so a mention can't accidentally hide
// inside a hex series (or vice versa). selfName is the operator's
// own advert name — when a mention's target matches selfName the
// segment is flagged mentionSelf so the renderer can highlight it
// (Slack "@you" style). Returns nil when nothing notable was
// found, letting the caller fall back to the plain canvas.Text
// path.
func mcParseChatSegments(text string, contacts []meshcore.Contact, selfName string) []mcHashSegment {
	if text == "" {
		return nil
	}
	cardLocs := mcContactCardRe.FindAllStringIndex(text, -1)
	urlLocs := mcURLRe.FindAllStringIndex(text, -1)
	mentionLocs := mcMentionRe.FindAllStringSubmatchIndex(text, -1)
	geoLocs := mcGeoRe.FindAllStringSubmatchIndex(text, -1)
	if len(cardLocs) == 0 && len(urlLocs) == 0 && len(mentionLocs) == 0 && len(geoLocs) == 0 {
		// No URLs / mentions / geo coords / cards — fall through
		// to hash-only parsing.
		return mcParseHashLinks(text, contacts)
	}
	// Splice contact-card spans into urlLocs as outermost frames.
	// Cards have priority over URL detection because mc:// would
	// otherwise be skipped by mcURLRe (which only matches https?://).
	// Decoded card data lives in the per-segment `card` field;
	// the visible text stays the original mc://contact/<...> URL
	// so non-NocordHF clients see something they recognise as
	// shareable.
	cardSegments := make(map[int]meshcore.ContactCard, len(cardLocs))
	if len(cardLocs) > 0 {
		merged := make([][]int, 0, len(urlLocs)+len(cardLocs))
		merged = append(merged, urlLocs...)
		for _, c := range cardLocs {
			urlText := text[c[0]:c[1]]
			if card, err := meshcore.DecodeContactCard(urlText); err == nil {
				cardSegments[c[0]] = card
				merged = append(merged, c)
			}
		}
		// Sort by start offset so the outer URL loop stays
		// monotonic. Two URL spans never overlap because the
		// regexes are greedy on non-whitespace.
		sort.Slice(merged, func(i, j int) bool { return merged[i][0] < merged[j][0] })
		urlLocs = merged
	}
	var out []mcHashSegment
	cursor := 0
	// emitMentionsHashesPlain handles a slice that we've already
	// stripped URLs and geo coordinates from. Splits remaining
	// content by mentions, then by hash links, then plain.
	emitMentionsHashesPlain := func(s string) {
		if s == "" {
			return
		}
		if len(mentionLocs) == 0 {
			if hashSegs := mcParseHashLinks(s, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: s})
			}
			return
		}
		subMentions := mcMentionRe.FindAllStringSubmatchIndex(s, -1)
		if len(subMentions) == 0 {
			if hashSegs := mcParseHashLinks(s, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: s})
			}
			return
		}
		subCursor := 0
		emitPlainHashes := func(p string) {
			if p == "" {
				return
			}
			if hashSegs := mcParseHashLinks(p, contacts); hashSegs != nil {
				out = append(out, hashSegs...)
			} else {
				out = append(out, mcHashSegment{text: p})
			}
		}
		for _, m := range subMentions {
			if m[0] > subCursor {
				emitPlainHashes(s[subCursor:m[0]])
			}
			name := s[m[2]:m[3]]
			seg := mcHashSegment{text: "@" + name, mention: true}
			if selfName != "" && strings.EqualFold(name, selfName) {
				seg.mentionSelf = true
			}
			for i := range contacts {
				if strings.EqualFold(contacts[i].AdvName, name) {
					seg.pub = contacts[i].PubKey
					break
				}
			}
			out = append(out, seg)
			subCursor = m[1]
		}
		if subCursor < len(s) {
			emitPlainHashes(s[subCursor:])
		}
	}
	// emitMentionsAndPlain extracts geo coordinate pairs (rendered
	// as Google Maps URL segments) before falling through to the
	// mention / hash extraction path. Geos are outermost-after-URL
	// so a "lat, lon" pair beats both mentions and hash series for
	// the same span (geos can't legally contain mentions or hex
	// series anyway).
	emitMentionsAndPlain := func(s string) {
		if s == "" {
			return
		}
		geoMatches := mcGeoRe.FindAllStringSubmatchIndex(s, -1)
		if len(geoMatches) == 0 {
			emitMentionsHashesPlain(s)
			return
		}
		geoCursor := 0
		for _, gm := range geoMatches {
			visible, href, start, end := mcParseGeoLink(gm, s)
			if href == "" {
				// Out-of-range pair — treat as plain text so
				// "1024.5, 99.9" doesn't become a broken link.
				continue
			}
			if start > geoCursor {
				emitMentionsHashesPlain(s[geoCursor:start])
			}
			out = append(out, mcHashSegment{text: visible, url: href})
			geoCursor = end
		}
		if geoCursor < len(s) {
			emitMentionsHashesPlain(s[geoCursor:])
		}
	}
	// Walk URL matches (URLs + contact-card URLs merged) as
	// outermost frames. Plain text between them goes through
	// mention + hash extraction.
	for _, u := range urlLocs {
		if u[0] > cursor {
			emitMentionsAndPlain(text[cursor:u[0]])
		}
		raw := text[u[0]:u[1]]
		// Contact-card span — emit as a card segment, not a URL,
		// so the renderer draws a pill instead of underlined text.
		if card, ok := cardSegments[u[0]]; ok {
			c := card
			out = append(out, mcHashSegment{text: raw, url: raw, card: &c})
			cursor = u[1]
			continue
		}
		// Peel trailing punctuation that's almost certainly part
		// of the surrounding prose, not the URL.
		trim := strings.TrimRight(raw, mcURLTrimTrailing)
		out = append(out, mcHashSegment{text: trim, url: trim})
		// If we trimmed any tail, emit it as plain so the visible
		// text matches the input character-for-character.
		if tail := raw[len(trim):]; tail != "" {
			out = append(out, mcHashSegment{text: tail})
		}
		cursor = u[1]
	}
	if cursor < len(text) {
		emitMentionsAndPlain(text[cursor:])
	}
	// If nothing in the result is a link / mention / URL, the
	// parser effectively did no work — let the plain-text path
	// handle it.
	for _, s := range out {
		if s.link || s.mention || s.url != "" {
			return out
		}
	}
	return nil
}

// mcParseHashLinks splits text into plain + link segments. Only
// path-hash *series* (≥2 hex tokens joined by commas) get scanned;
// individual tokens that resolve against the contacts slice (via
// pubkey-prefix match of 1/2/3 bytes) become links, the rest stay
// in plain runs. The roster is passed in (not read off
// g.mcContacts) so callers can avoid re-locking on every row
// binding.
func mcParseHashLinks(text string, contacts []meshcore.Contact) []mcHashSegment {
	if text == "" || len(contacts) == 0 {
		return nil
	}
	seriesLocs := mcHashSeriesRe.FindAllStringIndex(text, -1)
	if len(seriesLocs) == 0 {
		return nil
	}
	resolve := func(token string) (meshcore.PubKey, bool) {
		decoded, err := hex.DecodeString(token)
		if err != nil {
			return meshcore.PubKey{}, false
		}
		for i := range contacts {
			pk := contacts[i].PubKey
			if len(decoded) > len(pk) {
				continue
			}
			equal := true
			for j, b := range decoded {
				if pk[j] != b {
					equal = false
					break
				}
			}
			if equal {
				return pk, true
			}
		}
		return meshcore.PubKey{}, false
	}

	var out []mcHashSegment
	cursor := 0
	anyLink := false
	for _, sLoc := range seriesLocs {
		series := text[sLoc[0]:sLoc[1]]
		tokenLocs := mcHashTokenInSeriesRe.FindAllStringIndex(series, -1)
		if len(tokenLocs) < 2 {
			continue
		}
		// Emit any plain text between the previous cursor and this
		// series.
		if sLoc[0] > cursor {
			out = append(out, mcHashSegment{text: text[cursor:sLoc[0]]})
		}
		// Walk the series: token, separator, token, separator, …
		seriesCursor := 0
		for _, tLoc := range tokenLocs {
			if tLoc[0] > seriesCursor {
				out = append(out, mcHashSegment{text: series[seriesCursor:tLoc[0]]})
			}
			token := series[tLoc[0]:tLoc[1]]
			if pub, ok := resolve(token); ok {
				out = append(out, mcHashSegment{text: token, link: true, pub: pub})
				anyLink = true
			} else {
				out = append(out, mcHashSegment{text: token})
			}
			seriesCursor = tLoc[1]
		}
		if seriesCursor < len(series) {
			out = append(out, mcHashSegment{text: series[seriesCursor:]})
		}
		cursor = sLoc[1]
	}
	if !anyLink {
		// All tokens looked like a series but none resolved to a
		// contact — render the original text untouched so we don't
		// gratuitously segment a non-routing comma list.
		return nil
	}
	if cursor < len(text) {
		out = append(out, mcHashSegment{text: text[cursor:]})
	}
	return out
}
