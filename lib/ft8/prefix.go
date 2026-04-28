package ft8

// prefix.go — ITU amateur prefix whitelist for rescue-path defense in depth.
//
// Most rescue-path phantoms (CRC-valid OSD/AP codewords on noise) that escape
// our structural and agreement filters still land on a callsign with a valid
// structural shape. A fraction of those happen to use a 2-character prefix that
// is not allocated to any DXCC entity for amateur use — those we can catch
// here. The filter is opt-in at runtime via SetITUFilterEnabled(); when on, a
// rescue decode must have at least one callsign whose first two characters
// match an entry in the allocated set.
//
// This set is a curated snapshot of the most active DXCC prefix blocks. It
// is deliberately broader than "currently active on-air" — unused allocations
// (e.g. P5 North Korea) remain in the set so legitimate activity isn't
// rejected if it does occur. The tradeoff is that truly unassigned combos
// like "QA"-"QZ" (reserved for Q-codes) or "1B"-"1Z" (mostly unallocated) are
// absent and will trip the filter.

import (
	"strings"
	"sync/atomic"
)

var ituFilterEnabled atomic.Bool

func init() {
	ituFilterEnabled.Store(true)
}

// SetITUFilterEnabled toggles the ITU prefix filter at runtime. Safe to call
// from any goroutine. Defaults to enabled.
func SetITUFilterEnabled(on bool) { ituFilterEnabled.Store(on) }

// ITUFilterEnabled reports the current ITU filter state.
func ITUFilterEnabled() bool { return ituFilterEnabled.Load() }

// callPrefix2 returns the first two characters of call (base callsign, before
// any '/' portable suffix), uppercased. Returns empty if fewer than 2 chars or
// unusable.
func callPrefix2(call string) string {
	call = strings.ToUpper(call)
	if slash := strings.IndexByte(call, '/'); slash >= 0 {
		// Compound call: both sides can carry the "home" prefix. Prefer the
		// longer side (operator base); the shorter is usually "/P", "/M", "/R".
		left, right := call[:slash], call[slash+1:]
		if len(right) > len(left) {
			call = right
		} else {
			call = left
		}
	}
	call = strings.Trim(call, "<>")
	if len(call) < 2 {
		return ""
	}
	return call[:2]
}

// hasAllocatedPrefix returns true if the call's 2-char prefix is in the
// allocated set.
func hasAllocatedPrefix(call string) bool {
	p := callPrefix2(call)
	if p == "" {
		return false
	}
	return allocatedPrefixes[p]
}

// messageHasAllocatedCall returns true if every call position in the Type-1
// message has an allocated ITU prefix. Used as the rescue filter; BP decodes
// bypass this since they're CRC-protected at the raw signal. Hashed <...>
// positions are accepted (the hash resolves to a real call that was logged
// earlier). Previously this accepted messages with at least one allocated
// call, but rescue-path phantoms consistently land on "5Y2MWT 1K7YTV JE76"
// shapes where one side is real-looking — requiring all positions to validate
// catches those without hurting real compound QSOs.
func messageHasAllocatedCall(text string) bool {
	tokens := strings.Fields(strings.ToUpper(text))
	if len(tokens) == 0 {
		return false
	}
	// Figure out which token positions are call positions.
	var callIdx []int
	if tokens[0] == "CQ" {
		idx := 1
		if len(tokens) >= 2 && isCQModifier(tokens[1]) {
			idx = 2
		}
		if idx < len(tokens) {
			callIdx = append(callIdx, idx)
		}
	} else if len(tokens) >= 2 {
		callIdx = []int{0, 1}
	}
	if len(callIdx) == 0 {
		return false
	}
	for _, i := range callIdx {
		tok := tokens[i]
		// Hashed placeholder — always accepted; the original call was logged
		// under a previous decode and doesn't carry a prefix here.
		if len(tok) >= 2 && tok[0] == '<' && tok[len(tok)-1] == '>' {
			continue
		}
		if !hasAllocatedPrefix(tok) {
			return false
		}
	}
	return true
}

