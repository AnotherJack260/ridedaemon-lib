package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	stdnet "net"
	"strings"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

type sentCmd struct {
	cmd  uint32
	body string
}

// recorder is a DashFeatureListener that writes every call down as one line.
type recorder struct{ calls []string }

func (r *recorder) add(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}
func (r *recorder) OnNaviConfig(m int)              { r.add("navi %d", m) }
func (r *recorder) OnDashKey(code, typ, action int) { r.add("key 0x%x %d %d", code, typ, action) }
func (r *recorder) OnNotificationGate(on bool)      { r.add("gate %v", on) }
func (r *recorder) OnBluetoothCheck(a, b, c, d string) {
	r.add("btcheck %s|%s|%s|%s", a, b, c, d)
}
func (r *recorder) OnCarBluetoothInfo(a, b, c string) { r.add("carbt %s|%s|%s", a, b, c) }
func (r *recorder) OnCarInfo(a, b, c int)             { r.add("carinfo %d %d %d", a, b, c) }
func (r *recorder) OnCarKeyboardText(s string)        { r.add("ime %s", s) }
func (r *recorder) OnCarKeyboardAction(a, k int)      { r.add("imeaction %d %d", a, k) }
func (r *recorder) OnCarKeyboardSelection(a, b int)   { r.add("imesel %d %d", a, b) }
func (r *recorder) OnGoHome()                         { r.add("home") }

// newTestFeatures returns features whose commands land in *sent, answered with refuse when set
// and cmd+1 otherwise, on a clock the test moves.
func newTestFeatures(l DashFeatureListener) (*DashFeatures, *[]sentCmd, *time.Time, *int64) {
	sent := &[]sentCmd{}
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.FixedZone("Europe/Rome", 2*3600))
	refuse := new(int64)
	f := &DashFeatures{
		send: func(cmd uint32, body []byte) (*PhoneCommandReply, error) {
			*sent = append(*sent, sentCmd{cmd, string(body)})
			if *refuse != 0 {
				return &PhoneCommandReply{Command: *refuse}, nil
			}
			return &PhoneCommandReply{Command: int64(cmd + 1)}, nil
		},
		now:         func() time.Time { return clock },
		zoneID:      "Europe/Rome",
		listener:    l,
		maxNaviIcon: net.DefaultMaxNaviIcon,
	}
	return f, sent, &clock, refuse
}

func TestNaviInfoRateLimitAndIcons(t *testing.T) {
	f, sent, clock, _ := newTestFeatures(nil)
	info := &NaviInfo{ManeuverKind: ManeuverRoundabout, ManeuverDirection: DirectionRight,
		NextRoad: "SS1", DistanceToManeuver: 123, RemainingDistance: -1, RemainingTime: -1}

	steps := []struct {
		advance time.Duration
		want    bool
	}{
		{0, true},                       // first frame
		{999 * time.Millisecond, false}, // inside the second: dropped
		{time.Millisecond, true},        // 1000 ms after the last one sent
		{1500 * time.Millisecond, true},
	}
	for i, s := range steps {
		*clock = clock.Add(s.advance)
		if ok, err := f.SendNaviInfo(info); ok != s.want || err != nil {
			t.Fatalf("step %d: sent %v err %v, want %v", i, ok, err, s.want)
		}
	}
	if len(*sent) != 3 {
		t.Fatalf("%d frames on the wire, want 3", len(*sent))
	}
	// Legacy table until the dash says otherwise: a counterclockwise right exit has no icon.
	if body := (*sent)[0].body; !strings.Contains(body, `"naviIcon":0,"nextRoad":"SS1","roadRemainingDistance":100,`) ||
		!strings.Contains(body, `"destinationRemainingDistance":0,"destinationRemainingTime":0,`) {
		t.Fatalf("legacy frame = %s", body)
	}

	f.HandlePxcEvent(0, int(net.PxcNaviConfig), []byte(`{"maxNaviIcon":80}`))
	*clock = clock.Add(time.Second)
	if ok, _ := f.SendNaviInfo(info); !ok || !strings.Contains((*sent)[3].body, `"naviIcon":55,`) {
		t.Fatalf("after maxNaviIcon 80: %v %s", ok, (*sent)[3].body)
	}

	// Nothing to say (an icon the 80 table lacks, no road, no distances): skipped. An unknown
	// maneuver is not "nothing": CarbitRide draws it as icon 61 (9 on the legacy table).
	*clock = clock.Add(time.Second)
	if ok, err := f.SendNaviInfo(&NaviInfo{ManeuverKind: ManeuverRoundabout, ManeuverDirection: DirectionSharpLeft, RoundaboutClockwise: true}); ok || err != nil {
		t.Fatalf("empty frame sent: %v %v", ok, err)
	}
	if err := f.SendNaviEnd(); err != nil || !strings.HasPrefix((*sent)[4].body, `{"status":1,`) {
		t.Fatalf("end frame: %v %v", err, (*sent)[4])
	}
	if err := f.SendNaviStatus(false); err != nil || (*sent)[5] != (sentCmd{net.PxcNaviStatus, `{"status":false}`}) {
		t.Fatalf("navi status: %v %v", err, (*sent)[5])
	}
}

