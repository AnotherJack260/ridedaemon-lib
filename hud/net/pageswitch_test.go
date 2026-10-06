package net

import (
	"context"
	"encoding/json"
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

	_, server := openPhoneChannel(t, control)
	control.StartPageSwitchProbe()

	first := readPXCResponse(t, server)
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

	_, server := openPhoneChannel(t, control)
	control.StartPageSwitchProbe()
	control.StartPageSwitchProbe()

	for i := 0; i < 3; i++ {
		readPXCResponse(t, server)
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
