package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
)

type eventCaptureCallback struct {
	events chan []byte
}

func (c *eventCaptureCallback) OnError(string, bool) {}

func (c *eventCaptureCallback) OnEvent(_ int64, _ int, _ int, payload []byte) {
	c.events <- payload
}

func (c *eventCaptureCallback) OnStopped() {}

func TestBuildAnnexBAUFromAVCCRejectsTruncatedNAL(t *testing.T) {
	_, err := BuildAnnexBAUFromAVCC([]byte{0, 0, 0, 4, 0x65})
	if err == nil {
		t.Fatal("BuildAnnexBAUFromAVCC() accepted a truncated NAL")
	}
}

func TestNewMobileSessionAllowsLiveOnlyMode(t *testing.T) {
	config := NewMobileConfig(nil, 30, 10, 5, 10, 3)
	session, err := NewMobileSession(config, nil)
	if err != nil {
		t.Fatalf("create live-only mobile session: %v", err)
	}
	defer session.StopSession()
	if session.mux.NoSignal != nil {
		t.Fatal("live-only session unexpectedly has a static fallback")
	}
}

// A PXC body arrives as json.RawMessage. Relaying it through a plain []byte
// type assertion silently dropped it - see relayEvent - so this pins the shape
// the phone actually receives.
func TestRelayEventCopiesJSONRawMessagePayload(t *testing.T) {
	callback := &eventCaptureCallback{events: make(chan []byte, 1)}
	session := &MobileSession{cb: callback}
	payload := json.RawMessage(`{"HUName":"CFDL26"}`)

	session.relayEvent(core.HudEvent{
		Source: core.EventSourcePXC,
		Cmd:    65552,
		Data:   payload,
	})

	select {
	case received := <-callback.events:
		if string(received) != string(payload) {
			t.Fatalf("payload = %q, want %q", received, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("PXC event payload was not relayed")
	}
}

func TestRelayEventStillCopiesPlainByteSlicePayload(t *testing.T) {
	callback := &eventCaptureCallback{events: make(chan []byte, 1)}
	session := &MobileSession{cb: callback}
	payload := []byte{0x00, 0x04, 0xbb, 0x01}

	session.relayEvent(core.HudEvent{
		Source: core.EventSourceMediaControl,
		Cmd:    16,
		Data:   payload,
	})

	select {
	case received := <-callback.events:
		if string(received) != string(payload) {
			t.Fatalf("payload = %x, want %x", received, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("media control event payload was not relayed")
	}
}

// Before the dash has completed the handshake there is no phone channel to send on; the call
// must say so rather than block or panic.
func TestSendPhoneCommandBeforeTheSessionRunsIsAnError(t *testing.T) {
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()
	if reply, err := session.SendPhoneCommand(0x20110, []byte(`{}`), 100); err == nil || reply != nil {
		t.Fatalf("SendPhoneCommand = %+v, %v; want an error", reply, err)
	}
}

// NAL lengths a broken sample can carry, including the ones that wrap a 32-bit int.
func TestBuildAnnexBAUFromAVCCRejectsImpossibleLengths(t *testing.T) {
	for _, length := range []uint32{0, 0x7FFFFFFF, 0x80000000, 0xFFFFFFFF} {
		sample := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length), 0x65, 1, 2}
		if _, err := BuildAnnexBAUFromAVCC(sample); err == nil {
			t.Errorf("NAL length 0x%x accepted", length)
		}
	}
}

type transportCapture struct {
	commands chan int
}

func (c *transportCapture) OnError(string, bool) {}
func (c *transportCapture) OnEvent(_ int64, source int, command int, _ []byte) {
	if source == int(core.EventSourceTransport) {
		c.commands <- command
	}
}
func (c *transportCapture) OnStopped() {}

// The queue asking for a keyframe reaches the host as a transport event it can act on.
func TestAnOverflowWithNoIDRAsksTheHostForAKeyframe(t *testing.T) {
	callback := &transportCapture{commands: make(chan int, 4)}
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), callback)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()
	idr := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 0x65, 1}
	p := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 0x41, 1}
	session.mux.Live.PushFrame(idr)
	for i := 0; i < 6; i++ {
		session.mux.Live.PushFrame(p)
	}
	select {
	case command := <-callback.commands:
		if command != core.TransportCmdKeyframeNeeded {
			t.Fatalf("transport command = %d, want %d", command, core.TransportCmdKeyframeNeeded)
		}
	case <-time.After(time.Second):
		t.Fatal("the host was never asked for a keyframe")
	}
}
