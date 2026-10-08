package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	stdnet "net"
	"os"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// To make BuildAnnexBAUFromAVCC work well:
// Configure MediaCodec so that keyframes include SPS/PPS in the sample payload.
// That’s usually done with something like KEY_PREPEND_HEADER_TO_SYNC_FRAMES.
// Keep GOP = 1 or 2, no B-frames (you were already planning this).
// Then you don’t need to juggle SPS/PPS separately, they’ll appear as the first NALs in the keyframe sample, and
// our AVCC -> Annex-B converter just wraps them in start codes and keeps them.

// BuildAnnexBAUFromAVCC converts a single AVCC-formatted sample (one frame's worth of NAL units) into a single Annex-B AU.
// It assumes 4-byte big-endian NAL lengths.
func BuildAnnexBAUFromAVCC(avcc []byte) ([]byte, error) {
	if len(avcc) < 4 {
		return nil, fmt.Errorf("avcc sample too short")
	}

	out := make([]byte, 0, len(avcc)+32)

	// Prepend an AUD NAL. We use a common AUD RBSP payload 0xF0, which most decoders ignore.
	// Annex-B start code (4 bytes) + NAL header (0x09) + rbsp_byte
	out = append(out, 0x00, 0x00, 0x00, 0x01, 0x09, 0xF0)

	i := 0
	nalCount := 0
	for {
		if i+4 > len(avcc) {
			break
		}
		nalLen := int(binary.BigEndian.Uint32(avcc[i : i+4]))
		i += 4
		// Compared without i+nalLen: on the 32-bit ABI a length near 2^31 is negative or
		// overflows the sum, and either way the slice below would panic.
		if nalLen <= 0 || nalLen > len(avcc)-i { // Truncated / malformed sample
			return nil, fmt.Errorf("invalid AVCC NAL length")
		}

		out = append(out, 0x00, 0x00, 0x00, 0x01) // Start code
		out = append(out, avcc[i:i+nalLen]...)    // NAL bytes
		i += nalLen
		nalCount++
	}

	if i != len(avcc) || nalCount == 0 {
		return nil, fmt.Errorf("no NAL units found in AVCC sample")
	}
	return out, nil
}

func BuildAnnexBAU(sample []byte) ([]byte, error) {
	if len(sample) < 4 {
		return nil, fmt.Errorf("sample too short")
	}

	// 1) Detect Annex-B: start code at beginning
	if bytes.HasPrefix(sample, []byte{0x00, 0x00, 0x00, 0x01}) ||
		bytes.HasPrefix(sample, []byte{0x00, 0x00, 0x01}) {
		out := make([]byte, 0, len(sample)+16)
		// prepend AUD
		out = append(out, 0x00, 0x00, 0x00, 0x01, 0x09, 0xF0)
		out = append(out, sample...)
		return out, nil
	}

	// 2) Otherwise, assume AVCC (4-byte lengths)
	return BuildAnnexBAUFromAVCC(sample)
}

type CanBeFatalErr interface {
	error
	IsFatal() bool
}

