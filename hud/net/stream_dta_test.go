package net

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var testAccessUnit = []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84}

func TestFramedPacketKeepsTheIndexByDefault(t *testing.T) {
	frame, idx := buildFramedPacket(testAccessUnit, 7, true)

	if idx != 7 {
		t.Fatalf("index = %d, want 7", idx)
	}
	if got := binary.LittleEndian.Uint32(frame[0:4]); got != uint32(len(testAccessUnit)+4) {
		t.Fatalf("length = %d, want %d", got, len(testAccessUnit)+4)
	}
	if got := binary.LittleEndian.Uint32(frame[4:8]); got != 7 {
		t.Fatalf("framed index = %d, want 7", got)
	}
	if got := string(frame[8:]); got != string(testAccessUnit) {
		t.Fatalf("access unit = % x, want % x", frame[8:], testAccessUnit)
	}
}

func TestFramedPacketDropsTheIndexWhenPlain(t *testing.T) {
	frame, _ := buildFramedPacket(testAccessUnit, 7, false)

	if got := binary.LittleEndian.Uint32(frame[0:4]); got != uint32(len(testAccessUnit)) {
		t.Fatalf("length = %d, want %d", got, len(testAccessUnit))
	}
	// The access unit must start immediately after the length, so the dash hands a
	// clean Annex-B buffer to its decoder.
	if got := string(frame[4:]); got != string(testAccessUnit) {
		t.Fatalf("access unit = % x, want % x", frame[4:], testAccessUnit)
	}
}

func TestPlainFramingIsOptIn(t *testing.T) {
	s := NewMediaStream(":0", nil, 0, 0)

	// A dash reporting supportExtendProtocol=0 changes nothing until the host says
	// this dashboard is unidentified. This is what keeps every recognised profile on
	// the exact wire format it works with today.
	if s.NegotiatedExtendedProtocol(false) {
		t.Fatal("negotiation reported plain framing without the host allowing it")
	}
	if s.plainFraming.Load() {
		t.Fatal("plain framing engaged without the host allowing it")
	}

	s.SetPlainFramingAllowed(true)
	if !s.NegotiatedExtendedProtocol(false) {
		t.Fatal("negotiation did not report plain framing for a dash reporting extend=0")
	}
	if !s.plainFraming.Load() {
		t.Fatal("plain framing did not engage for a dash reporting extend=0")
	}
}

func TestAllowedPlainFramingStillHonoursAnExtendedDash(t *testing.T) {
	s := NewMediaStream(":0", nil, 0, 0)
	s.SetPlainFramingAllowed(true)

	if s.NegotiatedExtendedProtocol(true) {
		t.Fatal("negotiation reported plain framing for a dash reporting extend=1")
	}
	if s.plainFraming.Load() {
		t.Fatal("dash reporting extend=1 was switched to plain framing")
	}
}

