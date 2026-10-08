package net

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

type connectionState struct {
	frameCounter   uint32
	pollCount      uint64
	lastPullReport time.Time
}

// VideoPullPhase says why a pull count is being reported, so the phone can tell the
// three cases apart that a black screen collapses into today.
type VideoPullPhase byte

const (
	// VideoPullSocketOpen: the dash connected to the data port. It has not asked
	// for anything yet, and a session that never gets further than this is a dash
	// that opened the socket and then went quiet.
	VideoPullSocketOpen VideoPullPhase = 0
	// VideoPullFirst: the dash asked for its first frame. This is the only
	// evidence anywhere in the stack that video is actually being consumed.
	VideoPullFirst VideoPullPhase = 1
	// VideoPullProgress: periodic running total while the dash keeps pulling.
	VideoPullProgress VideoPullPhase = 2
	// VideoPullSocketClosed: final total for this connection. A zero here is the
	// diagnosis a rider has been waiting months for: the dash never asked at all.
	VideoPullSocketClosed VideoPullPhase = 3
)

// How often a running total is reported while the dash is pulling. Frequent enough
// that a log covering a short session still shows the stream was alive, rare enough
// that a 30-minute ride does not fill the rider's log with it.
const videoPullReportInterval = 5 * time.Second

// buildFramedPacket wraps one access unit for the dash's data socket.
//
// withIndex (the only format MOTO-HUB has ever streamed):
//
//	4B length (index + AU) | 4B frame index | Annex-B AU
//
// Without it, the format the EasyConn reverse-engineering notes document instead:
//
//	4B length (AU) | Annex-B AU
//
// A dash that scans for the 00 00 00 01 start code swallows the extra index either
// way, which is why every unit driven so far works with the indexed form. One that
// hands the buffer straight to its decoder does not. See
// [MediaStream.SetPlainFramingAllowed] for who gets to choose.
func buildFramedPacket(body []byte, frameCounter uint32, withIndex bool) ([]byte, uint32) {
	idx := frameCounter

	payloadLen := len(body)
	if withIndex {
		payloadLen += 4
	}

	// 4B len
	lenBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBytes, uint32(payloadLen))

	// data
	frame := make([]byte, 0, mediaStepFrameSize+len(body))
	frame = append(frame, lenBytes...)
	if withIndex {
		// 4B index
		idxBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(idxBytes, idx)
		frame = append(frame, idxBytes...)
	}
	frame = append(frame, body...)

	return frame, idx
}

func sendChunked(w *bufio.Writer, frame []byte, chunkSize int, sleep time.Duration) error {
	// no chunking needed
	if chunkSize <= 0 {
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return w.Flush()
	}

	offset := 0
	for offset < len(frame) {
		end := offset + chunkSize
		if end > len(frame) {
			end = len(frame)
		}

		// Write chunk :p
		if _, err := w.Write(frame[offset:end]); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}

		offset = end
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
	return nil
}

// Step frame 72 00 00 00 00 00 00 00 - 8 bytes
const mediaStepFrameSize = 8

type MediaStream struct {
	port     string
	quit     chan any
	wg       sync.WaitGroup
	listener net.Listener
	tracker  *ConnTracker

	stopOnce sync.Once

	// Shared frame source (temp)
	src stream.FrameSource

	// Config
	chunkSize  int           // e.g 0x1000
	chunkSleep time.Duration // e.g 3 * time.Millisecond

	// Frame format. plainFramingAllowed is set by the host before the session
	// starts; plainFraming is what the dash actually negotiated. Both default to
	// false, i.e. the indexed format every working unit has been streamed so far.
	plainFramingAllowed atomic.Bool
	plainFraming        atomic.Bool
	// lastPollNanos is the last time the dash asked for a frame, on any connection.
	lastPollNanos atomic.Int64
	// How the dash asked for frames in its last CAPTURE_CONFIG and 0x60 (SetCaptureConfig).
	// The zero values are poll mode, the only mode there was before.
	wantFps   atomic.Int32
	movieMode atomic.Bool
	jpeg      atomic.Bool

	// OnVideoPulls, when set before Start, receives every change worth reporting in
	// how many frames the dash has pulled on a connection: the socket opening, the
	// first pull, a running total every videoPullReportInterval, and the final count
	// when the connection ends. Called from the connection's own goroutine, so the
	// host must not block in it.
	OnVideoPulls func(phase VideoPullPhase, pulls uint64)

	// mcuLite takes the data socket of an MCU-Lite dash, which speaks its own frame protocol on
	// this port (transport_mcu_lite.go). Nil unless the host enabled MCU-Lite.
	mcuLite *MCULite

	// Interface events
	Errors chan error
}

