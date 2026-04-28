package ft8

import (
	"fmt"
	"math"
	"strings"
)

// EncodeCQ generates a complete FT8 waveform for "CQ <callsign> <grid>".
// level controls output amplitude (0.0–1.0); use TxLevel as a default.
// audioFreqHz is the audio tone base frequency in Hz (e.g. 1500.0).
// grid must be a 4 or 6 character Maidenhead locator; only the first 4 are used.
func EncodeCQ(callsign, grid string, level, audioFreqHz float64) ([]float32, error) {
	callsign = strings.ToUpper(strings.TrimSpace(callsign))
	grid = strings.ToUpper(strings.TrimSpace(grid))
	if len(grid) < 4 {
		return nil, fmt.Errorf("grid must be at least 4 characters")
	}
	grid = grid[:4]

	bits, err := packCQ(callsign, grid)
	if err != nil {
		return nil, err
	}

	codeword := encodeLDPC(bits)
	tones := modulateTones(codeword)
	return synthesise(tones, level, audioFreqHz), nil
}

// EncodeStandard encodes an arbitrary FT8 message text into a waveform.
// Standard two-callsign formats are tried first; if a callsign is too long
// for the 28-bit standard encoding, the message is automatically packed as a
// Type 4 nonstandard message instead.
//
//	"CALL1 CALL2 GRID"       — grid exchange (standard)
//	"CALL1 CALL2 +NN/-NN"    — SNR report (standard)
//	"CALL1 CALL2 R+NN/R-NN"  — roger + SNR (standard)
//	"CALL1 CALL2 RRR/RR73/73"
//	"NONSTD CALL RRR/RR73/73" — nonstandard fallback (Type 4, SNR dropped)
//
// audioFreqHz is the audio tone base frequency in Hz.
func EncodeStandard(text string, level, audioFreqHz float64) ([]float32, error) {
	bits, err := packStandard(text)
	if err != nil {
		// Fall back to Type 4 nonstandard encoding when a callsign is too long
		// or uses characters not supported by the standard 28-bit scheme.
		bits, err = packNonstd(text)
		if err != nil {
			return nil, err
		}
	}
	codeword := encodeLDPC(bits)
	tones := modulateTones(codeword)
	return synthesise(tones, level, audioFreqHz), nil
}

// packNonstd packs a message containing one nonstandard callsign (too long or
// with characters outside the standard FT8 alphabet) into a Type 4 payload.
//
// Type 4 layout: n12(12) + n58(58) + iflip(1) + nrpt(2) + icq(1) + i3=4(3)
//
// The nonstandard callsign is encoded in the 58-bit field (up to 11 chars,
// base-38 alphabet). The standard callsign is represented by its 12-bit hash.
// SNR reports are not supported in Type 4 and are silently ignored.
func packNonstd(text string) ([K]byte, error) {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(text)))
	if len(fields) < 1 {
		return [K]byte{}, fmt.Errorf("empty message")
	}

	// CQ <nonstd_call>
	if fields[0] == "CQ" && len(fields) >= 2 {
		n58, err := encodeNonstdCall58(fields[1])
		if err != nil {
			return [K]byte{}, fmt.Errorf("CQ nonstd %q: %w", fields[1], err)
		}
		var msg77 [77]byte
		uint64ToBits(0, 12, msg77[0:12])    // n12=0 (unused for CQ)
		uint64ToBits(n58, 58, msg77[12:70]) // nonstandard callsign
		msg77[70] = 0                       // iflip=0
		msg77[71] = 0
		msg77[72] = 0 // nrpt=0 (none)
		msg77[73] = 1 // icq=1
		msg77[74] = 1
		msg77[75] = 0
		msg77[76] = 0 // i3=4
		return addCRC(msg77), nil
	}

	if len(fields) < 2 {
		return [K]byte{}, fmt.Errorf("message needs at least two callsigns: %q", text)
	}
	call1, call2 := fields[0], fields[1]
	extra := ""
	if len(fields) >= 3 {
		extra = fields[2]
	}

	// Determine which callsign is nonstandard.
	_, err1 := encodeCallsign28(call1)
	_, err2 := encodeCallsign28(call2)

	var nonstdCall, stdCall string
	var iflip uint8
	switch {
	case err1 != nil && err2 == nil:
		nonstdCall, stdCall, iflip = call1, call2, 1 // nonstd is displayed first
	case err2 != nil && err1 == nil:
		nonstdCall, stdCall, iflip = call2, call1, 0 // nonstd is displayed second
	case err1 != nil:
		return [K]byte{}, fmt.Errorf("both callsigns are nonstandard: %q %q", call1, call2)
	default:
		return [K]byte{}, fmt.Errorf("neither callsign requires Type 4 encoding")
	}

	n58, err := encodeNonstdCall58(nonstdCall)
	if err != nil {
		return [K]byte{}, fmt.Errorf("nonstd call %q: %w", nonstdCall, err)
	}
	n12 := uint64(hash12(stdCall))
	// Register the standard callsign so we can resolve the hash in replies.
	RegisterCallsign(stdCall)

	// nrpt encoding: only RRR/RR73/73 are supported (no SNR in Type 4).
	var nrpt uint8
	switch extra {
	case "RRR":
		nrpt = 1
	case "RR73":
		nrpt = 2
	case "73":
		nrpt = 3
	default:
		nrpt = 0 // grid, SNR reports, blank — not expressible; treat as no-extra
	}

	var msg77 [77]byte
	uint64ToBits(n12, 12, msg77[0:12])
	uint64ToBits(n58, 58, msg77[12:70])
	msg77[70] = iflip
	msg77[71] = (nrpt >> 1) & 1
	msg77[72] = nrpt & 1
	msg77[73] = 0 // icq=0
	msg77[74] = 1
	msg77[75] = 0
	msg77[76] = 0 // i3=4
	return addCRC(msg77), nil
}

