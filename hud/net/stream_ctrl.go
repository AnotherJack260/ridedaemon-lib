package net

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

const mediaCtrlHeaderSize = 8

// Command requests
const (
	MediaCtrlInit       uint16 = 16
	MediaCtrlScreenConf uint16 = 96
	MediaCtrlChk        uint16 = 112
	MediaCtrlPing       uint16 = 64
)

// Video codec ids carried in REQ_RV_CONFIG_CAPTURE's wantEncoder and echoed in the reply, as
// Carbit Ride 2.4 numbers them (utils/Protocol.java:34-42): JPEG_CODEC=1, H264_CODEC=2,
// MP4_CODEC=4. 4 is not H.265: it is Carbit's software "MP4" encoder in libJNICodec.so
// (base/l0.java:615-630), which nothing here can produce. ECTinyPlus.proto's VideoCodecType
// (MP4=3) is a different enum; the mirror sender branches on Carbit's.
const (
	mediaEncoderJpeg uint32 = 1
	mediaEncoderH264 uint32 = 2
	mediaEncoderMP4  uint32 = 4
)

// Command responses
const (
	MediaCtrlAck       uint16 = 17
	MediaCtrlViewState uint16 = 97
	MediaCtrlRcv       uint16 = 113
	MediaCtrlPong      uint16 = 65
)

// Commands only carbit-exact media control answers differently (utils/Protocol.java:43-77).
const (
	MediaCtrlScreenEvent  uint16 = 32     // REQ_SCREEN_EVENT, the dash's touch
	MediaCtrlGetVersion   uint16 = 48     // REQ_GET_VERSION
	MediaCtrlVersion      uint16 = 49     // RLY_GET_VERSION
	MediaCtrlExecuteShell uint16 = 272    // REQ_EXECUTE_SHELL
	MediaCtrlShellResult  uint16 = 273    // RLY_EXECUTE_SHELL
	MediaCtrlUnsupported  uint16 = 0x7F02 // RLY_UN_SUPPORT
)

// carbitVersion is Carbit's 0x31 body: u32 5002, u32 1 (base/l0.java:475-481).
var carbitVersion = []byte{0x8A, 0x13, 0, 0, 1, 0, 0, 0}

// CaptureConfig is the dash's REQ_RV_CONFIG_CAPTURE (media cmd 0x10) read at the offsets Carbit
// reads it (utils/Protocol.java:266-310), plus what this side answered and what the dash's 0x60
// added. Raw values: no Carbit defaults are applied here (see the host's encoder plan for those).
type CaptureConfig struct {
	Width                 int `json:"width"`                 // @0 u16
	Height                int `json:"height"`                // @2 u16
	WantFps               int `json:"wantFps"`               // @4 i32; 0 = the dash polls
	WantEncoder           int `json:"wantEncoder"`           // @8 i32, as sent
	SupportCodec          int `json:"supportCodec"`          // @12 i32
	MinQuality            int `json:"minQuality"`            // @16 u16
	MaxQuality            int `json:"maxQuality"`            // @18 u16, a JPEG's byte budget
	BitRate               int `json:"bitRate"`               // @20 i32, bps
	CapScreenMode         int `json:"capScreenMode"`         // @24
	TouchMode             int `json:"touchMode"`             // @25
	Orientation           int `json:"orientation"`           // @26: 0 default, 1 land, 2 port, 3/4 round land/port
	DisplayID             int `json:"displayId"`             // @27
	VideoType             int `json:"videoType"`             // @28
	SupportExtendProtocol int `json:"supportExtendProtocol"` // @29

	// What the 0x11 reply said.
	Encoder       int `json:"encoder"`
	CaptureWidth  int `json:"captureWidth"`
	CaptureHeight int `json:"captureHeight"`

	// From the dash's 0x60. MovieMode is in effect only when carbit-exact media control echoed
	// supportFunction bit 0 back (base/l0.java:264-267); it is never on otherwise.
	SupportFunction int  `json:"supportFunction"`
	MovieMode       bool `json:"movieMode"`
}

