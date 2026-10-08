package net

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestMediaCaptureAckUsesRequestedCFDL26Dimensions(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 720)
	binary.LittleEndian.PutUint16(request[2:4], 712)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false, false), 2, 720, 704, 0)
}

func TestMediaCaptureAckPreservesLegacyNegotiation(t *testing.T) {
	request := make([]byte, 32)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 386)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 1

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false, false), 2, 800, 384, 1)
}

func TestMediaCaptureAckFallsBackForMissingPayload(t *testing.T) {
	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(nil, false, false), 2, 800, 384, 1)
}

// The QJ 5-inch dash's exact request: 800x352, wantEncoder=2 (H264), plain framing. With the
// experiment on, the reply must say JPEG while everything else - the 16-aligned geometry the
// dash asked for, and the framing byte it declared - is echoed untouched, because those two
// are not what the experiment is changing.
func TestMediaCaptureAckOffersJpegWhenTheExperimentIsOn(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 352)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, true, false), 1, 800, 352, 0)
}

// The experiment must never reach a dashboard that did not ask for it: the same request with
// the flag off has to keep answering H.264, or every streaming dash in the fleet changes wire
// format at once.
func TestMediaCaptureAckKeepsH264WhenTheExperimentIsOff(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 352)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false, false), 2, 800, 352, 0)
}

func TestMediaStartPreparesConsumerBeforeAcknowledgement(t *testing.T) {
	control := NewMediaControl(":0")
	prepared := false
	control.OnVideoStart = func() { prepared = true }
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlChk}, server)
		close(done)
	}()

	header := make([]byte, mediaCtrlHeaderSize)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("read media start acknowledgement: %v", err)
	}
	if !prepared {
		t.Fatal("consumer was not prepared before media start acknowledgement")
	}
	if command := binary.LittleEndian.Uint16(header[0:2]); command != MediaCtrlRcv {
		t.Fatalf("media start acknowledgement command = %d, want %d", command, MediaCtrlRcv)
	}
	_ = client.Close()
	<-done
}

func TestScreenConfigReturnsConfiguredSupportFunction(t *testing.T) {
	control := NewMediaControl(":0")
	control.SupportFunction = 128
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlScreenConf}, server)
		close(done)
	}()

	header := make([]byte, mediaCtrlHeaderSize)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("read screen-config response header: %v", err)
	}
	if command := binary.LittleEndian.Uint16(header[0:2]); command != MediaCtrlViewState {
		t.Fatalf("response command = %d, want %d", command, MediaCtrlViewState)
	}
	payload := make([]byte, binary.LittleEndian.Uint16(header[2:4]))
	if _, err := io.ReadFull(client, payload); err != nil {
		t.Fatalf("read screen-config response payload: %v", err)
	}
	var view View
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode screen-config response: %v", err)
	}
	if view.SupportFunction != 128 {
		t.Fatalf("supportFunction = %d, want 128", view.SupportFunction)
	}
	<-done
}

func assertMediaCaptureAck(
	t *testing.T,
	payload []byte,
	wantEncoder uint32,
	wantWidth uint16,
	wantHeight uint16,
	wantExtended byte,
) {
	t.Helper()
	// Protocol.RlyConfigCapture.size(): 9 bytes only when the extend-protocol byte is
	// non-zero, 8 otherwise. Asserting the length here rather than in one test keeps
	// every case above honest about the shape the official app would have written.
	wantLen := 8
	if wantExtended != 0 {
		wantLen = 9
	}
	if len(payload) != wantLen {
		t.Fatalf("payload length = %d, want %d", len(payload), wantLen)
	}
	if got := binary.LittleEndian.Uint32(payload[0:4]); got != wantEncoder {
		t.Fatalf("encoder = %d, want %d", got, wantEncoder)
	}
	if got := binary.LittleEndian.Uint16(payload[4:6]); got != wantWidth {
		t.Fatalf("width = %d, want %d", got, wantWidth)
	}
	if got := binary.LittleEndian.Uint16(payload[6:8]); got != wantHeight {
		t.Fatalf("height = %d, want %d", got, wantHeight)
	}
	if wantExtended != 0 {
		if got := payload[8]; got != wantExtended {
			t.Fatalf("extended protocol = %d, want %d", got, wantExtended)
		}
	}
}

