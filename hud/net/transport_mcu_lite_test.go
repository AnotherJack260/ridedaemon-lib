package net

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	stdnet "net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Golden protobuf bytes, encoded by protobuf-java 3.25.5 from the descriptor CarbitRide embeds
// (nec/ECTinyPlus.java:32) through DynamicMessage, with the values in each test.
const (
	// ClientInfo{sdkVersion "2.1.0", protoVersion "1.0", uuid "uuid-1234", channel 21321,
	// supportFunction 7, flavor 51, pwd "pw", timeStamp 1700000000000, huid "HU1234",
	// hardwareAuthState 1}
	goldenClientInfo = "0a05322e312e301203312e301a09757569642d3132333420c9a601280730333a0270774080d095ffbc314a064855313233345001"
	// ClientInfoRly{protoVersion "1.0", phoneType ANDROID, token 0x12345678,
	// hardwarePermissionState 0, hardwarePermissionMsg ""}
	goldenClientInfoRly = "0a03312e30100020f8acd1910128003200"
	// MirrorConfig{width 486, height 248, wantFps 10, wantEncoder JEPG, quality 80, bitTate 500000,
	// screenPhysicsWidth 3.5, screenPhysicsHeight 2.0, dpi 160}
	goldenMirrorConfig = "08e60310f801180a2001285030a0c21e3d00006040450000004048a001"
	// MirrorConfigRly{width 480, height 240, fps 10, encoder JEPG}
	goldenMirrorConfigRly = "08e00310f001180a2001"
	// MirrorDataGPS{status true, latitude "45.4642", longitude "9.19", altitude "120.5",
	// speed "13.9", course "270.0", time "123456789"}
	goldenGPS = "080112053132302e351a0431332e3922053237302e302a09313233343536373839320734352e343634323a04392e3139"
	// MirrorDataWeather{adCode "110000", city "Milano", humidity "60", province "Lombardia",
	// reportTime "2026-10-08 10:00", temperature "18", weather "sunny", windDirection "N", windPower "3"}
	goldenWeather = "0a0631313030303012064d696c616e6f1a02363022094c6f6d6261726469612a10323032362d31302d30382031303a3030320231383a0573756e6e7942014e4a0133"
)

var testGPS = MCULiteGPS{Status: true, Latitude: "45.4642", Longitude: "9.19", Altitude: "120.5",
	Speed: "13.9", Course: "270.0", Time: "123456789"}

var testWeather = MCULiteWeather{AdCode: "110000", City: "Milano", Humidity: "60", Province: "Lombardia",
	ReportTime: "2026-10-08 10:00", Temperature: "18", Weather: "sunny", WindDirection: "N", WindPower: "3"}

func TestMCULiteMessagesMatchCarbit(t *testing.T) {
	if got := hex.EncodeToString(encodeMCUClientInfoRly("1.0", 0x12345678)); got != goldenClientInfoRly {
		t.Fatalf("ClientInfoRly = %s", got)
	}
	if got := hex.EncodeToString(encodeMCUGPS(testGPS)); got != goldenGPS {
		t.Fatalf("MirrorDataGPS = %s", got)
	}
	if got := hex.EncodeToString(encodeMCUWeather(testWeather)); got != goldenWeather {
		t.Fatalf("MirrorDataWeather = %s", got)
	}
	info, err := decodeMCUClientInfo(mustHex(t, goldenClientInfo))
	if err != nil {
		t.Fatal(err)
	}
	want := MCULiteClientInfo{SDKVersion: "2.1.0", ProtoVersion: "1.0", UUID: "uuid-1234", Channel: 21321,
		SupportFunction: 7, Flavor: 51, Pwd: "pw", TimeStamp: 1700000000000, HUID: "HU1234", HardwareAuthState: 1}
	if *info != want {
		t.Fatalf("ClientInfo = %+v", *info)
	}
	cfg, err := decodeMCUMirrorConfig(mustHex(t, goldenMirrorConfig))
	if err != nil {
		t.Fatal(err)
	}
	wantCfg := MCULiteMirrorConfig{Width: 480, Height: 240, Fps: 10, Encoder: 1, Quality: 80,
		BitRate: 500000, Dpi: 160, ScreenWidth: 3.5, ScreenHeight: 2}
	if *cfg != wantCfg {
		t.Fatalf("MirrorConfig = %+v", *cfg)
	}
	if got := hex.EncodeToString(encodeMCUMirrorConfigRly(cfg)); got != goldenMirrorConfigRly {
		t.Fatalf("MirrorConfigRly = %s", got)
	}
	// protobuf-java refuses a message without its required fields, and so does this.
	if _, err := decodeMCUMirrorConfig([]byte{0x08, 0x10}); err == nil {
		t.Fatal("a MirrorConfig with only a width was accepted")
	}
	if _, err := decodeMCUClientInfo(mustHex(t, goldenClientInfo)[:20]); err == nil {
		t.Fatal("a truncated ClientInfo was accepted")
	}
}

