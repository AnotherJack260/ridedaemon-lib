package api

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	stdnet "net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

type eventCaptureCallback struct {
	events chan []byte
}

func (c *eventCaptureCallback) OnError(string, bool) {}

func (c *eventCaptureCallback) OnEvent(_ int64, _ int, _ int, payload []byte) {
	c.events <- payload
}

func (c *eventCaptureCallback) OnStopped() {}

func TestBuildAnnexBAUFromAVCCRejectsTruncatedNAL(t *testing.T) {
	_, err := BuildAnnexBAUFromAVCC([]byte{0, 0, 0, 4, 0x65})
	if err == nil {
		t.Fatal("BuildAnnexBAUFromAVCC() accepted a truncated NAL")
	}
}

func TestNewMobileSessionAllowsLiveOnlyMode(t *testing.T) {
	config := NewMobileConfig(nil, 30, 10, 5, 10, 3)
	session, err := NewMobileSession(config, nil)
	if err != nil {
		t.Fatalf("create live-only mobile session: %v", err)
	}
	defer session.StopSession()
	if session.mux.NoSignal != nil {
		t.Fatal("live-only session unexpectedly has a static fallback")
	}
}

// A PXC body arrives as json.RawMessage. Relaying it through a plain []byte
// type assertion silently dropped it - see relayEvent - so this pins the shape
// the phone actually receives.
func TestRelayEventCopiesJSONRawMessagePayload(t *testing.T) {
	callback := &eventCaptureCallback{events: make(chan []byte, 1)}
	session := &MobileSession{cb: callback}
	payload := json.RawMessage(`{"HUName":"CFDL26"}`)

	session.relayEvent(core.HudEvent{
		Source: core.EventSourcePXC,
		Cmd:    65552,
		Data:   payload,
	})

	select {
	case received := <-callback.events:
		if string(received) != string(payload) {
			t.Fatalf("payload = %q, want %q", received, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("PXC event payload was not relayed")
	}
}

func TestRelayEventStillCopiesPlainByteSlicePayload(t *testing.T) {
	callback := &eventCaptureCallback{events: make(chan []byte, 1)}
	session := &MobileSession{cb: callback}
	payload := []byte{0x00, 0x04, 0xbb, 0x01}

	session.relayEvent(core.HudEvent{
		Source: core.EventSourceMediaControl,
		Cmd:    16,
		Data:   payload,
	})

	select {
	case received := <-callback.events:
		if string(received) != string(payload) {
			t.Fatalf("payload = %x, want %x", received, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("media control event payload was not relayed")
	}
}

// Before the dash has completed the handshake there is no phone channel to send on; the call
// must say so rather than block or panic.
func TestSendPhoneCommandBeforeTheSessionRunsIsAnError(t *testing.T) {
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()
	if reply, err := session.SendPhoneCommand(0x20110, []byte(`{}`), 100); err == nil || reply != nil {
		t.Fatalf("SendPhoneCommand = %+v, %v; want an error", reply, err)
	}
}

// NAL lengths a broken sample can carry, including the ones that wrap a 32-bit int.
func TestBuildAnnexBAUFromAVCCRejectsImpossibleLengths(t *testing.T) {
	for _, length := range []uint32{0, 0x7FFFFFFF, 0x80000000, 0xFFFFFFFF} {
		sample := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length), 0x65, 1, 2}
		if _, err := BuildAnnexBAUFromAVCC(sample); err == nil {
			t.Errorf("NAL length 0x%x accepted", length)
		}
	}
}

type transportCapture struct {
	commands chan int
}

func (c *transportCapture) OnError(string, bool) {}
func (c *transportCapture) OnEvent(_ int64, source int, command int, _ []byte) {
	if source == int(core.EventSourceTransport) {
		c.commands <- command
	}
}
func (c *transportCapture) OnStopped() {}

// The queue asking for a keyframe reaches the host as a transport event it can act on.
func TestAnOverflowWithNoIDRAsksTheHostForAKeyframe(t *testing.T) {
	callback := &transportCapture{commands: make(chan int, 4)}
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), callback)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()
	idr := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 0x65, 1}
	p := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 0x41, 1}
	session.mux.Live.PushFrame(idr)
	for i := 0; i < 6; i++ {
		session.mux.Live.PushFrame(p)
	}
	select {
	case command := <-callback.commands:
		if command != core.TransportCmdKeyframeNeeded {
			t.Fatalf("transport command = %d, want %d", command, core.TransportCmdKeyframeNeeded)
		}
	case <-time.After(time.Second):
		t.Fatal("the host was never asked for a keyframe")
	}
}

// fakeDash is a dash's EasyConn port (10930) on loopback.
func fakeDash(t *testing.T) stdnet.Listener {
	t.Helper()
	dash, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dash.Close() })
	_ = dash.(*stdnet.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	return dash
}

// sessionFor is a session aimed at dash, serving its reverse ports on loopback listeners whose
// addresses it returns (PXC, media control, video).
func sessionFor(t *testing.T, dash stdnet.Listener) (*MobileSession, []string) {
	t.Helper()
	// The app's own numbers: 25 s to start, 5 s to tear down.
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 25, 5, 1, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := stdnet.SplitHostPort(dash.Addr().String())
	if err := session.SetECHost(NewStreamHost(host, port, "com.cfmoto.cfmotointernational")); err != nil {
		t.Fatal(err)
	}
	return session, useEphemeralPorts(t, session)
}

