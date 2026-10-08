package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	stdnet "net"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
	"github.com/google/uuid"
	"github.com/grandcat/zeroconf"
)

type HudEventSource int

const (
	UnknownEventSource HudEventSource = iota + 1
	EventSourcePXC
	EventSourceMediaControl
	// EventSourceTransport carries this transport's own decisions (not dash
	// traffic), so they show up in the phone's field logs.
	EventSourceTransport
)

// TransportCmdVideoFraming reports the video frame format negotiated from the
// dash's supportExtendProtocol byte. Payload: [extendedByte, plainFramingApplied],
// one byte each, 0 or 1.
const TransportCmdVideoFraming = 1

// TransportCmdVideoPulls reports how many frames the dash has actually asked for on
// the data socket. Payload: [phase, 8 bytes big-endian pull count], where phase is a
// net.VideoPullPhase.
//
// Nothing else in the stack can answer this. The phone counts what it pushes into
// this library and calls that delivery; a dash that opens every socket, negotiates
// the picture size, says STREAM_START and then never polls produces exactly the same
// numbers as one that renders perfectly. Several riders have spent months in that
// gap, and it can only be closed from here.
const TransportCmdVideoPulls = 2

// TransportCmdPageSwitchProbe reports one step of the phone-to-car page experiment.
// Payload: [step, ok, 4 bytes big-endian command], where step is a
// net.PageSwitchProbeStep and ok is 1 when the command reached the wire.
//
// The commands it sends move the DASH's own UI, so the only instrument that can read
// the result is the rider looking at the panel. These events exist to put a timestamp
// on each one, so "it lit up" can be matched to the command that preceded it.
const TransportCmdPageSwitchProbe = 3

// TransportCmdAppStatusNotify reports one ECP_P2C_APPSTATUS_BACKGROUND this session put
// on the wire: the mode in the first byte, 1 in the second if the write reached the
// socket. See hud/net/appstatus.go for why the phone sends it at all. The dash's own
// 0x20031 acknowledgement is not this event - it arrives through the PXC event stream
// like any other response, which is what makes it worth reading.
const TransportCmdAppStatusNotify = 4

// TransportCmdKeyframeNeeded says the live video queue overflowed with no IDR left in it and
// is dropping until the next one, so the host should ask its encoder for a sync frame now.
// Payload: 4 bytes big-endian, how many access units were dropped. At most once a second.
const TransportCmdKeyframeNeeded = 5

// Transport events that report a dash request carry that request's PXC command number, so a
// feature added in hud/net (RegisterC2PHandler + emitTransport) never needs a number from here.

type HudEvent struct {
	Source HudEventSource
	Time   time.Time
	Cmd    int
	Data   any
}

type StoppableServer interface {
	Start() error
	Stop(ctx context.Context) error
}

type EcHost struct {
	Ip      string
	Port    string
	Package string
}

type CfmotoHUD struct {
	// State
	mu      sync.Mutex
	running bool

	// Config
	host                  *EcHost
	targetFPS             int
	packageName           string
	phoneUUID             uuid.UUID
	phoneConfig           *net.PhoneConfig
	supportFunction       int
	proactivePxcHeartbeat bool
	plainVideoFraming     bool
	pageSwitchProbe       bool
	jpegStills            bool
	appStatusNotify       bool
	noFlavor51Lifecycle   bool
	appForeground         bool
	phoneScreen           net.PhoneScreen
	timeZoneID            string
	timeZoneOffsetSec     int
	timeZoneOffsetSet     bool
	skipDashClockSync     bool
	dashAsksForTime       bool

	// net management
	keyPair      *net.KeyPair
	ecService    *net.ECService
	pxcControl   *net.PXCControl
	mediaControl *net.MediaControl
	mediaStream  *net.MediaStream

	// Streaming
	muxSource *stream.MuxSource

	// Communications
	stopped  chan any
	stopOnce sync.Once
	Events   chan HudEvent
	Errors   chan error
}