// mcuLiteDash is a fake MCU-Lite dash with both PXC channels open on a served control.
type mcuLiteDash struct {
	control      *PXCControl
	lite         *MCULite
	ctrl, phone  stdnet.Conn
	mu           sync.Mutex
	infos        []MCULiteClientInfo
	mirrors      []MCULiteMirrorConfig
	gpsDue       int
	weatherDue   int
	onGPSDue     func()
	onWeatherDue func()
}

func newMCULiteDash(t *testing.T) *mcuLiteDash {
	t.Helper()
	d := &mcuLiteDash{control: NewPXCControl(":0", nil, nil), lite: NewMCULite()}
	d.lite.OnClientInfo = func(c MCULiteClientInfo) { d.mu.Lock(); d.infos = append(d.infos, c); d.mu.Unlock() }
	d.lite.OnMirrorConfig = func(c MCULiteMirrorConfig) { d.mu.Lock(); d.mirrors = append(d.mirrors, c); d.mu.Unlock() }
	d.lite.OnGPSDue = func() {
		d.mu.Lock()
		d.gpsDue++
		d.mu.Unlock()
		if d.onGPSDue != nil {
			d.onGPSDue()
		}
	}
	d.lite.OnWeatherDue = func() {
		d.mu.Lock()
		d.weatherDue++
		d.mu.Unlock()
		if d.onWeatherDue != nil {
			d.onWeatherDue()
		}
	}
	d.control.SetMCULite(d.lite)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.control.Stop(ctx)
	})
	d.ctrl = d.open(t, MCULiteChannelC2P)
	d.phone = d.open(t, MCULiteChannelP2C)
	return d
}

// serveData hands conn to the MCU-Lite data server, as the media stream's accept loop does.
func (d *mcuLiteDash) serveData(conn stdnet.Conn) bool {
	if !d.lite.claimData(conn) {
		return false
	}
	go d.lite.serveData(conn)
	return true
}

func (d *mcuLiteDash) open(t *testing.T, channel uint32) stdnet.Conn {
	t.Helper()
	car, dash := stdnet.Pipe()
	t.Cleanup(func() { _ = dash.Close() })
	_ = dash.SetDeadline(time.Now().Add(3 * time.Second))
	servePXCConn(t, d.control, car)
	d.ask(t, dash, channel, nil, channel+1)
	return dash
}

// ask writes one request and returns the answer, which must be want.
func (d *mcuLiteDash) ask(t *testing.T, conn stdnet.Conn, cmd uint32, body []byte, want uint32) []byte {
	t.Helper()
	writeDashFrame(t, conn, cmd, body)
	got := readPXCResponse(t, conn)
	if got.Command != want {
		t.Fatalf("0x%x answered 0x%x, want 0x%x", cmd, got.Command, want)
	}
	return got.Body
}

