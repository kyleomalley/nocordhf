package mapview

// SVG-based status badges (CQ + *OTA programmes). Each icon is a
// self-contained SVG embedded at build time; rasterisation happens
// once per (icon, size) and is cached so repeated map redraws don't
// pay the SVG-parsing cost.
//
// Same data powers two surfaces:
//   - HEARD list: BadgeImage(type) returns a 48×48 RGBA wrapped in a
//     Fyne canvas.Image (downscaled with ImageScalePixels for sharp
//     edges at the smaller roster slot size).
//   - Map: drawBadgeIcon paints into the map image at the requested
//     centre, using the rasterised RGBA at the desired pixel size.
//
// Icons are sourced from Lucide (lucide.dev, ISC license) plus a
// hand-tweaked CQ glyph. Each SVG includes its own coloured backdrop
// disc so callers don't need to draw one separately.

import (
	"bytes"
	_ "embed"
	"image"
	"image/draw"
	"sync"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

//go:embed assets/cq.svg
var cqSVG []byte

//go:embed assets/pota.svg
var potaSVG []byte

//go:embed assets/sota.svg
var sotaSVG []byte

//go:embed assets/wwff.svg
var wwffSVG []byte

//go:embed assets/iota.svg
var iotaSVG []byte

//go:embed assets/bota.svg
var botaSVG []byte

//go:embed assets/lota.svg
var lotaSVG []byte

//go:embed assets/nota.svg
var notaSVG []byte

//go:embed assets/portable.svg
var portableSVG []byte

var badgeSVGs = map[string][]byte{
	"CQ":       cqSVG,
	"POTA":     potaSVG,
	"SOTA":     sotaSVG,
	"WWFF":     wwffSVG,
	"IOTA":     iotaSVG,
	"BOTA":     botaSVG,
	"LOTA":     lotaSVG,
	"NOTA":     notaSVG,
	"PORTABLE": portableSVG,
}

// Cache keyed by (otaType, size). Rasterising the same SVG on every
// frame would be wasteful; the map redraws constantly during pan/zoom.
type badgeCacheKey struct {
	otaType string
	size    int
}

var (
	badgeCache   = map[badgeCacheKey]*image.RGBA{}
	badgeCacheMu sync.Mutex
)

// rasterizeBadge returns an RGBA of the named icon at size×size,
// using oksvg + rasterx. Cached per (type, size). Returns nil for
// unknown types.
func rasterizeBadge(otaType string, size int) *image.RGBA {
	if size < 1 {
		size = 1
	}
	key := badgeCacheKey{otaType, size}
	badgeCacheMu.Lock()
	if img, ok := badgeCache[key]; ok {
		badgeCacheMu.Unlock()
		return img
	}
	badgeCacheMu.Unlock()

	svg, ok := badgeSVGs[otaType]
	if !ok {
		return nil
	}
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svg))
	if err != nil {
		return nil
	}
	icon.SetTarget(0, 0, float64(size), float64(size))

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scanner := rasterx.NewScannerGV(size, size, img, img.Bounds())
	raster := rasterx.NewDasher(size, size, scanner)
	icon.Draw(raster, 1.0)

	badgeCacheMu.Lock()
	badgeCache[key] = img
	badgeCacheMu.Unlock()
	return img
}

// BadgeIcons returns the list of *OTA program names this package has
// icons for. Stable order: POTA, SOTA, WWFF, IOTA, BOTA, LOTA, NOTA,
// PORTABLE. (CQ exists too but isn't part of the *OTA family.)
func BadgeIcons() []string {
	return []string{"POTA", "SOTA", "WWFF", "IOTA", "BOTA", "LOTA", "NOTA", "PORTABLE"}
}

// BadgeImage returns a 48×48 RGBA of the named badge for use in Fyne
// canvas.Image widgets (HEARD list). Returns nil for unknown types
// so callers can hide the slot.
func BadgeImage(otaType string) *image.RGBA {
	return rasterizeBadge(otaType, 48)
}

// drawBadgeIcon paints the named badge onto img centred at (cx, cy)
// at the given pixel size. Returns false (no draw) if otaType is
// unknown. The size argument is the badge diameter in destination
// pixels — the SVG is rasterised crisply at that size and copied in.
func drawBadgeIcon(img *image.RGBA, cx, cy int, otaType string, size int) bool {
	src := rasterizeBadge(otaType, size)
	if src == nil {
		return false
	}
	dst := image.Rect(cx-size/2, cy-size/2, cx-size/2+size, cy-size/2+size)
	draw.Draw(img, dst, src, image.Point{}, draw.Over)
	return true
}