func NewCfmotoHUD(targetFPS int, mux *stream.MuxSource, supportFunction int) *CfmotoHUD {
	if targetFPS <= 0 {
		targetFPS = 30
	}
	id := uuid.New()
	pkg := "com.cfmoto.cfmotointernational"
	return &CfmotoHUD{
		packageName:     pkg,
		phoneUUID:       id,
		supportFunction: supportFunction,
		phoneConfig: &net.PhoneConfig{
			PxcVersion:            "1.0.2",
			PhoneUUID:             id.String(),
			PhoneBrand:            "google",
			PhoneModel:            "Pixel 6a",
			PhoneOsVersion:        "36",
			PhoneOs:               "Android",
			Package:               pkg,
			VersionCode:           121,
			Token:                 0,
			Pubkey:                "", // Done inside PXC
			EncryptedHUID:         "", // Done inside PXC
			BluetoothName:         "Pixel 6a",
			SupportH264IFrame:     true,
			SupportFunction:       supportFunction,
			AppVersionFingerPrint: "V:2.2.1(121)--ONLINE",
		},
		targetFPS: targetFPS,
		muxSource: mux,
		stopped:   make(chan any),
		Events:    make(chan HudEvent, 32),
		Errors:    make(chan error, 32),
	}
}

// SetProactivePxcHeartbeat enables the dual CAR_CTRL/CAR_DATA keepalive used by
// firmware that tears down idle PXC channels. Configure it before StartStream.
func (hud *CfmotoHUD) SetProactivePxcHeartbeat(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.proactivePxcHeartbeat = enabled
}

// SetPlainVideoFramingAllowed lets a dash that reports supportExtendProtocol=0 pull
// un-indexed video frames. The host only enables it for firmware it could not
// identify at all, so no recognised dashboard can change format. Configure it
// before StartStream.
func (hud *CfmotoHUD) SetPlainVideoFramingAllowed(allowed bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.plainVideoFraming = allowed
}

// SetPageSwitchProbe enables the phone-to-car page sequence once the dash says
// STREAM_START. Off by default and meant for one dashboard family that pulls the whole
// stream and paints none of it; see net.SetPageSwitchProbe for what goes on the wire
// and why no reference implementation sends it. Configure it before StartStream.
func (hud *CfmotoHUD) SetPageSwitchProbe(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.pageSwitchProbe = enabled
}

// SetJpegStills answers the capture negotiation with encoder=1 (JPEG) instead of echoing the
// encoder the dash asked for, and tells the live source its payloads are whole pictures rather
// than H.264 access units. The phone must be feeding stills when this is on - the daemon does
// not transcode anything, it only changes what is negotiated and what the queue will accept.
// Configure it before StartStream.
func (hud *CfmotoHUD) SetJpegStills(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.jpegStills = enabled
}

// SetAppStatusNotify makes the phone announce its mirroring state to the head unit the
// way the official app does - once when PXC comes up, and again when the dash starts
// pulling frames. Off by default; configure it before StartStream. See
// hud/net/appstatus.go.
func (hud *CfmotoHUD) SetAppStatusNotify(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.appStatusNotify = enabled
}

// SetFlavor51Lifecycle controls the official app's lifecycle commands a flavor-51 dash gets by
// default. See net.PXCControl.SetFlavor51Lifecycle. Configure it before StartStream.
func (hud *CfmotoHUD) SetFlavor51Lifecycle(enabled bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.noFlavor51Lifecycle = !enabled
}

// SetAppForeground says whether the host's own UI is in the phone's foreground, which turns the
// APPSTATUS notification into 0x20020. Takes effect at once on a running session.
func (hud *CfmotoHUD) SetAppForeground(foreground bool) {
	hud.mu.Lock()
	hud.appForeground = foreground
	pxc := hud.pxcControl
	hud.mu.Unlock()
	if pxc != nil {
		pxc.SetAppForeground(foreground)
	}
}

// SetPhoneScreen supplies the display metrics that notification carries. Only the host
// can know them. Configure it before StartStream.
func (hud *CfmotoHUD) SetPhoneScreen(width, height, rotation int) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.phoneScreen = net.PhoneScreen{Width: width, Height: height, Rotation: rotation}
}

