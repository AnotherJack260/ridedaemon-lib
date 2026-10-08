package net

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	stdnet "net"
	"strings"
	"testing"
	"time"
)

// openPhoneChannel gives the control the connection the dash opens with 0x20000 - the one
// CarbitRide (h0.java) reserves for everything the phone says first. The handshake's own
// reply is read off the pipe; with the notification on, mode 2 follows it on the same pipe.
func openPhoneChannel(t *testing.T, control *PXCControl) (client, server stdnet.Conn) {
	t.Helper()
	client, server = stdnet.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	if err := server.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	go func() {
		control.handleEvent(&PXCResponse{Command: PxcChannelCarData}, client)
		close(handled)
	}()
	if got := readPXCResponse(t, server); got.Command != PxcChannelCarData+1 {
		t.Fatalf("phone channel reply = 0x%x, want 0x%x", got.Command, PxcChannelCarData+1)
	}
	// The channel becomes the phone channel after its ack is on the wire, so nothing queued
	// can overtake the ack; wait for that before handing it to the test.
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("phone channel opening did not complete")
	}
	return client, server
}

func assertSilent(t *testing.T, conn stdnet.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("%s: something was written here", what)
	}
	// Later reads in the same test get the usual budget back.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
}

// Issue #12: a Zontes JCDZ21 hung up 18ms after 0x20030 arrived on the connection it was
// sending QUERY_SPEED on, 5 sessions out of 5. CarbitRide sends it on the phone channel only,
// right after answering 0x20000 (b0.r -> ec/v -> t0.q0).
func TestAppStatusGoesOutWhenThePhoneChannelOpens(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetAppStatusNotify(true)
	_, request := openControlChannel(t, control)

	_, phone := openPhoneChannel(t, control)

	got := readPXCResponse(t, phone)
	if got.Command != PxcAppStatus {
		t.Fatalf("after the phone channel reply: 0x%x, want 0x%x", got.Command, PxcAppStatus)
	}
	var body struct{ Mode int }
	if err := json.Unmarshal(got.Body, &body); err != nil || body.Mode != AppStatusBackground {
		t.Fatalf("mode = %d (%v), want %d", body.Mode, err, AppStatusBackground)
	}
	assertSilent(t, request, "the dash's request channel")
}

func TestAppStatusIsNeverWrittenOnTheDashRequestChannel(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetAppStatusNotify(true)
	reported := make(chan error, 1)
	control.OnAppStatus = func(_ int, err error) { reported <- err }
	_, request := openControlChannel(t, control)

	go control.SendAppStatus(AppStatusMirrorLive)

	assertSilent(t, request, "the dash's request channel")
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("a status with no phone channel to go on must say so")
		}
	case <-time.After(time.Second):
		t.Fatal("no report")
	}
}

func TestCheckSnResultGoesOnThePhoneChannel(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	requestClient, request := openControlChannel(t, control)
	_, phone := openPhoneChannel(t, control)

	go control.handleEvent(&PXCResponse{
		Command: PxcClientSet,
		Body:    []byte(`{"client_set":"easy_conn","sn":"9RK9"}`),
	}, requestClient)

	if got := readPXCResponse(t, request); got.Command != PxcCheckSnAck {
		t.Fatalf("on the request channel: 0x%x, want the 0x%x ack", got.Command, PxcCheckSnAck)
	}
	got := readPXCResponse(t, phone)
	if got.Command != PxcCheckSnResult {
		t.Fatalf("on the phone channel: 0x%x, want 0x%x", got.Command, PxcCheckSnResult)
	}
	var result checkSnResult
	if err := json.Unmarshal(got.Body, &result); err != nil || result.ID != "9RK9" || !result.IsOK {
		t.Fatalf("result = %+v (%v)", result, err)
	}
	assertSilent(t, request, "the dash's request channel after the ack")
}

