package callsign

import "strings"

// shortFromName maps the verbose Entity.Name strings used by the prefix table
// to the compact "Continent-Country" labels we display in the decode list.
// Example: "United States" → "NA-US", "Germany" → "EU-DE", "Hawaii" → "OC-HI".
//
// Coverage is intentionally curated to the entities that actually show up in
// US-coast traffic plus the major DX. Entities not in this map fall back to
// continent-derived-from-lat/lon plus the first 2 letters of the entity name
// uppercased — never breaks, just less polished.
var shortFromName = map[string]string{
	// North America
	"United States":      "NA-US",
	"Hawaii":             "OC-HI",
	"Alaska":             "NA-AK",
	"Canada":             "NA-CA",
	"Mexico":             "NA-MX",
	"Puerto Rico":        "NA-PR",
	"US Virgin Islands":  "NA-VI",
	"American Samoa":     "OC-AS",
	"Guam":               "OC-GU",
	"N. Mariana Islands": "OC-MP",
	"Midway Islands":     "OC-MI",
	"Wake Island":        "OC-WK",
	"Johnston Atoll":     "OC-JA",
	"Navassa Island":     "NA-NV",
	"Desecheo Island":    "NA-DI",

	// Caribbean / Central America
	"Costa Rica":         "NA-CR",
	"Guatemala":          "NA-GT",
	"Honduras":           "NA-HN",
	"Nicaragua":          "NA-NI",
	"Panama":             "NA-PA",
	"El Salvador":        "NA-SV",
	"Haiti":              "NA-HT",
	"Dominican Republic": "NA-DO",
	"Grenada":            "NA-GD",
	"St. Lucia":          "NA-LC",
	"Dominica":           "NA-DM",
	"St. Vincent":        "NA-VC",
	"Antigua & Barbuda":  "NA-AG",
	"St. Kitts & Nevis":  "NA-KN",
	"Barbados":           "NA-BB",
	"Jamaica":            "NA-JM",
	"Bahamas":            "NA-BS",
	"Cayman Islands":     "NA-KY",
	"Turks & Caicos":     "NA-TC",
	"Bermuda":            "NA-BM",
	"Curacao":            "NA-CW",
	"Bonaire":            "NA-BQ",
	"Sint Maarten":       "NA-SX",
	"Martinique":         "NA-MQ",
	"Guadeloupe":         "NA-GP",
	"St. Barthelemy":     "NA-BL",

	// South America
	"Brazil":        "SA-BR",
	"Argentina":     "SA-AR",
	"Chile":         "SA-CL",
	"Uruguay":       "SA-UY",
	"Peru":          "SA-PE",
	"Ecuador":       "SA-EC",
	"Colombia":      "SA-CO",
	"Venezuela":     "SA-VE",
	"Bolivia":       "SA-BO",
	"Paraguay":      "SA-PY",
	"French Guiana": "SA-GF",
	"Suriname":      "SA-SR",
	"Guyana":        "SA-GY",

	// Europe
	"England":        "EU-GB",
	"Scotland":       "EU-GB",
	"Wales":          "EU-GB",
	"N. Ireland":     "EU-GB",
	"Jersey":         "EU-JE",
	"Guernsey":       "EU-GG",
	"Isle of Man":    "EU-IM",
	"Ireland":        "EU-IE",
	"Germany":        "EU-DE",
	"France":         "EU-FR",
	"Italy":          "EU-IT",
	"Spain":          "EU-ES",
	"Portugal":       "EU-PT",
	"Netherlands":    "EU-NL",
	"Belgium":        "EU-BE",
	"Switzerland":    "EU-CH",
	"Austria":        "EU-AT",
	"Poland":         "EU-PL",
	"Czech Republic": "EU-CZ",
	"Slovakia":       "EU-SK",
	"Hungary":        "EU-HU",
	"Romania":        "EU-RO",
	"Bulgaria":       "EU-BG",
	"Greece":         "EU-GR",
	"Sweden":         "EU-SE",
	"Norway":         "EU-NO",
	"Denmark":        "EU-DK",
	"Finland":        "EU-FI",
	"Iceland":        "EU-IS",
	"Russia":         "EU-RU",
	"Asiatic Russia": "AS-RU",
	"Ukraine":        "EU-UA",
	"Belarus":        "EU-BY",
	"Latvia":         "EU-LV",
	"Lithuania":      "EU-LT",
	"Estonia":        "EU-EE",
	"Croatia":        "EU-HR",
	"Slovenia":       "EU-SI",
	"Serbia":         "EU-RS",
	"Bosnia":         "EU-BA",
	"Albania":        "EU-AL",
	"N. Macedonia":   "EU-MK",
	"Montenegro":     "EU-ME",
	"Moldova":        "EU-MD",
	"Cyprus":         "EU-CY",
	"Malta":          "EU-MT",
	"Luxembourg":     "EU-LU",
	"Liechtenstein":  "EU-LI",
	"Andorra":        "EU-AD",
	"Monaco":         "EU-MC",
	"San Marino":     "EU-SM",
	"Vatican":        "EU-VA",
	"Faroe Islands":  "EU-FO",
	"Azores":         "EU-AZ",
	"Madeira":        "EU-MD",
	"Canary Islands": "EU-IC",

	// Asia
	"Japan":        "AS-JP",
	"China":        "AS-CN",
	"Korea":        "AS-KR",
	"S. Korea":     "AS-KR",
	"N. Korea":     "AS-KP",
	"Taiwan":       "AS-TW",
	"Hong Kong":    "AS-HK",
	"India":        "AS-IN",
	"Pakistan":     "AS-PK",
	"Thailand":     "AS-TH",
	"Indonesia":    "AS-ID",
	"Philippines":  "AS-PH",
	"Vietnam":      "AS-VN",
	"Malaysia":     "AS-MY",
	"Singapore":    "AS-SG",
	"Sri Lanka":    "AS-LK",
	"Bangladesh":   "AS-BD",
	"Iran":         "AS-IR",
	"Iraq":         "AS-IQ",
	"Israel":       "AS-IL",
	"Saudi Arabia": "AS-SA",
	"UAE":          "AS-AE",
	"Kuwait":       "AS-KW",
	"Bahrain":      "AS-BH",
	"Qatar":        "AS-QA",
	"Oman":         "AS-OM",
	"Yemen":        "AS-YE",
	"Jordan":       "AS-JO",
	"Lebanon":      "AS-LB",
	"Syria":        "AS-SY",
	"Turkey":       "AS-TR",
	"Cambodia":     "AS-KH",
	"Mongolia":     "AS-MN",
	"Kazakhstan":   "AS-KZ",
	"Uzbekistan":   "AS-UZ",
	"Kyrgyzstan":   "AS-KG",

	// Oceania
	"Australia":        "OC-AU",
	"New Zealand":      "OC-NZ",
	"Papua New Guinea": "OC-PG",
	"Fiji":             "OC-FJ",
	"Samoa":            "OC-WS",
	"Tonga":            "OC-TO",
	"Vanuatu":          "OC-VU",
	"New Caledonia":    "OC-NC",
	"French Polynesia": "OC-PF",

	// Africa
	"South Africa": "AF-ZA",
	"Egypt":        "AF-EG",
	"Morocco":      "AF-MA",
	"Algeria":      "AF-DZ",
	"Tunisia":      "AF-TN",
	"Libya":        "AF-LY",
	"Sudan":        "AF-SD",
	"Ethiopia":     "AF-ET",
	"Kenya":        "AF-KE",
	"Tanzania":     "AF-TZ",
	"Uganda":       "AF-UG",
	"Nigeria":      "AF-NG",
	"Ghana":        "AF-GH",
	"Senegal":      "AF-SN",
	"Madagascar":   "AF-MG",
	"Mauritius":    "AF-MU",
	"Cape Verde":   "AF-CV",
	"Reunion":      "AF-RE",
	"Angola":       "AF-AO",
	"Zimbabwe":     "AF-ZW",
	"Turkmenistan": "AS-TM",
	"Tajikistan":   "AS-TJ",
}

