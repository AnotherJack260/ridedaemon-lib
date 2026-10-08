package net

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	stdnet "net"
	"testing"
	"time"
)

// openControlChannel gives the control the dash's request channel (0x10000, CAR_CTRL), the
// same way the dash does: by sending the handshake. The handshake's own reply is read
// off the pipe so it does not sit in front of the probe's commands.
func openControlChannel(t *testing.T, control *PXCControl) (client, server stdnet.Conn) {
	t.Helper()
	client, server = stdnet.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	if err := server.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go control.handleEvent(&PXCResponse{Command: PxcHandshake}, client)
	if got := readPXCResponse(t, server); got.Command != PxcHandshakeOk {
		t.Fatalf("handshake reply = 0x%x, want 0x%x", got.Command, PxcHandshakeOk)
	}
	return client, server
}

func TestPageSwitchProbeIsOffByDefault(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	_, server := openPhoneChannel(t, control)

	control.StartPageSwitchProbe()

	if err := server.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("page-switch probe sent something without being enabled")
	}
	assertNoPXCError(t, control)
}

func TestPageSwitchProbeSendsTheThreeCommandsInOrder(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetPageSwitchProbe(true)
	control.pageProbeSpacing = time.Millisecond

	steps := make(chan PageSwitchProbeStep, 8)
	control.OnPageSwitchProbe = func(step PageSwitchProbeStep, _ uint32, err error) {
		if err != nil {
			t.Errorf("probe step %d reported %v", step, err)
		}
		steps <- step
	}

	client, server := openPhoneChannel(t, control)
	control.StartPageSwitchProbe()

	// The probe goes through the phone command queue, which sends the next command only
	// once the dash has answered the last one; this dash answers each.
	first := readPXCResponse(t, server)
	dashAnswers(control, client, first)
	if first.Command != PxcPageStatus {
		t.Fatalf("first probe command = 0x%x, want 0x%x (PAGE_STATUS)", first.Command, PxcPageStatus)
	}
	var status struct {
		Page   int `json:"page"`
		Status int `json:"status"`
		Type   int `json:"type"`
	}
	if err := json.Unmarshal(first.Body, &status); err != nil {
		t.Fatalf("decode PAGE_STATUS body: %v", err)
	}
	if status.Page != EcpPageMirrorFloating || status.Status != ecpPageStatusOpen {
		t.Fatalf("PAGE_STATUS body = %+v, want the mirror page reported open", status)
	}

	second := readPXCResponse(t, server)
	dashAnswers(control, client, second)
	if second.Command != PxcJumpToCarPage {
		t.Fatalf("second probe command = 0x%x, want 0x%x (JUMP_TO_CAR_PAGE)", second.Command, PxcJumpToCarPage)
	}
	var jump struct {
		Page int `json:"page"`
	}
	if err := json.Unmarshal(second.Body, &jump); err != nil {
		t.Fatalf("decode JUMP_TO_CAR_PAGE body: %v", err)
	}
	if jump.Page != EcpPageMirrorFloating {
		t.Fatalf("JUMP_TO_CAR_PAGE page = %d, want %d", jump.Page, EcpPageMirrorFloating)
	}

	// The control goes last on purpose: it moves the dash AWAY from mirroring, so a
	// panel that only reacts here says the page plane works and the id is wrong.
	third := readPXCResponse(t, server)
	if third.Command != PxcSwitchToMainPage {
		t.Fatalf("third probe command = 0x%x, want 0x%x (SWITCH_TO_SYSTEM_MAIN_PAGE)", third.Command, PxcSwitchToMainPage)
	}
	if len(third.Body) != 0 {
		t.Fatalf("SWITCH_TO_SYSTEM_MAIN_PAGE body length = %d, want 0", len(third.Body))
	}

	want := []PageSwitchProbeStep{
		PageProbeStarted, PageProbePageStatus, PageProbeJumpToCarPage, PageProbeSwitchToMainPage,
	}
	for i, step := range want {
		if got := nextStep(t, steps); got != step {
			t.Fatalf("step %d = %d, want %d", i, got, step)
		}
	}
	assertNoPXCError(t, control)
}

