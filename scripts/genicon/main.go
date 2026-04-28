package main

// Generate the NocordHF app icon: rounded green "CQ" pill on a fully
// transparent background. Mirrors internal/nocord/assets/cq_flower.svg
// so the in-app marker and the dock/launcher icon match. Pure stdlib
// (image + image/png) — no external SVG renderer required at build
// time.
//
// Usage:  go run genicon2.go path/to/icon.png

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const size = 1024

func main() {
	if len(os.Args) < 2 {
		os.Stderr.WriteString("usage: genicon2 <out.png>\n")
		os.Exit(2)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	pillW, pillH := 798, 512
	pillX := (size - pillW) / 2
	pillY := (size - pillH) / 2
	radius := pillH / 2

	fillColor := color.RGBA{0x1b, 0xb2, 0x4a, 0xff}
	strokeColor := color.RGBA{0x0e, 0x6b, 0x2c, 0xff}
	strokeW := 40

	// Filled rounded rectangle (pill).
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if insideRoundedRect(x, y, pillX, pillY, pillW, pillH, radius) {
				img.SetRGBA(x, y, fillColor)
			}
		}
	}
	// Stroke ring around the pill.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := signedRoundedRectDist(x, y, pillX, pillY, pillW, pillH, radius)
			if d > 0 && d < float64(strokeW) {
				img.SetRGBA(x, y, strokeColor)
			}
		}
	}

	// "CQ" centred — gomonobold packaged with Go's stdlib so we don't
	// need to ship a font file in the repo.
	tt, err := opentype.Parse(gomonobold.TTF)
	if err != nil {
		panic(err)
	}
	face, err := opentype.NewFace(tt, &opentype.FaceOptions{
		Size: float64(size) * 0.32,
		DPI:  72,
	})
	if err != nil {
		panic(err)
	}
	defer face.Close()
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{0xff, 0xff, 0xff, 0xff}),
		Face: face,
	}
	str := "CQ"
	w := d.MeasureString(str)
	metrics := face.Metrics()
	textY := (size + (metrics.Ascent - metrics.Descent).Round()) / 2
	d.Dot = fixed.Point26_6{
		X: fixed.I(size/2) - w/2,
		Y: fixed.I(textY),
	}
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

// insideRoundedRect reports whether (x,y) is inside the rounded
// rectangle whose top-left corner is (rx,ry) with width rw and height
// rh and corner radius r.
func insideRoundedRect(x, y, rx, ry, rw, rh, r int) bool {
	return signedRoundedRectDist(x, y, rx, ry, rw, rh, r) <= 0
}

// signedRoundedRectDist returns the signed distance from (x,y) to the
// edge of the rounded rectangle. Negative inside, positive outside.
func signedRoundedRectDist(x, y, rx, ry, rw, rh, r int) float64 {
	cx := float64(x) - float64(rx) - float64(rw)/2
	cy := float64(y) - float64(ry) - float64(rh)/2
	dx := math.Abs(cx) - float64(rw)/2 + float64(r)
	dy := math.Abs(cy) - float64(rh)/2 + float64(r)
	if dx < 0 {
		dx = 0
	}
	if dy < 0 {
		dy = 0
	}
	outside := math.Sqrt(dx*dx+dy*dy) - float64(r)
	insideX := math.Min(math.Abs(cx)-float64(rw)/2+float64(r), 0)
	insideY := math.Min(math.Abs(cy)-float64(rh)/2+float64(r), 0)
	insideMax := math.Max(insideX, insideY)
	if insideMax < 0 {
		return insideMax
	}
	return outside
}