// The MCU-Lite handshake against a fake dash: channel openings, ClientInfo, MirrorConfig, OTA
// and heartbeat, each answered as PXCForMCULite answers it.
func TestMCULiteHandshake(t *testing.T) {
	d := newMCULiteDash(t)

	rly := d.ask(t, d.ctrl, MCULiteCmdClientInfo, mustHex(t, goldenClientInfo), MCULiteCmdClientInfo+1)
	d.lite.mu.Lock()
	token := d.lite.token
	d.lite.mu.Unlock()
	if !bytes.Equal(rly, encodeMCUClientInfoRly("1.0", token)) {
		t.Fatalf("ClientInfoRly = %x (token 0x%x)", rly, token)
	}
	if token >= 1<<31-1 {
		t.Fatalf("token 0x%x is outside nextInt(Integer.MAX_VALUE)", token)
	}
	if got := hex.EncodeToString(d.ask(t, d.ctrl, MCULiteCmdStartMirror, mustHex(t, goldenMirrorConfig), MCULiteCmdStartMirror+1)); got != goldenMirrorConfigRly {
		t.Fatalf("MirrorConfigRly = %s", got)
	}
	if body := d.ask(t, d.ctrl, MCULiteCmdCheckOTA, []byte{0x0a, 0x00}, MCULiteCmdCheckOTA+1); len(body) != 0 {
		t.Fatalf("CHECK_OTA answered with %x", body)
	}
	d.ask(t, d.ctrl, MCULiteCmdStartOTA, nil, MCULiteCmdStartOTA+1)
	d.ask(t, d.ctrl, PxcHeartbeat, nil, PxcHeartbeatOk)
	d.ask(t, d.ctrl, 0x1080050, nil, PxcStatusUnsupported)
	// An unreadable ClientInfo is refused; an unreadable MirrorConfig still gets its reply, empty.
	d.ask(t, d.ctrl, MCULiteCmdClientInfo, []byte{0x0a, 0x01, 'x'}, pxcStatusRefused)
	if body := d.ask(t, d.ctrl, MCULiteCmdStartMirror, []byte{0x08, 0x10}, MCULiteCmdStartMirror+1); len(body) != 0 {
		t.Fatalf("unreadable MirrorConfig answered with %x", body)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.infos) != 1 || d.infos[0].HUID != "HU1234" || d.infos[0].SupportFunction != 7 {
		t.Fatalf("OnClientInfo saw %+v", d.infos)
	}
	if len(d.mirrors) != 1 || d.mirrors[0].Width != 480 || d.mirrors[0].Quality != 80 {
		t.Fatalf("OnMirrorConfig saw %+v", d.mirrors)
	}
	assertNoPXCError(t, d.control)
}

// readMCUFrame reads one data frame and checks its 21-byte header.
func readMCUFrame(t *testing.T, conn stdnet.Conn) (byte, []byte) {
	t.Helper()
	header := make([]byte, mcuFrameHeader)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("data frame header: %v", err)
	}
	if !bytes.Equal(header[5:], make([]byte, 16)) {
		t.Fatalf("header padding = %x", header[5:])
	}
	payload := make([]byte, binary.LittleEndian.Uint32(header[1:])-mcuFrameHeader)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("data frame payload: %v", err)
	}
	return header[0], payload
}