// Same rule as the PXC and media control listeners: one socket dying is only the end of the
// session when nothing else is being served here.
func TestAnAbandonedVideoChannelDoesNotEndASessionTheOtherIsStillServing(t *testing.T) {
	stream := NewMediaStream(":0", nil, 0x1000, time.Millisecond)
	live, dashLive := net.Pipe()
	defer live.Close()
	defer dashLive.Close()
	abandoned, dashAbandoned := net.Pipe()
	defer abandoned.Close()

	serveVideoConn(t, stream, live)
	serveVideoConn(t, stream, abandoned)
	waitForVideoConnections(t, stream, 2)

	dashAbandoned.Close()

	err := readVideoError(t, stream)
	if err.IsFatal() {
		t.Fatalf("an abandoned video channel ended a session the other was serving: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving the dash") {
		t.Fatalf("the notice a rider can be shown does not say what happened: %v", err)
	}
}

// A dash that closes the video socket while nothing else is being served has ended the session,
// and the sentence it produces is the one every earlier build produced - failures are grouped
// downstream by that text.
func TestASoleVideoChannelClosingIsStillFatal(t *testing.T) {
	stream := NewMediaStream(":0", nil, 0x1000, time.Millisecond)
	conn, dash := net.Pipe()
	defer conn.Close()

	serveVideoConn(t, stream, conn)
	waitForVideoConnections(t, stream, 1)

	dash.Close()

	err := readVideoError(t, stream)
	if !err.IsFatal() {
		t.Fatalf("the only video channel closed and the session survived it: %v", err)
	}
	if !strings.Contains(err.Error(), "error reading header: EOF") {
		t.Fatalf("the fatal sentence changed, which regroups every rider's history: %v", err)
	}
}

func TestOurOwnTeardownIsNotReportedAsAVideoFault(t *testing.T) {
	// 127.0.0.1, not ":0" - see the media control test's note.
	stream := NewMediaStream("127.0.0.1:0", nil, 0x1000, time.Millisecond)
	if err := stream.Start(); err != nil {
		t.Fatalf("could not start media stream: %v", err)
	}
	conn, err := net.Dial("tcp", stream.listener.Addr().String())
	if err != nil {
		t.Fatalf("could not reach media stream: %v", err)
	}
	defer conn.Close()
	waitForVideoConnections(t, stream, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = stream.Stop(ctx) }()

	select {
	case failure, open := <-stream.Errors:
		if open {
			t.Fatalf("our own teardown was reported as a dash fault: %v", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("media stream never finished stopping")
	}
}

func serveVideoConn(t *testing.T, stream *MediaStream, conn net.Conn) {
	t.Helper()
	stream.wg.Add(1)
	go stream.handleConn(conn)
}

func waitForVideoConnections(t *testing.T, stream *MediaStream, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for stream.tracker.Others(nil) != want {
		if time.Now().After(deadline) {
			t.Fatalf("video connections = %d, want %d", stream.tracker.Others(nil), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readVideoError(t *testing.T, stream *MediaStream) FatalError {
	t.Helper()
	select {
	case err := <-stream.Errors:
		fatal, ok := err.(FatalError)
		if !ok {
			t.Fatalf("video error does not say whether it is fatal: %v", err)
		}
		return fatal
	case <-time.After(time.Second):
		t.Fatal("no video error arrived")
		return nil
	}
}

// idleFrameSource never has anything to send, which is the state a real session is in
// between encoder frames - the point of these tests is the poll, not the payload.
type idleFrameSource struct{}

func (idleFrameSource) NextFrame(time.Time) ([]byte, error) { return nil, nil }

type pullReport struct {
	phase VideoPullPhase
	pulls uint64
}

// The whole reason this counter exists: a dash that opens the data socket, says
// nothing, and goes away is indistinguishable from a working one by every number the
// phone owns. Here it is distinguishable.
func TestASilentDashIsReportedAsNeverHavingPulledAFrame(t *testing.T) {
	stream := NewMediaStream(":0", idleFrameSource{}, 0x1000, time.Millisecond)
	reports := make(chan pullReport, 8)
	stream.OnVideoPulls = func(phase VideoPullPhase, pulls uint64) {
		reports <- pullReport{phase, pulls}
	}
	conn, dash := net.Pipe()
	defer conn.Close()

	serveVideoConn(t, stream, conn)
	waitForVideoConnections(t, stream, 1)

	if got := readPullReport(t, reports); got.phase != VideoPullSocketOpen || got.pulls != 0 {
		t.Fatalf("socket open reported as %+v", got)
	}

	dash.Close()

	if got := readPullReport(t, reports); got.phase != VideoPullSocketClosed || got.pulls != 0 {
		t.Fatalf("a dash that never asked for a frame reported %+v", got)
	}
}

func TestTheDashsFirstPullIsReported(t *testing.T) {
	stream := NewMediaStream(":0", idleFrameSource{}, 0x1000, time.Millisecond)
	reports := make(chan pullReport, 8)
	stream.OnVideoPulls = func(phase VideoPullPhase, pulls uint64) {
		reports <- pullReport{phase, pulls}
	}
	conn, dash := net.Pipe()
	defer conn.Close()
	defer dash.Close()

	serveVideoConn(t, stream, conn)
	waitForVideoConnections(t, stream, 1)
	if got := readPullReport(t, reports); got.phase != VideoPullSocketOpen {
		t.Fatalf("socket open reported as %+v", got)
	}

	poll := make([]byte, mediaStepFrameSize)
	binary.LittleEndian.PutUint16(poll[0:2], 0x0072)
	go func() {
		_, _ = dash.Write(poll)
		// Drain the idle reply so the writer does not block on the pipe.
		_, _ = io.ReadFull(dash, make([]byte, 4))
	}()

	got := readPullReport(t, reports)
	if got.phase != VideoPullFirst || got.pulls != 1 {
		t.Fatalf("the first pull reported as %+v, want phase %d count 1", got, VideoPullFirst)
	}
}

func readPullReport(t *testing.T, reports chan pullReport) pullReport {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(2 * time.Second):
		t.Fatal("no pull report arrived")
		return pullReport{}
	}
}

// queuedFrames hands out its frames in order, then nothing.
type queuedFrames struct {
	mu     sync.Mutex
	frames [][]byte
}

func (q *queuedFrames) NextFrame(time.Time) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return nil, nil
	}
	frame := q.frames[0]
	q.frames = q.frames[1:]
	return frame, nil
}

var videoPoll = []byte{0x72, 0, 0, 0, 0, 0, 0, 0}

func expectNoVideo(t *testing.T, dash net.Conn, wait time.Duration) {
	t.Helper()
	_ = dash.SetReadDeadline(time.Now().Add(wait))
	defer dash.SetReadDeadline(time.Time{})
	if n, err := dash.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the dash was sent %d byte(s) it did not ask for (err %v)", n, err)
	}
}

func readVideo(t *testing.T, dash net.Conn, n int) []byte {
	t.Helper()
	_ = dash.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer dash.SetReadDeadline(time.Time{})
	b := make([]byte, n)
	if _, err := io.ReadFull(dash, b); err != nil {
		t.Fatalf("read %d video bytes: %v", n, err)
	}
	return b
}

// The Zontes 350D (JCDZ21-E42b) polls: wantFps=0, H.264, 1024x464, supportCodec 2, 8 Mbps,
// supportExtendProtocol=0, on the GENERIC profile, which allows plain framing. Its 0x11 and every
// byte of its video are pinned here as they were before push and movie mode existed, with
// carbit-exact media control off and on: nothing before a poll, one frame per poll, idle zeros.
func TestZontesPollPathIsUnchanged(t *testing.T) {
	request := make([]byte, 32)
	binary.LittleEndian.PutUint16(request[0:], 1024)
	binary.LittleEndian.PutUint16(request[2:], 464)
	binary.LittleEndian.PutUint32(request[8:], 2)
	binary.LittleEndian.PutUint32(request[12:], 2)
	binary.LittleEndian.PutUint32(request[20:], 8388608)
	idr := []byte{0, 0, 0, 1, 0x09, 0xF0, 0, 0, 0, 1, 0x65, 0x88, 0x84}
	p := []byte{0, 0, 0, 1, 0x09, 0xF0, 0, 0, 0, 1, 0x41, 0x9A}

	for _, carbit := range []bool{false, true} {
		control := NewMediaControl(":0")
		control.CarbitExact = carbit
		stream := NewMediaStream(":0", &queuedFrames{frames: [][]byte{idr, p}}, 0x1000, 3*time.Millisecond)
		stream.SetPlainFramingAllowed(true) // GENERIC
		control.OnCaptureNegotiated = func(extended bool) { stream.NegotiatedExtendedProtocol(extended) }
		control.OnCaptureConfig = stream.SetCaptureConfig

		rec := &writeRecorder{}
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlInit, Size: 32, Payload: request}, rec)
		if got := hex.EncodeToString(append(rec.writes[0], rec.writes[1]...)); got != "1100080000000000"+"020000000004d001" {
			t.Fatalf("carbit=%v: 0x11 = %s", carbit, got)
		}

		conn, dash := net.Pipe()
		serveVideoConn(t, stream, conn)
		expectNoVideo(t, dash, 150*time.Millisecond)
		var got []byte
		for _, n := range []int{4 + len(idr), 4 + len(p), 4} {
			if _, err := dash.Write(videoPoll); err != nil {
				t.Fatal(err)
			}
			got = append(got, readVideo(t, dash, n)...)
		}
		want := "0d000000" + "00000001" + "09f0" + "00000001" + "658884" +
			"0c000000" + "00000001" + "09f0" + "00000001" + "419a" +
			"00000000"
		if hex.EncodeToString(got) != want {
			t.Fatalf("carbit=%v: video\n got %x\nwant %s", carbit, got, want)
		}
		_ = dash.Close()
		_ = conn.Close()
	}
}

// A dash that named a frame rate is written to without polling (base/q0.java:1076-1078,
// 1250-1252), a frame every 1000/fps ms, and idle zeros when there is nothing to send.
func TestPushModeWritesFramesUnaskedAtTheDashsRate(t *testing.T) {
	stream := NewMediaStream(":0", &queuedFrames{frames: [][]byte{testAccessUnit, testAccessUnit, testAccessUnit}}, 0x1000, 3*time.Millisecond)
	stream.SetCaptureConfig(CaptureConfig{WantFps: 50, Encoder: 2}) // 20 ms
	conn, dash := net.Pipe()
	defer conn.Close()
	serveVideoConn(t, stream, conn)

	start := time.Now()
	for i := 0; i < 3; i++ {
		frame := readVideo(t, dash, 8+len(testAccessUnit))
		if binary.LittleEndian.Uint32(frame[0:]) != uint32(4+len(testAccessUnit)) || binary.LittleEndian.Uint32(frame[4:]) != uint32(i) ||
			string(frame[8:]) != string(testAccessUnit) {
			t.Fatalf("pushed frame %d = % x", i, frame)
		}
	}
	if idle := readVideo(t, dash, 4); string(idle) != "\x00\x00\x00\x00" {
		t.Fatalf("idle = % x", idle)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("four writes at 50 fps took %v, want about 60 ms", elapsed)
	}

	_ = dash.Close()
	if err := readVideoError(t, stream); !err.IsFatal() {
		t.Fatalf("the only video channel closed and the session survived it: %v", err)
	}
}

// Movie mode: the first 0x72 gets a frame, then one per credit granted in a 0x72's body byte 1
// (base/q0.java:262-274, 997-1016). H.264 frames carry a u64 timestamp counted in their length
// (q0.java:651-663); JPEG does not (q0.java:515-524).
//
// Neither carries the frame index, under the default framing or with the extend protocol on: the
// length is followed straight by the picture.
func TestMovieModeSendsAFrameForTheFirstPollThenOnePerCredit(t *testing.T) {
	for _, tc := range []struct {
		encoder  int
		trailer  int
		extended bool
	}{{2, 8, false}, {1, 0, false}, {2, 8, true}, {1, 0, true}} {
		frames := [][]byte{testAccessUnit, testAccessUnit, testAccessUnit, testAccessUnit}
		stream := NewMediaStream(":0", &queuedFrames{frames: frames}, 0x1000, 3*time.Millisecond)
		if tc.extended {
			stream.SetPlainFramingAllowed(true)
			stream.NegotiatedExtendedProtocol(true)
		}
		stream.SetCaptureConfig(CaptureConfig{WantFps: 30, Encoder: tc.encoder, MovieMode: true}) // movie beats push
		conn, dash := net.Pipe()
		serveVideoConn(t, stream, conn)

		frame := func() {
			t.Helper()
			b := readVideo(t, dash, 4+len(testAccessUnit)+tc.trailer)
			if binary.LittleEndian.Uint32(b) != uint32(len(testAccessUnit)+tc.trailer) || string(b[4:4+len(testAccessUnit)]) != string(testAccessUnit) {
				t.Fatalf("encoder %d: movie frame = % x", tc.encoder, b)
			}
		}
		expectNoVideo(t, dash, 100*time.Millisecond)
		_, _ = dash.Write(videoPoll)
		frame()
		expectNoVideo(t, dash, 100*time.Millisecond)
		_, _ = dash.Write([]byte{0x72, 0, 2, 0, 0, 0, 0, 0, 0, 2}) // two credits
		frame()
		frame()
		expectNoVideo(t, dash, 100*time.Millisecond)
		_ = dash.Close()
		_ = conn.Close()
	}
}

// A JPEG is length | JPEG in every mode (base/q0.java:512-525): no frame index under the default
// framing nor with the extend protocol on, polled or pushed.
func TestJPEGFramesCarryNoIndex(t *testing.T) {
	still := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3, 0xFF, 0xD9}
	for _, tc := range []struct {
		name     string
		wantFps  int
		extended bool
	}{
		{"polled, default framing", 0, false},
		{"polled, extend protocol", 0, true},
		{"pushed, default framing", 50, false},
		{"pushed, extend protocol", 50, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := NewMediaStream(":0", &queuedFrames{frames: [][]byte{still, still}}, 0x1000, 3*time.Millisecond)
			if tc.extended {
				stream.SetPlainFramingAllowed(true)
				stream.NegotiatedExtendedProtocol(true)
			}
			stream.SetCaptureConfig(CaptureConfig{WantFps: tc.wantFps, Encoder: 1})
			conn, dash := net.Pipe()
			defer conn.Close()
			defer dash.Close()
			serveVideoConn(t, stream, conn)
			for i := 0; i < 2; i++ {
				if tc.wantFps == 0 {
					if _, err := dash.Write(videoPoll); err != nil {
						t.Fatal(err)
					}
				}
				frame := readVideo(t, dash, 4+len(still))
				if binary.LittleEndian.Uint32(frame) != uint32(len(still)) || string(frame[4:]) != string(still) {
					t.Fatalf("frame %d = % x", i, frame)
				}
			}
		})
	}
}