func TestDashKeyDebounceAndTypes(t *testing.T) {
	r := &recorder{}
	f, _, _, _ := newTestFeatures(r)
	key := func(atMs int64, code, typ int) {
		f.HandlePxcEvent(atMs, int(net.PxcDashButton), []byte(fmt.Sprintf(`{"code":%d,"type":%d}`, code, typ)))
	}
	key(10_000, 0x1041, 2) // next
	key(10_300, 0x1042, 4) // prev 300 ms later: same clock, dropped
	key(10_400, 0x1040, 2) // play: not debounced
	key(10_500, 0x1042, 2) // 500 ms after the last one through
	key(10_600, 0x1041, 3) // type 3: CarbitRide refuses it
	key(10_700, 0x1010, 2) // a code with no action still reaches the app

	want := []string{"key 0x1041 2 2", "key 0x1040 2 1", "key 0x1042 2 3", "key 0x1010 2 0"}
	if strings.Join(r.calls, ";") != strings.Join(want, ";") {
		t.Fatalf("calls %q, want %q", r.calls, want)
	}
}

func TestNotificationGate(t *testing.T) {
	r := &recorder{}
	f, sent, _, _ := newTestFeatures(r)
	n := &PhoneNotification{AppName: "WhatsApp", Title: "Anna", Text: "ciao\na tutti"}

	if ok, err := f.SendNotification(n); ok || err != nil || len(*sent) != 0 {
		t.Fatalf("sent before 0x10740: %v %v", ok, err)
	}
	f.HandlePxcEvent(0, int(net.PxcNotificationGate), []byte(`{"enabled":1}`))
	if ok, err := f.SendNotification(n); !ok || err != nil {
		t.Fatalf("not sent after 0x10740: %v %v", ok, err)
	}
	want := "\x00" + `{"appName":"WhatsApp","title":"Anna","context":"ciaoa tutti","dateTime":"2026-10-08 12:00:00"}`
	if (*sent)[0] != (sentCmd{net.PxcNotification, want}) {
		t.Fatalf("notification = %q", (*sent)[0].body)
	}
	if ok, _ := f.SendNotification(&PhoneNotification{Title: "x"}); ok {
		t.Fatal("an empty text was sent")
	}
	f.HandlePxcEvent(0, int(net.PxcNotificationGate), []byte(`{"enabled":0}`))
	if f.NotificationsEnabled() {
		t.Fatal("gate still open after enabled:0")
	}
	// Channel 33205 needs no gate.
	f.HandlePxcEvent(0, int(net.PxcHudConf), []byte(`{"channel":"33205","disablePageInRVMap":4}`))
	if !f.NotificationsEnabled() || f.DisablePageInRVMap() != DisableInRVMapMessage {
		t.Fatal("CLIENT_INFO channel 33205 / disablePageInRVMap not taken")
	}
	if strings.Join(r.calls, ";") != "gate true;gate false" {
		t.Fatalf("calls %q", r.calls)
	}
}

