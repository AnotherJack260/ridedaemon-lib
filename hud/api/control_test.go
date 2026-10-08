package api

import (
	"testing"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

// The control API refuses what it cannot use instead of failing later on the wire.
func TestControlAPIRefusesWhatItCannotUse(t *testing.T) {
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()

	if err := session.SetPhoneIdentity(&PhoneIdentity{RSAKey: []byte("not a key")}); err == nil {
		t.Error("an unreadable identity key was accepted")
	}
	if err := session.SetPhoneIdentity(&PhoneIdentity{Model: "SM-S918B"}); err != nil {
		t.Errorf("a plain identity was refused: %v", err)
	}
	if err := session.SetTLSCertificate([]byte("cert"), []byte("key")); err == nil {
		t.Error("an unreadable TLS certificate was accepted")
	}
	if err := session.ReleaseDash(); err == nil {
		t.Error("ReleaseDash succeeded with no session running")
	}
}

// The transport event numbers listed in doc.go never share a number.
func TestTransportEventNumbersAreDistinct(t *testing.T) {
	seen := map[int]bool{}
	for _, cmd := range []int{
		core.TransportCmdVideoFraming, core.TransportCmdVideoPulls, core.TransportCmdPageSwitchProbe,
		core.TransportCmdAppStatusNotify, core.TransportCmdKeyframeNeeded, TransportCmdCaptureConfig,
		core.TransportCmdUSBBanner, int(net.PxcDashMainPage), int(net.PxcSwitchToAppPage),
		int(net.PxcDashUiFront), int(net.PxcDashDisconnect), int(net.PxcStartTLS),
	} {
		if seen[cmd] {
			t.Fatalf("transport event 0x%x is used twice", cmd)
		}
		seen[cmd] = true
	}
}