// continentFromLatLon returns the 2-letter continent code for the entity's
// approximate centroid. Used as a fallback when the entity isn't in the
// curated shortFromName map. Bounds are deliberately loose — better to
// label something "AS" than to fall through to a no-label state.
func continentFromLatLon(lat, lon float64) string {
	switch {
	case lat >= 14 && lon >= -170 && lon <= -50:
		return "NA"
	case lat < 14 && lon >= -82 && lon <= -34:
		return "SA"
	case lat >= 36 && lon >= -10 && lon <= 60:
		return "EU"
	case lat >= -10 && lon >= 60 && lon <= 180:
		return "AS"
	case lat < 38 && lon >= -18 && lon <= 52:
		return "AF"
	case lat <= 10 && (lon >= 110 || lon <= -130):
		return "OC"
	}
	return ""
}

// ShortCode returns a compact "Continent-Country" label for the call's home
// entity, suitable for display in the decode list. Returns "" if no entity
// matches (caller can omit the column entirely).
//
//	ShortCode("KB9ELS") = "NA-US"
//	ShortCode("DL1ABC") = "EU-DE"
//	ShortCode("XE2SSB") = "NA-MX"
//	ShortCode("NH6D")   = "OC-HI"
//	ShortCode("UNKWN9") = ""
func ShortCode(call string) string {
	ent, ok := Lookup(call)
	if !ok {
		return ""
	}
	if code, ok := shortFromName[ent.Name]; ok {
		return code
	}
	cont := continentFromLatLon(ent.Lat, ent.Lon)
	// Fall back to continent + first two letters of the name. Beats
	// nothing — at least the user sees a region indicator.
	short := ent.Name
	if len(short) >= 2 {
		short = strings.ToUpper(short[:2])
	}
	if cont == "" {
		return short
	}
	return cont + "-" + short
}