// The data channel: one frame per credit, GPS and weather ahead of the picture and once each,
// the still resent with its first 4 bytes XORed with the token, and the 60 s / 4 h timers due
// from the first frames.
func TestMCULiteDataChannel(t *testing.T) {
	d := newMCULiteDash(t)
	d.ask(t, d.ctrl, MCULiteCmdClientInfo, mustHex(t, goldenClientInfo), MCULiteCmdClientInfo+1) // GPS + weather
	fast := pbMessage(nil).varint(1, 64).varint(2, 32).varint(3, 500).varint(4, 1)
	d.ask(t, d.ctrl, MCULiteCmdStartMirror, fast, MCULiteCmdStartMirror+1)
	d.onGPSDue = func() { d.lite.SetGPS(testGPS) }
	d.onWeatherDue = func() { d.lite.SetWeather(testWeather) }
	still := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	d.lite.PushJPEG(still)

	car, dash := stdnet.Pipe()
	t.Cleanup(func() { _ = dash.Close() })
	_ = dash.SetDeadline(time.Now().Add(3 * time.Second))
	if !d.serveData(car) {
		t.Fatal("the data socket was not taken")
	}
	if _, err := dash.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}

	d.lite.mu.Lock()
	key := binary.LittleEndian.AppendUint32(nil, d.lite.token)
	d.lite.mu.Unlock()
	xored := append([]byte(nil), still...)
	for i := range key {
		xored[i] ^= key[i]
	}
	for i, want := range []struct {
		kind    byte
		payload string
	}{
		{mcuFrameJPEG, hex.EncodeToString(xored)}, // then GPS is due
		{mcuFrameGPS, goldenGPS},                  // then weather is due
		{mcuFrameWeather, goldenWeather},
		{mcuFrameJPEG, hex.EncodeToString(xored)},
	} {
		kind, payload := readMCUFrame(t, dash)
		if kind != want.kind || hex.EncodeToString(payload) != want.payload {
			t.Fatalf("frame %d = type %d %x, want type %d %s", i, kind, payload, want.kind, want.payload)
		}
	}
	// Four credits, four frames: nothing more until the dash grants another.
	assertSilent(t, dash, "after the credits ran out")
	d.mu.Lock()
	gps, weather := d.gpsDue, d.weatherDue
	d.mu.Unlock()
	if gps != 1 || weather != 1 {
		t.Fatalf("GPS due %d times, weather %d, want once each", gps, weather)
	}
	if _, err := dash.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if kind, _ := readMCUFrame(t, dash); kind != mcuFrameJPEG {
		t.Fatalf("after a new credit: type %d", kind)
	}
	assertNoPXCError(t, d.control)
}

// No credit for the watchdog's time ends the MCU-Lite session, PXC channels included
// (c0.java:30-40 then t0.g0()).
func TestMCULiteDataWatchdogEndsTheSession(t *testing.T) {
	d := newMCULiteDash(t)
	d.ask(t, d.ctrl, MCULiteCmdStartMirror, mustHex(t, goldenMirrorConfig), MCULiteCmdStartMirror+1)
	d.lite.watchdog = 50 * time.Millisecond
	car, dash := stdnet.Pipe()
	t.Cleanup(func() { _ = dash.Close() })
	if !d.serveData(car) {
		t.Fatal("the data socket was not taken")
	}
	err := readPXCError(t, d.control)
	if !err.IsFatal() || !strings.Contains(err.Error(), "no frame credit") {
		t.Fatalf("err = %v", err)
	}
	for name, conn := range map[string]stdnet.Conn{"request": d.ctrl, "phone": d.phone, "data": dash} {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("the %s channel was left open: %v", name, err)
		}
	}
}

// Losing the phone channel closes the request channel and ends the session (sdk/x0.java:263-289).
func TestMCULiteLosingAChannelEndsTheSession(t *testing.T) {
	d := newMCULiteDash(t)
	_ = d.phone.Close()
	for {
		err := readPXCError(t, d.control)
		if strings.Contains(err.Error(), "MCU-Lite: the dash closed MCU_LITE_P2C") {
			if !err.IsFatal() {
				t.Fatalf("not fatal: %v", err)
			}
			break
		}
	}
	_ = d.ctrl.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := d.ctrl.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the request channel was left open: %v", err)
	}
}

// The data socket is the MCU-Lite dash's only while its phone channel is up and before a
// MirrorConfig it is closed at once, as c0.run() does.
func TestMCULiteDataSocketRules(t *testing.T) {
	if NewMCULite().claimData(nil) {
		t.Fatal("taken with no MCU-Lite session")
	}
	var none *MCULite
	if none.claimData(nil) {
		t.Fatal("taken with MCU-Lite off")
	}
	d := newMCULiteDash(t)
	car, dash := stdnet.Pipe()
	defer dash.Close()
	if !d.serveData(car) {
		t.Fatal("not taken")
	}
	_ = dash.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := dash.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a data socket before MirrorConfig was left open: %v", err)
	}
}