// A dash that never opens the phone channel keeps getting the result where it always has.
func TestCheckSnResultFallsBackWhenNoPhoneChannelOpens(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.phoneChannelWait = 50 * time.Millisecond
	requestClient, request := openControlChannel(t, control)

	go control.handleEvent(&PXCResponse{
		Command: PxcClientSet,
		Body:    []byte(`{"client_set":"easy_conn","sn":"9RK9"}`),
	}, requestClient)

	if got := readPXCResponse(t, request); got.Command != PxcCheckSnAck {
		t.Fatalf("first: 0x%x, want 0x%x", got.Command, PxcCheckSnAck)
	}
	if got := readPXCResponse(t, request); got.Command != PxcCheckSnResult {
		t.Fatalf("second: 0x%x, want 0x%x", got.Command, PxcCheckSnResult)
	}
}

// Both channel openings reach the event stream: without them a rider's log cannot say
// whether the dash ever opened the phone channel, which is how this bug hid.
func TestChannelOpeningsReachTheEventStream(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	openControlChannel(t, control)
	openPhoneChannel(t, control)

	seen := map[uint32]bool{}
	for len(control.Events) > 0 {
		seen[(<-control.Events).Command] = true
	}
	if !seen[PxcHandshake] || !seen[PxcChannelCarData] {
		t.Fatalf("events = %v, want both 0x%x and 0x%x", seen, PxcHandshake, PxcChannelCarData)
	}
}

// sendAsync runs SendPhoneCommand off the test goroutine, since it blocks for the answer.
func sendAsync(control *PXCControl, cmd uint32, body []byte, timeout time.Duration) <-chan p2cResult {
	out := make(chan p2cResult, 1)
	go func() {
		reply, err := control.SendPhoneCommand(cmd, body, timeout)
		out <- p2cResult{reply: reply, err: err}
	}()
	return out
}

func awaitResult(t *testing.T, results <-chan p2cResult) p2cResult {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("SendPhoneCommand never returned")
		return p2cResult{}
	}
}

// The dash's answer comes back to the caller, whichever answer it is (x0.b(), x0.java:126-140),
// and the command never touches the dash's request channel.
func TestPhoneCommandReturnsTheDashAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply PXCResponse
	}{
		{"ack with body", PXCResponse{Command: 0x20111, Body: []byte(`{"ok":1}`)}},
		{"empty ack", PXCResponse{Command: 0x20111}},
		{"unsupported", PXCResponse{Command: PxcStatusUnsupported}},
		{"error", PXCResponse{Command: PxcStatusError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			_, request := openControlChannel(t, control)
			client, phone := openPhoneChannel(t, control)

			results := sendAsync(control, 0x20110, []byte(`{"status":0}`), 0)
			got := readPXCResponse(t, phone)
			if got.Command != 0x20110 || string(got.Body) != `{"status":0}` {
				t.Fatalf("on the phone channel: 0x%x %s", got.Command, got.Body)
			}
			reply := tc.reply
			control.handleEvent(&reply, client)

			r := awaitResult(t, results)
			if r.err != nil || r.reply == nil || r.reply.Command != tc.reply.Command ||
				string(r.reply.Body) != string(tc.reply.Body) {
				t.Fatalf("result = %+v, %v; want %+v", r.reply, r.err, tc.reply)
			}
			assertSilent(t, request, "the dash's request channel")
			// A status reply is an answer: acking it would answer an answer.
			assertSilent(t, phone, "the phone channel after the answer")
		})
	}
}

// One command at a time: the next waits for the answer to the last (x0.java:225-235).
func TestPhoneCommandsGoOutOneAtATime(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	client, phone := openPhoneChannel(t, control)

	first := sendAsync(control, 0x20110, nil, 0)
	if got := readPXCResponse(t, phone); got.Command != 0x20110 {
		t.Fatalf("first = 0x%x", got.Command)
	}
	second := sendAsync(control, 0x201E0, nil, 0)
	assertSilent(t, phone, "the phone channel before the first answer")

	// A late or stray answer to something else does not release the queue.
	control.handleEvent(&PXCResponse{Command: 0x201E1}, client)
	assertSilent(t, phone, "the phone channel after a stray answer")

	control.handleEvent(&PXCResponse{Command: 0x20111}, client)
	if r := awaitResult(t, first); r.err != nil || r.reply.Command != 0x20111 {
		t.Fatalf("first result = %+v, %v", r.reply, r.err)
	}
	if got := readPXCResponse(t, phone); got.Command != 0x201E0 {
		t.Fatalf("second = 0x%x", got.Command)
	}
	control.handleEvent(&PXCResponse{Command: 0x201E1}, client)
	if r := awaitResult(t, second); r.err != nil || r.reply.Command != 0x201E1 {
		t.Fatalf("second result = %+v, %v", r.reply, r.err)
	}
}

