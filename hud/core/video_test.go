package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
)

// A dash that negotiates JPEG gets a queue that forwards whole stills, the host gets the config
// as a transport event and from the getter, and the carbit-exact choice reaches the listener.
func TestCaptureConfigReachesTheQueueTheHostAndTheGetter(t *testing.T) {
	live := stream.NewLiveStreamSource(30, 3*time.Second, 6)
	hud := NewCfmotoHUD(30, &stream.MuxSource{Live: live}, 0)
	hud.SetCarbitExactMediaControl(true)
	control := net.NewMediaControl(":0")
	hud.wireVideo(control, net.NewMediaStream(":0", nil, 0, 0))
	if !control.CarbitExact || control.PhoneModel != "Pixel 6a" || control.PhoneOsVersion != "36" {
		t.Fatalf("listener not configured: exact=%v model=%q os=%q", control.CarbitExact, control.PhoneModel, control.PhoneOsVersion)
	}
	if hud.CaptureConfig() != nil {
		t.Fatal("a capture config before the dash sent one")
	}

	control.OnCaptureConfig(net.CaptureConfig{Width: 800, Height: 480, WantEncoder: 0, Encoder: 1})

	event := <-hud.Events
	var sent net.CaptureConfig
	if event.Source != EventSourceTransport || event.Cmd != TransportCmdCaptureConfig || json.Unmarshal(event.Data.([]byte), &sent) != nil || sent.Width != 800 {
		t.Fatalf("capture config event = %+v", event)
	}
	if got := hud.CaptureConfig(); got == nil || got.Encoder != 1 {
		t.Fatalf("getter = %+v", got)
	}
	still := []byte{0xFF, 0xD8, 0xFF, 0xD9} // no NAL in it: an H.264 queue would wait for an IDR forever
	live.PushFrame(still)
	if frame, _ := live.NextFrame(time.Now()); string(frame) != string(still) {
		t.Fatalf("a JPEG negotiation left the queue dropping stills: %x", frame)
	}
}
