package net

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	stdnet "net"
	"strings"
	"testing"
	"time"
)

func newHandshakeControl(t *testing.T) *PXCControl {
	t.Helper()
	keys, err := GenKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return NewPXCControl(":0", keys, &PhoneConfig{PxcVersion: "1.0.2"})
}

// After a Wi-Fi blip the dash reopens 0x10000 and sends CLIENT_INFO again, then waits on its
// connect page for the reply. The official app answers every one (sdk/b0.java:435-448); this
// used to answer only the first.
func TestEveryClientInfoIsAnswered(t *testing.T) {
	control := newHandshakeControl(t)
	body := json.RawMessage(`{"HUID":"JCDZ21-HUID","HUName":"JCDZ21-E42b","flavor":51,"channel":"21321","pxcVersion":"1.0.1"}`)

	var replies []string
	for i := 0; i < 2; i++ {
		got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: PxcHudConf, Body: body}, 1)[0]
		if got.Command != PxcPhoneConf {
			t.Fatalf("CLIENT_INFO #%d answered with 0x%x, want 0x%x", i+1, got.Command, PxcPhoneConf)
		}
		replies = append(replies, string(got.Body))
	}
	if replies[0] != replies[1] {
		t.Fatalf("the repeated reply differs:\n%s\n%s", replies[0], replies[1])
	}
	assertNoPXCError(t, control)
}

// The official app reads CLIENT_INFO with lenient opt* getters (ECP_C2P_CLIENT_INFO.java:
// 744-975). A field of a type this struct did not expect used to end the session right after
// the handshake; now it is coerced, or left at zero.
func TestClientInfoIsReadLeniently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		check func(*HUDConfig) bool
	}{
		{"numeric channel", `{"HUID":"x","channel":21321}`, func(c *HUDConfig) bool { return c.Channel == "21321" }},
		{"numeric version_code", `{"HUID":"x","version_code":7}`, func(c *HUDConfig) bool { return c.VersionCode == "7" }},
		{"text number", `{"HUID":"x","supportFunction":"128"}`, func(c *HUDConfig) bool { return c.SupportFunction == 128 }},
		{"fractional number", `{"HUID":"x","dpi":160.7}`, func(c *HUDConfig) bool { return c.Dpi == 160 }},
		{"text bool", `{"HUID":"x","supportBTCall":"true"}`, func(c *HUDConfig) bool { return c.SupportBTCall }},
		{"number as bool", `{"HUID":"x","supportMic":1,"HUName":"kept"}`, func(c *HUDConfig) bool { return !c.SupportMic && c.HUName == "kept" }},
		{"text clock", `{"HUID":"x","currentHUTime":"1788466877642"}`, func(c *HUDConfig) bool { return c.CurrentHUTime == 1788466877642 }},
		{"no HUID", `{"HUName":"JCDZ21-E42b"}`, func(c *HUDConfig) bool { return c.HUName == "JCDZ21-E42b" }},
		{"empty body", ``, func(c *HUDConfig) bool { return c.HUID == "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newHandshakeControl(t)
			got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: PxcHudConf, Body: json.RawMessage(tc.body)}, 1)[0]
			if got.Command != PxcPhoneConf {
				t.Fatalf("answered with 0x%x", got.Command)
			}
			assertNoPXCError(t, control)
			if !tc.check(control.HudConfig) {
				t.Fatalf("decoded %+v", control.HudConfig)
			}
		})
	}
}

// A body that is not JSON at all is still the end of the session: the official app answers it
// with an error (ECP_C2P_CLIENT_INFO.java:965-969), not a configuration.
func TestAClientInfoThatIsNotJSONIsStillFatal(t *testing.T) {
	control := newHandshakeControl(t)
	client, server := stdnet.Pipe()
	defer client.Close()
	defer server.Close()
	control.handleEvent(&PXCResponse{Command: PxcHudConf, Body: json.RawMessage(`HUID=garbage`)}, client)
	if err := readPXCError(t, control); !err.IsFatal() {
		t.Fatalf("err = %v, want fatal", err)
	}
}