// A dash that says STREAM_START twice must not be sent six commands.
func TestPageSwitchProbeRunsOncePerSession(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetPageSwitchProbe(true)
	control.pageProbeSpacing = time.Millisecond

	client, server := openPhoneChannel(t, control)
	control.StartPageSwitchProbe()
	control.StartPageSwitchProbe()

	for i := 0; i < 3; i++ {
		dashAnswers(control, client, readPXCResponse(t, server))
	}
	if err := server.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("second StartPageSwitchProbe ran the sequence again")
	}
}

// With no phone channel open there is nothing to send on, and saying so is the whole
// value of the event: a silent probe and an absent one look identical in a field log.
func TestPageSwitchProbeWithoutControlChannelReportsItself(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetPageSwitchProbe(true)

	steps := make(chan PageSwitchProbeStep, 8)
	control.OnPageSwitchProbe = func(step PageSwitchProbeStep, _ uint32, _ error) {
		steps <- step
	}

	control.StartPageSwitchProbe()
	if got := nextStep(t, steps); got != PageProbeNoControlChannel {
		t.Fatalf("step = %d, want PageProbeNoControlChannel", got)
	}
}

// The probe sits in the WaitGroup Stop() blocks on, and it sleeps between steps. If that
// sleep ever stops watching the quit channel, every session teardown silently grows by the
// spacing - six seconds at the shipped value - and nothing else in the suite would notice.
func TestStoppingDoesNotWaitForTheProbeToFinish(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetPageSwitchProbe(true)
	control.pageProbeSpacing = time.Minute

	_, server := openPhoneChannel(t, control)
	control.StartPageSwitchProbe()
	readPXCResponse(t, server) // step 1, then the probe parks on the long spacing

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := control.Stop(ctx); err != nil {
		t.Fatalf("Stop returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Stop took %s; the probe is not watching the quit channel", elapsed)
	}
}

func nextStep(t *testing.T, steps <-chan PageSwitchProbeStep) PageSwitchProbeStep {
	t.Helper()
	select {
	case step := <-steps:
		return step
	case <-time.After(time.Second):
		t.Fatal("probe reported no further step")
		return 0
	}
}

// dashAnswers delivers the dash's cmd+1 for a command it read, the way the connection's reader
// would.
func dashAnswers(control *PXCControl, conn stdnet.Conn, command PXCResponse) {
	control.handleEvent(&PXCResponse{Command: command.Command + 1}, conn)
}

// flavor51Control is a session whose dash said flavor 51 in CLIENT_INFO, as the Zontes JCDZ21 does.
func flavor51Control(channel string) *PXCControl {
	control := NewPXCControl(":0", nil, nil)
	control.HudConfig = &HUDConfig{Flavor: json.RawMessage(`51`), Channel: channel}
	return control
}

// The dash's lifecycle requests are acked with an empty cmd+1, as the official app acks every
// command it handles (sdk/v0.java:50-66), and each reaches the host as a typed transport event
// numbered after the request.
func TestDashLifecycleRequestsBecomeTransportEvents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmd     uint32
		body    string
		payload []byte
		event   bool
	}{
		{"dash asks to disconnect (km/u0)", PxcDashDisconnect, "", nil, true},
		{"dash UI to front (km/c1)", PxcDashUiFront, "", nil, true},
		{"dash asks for the phone home page (km/e1)", PxcDashMainPage, "", nil, true},
		{"switch to page 7 (nec/d.java:111-128)", PxcSwitchToAppPage, `{"type":7}`, []byte{0, 0, 0, 7, 0, 0, 0, 0}, true},
		{"switch to page 21 with a channel", PxcSwitchToAppPage, `{"type":"21","channel":3}`, []byte{0, 0, 0, 21, 0, 0, 0, 3}, true},
		{"switch with no page", PxcSwitchToAppPage, `{"channel":3}`, nil, false},
		{"switch with a body that is not JSON (km/d1 catches it)", PxcSwitchToAppPage, `nope`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			type event struct {
				cmd     int
				payload []byte
			}
			var events []event
			control.OnTransport = func(cmd int, payload []byte) { events = append(events, event{cmd, payload}) }

			got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: tc.cmd, Body: []byte(tc.body)}, 1)[0]
			if got.Command != tc.cmd+1 || len(got.Body) != 0 {
				t.Fatalf("reply = 0x%x %q, want an empty 0x%x", got.Command, got.Body, tc.cmd+1)
			}
			if !tc.event {
				if len(events) != 0 {
					t.Fatalf("events = %v, want none", events)
				}
				return
			}
			if len(events) != 1 || events[0].cmd != int(tc.cmd) || string(events[0].payload) != string(tc.payload) {
				t.Fatalf("events = %v, want one 0x%x with %v", events, tc.cmd, tc.payload)
			}
		})
	}
}

