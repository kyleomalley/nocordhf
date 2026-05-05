package meshcore

import (
	"bytes"
	"io"
	"testing"
)

func TestEncodeFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Marker: FrameOutgoing, Payload: []byte{byte(CmdGetContacts)}},
		{Marker: FrameOutgoing, Payload: []byte{byte(CmdGetChannel), 3}},
		{Marker: FrameIncoming, Payload: []byte{byte(RespOk)}},
		{Marker: FrameIncoming, Payload: append([]byte{byte(RespContact)}, make([]byte, 32+1+1+1+64+32+4+4+4+4)...)},
	}
	for i, want := range cases {
		raw := EncodeFrame(want)
		fr := NewFrameReader(bytes.NewReader(raw))
		got, err := fr.Next()
		if err != nil {
			t.Fatalf("case %d: decode err %v", i, err)
		}
		if got.Marker != want.Marker {
			t.Errorf("case %d: marker %x want %x", i, got.Marker, want.Marker)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("case %d: payload mismatch len(got)=%d len(want)=%d", i, len(got.Payload), len(want.Payload))
		}
	}
}

// TestFrameReaderResync mirrors the upstream JS resync behaviour: a
// bogus prefix byte should be discarded so we lock onto the next valid
// marker rather than permanently desyncing.
func TestFrameReaderResync(t *testing.T) {
	good := EncodeFrame(Frame{Marker: FrameIncoming, Payload: []byte{byte(RespOk)}})
	noisy := append([]byte{0xAA, 0xBB, 0x00, 0x01}, good...)
	fr := NewFrameReader(bytes.NewReader(noisy))
	got, err := fr.Next()
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got.Marker != FrameIncoming || len(got.Payload) != 1 || got.Payload[0] != byte(RespOk) {
		t.Fatalf("unexpected frame: %+v", got)
	}
}

// TestFrameReaderPartial verifies the reader can rejoin a frame that
// arrives in two chunks across the marker/length/payload boundaries —
// USB-CDC frequently splits writes at arbitrary positions.
func TestFrameReaderPartial(t *testing.T) {
	full := EncodeFrame(Frame{Marker: FrameIncoming, Payload: []byte{byte(RespCurrTime), 1, 2, 3, 4}})
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(full[:2])
		_, _ = pw.Write(full[2:5])
		_, _ = pw.Write(full[5:])
		_ = pw.Close()
	}()
	fr := NewFrameReader(pr)
	got, err := fr.Next()
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !bytes.Equal(got.Payload, full[3:]) {
		t.Fatalf("payload mismatch: got %v want %v", got.Payload, full[3:])
	}
}

// TestPacketFromBytesFloodTxt decodes a synthetic FLOOD TXT_MSG with
// a 2-hop path and a 16-byte payload — locks the header bit
// extraction + path-byte sizing against the upstream reference.
func TestPacketFromBytesFloodTxt(t *testing.T) {
	// header = (TXT_MSG << 2) | FLOOD = (0x02<<2) | 0x01 = 0x09
	// pathLen = top 2 bits = 0 (1-byte hashes), bottom 6 bits = 2 hops
	// path = [0xAA, 0xBB]
	// payload = 16 bytes (dest + src + 14 of "data")
	body := make([]byte, 0, 1+1+2+16)
	body = append(body, 0x09)
	body = append(body, 0x02)
	body = append(body, 0xAA, 0xBB)
	for i := 0; i < 16; i++ {
		body = append(body, byte(i))
	}
	pkt, err := PacketFromBytes(body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if pkt.RouteType() != RouteFlood {
		t.Errorf("route %s want FLOOD", pkt.RouteType())
	}
	if pkt.PayloadType() != PayloadTxtMsg {
		t.Errorf("payload %s want TXT_MSG", pkt.PayloadType())
	}
	if pkt.HopCount() != 2 {
		t.Errorf("hops %d want 2", pkt.HopCount())
	}
	if len(pkt.Path) != 2 || pkt.Path[0] != 0xAA || pkt.Path[1] != 0xBB {
		t.Errorf("path %v want [AA BB]", pkt.Path)
	}
	if len(pkt.Payload) != 16 {
		t.Errorf("payload len %d want 16", len(pkt.Payload))
	}
}

// TestPacketFromBytesTransportFlood verifies the two transport-code
// uint16-LE prefix is consumed before the pathLen byte.
func TestPacketFromBytesTransportFlood(t *testing.T) {
	// header = (ADVERT << 2) | TRANSPORT_FLOOD = (0x04<<2) | 0x00 = 0x10
	body := []byte{
		0x10,
		0x34, 0x12, // tc1 = 0x1234 LE
		0x78, 0x56, // tc2 = 0x5678 LE
		0x00, // pathLen = 0
		0xDE, 0xAD, 0xBE, 0xEF,
	}
	pkt, err := PacketFromBytes(body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if pkt.RouteType() != RouteTransportFlood {
		t.Errorf("route %s want TRANSPORT_FLOOD", pkt.RouteType())
	}
	if pkt.TransportCode1 != 0x1234 || pkt.TransportCode2 != 0x5678 {
		t.Errorf("tc %x %x want 1234 5678", pkt.TransportCode1, pkt.TransportCode2)
	}
	if len(pkt.Payload) != 4 {
		t.Errorf("payload len %d want 4", len(pkt.Payload))
	}
}

// TestParseChannelInfo locks the wire layout — index, 32-byte cstring
// name, 16-byte secret. Catches any regression in the layout.
func TestParseChannelInfo(t *testing.T) {
	body := make([]byte, 1+32+16)
	body[0] = 5
	copy(body[1:], []byte("general"))
	for i := 0; i < 16; i++ {
		body[1+32+i] = byte(i)
	}
	ch, err := parseChannelInfo(body)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if ch.Index != 5 {
		t.Errorf("idx %d want 5", ch.Index)
	}
	if ch.Name != "general" {
		t.Errorf("name %q want %q", ch.Name, "general")
	}
	for i := 0; i < 16; i++ {
		if ch.Secret[i] != byte(i) {
			t.Errorf("secret[%d] %d want %d", i, ch.Secret[i], i)
		}
	}
}
