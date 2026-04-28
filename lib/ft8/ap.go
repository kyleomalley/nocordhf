package ft8

import (
	"strings"
	"sync"
)

// ap.go — A-priori (AP) decoding hypotheses for FT8, matching reference design a1/a2/…
// markers. Each hypothesis fixes a subset of the 77 payload bits to a known
// value, which is injected into OSD as saturated LLRs. This lets the decoder
// recover signals that are ~2–3 dB too weak for blind decoding by constraining
// the search space.
//
// Implemented (matching reference design ft8b.f90 iaptype 1-6 semantics):
//   a1  — generic CQ: "CQ ? ?"     (pins n29a=4, i3=1)
//   a2b — "? MyCall ?"              (pins n29b to packed own call, i3=1)
//   a3b — "HisCall MyCall ?"        (pins both calls + i3)
//   a4  — "HisCall MyCall RRR"      (pins all 77 bits)
//   a5  — "HisCall MyCall 73"       (pins all 77 bits)
//   a6  — "HisCall MyCall RR73"     (pins all 77 bits)
//   a7  — bare i3 seed              (3 bits only; last-resort weak seed)
//
// a2b runs whenever MyCall is configured via SetAPContext (the operator
// sets this at startup). a3b/a4/a5/a6 additionally require HisCall from
// active QSO state. a1/a7 always run. Position-1 MyCall hypotheses (a2,
// a3) were dropped — they seed our own transmissions, which we don't need
// to AP-decode, and they doubled OSD work per candidate.

// osdMinSNR is the SNR floor below which OSD/AP-rescue decodes are rejected as
// phantom CRC collisions. reference design published AP decode floor is about -24 dB;
// going lower admits nearly random noise.
const osdMinSNR = -24.0

// agreeWeightedThreshold is the |LLR|-weighted agreement floor for rescue
// decodes. Starts at 0 to collect empirical data via the agree_w log field
// before enabling a cutoff — bumping this without data will break real decodes.
const agreeWeightedThreshold = 0.0

// apContext holds the operator's own callsign and (optionally) active QSO
// partner. Threaded into every AP hypothesis build so a2..a6 can seed OSD
// with packed callsign bits. Pre-computes the 28-bit n28 values once per
// SetAPContext so each candidate decode doesn't repeat the pack work.
type apContextState struct {
	myCall, hisCall string
	myN28, hisN28   uint32
	myOK, hisOK     bool
}

var (
	apContextMu sync.RWMutex
	apContext   apContextState
)

// SetAPContext configures the callsign context used by a2..a6 AP
// hypotheses. myCall should be the operator's own callsign (set once at
// startup). hisCall is the active QSO partner's callsign; pass empty when
// no QSO is in progress, and a3..a6 will be skipped. Safe from any
// goroutine.
func SetAPContext(myCall, hisCall string) {
	my := strings.ToUpper(strings.TrimSpace(myCall))
	his := strings.ToUpper(strings.TrimSpace(hisCall))
	myN28, myOK := uint32(0), false
	if my != "" {
		myN28, myOK = packN28(my)
	}
	hisN28, hisOK := uint32(0), false
	if his != "" {
		hisN28, hisOK = packN28(his)
	}
	apContextMu.Lock()
	apContext = apContextState{
		myCall:  my,
		hisCall: his,
		myN28:   myN28,
		hisN28:  hisN28,
		myOK:    myOK,
		hisOK:   hisOK,
	}
	apContextMu.Unlock()
}

func getAPContext() apContextState {
	apContextMu.RLock()
	defer apContextMu.RUnlock()
	return apContext
}

// hasHashToken returns true if text contains a <digits> hash token. These
// appear in Type-4 compound-callsign messages when the decoder can't resolve
// the hash against a prior-QSO table. On real BP decodes they're legitimate,
// but OSD/AP rescues frequently land on random Type-4 unpackings with short
// numeric hashes (e.g. "PQ2AR 6N8TX <64> RR73") because Type-4 has lots of
// codewords with valid CRCs. Use this to reject rescue-path Type-4 decodes.
func hasHashToken(text string) bool {
	for _, tok := range strings.Fields(text) {
		if len(tok) < 3 || tok[0] != '<' || tok[len(tok)-1] != '>' {
			continue
		}
		inner := tok[1 : len(tok)-1]
		if len(inner) == 0 {
			continue
		}
		// Accept digits plus '.' (reference-design style "<...12345>" for unresolved
		// hashes). Must contain at least one digit.
		hasDigit := false
		allDigitOrDot := true
		for _, c := range inner {
			if c >= '0' && c <= '9' {
				hasDigit = true
			} else if c != '.' {
				allDigitOrDot = false
				break
			}
		}
		if allDigitOrDot && hasDigit {
			return true
		}
	}
	return false
}