func parseCaptureConfig(b []byte) CaptureConfig {
	u8 := func(at int) int {
		if len(b) > at {
			return int(b[at])
		}
		return 0
	}
	u16 := func(at int) int {
		if len(b) >= at+2 {
			return int(binary.LittleEndian.Uint16(b[at:]))
		}
		return 0
	}
	i32 := func(at int) int {
		if len(b) >= at+4 {
			return int(int32(binary.LittleEndian.Uint32(b[at:])))
		}
		return 0
	}
	return CaptureConfig{
		Width: u16(0), Height: u16(2), WantFps: i32(4), WantEncoder: i32(8), SupportCodec: i32(12),
		MinQuality: u16(16), MaxQuality: u16(18), BitRate: i32(20),
		CapScreenMode: u8(24), TouchMode: u8(25), Orientation: u8(26), DisplayID: u8(27),
		VideoType: u8(28), SupportExtendProtocol: u8(29),
	}
}

// CarbitAlign16 is Carbit's capture-size rounding: to the nearest multiple of 16, a remainder of
// exactly 8 rounding down (utils/Protocol.java:143-159, then & -16 at :201 and base/l0.java:188).
func CarbitAlign16(v int) int {
	if v%16 > 8 {
		v += 15
	}
	return v &^ 15
}

// CarbitEncoder is the encoder Carbit answers: wantEncoder 0 is read as 1 (utils/Protocol.java:
// 276-279), then only 1, 2 and 4 are echoed and anything else is answered 0 (base/l0.java:190-198).
func CarbitEncoder(want int) int {
	switch uint32(want) {
	case 0, mediaEncoderJpeg:
		return int(mediaEncoderJpeg)
	case mediaEncoderH264, mediaEncoderMP4:
		return want
	}
	return 0
}

// parseCaptureExtend reads the dash's 0x60 JSON as Carbit does: supportFunction with optInt, and
// whether viewAreaConfig is an object (utils/Protocol.java:415-440). Unreadable means 0 / absent.
func parseCaptureExtend(b []byte) (supportFunction int, viewArea bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return 0, false
	}
	var sf float64
	_ = json.Unmarshal(fields["supportFunction"], &sf) // ponytail: optInt also reads "1" strings; no dash sends one
	return int(sf), bytes.HasPrefix(fields["viewAreaConfig"], []byte("{"))
}

type MediaCtrlResponse struct {
	Command uint16
	Size    uint16
	Padding uint32
	Payload []byte
}

type ViewConfig struct {
	State int `json:"state"`
}

type View struct {
	ViewAreaConfig  *ViewConfig `json:"viewAreaConfig,omitempty"`
	SupportFunction int         `json:"supportFunction"`
}

type MediaControl struct {
	port     string
	quit     chan any
	wg       sync.WaitGroup
	listener net.Listener
	tracker  *ConnTracker

	stopOnce sync.Once

	Errors          chan error
	Events          chan MediaCtrlResponse
	OnVideoStart    func()
	SupportFunction int
	// JpegStills answers REQ_RV_CONFIG_CAPTURE with encoder=1 (JPEG) whatever the dash
	// asked for. It is an experiment, not a negotiation: the reply is where the official
	// EasyConn app writes the encoder it will actually use, and one dash family asks for
	// H.264, drains the stream and paints nothing. See buildMediaCaptureAckPayload.
	JpegStills bool
	// OnCaptureNegotiated reports the supportExtendProtocol byte the dash asked for
	// and we echoed back, once the capture-config reply is on the wire.
	OnCaptureNegotiated func(extended bool)

	// CarbitExact answers this channel the way Carbit Ride 2.4 does where ridedaemon always
	// answered otherwise: 0x31 carries {5002, 1}, touch (0x20) gets no reply, 0x110 gets a
	// shell answer, anything unknown gets 0x7F02 instead of an empty cmd+1, 0x11 uses Carbit's
	// encoder mapping and size rounding, and 0x61 echoes the dash's own supportFunction (which
	// can turn on movie mode). Off by default: CFMOTO-lineage dashes were driven for years on
	// the cmd+1 answers and some touch dashes may count on the 0x21. Set it before Start, for
	// dashes whose official app is Carbit Ride / EasyConn (flavor 51, e.g. the Zontes 350D).
	CarbitExact bool
	// PhoneModel and PhoneOsVersion answer the dash's getprop shell commands (CarbitExact only).
	PhoneModel     string
	PhoneOsVersion string
	// OnCaptureConfig receives every CAPTURE_CONFIG with what it was answered, before the reply
	// is written (Carbit stores it first too, base/l0.java:235-238), and the same config again
	// whenever a 0x60 updates it. Called from the connection's goroutine.
	OnCaptureConfig func(CaptureConfig)

	captureMu sync.Mutex
	capture   *CaptureConfig
}