func TestMCUAndVMAreAnsweredLikeCarbit(t *testing.T) {
	for _, tc := range []struct {
		cmd       uint32
		body      string
		want      uint32
		wantBody  string
		byDefault bool
	}{
		{0x1050010, `{"version":"1"}`, 0x1050011, `{"useVirtualMachine":false}`, false},
		{0x1050010, "", 0x1050011, `{"useVirtualMachine":false}`, false},
		{0x1050010, "nope", PxcStatusError, "", false},
		{0x105001e, `{"page":3}`, 0x105001f, "", false},
		{0x105001a, `[1]`, PxcStatusError, "", false},
		{0x1050014, `{"auto_start_vm":true}`, 0x1050015, "", false},
		{0x1050022, "", PxcStatusUnsupported, "", false},
		{0x1060010, `{"project_type":"2","protocol_ver":1}`, 0x1060011, "", false},
		{0x1060010, `{"project_type":7}`, PxcStatusError, "", false},
		{0x1060020, "\x01\x02", 0x1060021, "", false},
		{0x1060040, "", PxcStatusUnsupported, "", false},
		// The channel openings, and the other families, are the default branch's as before.
		{0x1050000, "", 0x1050001, "", true},
		{0x1060000, "", 0x1060001, "", true},
		{0x1040000, "", 0x1040001, "", true},
		{0x1010000, "", 0x1010001, "", true},
	} {
		control := NewPXCControl(":0", nil, nil)
		car, dash := stdnet.Pipe()
		servePXCConn(t, control, car)
		_ = dash.SetDeadline(time.Now().Add(2 * time.Second))
		writeDashFrame(t, dash, tc.cmd, []byte(tc.body))
		got := readPXCResponse(t, dash)
		if got.Command != tc.want || string(got.Body) != tc.wantBody {
			t.Fatalf("0x%x %q answered 0x%x %q, want 0x%x %q", tc.cmd, tc.body, got.Command, got.Body, tc.want, tc.wantBody)
		}
		if handled := control.serveOtherFamily(&PXCResponse{Command: tc.cmd, Body: []byte(tc.body)}, &countingConn{}); handled == tc.byDefault {
			t.Fatalf("0x%x: family hook handled = %v", tc.cmd, handled)
		}
		_ = dash.Close()
	}
}

// The car family's replies over a real TCP socket are the golden bytes the server wrote before
// the family hook existed, with MCU-Lite off and on. MCU-Lite frames, with it off, still get the
// old empty ack.
func TestCarFamilyBytesAreUnchangedByTheFamilyHook(t *testing.T) {
	exchanges := []struct {
		cmd  uint32
		body string
		want string
	}{
		{PxcHandshake, "", "01000100100000001100010000000000"},
		{PxcHeartbeat, "", "01000070100000001100007000000000"},
		{0x10350, `{"code":4160,"type":2}`, "51030100100000004103010000000000"},
		{0x10410, "", "110401002000000031040100000000007b22737461747573223a66616c73657d"},
		{0x103a0, "{}", "a103010010000000b103010000000000"},
	}
	for _, mcuLite := range []bool{false, true} {
		control := NewPXCControl("127.0.0.1:0", nil, nil)
		list := exchanges[:len(exchanges):len(exchanges)]
		if mcuLite {
			control.SetMCULite(NewMCULite())
		} else {
			list = append(list, struct {
				cmd  uint32
				body string
				want string
			}{MCULiteCmdClientInfo, "x", "11000801100000000100080100000000"})
		}
		if err := control.Start(); err != nil {
			t.Fatal(err)
		}
		conn, err := stdnet.Dial("tcp", control.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		for _, x := range list {
			if _, err := conn.Write(pxcFrame(x.cmd, []byte(x.body))); err != nil {
				t.Fatal(err)
			}
			want := mustHex(t, x.want)
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("MCU-Lite %v, 0x%x: %v", mcuLite, x.cmd, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("MCU-Lite %v, 0x%x answered %x, want %s", mcuLite, x.cmd, got, x.want)
			}
		}
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = control.Stop(ctx)
		cancel()
	}
}

// A dash that repeats its MCU-Lite opening on the same connection gets it acked each time, and the
// connection keeps a single watcher instead of one more goroutine per repeat.
func TestARepeatedMCULiteOpeningKeepsOneWatcher(t *testing.T) {
	d := newMCULiteDash(t)
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		d.ask(t, d.phone, MCULiteChannelP2C, nil, MCULiteChannelP2C+1)
	}
	for i := 0; i < 100; i++ { // the first closes the phone channel: a new session (h0.java:369-373)
		d.ask(t, d.ctrl, MCULiteChannelC2P, nil, MCULiteChannelC2P+1)
	}
	if grown := runtime.NumGoroutine() - before; grown > 10 {
		t.Fatalf("200 repeated openings left %d more goroutines", grown)
	}
	_ = d.ctrl.Close()
	for {
		if err := readPXCError(t, d.control); strings.Contains(err.Error(), "the dash closed MCU_LITE_C2P") {
			break
		}
	}
}