// SetTimeZoneID supplies the host's IANA zone id for the PXC QUERY_TIME reply.
// Configure it before StartStream.
func (hud *CfmotoHUD) SetTimeZoneID(id string) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.timeZoneID = id
}

// SetTimeZoneOffsetSeconds supplies the host's UTC offset with DST applied, so
// the clock replies carry the rider's wall clock and not Go's UTC one. See
// net.PXCControl.SetTimeZoneOffsetSeconds. Configure it before StartStream.
func (hud *CfmotoHUD) SetTimeZoneOffsetSeconds(seconds int) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.timeZoneOffsetSec = seconds
	hud.timeZoneOffsetSet = true
}

// SetSkipDashClockSync answers QUERY_TIME with an empty ACK and does not push
// unsolicited clock JSON. See net.PXCControl.SetSkipDashClockSync. Configure
// it before StartStream.
func (hud *CfmotoHUD) SetSkipDashClockSync(skip bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.skipDashClockSync = skip
}

// SetDashAsksForTime suppresses the unsolicited clock push for a dashboard the
// host has already seen asking for the time. See net.PXCControl.SetDashAsksForTime.
// Configure it before StartStream.
func (hud *CfmotoHUD) SetDashAsksForTime(asks bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.dashAsksForTime = asks
}

func (hud *CfmotoHUD) handleServerEvent(evt any) {
	now := time.Now()
	hudEvent := HudEvent{Source: UnknownEventSource, Time: now, Data: evt}
	switch e := evt.(type) {
	case net.PXCResponse:
		hudEvent.Source = EventSourcePXC
		hudEvent.Cmd = int(e.Command)
		hudEvent.Data = e.Body
	case net.MediaCtrlResponse:
		hudEvent.Source = EventSourceMediaControl
		hudEvent.Cmd = int(e.Command)
		hudEvent.Data = e.Payload
	case HudEvent:
		// Already shaped (transport-originated events); forward as-is.
		hudEvent = e
	}

	select {
	case hud.Events <- hudEvent:
	default:
	}
}

func (hud *CfmotoHUD) isFatalErr(err error) bool {
	var fe net.FatalError
	if errors.As(err, &fe) {
		return fe.IsFatal()
	}
	return false
}

func (hud *CfmotoHUD) handleServerError(err error) {
	if err == nil {
		return
	}

	// Bubble up error
	select {
	case hud.Errors <- err:
	default:
	}
	if hud.isFatalErr(err) {
		// Fatal, we need to exit cleanly
		go func() {
			ctxWithTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = hud.StopStream(ctxWithTimeout)
		}()
	}
}

