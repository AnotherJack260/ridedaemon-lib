package net

import (
	"encoding/binary"
	"encoding/json"
	"errors"
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

// Lifecycle commands: the official app's own, unlike the probe above.
const (
	// PxcSwitchEcToFront is switchEc2Front (mm/k0.java), empty body: the phone's mirror is ready
	// and the dash should bring its EasyConn page forward. Sent when the mirror starts while the
	// dash has not received a frame yet (bridge/PermissionBridgeImpl.java:45,324; sdk/b0.java:696-707).
	PxcSwitchEcToFront uint32 = 0x201D0
	// PxcSwitchEcToBackground is switchEc2Background (mm/j0.java), empty body: the phone is no
	// longer mirroring and the dash may show its own UI (sdk/b0.java:686-689).
	PxcSwitchEcToBackground uint32 = 0x20370
	// PxcStopService is ECP_P2C_STOP_SERVICE (mm/i0.java), empty body: the phone is leaving.
	PxcStopService uint32 = 0x20150

	// PxcDashMainPage is the dash asking the phone to show its home screen (km/e1.java ->
	// onReceiveSwitchToMainPage, which launches the app's net.easyconn.carman.home intent,
	// ec/b.java:336-340, 838-850). Reported as a transport event with this number, empty payload.
	PxcDashMainPage uint32 = 0x10380
	// PxcDashUiFront is the dash bringing its own UI to the front (km/c1.java). The official app
	// stops the true mirror if it is running (ec/b.java:416-421). Transport event, empty payload.
	PxcDashUiFront uint32 = 0x10490
	// PxcSwitchToAppPage is ECP_C2P_SWITCH_TO_APP_PAGE {"type":n[,"channel":m]} (km/d1.java): the
	// dash asks for one of the phone's pages - 21 open the mirror, 7 the dash's own main page
	// (stop mirroring), 101 the phone screen, others an app page (nec/d.java:60-182). Transport
	// event with this number, payload 8 bytes big-endian: type, channel (0 when absent).
	PxcSwitchToAppPage uint32 = 0x103F0
	// PxcDashDisconnect is the dash asking the phone to disconnect (km/u0.java). Over Wi-Fi the
	// official app refuse-lists the dash's HUID and closes PXC (ec/b.java:588-591 -> ec/v.java:
	// 1695-1720, 989-997). Transport event with this number, empty payload: the host should stop
	// the session and not reconnect on its own.
	PxcDashDisconnect uint32 = 0x106F0
)

// EcpPagePhoneScreen is ECP_APP_PAGE_PHONE_SCREEN.
const EcpPagePhoneScreen = 101

// releaseDashWait is how long each of ReleaseDash's two commands waits for its answer:
// PxcUtils.waitSendStopResponse gives STOP_SERVICE's one second (utils/PxcUtils.java:80-114).
const releaseDashWait = time.Second

func init() {
	// Each is answered with the empty cmd+1 every handled command gets (sdk/v0.java:50-66); what
	// the official app does next is the host's to do, so each is handed on as a typed event.
	for _, cmd := range []uint32{PxcDashMainPage, PxcDashUiFront, PxcDashDisconnect} {
		RegisterC2PHandler(cmd, func(s *PXCControl, req *PXCResponse) ([]byte, error) {
			s.emitTransport(int(req.Command), nil)
			return nil, nil
		})
	}
	RegisterC2PHandler(PxcSwitchToAppPage, answerSwitchToAppPage)
}

// answerSwitchToAppPage reports the page the dash asked for. A body without a type, or one that
// is not JSON, is acked and dropped, as d1.g() does (km/d1.java:22-35). For page 101 the official
// app also states that page open: PAGE_STATUS {page:101,status:1,type:0} (nec/d.java:146-152).
func answerSwitchToAppPage(s *PXCControl, req *PXCResponse) ([]byte, error) {
	var page struct {
		Type    json.RawMessage `json:"type"`
		Channel json.RawMessage `json:"channel"`
	}
	_ = json.Unmarshal(req.Body, &page)
	pageType, ok := rawLong(page.Type)
	if !ok {
		logging.Printf("PXC SWITCH_TO_APP_PAGE without a page: %q", req.Body)
		return nil, nil
	}
	channel, _ := rawLong(page.Channel)
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:], uint32(pageType))
	binary.BigEndian.PutUint32(payload[4:], uint32(channel))
	s.emitTransport(int(req.Command), payload)
	if pageType == EcpPagePhoneScreen {
		body, _ := json.Marshal(map[string]int{"page": EcpPagePhoneScreen, "status": ecpPageStatusOpen, "type": 0})
		if _, err := s.queueP2C(PxcPageStatus, body, time.Time{}, nil); err != nil {
			logging.Printf("PXC PAGE_STATUS for page 101 not sent: %v", err)
		}
	}
	return nil, nil
}