// CarbitRide drops a command when there is no P2C sender (z.java:188-202); it never falls
// back to the request channel, where a Zontes hangs up on a command (issue #12).
func TestPhoneCommandWithoutPhoneChannelIsRefused(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	_, request := openControlChannel(t, control)

	_, err := control.SendPhoneCommand(0x20110, nil, 0)
	if !errors.Is(err, ErrNoPhoneChannel) {
		t.Fatalf("err = %v, want ErrNoPhoneChannel", err)
	}
	assertSilent(t, request, "the dash's request channel")
}

func TestPhoneCommandTimesOutOnTheDashTimeout(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.p2cTimeout.Store(int64(50 * time.Millisecond))
	_, phone := openPhoneChannel(t, control)

	results := sendAsync(control, 0x20110, nil, 0)
	readPXCResponse(t, phone)
	if r := awaitResult(t, results); !errors.Is(r.err, ErrPhoneCommandTimeout) {
		t.Fatalf("err = %v, want ErrPhoneCommandTimeout", r.err)
	}
	// The queue moves on.
	next := sendAsync(control, 0x201E0, nil, 0)
	if got := readPXCResponse(t, phone); got.Command != 0x201E0 {
		t.Fatalf("next = 0x%x", got.Command)
	}
	awaitResult(t, next)
}

// A caller that gives up while its command is still queued must not have it sent later.
func TestAnExpiredQueuedCommandIsNeverSent(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	client, phone := openPhoneChannel(t, control)

	first := sendAsync(control, 0x20110, nil, 0)
	readPXCResponse(t, phone)
	stale := sendAsync(control, 0x201E0, nil, 20*time.Millisecond)
	if r := awaitResult(t, stale); !errors.Is(r.err, ErrPhoneCommandTimeout) {
		t.Fatalf("stale err = %v", r.err)
	}
	control.handleEvent(&PXCResponse{Command: 0x20111}, client)
	awaitResult(t, first)
	assertSilent(t, phone, "the phone channel after the caller gave up")
}

// z0.getResponseProcessType() (z0.java:44-47) and its None overrides: the dash owes these no
// answer, so nothing waits for one.
func TestCommandsTheDashOwesNoAnswer(t *testing.T) {
	for cmd, want := range map[uint32]bool{
		0x40010: true, 0x30020: true, 0x30030: true, 0x30040: true,
		0x1060020: true, 0x1060030: true, 0x1090020: true,
		0x60000000: true, 0x60000050: true, 0x60030050: true,
		PxcStatusUnsupported: true, PxcStatusError: true, 0x80000030: true, 0x70000020: true,
		PxcHeartbeat: false, PxcCheckSnResult: false, PxcAppStatus: false, 0x20110: false,
	} {
		if got := p2cNoReply(cmd); got != want {
			t.Errorf("p2cNoReply(0x%x) = %v, want %v", cmd, got, want)
		}
	}

	control := NewPXCControl(":0", nil, nil)
	_, phone := openPhoneChannel(t, control)
	results := sendAsync(control, 0x60000050, []byte{1}, 0)
	readPXCResponse(t, phone)
	if r := awaitResult(t, results); r.err != nil || r.reply != nil {
		t.Fatalf("result = %+v, %v; want nil, nil", r.reply, r.err)
	}
}

// After a Wi-Fi blip the dash opens 0x20000 again. Commands go on the new one, and the old one
// is closed the way CarbitRide replaces its sender (z.java:117-137) - it used to be whichever a
// map scan met first, which could be the dead one.
func TestTheNewestPhoneChannelWins(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	oldClient, oldPhone := openPhoneChannel(t, control)
	_, newPhone := openPhoneChannel(t, control)

	if _, err := oldPhone.Read(make([]byte, 1)); err == nil {
		t.Fatal("the replaced phone channel was not closed")
	}
	if value, _ := control.connections.Load(oldClient); !value.(*pxcConnectionState).superseded.Load() {
		t.Fatal("the replaced phone channel is not marked superseded, so its close reads as a failure")
	}

	sendAsync(control, 0x20110, nil, 0)
	if got := readPXCResponse(t, newPhone); got.Command != 0x20110 {
		t.Fatalf("on the new phone channel: 0x%x", got.Command)
	}
}