// packStandard packs a standard two-callsign FT8 message into a 91-bit payload.
// Parses: CALL1 CALL2 [GRID | SNR | R+SNR | RRR | RR73 | 73 | (blank)]
func packStandard(text string) ([K]byte, error) {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(text)))
	if len(fields) < 2 {
		return [K]byte{}, fmt.Errorf("message must have at least two callsigns: %q", text)
	}

	call1 := fields[0]
	call2 := fields[1]
	extra := ""
	if len(fields) >= 3 {
		extra = fields[2]
	}

	n28a, err := encodeCall28Token(call1)
	if err != nil {
		return [K]byte{}, fmt.Errorf("call1 %q: %w", call1, err)
	}
	n28b, err := encodeCallsign28(call2)
	if err != nil {
		return [K]byte{}, fmt.Errorf("call2 %q: %w", call2, err)
	}

	// ir=1 when extra starts with "R" (roger flag), igrid4 encodes the rest
	ir := uint8(0)
	igrid4 := uint64(maxgrid4 + 1) // blank by default

	if extra != "" {
		if strings.HasPrefix(extra, "R") {
			ir = 1
			extra = extra[1:] // strip the R for further parsing
		}
		switch extra {
		case "RRR":
			igrid4 = uint64(maxgrid4 + 2)
		case "R73", "RR73":
			// RR73 encoded as maxgrid4+3; if the R was already stripped, "73" → RR73
			igrid4 = uint64(maxgrid4 + 3)
		case "73":
			igrid4 = uint64(maxgrid4 + 4)
		case "":
			// ir was set, extra was just "R" (rare) — leave igrid4 as blank
		default:
			// Try SNR: e.g. "+12", "-07"
			if len(extra) >= 2 && (extra[0] == '+' || extra[0] == '-') {
				snr := 0
				fmt.Sscanf(extra, "%d", &snr)
				igrid4 = uint64(int(maxgrid4) + 35 + snr)
			} else {
				// Try grid: e.g. "FN31"
				v, err := encodeGrid4(extra)
				if err != nil {
					return [K]byte{}, fmt.Errorf("extra field %q: not a grid, SNR, or status: %w", extra, err)
				}
				igrid4 = v
				ir = 0 // grid exchange never has ir=1 here
			}
		}
	}

	n29a := n28a << 1 // ipa=0
	n29b := n28b << 1 // ipb=0

	var msg77 [77]byte
	uint64ToBits(n29a, 29, msg77[0:29])
	uint64ToBits(n29b, 29, msg77[29:58])
	msg77[58] = ir
	uint64ToBits(igrid4, 15, msg77[59:74])
	msg77[74] = 0
	msg77[75] = 0
	msg77[76] = 1 // i3=1 standard
	return addCRC(msg77), nil
}

