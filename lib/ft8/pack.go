package ft8

// pack.go handles FT8 message packing and unpacking.
//
// Bit layout of the 77-bit payload (MSB first), matching ft8_lib/reference design:
//   bits  0-28  : n29a — 29 bits (28-bit callsign1 + 1-bit suffix flag ipa)
//   bits 29-57  : n29b — 29 bits (28-bit callsign2 + 1-bit suffix flag ipb)
//   bit  58     : ir   — roger flag (prepends "R " to grid/report)
//   bits 59-72  : igrid4 — 14 bits (grid square or report)
//   bit  73     : reserved (0)
//   bits 74-76  : i3   — message type (1 = standard, 4 = nonstandard, etc.)
//
// n28 callsign encoding (ft8_lib constants):
//   0          = "DE"
//   1          = "QRZ"
//   2          = "CQ"
//   3..1002    = "CQ nnn"
//   1003..NTOKENS-1 = "CQ ABCD"
//   NTOKENS..NTOKENS+MAX22-1 = 22-bit hash (unknown callsign)
//   NTOKENS+MAX22.. = standard 6-char callsign
//
// igrid4 encoding:
//   0..32399  = Maidenhead grid (MAXGRID4=32400, value = row*180 + col)
//   32400+1   = (blank)
//   32400+2   = RRR
//   32400+3   = RR73
//   32400+4   = 73
//   32400+35+snr = signal report (-35..-1 or +1..+49 after subtract 35)

import (
	"fmt"
	"math/big"
	"strings"
)

const (
	ntokens  = uint32(2063592) // special token limit (ft8_lib NTOKENS)
	max22    = uint32(4194304) // 2^22 (ft8_lib MAX22)
	maxgrid4 = uint16(32400)   // ft8_lib MAXGRID4
)

// Message is a decoded FT8 message.
type Message struct {
	Raw  string // original 77-bit payload as binary string for debugging
	Text string // human-readable decoded text
	Type int    // message type (1=standard, 4=nonstandard, etc.)
}

// Unpack77 decodes a 77-bit payload (one bit per byte, MSB first) into a Message.
// Routes by i3 (bits 74-76) and for i3=0 by n3 (bits 71-73). Message.Type is set to 10*i3+n3 for i3=0
// subtypes (so 0.1 = DXpedition is Type=1 with a sub; we store it as 10
// since i3=1 is already "standard") — specifically:
//   - 1, 2    standard / EU-VHF-portable
//   - 4       nonstandard callsign
//   - 100     free text (i3=0 n3=0)
//   - 101     DXpedition mode
//   - 103/104 ARRL Field Day
//   - 105     telemetry
//   - 3       ARRL RTTY Roundup
//   - 5       EU VHF contest
func Unpack77(bits [77]byte) Message {
	raw := bitsToString(bits[:])
	n3 := int(bits[71])<<2 | int(bits[72])<<1 | int(bits[73])
	i3 := int(bits[74])<<2 | int(bits[75])<<1 | int(bits[76])

	switch {
	case i3 == 0 && n3 == 0:
		text := unpackFreeText(bits)
		if text == "" {
			return Message{Raw: raw, Text: "[free-text err]", Type: 100}
		}
		return Message{Raw: raw, Text: text, Type: 100}
	case i3 == 0 && n3 == 1:
		text := unpackDXpedition(bits)
		if text == "" {
			return Message{Raw: raw, Text: "[dxpedition err]", Type: 101}
		}
		registerCallsignsFromText(text)
		return Message{Raw: raw, Text: text, Type: 101}
	case i3 == 0 && (n3 == 3 || n3 == 4):
		text := unpackFieldDay(bits, n3)
		if text == "" {
			return Message{Raw: raw, Text: "[field-day err]", Type: 100 + n3}
		}
		registerCallsignsFromText(text)
		return Message{Raw: raw, Text: text, Type: 100 + n3}
	case i3 == 0 && n3 == 5:
		text := unpackTelemetry(bits)
		if text == "" {
			return Message{Raw: raw, Text: "[telemetry err]", Type: 105}
		}
		return Message{Raw: raw, Text: text, Type: 105}
	case i3 == 0 && (n3 == 2 || n3 == 6 || n3 == 7):
		// n3=2 is unused; n3=6 is WSPR (not FT8); n3=7 is undefined.
		return Message{Raw: raw, Text: fmt.Sprintf("[i3=0 n3=%d]", n3), Type: 100 + n3}
	case i3 == 1 || i3 == 2:
		text := unpackStandard(bits, i3)
		if text != "" {
			registerCallsignsFromText(text)
			return Message{Raw: raw, Text: text, Type: i3}
		}
		return Message{Raw: raw, Text: fmt.Sprintf("[unpack err i3=%d]", i3), Type: i3}
	case i3 == 3:
		text := unpackRTTYRU(bits)
		if text == "" {
			return Message{Raw: raw, Text: "[rtty-ru err]", Type: 3}
		}
		registerCallsignsFromText(text)
		return Message{Raw: raw, Text: text, Type: 3}
	case i3 == 4:
		text := unpackNonstd(bits)
		if text != "" {
			return Message{Raw: raw, Text: text, Type: 4}
		}
		return Message{Raw: raw, Text: "[type4]", Type: 4}
	case i3 == 5:
		text := unpackEUVHF(bits)
		if text == "" {
			return Message{Raw: raw, Text: "[eu-vhf err]", Type: 5}
		}
		return Message{Raw: raw, Text: text, Type: 5}
	default:
		return Message{Raw: raw, Text: fmt.Sprintf("[type%d]", i3), Type: i3}
	}
}

