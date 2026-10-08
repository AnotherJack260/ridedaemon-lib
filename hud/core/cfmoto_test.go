package core

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	stdnet "net"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
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
	hud.startPxcEventFwd(hud.run, pxc, ready)

	payload := json.RawMessage(`{"HUName":"MTX800"}`)
	pxc.Events <- net.PXCResponse{Command: net.PxcHudConf, Body: payload}
	pxc.Events <- net.PXCResponse{Command: net.PxcHeartbeatOk}

	if event := <-hud.Events(); event.Source != EventSourcePXC ||
		event.Cmd != int(net.PxcHudConf) ||
		string(event.Data.(json.RawMessage)) != string(payload) {
		t.Fatalf("unexpected PXC config event: %+v", event)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("PXC heartbeat acknowledgement did not signal readiness")
	}
	if event := <-hud.Events(); event.Cmd != int(net.PxcHeartbeatOk) {
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

	hud.run.event(HudEvent{
		Source: EventSourceTransport,
		Cmd:    TransportCmdVideoFraming,
		Data:   []byte{0, 1},
	})

	event := <-hud.Events()
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

// A run that has stopped reports into its own channels and can stop only itself: whatever its
// servers still have to say, or a fatal error they raise late, never reaches the next run.
func TestAStoppedRunCannotReachTheNext(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	if hud.Rearm() {
		t.Fatal("Rearm replaced a run that had not stopped")
	}
	old := hud.run
	if err := hud.StopStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hud.Rearm() {
		t.Fatal("Rearm kept a stopped run")
	}
	old.event(HudEvent{Source: EventSourceTransport, Cmd: TransportCmdVideoFraming})
	hud.handleServerError(old, errors.New("late"))
	if err := hud.stopRun(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hud.Done():
		t.Fatal("the stopped run's stop ended the next run")
	case e := <-hud.Events():
		t.Fatalf("the next run heard the stopped run's event %+v", e)
	case err := <-hud.Errors():
		t.Fatalf("the next run heard the stopped run's error %v", err)
	default:
	}
}

// A stop that arrives while another is taking the run down waits for it instead of ending the run
// early: ending it would let the next run open while the first stop's servers still hold the ports.
// A start on that run is refused the same way.
func TestASecondStopWaitsForTheFirst(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	hud.run.stopping = true // the first stop, still shutting the servers down
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := hud.StopStream(ctx); err != nil {
		t.Fatal(err)
	}
	if hud.run.isStopped() || hud.Rearm() {
		t.Fatal("a second stop ended the run before the first had")
	}
	if err := hud.StartStream(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("a start on a run being stopped returned %v", err)
	}
}

// stuckServer's Stop gives up when its bound does and finishes only once released.
type stuckServer struct{ release chan struct{} }

func (s *stuckServer) Start() error { return nil }
func (s *stuckServer) Stop(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

// A stop returns at its bound even when a server is still unwinding, but that server shares the
// video queue, the capture config and the MCU-Lite state with the next run: the next start waits.
func TestTheNextStartWaitsForAServerStillStopping(t *testing.T) {
	hud := NewCfmotoHUD(30, &stream.MuxSource{}, 0)
	stuck := &stuckServer{release: make(chan struct{})}
	spent, cancel := context.WithCancel(context.Background())
	cancel() // the stop's bound, already over
	hud.unwind(spent, []StoppableServer{stuck})

	listen := func() {
		var ls [3]stdnet.Listener
		for i := range ls {
			ln, err := stdnet.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			ls[i] = ln
		}
		if err := hud.SetListeners(ls[0], ls[1], ls[2]); err != nil {
			t.Fatal(err)
		}
	}
	listen()
	ctx, cancelStart := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelStart()
	err := hud.StartStreamOverLink(ctx, func() error {
		t.Error("started beside a server still stopping")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start beside a stopping server returned %v", err)
	}

	settle := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		return hud.Settled(ctx)
	}
	if err := settle(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settled beside a server still stopping: %v", err)
	}

	close(stuck.release)
	listen()
	stop := errors.New("got past the wait")
	if err := hud.StartStreamOverLink(context.Background(), func() error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("start after the server stopped returned %v", err)
	}

	// A start in flight is not settled either, nor are the servers its failure leaves stopping.
	listen()
	release := make(chan struct{})
	go func() { _ = hud.StartStreamOverLink(context.Background(), func() error { <-release; return stop }) }()
	for registered := false; !registered; time.Sleep(time.Millisecond) { // until the start has registered
		hud.mu.Lock()
		registered = hud.startDone != nil
		hud.mu.Unlock()
	}
	if err := settle(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settled beside a start in flight: %v", err)
	}
	close(release)
	if err := hud.Settled(context.Background()); err != nil {
		t.Fatal(err)
	}
}
