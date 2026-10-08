package net

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Phone-to-car commands.
//
// CarbitRide writes every command of its own through one queue on the connection the dash
// opened with 0x20000 (z.e -> x0, sdk/z.java:162-207): one at a time, each waiting for the
// dash's cmd+1 before the next goes out (sdk/x0.java:225-235, 115-143). Everything this phone
// volunteers - CHECK_SN_RESULT, APPSTATUS, the page probe and whatever the host sends through
// SendPhoneCommand - goes through the same queue here, so no two of them are ever on the wire
// waiting for their acks at once, and every ack is matched to the command it answers.

// Status replies a dash can send instead of cmd+1. Both are even and both are answers: nothing
// here ever acknowledges one.
const (
	// PxcStatusUnsupported is what an EasyConn peer answers to a command it has no handler for
	// (sdk/b1.java:103-113, mm/m0.java:17-20).
	PxcStatusUnsupported uint32 = 0x80000010
	// PxcStatusError is a handler's failure reply (om/c.java:38-40, sdk/b1.java:135-144).
	PxcStatusError uint32 = 0x80000020
)

var (
	// ErrNoPhoneChannel: the dash has not opened 0x20000, so there is nothing to send on.
	// CarbitRide drops the command in that case (z.e(), sdk/z.java:188-202), and so does this.
	ErrNoPhoneChannel = errors.New("the dash has not opened the phone channel (0x20000)")
	// ErrPhoneCommandTimeout: the command went out and the dash did not answer in time.
	ErrPhoneCommandTimeout = errors.New("the dash did not answer")
	// ErrPhoneQueueFull: the queue is not draining; CarbitRide gives up the same way
	// (sdk/z.java:178-187).
	ErrPhoneQueueFull = errors.New("the phone command queue is full")

	errPhoneCommandExpired = errors.New("the caller gave up before it was sent")
	errSessionStopping     = errors.New("the session is stopping")
)

const (
	// defaultP2CReplyTimeout is how long x0 waits for a reply when CLIENT_INFO carries no
	// socketTimeoutPeriodWifi: 9 s on Wi-Fi (x0.a(), sdk/x0.java:86-113).
	defaultP2CReplyTimeout = 9 * time.Second
	// minP2CReplyTimeout floors the dash's own value. CarbitRide takes it as-is; the floor
	// keeps a dash that states 1 s from being judged between two of its own 2 s heartbeats.
	minP2CReplyTimeout = 3 * time.Second
	// ponytail: CarbitRide warns at 100 queued and gives up at 1000 (sdk/z.java:167-187);
	// nothing here queues more than a handful.
	p2cQueueSize = 64
)

// p2cNoReply reports the commands the dash owes no answer to: z0.getResponseProcessType()
// returning None (sdk/z0.java:44-47), plus the senders that override it to None - mm/k, mm/f
// and mm/j in the 0x6xxxxxxx block, and the status replies mm/m0, om/a, om/b and om/c.
func p2cNoReply(cmd uint32) bool {
	switch cmd {
	case 0x40010, 0x30020, 0x30030, 0x30040, 0x1060020, 0x1060030, 0x1090020,
		PxcStatusUnsupported, PxcStatusError, 0x80000030, 0x70000020:
		return true
	}
	return cmd&0xFF000000 == 0x60000000
}

// pxcIsReply reports a frame that answers something: every odd command, and the two even
// status replies above.
func pxcIsReply(cmd uint32) bool {
	return cmd&1 == 1 || cmd == PxcStatusUnsupported || cmd == PxcStatusError
}

type p2cRequest struct {
	cmd      uint32
	body     []byte
	deadline time.Time // zero: the caller waits as long as the queue takes
	// onWritten, when set, runs on the sender as soon as the write has been attempted, so an
	// internal caller can report what reached the wire without waiting for the ack.
	onWritten func(conn net.Conn, err error)
	done      chan p2cResult // buffered: the sender never blocks on a caller that left
}

type p2cResult struct {
	reply *PXCResponse
	err   error
}

// p2cFrame is one frame the dash wrote on a phone channel, handed to the sender waiting for it.
type p2cFrame struct {
	conn net.Conn
	res  PXCResponse
}