// A QJ 5-inch panel declares supportExtendProtocol=0 in every CAPTURE_CONFIG. Protocol.RlyConfigCapture in the CarbitRide APK answers such a dash
// with 8 bytes and appends the framing byte only when it is non-zero; this daemon sent
// 9 either way. The trailing zero is what is being removed here, and the transport must
// still be told the framing is plain.
func TestMediaCaptureAckOmitsTheFramingByteWhenTheDashDeclaresNone(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 352)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	payload := buildMediaCaptureAckPayload(request, false, false)
	if len(payload) != 8 {
		t.Fatalf("payload length = %d, want 8", len(payload))
	}

	control := NewMediaControl(":0")
	reported := true
	called := false
	control.OnCaptureNegotiated = func(extended bool) { reported, called = extended, true }
	server, client := net.Pipe()
	go func() { io.Copy(io.Discard, client) }()
	control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlInit, Payload: request}, server)
	server.Close()
	if !called {
		t.Fatal("OnCaptureNegotiated was never called for an 8-byte reply")
	}
	if reported {
		t.Fatal("OnCaptureNegotiated reported extended framing for a dash that declared none")
	}
}

// The rule the PXC server learned the hard way, now on this listener too: the bike opens these
// reverse ports more than once and abandons the one it has nothing to say on, so the first socket
// to die here is routinely not the last one alive.
func TestAnAbandonedMediaControlChannelDoesNotEndASessionTheOtherIsStillServing(t *testing.T) {
	control := NewMediaControl(":0")
	live, dashLive := net.Pipe()
	defer live.Close()
	defer dashLive.Close()
	abandoned, dashAbandoned := net.Pipe()
	defer abandoned.Close()

	serveMediaControlConn(t, control, live)
	serveMediaControlConn(t, control, abandoned)
	waitForMediaControlConnections(t, control, 2)

	dashAbandoned.Close()

	err := readMediaControlError(t, control)
	if err.IsFatal() {
		t.Fatalf("an abandoned media control channel ended a session the other was serving: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving the dash") {
		t.Fatalf("the notice a rider can be shown does not say what happened: %v", err)
	}
	waitForMediaControlConnections(t, control, 1)
}

// The other half of the same decision: a dash that really goes away takes every channel with it,
// and that has to go on reading as the session being over.
func TestTheLastMediaControlChannelToDieEndsTheSession(t *testing.T) {
	control := NewMediaControl(":0")
	first, dashFirst := net.Pipe()
	defer first.Close()
	second, dashSecond := net.Pipe()
	defer second.Close()

	serveMediaControlConn(t, control, first)
	serveMediaControlConn(t, control, second)
	waitForMediaControlConnections(t, control, 2)

	dashFirst.Close()
	dashSecond.Close()

	one := readMediaControlError(t, control)
	two := readMediaControlError(t, control)
	if !one.IsFatal() && !two.IsFatal() {
		t.Fatalf("both media control channels died and neither ended the session: %v / %v", one, two)
	}
}

// Every dash that opens this port once keeps the behaviour it always had.
func TestASoleMediaControlChannelFailingIsStillFatal(t *testing.T) {
	control := NewMediaControl(":0")
	conn, dash := net.Pipe()
	defer conn.Close()

	serveMediaControlConn(t, control, conn)
	waitForMediaControlConnections(t, control, 1)

	dash.Close()

	if err := readMediaControlError(t, control); !err.IsFatal() {
		t.Fatalf("the only media control channel died and the session survived it: %v", err)
	}
}

// Our own teardown closes these sockets, and the read it interrupts must not come back as a fault
// of the dash's. Without the guard every stop produced a fatal "use of closed network connection"
// on :10921 - the first line of a field teardown (2026-08-26), reported ahead of the dash-side
// death that actually caused it, because each callback is relayed on its own goroutine and the two
// race. It cost an afternoon to tell which end had really let go.
func TestOurOwnTeardownIsNotReportedAsAMediaControlFault(t *testing.T) {
	// 127.0.0.1, not ":0": a wildcard listener reports its address as "[::]:port", which is not
	// a destination anything can dial.
	control := NewMediaControl("127.0.0.1:0")
	if err := control.Start(); err != nil {
		t.Fatalf("could not start media control: %v", err)
	}
	conn, err := net.Dial("tcp", control.listener.Addr().String())
	if err != nil {
		t.Fatalf("could not reach media control: %v", err)
	}
	defer conn.Close()
	waitForMediaControlConnections(t, control, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = control.Stop(ctx) }()

	select {
	case failure, open := <-control.Errors:
		if open {
			t.Fatalf("our own teardown was reported as a dash fault: %v", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("media control never finished stopping")
	}
}

func serveMediaControlConn(t *testing.T, control *MediaControl, conn net.Conn) {
	t.Helper()
	control.wg.Add(1)
	go control.handleConn(conn)
}

func waitForMediaControlConnections(t *testing.T, control *MediaControl, want int) {
	t.Helper()
	// nil is never a connection this tracker holds, so Others(nil) is the count.
	deadline := time.Now().Add(time.Second)
	for control.tracker.Others(nil) != want {
		if time.Now().After(deadline) {
			t.Fatalf("media control connections = %d, want %d", control.tracker.Others(nil), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readMediaControlError(t *testing.T, control *MediaControl) FatalError {
	t.Helper()
	select {
	case err := <-control.Errors:
		fatal, ok := err.(FatalError)
		if !ok {
			t.Fatalf("media control error does not say whether it is fatal: %v", err)
		}
		return fatal
	case <-time.After(time.Second):
		t.Fatal("no media control error arrived")
		return nil
	}
}

// writeRecorder keeps every Write as its own slice, so a test can see how a reply was cut up.
type writeRecorder struct {
	net.Conn
	writes [][]byte
}

func (w *writeRecorder) Write(b []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), b...))
	return len(b), nil
}

func TestCaptureConfigIsReadAtCarbitsOffsets(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:], 1024)
	binary.LittleEndian.PutUint16(request[2:], 464)
	binary.LittleEndian.PutUint32(request[4:], 25)
	binary.LittleEndian.PutUint32(request[8:], 2)
	binary.LittleEndian.PutUint32(request[12:], 3)
	binary.LittleEndian.PutUint16(request[16:], 10)
	binary.LittleEndian.PutUint16(request[18:], 30000)
	binary.LittleEndian.PutUint32(request[20:], 8388608)
	copy(request[24:30], []byte{8, 1, 2, 0, 5, 1})

	want := CaptureConfig{
		Width: 1024, Height: 464, WantFps: 25, WantEncoder: 2, SupportCodec: 3,
		MinQuality: 10, MaxQuality: 30000, BitRate: 8388608,
		CapScreenMode: 8, TouchMode: 1, Orientation: 2, DisplayID: 0, VideoType: 5, SupportExtendProtocol: 1,
	}
	if got := parseCaptureConfig(request); got != want {
		t.Fatalf("parsed %+v\nwant   %+v", got, want)
	}
	if got := parseCaptureConfig(request[:4]); got.Width != 1024 || got.WantFps != 0 || got.Orientation != 0 {
		t.Fatalf("a short request invented fields: %+v", got)
	}
}

// Carbit's 0x11: wantEncoder 0 is JPEG, only 1/2/4 are echoed, and the size goes to the nearest
// 16 (a remainder of exactly 8 rounds down). The default path keeps its own answers.
func TestCarbitExactCaptureAckUsesCarbitsEncoderAndRounding(t *testing.T) {
	for _, tc := range []struct {
		wantEncoder, width, height uint32
		carbit                     bool
		encoder                    uint32
		replyW, replyH             uint16
	}{
		{0, 800, 480, true, 1, 800, 480},
		{1, 800, 480, true, 1, 800, 480},
		{2, 1024, 464, true, 2, 1024, 464},
		{3, 800, 480, true, 0, 800, 480},
		{4, 800, 480, true, 4, 800, 480},
		{5, 800, 480, true, 0, 800, 480},
		{2, 730, 712, true, 2, 736, 704},
		{2, 473, 465, true, 2, 480, 464},
		{0, 800, 480, false, 2, 800, 480},
		{3, 800, 480, false, 3, 800, 480},
		{2, 730, 473, false, 2, 720, 464},
	} {
		request := make([]byte, 32)
		binary.LittleEndian.PutUint16(request[0:], uint16(tc.width))
		binary.LittleEndian.PutUint16(request[2:], uint16(tc.height))
		binary.LittleEndian.PutUint32(request[8:], tc.wantEncoder)
		assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false, tc.carbit), tc.encoder, tc.replyW, tc.replyH, 0)
	}
}