// isValidCallsignToken returns true if t is a structurally plausible amateur
// call: [prefix with ≥1 letter][area digit][1-4 letter suffix], with optional
// "/SUFFIX" portable modifier. Prefix length 1-3 to cover real DXCC shapes
// (letter-only "K"; letter-digit "V3" Belize; digit-letter "3D" Fiji;
// letter-letter-digit "3DA" Swaziland). Hash-resolved long calls up to 11
// chars are accepted to preserve "HI0DMRA <CALL>" style messages.
//
// Phantom rejection for structurally-valid-but-unallocated prefixes like
// "G52ZRE" is handled downstream by the ITU prefix whitelist and agreement
// filter — we can't distinguish those from real calls like "V31DL" at the
// template level since the reference design's n28 packer accepts both.
func isValidCallsignToken(t string) bool {
	if len(t) < 3 || len(t) > 11 {
		return false
	}
	if len(t) == 4 &&
		t[0] >= 'A' && t[0] <= 'R' &&
		t[1] >= 'A' && t[1] <= 'R' &&
		t[2] >= '0' && t[2] <= '9' &&
		t[3] >= '0' && t[3] <= '9' {
		return false
	}
	// US amateur prefixes (K/N/W/A) never use a 2-digit area number — those
	// blocks aren't allocated. The n28 packer still accepts the LDD[A-Z]+
	// shape (real calls like V31DL, T88AB, J28AB use it for Belize/Palau/
	// Djibouti), so we reject only when the leading letter marks the call
	// as nominally US. Catches phantoms like "W99YUC" / "K77ABC".
	if len(t) >= 3 &&
		(t[0] == 'K' || t[0] == 'N' || t[0] == 'W' || t[0] == 'A') &&
		t[1] >= '0' && t[1] <= '9' &&
		t[2] >= '0' && t[2] <= '9' {
		return false
	}
	base := t
	if slash := strings.IndexByte(t, '/'); slash >= 0 {
		base = t[:slash]
		suffix := t[slash+1:]
		if len(suffix) < 1 || len(suffix) > 4 {
			return false
		}
	}
	if len(base) < 3 {
		return false
	}
	for i := 0; i < len(base); i++ {
		c := base[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	lastDigit := -1
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] >= '0' && base[i] <= '9' {
			lastDigit = i
			break
		}
	}
	if lastDigit < 0 {
		return false
	}
	callSuffix := base[lastDigit+1:]
	if len(callSuffix) < 1 || len(callSuffix) > 4 {
		return false
	}
	for i := 0; i < len(callSuffix); i++ {
		if callSuffix[i] < 'A' || callSuffix[i] > 'Z' {
			return false
		}
	}
	prefix := base[:lastDigit]
	if len(prefix) < 1 || len(prefix) > 3 {
		return false
	}
	hasLetter := false
	for i := 0; i < len(prefix); i++ {
		if prefix[i] >= 'A' && prefix[i] <= 'Z' {
			hasLetter = true
		}
	}
	return hasLetter
}

// isCallOrHashPosition accepts a token in a Type-1 call position: either a
// structurally-valid callsign or a "<...>" hash placeholder. Hashed calls are
// legitimate in both call positions.
func isCallOrHashPosition(t string) bool {
	if len(t) >= 3 && t[0] == '<' && t[len(t)-1] == '>' {
		return len(t) > 2 // non-empty inside
	}
	return isValidCallsignToken(t)
}

