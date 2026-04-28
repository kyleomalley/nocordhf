package ft8

import "testing"

// buildBits packs a series of (value, width) fields MSB-first into a 77-bit
// payload. Widths must sum to exactly 77 — the caller is responsible for
// the full layout including n3/i3 tail bits.
func buildBits(t *testing.T, fields []struct {
	val   uint64
	width int
}) [77]byte {
	t.Helper()
	var bits [77]byte
	pos := 0
	for _, f := range fields {
		for i := f.width - 1; i >= 0; i-- {
			if pos >= 77 {
				t.Fatalf("buildBits: fields exceed 77 bits (pos=%d)", pos)
			}
			bits[pos] = byte((f.val >> uint(i)) & 1)
			pos++
		}
	}
	if pos != 77 {
		t.Fatalf("buildBits: fields sum to %d bits, want 77", pos)
	}
	return bits
}

// TestUnpack77_DXpedition verifies i3=0 n3=1 routing produces
// "K1ABC RR73; W9XYZ <hash> -10" — the shape that previously mis-unpacked
// as Type-1 garbage (e.g. "KI7LPT 1O1 YP R OO73").
func TestUnpack77_DXpedition(t *testing.T) {
	// Clear hash tables so `lookupHash10(0)` returns the unresolved "<0>"
	// placeholder expected by this test. Without this, any prior test that
	// called Decode() (e.g. TestCorpus) may have registered a callsign whose
	// 10-bit hash happens to be 0, which would resolve "<0>" to "<CALL>" and
	// break the assertion.
	callHashMu.Lock()
	hash22Table = map[uint32]string{}
	hash12Table = map[uint16]string{}
	hash10Table = map[uint16]string{}
	callHashMu.Unlock()

	n28a, ok := packN28("K1ABC")
	if !ok {
		t.Fatal("pack K1ABC")
	}
	n28b, ok := packN28("W9XYZ")
	if !ok {
		t.Fatal("pack W9XYZ")
	}
	// irpt = -10  →  n5 = (irpt+30)/2 = 10
	fields := []struct {
		val   uint64
		width int
	}{
		{uint64(n28a), 28},
		{uint64(n28b), 28},
		{0, 10}, // n10 hash (unresolved → "<0>")
		{10, 5}, // n5
		{1, 3},  // n3
		{0, 3},  // i3
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "K1ABC RR73; W9XYZ <0> -10"
	if msg.Text != want {
		t.Errorf("DXpedition: got %q, want %q (Type=%d)", msg.Text, want, msg.Type)
	}
	if msg.Type != 101 {
		t.Errorf("DXpedition: Type = %d, want 101", msg.Type)
	}
}

// TestUnpack77_FieldDay verifies i3=0 n3=3.
func TestUnpack77_FieldDay(t *testing.T) {
	n28a, _ := packN28("WA9XYZ")
	n28b, _ := packN28("KA1ABC")
	// intx=15 (ntx=16), nclass=0 (class='A'), isec=11 (EMA), ir=1
	fields := []struct {
		val   uint64
		width int
	}{
		{uint64(n28a), 28},
		{uint64(n28b), 28},
		{1, 1},  // ir
		{15, 4}, // intx
		{0, 3},  // nclass
		{11, 7}, // isec
		{3, 3},  // n3
		{0, 3},  // i3
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "WA9XYZ KA1ABC R 16A EMA"
	if msg.Text != want {
		t.Errorf("FieldDay: got %q, want %q", msg.Text, want)
	}
	if msg.Type != 103 {
		t.Errorf("FieldDay: Type = %d, want 103", msg.Type)
	}
}

// TestUnpack77_RTTYRU verifies i3=3 ARRL RTTY Roundup.
func TestUnpack77_RTTYRU(t *testing.T) {
	n28a, _ := packN28("K1ABC")
	n28b, _ := packN28("W9XYZ")
	// irpt=5 → crpt="579"; imult=21 (MA); nexch=8000+21=8021; itu=1; ir=1
	fields := []struct {
		val   uint64
		width int
	}{
		{1, 1}, // itu
		{uint64(n28a), 28},
		{uint64(n28b), 28},
		{1, 1},     // ir
		{5, 3},     // irpt (2+5=7 → "579")
		{8021, 13}, // nexch
		{3, 3},     // i3
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "TU; K1ABC W9XYZ R 579 MA"
	if msg.Text != want {
		t.Errorf("RTTY-RU: got %q, want %q", msg.Text, want)
	}
	if msg.Type != 3 {
		t.Errorf("RTTY-RU: Type = %d, want 3", msg.Type)
	}
}

// TestUnpack77_RTTYRU_Serial verifies the numeric-serial branch.
func TestUnpack77_RTTYRU_Serial(t *testing.T) {
	n28a, _ := packN28("K1ABC")
	n28b, _ := packN28("W9XYZ")
	fields := []struct {
		val   uint64
		width int
	}{
		{0, 1}, // itu
		{uint64(n28a), 28},
		{uint64(n28b), 28},
		{0, 1},     // ir
		{5, 3},     // irpt (2+5=7 → "579")
		{1234, 13}, // nexch serial
		{3, 3},     // i3
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "K1ABC W9XYZ 579 1234"
	if msg.Text != want {
		t.Errorf("RTTY-RU serial: got %q, want %q", msg.Text, want)
	}
}

// TestUnpack77_Telemetry verifies i3=0 n3=5.
func TestUnpack77_Telemetry(t *testing.T) {
	// 23+24+24 = 71 bits.  Pack 0x000001, 0x000002, 0x000003 into those fields.
	fields := []struct {
		val   uint64
		width int
	}{
		{1, 23},
		{2, 24},
		{3, 24},
		{5, 3}, // n3
		{0, 3}, // i3
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "1000002000003"
	if msg.Text != want {
		t.Errorf("Telemetry: got %q, want %q", msg.Text, want)
	}
	if msg.Type != 105 {
		t.Errorf("Telemetry: Type = %d, want 105", msg.Type)
	}
}

// TestUnpack77_Standard_Unchanged guards the pre-existing standard path.
func TestUnpack77_Standard_Unchanged(t *testing.T) {
	n28a, _ := packN28("KO6IEH")
	n28b, _ := packN28("W1AW")
	// igrid4 for "DM13": M=12, (M,1,3) — we want just a clean known grid.
	// Use 0 (AA00) for simplicity.
	fields := []struct {
		val   uint64
		width int
	}{
		{uint64(n28a) << 1, 29}, // n29a with ipa=0
		{uint64(n28b) << 1, 29}, // n29b with ipb=0
		{0, 1},                  // ir
		{0, 15},                 // igrid4 = AA00
		{1, 3},                  // i3=1 (in the last 3 bits)
	}
	bits := buildBits(t, fields)
	msg := Unpack77(bits)
	want := "KO6IEH W1AW AA00"
	if msg.Text != want {
		t.Errorf("Standard: got %q, want %q", msg.Text, want)
	}
	if msg.Type != 1 {
		t.Errorf("Standard: Type = %d, want 1", msg.Type)
	}
}