// nonISOTrailers are trailing 2-letter codes used by shortFromName that
// are NOT real ISO-3166 country codes — building a regional-indicator
// flag from them would render an unrelated/blank glyph. Flag returns
// "" for these.
var nonISOTrailers = map[string]bool{
	"HI": true, "AK": true,
	"NV": true, "DI": true,
	"MI": true, "WK": true, "JA": true,
}

// Flag returns the unicode regional-indicator flag for a callsign's
// home entity, or "" when the entity has no real ISO-3166 code. Flag
// emoji are an explicit, narrow exception to the project's no-emoji
// rule — they're used in the HEARD list only.
//
// The trailing 2-letter code MUST be a real ISO-3166-1 alpha-2 entry,
// otherwise the OS renders the regional-indicator pair as two boxed
// letters (e.g. "AN" for Angola, which isn't a valid ISO code). We
// gate on iso3166Alpha2 below — anything not in that set falls back
// to the caller's text-only path.
func Flag(call string) string {
	code := ShortCode(call)
	if len(code) < 2 {
		return ""
	}
	tail := code[len(code)-2:]
	if nonISOTrailers[tail] {
		return ""
	}
	if !iso3166Alpha2[tail] {
		return ""
	}
	r1 := rune(0x1F1E6) + rune(tail[0]-'A')
	r2 := rune(0x1F1E6) + rune(tail[1]-'A')
	return string([]rune{r1, r2})
}

