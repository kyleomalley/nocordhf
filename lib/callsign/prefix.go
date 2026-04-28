// Package callsign maps amateur radio callsign prefixes to geopolitical entities
// with representative lat/lon coordinates for map placement when no grid square
// is available. Lookup uses longest-prefix-match, consistent with cty.dat conventions.
package callsign

import "strings"

// Entity is a DXCC entity / geopolitical region.
type Entity struct {
	Name string
	Lat  float64 // degrees north (negative = south)
	Lon  float64 // degrees east (negative = west)
}

// Lookup returns the best-matching entity for a callsign, using longest-prefix-match.
// The call is normalised (uppercased, portable/mobile suffixes stripped) before matching.
// Returns ok=false only if no prefix matched at all.
func Lookup(call string) (e Entity, ok bool) {
	call = normalise(call)
	if call == "" {
		return Entity{}, false
	}
	maxLen := len(call)
	if maxLen > 6 {
		maxLen = 6
	}
	for l := maxLen; l > 0; l-- {
		if ent, found := table[call[:l]]; found {
			return ent, true
		}
	}
	return Entity{}, false
}

// normalise uppercases the call and strips suffixes like /P /M /MM /QRP /AM.
// For compound calls (e.g. DL1ABC/W5, VP9/K1ABC) the part that looks like a
// standalone callsign prefix is kept; the rest is dropped.
func normalise(call string) string {
	call = strings.ToUpper(strings.TrimSpace(call))
	if idx := strings.Index(call, "/"); idx >= 0 {
		left, right := call[:idx], call[idx+1:]
		// Short right-side modifiers (P, M, MM, QRP, AM, etc.) are portable indicators.
		switch right {
		case "P", "M", "MM", "QRP", "AM", "A", "B":
			return left
		}
		// If right looks like a prefix-only fragment (no digits) use the left side.
		hasDigit := false
		for _, c := range right {
			if c >= '0' && c <= '9' {
				hasDigit = true
				break
			}
		}
		if !hasDigit {
			return left
		}
		// Right has digits — it might be the full callsign (VP9/K1ABC → use "VP9/K1ABC" left).
		// Use whichever side is shorter (more likely to be the operator's home prefix).
		if len(left) <= len(right) {
			return left
		}
		return right
	}
	return call
}