// SetPlainFramingAllowed lets the dash's own supportExtendProtocol byte select the
// un-indexed frame format. Off unless the host asks for it, so a dash it recognises
// keeps the exact bytes on the wire it gets today whatever it reports. Configure it
// before Start.
func (s *MediaStream) SetPlainFramingAllowed(allowed bool) {
	s.plainFramingAllowed.Store(allowed)
}

// NegotiatedExtendedProtocol reports the supportExtendProtocol byte echoed in the
// capture-config reply. It only switches the format when the host allowed it, and
// returns whether plain (un-indexed) framing is in effect after the call — the
// caller forwards that to the phone so the decision is visible in field logs, not
// just in this process's own logging.
func (s *MediaStream) NegotiatedExtendedProtocol(extended bool) bool {
	if !s.plainFramingAllowed.Load() {
		return false
	}
	plain := !extended
	if s.plainFraming.Swap(plain) != plain {
		if plain {
			logging.Printf("MediaStream: dash reports supportExtendProtocol=0, dropping the frame index")
		} else {
			logging.Printf("MediaStream: dash reports supportExtendProtocol=1, keeping the frame index")
		}
	}
	return plain
}

// SetCaptureConfig tells the stream how the dash wants its frames, for each connection it opens
// from now on: Carbit picks the mode as a data connection starts (base/q0.java:1244-1274).
// Movie mode first; then push mode when CAPTURE_CONFIG named a frame rate; else the poll loop.
func (s *MediaStream) SetCaptureConfig(c CaptureConfig) {
	s.wantFps.Store(int32(c.WantFps))
	s.movieMode.Store(c.MovieMode)
	s.jpeg.Store(c.Encoder == int(mediaEncoderJpeg))
}