type MobileConfig struct {
	StaticSignal        []byte
	TargetFPS           int
	StartupTimeoutSec   int
	TeardownTimeoutSec  int
	DiscoveryTimeoutSec int
	DiscoveryTries      int
	// SupportFunction is sent in the MediaCtrlScreenConf reply.  Different
	// dashboards use it as a capability bitmask, so the Android host must be
	// able to select it from its resolved T-Box profile.
	SupportFunction int
	// ProactivePxcHeartbeatEnabled keeps the reverse PXC sockets alive on
	// firmware profiles known to tear down silent channels: the phone channel
	// (0x20000) always, with each beat's answer awaited so a dead dash ends the
	// session after its own timeout, and the request channel only until the dash
	// shows a keepalive of its own there.
	ProactivePxcHeartbeatEnabled bool
	// PlainVideoFramingAllowed lets a dash reporting supportExtendProtocol=0 pull
	// video frames without the 4-byte index. Set it only for dashboards the host
	// could not identify, so recognised profiles keep today's wire format.
	PlainVideoFramingAllowed bool
	// PageSwitchProbeEnabled sends the phone-to-car page sequence once the dash says
	// STREAM_START, for one dashboard family that drains the video stream and paints
	// none of it. Off for everything else: it puts three unsolicited commands on the
	// wire that no reference implementation sends. See net.PXCControl.SetPageSwitchProbe.
	PageSwitchProbeEnabled bool
	// JpegStillsEnabled negotiates encoder=1 (JPEG) with the dash instead of echoing the
	// H.264 it asked for, and switches the live queue to forwarding whole payloads. The
	// host must be pushing stills through PushStill when this is on; nothing here
	// transcodes. Off for every dashboard but the one the experiment is aimed at.
	JpegStillsEnabled bool
	// AppStatusNotifyEnabled makes the phone push ECP_P2C_APPSTATUS_BACKGROUND the way
	// the official EasyConn app does: once when PXC comes up, and again - as "the
	// mirror is live" - when the dash starts pulling frames (0x20020 instead while
	// SetAppForeground says the app is in front). It is the only statement the official
	// app ever volunteers that a mirror exists, and no reference implementation sends it.
	//
	// A flavor-51 dash (CarbitRide's family, the Zontes 350D among them) gets it by default
	// whatever this says, with the rest of the official lifecycle; SetFlavor51Lifecycle(false)
	// is the kill switch that restores the earlier wire. For every other dash it stays off
	// unless this is set. See net.PXCControl.SetAppStatusNotify.
	AppStatusNotifyEnabled bool
	// PhoneScreenWidth, PhoneScreenHeight and PhoneScreenRotation are the phone's own
	// display metrics, carried inside that notification exactly as the official app
	// reads them off Display.getRealSize() and getRotation(). Only the host can know
	// them; zero is sent as zero rather than guessed.
	PhoneScreenWidth    int
	PhoneScreenHeight   int
	PhoneScreenRotation int
	// TimeZoneID is the host's IANA zone id ("Europe/Rome"), sent in the
	// QUERY_TIME reply. Android must supply it: Go's local location carries no
	// usable name on a device. Empty falls back to a fixed-offset id.
	TimeZoneID string
	// TimeZoneOffsetSeconds is the host's UTC offset for right now, DST already
	// applied. Required with TimeZoneID and applied only alongside it: the id
	// alone only ever labelled the reply, while the times inside it stayed on
	// Go's local location - UTC on Android - and dashes were being set hours
	// wrong. See net.PXCControl.SetTimeZoneOffsetSeconds.
	TimeZoneOffsetSeconds int
	// SkipDashClockSync answers 0x10450 with an empty body and never pushes
	// unsolicited clock JSON. For SSDQ01-0120 units that ask for time but keep
	// currentHUTime as uptime (01.01.1970 on the TFT). Default false leaves
	// panel-A Voge units on the JSON path that already sets their clock.
	SkipDashClockSync bool
	// DashAsksForTime says this dashboard sent 0x10450 on an earlier connection,
	// so the unsolicited clock push is not started at all. Only the host can know
	// this - a session cannot remember the one before it - and only the host keeps
	// it, per dashboard fingerprint rather than per motorcycle, because it is a
	// property of the firmware. Default false: an unknown dash is still offered
	// the push after the grace period.
	DashAsksForTime bool
}

func NewMobileConfig(static []byte, fps int, startupTimeoutSec, teardownTimeoutSec, discTimeout, discTries int) *MobileConfig {
	return &MobileConfig{
		StaticSignal:        static,
		TargetFPS:           fps,
		StartupTimeoutSec:   startupTimeoutSec,
		TeardownTimeoutSec:  teardownTimeoutSec,
		DiscoveryTimeoutSec: discTimeout,
		DiscoveryTries:      discTries,
	}
}

type MobileCallback interface {
	OnError(msg string, fatal bool)
	OnEvent(time int64, source int, command int, payload []byte)
	OnStopped()
}