// unpackNonstd decodes a type 4 (nonstandard callsign) message matching ft8_lib decode_nonstd.
// Layout: n12(12) + n58(58) + iflip(1) + nrpt(2) + icq(1) + i3(3) = 77 bits
func unpackNonstd(bits [77]byte) string {
	n12 := uint16(bitsToUint64(bits[0:12]))
	n58 := bitsToUint64(bits[12:70])
	iflip := uint8(bits[70])
	nrpt := uint8(bits[71])<<1 | uint8(bits[72])
	icq := uint8(bits[73])

	// Decode 58-bit plain-text callsign (11 chars, base-38, right-aligned)
	// ALPHANUM_SPACE_SLASH: index 0=' ', 1-10='0'-'9', 11-36='A'-'Z', 37='/'
	var c11 [11]byte
	n := n58
	for i := 10; ; i-- {
		c11[i] = alphanumSpaceSlash[n%38]
		if i == 0 {
			break
		}
		n /= 38
	}
	callDecoded := strings.TrimSpace(string(c11[:]))
	// The plaintext side of a Type-4 message is a full callsign — register it
	// so future Type-4 messages that hash it (as the other party's reply) resolve.
	if callDecoded != "" {
		RegisterCallsign(callDecoded)
	}

	// Resolve the 12-bit hash against callsigns we have heard or registered
	// (e.g. our own callsign, registered at startup via RegisterCallsign).
	var call1, call2 string
	if iflip == 0 {
		call1 = lookupHash12(n12)
		call2 = callDecoded
	} else {
		call1 = callDecoded
		call2 = lookupHash12(n12)
	}

	if icq != 0 {
		return fmt.Sprintf("CQ %s", call2)
	}

	extra := ""
	switch nrpt {
	case 1:
		extra = " RRR"
	case 2:
		extra = " RR73"
	case 3:
		extra = " 73"
	}
	return fmt.Sprintf("%s %s%s", call1, call2, extra)
}

// alphanumSpaceSlash is ft8_lib's FT8_CHAR_TABLE_ALPHANUM_SPACE_SLASH (38 entries):
// index 0=' ', 1-10='0'-'9', 11-36='A'-'Z', 37='/'
var alphanumSpaceSlash = []byte(" 0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ/")

// unpackStandard decodes a standard type 1/2 message matching ft8_lib decode_std.
func unpackStandard(bits [77]byte, i3 int) string {
	n29a := uint32(bitsToUint64(bits[0:29]))
	n29b := uint32(bitsToUint64(bits[29:58]))
	ir := uint8(bits[58])
	igrid4 := uint16(bitsToUint64(bits[59:74])) // 15 bits (59..73)

	ipa := uint8(n29a & 1)
	ipb := uint8(n29b & 1)
	n28a := n29a >> 1
	n28b := n29b >> 1

	call1 := unpack28(n28a, ipa, i3)
	call2 := unpack28(n28b, ipb, i3)
	if call1 == "" || call2 == "" {
		return ""
	}
	extra := unpackGrid(igrid4, ir)
	if extra == "" {
		return fmt.Sprintf("%s %s", call1, call2)
	}
	return fmt.Sprintf("%s %s %s", call1, call2, extra)
}