func (hud *CfmotoHUD) startPxcEventFwd(s *net.PXCControl, ready chan<- any) {
	// One consumer must both signal readiness and forward diagnostics. Multiple
	// consumers would race because a Go channel delivers each event only once.
	go func() {
		for evt := range s.Events {
			if evt.Command == net.PxcHeartbeat || evt.Command == net.PxcHeartbeatOk {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
			hud.handleServerEvent(evt)
		}
	}()
}

func (hud *CfmotoHUD) startMediaEventFwd(s *net.MediaControl) {
	// Media Ctrl events
	go func() {
		for evt := range s.Events {
			hud.handleServerEvent(evt)
		}
	}()
}

func (hud *CfmotoHUD) SearchForHost(ctx context.Context, timeout time.Duration) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var err error
	var mdns *net.MDNSService
	var entries <-chan *zeroconf.ServiceEntry

	if mdns, err = net.NewMDNSService(); err != nil {
		return err
	}
	if entries, err = mdns.LookupEntries(); err != nil {
		return err
	}

	go func() {
		mdnsErr := mdns.Browse(ctxWithTimeout, "_EasyConn._tcp", "local")
		if mdnsErr != nil {
			logging.Printf("error while browsing for mdns host: %s", mdnsErr)
		}
	}()

	// Find the display service IP, port and appropriate package name
	reService := regexp.MustCompile("^packagename=(" + hud.packageName + ")$")
	reIP := regexp.MustCompile("^ip=(.*)$")

	for {
		select {
		case <-ctxWithTimeout.Done():
			if errors.Is(ctxWithTimeout.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("search for host timed out after: %s", timeout)
			}
			return ctxWithTimeout.Err()
		case entry, ok := <-entries:
			if !ok {
				if ctxWithTimeout.Err() != nil {
					return ctxWithTimeout.Err()
				}
				return errors.New("mDNS entries channel closed before host found")
			}

			var ip, ecPort, packageName string

			for _, txt := range entry.Text {
				if reService.MatchString(txt) {
					sub := reService.FindStringSubmatch(txt)
					logging.Printf("Found EC service package: %s", sub[1])
					ecPort = strconv.Itoa(entry.Port)
					packageName = sub[1]
				}
				if reIP.MatchString(txt) {
					sub := reIP.FindStringSubmatch(txt)
					logging.Printf("Found EC IP package: %s", sub[1])
					ip = sub[1]
				}
			}

			if ip != "" && ecPort != "" {
				// Found what we need
				hud.host = &EcHost{
					Ip:      ip,
					Port:    ecPort,
					Package: packageName,
				}

				// Stop browsing as soon as we’ve found the host.
				cancel()

				return nil
			}
		}
	}
}

func (hud *CfmotoHUD) StartStream(ctx context.Context) (err error) {
	return hud.startStream(ctx, nil)
}

// StartStreamWithInitConn opens the reverse HUD servers before using a caller-routed EC handshake.
func (hud *CfmotoHUD) StartStreamWithInitConn(ctx context.Context, initConn stdnet.Conn) (err error) {
	return hud.startStream(ctx, initConn)
}