// Malformed frames close the connection and say so, and nothing a dash can put in a header
// takes the process down. A declared size of 1-15 used to underflow make() and crash the app;
// a bad magic used to close in silence.
func TestMalformedPxcFramesCloseCleanly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cmd, size   uint32
		magic       uint32
		wantInError string
	}{
		{"bad magic", PxcHeartbeat, 16, 0x1234, "magic"},
		{"size under the header", PxcHeartbeat, 5, 5 ^ PxcHeartbeat, "size 5"},
		{"absurd size", PxcHeartbeat, 0xFFFFFFF0, 0xFFFFFFF0 ^ PxcHeartbeat, "size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			car, dash := stdnet.Pipe()
			defer car.Close()
			defer dash.Close()
			servePXCConn(t, control, car)

			header := make([]byte, pxcHeaderSize)
			binary.LittleEndian.PutUint32(header[0:], tc.cmd)
			binary.LittleEndian.PutUint32(header[4:], tc.size)
			binary.LittleEndian.PutUint32(header[8:], tc.magic)
			if _, err := dash.Write(header); err != nil {
				t.Fatal(err)
			}
			err := readPXCError(t, control)
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantInError)
			}
			if _, err := dash.Read(make([]byte, 1)); err == nil {
				t.Fatal("the connection was left open")
			}
		})
	}
}

// Size 0 with a matching magic has always been read as a header-only frame here, and still is.
func TestAZeroSizePxcFrameIsAnsweredAsBefore(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	car, dash := stdnet.Pipe()
	defer car.Close()
	defer dash.Close()
	servePXCConn(t, control, car)
	if err := dash.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	header := make([]byte, pxcHeaderSize)
	binary.LittleEndian.PutUint32(header[0:], PxcHeartbeat)
	binary.LittleEndian.PutUint32(header[8:], PxcHeartbeat) // 0 ^ cmd
	if _, err := dash.Write(header); err != nil {
		t.Fatal(err)
	}
	if got := readPXCResponse(t, dash); got.Command != PxcHeartbeatOk {
		t.Fatalf("answered with 0x%x", got.Command)
	}
}

type countingConn struct {
	stdnet.Conn
	writes [][]byte
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), b...))
	return len(b), nil
}

// One frame, one write, as the official app flushes it (sdk/i.java:149-163). Four header
// writes on a TCP_NODELAY socket went out as four segments.
func TestAPxcFrameIsOneWrite(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	conn := &countingConn{}
	if err := control.writeResponse(&PXCResponse{Command: 0x10431, Body: []byte(`{"status":false}`)}, conn, nil); err != nil {
		t.Fatal(err)
	}
	if len(conn.writes) != 1 {
		t.Fatalf("%d writes, want 1", len(conn.writes))
	}
	got := readPXCResponse(t, strings.NewReader(string(conn.writes[0])))
	if got.Command != 0x10431 || string(got.Body) != `{"status":false}` {
		t.Fatalf("frame = 0x%x %s", got.Command, got.Body)
	}
}

// A poll whose header declares a body has it read and dropped, as the official app does
// (base/q0.java:1055-1061); left in the stream it read as the next poll.
func TestAVideoPollBodyIsDrained(t *testing.T) {
	stream := NewMediaStream(":0", idleFrameSource{}, 0x1000, time.Millisecond)
	conn, dash := stdnet.Pipe()
	defer conn.Close()
	defer dash.Close()
	serveVideoConn(t, stream, conn)
	if err := dash.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	pollWithBody := make([]byte, mediaStepFrameSize+3)
	binary.LittleEndian.PutUint16(pollWithBody[0:], 0x0072)
	binary.LittleEndian.PutUint16(pollWithBody[2:], 3)
	plainPoll := make([]byte, mediaStepFrameSize)
	binary.LittleEndian.PutUint16(plainPoll[0:], 0x0072)

	go func() {
		_, _ = dash.Write(pollWithBody)
		_, _ = dash.Write(plainPoll)
	}()
	for i := 0; i < 2; i++ {
		idle := make([]byte, 4)
		if _, err := io.ReadFull(dash, idle); err != nil {
			t.Fatalf("poll %d unanswered: %v", i+1, err)
		}
	}
	select {
	case err := <-stream.Errors:
		t.Fatalf("the poll body was read as a command: %v", err)
	default:
	}
}