// writeDashFrame writes one PXC frame the way the dash does.
func writeDashFrame(t *testing.T, conn stdnet.Conn, cmd uint32, body []byte) {
	t.Helper()
	frame := make([]byte, pxcHeaderSize+len(body))
	size := uint32(len(frame))
	binary.LittleEndian.PutUint32(frame[0:], cmd)
	binary.LittleEndian.PutUint32(frame[4:], size)
	binary.LittleEndian.PutUint32(frame[8:], size^cmd)
	copy(frame[pxcHeaderSize:], body)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("dash write 0x%x: %v", cmd, err)
	}
}

// startBeatingSession serves a dash that opens both channels, with the keepalive on and
// collapsed to test speed. It returns the dash's ends of CAR_CTRL and CAR_DATA.
func startBeatingSession(t *testing.T) (control *PXCControl, dashCtrl, dashData stdnet.Conn) {
	t.Helper()
	control = NewPXCControl(":0", nil, nil)
	control.SetProactiveHeartbeat(true)
	control.heartbeatInterval = 10 * time.Millisecond
	control.p2cTimeout.Store(int64(150 * time.Millisecond))
	carCtrl, dashCtrl := stdnet.Pipe()
	carData, dashData := stdnet.Pipe()
	for _, c := range []stdnet.Conn{carCtrl, dashCtrl, carData, dashData} {
		c := c
		t.Cleanup(func() { _ = c.Close() })
	}
	servePXCConn(t, control, carCtrl)
	servePXCConn(t, control, carData)

	// The dash keeps its own heartbeat on CAR_CTRL, as a CarbitRide-built dash does, so ours
	// stops there and only the phone channel gets one.
	writeDashFrame(t, dashCtrl, PxcHandshake, nil)
	if got := readPXCResponse(t, dashCtrl); got.Command != PxcHandshakeOk {
		t.Fatalf("handshake ack = 0x%x", got.Command)
	}
	writeDashFrame(t, dashCtrl, PxcHeartbeat, nil)
	if got := readPXCResponse(t, dashCtrl); got.Command != PxcHeartbeatOk {
		t.Fatalf("heartbeat ack = 0x%x", got.Command)
	}
	writeDashFrame(t, dashData, PxcChannelCarData, nil)
	if got := readPXCResponse(t, dashData); got.Command != PxcChannelCarData+1 {
		t.Fatalf("phone channel ack = 0x%x", got.Command)
	}
	return control, dashCtrl, dashData
}