type StreamHost struct {
	core.EcHost
}

func NewStreamHost(ip, port, pkg string) *StreamHost {
	return &StreamHost{
		core.EcHost{
			Ip:      ip,
			Port:    port,
			Package: pkg,
		},
	}
}

type MobileSession struct {
	cfg MobileConfig
	cb  MobileCallback

	hud      *core.CfmotoHUD
	mux      *stream.MuxSource
	streamer *stream.AUStreamer

	stopped chan struct{}

	// features is the DashFeatures made for this session (NewDashFeatures); relayEvent feeds it,
	// and a stop or a newer DashFeatures discards what it has not handled yet.
	features atomic.Pointer[DashFeatures]
}

func NewMobileSession(cfg *MobileConfig, cb MobileCallback) (*MobileSession, error) {
	ms := &MobileSession{
		cfg:     *cfg,
		cb:      cb,
		stopped: make(chan struct{}),
	}

	// Setup Sources
	var static stream.FrameSource
	if len(cfg.StaticSignal) > 0 {
		var err error
		static, err = stream.NewRawFrameSource(cfg.StaticSignal, cfg.TargetFPS)
		if err != nil {
			return nil, err
		}
	}
	// 6 queued AUs ≈ 200ms at 30fps: absorbs producer/poll clock jitter without
	// dropping frames of a predictive (GOP/intra-refresh) stream, while keeping
	// worst-case buffered latency low. Overflow drops up to the next IDR (PushFrame).
	live := stream.NewLiveStreamSource(cfg.TargetFPS, 3*time.Second, 6)
	// Before anything is pushed: the constructor arms an IDR wait that no JPEG can ever
	// satisfy, so leaving it armed would drop every still in silence.
	live.SetOpaquePayloads(cfg.JpegStillsEnabled)
	// An overflowing queue now drops to the next IDR instead of smearing the picture; this
	// tells the host to ask its encoder for one rather than wait for the GOP to come round.
	live.OnKeyframeNeeded = func(dropped int) {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(dropped))
		ms.relayEvent(core.HudEvent{
			Source: core.EventSourceTransport,
			Time:   time.Now(),
			Cmd:    core.TransportCmdKeyframeNeeded,
			Data:   payload,
		})
	}
	ms.mux = &stream.MuxSource{NoSignal: static, Live: live}

	// Build streamer
	ms.streamer = stream.NewAUStreamer(live)

	// Build hud session
	ms.hud = core.NewCfmotoHUD(cfg.TargetFPS, ms.mux, cfg.SupportFunction)
	ms.hud.SetProactivePxcHeartbeat(cfg.ProactivePxcHeartbeatEnabled)
	ms.hud.SetPlainVideoFramingAllowed(cfg.PlainVideoFramingAllowed)
	ms.hud.SetPageSwitchProbe(cfg.PageSwitchProbeEnabled)
	ms.hud.SetJpegStills(cfg.JpegStillsEnabled)
	ms.hud.SetAppStatusNotify(cfg.AppStatusNotifyEnabled)
	ms.hud.SetPhoneScreen(cfg.PhoneScreenWidth, cfg.PhoneScreenHeight, cfg.PhoneScreenRotation)
	ms.hud.SetTimeZoneID(cfg.TimeZoneID)
	// Only together with the id: an offset on its own would silently pin the
	// clock replies to UTC for a host that never configured a zone, which is the
	// very bug this exists to fix.
	if cfg.TimeZoneID != "" {
		ms.hud.SetTimeZoneOffsetSeconds(cfg.TimeZoneOffsetSeconds)
	}
	ms.hud.SetSkipDashClockSync(cfg.SkipDashClockSync)
	ms.hud.SetDashAsksForTime(cfg.DashAsksForTime)
	go func() {
		for {
			select {
			case cErr, ok := <-ms.hud.Errors:
				if !ok {
					// if we close Errors, this lets us exit cleanly
					return
				}
				if cErr != nil {
					ms.relayError(cErr)
				}
			case <-ms.hud.Done():
				return // HUD session is over - stop relaying
			}
		}
	}()
	go func() {
		for {
			select {
			case evt, ok := <-ms.hud.Events:
				if !ok {
					return
				}
				ms.relayEvent(evt)
			case <-ms.hud.Done():
				return
			}
		}
	}()

	go ms.watchHud()

	return ms, nil
}