// LastPoll is when the dash last said anything on the video socket; zero if never.
func (s *MediaStream) LastPoll() time.Time {
	if n := s.lastPollNanos.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// reportPulls hands one pull observation to the host, if it asked for them.
//
// Deliberately not derived from anything the phone can already see. Every counter the
// Android side owns - frames offered, timeouts, rejections - describes the pipe from
// the encoder into this library's ring buffer, and all of them look perfect while a
// dash sits there never asking for a byte. This is the other end of that pipe.
func (s *MediaStream) reportPulls(st *connectionState, phase VideoPullPhase) {
	report := s.OnVideoPulls
	if report == nil {
		return
	}
	st.lastPullReport = time.Now()
	report(phase, st.pollCount)
}

func NewMediaStream(port string, src stream.FrameSource, chunkSize int, chunkSleep time.Duration) *MediaStream {
	return &MediaStream{
		port:       port,
		quit:       make(chan any),
		tracker:    NewConnTracker(),
		Errors:     make(chan error, 16),
		src:        src,
		chunkSize:  chunkSize,
		chunkSleep: chunkSleep,
	}
}

func (s *MediaStream) emitError(err error) {
	select {
	case s.Errors <- err:
	default:
		logging.Printf("Dropping error from MediaStream [No one's listening!]: %v", err)
	}
}

// connectionError decides whether one connection's failure ends the session, on the same rule the
// PXC server has used since a dash's abandoned CAR_DATA channel was found tearing down working
// sessions: fatal only when nothing else is being served here.
//
// The bike is KNOWN to open :10922 twice and abandon one of them; whether it does the same here has
// not been proven on a dash, so this is the same rule applied to the same shape of listener rather
// than a fix for a failure already seen on :10920. It can only ever delay a verdict, never invent
// one: a dash that really goes away kills every connection, and whichever one notices last finds
// itself alone and says so.
//
// The fatal text is unchanged, character for character: failures are grouped downstream by the
// text of that line, so the sentence a real death produces has to go on reading the same.
func (s *MediaStream) connectionError(conn net.Conn, errType StrmErrorType, cause error) *StrmError {
	others := s.tracker.Others(conn)
	if others == 0 {
		return &StrmError{errType, cause, true}
	}
	return &StrmError{
		errType,
		fmt.Errorf("one video channel closed, %d still serving the dash: %v", others, cause),
		false,
	}
}

// isStopping reports our own teardown, so a read or write that CloseAll is about to interrupt does
// not come back as a fault of the dash's.
func (s *MediaStream) isStopping() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *MediaStream) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return // Normal shut down
			default:
				logging.Printf("Error accepting connection: %s", err)
				continue
			}
		}

		logging.Printf("New MediaStream client from %s", conn.RemoteAddr())
		if s.mcuLite.claimData(conn) {
			// Tracked like the others, so Stop closes it and waits for it.
			s.tracker.Add(conn)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.tracker.Remove(conn)
				s.mcuLite.serveData(conn)
			}()
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Handling individual TCP Connection/Client
func (s *MediaStream) handleConn(conn net.Conn) {
	s.tracker.Add(conn)
	defer func() {
		s.tracker.Remove(conn)
		_ = conn.Close()
		s.wg.Done()
	}()

	// Keep a fast & stead connection
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
	}

	input := bufio.NewReaderSize(conn, 8*1024)
	output := bufio.NewWriterSize(conn, 64*1024)
	defer func() {
		if err := output.Flush(); err != nil {
			logging.Printf("MediaStream: Error flushing output: %v", err)
		}
	}()

	// Retire the connection before judging its failure, never after - see the PXC server's note.
	fail := func(errType StrmErrorType, cause error) {
		if s.isStopping() {
			return
		}
		s.tracker.Remove(conn)
		s.emitError(s.connectionError(conn, errType, cause))
	}

	header := make([]byte, mediaStepFrameSize)
	zero4 := []byte{0, 0, 0, 0}

	st := &connectionState{
		frameCounter: 0,
		pollCount:    0,
	}
	s.reportPulls(st, VideoPullSocketOpen)
	defer s.reportPulls(st, VideoPullSocketClosed)

	logging.Printf("Starting MediaStream Loop")
	defer logging.Printf("Stopping MediaStream Loop")
	if s.movieMode.Load() {
		s.serveMovieMode(conn, input, output, st, fail)
		return
	}
	if fps := s.wantFps.Load(); fps != 0 {
		s.servePushMode(conn, input, output, st, fail, fps)
		return
	}
	for {
		// Read 8 bytes (poll header)
		if _, err := io.ReadFull(input, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				fail(StrmDecodeErr, fmt.Errorf("error reading header: %v", err))
				return
			}
			fail(StrmDecodeErr, fmt.Errorf("unknown error reading header: %v", err))
			return
		}

		cmd := binary.LittleEndian.Uint16(header[0:2])
		s.lastPollNanos.Store(time.Now().UnixNano())
		// The header's i16 length says how much body follows; the official app reads and
		// drops it (base/q0.java:1055-1061). Leaving it in the stream would read it as the
		// next poll and desync every frame after.
		if bodyLen := int16(binary.LittleEndian.Uint16(header[2:4])); bodyLen > 0 {
			if _, err := io.CopyN(io.Discard, input, int64(bodyLen)); err != nil {
				fail(StrmDecodeErr, fmt.Errorf("error reading poll body: %v", err))
				return
			}
		}

		if cmd != 0x0072 {
			// Not a poll pacing command, discard and send idle 0's
			s.emitError(&StrmError{
				StrmUnknownCommandErr,
				fmt.Errorf("non-0x0072 cmd %04x, sending idle", cmd),
				false,
			})
			if _, err := output.Write(zero4); err != nil {
				fail(StrmWriteErr, fmt.Errorf("error writing idle: %v", err))
				return
			}
			if err := output.Flush(); err != nil {
				fail(StrmWriteErr, fmt.Errorf("error flushing idle: %v", err))
				return
			}
			continue
		}

		s.countPull(st)

		body, err := s.src.NextFrame(time.Now())
		if err != nil {
			s.emitError(&StrmError{StrmUnknownCommandErr, fmt.Errorf("error reading frame: %v", err), false})
			// we will send an idle 0s body so the connection stays open
			if _, err = output.Write(zero4); err != nil {
				fail(StrmWriteErr, fmt.Errorf("MediaStream: error writing idle after src failure: %v", err))
				return
			}
			if err = output.Flush(); err != nil {
				fail(StrmWriteErr, fmt.Errorf("MediaStream: error flushing idle after src failure: %v", err))
				return
			}
			continue
		}

		// If no payload is available, send idle 0s
		if body == nil || len(body) == 0 {
			if _, err = output.Write(zero4); err != nil {
				fail(StrmWriteErr, fmt.Errorf("error writing idle: %v", err))
				return
			}
			if err = output.Flush(); err != nil {
				fail(StrmWriteErr, fmt.Errorf("flush error (idle): %v", err))
				return
			}
			continue
		}

		// Legacy pacing - 4B len + 4B idx + body
		frame, idx := buildFramedPacket(body, st.frameCounter, s.indexed())
		st.frameCounter = (st.frameCounter + 1) & 0x7FFFFFFF

		// Send it chunked, send it paced
		if err = sendChunked(output, frame, s.chunkSize, s.chunkSleep); err != nil {
			fail(StrmWriteErr, fmt.Errorf("error sending chunks (idx=%d): %v", idx, err))
			return
		}
	}
}

