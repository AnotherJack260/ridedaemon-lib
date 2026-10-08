package net

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Phone-to-car page control.
//
// Everything else this file answers is dash-originated: the dash asks, we reply with
// cmd+1. These three go the other way. They come from the EasyConn SDK bundled in the
// CarbitRide APK (net.easyconn.carman.sdk_communication.P2C), where the phone sends
// them to move the head unit's own UI and the dash acks each with cmd+1.
//
// NO reference implementation sends any of them - not open-cflink, not open-cfmoto,
// not open-cfmoto-zanderp. They are here as an experiment for one dashboard family
// (Carbit flavor 51, channel 37303) that opens the video socket, pulls the whole
// stream at ~30 Hz and paints none of it, which is what a head unit whose UI never
// left its waiting page looks like from this side. See SetPageSwitchProbe.
const (
	// PxcPageStatus is ECP_P2C_PAGE_STATUS: {"page":n,"status":n,"type":n}. The phone
	// declaring which of its pages is now open - the half missing from a session where
	// the dash drains the stream and shows nothing.
	PxcPageStatus uint32 = 0x20400
	// PxcJumpToCarPage is ECP_P2C_JUMP_TO_CAR_PAGE: {"page":n}. An order rather than a
	// statement; firmware that expects to choose its own page may refuse it.
	PxcJumpToCarPage uint32 = 0x20480
	// PxcSwitchToMainPage is ECP_P2C_SWITCH_TO_SYSTEM_MAIN_PAGE, empty body. Sent last
	// and deliberately: it moves the dash AWAY from mirroring, so it is the control in
	// this experiment. If only this one visibly moves the panel, the P2C page plane
	// works and the mirror page id is what is wrong.
	PxcSwitchToMainPage uint32 = 0x20170
)

// Page ids from ECP_C2P_STANDARD_PAGES in the same SDK.
const (
	EcpPageMain           = 7
	EcpPageMirrorFloating = 21
)

// ECP_APP_PAGE_STATUS_OPEN, from ECP_P2C_PAGE_STATUS.
const ecpPageStatusOpen = 1

// pageSwitchProbeSpacing is how long the probe waits between steps. It has to be long
// enough that a rider watching the panel can say WHICH step moved it - the whole point
// of sending three - and short enough to fit inside the seconds a blank-screen session
// usually lasts before the rider gives up.
const pageSwitchProbeSpacing = 3 * time.Second

// pageProbeEvery is the spacing actually used, so a test can collapse it. Mirrors
// heartbeatEvery.
func (s *PXCControl) pageProbeEvery() time.Duration {
	if s.pageProbeSpacing > 0 {
		return s.pageProbeSpacing
	}
	return pageSwitchProbeSpacing
}

// PageSwitchProbeStep identifies one step of the probe in the event stream.
type PageSwitchProbeStep uint8

const (
	// PageProbeStarted is emitted once, before the first command, so a log can tell a
	// probe that ran and achieved nothing from one that never fired.
	PageProbeStarted PageSwitchProbeStep = iota
	PageProbePageStatus
	PageProbeJumpToCarPage
	PageProbeSwitchToMainPage
	// PageProbeNoControlChannel is emitted instead of the sequence when the dash has not
	// opened the phone channel to send it on - see phoneChannel.
	PageProbeNoControlChannel
)

// SetPageSwitchProbe enables the phone-to-car page sequence above, once per session,
// after the dash says STREAM_START. Opt-in and off by default: it puts three
// unsolicited commands on the wire that no dashboard has ever been observed to
// receive, so only a profile that has asked for it should ever see them.
func (s *PXCControl) SetPageSwitchProbe(enabled bool) {
	s.pageProbe = enabled
}

// phoneChannel is the connection the dash opened with 0x20000 (named "CAR_DATA" here), or
// nil until it has. In CarbitRide that is ECP_CONN_TYPE_P2C (h0.java): the only socket it
// ever writes a phone-originated command on, through one queue (z.e -> x0). The 0x10000
// socket carries the dash's own requests and the phone only answers there.
//
// This used to pick the 0x10000 socket, or failing that whichever one spoke last, on the
// belief that some dashes open neither - but neither opening frame ever reached the event
// stream, so no rider's log could have shown one. A command of ours landing where the dash
// waits for an answer is a protocol error: a Zontes JCDZ21 hung up on 0x20030 there within
// 18ms, five sessions out of five (MOTO-HUB issue #12).
//
// It is the NEWEST one. It used to be whichever a map scan met first, which after a dash
// reopened 0x20000 could be the dead one - and CHECK_SN_RESULT written into a dead socket is
// the QR page all over again.
func (s *PXCControl) phoneChannel() net.Conn {
	s.phoneMu.Lock()
	defer s.phoneMu.Unlock()
	return s.phone
}

// setPhoneChannel makes conn the phone channel. A new 0x20000 replaces the old sender and
// closes its socket in CarbitRide - but only from the same client address; from another one
// the new socket is refused while the old sender is alive (sdk/z.java:117-137). Both are done
// here, the replaced one closed quietly so whatever waits on it gives up now instead of at its
// timeout. Reports whether conn was taken.
func (s *PXCControl) setPhoneChannel(conn net.Conn) bool {
	s.phoneMu.Lock()
	old := s.phone
	if old != nil && old != conn && peerHost(old) != peerHost(conn) {
		s.phoneMu.Unlock()
		logging.Printf("PXC: refusing a phone channel from %v, the dash's is from %v", conn.RemoteAddr(), old.RemoteAddr())
		return false
	}
	s.phone = conn
	s.phoneMu.Unlock()
	if old == nil || old == conn {
		return true
	}
	if value, ok := s.connections.Load(old); ok {
		value.(*pxcConnectionState).superseded.Store(true)
	}
	logging.Printf("PXC: the dash opened a new phone channel, closing the old one")
	_ = old.Close()
	return true
}