func (ms *MobileSession) relayError(err error) {
	msg := err.Error()
	fatal := true

	var fErr CanBeFatalErr
	if errors.As(err, &fErr) {
		fatal = fErr.IsFatal()
	}

	if ms.cb != nil {
		go ms.cb.OnError(msg, fatal)
	}
}

func (ms *MobileSession) relayEvent(evt core.HudEvent) {
	timestamp := time.Now().UnixMilli()
	src := int(evt.Source)
	command := evt.Cmd

	// A type switch, not an assertion: PXC bodies arrive as json.RawMessage,
	// which is a named type over []byte, and an assertion demands an exact
	// dynamic-type match. Asserting to []byte therefore failed silently for
	// every PXC event ever relayed - the phone saw a null payload, so T-Box
	// capabilities never decoded and every dash fell back to the generic
	// profile, while the session itself worked because the JSON is parsed here
	// in Go where the body is still intact. Media-control payloads are a plain
	// []byte and were unaffected, which is why only half the traffic looked
	// broken.
	var payload []byte
	switch data := evt.Data.(type) {
	case []byte:
		payload = append([]byte(nil), data...)
	case json.RawMessage:
		payload = append([]byte(nil), data...)
	}

	// Queued here, before OnEvent, so the features see the dash's commands in the order it sent
	// them; they run on their own worker, so a listener that blocks never holds up this relay.
	if f := ms.features.Load(); f != nil && evt.Source == core.EventSourcePXC {
		f.enqueue(timestamp, command, payload)
	}
	if ms.cb != nil {
		go ms.cb.OnEvent(timestamp, src, command, payload)
	}
}

func (ms *MobileSession) watchHud() {
	<-ms.hud.Done()
	if f := ms.features.Load(); f != nil {
		f.discard()
	}
	// notify callback
	if ms.cb != nil {
		go ms.cb.OnStopped()
	}
	close(ms.stopped)
}

// DiscoverHost uses zeroconf to search for the mDNS service, use only if SELinux or the mobile OS allows for it
func (ms *MobileSession) DiscoverHost() error {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), time.Duration(ms.cfg.StartupTimeoutSec)*time.Second)
	defer cancel()

	tries := 0
	for tries < ms.cfg.DiscoveryTries {
		tries++
		if err := ms.hud.SearchForHost(ctxWithTimeout, time.Duration(ms.cfg.DiscoveryTimeoutSec)*time.Second); err != nil {
			logging.Printf("error discovering host: %v\n", err)
			return err
		} else {
			return nil
		}
	}

	return errors.New("discovery timed out")
}

func (ms *MobileSession) SetECHost(host *StreamHost) error {
	if err := ms.hud.SetHost(&host.EcHost); err != nil {
		return err
	}
	return nil
}

func (ms *MobileSession) StartSession() error {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), time.Duration(ms.cfg.StartupTimeoutSec)*time.Second)
	defer cancel()
	if err := ms.hud.StartStream(ctxWithTimeout); err != nil {
		return err
	}
	return nil
}

// StartSessionWithSocketFd uses an already-connected TCP socket supplied by Android.
// The descriptor ownership is transferred to this method and is always closed here.
func (ms *MobileSession) StartSessionWithSocketFd(fd int64) error {
	if fd < 0 {
		return errors.New("invalid EC init socket descriptor")
	}
	file := os.NewFile(uintptr(fd), "ec-init")
	if file == nil {
		return errors.New("unable to adopt EC init socket descriptor")
	}
	conn, err := stdnet.FileConn(file)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("unable to adopt EC init socket: %w", err)
	}
	if closeErr != nil {
		_ = conn.Close()
		return fmt.Errorf("unable to release EC init descriptor: %w", closeErr)
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), time.Duration(ms.cfg.StartupTimeoutSec)*time.Second)
	defer cancel()
	if err := ms.hud.StartStreamWithInitConn(ctxWithTimeout, conn); err != nil {
		return err
	}
	return nil
}