// A dash that stops reading the data socket: the frame being written cannot hold the session up.
// With no credit coming the watchdog closes the socket under the write; with credits still coming
// the write's own deadline does. Either way the session ends and the data server returns.
func TestMCULiteStalledFrameWriteEndsTheSession(t *testing.T) {
	for _, keepCrediting := range []bool{false, true} {
		d := newMCULiteDash(t)
		d.ask(t, d.ctrl, MCULiteCmdStartMirror, mustHex(t, goldenMirrorConfig), MCULiteCmdStartMirror+1)
		d.lite.watchdog = 50 * time.Millisecond
		d.lite.PushJPEG(bytes.Repeat([]byte{0xFF}, 64))
		car, dash := stdnet.Pipe()
		t.Cleanup(func() { _ = dash.Close() })
		if !d.lite.claimData(car) {
			t.Fatal("the data socket was not taken")
		}
		served := make(chan struct{})
		go func() { d.lite.serveData(car); close(served) }()
		if _, err := dash.Write([]byte{4}); err != nil {
			t.Fatal(err)
		}
		if keepCrediting {
			go func() {
				for {
					if _, err := dash.Write([]byte{4}); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
		}
		err := readPXCError(t, d.control)
		if !err.IsFatal() || !strings.Contains(err.Error(), "MCU-Lite") {
			t.Fatalf("crediting %v: err = %v", keepCrediting, err)
		}
		if keepCrediting && !strings.Contains(err.Error(), "took no frame") {
			t.Fatalf("crediting, the write ended for another reason: %v", err)
		}
		select {
		case <-served:
		case <-time.After(time.Second):
			t.Fatalf("crediting %v: the data server is still stuck in its write", keepCrediting)
		}
	}
}

// The video server's Stop closes a data socket it handed to MCU-Lite and waits for it, as it does
// its own.
func TestStoppingTheVideoServerEndsTheMCULiteData(t *testing.T) {
	control := NewPXCControl("127.0.0.1:0", nil, nil)
	lite := NewMCULite()
	control.SetMCULite(lite)
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = control.Stop(ctx)
	})
	ctrl := dialDash(t, control, MCULiteChannelC2P)
	dialDash(t, control, MCULiteChannelP2C)
	writeDashFrame(t, ctrl, MCULiteCmdStartMirror, mustHex(t, goldenMirrorConfig))
	readPXCResponse(t, ctrl)
	lite.PushJPEG([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	video := NewMediaStream("127.0.0.1:0", nil, 0, 0)
	video.SetMCULite(lite)
	if err := video.Start(); err != nil {
		t.Fatal(err)
	}
	dash, err := stdnet.Dial("tcp", video.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dash.Close()
	_ = dash.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := dash.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if kind, _ := readMCUFrame(t, dash); kind != mcuFrameJPEG {
		t.Fatalf("frame type %d", kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := video.Stop(ctx); err != nil {
		t.Fatalf("Stop did not wait for the MCU-Lite data: %v", err)
	}
	if _, err := dash.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the MCU-Lite data socket outlived the video server: %v", err)
	}
}