// Asked for the phone screen (101), the official app states that page open on the phone channel
// (nec/d.java:146-152).
func TestPhoneScreenPageRequestIsAnsweredWithPageStatus(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	requestClient, request := openControlChannel(t, control)
	_, phone := openPhoneChannel(t, control)

	go control.handleEvent(&PXCResponse{Command: PxcSwitchToAppPage, Body: []byte(`{"type":101}`)}, requestClient)
	if got := readPXCResponse(t, request); got.Command != PxcSwitchToAppPage+1 {
		t.Fatalf("on the request channel: 0x%x", got.Command)
	}
	got := readPXCResponse(t, phone)
	if got.Command != PxcPageStatus || string(got.Body) != `{"page":101,"status":1,"type":0}` {
		t.Fatalf("on the phone channel: 0x%x %s", got.Command, got.Body)
	}
}

// switchEc2Front goes out once, on the phone channel, to a flavor-51 dash - except to the three
// channels the official app skips (sdk/b0.java:369-412, 696-707) - and to no other dash unless
// its profile asked for the official app's announcements.
func TestSwitchEcToFront(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control func() *PXCControl
		want    bool
	}{
		{"Zontes, flavor 51 channel 21321", func() *PXCControl { return flavor51Control("21321") }, true},
		{"flavor 51 channel 5417", func() *PXCControl { return flavor51Control("5417") }, false},
		{"another flavor", func() *PXCControl { return NewPXCControl(":0", nil, nil) }, false},
		{"another flavor whose profile opted in", func() *PXCControl {
			c := NewPXCControl(":0", nil, nil)
			c.SetAppStatusNotify(true)
			return c
		}, true},
		{"flavor 51, host opted out", func() *PXCControl {
			c := flavor51Control("21321")
			c.SetFlavor51Lifecycle(false)
			return c
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := tc.control()
			_, request := openControlChannel(t, control)
			client, phone := openPhoneChannel(t, control)
			if control.carbitLifecycle() {
				// The phone channel opening announced mode 2 first; ack it.
				expectAppStatus(t, control, client, phone, PxcAppStatus, AppStatusBackground)
			}

			control.SwitchEcToFront()
			control.SwitchEcToFront()
			if tc.want {
				if got := readPXCResponse(t, phone); got.Command != PxcSwitchEcToFront || len(got.Body) != 0 {
					t.Fatalf("on the phone channel: 0x%x %q", got.Command, got.Body)
				}
				control.handleEvent(&PXCResponse{Command: PxcSwitchEcToFront + 1}, client)
			}
			assertSilent(t, phone, "the phone channel")
			assertSilent(t, request, "the dash's request channel")
		})
	}
}

// ReleaseDash sends switchEc2Background then STOP_SERVICE, each after the previous one's answer,
// and sends STOP_SERVICE even when the dash refuses the first.
func TestReleaseDash(t *testing.T) {
	for _, first := range []uint32{PxcSwitchEcToBackground + 1, PxcStatusUnsupported} {
		control := NewPXCControl(":0", nil, nil)
		client, phone := openPhoneChannel(t, control)
		done := make(chan error, 1)
		go func() { done <- control.ReleaseDash() }()

		if got := readPXCResponse(t, phone); got.Command != PxcSwitchEcToBackground {
			t.Fatalf("first: 0x%x", got.Command)
		}
		control.handleEvent(&PXCResponse{Command: first}, client)
		if got := readPXCResponse(t, phone); got.Command != PxcStopService {
			t.Fatalf("second: 0x%x", got.Command)
		}
		control.handleEvent(&PXCResponse{Command: PxcStopService + 1}, client)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ReleaseDash() = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ReleaseDash() never returned")
		}
	}
}