// indexed reports whether a frame carries the 4-byte index. A JPEG never does, nor does a movie-mode
// frame, whatever the dash said about the extend protocol: Carbit writes length | JPEG
// (base/q0.java:512-525) and, in movie mode, length | AU | timestamp (q0.java:641-651). Only an
// H.264 frame outside movie mode keeps the format the host and the dash negotiated.
func (s *MediaStream) indexed() bool {
	return !s.plainFraming.Load() && !s.jpeg.Load() && !s.movieMode.Load()
}

// countPull counts one frame the dash asked for (in push and movie mode, where nothing is asked
// for one frame at a time: one frame written) and reports the first and then a running total.
func (s *MediaStream) countPull(st *connectionState) {
	st.pollCount++
	if st.pollCount == 1 {
		logging.Printf("MediaStream: dash pulled its first frame")
		s.reportPulls(st, VideoPullFirst)
	} else if time.Since(st.lastPullReport) >= videoPullReportInterval {
		s.reportPulls(st, VideoPullProgress)
	}
}

// nextFrame is the poll loop's source read for the other two modes: an error is reported, not
// fatal, and sent as no frame.
func (s *MediaStream) nextFrame() []byte {
	body, err := s.src.NextFrame(time.Now())
	if err != nil {
		s.emitError(&StrmError{StrmUnknownCommandErr, fmt.Errorf("error reading frame: %v", err), false})
		return nil
	}
	return body
}

// writeWhole writes one frame, or 00 00 00 00 when there is none (base/q0.java:609-610), and
// flushes it whole as Carbit does (q0.java:694-712). The poll loop's chunk pacing stays its own.
func (s *MediaStream) writeWhole(output *bufio.Writer, st *connectionState, body []byte) error {
	packet := []byte{0, 0, 0, 0}
	if len(body) > 0 {
		packet, _ = buildFramedPacket(body, st.frameCounter, s.indexed())
		st.frameCounter = (st.frameCounter + 1) & 0x7FFFFFFF
		s.countPull(st)
	}
	if _, err := output.Write(packet); err != nil {
		return err
	}
	return output.Flush()
}

// readDash reads what the dash sends in the modes that write without waiting for it, so a dash
// that goes away is noticed and nothing it sends backs up. Each 0x72's body goes to onPoll. The
// error that ends the reads is sent on failed, and exited closes when it returns.
func (s *MediaStream) readDash(input *bufio.Reader, onPoll func(body []byte), failed chan<- error, exited chan<- struct{}) {
	defer close(exited)
	header := make([]byte, mediaStepFrameSize)
	for {
		if _, err := io.ReadFull(input, header); err != nil {
			failed <- err
			return
		}
		s.lastPollNanos.Store(time.Now().UnixNano())
		body := make([]byte, max(0, int(int16(binary.LittleEndian.Uint16(header[2:4])))))
		if _, err := io.ReadFull(input, body); err != nil {
			failed <- err
			return
		}
		if onPoll != nil && binary.LittleEndian.Uint16(header[0:2]) == 0x0072 {
			onPoll(body)
		}
	}
}