// iso3166Alpha2 is the set of currently-assigned ISO-3166-1 alpha-2
// country codes. Used to gate Flag() so we don't emit unpaired
// regional-indicator letters for synthetic codes (e.g. our fallback
// path that would otherwise produce "AN" for Angola). Any code not
// in this set returns "" from Flag, letting the caller's text-only
// fallback (typically the ShortCode trailer) take over.
var iso3166Alpha2 = map[string]bool{
	"AD": true, "AE": true, "AF": true, "AG": true, "AI": true, "AL": true, "AM": true, "AO": true,
	"AQ": true, "AR": true, "AS": true, "AT": true, "AU": true, "AW": true, "AX": true, "AZ": true,
	"BA": true, "BB": true, "BD": true, "BE": true, "BF": true, "BG": true, "BH": true, "BI": true,
	"BJ": true, "BL": true, "BM": true, "BN": true, "BO": true, "BQ": true, "BR": true, "BS": true,
	"BT": true, "BV": true, "BW": true, "BY": true, "BZ": true,
	"CA": true, "CC": true, "CD": true, "CF": true, "CG": true, "CH": true, "CI": true, "CK": true,
	"CL": true, "CM": true, "CN": true, "CO": true, "CR": true, "CU": true, "CV": true, "CW": true,
	"CX": true, "CY": true, "CZ": true,
	"DE": true, "DJ": true, "DK": true, "DM": true, "DO": true, "DZ": true,
	"EC": true, "EE": true, "EG": true, "EH": true, "ER": true, "ES": true, "ET": true,
	"FI": true, "FJ": true, "FK": true, "FM": true, "FO": true, "FR": true,
	"GA": true, "GB": true, "GD": true, "GE": true, "GF": true, "GG": true, "GH": true, "GI": true,
	"GL": true, "GM": true, "GN": true, "GP": true, "GQ": true, "GR": true, "GS": true, "GT": true,
	"GU": true, "GW": true, "GY": true,
	"HK": true, "HM": true, "HN": true, "HR": true, "HT": true, "HU": true,
	"ID": true, "IE": true, "IL": true, "IM": true, "IN": true, "IO": true, "IQ": true, "IR": true,
	"IS": true, "IT": true,
	"JE": true, "JM": true, "JO": true, "JP": true,
	"KE": true, "KG": true, "KH": true, "KI": true, "KM": true, "KN": true, "KP": true, "KR": true,
	"KW": true, "KY": true, "KZ": true,
	"LA": true, "LB": true, "LC": true, "LI": true, "LK": true, "LR": true, "LS": true, "LT": true,
	"LU": true, "LV": true, "LY": true,
	"MA": true, "MC": true, "MD": true, "ME": true, "MF": true, "MG": true, "MH": true, "MK": true,
	"ML": true, "MM": true, "MN": true, "MO": true, "MP": true, "MQ": true, "MR": true, "MS": true,
	"MT": true, "MU": true, "MV": true, "MW": true, "MX": true, "MY": true, "MZ": true,
	"NA": true, "NC": true, "NE": true, "NF": true, "NG": true, "NI": true, "NL": true, "NO": true,
	"NP": true, "NR": true, "NU": true, "NZ": true,
	"OM": true,
	"PA": true, "PE": true, "PF": true, "PG": true, "PH": true, "PK": true, "PL": true, "PM": true,
	"PN": true, "PR": true, "PS": true, "PT": true, "PW": true, "PY": true,
	"QA": true,
	"RE": true, "RO": true, "RS": true, "RU": true, "RW": true,
	"SA": true, "SB": true, "SC": true, "SD": true, "SE": true, "SG": true, "SH": true, "SI": true,
	"SJ": true, "SK": true, "SL": true, "SM": true, "SN": true, "SO": true, "SR": true, "SS": true,
	"ST": true, "SV": true, "SX": true, "SY": true, "SZ": true,
	"TC": true, "TD": true, "TF": true, "TG": true, "TH": true, "TJ": true, "TK": true, "TL": true,
	"TM": true, "TN": true, "TO": true, "TR": true, "TT": true, "TV": true, "TW": true, "TZ": true,
	"UA": true, "UG": true, "UM": true, "US": true, "UY": true, "UZ": true,
	"VA": true, "VC": true, "VE": true, "VG": true, "VI": true, "VN": true, "VU": true,
	"WF": true, "WS": true,
	"XK": true, // Kosovo (commonly used as user-assigned)
	"YE": true, "YT": true,
	"ZA": true, "ZM": true, "ZW": true,
}

// CountryName returns the human-readable country / DXCC entity name
// for a callsign, or "" if the prefix isn't recognised. Used by the
// HEARD list to populate a hover tooltip.
func CountryName(call string) string {
	if ent, ok := Lookup(call); ok {
		return ent.Name
	}
	return ""
}