// encodeCall28Token encodes a callsign that may be a special token (CQ, DE, QRZ).
func encodeCall28Token(call string) (uint64, error) {
	switch call {
	case "CQ":
		return 2, nil
	case "DE":
		return 0, nil
	case "QRZ":
		return 1, nil
	}
	// CQ NNN / CQ XXXX handled by caller — just do standard callsign
	return encodeCallsign28(call)
}

// ── Message packing ───────────────────────────────────────────────────────────

// packCQ packs "CQ <callsign> <grid>" into a 91-bit FT8 payload (77 msg + 14 CRC).
// Bit layout matches ft8_lib/reference design:
//
//	bits  0-28: n29a = (n28_CQ << 1) | ipa   (ipa=0, n28_CQ=2)
//	bits 29-57: n29b = (n28_call << 1) | ipb  (ipb=0)
//	bit  58:    ir=0
//	bits 59-73: igrid4 (15 bits)
//	bits 74-76: i3=1 (standard message type)
func packCQ(callsign, grid string) ([K]byte, error) {
	n28a := uint64(2) // CQ token
	n28b, err := encodeCallsign28(callsign)
	if err != nil {
		return [K]byte{}, fmt.Errorf("callsign: %w", err)
	}
	igrid4, err := encodeGrid4(grid)
	if err != nil {
		return [K]byte{}, fmt.Errorf("grid: %w", err)
	}

	n29a := n28a << 1 // ipa=0
	n29b := n28b << 1 // ipb=0

	var msg77 [77]byte
	uint64ToBits(n29a, 29, msg77[0:29])
	uint64ToBits(n29b, 29, msg77[29:58])
	msg77[58] = 0                          // ir=0
	uint64ToBits(igrid4, 15, msg77[59:74]) // 15 bits (59..73)
	// i3=1: bits 74-76 = 0,0,1
	msg77[74] = 0
	msg77[75] = 0
	msg77[76] = 1

	return addCRC(msg77), nil
}

