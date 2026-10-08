package net

import (
	"bytes"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// The golden bodies below are written out by hand from CarbitRide's put order and Android
// org.json's escaping ('/' as "\/"), the citations being the ones on each builder.

func TestHudInfoBody(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.FixedZone("Europe/Rome", 2*3600))
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	arrive := strconv.FormatInt(now.UnixMilli()+600_000, 10)
	cases := []struct {
		name string
		info HudInfo
		want string
	}{
		{"navigating", HudInfo{NaviIcon: 2, NextRoad: "Via Roma/Nord", RoadRemainingDistance: 350,
			DestinationRemainingDistance: 12000, DestinationRemainingTime: 600},
			`{"status":0,"carDirection":0,"cameraType":0,"cameraSpeed":0,"cameraDistance":0,"naviIcon":2,` +
				`"nextRoad":"Via Roma\/Nord","roadRemainingDistance":350,"roadRemainingTime":0,` +
				`"destinationRemainingDistance":12000,"destinationRemainingTime":600,` +
				`"arriveTimeZone":"Europe\/Rome","arriveTime":` + arrive + `,"signalIntensity":2}`},
		{"end frame", HudInfo{Status: 1},
			`{"status":1,"carDirection":0,"cameraType":0,"cameraSpeed":0,"cameraDistance":0,"naviIcon":0,` +
				`"roadRemainingDistance":0,"roadRemainingTime":0,"destinationRemainingDistance":0,` +
				`"destinationRemainingTime":0,"arriveTimeZone":"Europe\/Rome","arriveTime":` + ms + `,"signalIntensity":2}`},
	}
	for _, c := range cases {
		if got := string(BuildHudInfoBody(c.info, now, "Europe/Rome")); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	if !(HudInfo{}).Empty() || (HudInfo{NextRoad: "x"}).Empty() {
		t.Error("Empty() does not match mm/c0.a.o()")
	}
}

func TestPhoneCommandBodies(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"navi on", BuildNaviStatusBody(true), []byte(`{"status":true}`)},
		{"navi off", BuildNaviStatusBody(false), []byte(`{"status":false}`)},
		{"notification, no icon", BuildNotificationBody(nil, "WhatsApp", "Anna", "ciao <3 & a/b", "2026-10-08 12:00:00"),
			append([]byte{0x00}, `{"appName":"WhatsApp","title":"Anna","context":"ciao <3 & a\/b","dateTime":"2026-10-08 12:00:00"}`...)},
		{"notification, icon, no app name", BuildNotificationBody(png, "", "", "hi", "2026-10-08 12:00:00"),
			append([]byte{0x01, 0x01, 0x00, 0x04, 0x00, 0x00, 0x00, 0x89, 'P', 'N', 'G'},
				`{"title":"","context":"hi","dateTime":"2026-10-08 12:00:00"}`...)},
		{"bt state", BuildBtStateBody(BtStateNotConnected, "AA:BB:CC:DD:EE:FF"), []byte(`{"state":4098,"phoneBtMac":"AA:BB:CC:DD:EE:FF"}`)},
		{"bt state, no mac", BuildBtStateBody(BtStateConnected, ""), []byte(`{"state":4099,"phoneBtMac":""}`)},
		{"bt callout", BuildBtCalloutBody("Mum", "+39 333"), []byte(`{"type":0,"name":"Mum","number":"+39 333"}`)},
		{"ime start", BuildImeStartBody(1, 6, "", 0), []byte(`{"inputType":1,"imeOptions":6,"rawText":"","minLines":1,"maxLines":3,"maxLength":1024}`)},
		{"ime start, max", BuildImeStartBody(1, 3, "Mil", 40), []byte(`{"inputType":1,"imeOptions":3,"rawText":"Mil","minLines":1,"maxLines":3,"maxLength":40}`)},
		{"ime text", BuildImeTextBody("Milano"), []byte(`{"text":"Milano"}`)},
		{"ime selection", BuildImeSelectionBody(-3, -1), []byte(`{"start":0,"stop":-1}`)},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

func TestFeatureDecoders(t *testing.T) {
	cases := []struct {
		name string
		cmd  uint32
		body string
		want any // nil: no event
		err  bool
	}{
		{"navi config", PxcNaviConfig, `{"maxNaviIcon":161,"supportFunction":3}`, &NaviConfig{161, 3}, false},
		{"navi config, no max", PxcNaviConfig, `{"supportFunction":1}`, &NaviConfig{-1, 1}, false},
		{"navi config, empty", PxcNaviConfig, ``, &NaviConfig{-1, 0}, false},
		{"navi config, numeric text", PxcNaviConfig, `{"maxNaviIcon":"80"}`, &NaviConfig{80, 0}, false},
		{"navi config, junk max", PxcNaviConfig, `{"maxNaviIcon":true}`, &NaviConfig{0, 0}, false},
		{"navi config, not json", PxcNaviConfig, `nope`, nil, true},
		{"key", PxcDashButton, `{"code":4161,"type":2}`, &DashKey{0x1041, 2}, false},
		{"key, empty", PxcDashButton, ``, nil, false},
		{"key, array", PxcDashButton, `[1]`, nil, true},
		{"go home", PxcGoHome, ``, &GoHome{}, false},
		{"gate on", PxcNotificationGate, `{"enabled":1}`, &NotificationGate{true}, false},
		{"gate text", PxcNotificationGate, `{"enabled":"1"}`, &NotificationGate{true}, false},
		{"gate bool is not 1", PxcNotificationGate, `{"enabled":true}`, &NotificationGate{false}, false},
		{"gate empty", PxcNotificationGate, ``, nil, false},
		{"bt check", PxcBluetoothCheck, `{"carBtName":"ZT","carBtMac":"00:11","phoneBtName":"P","phoneBtMac":"22:33"}`,
			&BluetoothCheck{"ZT", "00:11", "P", "22:33"}, false},
		{"car bt", PxcCarBluetoothInfo, `{"name":"ZT350","address":"00:11","pin":1234}`, &CarBluetoothInfo{"ZT350", "00:11", "1234"}, false},
		{"car info", PxcCarInfo, `{"reversing":4112}`, &CarInfo{-1, 4112, -1}, false},
		{"car info, all", PxcCarInfo, `{"drivingMode":4113,"reversing":4113,"bluetooth":4130}`, &CarInfo{4113, 4113, 4130}, false},
		{"ime text", PxcCarKeyboardText, `{"text":"Mi"}`, &ImeText{"Mi"}, false},
		{"ime action", PxcCarKeyboardAction, `{"actionId":6,"keyCode":4}`, &ImeAction{6, 4}, false},
		{"ime selection", PxcImeSelection, `{"start":2,"stop":5}`, &ImeSelection{2, 5}, false},
	}
	for _, c := range cases {
		event, ok, err := DecodeFeatureCommand(c.cmd, []byte(c.body))
		if !ok || (err != nil) != c.err || !reflect.DeepEqual(event, c.want) {
			t.Errorf("%s: event %#v ok %v err %v, want %#v err %v", c.name, event, ok, err, c.want, c.err)
		}
	}
	if _, ok, _ := DecodeFeatureCommand(PxcQueryTime, nil); ok {
		t.Error("QUERY_TIME decoded as a feature command")
	}
	registered := map[uint32]bool{}
	RegisterFeatureHandlers(func(cmd uint32, handler func([]byte) ([]byte, any, error)) {
		if reply, _, _ := handler([]byte(`{}`)); reply != nil {
			t.Errorf("0x%x: reply body %q, CarbitRide answers an empty cmd+1", cmd, reply)
		}
		registered[cmd] = true
	})
	if len(registered) != len(featureDecoders) {
		t.Errorf("registered %d handlers, want %d", len(registered), len(featureDecoders))
	}
}

func TestFeatureClientInfo(t *testing.T) {
	info, err := DecodeFeatureClientInfo([]byte(`{"channel":21321,"btAddress":"00:11","bluetoothPolicy":1,` +
		`"supportFunction":1,"supportFunctionEx":4,"disablePageInRVMap":4,"supportBTCall":"true"}`))
	want := &FeatureClientInfo{Channel: "21321", BtAddress: "00:11", BluetoothPolicy: 1, SupportFunction: 5,
		DisablePageInRVMap: 4, SupportBTCall: true}
	if err != nil || !reflect.DeepEqual(info, want) {
		t.Fatalf("got %+v %v, want %+v", info, err, want)
	}
}

func TestNaviIcon(t *testing.T) {
	cases := []struct {
		kind, dir int
		cw        bool
		max, want int
	}{
		{ManeuverTurn, DirectionLeft, false, 19, 2},
		{ManeuverTurn, DirectionLeft, false, 161, 2},
		{ManeuverTurn, DirectionKeepLeft, false, 19, 4},
		{ManeuverTurn, DirectionKeepLeft, false, 80, 51},
		{ManeuverTurn, DirectionUTurnLeft, false, 19, 8},
		{ManeuverTurn, DirectionUTurnRight, false, 19, 0},
		{ManeuverTurn, DirectionUTurnRight, false, 100, 60},
		{ManeuverTurn, DirectionUnspecified, false, 19, 9},
		{ManeuverRoundabout, DirectionRight, false, 80, 55},
		{ManeuverRoundabout, DirectionRight, false, 19, 0},
		{ManeuverRoundabout, DirectionUnspecified, true, 80, 58},
		{ManeuverRoundabout, DirectionUnspecified, false, 19, 11},
		{ManeuverRoundabout, DirectionUnspecified, false, 80, 11},
		{ManeuverRoundabout, DirectionUnspecified, false, 152, 0},
		{ManeuverRoundabout, DirectionUnspecified, false, 161, 158},
		{ManeuverRoundaboutExit, DirectionUnspecified, false, 19, 12},
		{ManeuverRoundaboutExit, DirectionUnspecified, false, 161, 160},
		{ManeuverDestination, DirectionLeft, false, 19, 15},
		{ManeuverMerge, DirectionUnspecified, false, 161, 157},
		{ManeuverMerge, DirectionLeft, false, 152, 114},
		{ManeuverOnRamp, DirectionUTurnLeft, false, 19, 9}, // jadx: "case 31: break" -> 9
		{ManeuverOffRamp, DirectionSlightRight, false, 19, 5},
		{ManeuverFork, DirectionRight, false, 152, 52},
		{ManeuverUnknown, DirectionUnspecified, false, 19, 9},
		{ManeuverUnknown, DirectionUnspecified, false, 80, 61},
		{ManeuverNameChange, DirectionUnspecified, false, 19, 9},
		{ManeuverFerryTrain, DirectionUnspecified, false, 161, 161},
	}
	for _, c := range cases {
		if got := NaviIcon(c.kind, c.dir, c.cw, c.max); got != c.want {
			t.Errorf("NaviIcon(%d, %d, %v, %d) = %d, want %d", c.kind, c.dir, c.cw, c.max, got, c.want)
		}
	}
	// Every descriptor lands inside the tables, whatever the app passes.
	for kind := -1; kind <= ManeuverNameChange+1; kind++ {
		for dir := -1; dir <= DirectionUTurnRight+1; dir++ {
			for _, cw := range []bool{false, true} {
				if m := googleManeuver(kind, dir, cw); m < 0 || m > 65 {
					t.Fatalf("googleManeuver(%d, %d, %v) = %d", kind, dir, cw, m)
				}
			}
		}
	}
}

func TestRoundaboutDirectionAndDistance(t *testing.T) {
	for angle, want := range map[float64]int{
		0: DirectionSharpRight, -10: DirectionSharpRight, 90: DirectionRight, 100: DirectionRight,
		112.5: DirectionSlightRight, 180: DirectionStraight, 225: DirectionSlightLeft,
		270: DirectionLeft, 359: DirectionSharpLeft, 400: DirectionUnspecified,
	} {
		if got := RoundaboutDirection(angle); got != want {
			t.Errorf("RoundaboutDirection(%v) = %d, want %d", angle, got, want)
		}
	}
	for meters, want := range map[int]int{-5: 0, 0: 0, 1: 50, 49: 50, 50: 50, 99: 50, 100: 100, 1234: 1200} {
		if got := FloorNaviDistance(meters); got != want {
			t.Errorf("FloorNaviDistance(%d) = %d, want %d", meters, got, want)
		}
	}
}

func TestDashKeyAction(t *testing.T) {
	for code, want := range map[int]int{
		0x1040: KeyActionPlay, 0x1041: KeyActionNext, 0x1042: KeyActionPrev, 0x1043: KeyActionPause,
		0x1044: KeyActionPause, 0x1047: KeyActionPlayPause, 0x1030: KeyActionVoice, 0x1080: KeyActionBack,
		0x1090: KeyActionLandscape, 0x1091: KeyActionPortrait, 0x1092: KeyActionRotate,
		0x1010: KeyActionNone, 0x1070: KeyActionNone,
	} {
		if got := DashKeyAction(code); got != want {
			t.Errorf("DashKeyAction(0x%x) = %d, want %d", code, got, want)
		}
	}
	if !DashKeyActs(2) || !DashKeyActs(4) || DashKeyActs(1) || DashKeyActs(3) {
		t.Error("DashKeyActs must accept exactly types 2 and 4")
	}
	if !AcquireBtA2dpAllowed(0, true, false) || AcquireBtA2dpAllowed(0, false, true) || !AcquireBtA2dpAllowed(1, false, true) {
		t.Error("AcquireBtA2dpAllowed does not follow bluetoothPolicy bit 0")
	}
}
