package main

// Generate the NocordHF macOS dock icon: a Big-Sur-style squircle
// filled with a stylised FT8 waterfall, with a Netscape-style hero
// "N" planted on a curved horizon arc. The N is the original 90s
// Netscape Navigator silhouette — heavy white sans-serif, shadowed,
// standing on its little planet. The waterfall plays the role of
// Netscape's starfield "sky", which keeps the FT8 subject matter
// front and centre while the silhouette stays readable down to 64px
// dock size.
//
// Pure stdlib + golang.org/x/image; no SVG renderer required.
//
// Usage:  go run ./scripts/genicon path/to/icon.png

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const size = 1024

func main() {
	if len(os.Args) < 2 {
		os.Stderr.WriteString("usage: genicon <out.png>\n")
		os.Exit(2)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Squircle (superellipse) geometry. macOS Big Sur+ icons use a
	// continuous-curvature shape rather than a plain rounded rect; an
	// n=5 superellipse is a close approximation. ~10% margin on each
	// side leaves the standard "icon safe area" bezel that AppKit
	// expects, so the dock won't visually clip the artwork.
	margin := size / 10
	innerSize := size - 2*margin
	cx := float64(size) / 2
	cy := float64(size) / 2
	half := float64(innerSize) / 2
	const superN = 5.0

	// Waterfall background: cool→hot palette, vertical tone tracks
	// baked at deterministic positions so the icon is identical across
	// rebuilds. Time-axis (vertical) gradient gives older "slot lines"
	// up top a darker tint, hotter colours toward the bottom.
	rng := rand.New(rand.NewSource(7))
	const numTones = 6
	tones := make([]int, 0, numTones)
	for i := 0; i < numTones; i++ {
		tones = append(tones, margin+rng.Intn(innerSize))
	}
	const toneRadius = 14

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !insideSquircle(float64(x), float64(y), cx, cy, half, superN) {
				continue
			}
			vy := float64(y-margin) / float64(innerSize)
			if vy < 0 {
				vy = 0
			}
			if vy > 1 {
				vy = 1
			}
			power := 0.20 + 0.55*vy
			power += 0.08 * math.Sin(float64(x)*0.05+float64(y)*0.018)
			for _, tx := range tones {
				dx := x - tx
				if dx < 0 {
					dx = -dx
				}
				if dx < toneRadius {
					power += 0.55 * (1.0 - float64(dx)/float64(toneRadius))
				}
			}
			if power < 0 {
				power = 0
			}
			if power > 1 {
				power = 1
			}
			img.SetRGBA(x, y, waterfallColor(power))
		}
	}

	// Glow ring just inside the squircle edge — a deep navy halo that
	// frames the artwork and matches the in-app waterfall's quiet
	// noise floor. Two pixels wide at this resolution; reads as a
	// subtle bezel at dock size.
	ringInner := half - 4
	ringOuter := half + 1
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := superellipseDist(float64(x), float64(y), cx, cy, half, superN)
			if d > -float64(half-ringInner)/half && d < float64(ringOuter-half)/half {
				img.SetRGBA(x, y, color.RGBA{0x00, 0x10, 0x30, 0xff})
			}
		}
	}

	// Horizon arc — the "tiny planet" the N stands on. Drawn as a
	// slice of a large circle whose centre sits well below the icon,
	// so the visible portion reads as a gentle curve. Filled with a
	// dark navy that contrasts the waterfall and grounds the glyph.
	horizonCx := float64(size) / 2
	horizonCy := float64(size)*1.25 + 0
	horizonR := float64(size) * 0.85
	horizonTop := float64(size) * 0.74 // glyph baseline sits on top of arc
	horizonColor := color.RGBA{0x05, 0x0a, 0x1c, 0xff}
	horizonRim := color.RGBA{0x16, 0x32, 0x6c, 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !insideSquircle(float64(x), float64(y), cx, cy, half, superN) {
				continue
			}
			fy := float64(y)
			if fy < horizonTop {
				continue
			}
			dx := float64(x) - horizonCx
			dy := fy - horizonCy
			d := math.Sqrt(dx*dx + dy*dy)
			if d <= horizonR {
				img.SetRGBA(x, y, horizonColor)
			}
			// Bright rim where the arc top meets the sky — three-pixel
			// glow line, like the atmospheric edge on the Netscape ball.
			if d > horizonR-3 && d <= horizonR {
				img.SetRGBA(x, y, horizonRim)
			}
		}
	}

	// Hero "N" — Netscape-style: heavy white silhouette standing on
	// the horizon arc, with a soft dropped shadow underneath and a
	// subtle italic lean to echo the original wordmark's slant. We
	// draw the shadow first by rendering the same glyph offset in
	// dark navy at low alpha, then the main glyph in pure white.
	tt, err := opentype.Parse(gomonobold.TTF)
	if err != nil {
		panic(err)
	}
	face, err := opentype.NewFace(tt, &opentype.FaceOptions{
		Size: float64(size) * 0.78,
		DPI:  72,
	})
	if err != nil {
		panic(err)
	}
	defer face.Close()

	str := "N"
	metrics := face.Metrics()
	measure := &font.Drawer{Face: face}
	w := measure.MeasureString(str)
	// Plant the baseline just above the horizon arc so the N looks
	// like it's standing on the planet rather than floating.
	baselineY := int(horizonTop) + (metrics.Ascent.Round() / 9)
	baseX := fixed.I(size/2) - w/2

	// Soft drop shadow: several semi-transparent passes nudged down
	// and right, fading out, to feel like the N is lit from above-
	// left. Avoids a hard double-image and reads correctly at small
	// scale.
	shadowPasses := []struct {
		dx, dy int
		alpha  uint8
	}{
		{6, 12, 0xb0},
		{10, 20, 0x80},
		{14, 28, 0x50},
	}
	for _, s := range shadowPasses {
		d := &font.Drawer{
			Dst:  img,
			Src:  image.NewUniform(color.RGBA{0x00, 0x00, 0x00, s.alpha}),
			Face: face,
		}
		d.Dot = fixed.Point26_6{X: baseX + fixed.I(s.dx), Y: fixed.I(baselineY + s.dy)}
		d.DrawString(str)
	}

	// Warm-gold N on top — picked over pure white because the waterfall
	// background is dominated by cool blues/teals with hot red tone
	// tracks; gold sits opposite the cool sky on the colour wheel and
	// stays distinct from the red streaks, where white would get
	// visually swallowed.
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{0xf5, 0xe8, 0xc8, 0xff}),
		Face: face,
	}
	d.Dot = fixed.Point26_6{X: baseX, Y: fixed.I(baselineY)}
	d.DrawString(str)

	out, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer out.Close()
	if err := png.Encode(out, img); err != nil {
		panic(err)
	}
}

