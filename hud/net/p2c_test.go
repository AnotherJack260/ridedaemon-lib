package net

import (
	"encoding/json"
	stdnet "net"
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
	go control.handleEvent(&PXCResponse{Command: PxcChannelCarData}, client)
	if got := readPXCResponse(t, server); got.Command != PxcChannelCarData+1 {
		t.Fatalf("phone channel reply = 0x%x, want 0x%x", got.Command, PxcChannelCarData+1)
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
