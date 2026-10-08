package api

import (
	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// TransportCmdCaptureConfig is the OnEvent command (source 4, transport) that says the dash sent
// a CAPTURE_CONFIG, or that its 0x60 updated one. The payload is the config as JSON; read it typed
// with CaptureConfig() and size the encoder with CarbitEncoderPlan.
const TransportCmdCaptureConfig = core.TransportCmdCaptureConfig

// CaptureConfig is the dash's REQ_RV_CONFIG_CAPTURE (media cmd 0x10), as sent, with what this side
// answered and what the dash's 0x60 added.
type CaptureConfig struct {
	Width, Height int // the size the dash asked for
	WantFps       int // 0: the dash polls for each frame; otherwise it is pushed frames at this rate
	WantEncoder   int // as sent: 0 or 1 JPEG, 2 H.264, 4 Carbit's software "MP4" encoder
	SupportCodec  int
	MinQuality    int
	MaxQuality    int // a JPEG's byte budget
	BitRate       int // bps; 0 means Carbit's 3 Mbps
	CapScreenMode int // bit 3 (8) asks for CBR, bit 2 (4) for a software encoder
	TouchMode     int
	// Orientation is the byte at offset 26: 0 default (landscape if wider than tall), 1 landscape,
	// 2 portrait, 3 round landscape, 4 round portrait (utils/Protocol.java:286-296).
	Orientation           int
	DisplayID             int
	VideoType             int
	SupportExtendProtocol int

	// What was answered: the encoder the dash now expects and the size it will decode.
	Encoder                     int
	CaptureWidth, CaptureHeight int

	// SupportFunction is the dash's 0x60 value; bit 0 asks for movie mode. MovieMode is whether
	// it is in effect, which needs carbit-exact media control (SetCarbitExactMediaControl).
	SupportFunction int
	MovieMode       bool

	// PushMode: frames are written to the dash at WantFps without waiting for a poll.
	PushMode bool
	// JpegRequired: the dash was answered JPEG, so feed it whole JPEGs through PushJpeg.
	JpegRequired bool
}

// EncoderPlan is what Carbit Ride 2.4 would configure for a CAPTURE_CONFIG. The app owns the
// encoder; this only says what the official app would have done.
type EncoderPlan struct {
	// Codec: 1 JPEG stills, 2 H.264 (MediaCodec "video/avc"), 4 Carbit's software "MP4" encoder
	// (libJNICodec, no app equivalent), 0 none: the dash asked for something Carbit cannot send.
	Codec         int
	Width, Height int  // the request rounded to the nearest 16 (utils/Protocol.java:143-159)
	BitRate       int  // bps: the request's, else 3_000_000 (utils/Protocol.java:324-328)
	Fps           int  // wantFps, else 36 (base/l0.java:107-121; EcFps MIDDLE, t0.java:93)
	IFrameSeconds int  // MediaFormat i-frame-interval: always 3 (nec/m.java:134)
	Cbr           bool // bitrate-mode CBR only on capScreenMode&8; else the codec's default (nec/m.java:130-132)
	// FrameIntervalMs is the push-mode pace, 1000/Fps whole milliseconds (nec/m.java:564-567).
	FrameIntervalMs int
	// JpegMaxBytes is the size each JPEG is squeezed under by a quality search: maxQuality, else
	// 25000 (base/u0.java:172-175, 86-130).
	JpegMaxBytes int
	// Landscape and RoundScreen are how Carbit reads the orientation byte (utils/Protocol.java:
	// 286-296; for 0, landscape when wider than tall, base/l0.java:219).
	Landscape   bool
	RoundScreen bool
}

// SetCarbitExactMediaControl makes the media-control channel (10921) answer exactly as Carbit
// Ride 2.4 does: 0x31 {5002, 1}, no 0x21 to touch, 0x7F02 to unknown commands, a 0x111 shell
// string, Carbit's encoder mapping (wantEncoder 0 answered JPEG) and nearest-16 sizing in 0x11,
// and the dash's own supportFunction in 0x61, which turns movie mode on when it has bit 0.
//
// Turn it on for dashes whose official app is Carbit Ride / EasyConn (flavor 51, e.g. the Zontes
// 350D JCDZ21-E42b) and for dashes no profile recognises. Leave it off for CFMOTO-lineage dashes
// (CFDL26, Voge/Morini, QJ): they have always had an empty cmd+1 to everything and a 0x21 per
// touch. Call it before StartSession.
func (ms *MobileSession) SetCarbitExactMediaControl(enabled bool) {
	ms.hud.SetCarbitExactMediaControl(enabled)
}

// CaptureConfig returns the dash's last CAPTURE_CONFIG this session, or null before one arrived.
func (ms *MobileSession) CaptureConfig() *CaptureConfig {
	c := ms.hud.CaptureConfig()
	if c == nil {
		return nil
	}
	return &CaptureConfig{
		Width: c.Width, Height: c.Height, WantFps: c.WantFps, WantEncoder: c.WantEncoder,
		SupportCodec: c.SupportCodec, MinQuality: c.MinQuality, MaxQuality: c.MaxQuality,
		BitRate: c.BitRate, CapScreenMode: c.CapScreenMode, TouchMode: c.TouchMode,
		Orientation: c.Orientation, DisplayID: c.DisplayID, VideoType: c.VideoType,
		SupportExtendProtocol: c.SupportExtendProtocol,
		Encoder:               c.Encoder, CaptureWidth: c.CaptureWidth, CaptureHeight: c.CaptureHeight,
		SupportFunction: c.SupportFunction, MovieMode: c.MovieMode,
		PushMode:     c.WantFps != 0 && !c.MovieMode,
		JpegRequired: c.Encoder == 1,
	}
}

// CarbitEncoderPlan is what Carbit Ride 2.4 would configure for c (base/l0.java:576-636,
// nec/m.java:69-136), whatever this session answered. Null for a null config.
func CarbitEncoderPlan(c *CaptureConfig) *EncoderPlan {
	if c == nil {
		return nil
	}
	p := &EncoderPlan{
		Codec:         net.CarbitEncoder(c.WantEncoder),
		Width:         net.CarbitAlign16(c.Width),
		Height:        net.CarbitAlign16(c.Height),
		BitRate:       c.BitRate,
		Fps:           c.WantFps,
		IFrameSeconds: 3,
		Cbr:           c.CapScreenMode&8 != 0,
		JpegMaxBytes:  c.MaxQuality,
		RoundScreen:   c.Orientation == 3 || c.Orientation == 4,
	}
	if p.BitRate == 0 {
		p.BitRate = 3000000
	}
	if p.Fps == 0 {
		p.Fps = 36
	}
	if p.Fps > 0 {
		p.FrameIntervalMs = 1000 / p.Fps
	}
	if p.JpegMaxBytes <= 0 {
		p.JpegMaxBytes = 25000
	}
	switch c.Orientation {
	case 1, 3:
		p.Landscape = true
	case 2, 4:
	default:
		p.Landscape = c.Width > c.Height
	}
	return p
}

// PushJpeg hands the dash one whole JPEG. It is PushStill for a dash that asked for JPEG itself
// (CaptureConfig.JpegRequired), in poll and push mode alike; with JpegStillsEnabled it behaves
// exactly as PushStill.
func (ms *MobileSession) PushJpeg(jpeg []byte) {
	if !ms.hud.IsRunning() || len(jpeg) == 0 {
		return
	}
	if c := ms.hud.CaptureConfig(); !ms.cfg.JpegStillsEnabled && (c == nil || c.Encoder != 1) {
		// Same reason as PushStill: a JPEG inside a frame the dash parses as H.264 helps no one.
		if ms.cb != nil {
			go ms.cb.OnError("PushJpeg called on a session that did not negotiate JPEG", false)
		}
		logging.Printf("MobileSession: PushJpeg ignored, the dash was not answered JPEG\n")
		return
	}
	ms.mux.Live.PushFrame(jpeg)
}