// unpack28 decodes a 28-bit callsign value, matching ft8_lib unpack28.
func unpack28(n28 uint32, ip uint8, i3 int) string {
	// Special tokens
	if n28 < ntokens {
		switch {
		case n28 == 0:
			return "DE"
		case n28 == 1:
			return "QRZ"
		case n28 == 2:
			return "CQ"
		case n28 <= 1002:
			return fmt.Sprintf("CQ %03d", n28-3)
		case n28 < ntokens:
			n := n28 - 1003
			var aaaa [4]byte
			for i := 3; ; i-- {
				aaaa[i] = lettersSpace[n%27]
				if i == 0 {
					break
				}
				n /= 27
			}
			return "CQ " + strings.TrimLeft(string(aaaa[:]), " ")
		}
	}

	n28 -= ntokens
	if n28 < max22 {
		// 22-bit hash — resolve to a registered callsign when possible.
		return lookupHash22(n28 & 0x3FFFFF)
	}

	// Standard callsign
	n := n28 - max22
	var c [6]byte
	c[5] = lettersSpace[n%27]
	n /= 27
	c[4] = lettersSpace[n%27]
	n /= 27
	c[3] = lettersSpace[n%27]
	n /= 27
	c[2] = numeric[n%10]
	n /= 10
	c[1] = alphanum[n%36]
	n /= 36
	c[0] = alphanumSpace[n%37]

	result := strings.TrimSpace(string(c[:]))
	if len(result) < 3 {
		return ""
	}
	if ip != 0 {
		switch i3 {
		case 1:
			result += "/R"
		case 2:
			result += "/P"
		}
	}
	return result
}

// unpackGrid decodes a 14-bit igrid4 value, matching ft8_lib unpackgrid.
func unpackGrid(igrid4 uint16, ir uint8) string {
	if igrid4 <= maxgrid4 {
		n := igrid4
		var g [4]byte
		g[3] = '0' + byte(n%10)
		n /= 10
		g[2] = '0' + byte(n%10)
		n /= 10
		g[1] = 'A' + byte(n%18)
		n /= 18
		g[0] = 'A' + byte(n%18)
		if ir > 0 {
			return "R " + string(g[:])
		}
		return string(g[:])
	}
	irpt := int(igrid4) - int(maxgrid4)
	prefix := ""
	if ir > 0 {
		prefix = "R"
	}
	switch irpt {
	case 1:
		return ""
	case 2:
		return "RRR"
	case 3:
		return "RR73"
	case 4:
		return "73"
	default:
		snr := irpt - 35
		return fmt.Sprintf("%s%+d", prefix, snr)
	}
}

// Character tables matching ft8_lib charn() tables.
// In ft8_lib, for tables that include a space, index 0 = ' ', then digits, then letters.
var (
	alphanumSpace = []byte(" 0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 37: FT8_CHAR_TABLE_ALPHANUM_SPACE (space=0)
	alphanum      = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")  // 36: FT8_CHAR_TABLE_ALPHANUM
	numeric       = []byte("0123456789")                            // 10: FT8_CHAR_TABLE_NUMERIC
	lettersSpace  = []byte(" ABCDEFGHIJKLMNOPQRSTUVWXYZ")           // 27: FT8_CHAR_TABLE_LETTERS_SPACE (space=0)
)

// encodeNonstdCall58 encodes a callsign of up to 11 characters as a 58-bit
// base-38 value, matching ft8_lib's pack_nonstd encoding.
// The alphabet is alphanumSpaceSlash: " 0-9A-Z/".
func encodeNonstdCall58(call string) (uint64, error) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if len(call) > 11 {
		return 0, fmt.Errorf("nonstandard callsign too long (max 11 chars): %q", call)
	}
	// Right-align in 11 characters by prepending spaces.
	for len(call) < 11 {
		call = " " + call
	}
	var n uint64
	for i := 0; i < 11; i++ {
		idx := strings.IndexByte(string(alphanumSpaceSlash), call[i])
		if idx < 0 {
			return 0, fmt.Errorf("character %q not in nonstandard callsign alphabet", call[i])
		}
		n = n*38 + uint64(idx)
	}
	return n, nil
}

