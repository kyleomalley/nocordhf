package mapview

// font.go — minimal 5×7 pixel bitmap font used to stamp text
// directly into the waterfall bitmap (timestamps, callsigns, labels).

// glyphs maps ASCII characters to a 5-column × 7-row bitmap.
// Each uint8 in the inner array is one row; bits 4..0 are the 5 columns (MSB left).
var glyphs = map[byte][7]uint8{
	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00110, 0b01000, 0b10000, 0b11111},
	'3': {0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},
	':': {0b00000, 0b00100, 0b00100, 0b00000, 0b00100, 0b00100, 0b00000},
	' ': {0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b00000},
	'/': {0b00001, 0b00010, 0b00010, 0b00100, 0b01000, 0b01000, 0b10000},
	'-': {0b00000, 0b00000, 0b00000, 0b11111, 0b00000, 0b00000, 0b00000},
	'+': {0b00000, 0b00100, 0b00100, 0b11111, 0b00100, 0b00100, 0b00000},
	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
	'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01110},
	'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'I': {0b01110, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'J': {0b00111, 0b00010, 0b00010, 0b00010, 0b10010, 0b10010, 0b01100},
	'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
	'L': {0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b11111},
	'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
	'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
	'O': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
	'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
	'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
	'S': {0b01110, 0b10001, 0b10000, 0b01110, 0b00001, 0b10001, 0b01110},
	'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
	'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'V': {0b10001, 0b10001, 0b10001, 0b10001, 0b01010, 0b01010, 0b00100},
	'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b10101, 0b01010},
	'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
	'Y': {0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100, 0b00100},
	'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},
}

const (
	glyphW     = 5 // pixels wide per character (source bitmap)
	glyphH     = 7 // pixels tall per character (source bitmap)
	glyphGap   = 1 // pixel gap between characters (source units)
	glyphScale = 2 // render each pixel as glyphScale×glyphScale block
)

// stampTextVertical draws text rotated 90° CW so it reads top-to-bottom
// (like text on the spine of a book).
//
// Rotation rule (90° CW): a pixel at glyph (row, col) maps to screen:
//
//	screen_x = x + col * scale
//	screen_y = y + (glyphH-1-row) * scale
//
// Characters are stacked downward; each advances y by (glyphH+glyphGap)*scale.
// Each rendered character is glyphW*scale wide and glyphH*scale tall.
// Pass scale=0 to use the default glyphScale constant.
func stampTextVertical(pix []byte, stride, x, y int, text string, fg [3]byte, scale int) {
	if scale <= 0 {
		scale = glyphScale
	}
	cy := y
	for i := 0; i < len(text); i++ {
		g, ok := glyphs[text[i]]
		if !ok {
			cy += (glyphH + glyphGap) * scale
			continue
		}
		for row := 0; row < glyphH; row++ {
			for col := 0; col < glyphW; col++ {
				if g[row]>>(4-col)&1 == 1 {
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							px := x + col*scale + dx
							py := cy + row*scale + dy
							off := py*stride + px*4
							if off >= 0 && off+3 < len(pix) {
								pix[off+0] = fg[0]
								pix[off+1] = fg[1]
								pix[off+2] = fg[2]
								pix[off+3] = 255
							}
						}
					}
				}
			}
		}
		cy += (glyphH + glyphGap) * scale
	}
}

// stampText draws text into pix at the default glyphScale.
func stampText(pix []byte, stride, x, y int, text string, fg [3]byte) {
	stampTextScaled(pix, stride, x, y, text, fg, glyphScale)
}

// stampTextScaled draws text into pix (an RGBA flat buffer with the given stride)
// starting at pixel (x, y). Lit pixels use fg; background is left unchanged.
// Each source pixel is rendered as scale×scale block.
func stampTextScaled(pix []byte, stride, x, y int, text string, fg [3]byte, scale int) {
	if scale <= 0 {
		scale = glyphScale
	}
	cx := x
	for i := 0; i < len(text); i++ {
		g, ok := glyphs[text[i]]
		if !ok {
			cx += (glyphW + glyphGap) * scale
			continue
		}
		for row := 0; row < glyphH; row++ {
			for col := 0; col < glyphW; col++ {
				if g[row]>>(4-col)&1 == 1 {
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							px := cx + col*scale + dx
							py := y + row*scale + dy
							off := py*stride + px*4
							if off+3 < len(pix) {
								pix[off+0] = fg[0]
								pix[off+1] = fg[1]
								pix[off+2] = fg[2]
								pix[off+3] = 255
							}
						}
					}
				}
			}
		}
		cx += (glyphW + glyphGap) * scale
	}
}
