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

// MeshCore contact-type icons. Used by the Contacts sidebar in
// MeshCore mode to flag Repeater / Room / Sensor entries with a
// glyph instead of single-letter markers. Chat-type contacts re-use
// Fyne's theme.AccountIcon (a person silhouette) so we don't
// duplicate a generic icon here.

//go:embed assets/repeater.svg
var meshRepeaterSVG []byte

// MeshRepeaterResource — stylised radio tower with a signal arc.
var MeshRepeaterResource = &fyne.StaticResource{
	StaticName:    "repeater.svg",
	StaticContent: meshRepeaterSVG,
}

//go:embed assets/room.svg
var meshRoomSVG []byte

// MeshRoomResource — overlapping people silhouettes.
var MeshRoomResource = &fyne.StaticResource{
	StaticName:    "room.svg",
	StaticContent: meshRoomSVG,
}

//go:embed assets/sensor.svg
var meshSensorSVG []byte

// MeshSensorResource — gauge dial with a needle.
var MeshSensorResource = &fyne.StaticResource{
	StaticName:    "sensor.svg",
	StaticContent: meshSensorSVG,
}

//go:embed assets/hash.svg
var meshHashSVG []byte

// MeshHashResource — Discord-style "#" glyph used to flag MeshCore
// channels whose name already starts with "#" (the operator-named
// public/group channels). Channels without a "#" prefix in their
// name fall back to the repeater icon.
var MeshHashResource = &fyne.StaticResource{
	StaticName:    "hash.svg",
	StaticContent: meshHashSVG,
}

// Star icons for the contact-row favorite toggle. Two variants
// because canvas.Image doesn't tint SVG resources at render time —
// we swap the underlying resource based on favourite state. Outline
// (gray) for non-favourite, solid (warm yellow) for favourite.

//go:embed assets/star.svg
var starSVG []byte

//go:embed assets/star_filled.svg
var starFilledSVG []byte

// StarResource — gray five-point star, drawn for non-favourite
// contacts. Tap toggles to StarFilledResource.
var StarResource = &fyne.StaticResource{
	StaticName:    "star.svg",
	StaticContent: starSVG,
}

// StarFilledResource — warm-yellow five-point star, drawn for
// favourited contacts. Tap toggles back to StarResource.
var StarFilledResource = &fyne.StaticResource{
	StaticName:    "star_filled.svg",
	StaticContent: starFilledSVG,
}