// The #9 replies, both ways. Every reply is one 8-byte header write, then the body in one write
// (sdk_communication/b.java:89-112).
func TestMediaControlRepliesWithAndWithoutCarbitExact(t *testing.T) {
	hdr := func(cmd, size uint16) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint16(b[0:], cmd)
		binary.LittleEndian.PutUint16(b[2:], size)
		return b
	}
	touch := []byte{2, 0, 100, 0, 50, 0, 0, 0, 0, 0}
	for _, tc := range []struct {
		name    string
		carbit  bool
		command uint16
		payload []byte
		want    [][]byte // each write; none = no reply
	}{
		{"default version", false, MediaCtrlGetVersion, nil, [][]byte{hdr(49, 0)}},
		{"default touch", false, MediaCtrlScreenEvent, touch, [][]byte{hdr(33, 0)}},
		{"default unknown", false, 0x99, nil, [][]byte{hdr(0x9A, 0)}},
		{"default shell", false, MediaCtrlExecuteShell, []byte("ls"), [][]byte{hdr(273, 0)}},
		{"carbit version", true, MediaCtrlGetVersion, nil, [][]byte{hdr(49, 8), {0x8A, 0x13, 0, 0, 1, 0, 0, 0}}},
		{"carbit touch", true, MediaCtrlScreenEvent, touch, nil},
		{"carbit unknown", true, 0x99, nil, [][]byte{hdr(0x7F02, 0)}},
		{"carbit model", true, MediaCtrlExecuteShell, []byte("getprop ro.product.model"), [][]byte{hdr(273, 9), []byte("Pixel 6a\x00")}},
		{"carbit release", true, MediaCtrlExecuteShell, []byte("GETPROP ro.build.version.release"), [][]byte{hdr(273, 3), []byte("36\x00")}},
		{"carbit other shell", true, MediaCtrlExecuteShell, []byte("ls"), [][]byte{hdr(273, 3), []byte("OK\x00")}},
		{"carbit ping", true, MediaCtrlPing, nil, [][]byte{hdr(65, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewMediaControl(":0")
			control.CarbitExact = tc.carbit
			control.PhoneModel, control.PhoneOsVersion = "Pixel 6a", "36"
			conn := &writeRecorder{}
			control.handleEvent(&MediaCtrlResponse{Command: tc.command, Size: uint16(len(tc.payload)), Payload: tc.payload}, conn)
			if len(conn.writes) != len(tc.want) {
				t.Fatalf("writes = % x, want % x", conn.writes, tc.want)
			}
			for i := range tc.want {
				if string(conn.writes[i]) != string(tc.want[i]) {
					t.Fatalf("write %d = % x, want % x", i, conn.writes[i], tc.want[i])
				}
			}
		})
	}
}