func peerHost(conn net.Conn) string {
	addr := conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

func (s *PXCControl) isPhoneConn(conn net.Conn) bool {
	name, _ := s.connectionState(conn).channel.Load().(string)
	return name == "CAR_DATA"
}

// defaultPhoneChannelWait bounds how long CHECK_SN_RESULT waits for the phone channel.
const defaultPhoneChannelWait = 3 * time.Second

// sendCheckSnResult puts CHECK_SN_RESULT on the phone channel, where CarbitRide sends it
// (g.java, after its own licence lookup, through the same P2C queue), waiting for the dash to
// open that channel if it has not yet. A dash that never opens one still gets the result on
// the request channel, as it always has, so no unit that streams today loses its answer.
func (s *PXCControl) sendCheckSnResult(res *PXCResponse, fallback net.Conn) {
	if s.isStopping() {
		return
	}
	wait := defaultPhoneChannelWait
	if s.phoneChannelWait > 0 {
		wait = s.phoneChannelWait
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// ponytail: 50ms poll; a channel-opened signal if this ever has to be instant.
		conn := s.phoneChannel()
		for deadline := time.Now().Add(wait); conn == nil && time.Now().Before(deadline); conn = s.phoneChannel() {
			select {
			case <-s.quit:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		if conn == nil {
			err := s.writeResponse(res, fallback, nil)
			s.noteCheckSnResult("request channel (no phone channel opened)", fallback, res, err)
			return
		}
		// Through the queue, as g.java does (sdk/g.java:158-178 -> z.e), so it waits its turn
		// behind anything in flight and the dash's 0x201C1 is matched to it.
		note := func(conn net.Conn, err error) { s.noteCheckSnResult("phone channel", conn, res, err) }
		if _, err := s.queueP2C(res.Command, res.Body, time.Time{}, note); err != nil {
			note(conn, err)
		}
	}()
}

func (s *PXCControl) noteCheckSnResult(channel string, conn net.Conn, res *PXCResponse, err error) {
	logging.Printf("PXC CHECK_SN_RESULT on the %s: err=%v", channel, err)
	if err != nil {
		s.emitError(s.connectionError(conn, PxcWriteErr, err))
		return
	}
	s.emitEvent(*res)
}

// StartPageSwitchProbe runs the page sequence once, in the background. Calling it
// again on the same session does nothing, so a dash that says STREAM_START twice does
// not get six commands.
func (s *PXCControl) StartPageSwitchProbe() {
	if !s.pageProbe || s.isStopping() {
		return
	}
	s.pageProbeOnce.Do(func() {
		s.wg.Add(1)
		go s.runPageSwitchProbe()
	})
}

func (s *PXCControl) runPageSwitchProbe() {
	defer s.wg.Done()

	conn := s.phoneChannel()
	if conn == nil {
		logging.Printf("PXC page-switch probe: the dash has not opened the phone channel to send on")
		s.notePageProbe(PageProbeNoControlChannel, 0, nil)
		return
	}
	s.notePageProbe(PageProbeStarted, 0, nil)

	mirror, err := json.Marshal(map[string]int{
		"page":   EcpPageMirrorFloating,
		"status": ecpPageStatusOpen,
		"type":   0,
	})
	if err != nil {
		s.emitError(&PxcError{PxcWriteErr, fmt.Errorf("encode PAGE_STATUS: %w", err), true})
		return
	}
	jump, err := json.Marshal(map[string]int{"page": EcpPageMirrorFloating})
	if err != nil {
		s.emitError(&PxcError{PxcWriteErr, fmt.Errorf("encode JUMP_TO_CAR_PAGE: %w", err), true})
		return
	}

	steps := []struct {
		step PageSwitchProbeStep
		cmd  uint32
		body []byte
	}{
		{PageProbePageStatus, PxcPageStatus, mirror},
		{PageProbeJumpToCarPage, PxcJumpToCarPage, jump},
		{PageProbeSwitchToMainPage, PxcSwitchToMainPage, nil},
	}

	for i, step := range steps {
		if i > 0 {
			select {
			case <-s.quit:
				return
			case <-time.After(s.pageProbeEvery()):
			}
		}
		if s.isStopping() {
			return
		}
		// Through the queue like every phone command; the step is reported as soon as it is
		// on the wire, its ack (if any) reaches the event stream on its own.
		written := make(chan error, 1)
		_, err := s.queueP2C(step.cmd, step.body, time.Time{}, func(_ net.Conn, err error) { written <- err })
		if err == nil {
			select {
			case <-s.quit:
				return
			case err = <-written:
			}
		}
		logging.Printf(
			"PXC page-switch probe: sent 0x%x bodyBytes=%d err=%v",
			step.cmd, len(step.body), err,
		)
		s.notePageProbe(step.step, step.cmd, err)
		if err != nil {
			// The channel is gone; the remaining steps would only repeat the same error.
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			return
		}
	}
}

func (s *PXCControl) notePageProbe(step PageSwitchProbeStep, cmd uint32, err error) {
	if s.OnPageSwitchProbe == nil {
		return
	}
	s.OnPageSwitchProbe(step, cmd, err)
}