func expectFatal(t *testing.T, control *PXCControl, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case err := <-control.Errors:
			if fe, ok := err.(FatalError); ok && fe.IsFatal() {
				if !strings.Contains(err.Error(), "stopped answering") {
					t.Fatalf("fatal error does not say the dash stopped answering: %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("a dash that stopped answering was not declared dead")
		}
	}
}

func expectNoFatal(t *testing.T, control *PXCControl, during time.Duration) {
	t.Helper()
	deadline := time.After(during)
	for {
		select {
		case err := <-control.Errors:
			if fe, ok := err.(FatalError); ok && fe.IsFatal() {
				t.Fatalf("a live dash was declared dead: %v", err)
			}
		case <-deadline:
			return
		}
	}
}

// The heartbeat goes on the phone channel only, every idle interval, and waits for its answer
// (sdk/x0.java:192-209). Once the dash beats on its request channel itself, nothing of ours is
// written there.
func TestHeartbeatGoesOnThePhoneChannelOnly(t *testing.T) {
	_, dashCtrl, dashData := startBeatingSession(t)

	for i := 0; i < 3; i++ {
		if got := readPXCResponse(t, dashData); got.Command != PxcHeartbeat {
			t.Fatalf("phone channel beat %d = 0x%x", i, got.Command)
		}
		writeDashFrame(t, dashData, PxcHeartbeatOk, nil)
	}
	assertSilent(t, dashCtrl, "the dash's request channel once it keeps its own heartbeat")
}

// A dash that answered and then went silent everywhere is dead after its own timeout, not
// after the app's 20 s watchdog (x0.java:200-209, 263-274).
func TestADashThatStopsAnsweringIsDeclaredDead(t *testing.T) {
	control, _, dashData := startBeatingSession(t)

	readPXCResponse(t, dashData)
	writeDashFrame(t, dashData, PxcHeartbeatOk, nil)
	readPXCResponse(t, dashData) // never answered

	expectFatal(t, control, time.Second)
}

// Conservative on purpose: a dash still talking on its request channel is alive, whatever
// happened to one answer on the phone channel.
func TestADashStillTalkingElsewhereIsNotDeclaredDead(t *testing.T) {
	control, dashCtrl, dashData := startBeatingSession(t)

	readPXCResponse(t, dashData)
	writeDashFrame(t, dashData, PxcHeartbeatOk, nil)
	readPXCResponse(t, dashData) // never answered

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(40 * time.Millisecond):
			}
			frame := make([]byte, pxcHeaderSize)
			binary.LittleEndian.PutUint32(frame[0:], PxcHeartbeat)
			binary.LittleEndian.PutUint32(frame[4:], pxcHeaderSize)
			binary.LittleEndian.PutUint32(frame[8:], pxcHeaderSize^PxcHeartbeat)
			if _, err := dashCtrl.Write(frame); err != nil {
				return
			}
			if _, err := io.ReadFull(dashCtrl, frame); err != nil {
				return
			}
		}
	}()
	expectNoFatal(t, control, 500*time.Millisecond)
}

// A dash that opens 0x20000 and never reads it (the abandoned-channel case behind
// connectionError) never answered there, so its silence there proves nothing.
func TestADashThatNeverAnsweredOnThePhoneChannelIsNotDeclaredDead(t *testing.T) {
	control, _, dashData := startBeatingSession(t)

	readPXCResponse(t, dashData) // never answered
	expectNoFatal(t, control, 400*time.Millisecond)
}

// x0.a(): socketTimeoutPeriodWifi seconds, else 9 s (sdk/x0.java:86-113), floored so a dash
// cannot be judged between two of its own beats.
func TestReplyTimeoutIsTheDashOwn(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{0, 9 * time.Second},
		{-5, 9 * time.Second},
		{1, 3 * time.Second},
		{12, 12 * time.Second},
	} {
		control := NewPXCControl(":0", nil, nil)
		control.noteSocketTimeout(tc.seconds)
		if got := control.p2cReplyTimeout(); got != tc.want {
			t.Errorf("socketTimeoutPeriodWifi=%d: timeout %s, want %s", tc.seconds, got, tc.want)
		}
	}
}

type peerConn struct {
	stdnet.Conn
	addr   stdnet.Addr
	closed bool
}

func (c *peerConn) RemoteAddr() stdnet.Addr { return c.addr }
func (c *peerConn) Close() error            { c.closed = true; return nil }

func peerAt(t *testing.T, hostPort string) *peerConn {
	t.Helper()
	addr, err := stdnet.ResolveTCPAddr("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	return &peerConn{addr: addr}
}

// CarbitRide replaces its P2C sender only for a 0x20000 from the same address, and refuses one
// from anywhere else while the old one is alive (sdk/z.java:117-137).
func TestOnlyTheDashCanReplaceItsPhoneChannel(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	dash := peerAt(t, "192.168.43.10:40001")
	stranger := peerAt(t, "192.168.43.66:40002")
	dashAgain := peerAt(t, "192.168.43.10:40003")

	if !control.setPhoneChannel(dash) {
		t.Fatal("the first phone channel was refused")
	}
	if control.setPhoneChannel(stranger) || control.phoneChannel() != dash || dash.closed {
		t.Fatal("another host took over the dash's phone channel")
	}
	if !control.setPhoneChannel(dashAgain) || control.phoneChannel() != dashAgain || !dash.closed {
		t.Fatal("the dash's own new phone channel did not replace the old one")
	}
}