func (ms *MobileSession) StopSession() error {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), time.Duration(ms.cfg.TeardownTimeoutSec)*time.Second)
	defer cancel()
	logging.Printf("Stopping session\n")
	if err := ms.hud.StopStream(ctxWithTimeout); err != nil {
		return err
	}
	return nil
}

func (ms *MobileSession) PushFrame(avccChunk []byte) {
	if !ms.hud.IsRunning() {
		return
	}
	if len(avccChunk) == 0 {
		return
	}

	au, err := BuildAnnexBAU(avccChunk)
	if err != nil {
		if ms.cb != nil {
			go ms.cb.OnError("invalid AVCC: "+err.Error(), false)
		}
		logging.Printf("MobileSession: invalid AVCC: %v\n", err)
		return
	}

	ms.mux.Live.PushFrame(au)
}

// PushStill hands the dash one whole JPEG. It exists beside PushFrame rather than inside it
// because the two payloads share nothing: a still needs no AVCC-to-Annex-B conversion, has no
// NAL to inspect, and must not be measured against the IDR rules that make sense of a
// predictive stream. Only useful with MobileConfig.JpegStillsEnabled, which is what makes the
// queue accept these and the capture negotiation announce them.
func (ms *MobileSession) PushStill(jpeg []byte) {
	if !ms.hud.IsRunning() {
		return
	}
	if len(jpeg) == 0 {
		return
	}
	if !ms.cfg.JpegStillsEnabled {
		// Pushing a still into a session that negotiated H.264 would put a JPEG on the wire
		// inside a frame the dash is parsing as an access unit. Refusing loudly beats that.
		if ms.cb != nil {
			go ms.cb.OnError("PushStill called on a session that did not negotiate JPEG", false)
		}
		logging.Printf("MobileSession: PushStill ignored, JpegStillsEnabled is off\n")
		return
	}

	ms.mux.Live.PushFrame(jpeg)
}

// PhoneCommandReply is the dash's answer to a phone command: its cmd+1 with whatever body it
// carried, or 0x80000010 (unsupported) / 0x80000020 (error) when the dash refused it.
// Command is an int64 so 0x8xxxxxxx reads the same on every ABI.
type PhoneCommandReply struct {
	Command int64
	Body    []byte
}

// SendPhoneCommand sends one phone-originated PXC command (a P2C command such as 0x20110) to
// the dash, on the connection it opened for them (0x20000), and blocks until the dash answers.
// Commands go out one at a time, each after the previous one has been answered, the way the
// official app sends them; the dash's answers also reach OnEvent as usual.
//
// The wait for the answer is the dash's own timeout (socketTimeoutPeriodWifi, else 9 s).
// timeoutMs > 0 additionally bounds the whole call, time spent queued included; a command
// still queued when it expires is never sent. Returns null without error for the few commands
// the dash owes no answer to. Errors when the dash has not opened the channel, the write
// fails, nothing comes back in time, or the session stops. Call it off the main thread.
func (ms *MobileSession) SendPhoneCommand(command int64, body []byte, timeoutMs int) (*PhoneCommandReply, error) {
	reply, err := ms.hud.SendPhoneCommand(
		uint32(command), append([]byte(nil), body...), time.Duration(timeoutMs)*time.Millisecond,
	)
	if err != nil || reply == nil {
		return nil, err
	}
	return &PhoneCommandReply{Command: int64(reply.Command), Body: append([]byte(nil), reply.Body...)}, nil
}

func (ms *MobileSession) IsRunning() bool {
	select {
	case <-ms.stopped:
		return false
	default:
		return true
	}
}