// unpackStandardPartial decodes a standard message, substituting "?????" for
// any field that can't be decoded. Used for CRC-failed candidates.
func unpackStandardPartial(bits [77]byte) string {
	i3 := int(bits[74])<<2 | int(bits[75])<<1 | int(bits[76])
	n29a := uint32(bitsToUint64(bits[0:29]))
	n29b := uint32(bitsToUint64(bits[29:58]))
	ir := uint8(bits[58])
	igrid4 := uint16(bitsToUint64(bits[59:74])) // 15 bits (59..73)

	call1 := unpack28(n29a>>1, uint8(n29a&1), i3)
	if call1 == "" {
		call1 = "?????"
	}
	call2 := unpack28(n29b>>1, uint8(n29b&1), i3)
	if call2 == "" {
		call2 = "?????"
	}
	extra := unpackGrid(igrid4, ir)
	return fmt.Sprintf("%s %s %s", call1, call2, extra)
}

// bitsToUint64 interprets a bit slice (MSB first, each element 0 or 1) as uint64.
func bitsToUint64(bits []byte) uint64 {
	var v uint64
	for _, b := range bits {
		v = (v << 1) | uint64(b&1)
	}
	return v
}

func bitsToString(bits []byte) string {
	sb := make([]byte, len(bits))
	for i, b := range bits {
		if b != 0 {
			sb[i] = '1'
		} else {
			sb[i] = '0'
		}
	}
	return string(sb)
}

// unpackFreeText decodes an i3=0 n3=0 free-text message.
// The first 71 bits encode a 13-character string via base-42 representation,
// using alphabet " 0-9A-Z+-./?" (42 chars). Matches reference design unpacktext77.
func unpackFreeText(bits [77]byte) string {
	const alphabet = " 0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ+-./?"
	n := new(big.Int)
	for i := 0; i < 71; i++ {
		n.Lsh(n, 1)
		if bits[i] != 0 {
			n.Or(n, big.NewInt(1))
		}
	}
	div := big.NewInt(42)
	mod := new(big.Int)
	out := make([]byte, 13)
	for i := 12; i >= 0; i-- {
		n.DivMod(n, div, mod)
		idx := int(mod.Int64())
		if idx < 0 || idx >= len(alphabet) {
			return ""
		}
		out[i] = alphabet[idx]
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}
	return text
}

// unpackDXpedition decodes i3=0 n3=1: "K1ABC RR73; W9XYZ <KH1/KH7Z> -11".
// Layout: n28a(28) + n28b(28) + n10(10) + n5(5) = 71 bits.
func unpackDXpedition(bits [77]byte) string {
	n28a := uint32(bitsToUint64(bits[0:28]))
	n28b := uint32(bitsToUint64(bits[28:56]))
	n10 := uint16(bitsToUint64(bits[56:66]))
	n5 := uint8(bitsToUint64(bits[66:71]))
	if n28a <= 2 || n28b <= 2 {
		return ""
	}
	call1 := unpack28(n28a, 0, 0)
	call2 := unpack28(n28b, 0, 0)
	if call1 == "" || call2 == "" {
		return ""
	}
	call3 := lookupHash10(n10)
	irpt := 2*int(n5) - 30
	sign := "+"
	if irpt < 0 {
		sign = "-"
	}
	abs := irpt
	if abs < 0 {
		abs = -abs
	}
	return fmt.Sprintf("%s RR73; %s %s %s%02d", call1, call2, call3, sign, abs)
}

// unpackFieldDay decodes i3=0 n3=3 or n3=4: "WA9XYZ KA1ABC R 16A EMA".
// Layout: n28a(28) + n28b(28) + ir(1) + intx(4) + nclass(3) + isec(7) = 71 bits.
// n3=4 adds 16 to the transmitter count.
func unpackFieldDay(bits [77]byte, n3 int) string {
	n28a := uint32(bitsToUint64(bits[0:28]))
	n28b := uint32(bitsToUint64(bits[28:56]))
	ir := uint8(bits[56])
	intx := uint8(bitsToUint64(bits[57:61]))
	nclass := uint8(bitsToUint64(bits[61:64]))
	isec := int(bitsToUint64(bits[64:71]))
	if n28a <= 2 || n28b <= 2 {
		return ""
	}
	if isec < 1 || isec > len(arrlSections) {
		return ""
	}
	call1 := unpack28(n28a, 0, 0)
	call2 := unpack28(n28b, 0, 0)
	if call1 == "" || call2 == "" {
		return ""
	}
	ntx := int(intx) + 1
	if n3 == 4 {
		ntx += 16
	}
	classCh := byte('A') + nclass
	sec := strings.TrimSpace(arrlSections[isec-1])
	cntx := fmt.Sprintf("%d%c", ntx, classCh)
	if ir == 1 {
		return fmt.Sprintf("%s %s R %s %s", call1, call2, cntx, sec)
	}
	return fmt.Sprintf("%s %s %s %s", call1, call2, cntx, sec)
}