// allocatedPrefixes is a curated set of 2-character DXCC amateur prefix
// blocks. A callsign whose first two characters are not in this set is
// assumed unallocated and fails the ITU filter. Grouped by region for
// maintainability; additions are expected as gaps surface.
var allocatedPrefixes = func() map[string]bool {
	raw := `
	AA AB AC AD AE AF AG AH AI AJ AK AL
	K0 K1 K2 K3 K4 K5 K6 K7 K8 K9
	KA KB KC KD KE KF KG KH KI KJ KK KL KM KN KO KP KQ KR KS KT KU KV KW KX KY KZ
	N0 N1 N2 N3 N4 N5 N6 N7 N8 N9
	NA NB NC ND NE NF NG NH NI NJ NK NL NM NN NO NP NQ NR NS NT NU NV NW NX NY NZ
	W0 W1 W2 W3 W4 W5 W6 W7 W8 W9
	WA WB WC WD WE WF WG WH WI WJ WK WL WM WN WO WP WQ WR WS WT WU WV WW WX WY WZ

	VA VB VC VD VE VF VG VO VX VY
	CF CG CH CI CJ CK CY CZ
	XJ XK XL XM XN XO

	XE XF XH
	4A 4B 4C 6D 6E 6F 6G 6H 6I 6J

	G0 G1 G2 G3 G4 G5 G6 G7 G8 G9
	GA GB GC GD GE GF GH GI GJ GK GL GM GN GO GP GQ GR GS GT GU GV GW GX GY GZ
	M0 M1 M2 M3 M4 M5 M6 M7
	MA MB MC MD ME MG MH MI MJ MK ML MM MN MO MP MR MS MT MU MV MW MX MY MZ
	2A 2B 2C 2D 2E 2H 2I 2J 2M 2O 2S 2U 2W

	EI EJ

	DA DB DC DD DE DF DG DH DI DJ DK DL DM DN DO DP DQ DR

	F0 F1 F2 F3 F4 F5 F6 F7 F8 F9
	FA FB FC FD FE FF FG FH FJ FK FM FO FP FR FS FT FW FX FY
	TK TM TO TX

	I0 I1 I2 I3 I4 I5 I6 I7 I8 I9
	IA IB IC ID IE IF IG IH II IK IL IM IN IO IP IQ IR IS IT IU IV IW IX IY IZ

	EA EB EC ED EE EF EG EH

	CQ CR CS CT CU

	ON OO OP OQ OR OS OT
	PA PB PC PD PE PF PG PH PI
	LX

	OE
	OK OL OM
	HA HB HG
	S5 S52 S53 S54 S55 S56 S57 S58 S59
	9A E7
	YU YT YU1 YT1 4N 4O
	Z3 Z6 E4
	LZ
	SV SW SY SZ SX
	YO YP YQ YR
	ER
	SP SQ SR SN SO
	3Z HF

	OH OJ
	OX OY
	TF
	LA LB LG LJ LN
	SM SF SA SB SC SD SE SI SJ SK 7S 8S
	OZ
	ES EW EU EV
	UR US UT UU UV UW UX UY UZ EM EN EO
	YL
	LY

	R0 R1 R2 R3 R4 R5 R6 R7 R8 R9
	RA RB RC RD RE RF RG RH RI RJ RK RL RM RN RO RP RQ RR RS RT RU RV RW RX RY RZ
	UA UB UC UD UE UF UG UH UI UK
	UN UO UP UQ
	4L EK EY EX EZ

	JA JB JC JD JE JF JG JH JI JJ JK JL JM JN JO JP JQ JR JS
	7J 7K 7L 7M 7N
	8J 8K 8L 8M 8N

	YB YC YD YE YF YG YH
	8A 8B 8C 8D 8E 8F 8G 8H 8I

	HL HM DS 6K 6L 6M 6N P5

	BA BD BG BH BI BJ BL BM BN BO BP BQ BR BS BT BU BV BW BX BY BZ
	XX

	VR
	VU AT AU AV AW
	AP AQ AR AS
	S2 S21 S3
	4S
	9N
	XZ XY XW XX9 A5 A6 A7 A9
	8Q
	9V
	9M
	HS E2 HZ 7Z HB0
	YI YK 4K
	TA TB TC YM
	EP
	JT

	VK VL ZL ZM
	YJ
	E5 E51
	A3
	T2 T3 T8 V6 V7 V8 V63 V73 V85
	P2 P29
	H4 H40 H44
	3D 3D2 3DA
	FK FO FP FR FW
	KH KP KL
	ZK ZL7 ZL8 ZL9
	5W

	TI TG TJ TL TN TR TT TU TY TZ
	3C 3V 3X 3Y
	5A 5H 5N 5R 5T 5U 5V 5X 5Y 5Z
	6T 6U 6V 6W 7O 7P 7Q 7X
	9G 9J 9L 9Q 9U 9X 9Y

	CE CX CP OA
	PY PP PQ PR PS PT PU PV PW PX ZV ZW ZX ZY ZZ
	LU L2 L3 L4 L5 L6 L7 L8 L9 AY AZ
	HC HI HK HP HR HQ
	YV
	YN YS
	ZP
	8P 8R
	FY HH HK HI
	CO CM
	J2 J3 J5 J6 J7 J8
	V2 V3 V4
	9Y 9Z
	ZA ZB ZC4 ZD7 ZD8 ZD9 ZF

	C3
	9H
	CN
	3A 3B 3C 5T 9H
	HB
	SV0 SV5 SV9
	T7
	A2 A22 A25
	C2 C5 C6 C7 C8 C9
	D2 D4 D6 D7

	A4 A5 A6 A7 A8 A9
	JY
	OD
	YA YI YK
	4X
	9K 9M 9N
	`
	out := make(map[string]bool, 600)
	for _, tok := range strings.Fields(raw) {
		out[strings.ToUpper(tok)] = true
	}
	return out
}()