// servePushMode writes to a dash whose CAPTURE_CONFIG named a frame rate, without waiting to be
// asked: Carbit starts such a connection without reading a first command (base/q0.java:1250-1252)
// and loops send-then-pace (q0.java:1076-1078). The pace is the one Carbit computes: a frame every
// 1000/fps whole milliseconds from the frame's start (nec/m.java:564-567, 424-438). Whatever the
// dash sends meanwhile is read and ignored; Carbit never reads it at all.
//
// ponytail: Carbit 2.4 never stamps that start on this path (o1.f75231b stays 0), so its sleep
// never fires and frames leave as fast as MediaCodec yields them, each wait capped at 200 ms. A
// ring buffer cannot block like dequeueOutputBuffer, so this keeps Carbit's computed pace and
// leaves the app to encode at that same fps (EncoderPlan.Fps).
func (s *MediaStream) servePushMode(conn net.Conn, input *bufio.Reader, output *bufio.Writer, st *connectionState, fail func(StrmErrorType, error), fps int32) {
	interval := time.Duration(1000/fps) * time.Millisecond
	if interval < time.Millisecond {
		interval = time.Millisecond // ponytail: a dash asking for over 1000 fps, or a negative rate
	}
	logging.Printf("MediaStream: dash asked for %d fps, pushing a frame every %v", fps, interval)
	failed, exited := make(chan error, 1), make(chan struct{})
	go s.readDash(input, nil, failed, exited)
	defer func() { _ = conn.Close(); <-exited }()

	for {
		start := time.Now()
		if err := s.writeWhole(output, st, s.nextFrame()); err != nil {
			fail(StrmWriteErr, fmt.Errorf("error pushing frame: %v", err))
			return
		}
		select {
		case err := <-failed:
			// The poll loop's sentence for a dash that closed, so both group together downstream.
			fail(StrmDecodeErr, fmt.Errorf("error reading header: %v", err))
			return
		case <-time.After(interval - time.Since(start)):
		}
	}
}

// movieClock stands in for SystemClock.elapsedRealtime() in movie-mode trailers: monotonic
// milliseconds, only meaningful against each other. ponytail: counted from process start, not boot.
var movieClock = time.Now()

// serveMovieMode serves a dash in movie mode (supportFunction bit 0 in its 0x60, echoed back;
// base/l0.java:264-267). It grants frames rather than polling for each: byte 1 of a 0x72's body is
// how many it will take, replacing what was left, not adding to it (utils/Protocol.java:998-1004,
// base/q0.java:262-274). The first 0x72 is answered with a frame at once (q0.java:1147-1178), then
// one frame per credit (q0.java:997-1016). Every H.264 frame carries a u64 LE millisecond
// timestamp after the payload, counted in its length (q0.java:651-663, 342-347); a JPEG does not
// (q0.java:515-524), nor does an empty 00 00 00 00.
func (s *MediaStream) serveMovieMode(conn net.Conn, input *bufio.Reader, output *bufio.Writer, st *connectionState, fail func(StrmErrorType, error)) {
	logging.Printf("MediaStream: movie mode, sending frames as the dash grants them")
	var credits atomic.Int32
	polled := make(chan struct{}, 1)
	failed, exited := make(chan error, 1), make(chan struct{})
	go s.readDash(input, func(body []byte) {
		if len(body) >= 2 {
			credits.Store(int32(int8(body[1]))) // a Java byte: 0x80 and up grant nothing
		}
		select {
		case polled <- struct{}{}:
		default:
		}
	}, failed, exited)
	defer func() { _ = conn.Close(); <-exited }()

	waitForPoll := func() bool {
		select {
		case <-polled:
			return true
		case err := <-failed:
			fail(StrmDecodeErr, fmt.Errorf("error reading header: %v", err))
			return false
		}
	}
	if !waitForPoll() {
		return
	}
	for first := true; ; first = false {
		if !first {
			for credits.Load() <= 0 {
				if !waitForPoll() {
					return
				}
			}
			credits.Add(-1)
		}
		body := s.nextFrame()
		if len(body) > 0 && !s.jpeg.Load() {
			body = binary.LittleEndian.AppendUint64(append([]byte(nil), body...), uint64(time.Since(movieClock).Milliseconds()))
		}
		if err := s.writeWhole(output, st, body); err != nil {
			fail(StrmWriteErr, fmt.Errorf("error sending movie frame: %v", err))
			return
		}
	}
}

func (s *MediaStream) Start() error {
	// Create listener
	ln, err := net.Listen("tcp", s.port)
	if err != nil {
		return err
	}

	s.listener = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *MediaStream) Stop(ctx context.Context) error {
	logging.Printf("Stopping media stream")
	s.stopOnce.Do(func() {
		close(s.quit)
		// Close listener
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logging.Printf("[TCPService] error closing listener: %v", err)
			}
		}
	})

	// Close all open TCP connections
	s.tracker.CloseAll()

	// Wait for go routines to vacate
	done := make(chan any)
	go func() {
		logging.Printf("Waiting for Media routines to vacate")
		s.wg.Wait()
		logging.Printf("Media routines exited")
		close(done)
	}()

	// Either timeout with context or go routines vacate and we close normally
	select {
	case <-ctx.Done():
		logging.Printf("Media stream ctx timeout")
		return ctx.Err()
	case <-done:
		logging.Printf("Media stream closed through done channel")
		if s.Errors != nil {
			close(s.Errors)
		}
		return nil
	}
}