// unpackTelemetry decodes i3=0 n3=5: up to 18 hex digits of user telemetry.
// Layout: three integers of 23+24+24 = 71 bits, concatenated MSB first.
func unpackTelemetry(bits [77]byte) string {
	a := bitsToUint64(bits[0:23])
	b := bitsToUint64(bits[23:47])
	c := bitsToUint64(bits[47:71])
	s := fmt.Sprintf("%06X%06X%06X", a, b, c)
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// unpackRTTYRU decodes i3=3: ARRL RTTY Roundup.
// Layout: itu(1) + n28a(28) + n28b(28) + ir(1) + irpt(3) + nexch(13) + i3(3) = 77.
// Exchange is either a 4-digit serial (1..7999) or a US/Canadian state/province
// from a 171-entry table (nexch = 8000+imult).
func unpackRTTYRU(bits [77]byte) string {
	itu := uint8(bits[0])
	n28a := uint32(bitsToUint64(bits[1:29]))
	n28b := uint32(bitsToUint64(bits[29:57]))
	ir := uint8(bits[57])
	irpt := uint8(bitsToUint64(bits[58:61]))
	nexch := int(bitsToUint64(bits[61:74]))
	if n28a <= 2 || n28b <= 2 {
		return ""
	}
	call1 := unpack28(n28a, 0, 0)
	call2 := unpack28(n28b, 0, 0)
	if call1 == "" || call2 == "" {
		return ""
	}
	crpt := fmt.Sprintf("5%d9", int(irpt)+2)
	var exch string
	if nexch >= 1 && nexch <= 7999 {
		exch = fmt.Sprintf("%04d", nexch)
	} else if nexch > 8000 {
		imult := nexch - 8000
		if imult < 1 || imult > len(rttyMultipliers) {
			return ""
		}
		exch = strings.TrimSpace(rttyMultipliers[imult-1])
	} else {
		return ""
	}
	prefix := ""
	if itu == 1 {
		prefix = "TU; "
	}
	if ir == 1 {
		return fmt.Sprintf("%s%s %s R %s %s", prefix, call1, call2, crpt, exch)
	}
	return fmt.Sprintf("%s%s %s %s %s", prefix, call1, call2, crpt, exch)
}

// unpackEUVHF decodes i3=5: "<PA3XYZ> <G4ABC/P> R 590003 IO91NP".
// Layout: n12(12) + n22(22) + ir(1) + irpt(3) + iserial(11) + igrid6(25) = 74,
// plus i3(3) = 77. call1 via 12-bit hash, call2 via 22-bit hash.
func unpackEUVHF(bits [77]byte) string {
	n12 := uint16(bitsToUint64(bits[0:12]))
	n22 := uint32(bitsToUint64(bits[12:34]))
	ir := uint8(bits[34])
	irpt := uint8(bitsToUint64(bits[35:38]))
	iserial := uint16(bitsToUint64(bits[38:49]))
	igrid6 := uint32(bitsToUint64(bits[49:74]))
	if igrid6 > 18662399 {
		return ""
	}
	call1 := lookupHash12(n12)
	call2 := lookupHash22(n22)
	nrs := 52 + int(irpt)
	cexch := fmt.Sprintf("%d%04d", nrs, iserial)
	grid6 := decodeGrid6(int(igrid6))
	if grid6 == "" {
		return ""
	}
	if ir == 1 {
		return fmt.Sprintf("%s %s R %s %s", call1, call2, cexch, grid6)
	}
	return fmt.Sprintf("%s %s %s %s", call1, call2, cexch, grid6)
}

// lookupHash10 resolves a 10-bit hash to a bracketed call or "<N>" placeholder.
func lookupHash10(h uint16) string {
	callHashMu.RLock()
	call, ok := hash10Table[h]
	callHashMu.RUnlock()
	if ok {
		return "<" + call + ">"
	}
	return fmt.Sprintf("<%d>", h)
}

// decodeGrid6 converts the 25-bit EU-VHF grid index to a 6-char Maidenhead.
// Matches reference design to_grid6 (base 18*18*10*10*24*24 = 18_662_400).
func decodeGrid6(n int) string {
	if n < 0 || n >= 18662400 {
		return ""
	}
	j1 := n / (18 * 10 * 10 * 24 * 24)
	n -= j1 * (18 * 10 * 10 * 24 * 24)
	j2 := n / (10 * 10 * 24 * 24)
	n -= j2 * (10 * 10 * 24 * 24)
	j3 := n / (10 * 24 * 24)
	n -= j3 * (10 * 24 * 24)
	j4 := n / (24 * 24)
	n -= j4 * (24 * 24)
	j5 := n / 24
	j6 := n - j5*24
	if j1 > 17 || j2 > 17 || j3 > 9 || j4 > 9 || j5 > 23 || j6 > 23 {
		return ""
	}
	return string([]byte{
		byte('A' + j1), byte('A' + j2),
		byte('0' + j3), byte('0' + j4),
		byte('A' + j5), byte('A' + j6),
	})
}

// arrlSections is the 86-entry ARRL Section list (reference design csec, 1-indexed).
var arrlSections = []string{
	"AB", "AK", "AL", "AR", "AZ", "BC", "CO", "CT", "DE", "EB",
	"EMA", "ENY", "EPA", "EWA", "GA", "GH", "IA", "ID", "IL", "IN",
	"KS", "KY", "LA", "LAX", "NS", "MB", "MDC", "ME", "MI", "MN",
	"MO", "MS", "MT", "NC", "ND", "NE", "NFL", "NH", "NL", "NLI",
	"NM", "NNJ", "NNY", "TER", "NTX", "NV", "OH", "OK", "ONE", "ONN",
	"ONS", "OR", "ORG", "PAC", "PR", "QC", "RI", "SB", "SC", "SCV",
	"SD", "SDG", "SF", "SFL", "SJV", "SK", "SNJ", "STX", "SV", "TN",
	"UT", "VA", "VI", "VT", "WCF", "WI", "WMA", "WNY", "WPA", "WTX",
	"WV", "WWA", "WY", "DX", "PE", "NB",
}

// rttyMultipliers is the 171-entry RTTY-RU state/province/country table
// (reference design cmult, 1-indexed).
var rttyMultipliers = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA",
	"HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD",
	"MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
	"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC",
	"SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY",
	"NB", "NS", "QC", "ON", "MB", "SK", "AB", "BC", "NWT", "NF",
	"LB", "NU", "YT", "PEI", "DC", "DR", "FR", "GD", "GR", "OV",
	"ZH", "ZL", "X01", "X02", "X03", "X04", "X05", "X06", "X07", "X08",
	"X09", "X10", "X11", "X12", "X13", "X14", "X15", "X16", "X17", "X18",
	"X19", "X20", "X21", "X22", "X23", "X24", "X25", "X26", "X27", "X28",
	"X29", "X30", "X31", "X32", "X33", "X34", "X35", "X36", "X37", "X38",
	"X39", "X40", "X41", "X42", "X43", "X44", "X45", "X46", "X47", "X48",
	"X49", "X50", "X51", "X52", "X53", "X54", "X55", "X56", "X57", "X58",
	"X59", "X60", "X61", "X62", "X63", "X64", "X65", "X66", "X67", "X68",
	"X69", "X70", "X71", "X72", "X73", "X74", "X75", "X76", "X77", "X78",
	"X79", "X80", "X81", "X82", "X83", "X84", "X85", "X86", "X87", "X88",
	"X89", "X90", "X91", "X92", "X93", "X94", "X95", "X96", "X97", "X98",
	"X99",
}

// registerCallsignsFromText parses a decoded standard message text and
// registers any callsigns found into the hash table. This populates the
// table so that future Type 4 nonstandard messages using 12-bit hashes of
// these callsigns can be resolved back to the full callsign.
func registerCallsignsFromText(text string) {
	for _, tok := range strings.Fields(text) {
		switch tok {
		case "CQ", "DE", "QRZ", "RRR", "RR73", "73", "RR", "DX":
			continue
		}
		if strings.HasPrefix(tok, "<") {
			continue // already a hash placeholder
		}
		// Require at least one letter and one digit (basic callsign shape).
		hasL, hasD := false, false
		for _, c := range tok {
			if c >= 'A' && c <= 'Z' {
				hasL = true
			}
			if c >= '0' && c <= '9' {
				hasD = true
			}
		}
		if hasL && hasD {
			RegisterCallsign(tok)
		}
	}
}