// insideSquircle reports whether (x,y) is inside the superellipse
// centred at (cx,cy) with half-extent `half` in both axes and
// exponent n. n=2 gives a circle, n→∞ a square; n≈5 matches the
// macOS Big Sur icon shape closely.
func insideSquircle(x, y, cx, cy, half, n float64) bool {
	return superellipseDist(x, y, cx, cy, half, n) <= 0
}

// superellipseDist returns a signed-ish distance: <0 inside, >0
// outside, in normalised units (i.e. the contour at d=0 is the
// squircle boundary). Not a true Euclidean distance, but monotone
// across the boundary which is all we need for ring rendering.
func superellipseDist(x, y, cx, cy, half, n float64) float64 {
	dx := math.Abs(x-cx) / half
	dy := math.Abs(y-cy) / half
	return math.Pow(dx, n) + math.Pow(dy, n) - 1
}

// waterfallColor maps a normalised power value [0,1] to the standard
// FT8 waterfall palette: deep blue → teal → green → yellow → orange
// → red. Piecewise linear is plenty for a 1024px icon.
func waterfallColor(p float64) color.RGBA {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	stops := []struct {
		t       float64
		r, g, b uint8
	}{
		{0.00, 0x05, 0x10, 0x40},
		{0.20, 0x10, 0x40, 0x80},
		{0.40, 0x10, 0x90, 0x90},
		{0.60, 0x30, 0xc0, 0x40},
		{0.75, 0xe5, 0xd0, 0x20},
		{0.88, 0xff, 0x80, 0x10},
		{1.00, 0xff, 0x20, 0x20},
	}
	for i := 1; i < len(stops); i++ {
		if p <= stops[i].t {
			a, b := stops[i-1], stops[i]
			f := (p - a.t) / (b.t - a.t)
			return color.RGBA{
				R: uint8(float64(a.r) + (float64(b.r)-float64(a.r))*f),
				G: uint8(float64(a.g) + (float64(b.g)-float64(a.g))*f),
				B: uint8(float64(a.b) + (float64(b.b)-float64(a.b))*f),
				A: 0xff,
			}
		}
	}
	last := stops[len(stops)-1]
	return color.RGBA{last.r, last.g, last.b, 0xff}
}
