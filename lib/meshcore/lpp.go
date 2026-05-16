package meshcore

// lpp.go — Cayenne LPP (Low Power Payload) parser. MeshCore
// sensor nodes ship telemetry replies as a sequence of LPP
// channel/type/value records; we decode the common types into
// typed Go fields so the GUI can render "battery: 78%, temp:
// 22.4°C, humidity: 45.5%, GPS: 33.02, -117.07" inline without
// every consumer re-implementing the per-type scaling.
//
// Unknown types stop the parse — the upstream JS reference does
// the same and notes garbage tail bytes as the motivation.

import (
	"encoding/binary"
)

// LPPType identifies one telemetry channel's measurement type.
// Mirrors the values in meshcore.js src/cayenne_lpp.js.
type LPPType byte

const (
	LPPDigitalInput       LPPType = 0   // 1 byte
	LPPDigitalOutput      LPPType = 1   // 1 byte
	LPPAnalogInput        LPPType = 2   // 2 bytes, signed × 0.01
	LPPAnalogOutput       LPPType = 3   // 2 bytes, signed × 0.01
	LPPGenericSensor      LPPType = 100 // 4 bytes uint32 BE
	LPPLuminosity         LPPType = 101 // 2 bytes, signed lux
	LPPPresence           LPPType = 102 // 1 byte bool
	LPPTemperature        LPPType = 103 // 2 bytes, signed × 0.1 °C
	LPPRelativeHumidity   LPPType = 104 // 1 byte × 0.5 %
	LPPAccelerometer      LPPType = 113 // 6 bytes (x,y,z) × 0.001 g
	LPPBarometricPressure LPPType = 115 // 2 bytes × 0.1 hPa
	LPPVoltage            LPPType = 116 // 2 bytes × 0.01 V
	LPPCurrent            LPPType = 117 // 2 bytes × 0.001 A
	LPPFrequency          LPPType = 118 // 4 bytes × 1 Hz
	LPPPercentage         LPPType = 120 // 1 byte 0-100 %
	LPPAltitude           LPPType = 121 // 2 bytes signed metres
	LPPUnixTime           LPPType = 133 // 4 bytes
	LPPGPS                LPPType = 136 // 9 bytes (3 lat + 3 lon + 3 alt)
)

// LPPReading is one parsed channel/type/value triple.
//
// Value carries the type-appropriate decoded measurement:
//
//	float64 for everything continuous (temp, humidity, voltage, …)
//	[3]float64 for accelerometer / gyrometer / GPS triplets
//	uint32 for unix-time, generic-sensor, frequency
//	bool for presence / digital-input
//
// Unit is the human-readable unit string ("°C", "%", "V", "Hz", …).
// Both Value and Unit are populated for every recognised type so
// the GUI can render a uniform "channel/type/value/unit" table.
type LPPReading struct {
	Channel byte
	Type    LPPType
	Value   any
	Unit    string
	// Raw is the per-record payload bytes — preserved verbatim for
	// types the parser doesn't decode (so the GUI can still show
	// hex without dropping data).
	Raw []byte
}

// ParseLPP walks b as a sequence of [channel][type][value...]
// records and returns the decoded list. Stops at the first
// channel=0 type=0 pair (matches the upstream's "garbage tail
// bytes" guard) or when it encounters an unknown type — in the
// latter case it appends a Raw-only record and returns so the
// caller can decide whether to keep the partial result.
func ParseLPP(b []byte) []LPPReading {
	var out []LPPReading
	i := 0
	for i+1 < len(b) {
		ch := b[i]
		tp := LPPType(b[i+1])
		if ch == 0 && tp == 0 {
			break
		}
		i += 2
		r := LPPReading{Channel: ch, Type: tp}
		size := lppSize(tp)
		if size < 0 || i+size > len(b) {
			// Unknown type or truncated record. Capture the
			// remaining bytes as Raw and bail — partial output
			// is better than silently dropping.
			r.Raw = append(r.Raw, b[i:]...)
			out = append(out, r)
			return out
		}
		payload := b[i : i+size]
		r.Raw = append(r.Raw, payload...)
		switch tp {
		case LPPDigitalInput, LPPDigitalOutput, LPPPresence:
			r.Value = payload[0] != 0
		case LPPAnalogInput, LPPAnalogOutput:
			r.Value = float64(int16(binary.BigEndian.Uint16(payload))) / 100
			r.Unit = ""
		case LPPGenericSensor, LPPFrequency, LPPUnixTime:
			r.Value = binary.BigEndian.Uint32(payload)
			if tp == LPPFrequency {
				r.Unit = "Hz"
			}
		case LPPLuminosity:
			r.Value = float64(int16(binary.BigEndian.Uint16(payload)))
			r.Unit = "lux"
		case LPPTemperature:
			r.Value = float64(int16(binary.BigEndian.Uint16(payload))) / 10
			r.Unit = "°C"
		case LPPRelativeHumidity:
			r.Value = float64(payload[0]) / 2
			r.Unit = "%"
		case LPPBarometricPressure:
			r.Value = float64(binary.BigEndian.Uint16(payload)) / 10
			r.Unit = "hPa"
		case LPPVoltage:
			r.Value = float64(binary.BigEndian.Uint16(payload)) / 100
			r.Unit = "V"
		case LPPCurrent:
			r.Value = float64(binary.BigEndian.Uint16(payload)) / 1000
			r.Unit = "A"
		case LPPPercentage:
			r.Value = float64(payload[0])
			r.Unit = "%"
		case LPPAltitude:
			r.Value = float64(int16(binary.BigEndian.Uint16(payload)))
			r.Unit = "m"
		case LPPAccelerometer:
			r.Value = [3]float64{
				float64(int16(binary.BigEndian.Uint16(payload[0:2]))) / 1000,
				float64(int16(binary.BigEndian.Uint16(payload[2:4]))) / 1000,
				float64(int16(binary.BigEndian.Uint16(payload[4:6]))) / 1000,
			}
			r.Unit = "g"
		case LPPGPS:
			r.Value = [3]float64{
				lpp24BitSigned(payload[0:3]) / 10000,
				lpp24BitSigned(payload[3:6]) / 10000,
				lpp24BitSigned(payload[6:9]) / 100,
			}
			r.Unit = "deg,deg,m"
		}
		out = append(out, r)
		i += size
	}
	return out
}

// lppSize returns the per-record value byte count for type t, or
// -1 when the type isn't recognised (caller bails the parse).
func lppSize(t LPPType) int {
	switch t {
	case LPPDigitalInput, LPPDigitalOutput, LPPPresence,
		LPPRelativeHumidity, LPPPercentage:
		return 1
	case LPPAnalogInput, LPPAnalogOutput, LPPLuminosity,
		LPPTemperature, LPPBarometricPressure, LPPVoltage,
		LPPCurrent, LPPAltitude:
		return 2
	case LPPGenericSensor, LPPFrequency, LPPUnixTime:
		return 4
	case LPPAccelerometer:
		return 6
	case LPPGPS:
		return 9
	default:
		return -1
	}
}

// lpp24BitSigned decodes a 24-bit signed integer (big-endian)
// into a float64. Cayenne LPP encodes GPS coordinates as
// truncated 24-bit signed ints; the upper byte's high bit
// extends to fill the int32 sign bits.
func lpp24BitSigned(b []byte) float64 {
	if len(b) < 3 {
		return 0
	}
	v := int32(uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]))
	if v&0x800000 != 0 {
		v |= ^0xFFFFFF // sign-extend
	}
	return float64(v)
}