// The questions the official app answers with a fixed body (pxcCannedReplies), and the
// commands it answers with an empty cmd+1, which this answers the same way.
func TestRepliesMatchTheOfficialApp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cmd      uint32
		body     string
		wantBody string
	}{
		{"CHECK_NETWORK", 0x10410, "", `{"status":false}`},
		{"QUERY_GPS", 0x10430, "", `{"status":false}`},
		{"CHECK_APP_NETWORKING_CAPABILITY", 0x10750, "", `{"status":0}`},
		{"text to show", 0x104E0, `{"text":"hi","level":1}`, `{}`},
		// km/k0.java: the dash reports its FTP speeds and is answered with an empty ack.
		{"QUERY_SPEED", PxcSpeedConf, `{"usbSpeed":0,"wifiSpeed":2}`, ""},
		{"OTA FTP info", PxcOtaFtpInfo, `{"port":11021}`, ""},
		{"buttons", 0x10350, `{"code":4160,"type":2}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: tc.cmd, Body: json.RawMessage(tc.body)}, 1)[0]
			if got.Command != tc.cmd+1 || string(got.Body) != tc.wantBody {
				t.Fatalf("reply = 0x%x %q, want 0x%x %q", got.Command, got.Body, tc.cmd+1, tc.wantBody)
			}
			assertNoPXCError(t, control)
		})
	}
}

// Everything the owner's Zontes 350D (JCDZ21-E42b, flavor 51, channel 21321) sends on PXC,
// answered the way the official app answers it. 0x10 and 0x70 go to the media control port
// and are covered by its own tests.
func TestEveryZontesCommandIsAnsweredLikeTheOfficialApp(t *testing.T) {
	control := newHandshakeControl(t)
	control.phoneChannelWait = 10 * time.Millisecond
	for _, tc := range []struct {
		name  string
		cmd   uint32
		body  string
		reply uint32
		json  bool
	}{
		{"handshake (sdk/h0.java:104-117)", PxcHandshake, "", PxcHandshakeOk, false},
		{"phone channel (sdk/h0.java:104-117)", PxcChannelCarData, "", PxcChannelCarData + 1, false},
		{"CLIENT_INFO (ECP_C2P_CLIENT_INFO.java:1007-1058)", PxcHudConf,
			`{"HUID":"h","HUName":"JCDZ21-E42b","flavor":51,"channel":"21321","pxcVersion":"1.0.1"}`, PxcPhoneConf, true},
		{"QUERY_SPEED (km/k0.java)", PxcSpeedConf, `{"usbSpeed":0,"wifiSpeed":0}`, PxcSpeedOk, false},
		{"QUERY_TIME (km/n0.java)", PxcQueryTime, "", PxcQueryTimeAck, true},
		{"CHECK_SN (km/m.java)", PxcClientSet, `{"client_set":"easy_conn","sn":"S"}`, PxcCheckSnAck, false},
		{"heartbeat (om/d.java)", PxcHeartbeat, "", PxcHeartbeatOk, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: tc.cmd, Body: json.RawMessage(tc.body)}, 1)[0]
			if got.Command != tc.reply {
				t.Fatalf("reply = 0x%x, want 0x%x", got.Command, tc.reply)
			}
			if tc.json != json.Valid(got.Body) || (!tc.json && len(got.Body) != 0) {
				t.Fatalf("reply body = %q, want JSON=%v", got.Body, tc.json)
			}
		})
	}
}

// CAR_SUPPORT_CMDS is acked empty, or answered 0x80000020 when the body is not a JSON object,
// as km/h.java:22-36 does.
func TestCarSupportCmdsIsAnsweredLikeTheOfficialApp(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       uint32
	}{
		{"a JSON object", `{"cmds":[131344,131600]}`, PxcCarSupportCmds + 1},
		{"no body", ``, PxcCarSupportCmds + 1},
		{"not JSON", `131344,131600`, PxcStatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: PxcCarSupportCmds, Body: json.RawMessage(tc.body)}, 1)[0]
			if got.Command != tc.want || len(got.Body) != 0 {
				t.Fatalf("reply = 0x%x %q, want 0x%x", got.Command, got.Body, tc.want)
			}
		})
	}
}

// The official app answers nothing to 0x102B0 (sdk/v0.java:77-80). A flavor-51 dash gets that
// silence; every other dash keeps the cmd+1 CFDL26-style firmware waits for.
func TestTboxSerialIsAnsweredOnlyOffFlavor51(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flavor string
		optOut bool
		reply  bool
	}{
		{"Zontes, flavor 51", `51`, false, false},
		{"flavor 51, host opted out", `51`, true, true},
		{"CFMOTO-style dash", `3`, false, true},
		{"no CLIENT_INFO yet", ``, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			control.SetFlavor51Lifecycle(!tc.optOut)
			if tc.flavor != "" {
				control.HudConfig = &HUDConfig{Flavor: json.RawMessage(tc.flavor)}
			}
			client, server := stdnet.Pipe()
			defer client.Close()
			defer server.Close()
			go control.handleEvent(&PXCResponse{Command: PxcTboxSerial, Body: []byte(`{"tbox":{"sn":"S1"}}`)}, client)
			if !tc.reply {
				assertSilent(t, server, "a flavor-51 dash after 0x102B0")
				return
			}
			_ = server.SetReadDeadline(time.Now().Add(time.Second))
			if got := readPXCResponse(t, server); got.Command != PxcTboxSerial+1 || len(got.Body) != 0 {
				t.Fatalf("reply = 0x%x %q", got.Command, got.Body)
			}
		})
	}
}

// The owner's Zontes 350D (JCDZ21-E42b, flavor 51, channel 21321) end to end over TCP: what the
// phone volunteers goes on the phone channel only, one command at a time, in the official app's
// order - APPSTATUS 2 when the channel opens, CHECK_SN_RESULT, then at STREAM_START APPSTATUS 1
// and switchEc2Front, and when the rider stops switchEc2Background and STOP_SERVICE.
func TestZontesSessionGetsTheOfficialLifecycle(t *testing.T) {
	keys, err := GenKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	control := NewPXCControl("127.0.0.1:0", keys, &PhoneConfig{PxcVersion: "1.0.2"})
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = control.Stop(ctx)
	})

	ctrl := dialDash(t, control, PxcHandshake)
	writeDashFrame(t, ctrl, PxcHudConf, []byte(`{"HUID":"h","HUName":"JCDZ21-E42b","flavor":51,"channel":"21321","pxcVersion":"1.0.1"}`))
	if got := readPXCResponse(t, ctrl); got.Command != PxcPhoneConf {
		t.Fatalf("CLIENT_INFO answered 0x%x", got.Command)
	}
	data := dialDash(t, control, PxcChannelCarData)
	expect := func(cmd uint32, body string) {
		t.Helper()
		got := readPXCResponse(t, data)
		if got.Command != cmd || (body != "" && !strings.Contains(string(got.Body), body)) {
			t.Fatalf("phone channel: 0x%x %s, want 0x%x %s", got.Command, got.Body, cmd, body)
		}
		writeDashFrame(t, data, cmd+1, nil)
	}
	expect(PxcAppStatus, `"mode":2`)

	writeDashFrame(t, ctrl, PxcClientSet, []byte(`{"client_set":"easy_conn","sn":"S"}`))
	if got := readPXCResponse(t, ctrl); got.Command != PxcCheckSnAck {
		t.Fatalf("CHECK_SN answered 0x%x", got.Command)
	}
	expect(PxcCheckSnResult, `"isOk":true`)

	// What core does on STREAM_START.
	control.SendAppStatus(AppStatusMirrorLive)
	control.SwitchEcToFront()
	expect(PxcAppStatus, `"mode":1`)
	expect(PxcSwitchEcToFront, "")

	released := make(chan error, 1)
	go func() { released <- control.ReleaseDash() }()
	expect(PxcSwitchEcToBackground, "")
	expect(PxcStopService, "")
	if err := <-released; err != nil {
		t.Fatalf("ReleaseDash() = %v", err)
	}
	assertSilent(t, ctrl, "the Zontes request channel")
}