func TestBluetoothAndKeyboardGates(t *testing.T) {
	r := &recorder{}
	f, sent, _, refuse := newTestFeatures(r)

	if err := f.ReportBluetoothState(BtStateAdapterOff, "22:33"); err != nil {
		t.Fatal(err)
	}
	if err := f.ReportBluetoothState(BtStateConnected, "22:33"); err != nil {
		t.Fatal(err)
	}
	wantBt := []sentCmd{{net.PxcBtNotConnected, ""}, {net.PxcBtState, `{"state":4097,"phoneBtMac":"22:33"}`},
		{net.PxcBtState, `{"state":4099,"phoneBtMac":"22:33"}`}}
	if fmt.Sprint(*sent) != fmt.Sprint(wantBt) {
		t.Fatalf("bt = %v, want %v", *sent, wantBt)
	}
	*sent = nil

	// Before CLIENT_INFO: policy 0 (A2DP only), no car keyboard, no BT calls.
	if ok, _ := f.AcquireBtA2dp(false, true); ok {
		t.Fatal("A2DP grabbed over a headset link with bluetoothPolicy 0")
	}
	if ok, _ := f.StartCarKeyboard(1, 6, "", 0); ok {
		t.Fatal("car keyboard opened without supportFunction bit 0")
	}
	if ok, _ := f.SendBtCallout("Mum", "1"); ok {
		t.Fatal("BT call without supportBTCall")
	}
	f.HandlePxcEvent(0, int(net.PxcHudConf), []byte(`{"bluetoothPolicy":1,"supportFunction":1,"btAddress":"00:11"}`))
	if ok, err := f.AcquireBtA2dp(false, true); !ok || err != nil {
		t.Fatalf("A2DP not grabbed with bluetoothPolicy 1: %v", err)
	}
	for _, send := range []func() (bool, error){
		func() (bool, error) { return f.StartCarKeyboard(1, 6, "Mi", 0) },
		func() (bool, error) { return f.SendCarKeyboardText("Mi") },
		func() (bool, error) { return f.SendCarKeyboardSelection(2, 2) },
		f.EndCarKeyboard,
	} {
		if ok, err := send(); !ok || err != nil {
			t.Fatalf("car keyboard: %v %v", ok, err)
		}
	}
	wantCmds := []uint32{net.PxcAcquireBtA2dp, net.PxcImeStart, net.PxcImeText, net.PxcImeSelection, net.PxcImeEnd}
	for i, c := range *sent {
		if c.cmd != wantCmds[i] {
			t.Fatalf("command %d = 0x%x, want 0x%x", i, c.cmd, wantCmds[i])
		}
	}
	if (*sent)[0].body != `{"sendPlayEvent":false}` {
		t.Fatalf("ACQUIRE_BT_A2DP body %s", (*sent)[0].body)
	}

	// CAR_INFO bluetooth 4130 re-runs the BT check against CLIENT_INFO's btAddress.
	f.HandlePxcEvent(0, int(net.PxcCarInfo), []byte(`{"bluetooth":4130}`))
	f.HandlePxcEvent(0, int(net.PxcBluetoothCheck), []byte(`{"carBtName":"ZT","carBtMac":""}`)) // no MAC: ignored
	if strings.Join(r.calls, ";") != "carinfo -1 -1 4130;btcheck |00:11||" {
		t.Fatalf("calls %q", r.calls)
	}

	// A refusal is an error.
	*refuse = int64(net.PxcStatusUnsupported)
	if err := f.SendNaviStatus(true); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal: %v", err)
	}
}

type pxcEvent struct {
	source, cmd int
	payload     []byte
}

type eventsCallback struct{ events chan pxcEvent }

func (c eventsCallback) OnError(string, bool) {}
func (c eventsCallback) OnEvent(_ int64, source, cmd int, payload []byte) {
	c.events <- pxcEvent{source, cmd, payload}
}
func (c eventsCallback) OnStopped() {}

// awaitEvent waits for OnEvent(source, cmd); OnEvent runs on its own goroutine per event, so
// others may come first.
func awaitEvent(t *testing.T, events chan pxcEvent, source, cmd int) pxcEvent {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if e.source == source && e.cmd == cmd {
				return e
			}
		case <-timeout:
			t.Fatalf("OnEvent(%d, 0x%x) never came", source, cmd)
		}
	}
}

