package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