func NewMediaControl(port string) *MediaControl {
	return &MediaControl{
		port:    port,
		quit:    make(chan any),
		tracker: NewConnTracker(),
		Errors:  make(chan error, 16),
		Events:  make(chan MediaCtrlResponse, 16),
	}
}

func (s *MediaControl) emitEvent(evt MediaCtrlResponse) {
	select {
	case s.Events <- evt:
	default:
		// channel full, drop
	}
}

func (s *MediaControl) emitError(err error) {
	select {
	case s.Errors <- err:
	default:
		logging.Printf("Dropping error from MediaControl [No one's listening!]: %v", err)
	}
}

// connectionError decides whether one connection's failure ends the session, on the same rule the
// PXC server has used since a dash's abandoned CAR_DATA channel was found tearing down working
// sessions: fatal only when nothing else is being served here.
//
// The bike is KNOWN to open :10922 twice and abandon one of them; whether it does the same here has
// not been proven on a dash, so this is the same rule applied to the same shape of listener rather
// than a fix for a failure already seen on :10921. It can only ever delay a verdict, never invent
// one: a dash that really goes away kills every connection, and whichever one notices last finds
// itself alone and says so.
//
// The fatal text is unchanged, character for character: failures are grouped downstream by the
// text of that line, so the sentence a real death produces has to go on reading the same.
func (s *MediaControl) connectionError(conn net.Conn, errType CtrlErrorType, cause error) *CtrlError {
	if s.tracker.Others(conn) == 0 {
		return &CtrlError{errType, cause, true}
	}
	// The host relays a non-fatal transport error to the rider as a notice, so this one says what
	// happened before it says what the socket complained about.
	return &CtrlError{
		errType,
		fmt.Errorf(
			"one media control channel closed, %d still serving the dash: %v",
			s.tracker.Others(conn), cause,
		),
		false,
	}
}

// isStopping reports our own teardown, so the read that CloseAll is about to interrupt does not
// come back as a fault. Without it every stop produced a fatal "use of closed network connection"
// on this port - the first line of a field teardown (2026-08-26), and the one that made a
// dash-side death look like ours.
func (s *MediaControl) isStopping() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *MediaControl) decodeHeader(b []byte) (*MediaCtrlResponse, error) {
	if len(b) < mediaCtrlHeaderSize {
		return nil, errors.New("invalid header")
	}
	return &MediaCtrlResponse{
		Command: binary.LittleEndian.Uint16(b[0:2]),
		Size:    binary.LittleEndian.Uint16(b[2:4]),
		Padding: binary.LittleEndian.Uint32(b[4:8]),
	}, nil
}