// A dash button, end to end through a real session: the C2P registry answers it with the empty
// cmd+1, the app still gets it from OnEvent as source PXC with its body, as before the feature
// handlers were registered, and the session hands it to its DashFeatures without the app.
func TestFeatureCommandReachesOnEventThroughTheRegistry(t *testing.T) {
	cb := eventsCallback{make(chan pxcEvent, 64)}
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 5, 2, 1, 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	useEphemeralPorts(t, session)
	keys := make(chan [3]int, 1)
	NewDashFeatures(session, keyListener{keys: keys})
	hostEnd, deviceEnd := stdnet.Pipe()
	defer hostEnd.Close()
	started := make(chan error, 1)
	go func() { started <- session.StartSessionOverUSB(pipeStream{deviceEnd}) }()

	host := newADBHost(t, hostEnd)
	host.send(0x4e584e43, 0x01000001, 256*1024, []byte("host::\x00"))
	if p := <-host.packets; p[0] != 0x4e584e43 {
		t.Fatalf("CNXN answered 0x%x", p[0])
	}
	ctrl := host.open(1, "tcp:10922")
	body := []byte(`{"code":4160,"type":2}`)
	host.send(0x45545257, 1, ctrl, pxc(net.PxcDashButton, body))
	if got := host.await(1, 16); binary.LittleEndian.Uint32(got) != net.PxcDashButton+1 || binary.LittleEndian.Uint32(got[4:]) != 16 {
		t.Fatalf("0x10350 answered %x", got)
	}
	if e := awaitEvent(t, cb.events, int(core.EventSourcePXC), int(net.PxcDashButton)); !bytes.Equal(e.payload, body) {
		t.Fatalf("OnEvent payload %q, want %q", e.payload, body)
	}
	select {
	case k := <-keys:
		if k != [3]int{0x1040, 2, KeyActionPlay} {
			t.Fatalf("OnDashKey%v", k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session never handed the button to its DashFeatures")
	}

	host.send(0x45545257, 1, ctrl, pxc(net.PxcHeartbeat, nil)) // the dash's first beat completes the start
	if err := <-started; err != nil {
		t.Fatalf("StartSessionOverUSB: %v", err)
	}
	if err := session.StopSession(); err != nil {
		t.Fatal(err)
	}
	for range host.packets {
	}
}

// keyListener reports OnDashKey; the first call waits for release when it is set.
type keyListener struct {
	noListener
	keys    chan [3]int
	release chan struct{}
}

func (l keyListener) OnDashKey(code, keyType, action int) {
	if l.release != nil {
		<-l.release
	}
	l.keys <- [3]int{code, keyType, action}
}

// The session feeds its DashFeatures the feature commands, in the dash's order, on a worker: a
// listener that blocks holds up neither relayEvent nor OnEvent, and nothing else is fed to it.
func TestRelayEventFeedsTheFeaturesInOrderWithoutBlocking(t *testing.T) {
	cb := eventsCallback{make(chan pxcEvent, 64)}
	session := &MobileSession{cb: cb}
	release, keys := make(chan struct{}), make(chan [3]int, 8)
	NewDashFeatures(session, keyListener{keys: keys, release: release})

	codes := []int{0x1040, 0x1043, 0x1047, 0x1080}
	relayed := make(chan struct{})
	go func() {
		for _, code := range codes {
			session.relayEvent(core.HudEvent{Source: core.EventSourcePXC, Cmd: int(net.PxcDashButton),
				Data: json.RawMessage(fmt.Sprintf(`{"code":%d,"type":2}`, code))})
		}
		// Not PXC: never a feature command, whatever its number.
		session.relayEvent(core.HudEvent{Source: core.EventSourceMediaControl, Cmd: int(net.PxcDashButton),
			Data: []byte(`{"code":4161,"type":2}`)})
		close(relayed)
	}()
	select {
	case <-relayed:
	case <-time.After(time.Second):
		t.Fatal("relayEvent blocked behind the feature listener")
	}
	for range codes {
		awaitEvent(t, cb.events, int(core.EventSourcePXC), int(net.PxcDashButton))
	}

	close(release)
	for i, code := range codes {
		select {
		case k := <-keys:
			if k[0] != code {
				t.Fatalf("call %d: OnDashKey(0x%x), want 0x%x", i, k[0], code)
			}
		case <-time.After(time.Second):
			t.Fatalf("call %d never came", i)
		}
	}
	select {
	case k := <-keys:
		t.Fatalf("a media-control event reached the features: %v", k)
	case <-time.After(50 * time.Millisecond):
	}
}

// naviListener reports OnNaviConfig once release is closed.
type naviListener struct {
	noListener
	icons   chan int
	release chan struct{}
}

func (l naviListener) OnNaviConfig(maxNaviIcon int) {
	<-l.release
	l.icons <- maxNaviIcon
}

func relayNaviConfig(session *MobileSession, maxNaviIcon int) {
	session.relayEvent(core.HudEvent{Source: core.EventSourcePXC, Cmd: int(net.PxcNaviConfig),
		Data: json.RawMessage(fmt.Sprintf(`{"maxNaviIcon":%d}`, maxNaviIcon))})
}

func queuedFeatureEvents(f *DashFeatures) int {
	f.queueMu.Lock()
	defer f.queueMu.Unlock()
	return len(f.queue)
}

// awaitWorkerBusy waits until the worker has taken everything queued into the listener.
func awaitWorkerBusy(t *testing.T, f *DashFeatures) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); queuedFeatureEvents(f) != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the feature worker never took its first event")
		}
	}
}

