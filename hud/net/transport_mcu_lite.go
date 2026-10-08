package net

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// MCU-Lite is the EasyConn variant for small dashes (CarbitRide's PXCForMCULite, sdk/e0.java).
// It shares the PXC port and framing with the car family, but its bodies are protobuf
// (ECTinyPlus, transport_mcu_lite_pb.go) and its picture is JPEG stills the dash pulls with
// one-byte credits on the media data port (common/base/c0.java), not H.264 polls. As CarbitRide
// runs it:
//
//   - the dash opens 0x1080000 (its request channel) and 0x1090000 (the phone's); both are acked
//     with cmd+1, and a new 0x1080000 first closes the whole previous MCU-Lite session
//     (sdk/h0.java:369-400);
//   - 0x1080010 ClientInfo -> 0x1080011 ClientInfoRly, whose random token keys the picture XOR
//     (um/b.java:105-137). An unreadable ClientInfo is refused with 0x80000030 (sdk/b1.java:136);
//   - 0x1080020 MirrorConfig -> 0x1080021 MirrorConfigRly: size floored to 16, fps and encoder
//     echoed (um/c.java:29-52). The phone then captures JPEG at that size;
//   - 0x1080030 CHECK_OTA and 0x1080040 START_OTA -> empty acks; the update is Carbit's cloud;
//   - the dash keeps its request channel alive with 0x70000000 (answered by the car code); the
//     phone sends no heartbeat on its own channel ("MCU Lite not send p2c heartbeat",
//     sdk/x0.java:194-195);
//   - the dash connects to the data port and writes single bytes, each the number of frames it
//     can take now; see serveData.
//
// Over USB the same exchange arrives through the adb tunnel (transport_usb.go), from 127.0.0.1.
const (
	MCULiteChannelC2P     uint32 = 0x1080000 // the dash's request channel
	MCULiteChannelP2C     uint32 = 0x1090000 // the phone's channel; nothing is sent on it here
	MCULiteCmdClientInfo  uint32 = 0x1080010
	MCULiteCmdStartMirror uint32 = 0x1080020
	MCULiteCmdCheckOTA    uint32 = 0x1080030
	MCULiteCmdStartOTA    uint32 = 0x1080040

	// pxcStatusRefused is om/a: the phone refusing a dash (sdk/b1.java:135-137).
	pxcStatusRefused uint32 = 0x80000030
)

// Data frames on the media data port (tm/a.java): a 21-byte header - type, u32 LE length
// counting the header, 16 zero bytes - then the payload.
const (
	mcuFrameJPEG    byte = 0 // the latest still, its first 4 bytes XORed with the token
	mcuFrameWeather byte = 1 // MirrorDataWeather, once
	mcuFrameGPS     byte = 2 // MirrorDataGPS, once
	mcuFrameHeader       = 21

	// mcuLiteDataTimeout: c0.java:30-40 gives up on a dash that sent no credit for 15 s, and
	// takes the MCU-Lite PXC channels down with it (t0.g0()).
	mcuLiteDataTimeout = 15 * time.Second
	mcuLiteGPSEvery    = time.Minute   // c0.java:174, supportFunction bit 2
	mcuLiteWeatherNext = 4 * time.Hour // c0.java:177, supportFunction bit 1
)

// MCULite serves one MCU-Lite dash. Create it with NewMCULite, set the callbacks, and hand it to
// PXCControl.SetMCULite and MediaStream.SetMCULite before they start.
type MCULite struct {
	// Callbacks, set before the session starts. They run on protocol goroutines: do not block.
	OnClientInfo   func(MCULiteClientInfo)
	OnMirrorConfig func(MCULiteMirrorConfig) // start capturing JPEG at this size
	// OnGPSDue is CarbitRide's X(): every 60 s while frames flow, for a dash with supportFunction
	// bit 2. Answer with SetGPS. OnWeatherDue is Y(), every 4 h with bit 1; a no-op in
	// CarbitRide 2.4 (ec/b.java:1125-1127), so a host may ignore it.
	OnGPSDue     func()
	OnWeatherDue func()

	pxc      *PXCControl
	watchdog time.Duration // overrides mcuLiteDataTimeout in tests

	mu      sync.Mutex
	ctrl    net.Conn // 0x1080000
	phone   net.Conn // 0x1090000
	data    net.Conn
	token   uint32
	support uint32
	mirror  *MCULiteMirrorConfig
	jpeg    []byte
	gps     []byte
	weather []byte
}

func NewMCULite() *MCULite { return &MCULite{} }

// SetMCULite makes this server answer the MCU-Lite family. Configure it before Start.
func (s *PXCControl) SetMCULite(m *MCULite) {
	s.mcuLite = m
	if m != nil {
		m.pxc = s
	}
}

// SetMCULite hands an MCU-Lite dash's data socket to m. Configure it before Start.
func (s *MediaStream) SetMCULite(m *MCULite) { s.mcuLite = m }

