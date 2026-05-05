package meshcore

import (
	"encoding/binary"
	"fmt"
)

// Packet is one decoded MeshCore mesh-layer packet — what the radio
// hears on the air, surfaced to the host via PushLogRxData. Fields
// mirror the structure parsed by liamcottle/meshcore.js (src/packet.js)
// and the firmware in meshcore-dev/MeshCore.
//
// Operators inspect these in the RxLog pane to debug routing,
// confirm coverage, and watch advert traffic in real time.
type Packet struct {
	Header         byte
	PathLen        byte
	Path           []byte
	Payload        []byte
	TransportCode1 uint16 // 0 unless RouteType is one of the TRANSPORT_* variants
	TransportCode2 uint16
}

// Packet header bit layout:
//
//	bits 0-1  RouteType        (PH_ROUTE_MASK)
//	bits 2-5  PayloadType      (PH_TYPE_MASK after PH_TYPE_SHIFT)
//	bits 6-7  PayloadVersion   (PH_VER_MASK after PH_VER_SHIFT)
const (
	phRouteMask = 0x03
	phTypeShift = 2
	phTypeMask  = 0x0F
	phVerShift  = 6
	phVerMask   = 0x03
)

// RouteType identifies how a packet is forwarded across the mesh.
type RouteType byte

const (
	RouteTransportFlood  RouteType = 0
	RouteFlood           RouteType = 1
	RouteDirect          RouteType = 2
	RouteTransportDirect RouteType = 3
)

// String returns the upstream label used in `meshcore.js` so debug
// output matches the web client's RxLog rows verbatim.
func (r RouteType) String() string {
	switch r {
	case RouteFlood:
		return "FLOOD"
	case RouteDirect:
		return "DIRECT"
	case RouteTransportFlood:
		return "TRANSPORT_FLOOD"
	case RouteTransportDirect:
		return "TRANSPORT_DIRECT"
	default:
		return fmt.Sprintf("RT(%d)", byte(r))
	}
}

// hasTransportCodes reports whether the route type prefixes the
// pathLen byte with two uint16 transport codes.
func (r RouteType) hasTransportCodes() bool {
	return r == RouteTransportFlood || r == RouteTransportDirect
}

// PayloadType identifies the body interpretation of a packet.
type PayloadType byte

const (
	PayloadReq       PayloadType = 0x00
	PayloadResponse  PayloadType = 0x01
	PayloadTxtMsg    PayloadType = 0x02
	PayloadAck       PayloadType = 0x03
	PayloadAdvert    PayloadType = 0x04
	PayloadGrpTxt    PayloadType = 0x05
	PayloadGrpData   PayloadType = 0x06
	PayloadAnonReq   PayloadType = 0x07
	PayloadPath      PayloadType = 0x08
	PayloadTrace     PayloadType = 0x09
	PayloadRawCustom PayloadType = 0x0F
)

// String returns the upstream label used by `meshcore.js`.
func (p PayloadType) String() string {
	switch p {
	case PayloadReq:
		return "REQ"
	case PayloadResponse:
		return "RESPONSE"
	case PayloadTxtMsg:
		return "TXT_MSG"
	case PayloadAck:
		return "ACK"
	case PayloadAdvert:
		return "ADVERT"
	case PayloadGrpTxt:
		return "GRP_TXT"
	case PayloadGrpData:
		return "GRP_DATA"
	case PayloadAnonReq:
		return "ANON_REQ"
	case PayloadPath:
		return "PATH"
	case PayloadTrace:
		return "TRACE"
	case PayloadRawCustom:
		return "RAW_CUSTOM"
	default:
		return fmt.Sprintf("PT(%d)", byte(p))
	}
}

// PacketFromBytes decodes a raw mesh packet (the `raw` field of a
// PushLogRxData event). Returns ErrShortPayload if the input is
// truncated mid-frame.
func PacketFromBytes(b []byte) (Packet, error) {
	r := newBufReader(b)
	header, err := r.byte_()
	if err != nil {
		return Packet{}, err
	}
	pkt := Packet{Header: header}
	if pkt.RouteType().hasTransportCodes() {
		tc1, err := r.uint16LE()
		if err != nil {
			return pkt, err
		}
		tc2, err := r.uint16LE()
		if err != nil {
			return pkt, err
		}
		pkt.TransportCode1 = tc1
		pkt.TransportCode2 = tc2
	}
	pathLenByte, err := r.byte_()
	if err != nil {
		return pkt, err
	}
	pkt.PathLen = pathLenByte
	hashSize := int(pathLenByte>>6) + 1 // top 2 bits + 1
	hashCount := int(pathLenByte & 0x3F)
	pathByteLen := hashSize * hashCount
	if pathByteLen > 0 {
		path, err := r.bytes(pathByteLen)
		if err != nil {
			return pkt, err
		}
		pkt.Path = make([]byte, len(path))
		copy(pkt.Path, path)
	}
	if remaining := r.remaining(); remaining > 0 {
		body, err := r.bytes(remaining)
		if err != nil {
			return pkt, err
		}
		pkt.Payload = make([]byte, len(body))
		copy(pkt.Payload, body)
	}
	return pkt, nil
}

// RouteType returns the parsed route bits from Header.
func (p Packet) RouteType() RouteType { return RouteType(p.Header & phRouteMask) }

// PayloadType returns the parsed payload-type bits from Header.
func (p Packet) PayloadType() PayloadType {
	return PayloadType((p.Header >> phTypeShift) & phTypeMask)
}

// PayloadVersion returns the parsed payload-version bits from Header.
func (p Packet) PayloadVersion() byte { return (p.Header >> phVerShift) & phVerMask }

// HopCount returns the number of path hashes encoded in Path —
// roughly how many hops the packet has taken so far.
func (p Packet) HopCount() int {
	if p.PathLen == 0xFF {
		return 0 // sentinel for "delivered direct, no path"
	}
	return int(p.PathLen & 0x3F)
}

// uint16LE adds a small reader helper alongside bufReader's int/byte
// methods. Defined here (not in client.go) so packet parsing can live
// without pulling in the rest of the client.
func (r *bufReader) uint16LE() (uint16, error) {
	v, err := r.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(v), nil
}

// remaining returns the number of unread bytes left in the buffer.
func (r *bufReader) remaining() int { return len(r.b) - r.i }