func (hud *CfmotoHUD) startStream(ctx context.Context, initConn stdnet.Conn) (err error) {
	hud.mu.Lock()
	if hud.running {
		hud.mu.Unlock()
		return errors.New("already running")
	}
	if hud.host == nil {
		hud.mu.Unlock()
		return errors.New("host is not set or has not been found")
	}
	if hud.muxSource == nil {
		hud.mu.Unlock()
		return errors.New("mux source is not set")
	}
	hud.mu.Unlock()

	if hud.keyPair, err = net.GenKeyPair(); err != nil {
		return err
	}

	var started []StoppableServer

	// If err != nil on exit, stop everything still running
	defer func() {
		if err == nil || len(started) == 0 {
			return
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// stop reverse order
		for i := len(started) - 1; i >= 0; i-- {
			if stopErr := started[i].Stop(shutdownCtx); stopErr != nil {
				logging.Printf("Error stopping server %T: %v", started[i], stopErr)
			}
		}

		hud.mu.Lock()
		hud.running = false
		hud.mu.Unlock()
	}()

	pxcReady := make(chan any, 1)
	pxcServer := net.NewPXCControl(":10922", hud.keyPair, hud.phoneConfig)
	pxcServer.SetProactiveHeartbeat(hud.proactivePxcHeartbeat)
	pxcServer.SetPageSwitchProbe(hud.pageSwitchProbe)
	pxcServer.OnPageSwitchProbe = func(step net.PageSwitchProbeStep, command uint32, err error) {
		payload := make([]byte, 6)
		payload[0] = byte(step)
		if err == nil {
			payload[1] = 1
		}
		binary.BigEndian.PutUint32(payload[2:], command)
		hud.handleServerEvent(HudEvent{
			Source: EventSourceTransport,
			Time:   time.Now(),
			Cmd:    TransportCmdPageSwitchProbe,
			Data:   payload,
		})
	}
	pxcServer.SetAppStatusNotify(hud.appStatusNotify)
	pxcServer.SetFlavor51Lifecycle(!hud.noFlavor51Lifecycle)
	hud.mu.Lock()
	foreground := hud.appForeground // the one setter the host may call while this runs
	hud.mu.Unlock()
	pxcServer.SetAppForeground(foreground)
	pxcServer.SetPhoneScreen(hud.phoneScreen)
	pxcServer.OnAppStatus = func(mode int, err error) {
		payload := make([]byte, 2)
		payload[0] = byte(mode)
		if err == nil {
			payload[1] = 1
		}
		hud.handleServerEvent(HudEvent{
			Source: EventSourceTransport,
			Time:   time.Now(),
			Cmd:    TransportCmdAppStatusNotify,
			Data:   payload,
		})
	}
	pxcServer.OnTransport = func(cmd int, payload []byte) {
		hud.handleServerEvent(HudEvent{Source: EventSourceTransport, Time: time.Now(), Cmd: cmd, Data: payload})
	}
	pxcServer.SetTimeZoneID(hud.timeZoneID)
	if hud.timeZoneOffsetSet {
		pxcServer.SetTimeZoneOffsetSeconds(hud.timeZoneOffsetSec)
	}
	pxcServer.SetSkipDashClockSync(hud.skipDashClockSync)
	pxcServer.SetDashAsksForTime(hud.dashAsksForTime)
	hud.startPxcEventFwd(pxcServer, pxcReady)
	// PXC error handling
	go func() {
		for pxcError := range pxcServer.Errors {
			hud.handleServerError(pxcError)
		}
	}()
	mediaControl := net.NewMediaControl(":10921")
	mediaControl.SupportFunction = hud.supportFunction
	mediaControl.JpegStills = hud.jpegStills
	// STREAM_START is the latest moment that still precedes a single painted pixel: the
	// dash has negotiated the capture and said it wants the stream. If a page command is
	// what unblocks its UI, this is where it belongs.
	mediaControl.OnVideoStart = func() {
		hud.muxSource.PrepareLiveConsumer()
		pxcServer.StartPageSwitchProbe()
		// The official app's second APPSTATUS_BACKGROUND goes out from setTrueMirror(),
		// once REQ_RV_DATA_START has been answered 113. This is that instant on our
		// side: the dash has asked for the stream and the live consumer is armed.
		pxcServer.SendAppStatus(net.AppStatusMirrorLive)
		// switchEc2Front goes out when the phone's stream starts and no frame has reached the
		// dash yet (sdk/b0.java:696-707), which is now.
		pxcServer.SwitchEcToFront()
	}
	// -- maybe we could change chunkStep to something smaller to reduce latency?
	mediaStream := net.NewMediaStream(":10920", hud.muxSource, 0x1000, 3*time.Millisecond)
	mediaStream.SetPlainFramingAllowed(hud.plainVideoFraming)
	pxcServer.HeardElsewhere = mediaStream.LastPoll
	mediaStream.OnVideoPulls = func(phase net.VideoPullPhase, pulls uint64) {
		payload := make([]byte, 9)
		payload[0] = byte(phase)
		binary.BigEndian.PutUint64(payload[1:], pulls)
		hud.handleServerEvent(HudEvent{
			Source: EventSourceTransport,
			Time:   time.Now(),
			Cmd:    TransportCmdVideoPulls,
			Data:   payload,
		})
	}
	// The capture-config exchange is the only place the dash states which frame
	// format it can parse, and it happens before it opens the data socket. The
	// outcome is forwarded to the phone: a field log must be able to say which
	// format was on the wire, or a framing experiment cannot be read at all.
	mediaControl.OnCaptureNegotiated = func(extended bool) {
		plain := mediaStream.NegotiatedExtendedProtocol(extended)
		toByte := func(v bool) byte {
			if v {
				return 1
			}
			return 0
		}
		hud.handleServerEvent(HudEvent{
			Source: EventSourceTransport,
			Time:   time.Now(),
			Cmd:    TransportCmdVideoFraming,
			Data:   []byte{toByte(extended), toByte(plain)},
		})
	}

	// Media error handling
	go func() {
		for medCtrl := range mediaControl.Errors {
			hud.handleServerError(medCtrl)
		}
	}()
	go func() {
		for medStr := range mediaStream.Errors {
			hud.handleServerError(medStr)
		}
	}()
	hud.startMediaEventFwd(mediaControl)

	servers := []StoppableServer{pxcServer, mediaStream, mediaControl}
	started, err = startReverseServersThenInit(servers, func() error {
		logging.Printf("Reverse HUD servers are listening; sending EC stream init command")
		hud.ecService = net.NewECService(
			hud.host.Ip, hud.host.Port, hud.host.Package, hud.phoneConfig.PhoneOs,
		)
		if initConn != nil {
			return hud.ecService.InitStreamCmdWithConn(initConn)
		}
		return hud.ecService.InitStreamCmd()
	})
	if err != nil {
		hud.ecService = nil
		return err
	}

	// Wait until the T-Box has completed the reverse PXC handshake.
	select {
	case <-pxcReady:
		break
	case <-ctx.Done():
		return ctx.Err()
	}

	hud.mu.Lock()
	hud.pxcControl = pxcServer
	hud.mediaStream = mediaStream
	hud.mediaControl = mediaControl
	hud.running = true
	hud.mu.Unlock()

	return nil
}

func startReverseServersThenInit(
	servers []StoppableServer,
	initStream func() error,
) (started []StoppableServer, err error) {
	for index, server := range servers {
		if err = server.Start(); err != nil {
			return started, fmt.Errorf("start reverse server %d (%T): %w", index, server, err)
		}
		started = append(started, server)
	}
	if err = initStream(); err != nil {
		return started, fmt.Errorf("initialize EasyConn stream: %w", err)
	}
	return started, nil
}

func (hud *CfmotoHUD) StopStream(ctx context.Context) error {
	hud.mu.Lock()
	if !hud.running {
		hud.mu.Unlock()
		hud.stopOnce.Do(func() {
			close(hud.stopped)
		})
		return nil
	}

	// Take copies of servers and clear state under lock
	pxc := hud.pxcControl
	mediaCtrl := hud.mediaControl
	mediaStream := hud.mediaStream

	hud.pxcControl = nil
	hud.mediaControl = nil
	hud.mediaStream = nil
	hud.running = false
	hud.mu.Unlock()

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	servers := []StoppableServer{mediaStream, mediaCtrl, pxc}

	logging.Printf("Stopping stream servers")
	for _, server := range servers {
		if server == nil {
			continue
		}
		wg.Add(1)
		go func(server StoppableServer) {
			defer wg.Done()
			_ = server.Stop(ctxWithTimeout)
		}(server)
	}

	done := make(chan any)
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}

	// close(hud.Events) -- treat them as broadcast channels, parent must close attached routines

	logging.Printf("signaling hud stopped")
	hud.stopOnce.Do(func() {
		close(hud.stopped)
	})

	return nil
}

// SendPhoneCommand sends one phone-originated PXC command on the phone channel and returns
// the dash's answer. See net.PXCControl.SendPhoneCommand.
func (hud *CfmotoHUD) SendPhoneCommand(cmd uint32, body []byte, timeout time.Duration) (*net.PXCResponse, error) {
	hud.mu.Lock()
	pxc := hud.pxcControl
	hud.mu.Unlock()
	if pxc == nil {
		return nil, errors.New("the session is not running")
	}
	return pxc.SendPhoneCommand(cmd, body, timeout)
}

// ReleaseDash hands the dash back its own UI before the session stops. See
// net.PXCControl.ReleaseDash.
func (hud *CfmotoHUD) ReleaseDash() error {
	hud.mu.Lock()
	pxc := hud.pxcControl
	hud.mu.Unlock()
	if pxc == nil {
		return errors.New("the session is not running")
	}
	return pxc.ReleaseDash()
}

func (hud *CfmotoHUD) IsRunning() bool {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	return hud.running
}

func (hud *CfmotoHUD) Done() <-chan any {
	return hud.stopped
}

func (hud *CfmotoHUD) SetHost(host *EcHost) error {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if hud.running {
		return errors.New("already running")
	}
	hud.host = host
	return nil
}