// PushJPEG offers the dash a new still. It is sent on every credit until the next one replaces
// it, as CarbitRide resends its cached frame (common/base/y.java:180-189). CarbitRide keeps each
// under 25000 bytes by lowering the quality (y.java:93-104); that is the host's encoder's job.
func (m *MCULite) PushJPEG(jpeg []byte) {
	if len(jpeg) < 4 {
		return // y.v() cannot XOR a still this short and drops it (y.java:157-171)
	}
	m.mu.Lock()
	m.jpeg = append([]byte(nil), jpeg...)
	m.mu.Unlock()
}

// SetGPS queues one GPS frame; it goes out ahead of the picture on the next credit.
func (m *MCULite) SetGPS(g MCULiteGPS) {
	m.mu.Lock()
	m.gps = encodeMCUGPS(g)
	m.mu.Unlock()
}

// SetWeather queues one weather frame, after any GPS frame and ahead of the picture.
func (m *MCULite) SetWeather(w MCULiteWeather) {
	m.mu.Lock()
	m.weather = encodeMCUWeather(w)
	m.mu.Unlock()
}

// answer is PXCForMCULite's handler table. It returns nil for a frame the default branch should
// answer as before.
func (m *MCULite) answer(s *PXCControl, event *PXCResponse, conn net.Conn) *PXCResponse {
	ack := &PXCResponse{Command: event.Command + 1}
	switch event.Command {
	case MCULiteChannelC2P:
		// "close all" before a new session (h0.java:369-373): the old channels and the data.
		m.replace(&m.ctrl, conn, true)
		m.watch(s, conn, "MCU_LITE_C2P")
		return ack
	case MCULiteChannelP2C:
		m.replace(&m.phone, conn, false) // z.B(): a new phone channel replaces the old one
		m.watch(s, conn, "MCU_LITE_P2C")
		return ack
	case MCULiteCmdClientInfo:
		info, err := decodeMCUClientInfo(event.Body)
		if err != nil {
			logging.Printf("MCU-Lite ClientInfo refused: %v", err)
			return &PXCResponse{Command: pxcStatusRefused}
		}
		// new Random().nextInt(Integer.MAX_VALUE) (um/b.java:128).
		token := uint32(rand.Int32N(1<<31 - 1))
		m.mu.Lock()
		m.token, m.support = token, info.SupportFunction
		m.mu.Unlock()
		if m.OnClientInfo != nil {
			m.OnClientInfo(*info)
		}
		ack.Body = encodeMCUClientInfoRly(info.ProtoVersion, token)
		return ack
	case MCULiteCmdStartMirror:
		cfg, err := decodeMCUMirrorConfig(event.Body)
		if err != nil {
			// um/c.java: an unreadable config still gets its reply, empty.
			logging.Printf("MCU-Lite MirrorConfig unreadable: %v", err)
			return ack
		}
		m.mu.Lock()
		m.mirror = cfg
		m.mu.Unlock()
		if m.OnMirrorConfig != nil {
			m.OnMirrorConfig(*cfg)
		}
		ack.Body = encodeMCUMirrorConfigRly(cfg)
		return ack
	case MCULiteCmdCheckOTA, MCULiteCmdStartOTA:
		return ack
	}
	// Not in PXCForMCULite's table: b1.a() answers 0x80000010 (sdk/b1.java:103-112).
	return &PXCResponse{Command: PxcStatusUnsupported}
}

// replace puts conn in slot, closing what it held; all also closes the other channels and the
// data socket. Whatever is closed here was replaced, so its closing is not a failure.
func (m *MCULite) replace(slot *net.Conn, conn net.Conn, all bool) {
	m.mu.Lock()
	old := []net.Conn{*slot}
	if all {
		old = append(old, m.phone, m.data)
		m.phone, m.data = nil, nil
	}
	*slot = conn
	m.mu.Unlock()
	for _, c := range old {
		if c != nil && c != conn {
			m.quietClose(c)
		}
	}
}

func (m *MCULite) quietClose(c net.Conn) {
	if value, ok := m.pxc.connections.Load(c); ok {
		value.(*pxcConnectionState).superseded.Store(true)
	}
	_ = c.Close()
}

// watch ends the MCU-Lite session when one of its PXC channels closes: losing the request
// channel closes the phone's and losing the phone's closes the request channel and the data
// (sdk/b1.java:286-297, sdk/x0.java:263-289). One watcher per connection, however often the dash
// repeats its opening on it.
func (m *MCULite) watch(s *PXCControl, conn net.Conn, name string) {
	s.nameChannel(conn, name)
	state := s.connectionState(conn)
	if !state.mcuWatched.CompareAndSwap(false, true) {
		return
	}
	done := state.done
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-done:
			m.teardown(conn, fmt.Errorf("MCU-Lite: the dash closed %s", name))
		case <-s.quit:
			m.teardown(conn, nil)
		}
	}()
}

// teardown closes every MCU-Lite channel and ends the session with reason, unless from is no
// longer one of them (it was replaced) or the session is stopping anyway.
func (m *MCULite) teardown(from net.Conn, reason error) {
	m.mu.Lock()
	if from != m.ctrl && from != m.phone && from != m.data {
		m.mu.Unlock()
		return
	}
	conns := []net.Conn{m.ctrl, m.phone, m.data}
	m.ctrl, m.phone, m.data = nil, nil, nil
	m.mu.Unlock()
	for _, c := range conns {
		if c != nil && c != from {
			m.quietClose(c)
		}
	}
	if reason != nil && !m.pxc.isStopping() {
		m.pxc.emitError(&PxcError{PxcTimeoutErr, reason, true})
	}
}