func expectIcons(t *testing.T, icons chan int, want ...int) {
	t.Helper()
	for i, w := range want {
		select {
		case got := <-icons:
			if got != w {
				t.Fatalf("call %d: OnNaviConfig(%d), want %d", i, got, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("call %d (OnNaviConfig(%d)) never came", i, w)
		}
	}
	select {
	case got := <-icons:
		t.Fatalf("OnNaviConfig(%d) after the expected calls", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// A stuck listener: heartbeats and the other PXC traffic never enter the features' queue, and the
// feature commands that do are bounded, the oldest dropped first.
func TestAStuckFeatureListenerHoldsOnlyTheNewestFeatureCommands(t *testing.T) {
	session := &MobileSession{}
	release, icons := make(chan struct{}), make(chan int, 2*maxQueuedFeatureEvents)
	f := NewDashFeatures(session, naviListener{icons: icons, release: release})
	relayNaviConfig(session, 0)
	awaitWorkerBusy(t, f)

	for i := 0; i < 500; i++ {
		session.relayEvent(core.HudEvent{Source: core.EventSourcePXC, Cmd: int(net.PxcHeartbeat)})
		session.relayEvent(core.HudEvent{Source: core.EventSourcePXC, Cmd: int(net.PxcHeartbeatOk)})
	}
	session.relayEvent(core.HudEvent{Source: core.EventSourcePXC, Cmd: 0x10410, Data: json.RawMessage(`{}`)})
	if n := queuedFeatureEvents(f); n != 0 {
		t.Fatalf("%d events that are not feature commands were queued", n)
	}
	for n := 1; n <= 100; n++ {
		relayNaviConfig(session, n)
	}
	if n := queuedFeatureEvents(f); n != maxQueuedFeatureEvents {
		t.Fatalf("%d feature commands queued, want the bound %d", n, maxQueuedFeatureEvents)
	}

	close(release)
	want := []int{0}
	for n := 100 - maxQueuedFeatureEvents + 1; n <= 100; n++ {
		want = append(want, n)
	}
	expectIcons(t, icons, want...)
}

// What a DashFeatures still has queued is discarded when NewDashFeatures replaces it and when the
// session stops; the call already running finishes.
func TestPendingFeatureCallsAreDiscardedOnReplacementAndStop(t *testing.T) {
	session := &MobileSession{}
	release, icons := make(chan struct{}), make(chan int, 8)
	old := NewDashFeatures(session, naviListener{icons: icons, release: release})
	relayNaviConfig(session, 1)
	awaitWorkerBusy(t, old)
	relayNaviConfig(session, 2)
	relayNaviConfig(session, 3)

	opened, newIcons := make(chan struct{}), make(chan int, 8)
	close(opened)
	NewDashFeatures(session, naviListener{icons: newIcons, release: opened})
	close(release)
	expectIcons(t, icons, 1)
	relayNaviConfig(session, 4)
	expectIcons(t, newIcons, 4)

	stopping, err := NewMobileSession(NewMobileConfig(nil, 30, 5, 2, 1, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	release, icons = make(chan struct{}), make(chan int, 8)
	f := NewDashFeatures(stopping, naviListener{icons: icons, release: release})
	relayNaviConfig(stopping, 5)
	awaitWorkerBusy(t, f)
	relayNaviConfig(stopping, 6)
	if err := stopping.StopSession(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); stopping.IsRunning(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session never stopped")
		}
	}
	close(release)
	expectIcons(t, icons, 5)
}