// A dash that leaves switchEc2Background unanswered still gets STOP_SERVICE: the first command's
// second frees the sender, which would otherwise wait out the dash's own 9 s while STOP_SERVICE
// expired in the queue. A command queued meanwhile goes after the pair, never between, and since
// an answer to the pair may still come, the channel is retired before that command can take it.
func TestReleaseDashSendsStopServiceWhenTheFirstGoesUnanswered(t *testing.T) {
	for _, stopAnswered := range []bool{true, false} {
		control := NewPXCControl(":0", nil, nil) // the dash's own timeout: the default 9 s
		client, phone := openPhoneChannel(t, control)
		_ = phone.SetDeadline(time.Now().Add(5 * time.Second))
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- control.ReleaseDash() }()

		if got := readPXCResponse(t, phone); got.Command != PxcSwitchEcToBackground {
			t.Fatalf("first: 0x%x", got.Command)
		}
		other := sendAsync(control, 0x20110, nil, 0)
		if got := readPXCResponse(t, phone); got.Command != PxcStopService {
			t.Fatalf("second: 0x%x, want STOP_SERVICE before anything queued later", got.Command)
		}
		if elapsed := time.Since(start); elapsed > 3*releaseDashWait/2 {
			t.Fatalf("STOP_SERVICE went out after %v, want about %v", elapsed, releaseDashWait)
		}
		if stopAnswered {
			control.handleEvent(&PXCResponse{Command: PxcStopService + 1}, client)
		}
		select {
		case err := <-done:
			if stopAnswered && err != nil {
				t.Fatalf("ReleaseDash() = %v", err)
			}
			if !stopAnswered && !errors.Is(err, ErrPhoneCommandTimeout) {
				t.Fatalf("ReleaseDash() = %v, want ErrPhoneCommandTimeout", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("ReleaseDash() never returned")
		}
		if r := awaitResult(t, other); !errors.Is(r.err, ErrNoPhoneChannel) {
			t.Fatalf("the command queued behind the release: %+v, %v; want ErrNoPhoneChannel", r.reply, r.err)
		}
		if _, err := phone.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("the phone channel was kept for a late answer to the release: %v", err)
		}
	}
}

// The dash reopens its phone channel while switchEc2Background waits: STOP_SERVICE goes out on the
// new one, and when that goes unanswered too it is the new one that is retired, not only the
// replaced one, so no late answer to the pair can reach a later command.
func TestReleaseDashRetiresTheChannelItsUnansweredCommandsWentOn(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	_, first := openPhoneChannel(t, control)
	done := make(chan error, 1)
	go func() { done <- control.ReleaseDash() }()
	if got := readPXCResponse(t, first); got.Command != PxcSwitchEcToBackground {
		t.Fatalf("first: 0x%x", got.Command)
	}
	_, second := openPhoneChannel(t, control)
	_ = second.SetDeadline(time.Now().Add(5 * time.Second))
	if got := readPXCResponse(t, second); got.Command != PxcStopService {
		t.Fatalf("on the reopened channel: 0x%x", got.Command)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrPhoneCommandTimeout) {
			t.Fatalf("ReleaseDash() = %v, want ErrPhoneCommandTimeout", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("ReleaseDash() never returned")
	}
	if control.phoneChannel() != nil {
		t.Fatal("the channel STOP_SERVICE went unanswered on is still the phone channel")
	}
	if _, err := second.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the reopened channel was kept for a late answer: %v", err)
	}
}

func TestReleaseDashWithoutPhoneChannelSaysSo(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	if err := control.ReleaseDash(); !errors.Is(err, ErrNoPhoneChannel) {
		t.Fatalf("ReleaseDash() = %v, want ErrNoPhoneChannel", err)
	}
}