// SwitchEcToFront sends switchEc2Front once per session, on the phone channel. The official app
// sends it when its mirror starts and the dash has not yet received a frame, except to channels
// 5414, 5417 and 5418 (sdk/b0.java:369-412, 696-707). Called when the dash says STREAM_START, the
// moment this side's stream starts and before any frame went out. Only for a dash that gets the
// official app's lifecycle commands (carbitLifecycle).
func (s *PXCControl) SwitchEcToFront() {
	if s.isStopping() || !s.carbitLifecycle() {
		return
	}
	switch s.dashChannel() {
	case "5414", "5417", "5418":
		logging.Printf("PXC switchEc2Front skipped for channel %s, as the official app skips it", s.dashChannel())
		return
	}
	s.ecFrontOnce.Do(func() {
		if _, err := s.queueP2C(PxcSwitchEcToFront, nil, time.Time{}, func(_ net.Conn, err error) {
			logging.Printf("PXC switchEc2Front (0x%x) sent: err=%v", PxcSwitchEcToFront, err)
		}); err != nil {
			logging.Printf("PXC switchEc2Front (0x%x) not sent: %v", PxcSwitchEcToFront, err)
		}
	})
}

// ReleaseDash hands the dash back its own UI the way the official app does when the rider
// disconnects: switchEc2Background, then STOP_SERVICE, one after the other on the phone channel
// (ec/v.java:1695-1704; HomeActivity.java:896-898). Each waits for the dash's answer at most a
// second, as the official app waits for STOP_SERVICE's (utils/PxcUtils.java:80-114), and that
// second frees the sender: a dash that leaves switchEc2Background unanswered still gets
// STOP_SERVICE. The two go out back to back, nothing else between them, and neither is dropped
// for waiting its turn, so STOP_SERVICE is on the wire before ReleaseDash returns; a command
// already in flight holds them up for at most the dash's own timeout. If either went unanswered
// the phone channel is retired after them (runP2CChain). It sends both whatever the dash's
// flavor: the host asked. Returns STOP_SERVICE's outcome.
func (s *PXCControl) ReleaseDash() error {
	stop := &p2cRequest{cmd: PxcStopService, wait: releaseDashWait}
	background := &p2cRequest{cmd: PxcSwitchEcToBackground, wait: releaseDashWait, then: stop}
	if err := s.enqueueP2C(background); err != nil {
		return fmt.Errorf("phone command 0x%x: %w", background.cmd, err)
	}
	if err := s.awaitP2C(background); err != nil {
		if errors.Is(err, ErrNoPhoneChannel) || errors.Is(err, errSessionStopping) {
			return err
		}
		logging.Printf("PXC release: %v; sending STOP_SERVICE anyway", err)
	}
	return s.awaitP2C(stop)
}

// awaitP2C waits for a queued request's outcome.
func (s *PXCControl) awaitP2C(req *p2cRequest) error {
	var err error
	select {
	case r := <-req.done:
		err = r.err
	case <-s.quit:
		err = errSessionStopping
	}
	if err != nil {
		return fmt.Errorf("phone command 0x%x: %w", req.cmd, err)
	}
	return nil
}