// acceptInit takes the phone's EC connection and reads its whole init command.
func acceptInit(t *testing.T, dash stdnet.Listener) stdnet.Conn {
	t.Helper()
	conn, err := dash.Accept()
	if err != nil {
		t.Fatalf("the phone never dialled the EC port: %v", err)
	}
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, int(binary.LittleEndian.Uint16(header[4:]))-16)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// ecInitOK is the dash's init acknowledgement: code 17, a bare 16-byte header.
var ecInitOK = []byte{17, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

func requireRebindable(t *testing.T, addrs []string) {
	t.Helper()
	for _, addr := range addrs {
		ln, err := stdnet.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("%s still held after StopSession: %v", addr, err)
		}
		_ = ln.Close()
	}
}

// Field log, Zontes 350D, 2026-10-08: the dash took the EC connection and never answered the
// init, the rider tapped Stop, and 10920-10922 were still held 2.5 s later - StopSession saw no
// running session and returned while the start went on waiting for the dash, listeners bound,
// until its own 25 s budget ran out. A stop now ends a pending start, wherever it is waiting.
func TestStopDuringPendingStartFreesEverything(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withFd     bool // the app's path: Android dials 10930 and hands the socket over
		answerInit bool // past the EC init, waiting for the dash's PXC connection
	}{
		{name: "silent dash"},
		{name: "silent dash, socket from the host", withFd: true},
		{name: "init answered, no PXC", answerInit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			dash := fakeDash(t)
			session, ports := sessionFor(t, dash)

			started := make(chan error, 1)
			go func() {
				if !tc.withFd {
					started <- session.StartSession()
					return
				}
				conn, err := stdnet.Dial("tcp", dash.Addr().String())
				if err != nil {
					started <- err
					return
				}
				file, err := conn.(*stdnet.TCPConn).File()
				_ = conn.Close()
				if err != nil {
					started <- err
					return
				}
				fd, err := syscall.Dup(int(file.Fd())) // detachFd(): the session owns this one
				_ = file.Close()
				if err != nil {
					started <- err
					return
				}
				started <- session.StartSessionWithSocketFd(int64(fd))
			}()
			ec := acceptInit(t, dash)
			defer ec.Close()
			if tc.answerInit {
				if _, err := ec.Write(ecInitOK); err != nil {
					t.Fatal(err)
				}
			}

			stopAt := time.Now()
			if err := session.StopSession(); err != nil {
				t.Fatal(err)
			}
			requireRebindable(t, ports)
			if took := time.Since(stopAt); took > time.Second {
				t.Fatalf("StopSession took %v to free the ports", took)
			}
			select {
			case err := <-started:
				if !errors.Is(err, core.ErrStopped) {
					t.Fatalf("the pending start returned %v, want core.ErrStopped", err)
				}
			case <-time.After(time.Second):
				t.Fatal("the pending start did not return after StopSession")
			}
			_ = ec.SetReadDeadline(time.Now().Add(time.Second))
			var timeout stdnet.Error
			if _, err := ec.Read(make([]byte, 1)); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatalf("the phone's EC socket was not closed: read returned %v", err)
			}

			requireNoGoroutineLeak(t, before)
		})
	}
}

// The opposite race: StopSession issued the instant the dash completes the PXC handshake - as it
// sends the heartbeat, or as the phone acknowledges it. Either the start wins and the stop ends a
// running session, or the stop wins and the start returns ErrStopped; never a start that reports
// success to a session already stopped, ports bound. The stop is quick either way: a PXC
// connection accepted while the server stopped used to hold Stop for its whole timeout.
func TestStopAsStartCompletesLeavesNothingRunning(t *testing.T) {
	won := map[bool]int{}
	for i := 0; i < 20; i++ {
		dash := fakeDash(t)
		session, ports := sessionFor(t, dash)
		started := make(chan error, 1)
		go func() { started <- session.StartSession() }()
		ec := acceptInit(t, dash)
		if _, err := ec.Write(ecInitOK); err != nil {
			t.Fatal(err)
		}
		p, err := stdnet.Dial("tcp", ports[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write(pxc(net.PxcHeartbeat, nil)); err != nil { // what ends the start's wait
			t.Fatal(err)
		}
		if i%2 == 1 {
			_, _ = io.ReadFull(p, make([]byte, 16)) // the phone's HEARTBEAT_OK
		}
		stopAt := time.Now()
		if err := session.StopSession(); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(stopAt); took > time.Second {
			t.Fatalf("StopSession took %v", took)
		}
		select {
		case err = <-started:
		case <-time.After(time.Second):
			t.Fatal("StartSession did not return")
		}
		_ = ec.Close()
		_ = p.Close()
		if err != nil && !errors.Is(err, core.ErrStopped) {
			t.Fatalf("StartSession: %v", err)
		}
		won[err == nil]++
		if session.hud.IsRunning() {
			t.Fatal("the session is still running after StopSession")
		}
		requireRebindable(t, ports)
	}
	t.Logf("start won %d, stop won %d", won[true], won[false])
}

// requireNoGoroutineLeak waits for the goroutine count to come back to before.
func requireNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if now := runtime.NumGoroutine(); now > before {
		stacks := make([]byte, 1<<20)
		t.Fatalf("%d goroutines before, %d after:\n%s", before, now, stacks[:runtime.Stack(stacks, true)])
	}
}