// table maps prefixes to entities. Longer prefixes shadow shorter ones automatically
// because Lookup iterates from longest to shortest.
var table = map[string]Entity{

	// ── North America ────────────────────────────────────────────────────────

	// USA special areas (must be listed before the single-letter K/W/N entries)
	"KH6": {"Hawaii", 21.3, -157.8},
	"AH6": {"Hawaii", 21.3, -157.8},
	"NH6": {"Hawaii", 21.3, -157.8},
	"WH6": {"Hawaii", 21.3, -157.8},
	"KH0": {"N. Mariana Islands", 14.5, 145.8},
	"AH0": {"N. Mariana Islands", 14.5, 145.8},
	"KH2": {"Guam", 13.4, 144.7},
	"AH2": {"Guam", 13.4, 144.7},
	"KH3": {"Johnston Atoll", 16.7, -169.5},
	"KH4": {"Midway Islands", 28.2, -177.4},
	"KH8": {"American Samoa", -14.3, -170.7},
	"KH9": {"Wake Island", 19.3, 166.6},
	"KL":  {"Alaska", 61.2, -149.9},
	"AL":  {"Alaska", 61.2, -149.9},
	"NL":  {"Alaska", 61.2, -149.9},
	"WL":  {"Alaska", 61.2, -149.9},
	"KP1": {"Navassa Island", 18.4, -75.0},
	"KP2": {"US Virgin Islands", 17.7, -64.7},
	"NP2": {"US Virgin Islands", 17.7, -64.7},
	"WP2": {"US Virgin Islands", 17.7, -64.7},
	"KP4": {"Puerto Rico", 18.2, -66.6},
	"NP4": {"Puerto Rico", 18.2, -66.6},
	"WP4": {"Puerto Rico", 18.2, -66.6},
	"KP5": {"Desecheo Island", 18.4, -67.5},
	// USA mainland (all AA-AK blocks + W/K/N)
	"AA": {"United States", 37.1, -95.7},
	"AB": {"United States", 37.1, -95.7},
	"AC": {"United States", 37.1, -95.7},
	"AD": {"United States", 37.1, -95.7},
	"AE": {"United States", 37.1, -95.7},
	"AF": {"United States", 37.1, -95.7},
	"AG": {"United States", 37.1, -95.7},
	"AI": {"United States", 37.1, -95.7},
	"AJ": {"United States", 37.1, -95.7},
	"AK": {"United States", 37.1, -95.7},
	"W":  {"United States", 37.1, -95.7},
	"K":  {"United States", 37.1, -95.7},
	"N":  {"United States", 37.1, -95.7},
	// Canada
	"VE": {"Canada", 56.1, -96.3},
	"VA": {"Canada", 56.1, -96.3},
	"VY": {"Canada", 56.1, -96.3},
	// Mexico
	"XE": {"Mexico", 24.0, -103.0},
	"XF": {"Mexico", 24.0, -103.0},

	// ── Caribbean / Central America ──────────────────────────────────────────
	"TI":  {"Costa Rica", 10.0, -84.0},
	"TG":  {"Guatemala", 15.5, -90.2},
	"HQ":  {"Honduras", 15.0, -86.5},
	"HR":  {"Honduras", 15.0, -86.5},
	"YN":  {"Nicaragua", 12.9, -85.6},
	"HP":  {"Panama", 8.6, -79.5},
	"YS":  {"El Salvador", 13.7, -89.0},
	"HH":  {"Haiti", 18.9, -72.3},
	"HI":  {"Dominican Republic", 18.9, -70.2},
	"J3":  {"Grenada", 12.1, -61.7},
	"J6":  {"St. Lucia", 13.9, -60.9},
	"J7":  {"Dominica", 15.4, -61.4},
	"J8":  {"St. Vincent", 13.1, -61.2},
	"V2":  {"Antigua & Barbuda", 17.1, -61.8},
	"V4":  {"St. Kitts & Nevis", 17.3, -62.7},
	"8P":  {"Barbados", 13.1, -59.6},
	"6Y":  {"Jamaica", 18.2, -77.4},
	"C6":  {"Bahamas", 25.0, -77.5},
	"ZF":  {"Cayman Islands", 19.3, -81.4},
	"VP5": {"Turks & Caicos", 21.8, -71.8},
	"VP9": {"Bermuda", 32.3, -64.7},
	"PJ2": {"Curacao", 12.2, -69.0},
	"PJ4": {"Bonaire", 12.2, -68.3},
	"PJ7": {"Sint Maarten", 18.0, -63.1},
	"FM":  {"Martinique", 14.7, -61.0},
	"FG":  {"Guadeloupe", 16.2, -61.5},
	"FJ":  {"St. Barthelemy", 17.9, -62.8},

	// ── South America ────────────────────────────────────────────────────────
	"PY": {"Brazil", -10.0, -53.0},
	"PR": {"Brazil", -10.0, -53.0},
	"PS": {"Brazil", -10.0, -53.0},
	"PT": {"Brazil", -10.0, -53.0},
	"PU": {"Brazil", -10.0, -53.0},
	"PV": {"Brazil", -10.0, -53.0},
	"PW": {"Brazil", -10.0, -53.0},
	"PX": {"Brazil", -10.0, -53.0},
	"LU": {"Argentina", -34.0, -64.0},
	"CE": {"Chile", -30.0, -70.7},
	"CX": {"Uruguay", -33.0, -56.0},
	"OA": {"Peru", -9.2, -74.7},
	"OB": {"Peru", -9.2, -74.7},
	"HC": {"Ecuador", -1.8, -78.2},
	"HK": {"Colombia", 4.6, -74.1},
	"5J": {"Colombia", 4.6, -74.1},
	"YV": {"Venezuela", 8.0, -66.0},
	"CP": {"Bolivia", -17.0, -65.0},
	"ZP": {"Paraguay", -23.0, -58.0},
	"FY": {"French Guiana", 4.0, -53.0},
	"PZ": {"Suriname", 4.0, -56.0},
	"8R": {"Guyana", 5.0, -59.0},

	// ── Europe ───────────────────────────────────────────────────────────────
	// UK & Ireland
	"G":  {"England", 51.5, -0.1},
	"M":  {"England", 51.5, -0.1},
	"2E": {"England", 51.5, -0.1},
	"GM": {"Scotland", 57.0, -4.0},
	"MM": {"Scotland", 57.0, -4.0},
	"2M": {"Scotland", 57.0, -4.0},
	"GW": {"Wales", 52.5, -3.5},
	"MW": {"Wales", 52.5, -3.5},
	"GI": {"N. Ireland", 54.7, -6.2},
	"MI": {"N. Ireland", 54.7, -6.2},
	"GJ": {"Jersey", 49.2, -2.2},
	"MJ": {"Jersey", 49.2, -2.2},
	"GU": {"Guernsey", 49.5, -2.6},
	"MU": {"Guernsey", 49.5, -2.6},
	"GD": {"Isle of Man", 54.2, -4.5},
	"MD": {"Isle of Man", 54.2, -4.5},
	"EI": {"Ireland", 53.2, -8.2},
	"EJ": {"Ireland", 53.2, -8.2},
	// Germany
	"DL": {"Germany", 51.2, 10.4},
	"DA": {"Germany", 51.2, 10.4},
	"DB": {"Germany", 51.2, 10.4},
	"DC": {"Germany", 51.2, 10.4},
	"DD": {"Germany", 51.2, 10.4},
	"DE": {"Germany", 51.2, 10.4},
	"DF": {"Germany", 51.2, 10.4},
	"DG": {"Germany", 51.2, 10.4},
	"DH": {"Germany", 51.2, 10.4},
	"DI": {"Germany", 51.2, 10.4},
	"DJ": {"Germany", 51.2, 10.4},
	"DK": {"Germany", 51.2, 10.4},
	"DM": {"Germany", 51.2, 10.4},
	"DN": {"Germany", 51.2, 10.4},
	"DO": {"Germany", 51.2, 10.4},
	"DP": {"Germany", 51.2, 10.4},
	// France
	"F":  {"France", 46.2, 2.2},
	"TM": {"France", 46.2, 2.2},
	// Italy
	"I":  {"Italy", 42.8, 12.8},
	"IA": {"Italy", 42.8, 12.8},
	"IB": {"Italy", 42.8, 12.8},
	"IC": {"Italy", 42.8, 12.8},
	"ID": {"Italy", 42.8, 12.8},
	"IE": {"Italy", 42.8, 12.8},
	"IF": {"Italy", 42.8, 12.8},
	"IG": {"Italy", 42.8, 12.8},
	"IH": {"Italy", 42.8, 12.8},
	"II": {"Italy", 42.8, 12.8},
	"IK": {"Italy", 42.8, 12.8},
	"IN": {"Italy", 42.8, 12.8},
	"IQ": {"Italy", 42.8, 12.8},
	"IR": {"Italy", 42.8, 12.8},
	"IS": {"Italy", 42.8, 12.8},
	"IU": {"Italy", 42.8, 12.8},
	"IW": {"Italy", 42.8, 12.8},
	"IZ": {"Italy", 42.8, 12.8},
	// Spain
	"EA":  {"Spain", 40.4, -3.7},
	"EB":  {"Spain", 40.4, -3.7},
	"EC":  {"Spain", 40.4, -3.7},
	"ED":  {"Spain", 40.4, -3.7},
	"EE":  {"Spain", 40.4, -3.7},
	"EF":  {"Spain", 40.4, -3.7},
	"EG":  {"Spain", 40.4, -3.7},
	"EH":  {"Spain", 40.4, -3.7},
	"EA6": {"Balearic Islands", 39.6, 2.9},
	"EB6": {"Balearic Islands", 39.6, 2.9},
	"EA8": {"Canary Islands", 28.1, -15.6},
	"EB8": {"Canary Islands", 28.1, -15.6},
	"EA9": {"Ceuta & Melilla", 35.9, -5.3},
	// Portugal
	"CT":  {"Portugal", 39.5, -8.0},
	"CQ":  {"Portugal", 39.5, -8.0},
	"CR":  {"Portugal", 39.5, -8.0},
	"CS":  {"Portugal", 39.5, -8.0},
	"CT3": {"Madeira", 32.7, -17.0},
	"CQ3": {"Madeira", 32.7, -17.0},
	"CU":  {"Azores", 38.6, -27.2},
	// Netherlands
	"PA": {"Netherlands", 52.3, 5.3},
	"PB": {"Netherlands", 52.3, 5.3},
	"PC": {"Netherlands", 52.3, 5.3},
	"PD": {"Netherlands", 52.3, 5.3},
	"PE": {"Netherlands", 52.3, 5.3},
	"PF": {"Netherlands", 52.3, 5.3},
	"PG": {"Netherlands", 52.3, 5.3},
	"PH": {"Netherlands", 52.3, 5.3},
	"PI": {"Netherlands", 52.3, 5.3},
	// Belgium
	"ON": {"Belgium", 50.5, 4.4},
	"OO": {"Belgium", 50.5, 4.4},
	"OP": {"Belgium", 50.5, 4.4},
	"OQ": {"Belgium", 50.5, 4.4},
	"OR": {"Belgium", 50.5, 4.4},
	"OS": {"Belgium", 50.5, 4.4},
	"OT": {"Belgium", 50.5, 4.4},
	// Switzerland
	"HB":  {"Switzerland", 46.8, 8.2},
	"HB0": {"Liechtenstein", 47.2, 9.5},
	// Austria
	"OE": {"Austria", 47.8, 13.3},
	// Hungary
	"HA": {"Hungary", 47.5, 19.1},
	"HG": {"Hungary", 47.5, 19.1},
	// Czech Republic
	"OK": {"Czech Republic", 50.1, 14.4},
	"OL": {"Czech Republic", 50.1, 14.4},
	// Slovakia
	"OM": {"Slovakia", 48.7, 19.2},
	// Poland
	"SP": {"Poland", 52.1, 19.1},
	"SN": {"Poland", 52.1, 19.1},
	"SO": {"Poland", 52.1, 19.1},
	"SQ": {"Poland", 52.1, 19.1},
	"SR": {"Poland", 52.1, 19.1},
	// Scandinavia
	"OH":  {"Finland", 64.9, 25.7},
	"OF":  {"Finland", 64.9, 25.7},
	"OG":  {"Finland", 64.9, 25.7},
	"OH0": {"Aland Islands", 60.2, 19.9},
	"OG0": {"Aland Islands", 60.2, 19.9},
	"SM":  {"Sweden", 59.3, 18.1},
	"SA":  {"Sweden", 59.3, 18.1},
	"SB":  {"Sweden", 59.3, 18.1},
	"SC":  {"Sweden", 59.3, 18.1},
	"SD":  {"Sweden", 59.3, 18.1},
	"SE":  {"Sweden", 59.3, 18.1},
	"SF":  {"Sweden", 59.3, 18.1},
	"SG":  {"Sweden", 59.3, 18.1},
	"SH":  {"Sweden", 59.3, 18.1},
	"SI":  {"Sweden", 59.3, 18.1},
	"SJ":  {"Sweden", 59.3, 18.1},
	"SK":  {"Sweden", 59.3, 18.1},
	"SL":  {"Sweden", 59.3, 18.1},
	"LA":  {"Norway", 60.5, 8.5},
	"LB":  {"Norway", 60.5, 8.5},
	"LC":  {"Norway", 60.5, 8.5},
	"LD":  {"Norway", 60.5, 8.5},
	"LE":  {"Norway", 60.5, 8.5},
	"LF":  {"Norway", 60.5, 8.5},
	"LG":  {"Norway", 60.5, 8.5},
	"LH":  {"Norway", 60.5, 8.5},
	"LI":  {"Norway", 60.5, 8.5},
	"LJ":  {"Norway", 60.5, 8.5},
	"LK":  {"Norway", 60.5, 8.5},
	"LL":  {"Norway", 60.5, 8.5},
	"LM":  {"Norway", 60.5, 8.5},
	"LN":  {"Norway", 60.5, 8.5},
	"OZ":  {"Denmark", 56.3, 10.6},
	"OU":  {"Denmark", 56.3, 10.6},
	"OV":  {"Denmark", 56.3, 10.6},
	"OW":  {"Denmark", 56.3, 10.6},
	"OX":  {"Greenland", 72.0, -41.0},
	"OY":  {"Faroe Islands", 62.0, -7.0},
	"TF":  {"Iceland", 65.0, -18.0},
	// Baltic states
	"YL": {"Latvia", 57.0, 24.8},
	"ES": {"Estonia", 58.7, 25.0},
	"LY": {"Lithuania", 55.9, 23.3},
	// Romania
	"YO": {"Romania", 45.9, 24.9},
	"YP": {"Romania", 45.9, 24.9},
	"YQ": {"Romania", 45.9, 24.9},
	"YR": {"Romania", 45.9, 24.9},
	// Bulgaria
	"LZ": {"Bulgaria", 42.7, 25.5},
	// Greece & Aegean
	"SV":  {"Greece", 38.0, 23.7},
	"SX":  {"Greece", 38.0, 23.7},
	"SY":  {"Greece", 38.0, 23.7},
	"SZ":  {"Greece", 38.0, 23.7},
	"SW":  {"Greece", 38.0, 23.7},
	"SV5": {"Dodecanese", 36.4, 28.2},
	"SX5": {"Dodecanese", 36.4, 28.2},
	"SV9": {"Crete", 35.3, 24.8},
	"SX9": {"Crete", 35.3, 24.8},
	// Turkey
	"TA": {"Turkey", 39.1, 35.0},
	"TB": {"Turkey", 39.1, 35.0},
	"TC": {"Turkey", 39.1, 35.0},
	"YM": {"Turkey", 39.1, 35.0},
	// Balkans
	"YU": {"Serbia", 44.5, 20.5},
	"YT": {"Serbia", 44.5, 20.5},
	"YZ": {"Serbia", 44.5, 20.5},
	"S5": {"Slovenia", 46.1, 14.8},
	"9A": {"Croatia", 45.1, 15.2},
	"4O": {"Montenegro", 42.5, 19.3},
	"Z6": {"Kosovo", 42.6, 20.9},
	"Z3": {"N. Macedonia", 41.6, 21.7},
	"E7": {"Bosnia-Herzegovina", 44.2, 17.4},
	"T9": {"Bosnia-Herzegovina", 44.2, 17.4},
	"ZA": {"Albania", 41.1, 20.0},
	// Middle East
	"4X": {"Israel", 31.5, 35.0},
	"4Z": {"Israel", 31.5, 35.0},
	"OD": {"Lebanon", 33.9, 35.5},
	"YK": {"Syria", 34.8, 38.9},
	"JY": {"Jordan", 31.9, 35.9},
	"A4": {"Oman", 23.6, 58.1},
	"A5": {"Bhutan", 27.5, 90.4},
	"A6": {"United Arab Emirates", 24.5, 54.4},
	"A7": {"Qatar", 25.3, 51.2},
	"A9": {"Bahrain", 26.0, 50.6},
	"HZ": {"Saudi Arabia", 25.0, 45.0},
	"7Z": {"Saudi Arabia", 25.0, 45.0},
	"8Z": {"Saudi Arabia", 25.0, 45.0},
	"YI": {"Iraq", 33.3, 44.4},
	"EP": {"Iran", 32.0, 53.0},
	"EQ": {"Iran", 32.0, 53.0},
	"AP": {"Pakistan", 30.0, 70.0},
	"6P": {"Pakistan", 30.0, 70.0},
	// Russia (split European/Asiatic)
	"UA9": {"Asiatic Russia", 60.0, 100.0},
	"UA0": {"Asiatic Russia", 60.0, 100.0},
	"RA9": {"Asiatic Russia", 60.0, 100.0},
	"RA0": {"Asiatic Russia", 60.0, 100.0},
	"RK9": {"Asiatic Russia", 60.0, 100.0},
	"RK0": {"Asiatic Russia", 60.0, 100.0},
	"RN9": {"Asiatic Russia", 60.0, 100.0},
	"RN0": {"Asiatic Russia", 60.0, 100.0},
	"RW9": {"Asiatic Russia", 60.0, 100.0},
	"RW0": {"Asiatic Russia", 60.0, 100.0},
	"UA2": {"Kaliningrad", 54.7, 20.5},
	"RA2": {"Kaliningrad", 54.7, 20.5},
	"UA":  {"Russia", 55.8, 37.6},
	"RA":  {"Russia", 55.8, 37.6},
	"RK":  {"Russia", 55.8, 37.6},
	"RL":  {"Russia", 55.8, 37.6},
	"RM":  {"Russia", 55.8, 37.6},
	"RN":  {"Russia", 55.8, 37.6},
	"RO":  {"Russia", 55.8, 37.6},
	"RP":  {"Russia", 55.8, 37.6},
	"RQ":  {"Russia", 55.8, 37.6},
	"RR":  {"Russia", 55.8, 37.6},
	"RS":  {"Russia", 55.8, 37.6},
	"RT":  {"Russia", 55.8, 37.6},
	"RU":  {"Russia", 55.8, 37.6},
	"RV":  {"Russia", 55.8, 37.6},
	"RW":  {"Russia", 55.8, 37.6},
	"RX":  {"Russia", 55.8, 37.6},
	"RY":  {"Russia", 55.8, 37.6},
	"RZ":  {"Russia", 55.8, 37.6},
	// Ukraine, Belarus, Moldova
	"UR": {"Ukraine", 48.4, 31.2},
	"US": {"Ukraine", 48.4, 31.2},
	"UT": {"Ukraine", 48.4, 31.2},
	"UU": {"Ukraine", 48.4, 31.2},
	"UV": {"Ukraine", 48.4, 31.2},
	"UW": {"Ukraine", 48.4, 31.2},
	"UX": {"Ukraine", 48.4, 31.2},
	"UY": {"Ukraine", 48.4, 31.2},
	"UZ": {"Ukraine", 48.4, 31.2},
	"EW": {"Belarus", 53.7, 27.9},
	"ER": {"Moldova", 47.0, 28.9},
	// Caucasus
	"4J": {"Azerbaijan", 40.4, 49.9},
	"4K": {"Azerbaijan", 40.4, 49.9},
	"4L": {"Georgia", 41.7, 44.8},
	"EK": {"Armenia", 40.2, 44.5},
	// Central Asia
	"EX": {"Kyrgyzstan", 41.2, 74.8},
	"EY": {"Tajikistan", 38.6, 68.8},
	"EZ": {"Turkmenistan", 38.0, 57.9},
	"UK": {"Uzbekistan", 41.4, 64.6},
	"UN": {"Kazakhstan", 48.0, 67.0},
	"UO": {"Kazakhstan", 48.0, 67.0},
	"UP": {"Kazakhstan", 48.0, 67.0},
	"UQ": {"Kazakhstan", 48.0, 67.0},
	// Afghanistan
	"YA": {"Afghanistan", 34.5, 69.2},
	"T6": {"Afghanistan", 34.5, 69.2},

	// ── Asia-Pacific ─────────────────────────────────────────────────────────
	// Japan
	"JA": {"Japan", 35.7, 139.7},
	"JB": {"Japan", 35.7, 139.7},
	"JC": {"Japan", 35.7, 139.7},
	"JD": {"Japan", 35.7, 139.7},
	"JE": {"Japan", 35.7, 139.7},
	"JF": {"Japan", 35.7, 139.7},
	"JG": {"Japan", 35.7, 139.7},
	"JH": {"Japan", 35.7, 139.7},
	"JI": {"Japan", 35.7, 139.7},
	"JJ": {"Japan", 35.7, 139.7},
	"JK": {"Japan", 35.7, 139.7},
	"JL": {"Japan", 35.7, 139.7},
	"JM": {"Japan", 35.7, 139.7},
	"JN": {"Japan", 35.7, 139.7},
	"JO": {"Japan", 35.7, 139.7},
	"JP": {"Japan", 35.7, 139.7},
	"JQ": {"Japan", 35.7, 139.7},
	"JR": {"Japan", 35.7, 139.7},
	"JS": {"Japan", 35.7, 139.7},
	"7J": {"Japan", 35.7, 139.7},
	"8J": {"Japan", 35.7, 139.7},
	// South Korea
	"HL": {"South Korea", 37.6, 127.0},
	"DS": {"South Korea", 37.6, 127.0},
	"DT": {"South Korea", 37.6, 127.0},
	// China
	"BA": {"China", 35.9, 104.2},
	"BB": {"China", 35.9, 104.2},
	"BC": {"China", 35.9, 104.2},
	"BD": {"China", 35.9, 104.2},
	"BE": {"China", 35.9, 104.2},
	"BF": {"China", 35.9, 104.2},
	"BG": {"China", 35.9, 104.2},
	"BH": {"China", 35.9, 104.2},
	"BI": {"China", 35.9, 104.2},
	"BJ": {"China", 35.9, 104.2},
	"BK": {"China", 35.9, 104.2},
	"BL": {"China", 35.9, 104.2},
	"BM": {"China", 35.9, 104.2},
	"BN": {"China", 35.9, 104.2},
	"BO": {"China", 35.9, 104.2},
	"BP": {"China", 35.9, 104.2},
	"BQ": {"China", 35.9, 104.2},
	"BR": {"China", 35.9, 104.2},
	"BS": {"China", 35.9, 104.2},
	"BT": {"China", 35.9, 104.2},
	"BU": {"China", 35.9, 104.2},
	"BV": {"Taiwan", 25.0, 121.5},
	"BY": {"China", 35.9, 104.2},
	"BZ": {"China", 35.9, 104.2},
	// India
	"VU": {"India", 20.6, 78.9},
	"AT": {"India", 20.6, 78.9},
	"AU": {"India", 20.6, 78.9},
	// Southeast Asia
	"HS": {"Thailand", 15.9, 100.9},
	"E2": {"Thailand", 15.9, 100.9},
	"XV": {"Vietnam", 16.0, 107.8},
	"3W": {"Vietnam", 16.0, 107.8},
	"XW": {"Laos", 18.0, 103.0},
	"XU": {"Cambodia", 12.6, 104.9},
	"XY": {"Myanmar", 19.8, 96.1},
	"XZ": {"Myanmar", 19.8, 96.1},
	"9V": {"Singapore", 1.3, 103.8},
	"S6": {"Singapore", 1.3, 103.8},
	"9M": {"Malaysia", 3.1, 108.9},
	"9W": {"Malaysia", 3.1, 108.9},
	"YB": {"Indonesia", -5.0, 120.0},
	"YC": {"Indonesia", -5.0, 120.0},
	"YD": {"Indonesia", -5.0, 120.0},
	"YE": {"Indonesia", -5.0, 120.0},
	"YF": {"Indonesia", -5.0, 120.0},
	"YG": {"Indonesia", -5.0, 120.0},
	"YH": {"Indonesia", -5.0, 120.0},
	"PK": {"Indonesia", -5.0, 120.0},
	"PL": {"Indonesia", -5.0, 120.0},
	"PM": {"Indonesia", -5.0, 120.0},
	"PN": {"Indonesia", -5.0, 120.0},
	"PO": {"Indonesia", -5.0, 120.0},
	"DU": {"Philippines", 12.9, 121.8},
	"4D": {"Philippines", 12.9, 121.8},
	"4E": {"Philippines", 12.9, 121.8},
	"4F": {"Philippines", 12.9, 121.8},
	"4G": {"Philippines", 12.9, 121.8},
	"4I": {"Philippines", 12.9, 121.8},
	"VR": {"Hong Kong", 22.3, 114.2},
	"XX": {"Macao", 22.2, 113.5},
	// Mongolia
	"JT": {"Mongolia", 47.9, 106.9},
	"JU": {"Mongolia", 47.9, 106.9},
	"JV": {"Mongolia", 47.9, 106.9},
	// Sri Lanka
	"4S": {"Sri Lanka", 7.9, 80.7},
	// Bangladesh
	"S2": {"Bangladesh", 23.7, 90.4},
	"S3": {"Bangladesh", 23.7, 90.4},
	// Nepal
	"9N": {"Nepal", 28.4, 84.1},
	// Maldives
	"8Q": {"Maldives", 4.2, 73.2},
	// North Korea
	"P5": {"North Korea", 39.0, 125.8},
	"P6": {"North Korea", 39.0, 125.8},
	// Cyprus
	"5B": {"Cyprus", 35.1, 33.4},
	"C4": {"Cyprus", 35.1, 33.4},
	"P3": {"Cyprus", 35.1, 33.4},

	// ── Oceania ───────────────────────────────────────────────────────────────
	"VK":  {"Australia", -26.0, 133.0},
	"ZL":  {"New Zealand", -41.3, 174.8},
	"ZM":  {"New Zealand", -41.3, 174.8},
	"FK":  {"New Caledonia", -21.3, 165.5},
	"FO":  {"French Polynesia", -17.6, -149.4},
	"KH1": {"Baker & Howland", 0.2, -176.5},
	"KH7": {"Johnston Atoll", 16.7, -169.5},
	"T2":  {"Tuvalu", -8.5, 179.2},
	"T3":  {"Kiribati (West)", 1.4, -157.4},
	"A3":  {"Tonga", -21.2, -175.2},
	"ZK":  {"Cook Islands", -21.2, -159.8},
	"E5":  {"Cook Islands", -21.2, -159.8},
	"YJ":  {"Vanuatu", -15.4, 166.9},
	"H4":  {"Solomon Islands", -9.4, 160.2},
	"H40": {"Temotu", -10.7, 165.8},
	"P2":  {"Papua New Guinea", -6.3, 143.9},
	"3D2": {"Fiji", -17.7, 178.5},
	"5W":  {"Samoa", -13.8, -172.1},
	"C2":  {"Nauru", -0.5, 166.9},

	// ── Africa ────────────────────────────────────────────────────────────────
	"ZS":  {"South Africa", -29.0, 25.1},
	"ZT":  {"South Africa", -29.0, 25.1},
	"ZU":  {"South Africa", -29.0, 25.1},
	"ZR":  {"South Africa", -29.0, 25.1},
	"5N":  {"Nigeria", 9.1, 8.7},
	"5O":  {"Nigeria", 9.1, 8.7},
	"9J":  {"Zambia", -13.1, 27.8},
	"9I":  {"Zambia", -13.1, 27.8},
	"Z2":  {"Zimbabwe", -20.0, 30.0},
	"Z21": {"Zimbabwe", -20.0, 30.0},
	"7Q":  {"Malawi", -13.3, 34.3},
	"3B":  {"Mauritius", -20.3, 57.5},
	"5R":  {"Madagascar", -18.9, 47.5},
	"6O":  {"Somalia", 5.1, 46.2},
	"ET":  {"Ethiopia", 9.0, 38.7},
	"5Z":  {"Kenya", 0.0, 37.9},
	"5H":  {"Tanzania", -6.4, 34.9},
	"5I":  {"Tanzania", -6.4, 34.9},
	"9X":  {"Rwanda", -1.9, 29.9},
	"9Y":  {"Trinidad & Tobago", 10.5, -61.3},
	"9Z":  {"Trinidad & Tobago", 10.5, -61.3},
	"5X":  {"Uganda", 1.4, 32.3},
	"9Q":  {"DR Congo", -4.3, 23.7},
	"9R":  {"DR Congo", -4.3, 23.7},
	"9S":  {"DR Congo", -4.3, 23.7},
	"TN":  {"Republic of Congo", -0.2, 15.8},
	"TR":  {"Gabon", -0.7, 11.8},
	"TT":  {"Chad", 15.5, 18.7},
	"TJ":  {"Cameroon", 4.1, 12.4},
	"TL":  {"Central African Rep.", 6.6, 20.9},
	"5V":  {"Togo", 8.7, 1.2},
	"TY":  {"Benin", 9.3, 2.3},
	"5T":  {"Mauritania", 18.1, -15.9},
	"6W":  {"Senegal", 14.7, -17.4},
	"6V":  {"Senegal", 14.7, -17.4},
	"TU":  {"Ivory Coast", 5.4, -4.0},
	"9G":  {"Ghana", 7.9, -1.0},
	"5U":  {"Niger", 17.6, 8.1},
	"XT":  {"Burkina Faso", 12.4, -1.6},
	"6X":  {"Madagascar", -18.9, 47.5},
	"3X":  {"Guinea", 11.0, -13.7},
	"J5":  {"Guinea-Bissau", 11.9, -15.2},
	"EL":  {"Liberia", 6.4, -9.4},
	"9L":  {"Sierra Leone", 8.5, -11.8},
	"D4":  {"Cape Verde", 16.0, -24.0},
	"ST":  {"Sudan", 15.6, 32.5},
	"SS":  {"Sudan", 15.6, 32.5},
	"6I":  {"Sudan", 15.6, 32.5},
	"ST2": {"South Sudan", 6.9, 31.6},
	"SU":  {"Egypt", 27.0, 30.9},
	"3V":  {"Tunisia", 33.9, 9.6},
	"7X":  {"Algeria", 28.0, 3.0},
	"CN":  {"Morocco", 31.8, -7.1},
	"D2":  {"Angola", -11.2, 17.9},
	"D3":  {"Angola", -11.2, 17.9},
	"ZD7": {"St. Helena", -15.9, -5.7},
	"ZD8": {"Ascension Island", -7.9, -14.4},
	"ZD9": {"Tristan da Cunha", -37.1, -12.3},
	"7P":  {"Lesotho", -29.6, 28.2},
	"3DA": {"Eswatini", -26.5, 31.5},
	"C9":  {"Mozambique", -18.3, 35.0},
	"C8":  {"Mozambique", -18.3, 35.0},
	"V5":  {"Namibia", -22.6, 17.1},
	"A2":  {"Botswana", -22.3, 24.7},
	"T5":  {"Somalia", 5.1, 46.2},
	"5A":  {"Libya", 26.3, 17.2},
}