// claimData takes a data-port connection for serveData when an MCU-Lite session is up and the
// connection comes from its dash: t0.c.run() picks the MCU-Lite data thread when PXCForMCULite's
// phone channel is alive and the socket is from the request channel's host
// (common/base/t0.java:258-264). The caller serves it, so its shutdown waits for it.
func (m *MCULite) claimData(conn net.Conn) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	if m.phone == nil || (m.ctrl != nil && peerHost(m.ctrl) != peerHost(conn)) {
		m.mu.Unlock()
		return false
	}
	old := m.data
	m.data = conn
	m.mu.Unlock()
	if old != nil {
		_ = old.Close() // one data thread at a time (t0.f0())
	}
	return true
}

// serveData is c0.run(). The dash writes single bytes, each the number of frames it can take now
// (it replaces the count, it does not add to it). For each credit the phone sends the next frame
// - a pending GPS frame, else a pending weather frame, else the latest still - and then waits
// 1000/fps ms. Credits with nothing to send are kept. After a sent frame the GPS (60 s) and
// weather (4 h) timers are checked. No credit for 15 s ends the MCU-Lite session, and so does a
// frame the dash takes nothing of for as long.
func (m *MCULite) serveData(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		m.mu.Lock()
		if m.data == conn {
			m.data = nil
		}
		m.mu.Unlock()
	}()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	m.mu.Lock()
	cfg := m.mirror
	m.mu.Unlock()
	if cfg == nil {
		logging.Printf("MCU-Lite data socket before any MirrorConfig, closing it (c0.java:136-140)")
		return
	}
	// ponytail: CarbitRide divides by wantFps and dies on 0; 1 fps is the floor here.
	pace := time.Second / time.Duration(max(cfg.Fps, 1))
	timeout := mcuLiteDataTimeout
	if m.watchdog > 0 {
		timeout = m.watchdog
	}

	// c0.a: a watchdog of its own, restarted by every credit, so a frame the dash stopped reading
	// cannot keep it from firing. It closes the data socket, which ends that write, and the
	// session with it (c0.java:21-40).
	end := func(reason error) {
		_ = conn.Close()
		m.teardown(conn, reason)
	}
	watchdog := time.AfterFunc(timeout, func() {
		end(fmt.Errorf("MCU-Lite: no frame credit from the dash for %s", timeout))
	})
	defer watchdog.Stop()

	var credits atomic.Int32
	wake := make(chan struct{}, 1)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		b := make([]byte, 1)
		for {
			if _, err := conn.Read(b); err != nil {
				return
			}
			credits.Store(int32(b[0]))
			watchdog.Reset(timeout)
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	defer func() { _ = conn.Close(); <-closed }() // before watchdog.Stop: the reader resets it

	var lastGPS, lastWeather time.Time
	for {
		if credits.Load() <= 0 {
			select {
			case <-wake:
			case <-closed:
				return
			}
			continue
		}
		frame := m.nextFrame()
		if frame != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(timeout))
			if _, err := conn.Write(frame); err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					end(fmt.Errorf("MCU-Lite: the dash took no frame for %s", timeout))
				}
				return
			}
			credits.Add(-1)
		}
		select {
		case <-time.After(pace):
		case <-closed:
			return
		}
		if frame == nil {
			continue
		}
		m.mu.Lock()
		support := m.support
		m.mu.Unlock()
		if now := time.Now(); support&4 != 0 && now.Sub(lastGPS) >= mcuLiteGPSEvery {
			lastGPS = now
			if m.OnGPSDue != nil {
				m.OnGPSDue()
			}
		} else if support&2 != 0 && now.Sub(lastWeather) >= mcuLiteWeatherNext {
			lastWeather = now
			if m.OnWeatherDue != nil {
				m.OnWeatherDue()
			}
		}
	}
}

// nextFrame is c0.M(): GPS first, then weather, each sent once, then the latest still.
func (m *MCULite) nextFrame() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.gps != nil:
		gps := m.gps
		m.gps = nil
		return mcuDataFrame(mcuFrameGPS, gps)
	case m.weather != nil:
		weather := m.weather
		m.weather = nil
		return mcuDataFrame(mcuFrameWeather, weather)
	case m.jpeg != nil:
		still := append([]byte(nil), m.jpeg...)
		key := binary.LittleEndian.AppendUint32(nil, m.token)
		for i := range key {
			still[i] ^= key[i]
		}
		return mcuDataFrame(mcuFrameJPEG, still)
	}
	return nil
}

func mcuDataFrame(kind byte, payload []byte) []byte {
	frame := make([]byte, mcuFrameHeader, mcuFrameHeader+len(payload))
	frame[0] = kind
	binary.LittleEndian.PutUint32(frame[1:], uint32(mcuFrameHeader+len(payload)))
	return append(frame, payload...)
}