// Movie mode is decided as a connection starts (base/q0.java:1244-1274): a 0x60 that turns it on
// while an H.264 connection is being polled leaves that connection's frames as they were, index
// included, instead of half switching them.
func TestMovieModeTurnedOnMidConnectionLeavesItsFramesAlone(t *testing.T) {
	stream := NewMediaStream(":0", &queuedFrames{frames: [][]byte{testAccessUnit, testAccessUnit}}, 0x1000, 3*time.Millisecond)
	stream.SetCaptureConfig(CaptureConfig{Encoder: 2})
	conn, dash := net.Pipe()
	defer conn.Close()
	defer dash.Close()
	serveVideoConn(t, stream, conn)
	for i, movie := range []bool{false, true} {
		stream.SetCaptureConfig(CaptureConfig{Encoder: 2, MovieMode: movie})
		if _, err := dash.Write(videoPoll); err != nil {
			t.Fatal(err)
		}
		frame := readVideo(t, dash, 8+len(testAccessUnit))
		if binary.LittleEndian.Uint32(frame) != uint32(4+len(testAccessUnit)) || binary.LittleEndian.Uint32(frame[4:]) != uint32(i) {
			t.Fatalf("polled frame %d (movie mode %v) = % x", i, movie, frame)
		}
	}
}