// isCQModifier returns true for tokens valid as the optional zone/continent/
// activity modifier in "CQ MOD CALL GRID" messages. Covers:
//   - continent/zone: DX, NA, EU, SA, AF, AS, OC, QRP
//   - numeric zone: "CQ 3 UA9CC MO26", 1-3 digit zone numbers
//   - activity: POTA, SOTA, WWFF, IOTA, TEST (reference design standard-message
//     extension; "CQ POTA KE8WCR EN80" is a legitimate FT8 transmission
//     encoded via the special n28 reservation for named activities).
func isCQModifier(t string) bool {
	switch t {
	case "DX", "NA", "EU", "SA", "AF", "AS", "OC", "QRP",
		"POTA", "SOTA", "WWFF", "IOTA", "TEST":
		return true
	}
	if len(t) >= 1 && len(t) <= 3 {
		for _, c := range t {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// isGrid4 matches a Maidenhead 4-char grid locator: [A-R][A-R][0-9][0-9].
func isGrid4(t string) bool {
	if len(t) != 4 {
		return false
	}
	return t[0] >= 'A' && t[0] <= 'R' &&
		t[1] >= 'A' && t[1] <= 'R' &&
		t[2] >= '0' && t[2] <= '9' &&
		t[3] >= '0' && t[3] <= '9'
}

// isReport matches the trailing report token in Type-1 messages: signal report
// "+NN"/"-NN", response report "R+NN"/"R-NN", or a sign-off RRR/RR73/73.
//
// FT8 Type-1 signal reports are encoded in 14 bits as `32400+35+snr`, giving
// a legal range of −35..−1 and +1..+49 (zero excluded). Tokens that parse
// numerically but yield a value outside that range are unpacked-from-noise
// phantoms (e.g. "R+58" seen on the corpus — value +58 is impossible, the
// underlying bits are noise). Reject those.
func isReport(t string) bool {
	switch t {
	case "RRR", "RR73", "73":
		return true
	}
	digits := t
	if len(digits) >= 1 && digits[0] == 'R' {
		digits = digits[1:]
	}
	if len(digits) < 2 || len(digits) > 3 {
		return false
	}
	if digits[0] != '+' && digits[0] != '-' {
		return false
	}
	val := 0
	for i := 1; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
		val = val*10 + int(digits[i]-'0')
	}
	if digits[0] == '-' {
		val = -val
	}
	return val >= -35 && val <= 49 && val != 0
}

// isValidType1Message validates that text parses as a well-formed Type-1 FT8
// message with all call positions structurally plausible. This is stricter than
// "at least one valid callsign" — it rejects rescue-path phantoms like
// "200BSQ ND1UVX LI57" where one call is bogus even though the other is clean.
// Tail tokens after the callsigns must also be well-formed (a single grid or
// report) so rescue phantoms like "CQ 2S5BTB R LH15" don't pass.
//
// Layouts accepted:
//
//	CQ CALL [GRID]
//	CQ MOD CALL [GRID]      (MOD = zone 0-999 or continent code)
//	CALL1 CALL2 [TAIL]      (TAIL = grid4, +NN/-NN, R+NN/R-NN, RRR, RR73, 73)
func isValidType1Message(text string) bool {
	tokens := strings.Fields(strings.ToUpper(text))
	if len(tokens) == 0 {
		return false
	}
	if tokens[0] == "CQ" {
		if len(tokens) < 2 {
			return false
		}
		callIdx := 1
		if isCQModifier(tokens[1]) {
			if len(tokens) < 3 {
				return false
			}
			callIdx = 2
		}
		if !isCallOrHashPosition(tokens[callIdx]) {
			return false
		}
		// CQ tail: either nothing, or exactly one grid4 token.
		switch len(tokens) - callIdx - 1 {
		case 0:
			return true
		case 1:
			return isGrid4(tokens[callIdx+1])
		default:
			return false
		}
	}
	if len(tokens) < 2 || len(tokens) > 3 {
		return false
	}
	if !isCallOrHashPosition(tokens[0]) || !isCallOrHashPosition(tokens[1]) {
		return false
	}
	if len(tokens) == 3 {
		return isGrid4(tokens[2]) || isReport(tokens[2])
	}
	return true
}

// Message-payload bit layout for a standard (i3=1) Type 1 message:
//   bits  0..28  (29): n29a = (n28_call1 << 1) | ipa
//   bits 29..57  (29): n29b = (n28_call2 << 1) | ipb
//   bit    58    (1):  ir   (response flag)
//   bits 59..73  (15): igrid4
//   bits 74..76  (3):  i3
//
// For AP-1 "CQ ? ?": n28_call1 is the special CQ token (= 2), ipa = 0, i3 = 1.
// That pins 29 bits at positions 0–28 and 3 bits at 74–76 — 32 known bits.

// apSeed returns seedPos/seedVal slices to pass to decodeOSDSeeded. Verify
// is called on the decoded message text after a successful OSD match — it
// must return true or the decode is rejected. OSD can flip any bit
// (including seeded ones) at high enough correlation cost, so a
// pin-everything hypothesis still needs to confirm the final payload
// matches the seed's intent.
type apSeed struct {
	name   string
	pos    []int
	val    []byte
	verify func(text string) bool
}

func (s *apSeed) setBits(start, n int, v uint64) {
	for i := 0; i < n; i++ {
		bit := byte((v >> uint(n-1-i)) & 1)
		s.pos = append(s.pos, start+i)
		s.val = append(s.val, bit)
	}
}

// seedAll77 seeds every bit of a fully-determined 77-bit message. Used by
// a4/a5/a6 which pin a complete "HisCall MyCall RRR/73/RR73" exchange.
func (s *apSeed) seedAll77(bits [77]byte) {
	for i := 0; i < 77; i++ {
		s.pos = append(s.pos, i)
		s.val = append(s.val, bits[i])
	}
}

// tokensEqualIgnoringBrackets reports whether got equals want after
// stripping angle-brackets from got. "<KO6IEH>" == "KO6IEH".
func tokensEqualIgnoringBrackets(got, want string) bool {
	got = strings.TrimSpace(got)
	if len(got) >= 2 && got[0] == '<' && got[len(got)-1] == '>' {
		got = got[1 : len(got)-1]
	}
	return got == want
}

// apHypothesesForCandidate builds the ordered list of AP hypotheses to try
// when plain OSD has already failed. Ordering: most-constrained first so
// real weak-signal decodes trigger on the tightest-fitting seed; a7 runs
// last as the weakest catch-all.
func apHypothesesForCandidate() []apSeed {
	ctx := getAPContext()
	var out []apSeed

	// a1: generic CQ. n29a = (n28_CQ<<1)|0 = 4, i3=1.
	a1 := apSeed{name: "a1"}
	a1.setBits(0, 29, 4)
	a1.setBits(74, 3, 1)
	a1.verify = func(text string) bool { return strings.HasPrefix(text, "CQ ") }
	out = append(out, a1)

	if ctx.myOK {
		myN29 := uint64(ctx.myN28) << 1 // ipa=0

		// a2b: "? MyCall ?" — catches transmissions addressed to us
		// (call2 position). This is the workhorse for QSO pickup — any
		// station replying to our CQ lands here regardless of whether
		// we've set HisCall yet.
		a2b := apSeed{name: "a2b"}
		a2b.setBits(29, 29, myN29)
		a2b.setBits(74, 3, 1)
		myCall := ctx.myCall
		a2b.verify = func(text string) bool {
			tokens := strings.Fields(text)
			if len(tokens) < 2 {
				return false
			}
			// In "CQ CALL ..." messages the second token is the calling
			// station — not "to us". Require call2 == myCall AND not CQ.
			if tokens[0] == "CQ" {
				return false
			}
			return tokensEqualIgnoringBrackets(tokens[1], myCall)
		}
		out = append(out, a2b)
	}

	if ctx.myOK && ctx.hisOK {
		myN29 := uint64(ctx.myN28) << 1
		hisN29 := uint64(ctx.hisN28) << 1
		myCall, hisCall := ctx.myCall, ctx.hisCall

		// a3b: "HisCall MyCall ?" — partner is transmitting back to us.
		a3b := apSeed{name: "a3b"}
		a3b.setBits(0, 29, hisN29)
		a3b.setBits(29, 29, myN29)
		a3b.setBits(74, 3, 1)
		a3b.verify = func(text string) bool {
			tokens := strings.Fields(text)
			return len(tokens) >= 2 &&
				tokensEqualIgnoringBrackets(tokens[0], hisCall) &&
				tokensEqualIgnoringBrackets(tokens[1], myCall)
		}
		out = append(out, a3b)

		// a4/a5/a6: full 77-bit seeds for "HisCall MyCall {RRR,73,RR73}"
		// — partner sign-off / confirm messages. igrid4 values per
		// pack.go: MAXGRID4+{2,3,4} = RRR / RR73 / 73.
		fullSeed := func(name string, igrid4 uint16, wantSuffix string) apSeed {
			s := apSeed{name: name}
			bits, ok := packMessage77(hisCall, myCall, 0, 0, 0, igrid4, 1)
			if !ok {
				return s // empty seed; caller treats as no-op
			}
			s.seedAll77(bits)
			s.verify = func(text string) bool {
				return strings.HasSuffix(text, " "+wantSuffix)
			}
			return s
		}
		if s := fullSeed("a4", uint16(maxgrid4)+2, "RRR"); len(s.pos) > 0 {
			out = append(out, s)
		}
		if s := fullSeed("a5", uint16(maxgrid4)+4, "73"); len(s.pos) > 0 {
			out = append(out, s)
		}
		if s := fullSeed("a6", uint16(maxgrid4)+3, "RR73"); len(s.pos) > 0 {
			out = append(out, s)
		}
	}

	// a7: bare i3 seed (3 bits). Weakest, last-resort catch for non-CQ
	// Type-1 traffic that none of the above fit.
	a7 := apSeed{name: "a7"}
	a7.setBits(74, 3, 1)
	out = append(out, a7)

	return out
}
