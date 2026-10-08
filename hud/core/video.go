package core

import (
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

// TransportCmdCaptureConfig reports the dash's CAPTURE_CONFIG (media cmd 0x10) with what it was
// answered, when it arrives and again when the dash's 0x60 updates it. Payload: net.CaptureConfig
// as JSON. Numbered apart from 1-5 so the other transport events can keep counting up.
const TransportCmdCaptureConfig = 16

// videoPlane is the media-control and video state the host sets before a session or reads back.
type videoPlane struct {
	carbitExact bool
	capture     atomic.Pointer[net.CaptureConfig]
}

// SetCarbitExactMediaControl answers the media-control channel exactly as Carbit Ride 2.4 does.
// See net.MediaControl.CarbitExact. Configure it before StartStream.
func (hud *CfmotoHUD) SetCarbitExactMediaControl(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.video.carbitExact = enabled
}

// CaptureConfig is the dash's last CAPTURE_CONFIG this session, or nil before one arrived.
func (hud *CfmotoHUD) CaptureConfig() *net.CaptureConfig {
	return hud.video.capture.Load()
}

// wireVideo connects the media-control listener to the video listener and the host: the
// CAPTURE_CONFIG decides how frames are delivered and whether the live queue carries JPEG.
func (hud *CfmotoHUD) wireVideo(mc *net.MediaControl, ms *net.MediaStream) {
	hud.video.capture.Store(nil)
	mc.CarbitExact = hud.video.carbitExact
	mc.PhoneModel = hud.phoneConfig.PhoneModel
	mc.PhoneOsVersion = hud.phoneConfig.PhoneOsVersion
	mc.OnCaptureConfig = func(c net.CaptureConfig) {
		ms.SetCaptureConfig(c)
		hud.muxSource.SetLiveOpaque(c.Encoder == 1)
		hud.muxSource.SetLivePollMode(c.WantFps == 0 && !c.MovieMode) // as MediaStream.handleConn picks the loop
		hud.video.capture.Store(&c)
		payload, _ := json.Marshal(c)
		hud.handleServerEvent(HudEvent{
			Source: EventSourceTransport,
			Time:   time.Now(),
			Cmd:    TransportCmdCaptureConfig,
			Data:   payload,
		})
	}
}