// SendPhoneCommand puts one phone-originated command on the phone channel, behind anything
// already queued, and returns the dash's answer: its cmd+1, or PxcStatusUnsupported /
// PxcStatusError if it refused. A command the dash owes no answer to (p2cNoReply) returns
// nil, nil once written.
//
// The wait for the answer is the dash's own (socketTimeoutPeriodWifi, else 9 s). timeout, when
// positive, bounds the whole call including the time spent queued; a command still queued
// when it expires is never sent. One already on the wire keeps the queue until its answer or
// the dash's timeout, as CarbitRide's does; its write is bounded by that timeout too. A command
// that misses it, in the write or the answer, retires the phone channel (retire): the commands
// behind it fail with ErrNoPhoneChannel until the dash opens a new one.
func (s *PXCControl) SendPhoneCommand(cmd uint32, body []byte, timeout time.Duration) (*PXCResponse, error) {
	var deadline time.Time
	var expired <-chan time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	req, err := s.queueP2C(cmd, body, deadline, nil)
	if err != nil {
		return nil, fmt.Errorf("phone command 0x%x: %w", cmd, err)
	}
	select {
	case r := <-req.done:
		if r.err != nil {
			return nil, fmt.Errorf("phone command 0x%x: %w", cmd, r.err)
		}
		return r.reply, nil
	case <-expired:
		return nil, fmt.Errorf("phone command 0x%x: %w within %s", cmd, ErrPhoneCommandTimeout, timeout)
	case <-s.quit:
		return nil, fmt.Errorf("phone command 0x%x: %w", cmd, errSessionStopping)
	}
}

// queueP2C hands one command to the sender without waiting for it.
func (s *PXCControl) queueP2C(cmd uint32, body []byte, deadline time.Time, onWritten func(net.Conn, error)) (*p2cRequest, error) {
	if s.isStopping() {
		return nil, errSessionStopping
	}
	if s.phoneChannel() == nil {
		return nil, ErrNoPhoneChannel
	}
	req := &p2cRequest{
		cmd:       cmd,
		body:      body,
		deadline:  deadline,
		onWritten: onWritten,
		done:      make(chan p2cResult, 1),
	}
	select {
	case s.p2cQueue <- req:
		return req, nil
	default:
		return nil, ErrPhoneQueueFull
	}
}

// startP2C starts the session's one sender. Called when the dash opens the phone channel, from
// that connection's own goroutine, so the WaitGroup is never at zero here.
func (s *PXCControl) startP2C() {
	s.p2cOnce.Do(func() {
		s.wg.Add(1)
		go s.runP2C()
	})
}

// runP2C is x0.run(): take the next command, send it, wait for its answer, and when nothing
// has been queued for a heartbeat interval, send a heartbeat and wait for its answer the same
// way (sdk/x0.java:184-224). The heartbeat stays opt-in (SetProactiveHeartbeat), as it was.
//
// With SetIdleProbe, the first idle tick after a command and every 7th after it carries cmd 0
// instead, as x0 sends it (sdk/x0.java:39-56, 185-217).
func (s *PXCControl) runP2C() {
	defer s.wg.Done()
	idle := time.NewTimer(s.heartbeatEvery())
	defer idle.Stop()
	idleTicks := 0
	for {
		select {
		case <-s.quit:
			return
		case req := <-s.p2cQueue:
			idleTicks = 0
			if !req.deadline.IsZero() && time.Now().After(req.deadline) {
				req.done <- p2cResult{err: errPhoneCommandExpired}
				continue
			}
			reply, err := s.p2cExchange(req.cmd, req.body, req.onWritten)
			req.done <- p2cResult{reply: reply, err: err}
		case <-idle.C:
			// Only while there is a phone channel: once the dash has closed it, there is
			// nothing to keep alive and nothing to log every 2 s.
			if s.phoneChannel() == nil {
				break
			}
			probe := s.idleProbe && idleTicks%7 == 0
			idleTicks++
			if probe {
				// x0 expects 0x80000010 and only logs anything else; a miss is not an error there.
				if reply, err := s.p2cExchange(0, nil, nil); err == nil && reply.Command != PxcStatusUnsupported {
					logging.Printf("PXC idle probe: cmd 0 answered 0x%x", reply.Command)
				}
			} else if s.proactiveHeartbeat {
				if _, err := s.p2cExchange(PxcHeartbeat, nil, nil); err != nil && !s.isStopping() {
					logging.Printf("PXC CAR_DATA heartbeat: %v", err)
				}
			}
		}
		idle.Reset(s.heartbeatEvery())
	}
}

// SetIdleProbe sends CarbitRide's cmd-0 probe on the phone channel's first idle tick after a
// command and every 7th after it, in place of the heartbeat (sdk/x0.java:185-217). Off by default:
// no dash has been seen to need it. Configure it before Start.
func (s *PXCControl) SetIdleProbe(enabled bool) {
	s.idleProbe = enabled
}

// noteSocketTimeout takes CLIENT_INFO's socketTimeoutPeriodWifi as the reply timeout, as
// x0.a() does (sdk/x0.java:86-113).
func (s *PXCControl) noteSocketTimeout(seconds int) {
	if seconds > 0 {
		s.p2cTimeout.Store(int64(max(time.Duration(seconds)*time.Second, minP2CReplyTimeout)))
	}
}