// Carbit's 0x61 echoes the dash's supportFunction and carries viewAreaConfig only when asked, and
// bit 0 is movie mode. The default reply keeps the profile's value, and never turns movie mode on.
func TestScreenConfigEchoAndMovieMode(t *testing.T) {
	for _, tc := range []struct {
		carbit bool
		ask    string
		reply  string
		movie  bool
	}{
		{true, `{"supportFunction":1}`, `{"supportFunction":1}`, true},
		{true, `{"supportFunction":2,"viewAreaConfig":{"initArea":0}}`, `{"viewAreaConfig":{"state":0},"supportFunction":2}`, false},
		{true, `not json`, `{"supportFunction":0}`, false},
		{false, `{"supportFunction":1}`, `{"viewAreaConfig":{"state":0},"supportFunction":128}`, false},
	} {
		control := NewMediaControl(":0")
		control.CarbitExact = tc.carbit
		control.SupportFunction = 128
		var last CaptureConfig
		control.OnCaptureConfig = func(c CaptureConfig) { last = c }
		conn := &writeRecorder{}
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlInit, Payload: make([]byte, 32)}, conn)
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlScreenConf, Payload: []byte(tc.ask)}, conn)

		if got := string(conn.writes[len(conn.writes)-1]); got != tc.reply {
			t.Errorf("carbit=%v %s: reply %s, want %s", tc.carbit, tc.ask, got, tc.reply)
		}
		if last.MovieMode != tc.movie {
			t.Errorf("carbit=%v %s: movie mode %v, want %v", tc.carbit, tc.ask, last.MovieMode, tc.movie)
		}
	}
}

// A new CAPTURE_CONFIG turns movie mode off (base/l0.java:136-138), and the host hears of it
// before the reply is on the wire, so a dash that opens 10920 at once is served in the right mode.
func TestCaptureConfigReachesTheHostBeforeTheReplyAndResetsMovieMode(t *testing.T) {
	control := NewMediaControl(":0")
	control.CarbitExact = true
	conn := &writeRecorder{}
	var last CaptureConfig
	control.OnCaptureConfig = func(c CaptureConfig) {
		if last == (CaptureConfig{}) && len(conn.writes) != 0 {
			t.Fatal("the capture config reached the host after its reply was written")
		}
		last = c
	}
	request := make([]byte, 32)
	binary.LittleEndian.PutUint16(request[0:], 1024)
	binary.LittleEndian.PutUint16(request[2:], 464)
	binary.LittleEndian.PutUint32(request[8:], 2)
	control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlInit, Payload: request}, conn)
	control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlScreenConf, Payload: []byte(`{"supportFunction":1}`)}, conn)
	if !last.MovieMode {
		t.Fatal("0x60 bit 0 did not turn movie mode on")
	}
	control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlInit, Payload: request}, conn)
	if last.MovieMode || last.Encoder != 2 || last.CaptureWidth != 1024 || last.CaptureHeight != 464 {
		t.Fatalf("after a new CAPTURE_CONFIG: %+v", last)
	}
}
