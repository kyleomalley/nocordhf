package mapview

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"

	"fyne.io/fyne/v2"

	"github.com/kyleomalley/nocordhf/lib/pskreporter"
)

// pskrTierColor returns the RGBA dot color for an activity tier. Gray for a
// quiet band, climbing through amber → green → bright green as activity rises.
func pskrTierColor(t pskreporter.Tier) color.RGBA {
	switch t {
	case pskreporter.TierLow:
		return color.RGBA{0xd0, 0xa8, 0x38, 0xff} // amber
	case pskreporter.TierMedium:
		return color.RGBA{0x4a, 0x9d, 0x52, 0xff} // green
	case pskreporter.TierHigh:
		return color.RGBA{0x3a, 0xe0, 0x6a, 0xff} // bright green
	default:
		return color.RGBA{0x55, 0x55, 0x55, 0xff} // gray (quiet)
	}
}

// pskrIconCache memoises the generated tab-icon PNGs per tier so we don't
// re-encode on every tab-label refresh.
var (
	pskrIconMu    sync.Mutex
	pskrIconCache = map[pskreporter.Tier]fyne.Resource{}
)

// pskrTierIcon returns a small PNG fyne.Resource of a filled circle in the
// tier's color. Fyne renders PNG tab icons at their own colors (unlike SVGs,
// which get theme-tinted) so the palette survives into the tab bar.
func pskrTierIcon(t pskreporter.Tier) fyne.Resource {
	pskrIconMu.Lock()
	defer pskrIconMu.Unlock()
	if r, ok := pskrIconCache[t]; ok {
		return r
	}
	const size = 24
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	c := pskrTierColor(t)
	cx, cy := float64(size)/2, float64(size)/2
	r := float64(size)/2 - 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := dx*dx + dy*dy
			switch {
			case d <= r*r:
				img.SetRGBA(x, y, c)
			case d <= (r+1)*(r+1):
				// 1-pixel soft edge for a less jagged dot.
				a := uint8(float64(c.A) * (1 - (d-r*r)/(2*r)))
				img.SetRGBA(x, y, color.RGBA{c.R, c.G, c.B, a})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	res := fyne.NewStaticResource(fmt.Sprintf("pskr_tier_%d.png", t), buf.Bytes())
	pskrIconCache[t] = res
	return res
}
