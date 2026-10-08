package net

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ECTinyPlus, the MCU-Lite message set. proto2, package ectinyplus, rebuilt from the descriptor
// CarbitRide embeds (nec/ECTinyPlus.java:32). Only the messages this phone reads or writes are
// coded here. The OTA ones (OTAConfig, OTAStart, OTAResult, OTAData) carry Carbit's cloud update:
// they are acked and reach the host raw.
//
//	message ClientInfo {          // dash -> phone, 0x1080010; every field required
//	  string sdkVersion = 1; string protoVersion = 2; string uuid = 3; uint32 channel = 4;
//	  uint32 supportFunction = 5; uint32 flavor = 6; string pwd = 7; uint64 timeStamp = 8;
//	  string huid = 9; uint32 hardwareAuthState = 10; }
//	message ClientInfoRly {       // phone -> dash, 0x1080011
//	  required string protoVersion = 1; required PhoneType phoneType = 2; // ANDROID 0, IOS 1
//	  optional string phoneOsVersion = 3; required uint32 token = 4;
//	  required uint32 hardwarePermissionState = 5; required string hardwarePermissionMsg = 6; }
//	message MirrorConfig {        // dash -> phone, 0x1080020
//	  required uint32 width = 1; required uint32 height = 2; required uint32 wantFps = 3;
//	  required VideoCodecType wantEncoder = 4; optional uint32 quality = 5;
//	  optional uint32 bitTate = 6; optional float screenPhysicsWidth = 7;
//	  optional float screenPhysicsHeight = 8; optional uint32 dpi = 9; }
//	message MirrorConfigRly {     // phone -> dash, 0x1080021; every field required
//	  uint32 width = 1; uint32 height = 2; uint32 fps = 3; VideoCodecType encoder = 4; }
//	message MirrorDataGPS {       // data frame type 2; every field required
//	  bool status = 1; string altitude = 2; string speed = 3; string course = 4;
//	  string time = 5; string latitude = 6; string longitude = 7; }
//	message MirrorDataWeather {   // data frame type 1; every field required, all strings
//	  adCode = 1; city = 2; humidity = 3; province = 4; reportTime = 5; temperature = 6;
//	  weather = 7; windDirection = 8; windPower = 9; }
//	enum VideoCodecType { NONE = 0; JEPG = 1; H264 = 2; MP4 = 3; }

// MCULiteClientInfo is the dash's ClientInfo (0x1080010).
type MCULiteClientInfo struct {
	SDKVersion        string
	ProtoVersion      string
	UUID              string
	Channel           uint32
	SupportFunction   uint32 // bit 0 OTA, bit 1 weather, bit 2 GPS (um/b.java:150-160)
	Flavor            uint32
	Pwd               string
	TimeStamp         uint64
	HUID              string
	HardwareAuthState uint32
}

// MCULiteMirrorConfig is the picture the dash asked for (0x1080020), the size already floored to
// a multiple of 16 the way the reply states it, and the quality clamped to 0-100 (um/c.java:44-60).
type MCULiteMirrorConfig struct {
	Width, Height uint32
	Fps           uint32
	Encoder       uint32 // echoed, but the picture is JPEG whatever it says (c0.java:105-131)
	Quality       uint32 // JPEG quality
	BitRate       uint32
	Dpi           uint32  // 0: the phone picks one for the size (um/c.java:61-66)
	ScreenWidth   float32 // physical screen, inches (common/base/q.java:30-33)
	ScreenHeight  float32
}

// MCULiteGPS is one MirrorDataGPS, every value already a string as CarbitRide sends it
// (ec/b.java:1063-1078): time is the phone's uptime in milliseconds.
type MCULiteGPS struct {
	Status                                             bool
	Latitude, Longitude, Altitude, Speed, Course, Time string
}

// MCULiteWeather is one MirrorDataWeather (common/base/y.java:358-372).
type MCULiteWeather struct {
	AdCode, City, Humidity, Province, ReportTime, Temperature, Weather, WindDirection, WindPower string
}

var errBadProto = errors.New("malformed protobuf")

// pbMessage builds a protobuf message field by field, in field-number order as protobuf-java
// writes them. ponytail: hand-written varint encoding; the set is six small messages, no runtime.
type pbMessage []byte

func (m pbMessage) varint(field int, v uint64) pbMessage {
	return binary.AppendUvarint(binary.AppendUvarint(m, uint64(field)<<3), v)
}

func (m pbMessage) str(field int, s string) pbMessage {
	m = binary.AppendUvarint(binary.AppendUvarint(m, uint64(field)<<3|2), uint64(len(s)))
	return append(m, s...)
}