// addCRC appends the 14-bit CRC to a 77-bit message, returning the 91-bit payload.
// Matches ft8_lib ftx_add_crc: zero-extend to 82 bits, compute CRC14 (poly 0x2757),
// store CRC in bits 77-90.
func addCRC(msg77 [77]byte) [K]byte {
	// Pack 77 bits MSB-first into bytes, zero-extended to 12 bytes
	var b [12]byte
	for i := 0; i < 77; i++ {
		if msg77[i]&1 == 1 {
			b[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	b[9] &= 0xF8 // clear bits 77-79 (CRC storage area)
	b[10] = 0
	b[11] = 0

	crc := crc14(b[:], 82) // 96 - 14 = 82 bits

	var out [K]byte
	copy(out[:77], msg77[:])
	for i := 0; i < 14; i++ {
		out[77+i] = byte((crc >> (13 - uint(i))) & 1)
	}
	return out
}

// encodeCallsign28 encodes a standard callsign into a 28-bit n28 value.
// Matches ft8_lib pack_basecall: n28 = NTOKENS + MAX22 + n where
// n = i0*36*10*27*27*27 + i1*10*27*27*27 + i2*27*27*27 + i3*27*27 + i4*27 + i5
func encodeCallsign28(call string) (uint64, error) {
	call = strings.ToUpper(strings.TrimSpace(call))
	// Normalise so the digit is always at position 2 (ft8_lib pack_basecall rule).
	// e.g. "W5RWD" → " W5RWD", "KO6IEH" stays as-is.
	// Exception: callsigns like "J79WTA" already have a digit at both pos 1 and pos 2;
	// pos 2 is already the serial digit so no prepend is needed.
	if len(call) >= 2 && call[1] >= '0' && call[1] <= '9' {
		if len(call) < 3 || call[2] < '0' || call[2] > '9' {
			// Digit at pos 1 but not pos 2 — prepend space so serial digit lands at pos 2.
			call = " " + call
		}
	}
	for len(call) < 6 {
		call += " "
	}
	if len(call) > 6 {
		return 0, fmt.Errorf("callsign too long: %q", call)
	}

	indexOf := func(alpha string, ch byte) (int, error) {
		idx := strings.IndexByte(alpha, ch)
		if idx < 0 {
			return 0, fmt.Errorf("character %q not in alphabet %q", ch, alpha)
		}
		return idx, nil
	}

	// ft8_lib table ordering: space=0, then digits 0-9, then letters A-Z
	i0, err := indexOf(" 0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", call[0])
	if err != nil {
		return 0, err
	}
	i1, err := indexOf("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", call[1])
	if err != nil {
		return 0, err
	}
	i2, err := indexOf("0123456789", call[2])
	if err != nil {
		return 0, err
	}
	i3, err := indexOf(" ABCDEFGHIJKLMNOPQRSTUVWXYZ", call[3])
	if err != nil {
		return 0, err
	}
	i4, err := indexOf(" ABCDEFGHIJKLMNOPQRSTUVWXYZ", call[4])
	if err != nil {
		return 0, err
	}
	i5, err := indexOf(" ABCDEFGHIJKLMNOPQRSTUVWXYZ", call[5])
	if err != nil {
		return 0, err
	}

	n := uint64(i0)
	n = n*36 + uint64(i1)
	n = n*10 + uint64(i2)
	n = n*27 + uint64(i3)
	n = n*27 + uint64(i4)
	n = n*27 + uint64(i5)
	return uint64(ntokens) + uint64(max22) + n, nil
}

// encodeGrid4 encodes a 4-character Maidenhead grid into a 14-bit igrid4.
// Matches ft8_lib packgrid: igrid4 = (c0-'A')*18*10*10 + (c1-'A')*10*10 + (c2-'0')*10 + (c3-'0')
func encodeGrid4(grid string) (uint64, error) {
	if len(grid) != 4 {
		return 0, fmt.Errorf("grid must be 4 characters, got %q", grid)
	}
	grid = strings.ToUpper(grid)
	c0 := int(grid[0] - 'A')
	c1 := int(grid[1] - 'A')
	c2 := int(grid[2] - '0')
	c3 := int(grid[3] - '0')
	if c0 < 0 || c0 > 17 || c1 < 0 || c1 > 17 || c2 < 0 || c2 > 9 || c3 < 0 || c3 > 9 {
		return 0, fmt.Errorf("invalid grid square %q", grid)
	}
	igrid4 := uint64(c0)*18*10*10 + uint64(c1)*10*10 + uint64(c2)*10 + uint64(c3)
	return igrid4, nil
}

// uint64ToBits writes the low `n` bits of v MSB-first into dst.
func uint64ToBits(v uint64, n int, dst []byte) {
	for i := 0; i < n; i++ {
		dst[i] = byte((v >> uint(n-1-i)) & 1)
	}
}

// packN28 encodes a callsign or special token into the 28-bit n28 value used
// by standard (i3=1/2) FT8 messages. Matches reference design pack28 in
// lib/77bit/packjt77.f90. Falls back to the 22-bit hash path for
// non-standard callsigns (compound, >6 chars, unusual shape).
func packN28(call string) (uint32, bool) {
	c := strings.ToUpper(strings.TrimSpace(call))
	switch c {
	case "DE":
		return 0, true
	case "QRZ":
		return 1, true
	case "CQ":
		return 2, true
	}
	if n, err := encodeCallsign28(c); err == nil {
		return uint32(n), true
	}
	h := hash22(c)
	if h == 0 {
		return 0, false
	}
	return ntokens + h, true
}

// packMessage77 builds the 77-bit FT8 Type-1/Type-2 payload for the given
// fields, MSB-first bit layout matching reference design packjt77.f90:1213.
// ipa/ipb are suffix flags (/R or /P on the corresponding callsign).
func packMessage77(call1, call2 string, ipa, ipb, ir byte, igrid4 uint16, i3 byte) ([77]byte, bool) {
	var out [77]byte
	n28a, ok := packN28(call1)
	if !ok {
		return out, false
	}
	n28b, ok := packN28(call2)
	if !ok {
		return out, false
	}
	n29a := (uint64(n28a) << 1) | uint64(ipa&1)
	n29b := (uint64(n28b) << 1) | uint64(ipb&1)
	uint64ToBits(n29a, 29, out[0:29])
	uint64ToBits(n29b, 29, out[29:58])
	out[58] = ir & 1
	uint64ToBits(uint64(igrid4), 15, out[59:74])
	out[74] = (i3 >> 2) & 1
	out[75] = (i3 >> 1) & 1
	out[76] = i3 & 1
	return out, true
}

// ── LDPC encoding ─────────────────────────────────────────────────────────────

// generatorRows holds the 83×91 generator matrix G in packed hex form,
// from ldpc_174_91_c_generator.f90. Each string is 23 hex nibbles = 92 bits;
// only the first 91 are used.
var generatorRows = [M]string{
	"8329ce11bf31eaf509f27fc",
	"761c264e25c259335493132",
	"dc265902fb277c6410a1bdc",
	"1b3f417858cd2dd33ec7f62",
	"09fda4fee04195fd034783a",
	"077cccc11b8873ed5c3d48a",
	"29b62afe3ca036f4fe1a9da",
	"6054faf5f35d96d3b0c8c3e",
	"e20798e4310eed27884ae90",
	"775c9c08e80e26ddae56318",
	"b0b811028c2bf997213487c",
	"18a0c9231fc60adf5c5ea32",
	"76471e8302a0721e01b12b8",
	"ffbccb80ca8341fafb47b2e",
	"66a72a158f9325a2bf67170",
	"c4243689fe85b1c51363a18",
	"0dff739414d1a1b34b1c270",
	"15b48830636c8b99894972e",
	"29a89c0d3de81d665489b0e",
	"4f126f37fa51cbe61bd6b94",
	"99c47239d0d97d3c84e0940",
	"1919b75119765621bb4f1e8",
	"09db12d731faee0b86df6b8",
	"488fc33df43fbdeea4eafb4",
	"827423ee40b675f756eb5fe",
	"abe197c484cb74757144a9a",
	"2b500e4bc0ec5a6d2bdbdd0",
	"c474aa53d70218761669360",
	"8eba1a13db3390bd6718cec",
	"753844673a27782cc42012e",
	"06ff83a145c37035a5c1268",
	"3b37417858cc2dd33ec3f62",
	"9a4a5a28ee17ca9c324842c",
	"bc29f465309c977e89610a4",
	"2663ae6ddf8b5ce2bb29488",
	"46f231efe457034c1814418",
	"3fb2ce85abe9b0c72e06fbe",
	"de87481f282c153971a0a2e",
	"fcd7ccf23c69fa99bba1412",
	"f0261447e9490ca8e474cec",
	"4410115818196f95cdd7012",
	"088fc31df4bfbde2a4eafb4",
	"b8fef1b6307729fb0a078c0",
	"5afea7acccb77bbc9d99a90",
	"49a7016ac653f65ecdc9076",
	"1944d085be4e7da8d6cc7d0",
	"251f62adc4032f0ee714002",
	"56471f8702a0721e00b12b8",
	"2b8e4923f2dd51e2d537fa0",
	"6b550a40a66f4755de95c26",
	"a18ad28d4e27fe92a4f6c84",
	"10c2e586388cb82a3d80758",
	"ef34a41817ee02133db2eb0",
	"7e9c0c54325a9c15836e000",
	"3693e572d1fde4cdf079e86",
	"bfb2cec5abe1b0c72e07fbe",
	"7ee18230c583cccc57d4b08",
	"a066cb2fedafc9f52664126",
	"bb23725abc47cc5f4cc4cd2",
	"ded9dba3bee40c59b5609b4",
	"d9a7016ac653e6decdc9036",
	"9ad46aed5f707f280ab5fc4",
	"e5921c77822587316d7d3c2",
	"4f14da8242a8b86dca73352",
	"8b8b507ad467d4441df770e",
	"22831c9cf1169467ad04b68",
	"213b838fe2ae54c38ee7180",
	"5d926b6dd71f085181a4e12",
	"66ab79d4b29ee6e69509e56",
	"958148682d748a38dd68baa",
	"b8ce020cf069c32a723ab14",
	"f4331d6d461607e95752746",
	"6da23ba424b9596133cf9c8",
	"a636bcbc7b30c5fbeae67fe",
	"5cb0d86a07df654a9089a20",
	"f11f106848780fc9ecdd80a",
	"1fbb5364fb8d2c9d730d5ba",
	"fcb86bc70a50c9d02a5d034",
	"a534433029eac15f322e34c",
	"c989d9c7c3d3b8c55d75130",
	"7bb38b2f0186d46643ae962",
	"2644ebadeb44b9467d1f42c",
	"608cc857594bfbb55d69600",
}

// parityGenBits holds the parity-row bits of the LDPC generator, pre-parsed
// once from generatorRows' hex strings. Indexed as [i][col] where i in [0,M)
// is the parity row and col in [0,K). Populated at package init; read-only
// thereafter.
//
// Before this cache, encodeLDPC re-parsed the hex strings on every call.
// OSD invokes encodeLDPC once per candidate trial (up to ~2000 trials per
// OSD call on weak candidates), so the parse cost dominated OSD runtime.
var parityGenBits = func() [M][K]byte {
	var g [M][K]byte
	for i := 0; i < M; i++ {
		row := generatorRows[i]
		col := 0
		for _, ch := range row {
			var nibble byte
			if ch >= '0' && ch <= '9' {
				nibble = byte(ch - '0')
			} else {
				nibble = byte(ch-'a') + 10
			}
			for bit := 3; bit >= 0 && col < K; bit-- {
				g[i][col] = (nibble >> uint(bit)) & 1
				col++
			}
		}
	}
	return g
}()

// encodeLDPC encodes a 91-bit message into a 174-bit FT8 codeword.
// Matches encode174_91.f90: codeword = [message | parity], where
// parity[i] = sum(message[j]*G[i][j]) mod 2.
func encodeLDPC(msg [K]byte) [N]byte {
	var codeword [N]byte
	copy(codeword[:K], msg[:])
	for i := 0; i < M; i++ {
		g := &parityGenBits[i]
		var sum byte
		for col := 0; col < K; col++ {
			sum ^= g[col] & msg[col]
		}
		codeword[K+i] = sum
	}
	return codeword
}

// ── FSK tone mapping ──────────────────────────────────────────────────────────

// modulateTones converts a 174-bit codeword into 58 data symbols (3 bits each),
// interleaved with the 3 Costas sync arrays, producing 79 tone indices (0-7).
func modulateTones(codeword [N]byte) [NumSymbols]int {
	// Gray-code map: value → tone (encoder direction, inverse of graymap decoder)
	// encoder graymap from genft8.f90: [0,1,3,2,5,6,4,7]
	encoderGray := [8]int{0, 1, 3, 2, 5, 6, 4, 7}

	var tones [NumSymbols]int
	// Costas arrays at positions 0-6, 36-42, 72-78
	for i, t := range costasSeq {
		tones[i] = t
		tones[36+i] = t
		tones[72+i] = t
	}

	// 58 data symbols, each 3 bits from the codeword
	symIdx := 0
	for sym := 0; sym < NumSymbols; sym++ {
		if sym < 7 || (sym >= 36 && sym < 43) || sym >= 72 {
			continue
		}
		b := symIdx * 3
		val := int(codeword[b])<<2 | int(codeword[b+1])<<1 | int(codeword[b+2])
		tones[sym] = encoderGray[val]
		symIdx++
	}
	return tones
}

// ── Waveform synthesis ────────────────────────────────────────────────────────

const (
	txFreq       = 1500.0 // Hz — base audio frequency for tone 0
	TxSampleRate = 48000  // Hz — USB audio output rate (IC-7300 requirement)
	TxChannels   = 1      // mono — matches reference design IC-7300 configuration
	TxLevel      = 0.1    // output amplitude 0.0–1.0; keep below ALC threshold
)

// txSamplesPerSym is SamplesPerSym scaled to TxSampleRate.
const txSamplesPerSym = SamplesPerSym * TxSampleRate / sampleRate // 7680

// gfskPulse returns the Gaussian pulse shape used by reference design for GFSK modulation.
// bt is the time-bandwidth product (2.0 for FT8), t is in symbol units.
func gfskPulse(bt, t float64) float64 {
	c := math.Pi * math.Sqrt(2.0/math.Log(2.0))
	return 0.5 * (math.Erf(c*bt*(t+0.5)) - math.Erf(c*bt*(t-0.5)))
}

// synthesise converts 79 tone indices into a 15-second mono float32 waveform
// at TxSampleRate (48 kHz) using GFSK modulation matching reference design gen_ft8wave.
//
// This is a faithful port of gen_ft8wave.f90:
//  1. Pre-compute a 3-symbol-wide Gaussian pulse table.
//  2. Build a smoothed dphi[] array where each symbol's tone DEVIATION is
//     spread over 3 symbols via the pulse, then add the base-frequency offset.
//  3. Integrate phase and emit sin(phase).
//  4. Apply raised-cosine ramps to the first/last nsps/8 samples.
func synthesise(tones [NumSymbols]int, level, audioFreqHz float64) []float32 {
	const (
		totalSamples = TxSampleRate * 15 // 720,000
		nsps         = txSamplesPerSym   // 7680 samples per symbol @ 48 kHz
		bt           = 2.0               // GFSK bandwidth-time product
		hmod         = 1.0               // modulation index
		nsym         = NumSymbols        // 79
	)
	twopi := 2.0 * math.Pi
	dt := 1.0 / float64(TxSampleRate)

	// Step 1: Pre-compute the Gaussian pulse (3 symbols wide = 3*nsps samples).
	pulseLen := 3 * nsps
	pulse := make([]float64, pulseLen)
	for i := 0; i < pulseLen; i++ {
		tt := (float64(i) - 1.5*float64(nsps)) / float64(nsps)
		pulse[i] = gfskPulse(bt, tt)
	}

	// Step 2: Build dphi array. Length = (nsym+2)*nsps to include dummy symbols.
	dphiLen := (nsym + 2) * nsps
	dphi := make([]float64, dphiLen)
	dphiPeak := twopi * hmod / float64(nsps)

	// Each real symbol spreads over 3*nsps samples starting at symbol offset.
	for j := 0; j < nsym; j++ {
		ib := j * nsps
		for i := 0; i < pulseLen; i++ {
			dphi[ib+i] += dphiPeak * pulse[i] * float64(tones[j])
		}
	}

	// Dummy symbols: extend first and last tone values to stabilise edges.
	// First dummy: pulse(nsps+1 : 3*nsps) applied at dphi(0 : 2*nsps-1)
	for i := nsps; i < pulseLen; i++ {
		dphi[i-nsps] += dphiPeak * float64(tones[0]) * pulse[i]
	}
	// Last dummy: pulse(0 : 2*nsps-1) applied at dphi(nsym*nsps : (nsym+2)*nsps-1)
	for i := 0; i < 2*nsps; i++ {
		idx := nsym*nsps + i
		if idx < dphiLen {
			dphi[idx] += dphiPeak * float64(tones[nsym-1]) * pulse[i]
		}
	}

	// Add base frequency offset (f0) to entire dphi array.
	if audioFreqHz <= 0 {
		audioFreqHz = txFreq // fallback to default 1500 Hz
	}
	f0dphi := twopi * audioFreqHz * dt
	for i := range dphi {
		dphi[i] += f0dphi
	}

	// Step 3: Generate waveform. Skip the first dummy symbol (start at sample nsps).
	out := make([]float32, totalSamples)
	startSample := int(0.5 * float64(TxSampleRate)) // 0.5s offset into the 15s window
	phi := 0.0
	for j := 0; j < nsym*nsps; j++ {
		k := startSample + j
		if k >= totalSamples {
			break
		}
		out[k] = float32(level * math.Sin(phi))
		phi = math.Mod(phi+dphi[nsps+j], twopi) // nsps offset skips first dummy symbol
	}

	// Step 4: Raised-cosine envelope ramps (nsps/8 samples, matching reference design nramp).
	nramp := nsps / 8
	for i := 0; i < nramp; i++ {
		k := startSample + i
		if k < totalSamples {
			w := float32(0.5 * (1.0 - math.Cos(twopi*float64(i)/float64(2*nramp))))
			out[k] *= w
		}
	}
	k1 := startSample + nsym*nsps - nramp
	for i := 0; i < nramp; i++ {
		k := k1 + i
		if k >= 0 && k < totalSamples {
			w := float32(0.5 * (1.0 + math.Cos(twopi*float64(i)/float64(2*nramp))))
			out[k] *= w
		}
	}

	return out
}
