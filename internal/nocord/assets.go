package nocord

import (
	_ "embed"

	"fyne.io/fyne/v2"
)

// cqFlowerSVG is the small ICQ-style flower icon used to mark stations in
// the HEARD sidebar that have CQ'd within the last two FT8 slots. Embedded
// rather than read from disk so a `go run` from anywhere works.
//
//go:embed assets/cq_flower.svg
var cqFlowerSVG []byte

// CQFlowerResource is a Fyne static resource wrapping the embedded SVG; the
// HEARD list passes it to canvas.NewImageFromResource so the icon scales
// crisply to whatever pixel size the row layout asks for.
var CQFlowerResource = &fyne.StaticResource{
	StaticName:    "cq_flower.svg",
	StaticContent: cqFlowerSVG,
}