// pbFields walks one message, handing fn each field: v for varint and fixed fields, b for
// length-delimited ones. It returns the set of field numbers seen (below 64), for the required
// check protobuf-java makes in parseFrom.
func pbFields(msg []byte, fn func(field int, v uint64, b []byte)) (seen uint64, err error) {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 || key>>3 == 0 {
			return seen, errBadProto
		}
		msg = msg[n:]
		var v uint64
		var b []byte
		switch key & 7 {
		case 0:
			if v, n = binary.Uvarint(msg); n <= 0 {
				return seen, errBadProto
			}
			msg = msg[n:]
		case 1:
			if len(msg) < 8 {
				return seen, errBadProto
			}
			v, msg = binary.LittleEndian.Uint64(msg), msg[8:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return seen, errBadProto
			}
			b, msg = msg[n:n+int(l)], msg[n+int(l):]
		case 5:
			if len(msg) < 4 {
				return seen, errBadProto
			}
			v, msg = uint64(binary.LittleEndian.Uint32(msg)), msg[4:]
		default:
			return seen, errBadProto
		}
		if key>>3 < 64 {
			seen |= 1 << (key >> 3)
		}
		fn(int(key>>3), v, b)
	}
	return seen, nil
}

// required is the field set 1..n.
func required(n int) uint64 { return (1<<(n+1) - 1) &^ 1 }

func decodeMCUClientInfo(body []byte) (*MCULiteClientInfo, error) {
	var c MCULiteClientInfo
	seen, err := pbFields(body, func(f int, v uint64, b []byte) {
		switch f {
		case 1:
			c.SDKVersion = string(b)
		case 2:
			c.ProtoVersion = string(b)
		case 3:
			c.UUID = string(b)
		case 4:
			c.Channel = uint32(v)
		case 5:
			c.SupportFunction = uint32(v)
		case 6:
			c.Flavor = uint32(v)
		case 7:
			c.Pwd = string(b)
		case 8:
			c.TimeStamp = v
		case 9:
			c.HUID = string(b)
		case 10:
			c.HardwareAuthState = uint32(v)
		}
	})
	if err == nil && seen&required(10) != required(10) {
		err = fmt.Errorf("ClientInfo is missing required fields (seen 0x%x)", seen)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func decodeMCUMirrorConfig(body []byte) (*MCULiteMirrorConfig, error) {
	var c MCULiteMirrorConfig
	seen, err := pbFields(body, func(f int, v uint64, _ []byte) {
		switch f {
		case 1:
			c.Width = uint32(v)
		case 2:
			c.Height = uint32(v)
		case 3:
			c.Fps = uint32(v)
		case 4:
			c.Encoder = uint32(v)
		case 5:
			c.Quality = uint32(v)
		case 6:
			c.BitRate = uint32(v)
		case 7:
			c.ScreenWidth = math.Float32frombits(uint32(v))
		case 8:
			c.ScreenHeight = math.Float32frombits(uint32(v))
		case 9:
			c.Dpi = uint32(v)
		}
	})
	if err == nil && seen&required(4) != required(4) {
		err = fmt.Errorf("MirrorConfig is missing required fields (seen 0x%x)", seen)
	}
	if err != nil {
		return nil, err
	}
	c.Width &^= 15
	c.Height &^= 15
	c.Quality = min(c.Quality, 100)
	return &c, nil
}

// encodeMCUClientInfoRly is um/b.n(): phoneOsVersion is never set, the permission state is the
// stored one (0 until Carbit's cloud says otherwise) and its message empty.
func encodeMCUClientInfoRly(protoVersion string, token uint32) []byte {
	return pbMessage(nil).str(1, protoVersion).varint(2, 0).varint(4, uint64(token)).varint(5, 0).str(6, "")
}

func encodeMCUMirrorConfigRly(c *MCULiteMirrorConfig) []byte {
	return pbMessage(nil).varint(1, uint64(c.Width)).varint(2, uint64(c.Height)).
		varint(3, uint64(c.Fps)).varint(4, uint64(c.Encoder))
}

func encodeMCUGPS(g MCULiteGPS) []byte {
	status := uint64(0)
	if g.Status {
		status = 1
	}
	return pbMessage(nil).varint(1, status).str(2, g.Altitude).str(3, g.Speed).str(4, g.Course).
		str(5, g.Time).str(6, g.Latitude).str(7, g.Longitude)
}

func encodeMCUWeather(w MCULiteWeather) []byte {
	return pbMessage(nil).str(1, w.AdCode).str(2, w.City).str(3, w.Humidity).str(4, w.Province).
		str(5, w.ReportTime).str(6, w.Temperature).str(7, w.Weather).str(8, w.WindDirection).
		str(9, w.WindPower)
}
