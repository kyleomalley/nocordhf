package callsign

import "image/color"

// flagBG maps an ISO-3166-style 2-letter country code (the trailing two
// letters of ShortCode) to a single dominant flag colour. Used to render
// a small "flag chip" — a coloured pill containing the country code —
// next to a callsign in the HEARD list. Cheap visual cue without
// shipping 250 SVG assets, and avoids unicode-flag-emoji rendering
// problems in Fyne.
//
// Coverage matches what shortFromName tags as a real ISO trailer; we
// don't bother colouring synthetic codes (HI/AK/NV/...) since those
// fall back to a neutral grey via FlagBG below.
var flagBG = map[string]color.RGBA{
	// North America
	"US": {180, 25, 50, 255},   // red
	"CA": {220, 40, 40, 255},   // red
	"MX": {0, 130, 70, 255},    // green
	"PR": {30, 80, 170, 255},   // blue
	"VI": {255, 255, 255, 255}, // white
	"CU": {30, 80, 170, 255},   // blue
	"BS": {0, 110, 100, 255},   // teal
	"JM": {30, 30, 30, 255},    // black
	"DO": {30, 80, 170, 255},   // blue
	"HT": {30, 80, 170, 255},   // blue

	// South America
	"BR": {0, 130, 70, 255},    // green
	"AR": {130, 180, 230, 255}, // light blue
	"CL": {220, 40, 40, 255},   // red
	"PE": {220, 40, 40, 255},   // red
	"CO": {255, 215, 0, 255},   // yellow
	"VE": {255, 215, 0, 255},   // yellow
	"UY": {130, 180, 230, 255}, // light blue
	"PY": {220, 40, 40, 255},   // red
	"BO": {0, 130, 70, 255},    // green
	"EC": {255, 215, 0, 255},   // yellow

	// Europe
	"GB": {30, 80, 170, 255},  // blue
	"DE": {30, 30, 30, 255},   // black
	"FR": {30, 80, 170, 255},  // blue
	"IT": {0, 130, 70, 255},   // green
	"ES": {220, 40, 40, 255},  // red
	"PT": {0, 130, 70, 255},   // green
	"NL": {220, 40, 40, 255},  // red
	"BE": {30, 30, 30, 255},   // black
	"CH": {220, 40, 40, 255},  // red
	"AT": {220, 40, 40, 255},  // red
	"PL": {220, 40, 40, 255},  // red
	"CZ": {30, 80, 170, 255},  // blue
	"SK": {30, 80, 170, 255},  // blue
	"HU": {0, 130, 70, 255},   // green
	"RO": {30, 80, 170, 255},  // blue
	"BG": {0, 130, 70, 255},   // green
	"GR": {30, 80, 170, 255},  // blue
	"SE": {30, 110, 200, 255}, // blue
	"NO": {200, 30, 60, 255},  // red
	"DK": {220, 40, 40, 255},  // red
	"FI": {30, 110, 200, 255}, // blue
	"IS": {30, 80, 170, 255},  // blue
	"IE": {0, 130, 70, 255},   // green
	"UA": {30, 110, 200, 255}, // blue
	"BY": {0, 130, 70, 255},   // green
	"LT": {255, 215, 0, 255},  // yellow
	"LV": {120, 30, 30, 255},  // dark red
	"EE": {30, 110, 200, 255}, // blue
	"HR": {220, 40, 40, 255},  // red
	"RS": {220, 40, 40, 255},  // red
	"SI": {30, 80, 170, 255},  // blue
	"BA": {30, 80, 170, 255},  // blue
	"MK": {220, 40, 40, 255},  // red
	"AL": {220, 40, 40, 255},  // red
	"MT": {220, 40, 40, 255},  // red
	"LU": {220, 40, 40, 255},  // red
	"MD": {255, 215, 0, 255},  // yellow

	// Asia
	"RU": {220, 40, 40, 255},   // red
	"CN": {220, 40, 40, 255},   // red
	"JP": {220, 40, 40, 255},   // red disc
	"KR": {220, 40, 40, 255},   // red
	"KP": {220, 40, 40, 255},   // red
	"TW": {220, 40, 40, 255},   // red
	"HK": {220, 40, 40, 255},   // red
	"VN": {220, 40, 40, 255},   // red
	"TH": {220, 40, 40, 255},   // red
	"PH": {30, 80, 170, 255},   // blue
	"ID": {220, 40, 40, 255},   // red
	"MY": {220, 40, 40, 255},   // red
	"SG": {220, 40, 40, 255},   // red
	"IN": {255, 130, 30, 255},  // saffron
	"PK": {0, 130, 70, 255},    // green
	"BD": {0, 130, 70, 255},    // green
	"LK": {200, 130, 50, 255},  // orange-yellow
	"NP": {220, 40, 40, 255},   // red
	"IR": {0, 130, 70, 255},    // green
	"IQ": {220, 40, 40, 255},   // red
	"SA": {0, 130, 70, 255},    // green
	"AE": {220, 40, 40, 255},   // red
	"IL": {30, 80, 170, 255},   // blue
	"TR": {220, 40, 40, 255},   // red
	"KZ": {130, 180, 230, 255}, // light blue

	// Africa
	"ZA": {0, 130, 70, 255},  // green
	"EG": {220, 40, 40, 255}, // red
	"MA": {220, 40, 40, 255}, // red
	"NG": {0, 130, 70, 255},  // green
	"KE": {220, 40, 40, 255}, // red
	"ET": {0, 130, 70, 255},  // green
	"GH": {220, 40, 40, 255}, // red
	"DZ": {0, 130, 70, 255},  // green
	"TN": {220, 40, 40, 255}, // red
	"LY": {0, 130, 70, 255},  // green

	// Oceania
	"AU": {30, 80, 170, 255}, // blue
	"NZ": {30, 80, 170, 255}, // blue
	"FJ": {130, 180, 230, 255},
}

// FlagBG returns the background colour for a country-code chip plus an
// "ok" flag indicating whether we have a curated colour for the code.
// Unknown codes get a neutral grey so they still render as recognisable
// chips in the HEARD list.
func FlagBG(call string) (bg color.RGBA, fg color.RGBA, ok bool) {
	code := ShortCode(call)
	if len(code) < 2 {
		return color.RGBA{60, 65, 75, 255}, color.RGBA{200, 205, 215, 255}, false
	}
	tail := code[len(code)-2:]
	if c, found := flagBG[tail]; found {
		return c, contrastText(c), true
	}
	return color.RGBA{60, 65, 75, 255}, color.RGBA{200, 205, 215, 255}, false
}

// contrastText picks black or white text for the given background based
// on perceived luminance — keeps the country code legible whether the
// flag colour is light or dark.
func contrastText(bg color.RGBA) color.RGBA {
	// ITU-R BT.601 luma.
	y := 0.299*float64(bg.R) + 0.587*float64(bg.G) + 0.114*float64(bg.B)
	if y > 140 {
		return color.RGBA{20, 20, 25, 255}
	}
	return color.RGBA{240, 245, 250, 255}
}