// judgeSilentDash ends the session when the dash has stopped answering. CarbitRide ends it as
// soon as one P2C reply misses the dash's own timeout (sdk/x0.java:200-209, 263-274, then
// t0.M0() at :328). Here that only happens when the dash has answered on this channel before
// and has said nothing on ANY PXC connection, nor polled for video, for that long: a dash that
// abandons 0x20000 (the Voge case in connectionError), keeps talking on 0x10000 or keeps
// pulling frames is alive and is left alone.
func (s *PXCControl) judgeSilentDash(state *pxcConnectionState, cmd uint32, timeout time.Duration) {
	if !state.answered.Load() {
		return
	}
	silent := time.Since(time.Unix(0, s.lastHeardNanos.Load()))
	if silent < timeout {
		return
	}
	if s.HeardElsewhere != nil && time.Since(s.HeardElsewhere()) < timeout {
		return // still pulling video: alive, whatever PXC is doing
	}
	s.emitError(&PxcError{PxcTimeoutErr, fmt.Errorf(
		"the dash stopped answering: no reply to 0x%x on the phone channel and nothing on any PXC channel for %s",
		cmd, silent.Round(100*time.Millisecond),
	), true})
}

// p2cReplyTimeout is the dash's own socketTimeoutPeriodWifi once CLIENT_INFO has stated it.
func (s *PXCControl) p2cReplyTimeout() time.Duration {
	if t := time.Duration(s.p2cTimeout.Load()); t > 0 {
		return t
	}
	return defaultP2CReplyTimeout
}

// p2cExchange writes one command on the newest phone channel and waits for its answer.
func (s *PXCControl) p2cExchange(cmd uint32, body []byte, onWritten func(net.Conn, error)) (*PXCResponse, error) {
	conn := s.phoneChannel()
	if conn == nil {
		if onWritten != nil {
			onWritten(nil, ErrNoPhoneChannel)
		}
		return nil, ErrNoPhoneChannel
	}
	state := s.connectionState(conn)

	// An answer that arrived after its own command timed out must not answer this one.
	for drained := false; !drained; {
		select {
		case <-s.p2cReplies:
		default:
			drained = true
		}
	}

	err := s.writeResponse(&PXCResponse{Command: cmd, Body: body}, conn, nil)
	if onWritten != nil {
		onWritten(conn, err)
	}
	if err == nil && cmd == PxcHeartbeat {
		state.beatsSince.Add(1)
	}
	if err != nil || p2cNoReply(cmd) {
		return nil, err
	}

	timeout := s.p2cReplyTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-s.quit:
			return nil, errSessionStopping
		case <-state.done:
			return nil, errors.New("the phone channel closed before the dash answered")
		case <-timer.C:
			s.judgeSilentDash(state, cmd, timeout)
			// The answer may still come, and with no transaction ids it would be taken for the
			// next command's. CarbitRide ends the whole session here (see judgeSilentDash); this
			// retires only the channel, which the dash reopens. A dash that answers within its own
			// timeout - every one seen, the Zontes included - never gets here.
			s.retire(conn, fmt.Errorf("no answer to 0x%x on the phone channel within %s", cmd, timeout))
			return nil, fmt.Errorf("%w within %s", ErrPhoneCommandTimeout, timeout)
		case frame := <-s.p2cReplies:
			if frame.conn != conn {
				continue
			}
			switch frame.res.Command {
			case cmd + 1, PxcStatusUnsupported, PxcStatusError:
				// x0.b(): cmd+1 is the answer, 0x80000010 and 0x80000020 are the dash
				// saying no (sdk/x0.java:126-140). All three are the dash answering.
				return &frame.res, nil
			}
			// x0.b() tears the channel down on any other reply (sdk/x0.java:136-137). A
			// stray frame is only skipped here: it already reached the event stream.
			logging.Printf("PXC phone channel: 0x%x while waiting for 0x%x, skipped", frame.res.Command, cmd+1)
		}
	}
}

// deliverP2CReply hands a frame the dash wrote on a phone channel to the sender. x0 reads its
// answers off the same socket it writes (sdk/x0.java:115-143).
func (s *PXCControl) deliverP2CReply(conn net.Conn, event *PXCResponse) {
	s.connectionState(conn).answered.Store(true)
	select {
	case s.p2cReplies <- p2cFrame{conn: conn, res: *event}:
	default:
		logging.Printf("PXC phone channel: no room for reply 0x%x, dropped", event.Command)
	}
}