func (s *MediaControl) acceptLoop() {
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

		logging.Printf("New MediaControl client from %s", conn.RemoteAddr())
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// writeResponse writes the 8-byte header in one write and the body in a second, as Carbit does
// (sdk_communication/b.java:89-112). It used to write the header as three fields, which with
// TCP_NODELAY (Go's default) can reach a dash as three segments of 2, 2 and 4 bytes.
func (s *MediaControl) writeResponse(res *MediaCtrlResponse, conn net.Conn) error {
	header := make([]byte, mediaCtrlHeaderSize)
	binary.LittleEndian.PutUint16(header[0:2], res.Command)
	binary.LittleEndian.PutUint16(header[2:4], res.Size)
	binary.LittleEndian.PutUint32(header[4:8], res.Padding)
	if _, err := conn.Write(header); err != nil {
		return err
	}

	// -- write payload
	if len(res.Payload) > 0 {
		if _, err := conn.Write(res.Payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *MediaControl) handleEvent(event *MediaCtrlResponse, conn net.Conn) {
	switch event.Command {
	case MediaCtrlInit:
		s.emitEvent(*event)
		payload := buildMediaCaptureAckPayload(event.Payload, s.JpegStills, s.CarbitExact)
		s.captureNegotiated(event.Payload, payload)
		response := &MediaCtrlResponse{Command: MediaCtrlAck, Size: uint16(len(payload)), Payload: payload}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
		// Read the framing decision from the payload we built, not from its length:
		// an 8-byte reply is now the normal shape for a dash that asked for no extend
		// protocol, and treating it as "nothing to report" would have silently stopped
		// the transport from being told which framing this session uses.
		if s.OnCaptureNegotiated != nil {
			s.OnCaptureNegotiated(len(payload) >= 9 && payload[8] != 0)
		}
	case MediaCtrlScreenConf:
		s.emitEvent(*event)
		supportFunction, viewArea := parseCaptureExtend(event.Payload)
		viewState := View{
			ViewAreaConfig:  &ViewConfig{State: 0},
			SupportFunction: s.SupportFunction,
		}
		if s.CarbitExact {
			// Carbit echoes the dash's own value and answers viewAreaConfig only when asked
			// (base/l0.java:254-266).
			viewState.SupportFunction = supportFunction
			if !viewArea {
				viewState.ViewAreaConfig = nil
			}
		}
		s.extendNegotiated(supportFunction)
		var payload []byte
		if p, err := json.Marshal(viewState); err != nil {
			s.emitError(&CtrlError{CtrlDecodeErr, err, true})
			break
		} else {
			payload = p
		}
		response := &MediaCtrlResponse{Command: MediaCtrlViewState, Size: uint16(len(payload)), Payload: payload}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	case MediaCtrlChk:
		if s.OnVideoStart != nil {
			s.OnVideoStart()
		}
		s.emitEvent(*event)
		response := &MediaCtrlResponse{Command: MediaCtrlRcv, Size: 0}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	case MediaCtrlPing:
		response := &MediaCtrlResponse{Command: MediaCtrlPong, Size: 0}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	default:
		s.emitEvent(*event)
		// Try to send a default command + 1 empty response
		response := &MediaCtrlResponse{Command: event.Command + 1, Size: 0}
		if s.CarbitExact {
			response = s.carbitReply(event)
		}
		if response == nil {
			break
		}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	}
}

// carbitReply is Carbit's answer to a command ridedaemon otherwise acks with an empty cmd+1
// (base/l0.java:465-498), or nil where Carbit answers nothing.
func (s *MediaControl) carbitReply(event *MediaCtrlResponse) *MediaCtrlResponse {
	var command uint16
	var body []byte
	switch event.Command {
	case MediaCtrlScreenEvent:
		return nil // injected as touch, never answered (base/l0.java:471-473)
	case MediaCtrlGetVersion:
		command, body = MediaCtrlVersion, carbitVersion
	case MediaCtrlExecuteShell:
		// Two getprops it knows, "OK" for the rest, NUL-terminated; the "release" it gives is
		// the SDK level (base/l0.java:275-291).
		answer := "OK"
		switch cmd := string(event.Payload); {
		case strings.EqualFold(cmd, "getprop ro.build.version.release"):
			answer = s.PhoneOsVersion
		case strings.EqualFold(cmd, "getprop ro.product.model"):
			answer = s.PhoneModel
		}
		command, body = MediaCtrlShellResult, append([]byte(answer), 0)
	default:
		command = MediaCtrlUnsupported // empty RLY_UN_SUPPORT (base/l0.java:496)
	}
	return &MediaCtrlResponse{Command: command, Size: uint16(len(body)), Payload: body}
}

// captureNegotiated records a CAPTURE_CONFIG with what it is being answered and tells the host. A
// new one turns movie mode off until a 0x60 turns it on again (base/l0.java:136-138).
func (s *MediaControl) captureNegotiated(request, reply []byte) {
	c := parseCaptureConfig(request)
	c.Encoder = int(binary.LittleEndian.Uint32(reply[0:4]))
	c.CaptureWidth = int(binary.LittleEndian.Uint16(reply[4:6]))
	c.CaptureHeight = int(binary.LittleEndian.Uint16(reply[6:8]))
	s.captureMu.Lock()
	s.capture = &c
	s.captureMu.Unlock()
	if s.OnCaptureConfig != nil {
		s.OnCaptureConfig(c)
	}
}

// extendNegotiated adds a 0x60's supportFunction to the current CAPTURE_CONFIG. Movie mode
// follows bit 0 only where it is echoed back (base/l0.java:264-267). Before any CAPTURE_CONFIG
// there is nothing to update, and the next one would clear it anyway.
func (s *MediaControl) extendNegotiated(supportFunction int) {
	s.captureMu.Lock()
	if s.capture == nil {
		s.captureMu.Unlock()
		return
	}
	c := *s.capture
	c.SupportFunction = supportFunction
	c.MovieMode = s.CarbitExact && supportFunction&1 == 1
	s.capture = &c
	s.captureMu.Unlock()
	if s.OnCaptureConfig != nil {
		s.OnCaptureConfig(c)
	}
}

// buildMediaCaptureAckPayload builds RLY_RV_CONFIG_CAPTURE, the reply that tells the dash
// which encoder, geometry and framing the phone will actually use.
//
// forceJpeg answers encoder=1 (VideoCodecType.JEPG in EasyConn's own ECTinyPlus.proto:
// NONE=0, JEPG=1, H264=2, MP4=3) even when the dash asked for H.264. That is deliberate and
// is the whole experiment: the official app dispatches its mirror sender on this field, and
// a dash that asks for H.264 while painting none of the H.264 it pulls is the one case where
// offering it the other format is worth a rider's session. Everything else keeps the echo.
//
// carbitExact answers the encoder and size the way Carbit does (CarbitEncoder, CarbitAlign16)
// instead of echoing any non-zero encoder, turning 0 into H.264 and flooring the size to 16.
func buildMediaCaptureAckPayload(request []byte, forceJpeg, carbitExact bool) []byte {
	encoder := mediaEncoderH264
	width := uint16(800)
	height := uint16(384)
	extendedProtocol := byte(1)

	align := func(v uint16) uint16 { return v &^ 0x0f }
	if carbitExact {
		align = func(v uint16) uint16 { return uint16(CarbitAlign16(int(v))) }
	}
	if len(request) >= 4 {
		requestedWidth := binary.LittleEndian.Uint16(request[0:2])
		requestedHeight := binary.LittleEndian.Uint16(request[2:4])
		if aligned := align(requestedWidth); aligned >= 16 {
			width = aligned
		}
		if aligned := align(requestedHeight); aligned >= 16 {
			height = aligned
		}
	}
	if len(request) >= 12 {
		requestedEncoder := binary.LittleEndian.Uint32(request[8:12])
		if carbitExact {
			encoder = uint32(CarbitEncoder(int(int32(requestedEncoder))))
		} else if requestedEncoder != 0 {
			encoder = requestedEncoder
		}
	}
	if len(request) >= 30 {
		extendedProtocol = request[29]
	}
	if forceJpeg {
		encoder = mediaEncoderJpeg
	}

	// Protocol.RlyConfigCapture.size() in the CarbitRide APK returns 8 unless
	// supportExtendProtocol is non-zero, and toByteArray() appends the byte only in
	// that case. This built a 9-byte reply either way, so every dashboard that runs
	// without the extend protocol - which is the whole family that reports
	// supportExtendProtocol=0, a QJ 5-inch panel among them - was being handed a
	// trailing zero the official app never sends.
	size := 8
	if extendedProtocol != 0 {
		size = 9
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint32(payload[0:4], encoder)
	binary.LittleEndian.PutUint16(payload[4:6], width)
	binary.LittleEndian.PutUint16(payload[6:8], height)
	if size == 9 {
		payload[8] = extendedProtocol
	}
	logging.Printf(
		"Media capture negotiated encoder=%d width=%d height=%d extended=%d",
		encoder,
		width,
		height,
		extendedProtocol,
	)
	return payload
}

// Handling individual TCP Connection/Client
func (s *MediaControl) handleConn(conn net.Conn) {
	s.tracker.Add(conn)
	defer func() {
		s.tracker.Remove(conn)
		_ = conn.Close()
		s.wg.Done()
	}()

	reader := bufio.NewReader(conn)

	// Retire the connection before judging its failure, never after: when the dash goes away every
	// channel fails at once and whichever one is judged last has to find an empty tracker.
	fail := func(errType CtrlErrorType, cause error) {
		if s.isStopping() {
			return
		}
		s.tracker.Remove(conn)
		s.emitError(s.connectionError(conn, errType, cause))
	}

	logging.Printf("Starting StreamCtrl Loop")
	defer logging.Printf("Stopping StreamCtrl Loop")
	for {
		var request *MediaCtrlResponse

		// Read 8 byte header
		headerBytes := make([]byte, mediaCtrlHeaderSize)
		if n, err := io.ReadFull(reader, headerBytes); err != nil {
			fail(CtrlDecodeErr, fmt.Errorf("error reading header: %v (read %d bytes: %x)", err, n, headerBytes[:n]))
			return
		}
		if req, err := s.decodeHeader(headerBytes); err != nil {
			fail(CtrlDecodeErr, fmt.Errorf("error decoding header: %v", err))
			return
		} else {
			request = req
		}

		// Read body if payload is greater than 0
		var payload []byte
		if request.Size > 0 {
			payload = make([]byte, request.Size)
			if _, err := io.ReadFull(reader, payload); err != nil {
				fail(CtrlDecodeErr, fmt.Errorf("[MediaControl] read payload failed from %s: %v", conn.RemoteAddr(), err))
				return
			}
			request.Payload = payload
		}

		// Decide what to do with the request
		s.handleEvent(request, conn)
	}
}

func (s *MediaControl) Start() error {
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

func (s *MediaControl) Stop(ctx context.Context) error {
	logging.Printf("Stopping media control")
	// Signal connection accept loop to stop
	s.stopOnce.Do(func() {
		close(s.quit)
		// Close listener
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logging.Printf("[TCPService] error closing listener: %v", err)
			}
		}
	})

	// Close all tcp connections
	s.tracker.CloseAll()

	// Wait for go routines to vacate
	done := make(chan any)
	go func() {
		logging.Printf("Waiting for Stream Ctrl routines to vacate")
		s.wg.Wait()
		logging.Printf("Stream Ctrl routines vacated")
		close(done)
	}()

	// Either timeout with context or go routines vacate and we close normally
	select {
	case <-ctx.Done():
		logging.Printf("Stream Ctrl ctx timeout")
		return ctx.Err()
	case <-done:
		logging.Printf("Stream Ctrl closed through done channel")
		if s.Errors != nil {
			close(s.Errors)
		}
		if s.Events != nil {
			close(s.Events)
		}
		return nil
	}
}
