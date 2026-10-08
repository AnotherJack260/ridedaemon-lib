package core

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

func TestStopBeforeStartSignalsDone(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	if err := hud.StopStream(context.Background()); err != nil {
		t.Fatalf("StopStream() error = %v", err)
	}
	select {
	case <-hud.Done():
	default:
		t.Fatal("StopStream() did not signal Done before stream start")
	}
}

func TestPxcCoordinatorForwardsConfigAndSignalsHeartbeat(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	pxc := &net.PXCControl{Events: make(chan net.PXCResponse, 2)}
	ready := make(chan any, 1)
	hud.startPxcEventFwd(pxc, ready)

	payload := json.RawMessage(`{"HUName":"MTX800"}`)
	pxc.Events <- net.PXCResponse{Command: net.PxcHudConf, Body: payload}
	pxc.Events <- net.PXCResponse{Command: net.PxcHeartbeatOk}

	if event := <-hud.Events; event.Source != EventSourcePXC ||
		event.Cmd != int(net.PxcHudConf) ||
		string(event.Data.(json.RawMessage)) != string(payload) {
		t.Fatalf("unexpected PXC config event: %+v", event)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("PXC heartbeat acknowledgement did not signal readiness")
	}
	if event := <-hud.Events; event.Cmd != int(net.PxcHeartbeatOk) {
		t.Fatalf("heartbeat event = %+v, want heartbeat ACK", event)
	}
	close(pxc.Events)
}

func TestSetHostWhenRunningReturnsErrorWithoutPanicking(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	hud.running = true
	if err := hud.SetHost(&EcHost{}); err == nil {
		t.Fatal("SetHost() succeeded while the HUD was running")
	}
}

func TestVideoFramingDecisionIsForwardedAsTransportEvent(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)

	hud.handleServerEvent(HudEvent{
		Source: EventSourceTransport,
		Cmd:    TransportCmdVideoFraming,
		Data:   []byte{0, 1},
	})

	event := <-hud.Events
	if event.Source != EventSourceTransport || event.Cmd != TransportCmdVideoFraming {
		t.Fatalf("transport event was rewrapped: %+v", event)
	}
	payload, ok := event.Data.([]byte)
	if !ok || len(payload) != 2 || payload[0] != 0 || payload[1] != 1 {
		t.Fatalf("transport payload = %+v, want [0 1]", event.Data)
	}
}

// Advertising supportSyncCorrectTime was tried on 2026-08-10 and withdrawn the
// same day: the reference implementation shipped that claim and then reported
// that firmware which saw it applied the 0x10601 reply aggressively, driving
// Zontes and Voge clusters to 00:00 even when their clock was already right.
// The phone still answers 0x10600 with a full body - that half has evidence
// behind it - it just does not ask to be asked.
func TestPhoneConfigDoesNotAnnounceClockSyncSupport(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	body, err := json.Marshal(hud.phoneConfig)
	if err != nil {
		t.Fatalf("Marshal(phoneConfig) error = %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("Unmarshal(phoneConfig) error = %v", err)
	}
	if value, present := sent["supportSyncCorrectTime"]; present {
		t.Errorf(
			"CLIENT_INFO reply advertises supportSyncCorrectTime = %v; claiming it drove "+
				"Zontes and Voge clusters to 00:00, so it must stay off the wire", value,
		)
	}
}

// The host's persisted identity replaces the defaults field by field; an empty identity changes
// nothing, so a host that never calls this keeps the reply every earlier build sent.
func TestPhoneIdentityShapesTheClientInfoReply(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	before := *hud.phoneConfig
	if err := hud.SetPhoneIdentity(PhoneIdentity{}); err != nil {
		t.Fatal(err)
	}
	if after := *hud.phoneConfig; after.Package != before.Package || after.PhoneModel != before.PhoneModel ||
		after.PhoneUUID != before.PhoneUUID || after.PhoneOsVersion != before.PhoneOsVersion || hud.identityKey != nil {
		t.Fatalf("an empty identity changed the reply: %+v", after)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	err := hud.SetPhoneIdentity(PhoneIdentity{
		Package: "net.easyconn.easyride.wws", PhoneUUID: "a1b2", RSAKey: der,
		Brand: "samsung", Model: "SM-S918B", SdkVersion: 35, BluetoothName: "Galaxy S23 Ultra",
		EchoSupportFunction: true, SpeechEngineType: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	pc := hud.phoneConfig
	if pc.Package != "net.easyconn.easyride.wws" || pc.PhoneUUID != "a1b2" || pc.PhoneBrand != "samsung" ||
		pc.PhoneModel != "SM-S918B" || pc.PhoneOsVersion != "35" || pc.BluetoothName != "Galaxy S23 Ultra" {
		t.Fatalf("identity not applied: %+v", *pc)
	}
	if hud.identityKey == nil || !hud.identityKey.PublicKey.Equal(&key.PublicKey) ||
		!hud.echoSupportFunction || hud.speechEngineType != -1 {
		t.Fatal("key, echo or speech engine not kept for the session")
	}

	if err := hud.SetPhoneIdentity(PhoneIdentity{RSAKey: []byte("junk")}); err == nil {
		t.Fatal("an unreadable key was accepted")
	}
	hud.running = true
	if err := hud.SetPhoneIdentity(PhoneIdentity{Model: "x"}); err == nil {
		t.Fatal("the identity changed under a running session")
	}
}
