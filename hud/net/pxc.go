package net

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// This protocol is not so weird, it has a 16 byte header plus payload
// standard little endian order and some simple checks, the client (bike)
// establishes connection 2 times, so it's normal for one connection to drop
// after setting up the speed configuration.

const (
	pxcHeaderSize             = 16
	defaultPxcHeartbeatPeriod = 2 * time.Second
)

// Command requests
const (
	PxcHandshake      uint32 = 65536
	PxcHudConf        uint32 = 65552
	PxcSpeedConf      uint32 = 67216
	PxcHeartbeat      uint32 = 1879048192
	PxcClientSet      uint32 = 66528
	PxcChannelCarData uint32 = 0x20000

	// Newer CFDL26 firmware sends additional control notifications before it
	// opens the media ports. PXC request commands are even and use cmd+1 as ACK.
	PxcOtaFtpInfo       uint32 = 0x103a0
	PxcMediaFeatureConf uint32 = 0x10020
	PxcCheckSnResult    uint32 = 0x201c0
	// The dash asks the phone what time it is. The default branch below would
	// already answer this with an empty cmd+1, which is what shipped until now;
	// naming it lets the reply carry the timestamp the request is asking for.
	PxcHuTimeSync uint32 = 0x10600
	// The other clock question, answered with JSON instead of a binary stamp.
	// Firmware picks one or the other, never both: see querytime.go.
	PxcQueryTime uint32 = 0x10450
)

// Command responses
const (
	PxcHandshakeOk   uint32 = 65537
	PxcPhoneConf     uint32 = 65553
	PxcSpeedOk       uint32 = 67217
	PxcHeartbeatOk   uint32 = 1879048193
	PxcClientOk      uint32 = 66529
	PxcCheckSnAck    uint32 = PxcClientSet + 1
	PxcCheckSnDone   uint32 = PxcCheckSnResult + 1
	PxcHuTimeSyncAck uint32 = PxcHuTimeSync + 1
	PxcQueryTimeAck  uint32 = PxcQueryTime + 1
)

type checkSnRequest struct {
	ClientSet string `json:"client_set"`
	Serial    string `json:"sn"`
}

type checkSnResult struct {
	IsOK      bool   `json:"isOk"`
	ErrorCode int    `json:"errCode"`
	ErrorMsg  string `json:"errMsg"`
	ID        string `json:"id"`
	ClientSet string `json:"client_set"`
}

type PXCResponse struct {
	Command uint32
	Size    uint32
	Magic   uint32
	Token   uint32
	Body    json.RawMessage
}

type HUDConfig struct {
	HUID                 string `json:"HUID"`
	HUName               string `json:"HUName"`
	BluetoothPolicy      int    `json:"bluetoothPolicy"`
	BtAddress            string `json:"btAddress"`
	BtName               string `json:"btName"`
	BtPin                string `json:"btPin"`
	CarBrand             string `json:"carBrand"`
	CarConfig            string `json:"carConfig"`
	CarMicSupportFeature int    `json:"carMicSupportFeature"`
	CarModel             string `json:"carModel"`
	Channel              string `json:"channel"`
	CurrentHUTime        uint64 `json:"currentHUTime"`
	DisablePageInRVMap   int    `json:"disablePageInRVMap"`
	DisableShowCallInfo  bool   `json:"disableShowCallInfo"`
	DisableShowInRVInfo  any    `json:"disableShowInRVInfo"`
	Dpi                  int    `json:"dpi"`
	EnableDPI            bool   `json:"enableDPI"`
	EnableSockServerAuth bool   `json:"enableSockServerAuth"`
	// Firmware in the field uses a numeric flavor, while the MOTO-HUB simulator
	// identifies its development profile with a string ("simulator"). The
	// transport forwards the original HUD_CONFIG to Android and does not need a
	// normalized value here, so retain the JSON form and accept both variants.
	Flavor                    json.RawMessage `json:"flavor"`
	MirrorMode                int             `json:"mirrorMode"`
	PackageName               string          `json:"package_name"`
	ProductType               int             `json:"productType"`
	PxcVersion                string          `json:"pxcVersion"`
	ScreenType                int             `json:"screenType"`
	SdkVersion                string          `json:"sdkVersion"`
	SocketTimeoutPeriodWifi   int             `json:"socketTimeoutPeriodWifi"`
	SteeringMode              int             `json:"steeringMode"`
	SupportBTCall             bool            `json:"supportBTCall"`
	SupportBTSetting          bool            `json:"supportBTSetting"`
	SupportBackDesktop        bool            `json:"supportBackDesktop"`
	SupportBackDesktopNew     bool            `json:"supportBackDesktopNew"`
	SupportConnect            int             `json:"supportConnect"`
	SupportDownloadScreenEvt  bool            `json:"supportDownloadScreenEvt"`
	SupportFunction           int             `json:"supportFunction"`
	SupportHID                bool            `json:"supportHID"`
	SupportLandscapeAdaptive  bool            `json:"supportLandscapeAdaptive"`
	SupportMic                bool            `json:"supportMic"`
	SupportMirrorOverlayTouch bool            `json:"supportMirrorOverlayTouch"`
	SupportMirrorReconnect    bool            `json:"supportMirrorReconnect"`
	SupportOTASpeenUp         bool            `json:"supportOTASpeenUp"`
	SupportOTAUpdate          bool            `json:"supportOTAUpdate"`
	SupportPhoneSignal        bool            `json:"supportPhoneSignal"`
	SupportRVForAdb           bool            `json:"supportRVForAdb"`
	SupportScreenMirroring    bool            `json:"supportScreenMirroring"`
	SupportScreenTouch        bool            `json:"supportScreenTouch"`
	SupportSyncCorrectTime    bool            `json:"supportSyncCorrectTime"`
	SupportThirdPartyApp      bool            `json:"supportThirdPartyApp"`
	TransportType             int             `json:"transportType"`
	UseBTCallRecords          bool            `json:"useBTCallRecords"`
	UUID                      string          `json:"uuid"`
	VersionCode               string          `json:"version_code"`
	VersionName               string          `json:"version_name"`
	WakeUpWord                string          `json:"wakeupWord"`
}

type PhoneConfig struct {
	PxcVersion        string `json:"pxcVersion"`
	PhoneUUID         string `json:"phoneUUID"`
	PhoneBrand        string `json:"phoneBrand"`
	PhoneModel        string `json:"phoneModel"`
	PhoneOsVersion    string `json:"phoneOsVersion"`
	PhoneOs           string `json:"phoneOs"`
	Package           string `json:"package"`
	VersionCode       int    `json:"versionCode"`
	Token             int    `json:"token"`
	Pubkey            string `json:"pubkey"`
	EncryptedHUID     string `json:"encryptedHUID"`
	BluetoothName     string `json:"bluetoothName"`
	SupportH264IFrame bool   `json:"supportH264IFrame"`
	// The official app always states a supportFunction in this reply
	// (ECP_C2P_CLIENT_INFO.g0(), which writes it unless the caller has already supplied
	// one), and so does open-cflink's captured handshake. This daemon did not, and the
	// value it does compute went only into the RLY for MEDIA_CONTROL 0x60 - which
	// firmware that runs supportExtendProtocol=0 never asks for, so on those units the
	// number was never stated at all.
	SupportFunction int `json:"supportFunction"`
	// There is deliberately no supportSyncCorrectTime here. It was added on
	// 2026-08-10 on the theory that a Carbit dash only asks a phone that claims
	// the capability, and removed the same day: the reference implementation
	// shipped exactly that claim and then withdrew it, reporting that firmware
	// which saw it applied the 0x10601 reply aggressively and drove Zontes and
	// Voge clusters to 00:00 even when their clock was already correct.
	//
	// We answer 0x10600 with a full body regardless (see hutimesync.go), which is
	// the half with evidence behind it - an empty ack is what pushed Morini and
	// Voge clusters to 1970. Answering when asked costs nothing; advertising for
	// the work is what caused harm. Do not re-add this without a rider log
	// showing a dash that stays silent until it is claimed.
	AppVersionFingerPrint string `json:"appVersionFingerPrint"`
}

type PXCControl struct {
	port     string
	quit     chan any
	wg       sync.WaitGroup
	listener net.Listener
	tracker  *ConnTracker

	stopOnce sync.Once

	Events      chan PXCResponse
	Errors      chan error
	KeyPair     *KeyPair
	HudConfig   *HUDConfig
	PhoneConfig *PhoneConfig

	connections        sync.Map // map[net.Conn]*pxcConnectionState
	heartbeatInterval  time.Duration
	proactiveHeartbeat bool
	timeZoneID         string
	timeZoneOffsetSec  int
	timeZoneOffsetSet  bool
	skipDashClockSync  bool
	dashAsksForTime    bool

	queryTimeMu            sync.Mutex
	queryTimeAnswered      bool
	proactiveQueryTimeOnce sync.Once
	// queryTimeGrace, when non-zero, overrides defaultQueryTimeGrace. Tests
	// shrink it; production leaves it at zero.
	queryTimeGrace time.Duration

	// The phone-to-car page experiment; see pageswitch.go.
	pageProbe        bool
	pageProbeOnce    sync.Once
	pageProbeSpacing time.Duration
	// OnPageSwitchProbe reports each step of that experiment, including the two ways it
	// can decline to run, so a rider's log says which command was on the wire when the
	// panel did or did not move.
	OnPageSwitchProbe func(step PageSwitchProbeStep, command uint32, err error)

	// The phone-to-car mirroring notification; see appstatus.go.
	appStatus appStatusState
	// phoneChannelWait, when non-zero, overrides defaultPhoneChannelWait. Tests shrink it;
	// production leaves it at zero.
	phoneChannelWait time.Duration
	// OnAppStatus reports each ECP_P2C_APPSTATUS_BACKGROUND this session put on the
	// wire, so a rider's log carries the mode and whether the write landed. The dash's
	// own 0x20031 ack arrives separately, through the unhandled-response branch.
	OnAppStatus func(mode int, err error)

	// The phone channel and its one sender; see p2c.go and phoneChannel.
	phoneMu    sync.Mutex
	phone      net.Conn
	p2cOnce    sync.Once
	p2cQueue   chan *p2cRequest
	p2cReplies chan p2cFrame
	// p2cTimeout is the dash's own reply timeout, from CLIENT_INFO; zero until it says.
	p2cTimeout atomic.Int64
}

type pxcConnectionState struct {
	writeMu       sync.Mutex
	heartbeatOnce sync.Once
	done          chan struct{}
	doneOnce      sync.Once

	// What the dash called this connection when it opened it, and how it has behaved since.
	// None of it changes a decision here; all of it is what a field log needs to tell an
	// abandoned channel timing out from a working one dying - see channelEpitaph.
	channel     atomic.Value // string: "CAR_CTRL" | "CAR_DATA"
	lastRxNanos atomic.Int64
	beatsSince  atomic.Int64 // keepalives written since the dash last said anything
	// superseded marks a phone channel the dash replaced with a newer one, so its closing is
	// not reported as a failure.
	superseded atomic.Bool
}

func NewPXCControl(port string, kp *KeyPair, config *PhoneConfig) *PXCControl {
	return &PXCControl{
		port:        port,
		quit:        make(chan any),
		tracker:     NewConnTracker(),
		Events:      make(chan PXCResponse, 16),
		Errors:      make(chan error, 16),
		KeyPair:     kp,
		PhoneConfig: config,
		p2cQueue:    make(chan *p2cRequest, p2cQueueSize),
		p2cReplies:  make(chan p2cFrame, 8),
	}
}

// SetProactiveHeartbeat controls the firmware-specific dual-channel keepalive.
// It is intentionally opt-in so generic T-Boxes retain their existing behavior.
func (s *PXCControl) SetProactiveHeartbeat(enabled bool) {
	s.proactiveHeartbeat = enabled
}

// SetTimeZoneID supplies the host's IANA zone id ("Europe/Rome") for the
// QUERY_TIME reply. Android knows it authoritatively; Go's own local location is
// usually nameless on a device. Configure it before Start.
func (s *PXCControl) SetTimeZoneID(id string) {
	s.timeZoneID = id
}

// SetTimeZoneOffsetSeconds supplies the host's UTC offset, DST already applied,
// for the clock replies. Configure it before Start.
//
// The id alone is not enough and was not enough in the field. On Android, Go's
// local location is UTC, so an ack built from a plain time.Now() carried a UTC
// wall clock while announcing the rider's zone by name - a Voge log on 1.1.45
// (2026-08-06) shows dateTime "05.08.2026 18:06:56" sent at 20:06:56 local, and
// currentTime equal to time because the offset read off that instant was zero.
// Dashes that seed their clock from this are set two hours wrong in Italy in
// summer, which riders reported as the dash losing its clock.
//
// The offset is taken rather than resolved from the id because Go embeds no zone
// database here (no time/tzdata import) and its Android lookup path moved into
// an APEX module on recent releases, so time.LoadLocation cannot be relied on.
// Android computes the offset for the current instant, DST included.
//
// It is captured once per session: a session running across a DST change would
// keep the offset it started with, which no ride is long enough to notice.
func (s *PXCControl) SetTimeZoneOffsetSeconds(seconds int) {
	s.timeZoneOffsetSec = seconds
	s.timeZoneOffsetSet = true
}

// SetSkipDashClockSync answers 0x10450 with an empty 0x10451 and never pushes
// an unsolicited clock JSON. Some SSDQ01-0120 units (VOGE-5G-dc41 logs) ask
// for time, ignore the JSON, and keep currentHUTime as uptime — 01.01.1970
// on the TFT. Writing millis there does not fix them and can clobber a clock
// the rider set by hand. Handshake still completes; the dash is just not told
// a wall clock. Configure it before Start.
func (s *PXCControl) SetSkipDashClockSync(skip bool) {
	s.skipDashClockSync = skip
}

// SetDashAsksForTime records that this dashboard has been seen sending 0x10450
// on an earlier connection, and suppresses the unsolicited push entirely. The
// solicited answer is untouched: the dash still asks and is still told the time.
//
// The grace period alone cannot decide this. It is a race against however long
// this particular handshake took, and a dash that asks at +2.4s one session can
// ask at +5.1s the next on a slower phone or a busier link - so any fixed window
// eventually loses and hands a second clock packet to a unit that never needed
// one. Whether a dash asks at all, by contrast, is a stable property of its
// firmware: seen once, it holds. The host remembers it per dashboard fingerprint
// and tells us here, which turns a timing guess into a fact.
//
// Deliberately not inferred inside this package: a PXCControl lives for one
// session and cannot remember the previous one, which is the whole point.
// Configure it before Start.
func (s *PXCControl) SetDashAsksForTime(asks bool) {
	s.dashAsksForTime = asks
}

// queryTimeAckBody and huTimeSyncAckBody exist so the two clock answers cannot
// be wired to a bare time.Now() again without a test noticing: the wiring, not
// the formatting, is what was wrong in the field.
func (s *PXCControl) queryTimeAckBody() []byte {
	if s.skipDashClockSync {
		return nil
	}
	return queryTimeAck(s.hostNow(), s.timeZoneID, s.HudConfig)
}

func (s *PXCControl) huTimeSyncAckBody(request []byte) ([]byte, string) {
	return huTimeSyncAck(request, s.hostNow())
}

// hostNow is time.Now() as the phone's own clock reads it. Everything the dash
// is told about the time must come from here, never from time.Now() directly.
func (s *PXCControl) hostNow() time.Time {
	now := time.Now()
	if !s.timeZoneOffsetSet {
		// Nothing better to go on than Go's idea of local, which is what every
		// build before the offset plumbing already sent.
		return now
	}
	return now.In(time.FixedZone(s.timeZoneID, s.timeZoneOffsetSec))
}

func (s *PXCControl) noteQueryTimeAnswered() {
	s.queryTimeMu.Lock()
	s.queryTimeAnswered = true
	s.queryTimeMu.Unlock()
}

func (s *PXCControl) queryTimeGracePeriod() time.Duration {
	if s.queryTimeGrace > 0 {
		return s.queryTimeGrace
	}
	return defaultQueryTimeGrace
}

// maybeScheduleProactiveQueryTime pushes one 0x10451 if this dash reports an
// unset clock and never asks 0x10450. A dash that already asks is unchanged:
// the solicited handler marks the question answered and this wait exits.
// SkipDashClockSync also bails out: those units must not receive clock JSON,
// and so does DashAsksForTime, which is the host telling us this dashboard
// asked on an earlier connection - see SetDashAsksForTime for why the grace
// period on its own is not enough to decide that.
func (s *PXCControl) maybeScheduleProactiveQueryTime(conn net.Conn) {
	if s.skipDashClockSync {
		return
	}
	if s.dashAsksForTime {
		// Seen asking on an earlier connection. Do not race its question.
		return
	}
	if s.HudConfig == nil || !huTimeLooksLikeUptime(s.HudConfig.CurrentHUTime) {
		return
	}
	huTime := s.HudConfig.CurrentHUTime
	s.proactiveQueryTimeOnce.Do(func() {
		state := s.connectionState(conn)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			timer := time.NewTimer(s.queryTimeGracePeriod())
			defer timer.Stop()
			select {
			case <-s.quit:
				return
			case <-state.done:
				return
			case <-timer.C:
			}
			s.queryTimeMu.Lock()
			already := s.queryTimeAnswered
			if !already {
				s.queryTimeAnswered = true
			}
			s.queryTimeMu.Unlock()
			if already {
				return
			}
			body := s.queryTimeAckBody()
			logging.Printf(
				"PXC QUERY_TIME never arrived (currentHUTime=%d); sending unsolicited ACK with %d body bytes",
				huTime,
				len(body),
			)
			response := &PXCResponse{Command: PxcQueryTimeAck, Body: body}
			if err := s.writeResponse(response, conn, nil); err != nil {
				if !s.isStopping() {
					s.emitError(&PxcError{PxcWriteErr, err, false})
				}
				return
			}
			s.emitEvent(*response)
		}()
	})
}

func (s *PXCControl) connectionState(conn net.Conn) *pxcConnectionState {
	state := &pxcConnectionState{done: make(chan struct{})}
	actual, _ := s.connections.LoadOrStore(conn, state)
	return actual.(*pxcConnectionState)
}

// nameChannel records which of the two reverse channels this connection is, from the request the
// dash opens it with. Deliberately not folded into startChannelHeartbeat: that returns immediately
// when the proactive keepalive is off, and a dash without the keepalive is exactly the one whose
// log needs the name most.
func (s *PXCControl) nameChannel(conn net.Conn, name string) {
	s.connectionState(conn).channel.Store(name)
}

// noteHeard marks the moment the dash last spoke on this connection and clears the run of
// unanswered keepalives.
func (s *PXCControl) noteHeard(state *pxcConnectionState) {
	state.lastRxNanos.Store(time.Now().UnixNano())
	state.beatsSince.Store(0)
}

func (s *PXCControl) releaseConnectionState(conn net.Conn) {
	value, ok := s.connections.LoadAndDelete(conn)
	if !ok {
		return
	}
	state := value.(*pxcConnectionState)
	state.doneOnce.Do(func() { close(state.done) })
	s.phoneMu.Lock()
	if s.phone == conn {
		s.phone = nil
	}
	s.phoneMu.Unlock()
}

func (s *PXCControl) heartbeatEvery() time.Duration {
	if s.heartbeatInterval > 0 {
		return s.heartbeatInterval
	}
	return defaultPxcHeartbeatPeriod
}

func (s *PXCControl) emitEvent(evt PXCResponse) {
	select {
	case s.Events <- evt:
	default:
		// channel full, drop
	}
}

func (s *PXCControl) emitError(err error) {
	select {
	case s.Errors <- err:
	default:
		logging.Printf("Dropping error from PXC [No one's listening!]: %v", err)
	}
}

// connectionError decides whether one connection's failure ends the session.
//
// The bike establishes this port twice - CAR_CTRL and CAR_DATA - which the note
// at the top of this file has called normal since the protocol was first
// written down, and then stops servicing the channel it has nothing to say on.
// A dash with no vehicle data to send simply abandons CAR_DATA; our proactive
// keepalive keeps writing into it until the kernel gives up on the
// retransmissions, minutes later, with ETIMEDOUT.
//
// Marking that fatal took down sessions that were working. Two Voge riders,
// one on a Samsung SM-S918B and one on a OnePlus, lost the TFT to
// "read tcp :10922->...: read: connection timed out" - the first of them ten
// times in a single day of riding - while the surviving PXC channel had
// answered a heartbeat 546ms and 1429ms earlier respectively, frames were
// still going out, and a reconnect two seconds later completed the whole
// handshake against the same dash.
//
// So it is fatal only when nothing else is being served here. A dash that
// really goes away kills every connection, and whichever one notices last finds
// itself alone and says so - as do the media control and media stream servers,
// which run their own listeners and report independently.
func (s *PXCControl) connectionError(conn net.Conn, errType PxcErrorType, cause error) *PxcError {
	others := s.tracker.Others(conn)
	if others == 0 {
		// Left exactly as it was written. A rider's collector groups failures by
		// the text of this line, so the sentence a real death produces has to go
		// on reading the same as every death before it.
		return &PxcError{errType, cause, true}
	}
	// The host relays a non-fatal transport error to the rider as a notice, so
	// this one says what happened before it says what the socket complained
	// about.
	return &PxcError{
		errType,
		fmt.Errorf(
			"one PXC channel closed, %d still serving the dash%s: %v",
			others, s.channelEpitaph(conn), cause,
		),
		false,
	}
}

// channelEpitaph names the channel that just died and says how long the dash had been ignoring it,
// or "" when this connection never got as far as naming itself.
//
// The raw TCP text is the wrong evidence for the question a field log has to answer. A Voge dash
// over Wi-Fi Direct ended a healthy twenty-minute session two minutes after one of these closed
// (2026-08-26), and telling "a channel the dash abandoned at the start, timing out on our own
// writes" from "a channel that was working until a moment ago" needed this source rather than the
// log. The run of unanswered keepalives is what separates them: ours went into that socket for the
// whole ride and nothing ever came back.
func (s *PXCControl) channelEpitaph(conn net.Conn) string {
	value, ok := s.connections.Load(conn)
	if !ok {
		return ""
	}
	state := value.(*pxcConnectionState)
	name, _ := state.channel.Load().(string)
	if name == "" {
		name = "it"
	}
	beats := state.beatsSince.Load()
	last := state.lastRxNanos.Load()
	if last == 0 {
		return fmt.Sprintf(" (%s never said anything, %d keepalive(s) unanswered)", name, beats)
	}
	silence := time.Since(time.Unix(0, last)).Round(time.Second)
	return fmt.Sprintf(" (%s last spoke %s ago, %d keepalive(s) unanswered since)", name, silence, beats)
}

func (s *PXCControl) decodeHeader(payload []byte) (*PXCResponse, error) {
	if len(payload) < pxcHeaderSize {
		return nil, errors.New("invalid header")
	}
	return &PXCResponse{
		Command: binary.LittleEndian.Uint32(payload[0:4]),
		Size:    binary.LittleEndian.Uint32(payload[4:8]),
		Magic:   binary.LittleEndian.Uint32(payload[8:12]),
		Token:   binary.LittleEndian.Uint32(payload[12:16]),
		Body:    nil,
	}, nil
}

func (s *PXCControl) buildPC() error {
	if s.HudConfig == nil {
		return errors.New("hudConfig is nil")
	}
	if s.HudConfig.HUID == "" {
		return errors.New("hudConfig.HUID is empty")
	}

	// Set public key
	if pubK, err := s.KeyPair.GetPublicEncoded(); err != nil {
		return err
	} else {
		s.PhoneConfig.Pubkey = pubK
	}

	// Encrypt HUID
	if enc, err := LegacyPrivateEncryptPKCS1v15(&s.KeyPair.PrivateKey, []byte(s.HudConfig.HUID)); err != nil {
		return err
	} else {
		s.PhoneConfig.EncryptedHUID = base64.StdEncoding.EncodeToString(enc)
	}
	return nil
}

func (s *PXCControl) writeResponse(res *PXCResponse, conn net.Conn, raw *[]byte) error {
	state := s.connectionState(conn)
	state.writeMu.Lock()
	defer state.writeMu.Unlock()

	if raw == nil && res != nil {
		// -- build header
		res.Token = 0 // Padding is always 0 for responses
		res.Size = pxcHeaderSize
		if len(res.Body) > 0 {
			res.Size = res.Size + uint32(len(res.Body))
		}
		res.Magic = res.Size ^ res.Command

		// -- write header
		if err := binary.Write(conn, binary.LittleEndian, res.Command); err != nil {
			return err
		}
		if err := binary.Write(conn, binary.LittleEndian, res.Size); err != nil {
			return err
		}
		if err := binary.Write(conn, binary.LittleEndian, res.Magic); err != nil {
			return err
		}
		if err := binary.Write(conn, binary.LittleEndian, res.Token); err != nil {
			return err
		}

		// -- write body
		if len(res.Body) > 0 {
			if _, err := conn.Write(res.Body); err != nil {
				return err
			}
		}
	} else if raw != nil {
		if _, err := conn.Write(*raw); err != nil {
			return err
		}
	}

	return nil
}

// Keep one selected PXC channel alive with empty 0x70000000 frames. A separate
// loop is required for CAR_CTRL and CAR_DATA because either idle socket can
// trigger a firmware-side teardown.
func (s *PXCControl) startChannelHeartbeat(conn net.Conn, channel string) {
	if !s.proactiveHeartbeat || s.isStopping() {
		return
	}
	state := s.connectionState(conn)
	state.heartbeatOnce.Do(func() {
		interval := s.heartbeatEvery()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			logging.Printf("PXC %s heartbeat started every %s", channel, interval)
			for {
				select {
				case <-s.quit:
					return
				case <-state.done:
					return
				case <-ticker.C:
					if err := s.writeResponse(&PXCResponse{Command: PxcHeartbeat}, conn, nil); err != nil {
						if !s.isStopping() {
							logging.Printf("PXC %s heartbeat stopped: %v", channel, err)
						}
						return
					}
					state.beatsSince.Add(1)
				}
			}
		}()
	})
}

func (s *PXCControl) handleEvent(event *PXCResponse, conn net.Conn) {
	if pxcIsReply(event.Command) && s.isPhoneConn(conn) {
		s.deliverP2CReply(conn, event)
	}
	switch event.Command {
	case PxcHandshake:
		// Named before it is answered, not after: a write that fails here produces an error whose
		// whole value is saying WHICH channel could not be answered.
		s.nameChannel(conn, "CAR_CTRL")
		// Emitted so a rider's log shows which channels the dash opened at all - their
		// absence from every export is what let phone commands go out on the wrong one.
		s.emitEvent(*event)
		response := &PXCResponse{Command: PxcHandshakeOk}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			return
		}
		s.startChannelHeartbeat(conn, "CAR_CTRL")
	case PxcChannelCarData:
		// CarbitRide's P2C channel; see phoneChannel.
		s.nameChannel(conn, "CAR_DATA")
		s.emitEvent(*event)
		response := &PXCResponse{Command: PxcChannelCarData + 1}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			return
		}
		s.setPhoneChannel(conn)
		s.startP2C()
		s.startChannelHeartbeat(conn, "CAR_DATA")
		// The official app's first APPSTATUS goes out as soon as this channel is up
		// (b0.r -> ec/v -> t0.q0), before any capture is negotiated.
		s.SendAppStatus(AppStatusBackground)
	case PxcHudConf:
		if s.HudConfig != nil {
			break
		}
		s.emitEvent(*event)
		// Read HudConfig and store it
		var conf HUDConfig
		if err := json.Unmarshal(event.Body, &conf); err != nil {
			s.emitError(&PxcError{PxcHudCfgErr, err, true})
			break
		}
		s.HudConfig = &conf
		// Set encrypted huid to phone config
		if err := s.buildPC(); err != nil {
			s.emitError(&PxcError{PxcHudCfgErr, err, true})
			break
		}
		// Respond with phone conf
		if b, err := json.Marshal(s.PhoneConfig); err != nil {
			s.emitError(&PxcError{PxcHudCfgErr, err, true})
			break
		} else {
			response := &PXCResponse{Command: PxcPhoneConf, Body: b}
			if err = s.writeResponse(response, conn, nil); err != nil {
				s.emitError(s.connectionError(conn, PxcWriteErr, err))
				break
			}
			s.emitEvent(*response)
			s.maybeScheduleProactiveQueryTime(conn)
		}
	case PxcSpeedConf:
		s.emitEvent(*event)
		response := &PXCResponse{Command: PxcSpeedOk}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
		}
	case PxcClientSet:
		s.emitEvent(*event)
		ack := &PXCResponse{Command: PxcCheckSnAck}
		if err := s.writeResponse(ack, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			break
		}

		var request checkSnRequest
		if len(event.Body) > 0 {
			if err := json.Unmarshal(event.Body, &request); err != nil {
				s.emitError(&PxcError{PxcDecodeErr, fmt.Errorf("decode CHECK_SN: %w", err), false})
			}
		}
		result := checkSnResult{
			IsOK:      true,
			ErrorCode: 0,
			ErrorMsg:  "",
			ID:        request.Serial,
			ClientSet: request.ClientSet,
		}
		if result.ClientSet == "" {
			result.ClientSet = "easy_conn"
		}
		body, err := json.Marshal(result)
		if err != nil {
			s.emitError(&PxcError{PxcWriteErr, err, true})
			break
		}
		s.sendCheckSnResult(&PXCResponse{Command: PxcCheckSnResult, Body: body}, conn)
	case PxcHeartbeat:
		s.emitEvent(*event) // Necessary to indicate PXC finished
		response := &PXCResponse{Command: PxcHeartbeatOk}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
		}
	case PxcCheckSnDone:
		// The bike acknowledges the phone-originated CHECK_SN_RESULT.
		s.emitEvent(*event)
	case PxcStatusUnsupported, PxcStatusError:
		// Even, but answers to something the phone sent (sdk/x0.java:133-140): acking one
		// would be answering an answer.
		logging.Printf("PXC status reply command=0x%x bodyBytes=%d", event.Command, len(event.Body))
		s.emitEvent(*event)
	case PxcHuTimeSync:
		// Same shape as the default even-command branch - emit, then ACK with
		// cmd+1 - except the body carries the wall-clock time the dash asked
		// for instead of being empty.
		s.emitEvent(*event)
		body, mode := s.huTimeSyncAckBody(event.Body)
		// mode is the whole diagnosis when a rider reports a wrong cluster clock:
		// "echo" means the dash already knew the time and we left it alone,
		// "phone" means its stamp was missing or nonsense and we supplied ours.
		logging.Printf("Answering PXC HU_TIME_SYNC with %d body bytes, mode=%s", len(body), mode)
		response := &PXCResponse{Command: PxcHuTimeSyncAck, Body: body}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			break
		}
		// A rider's diagnostics export only ever contains what crosses this
		// Events channel - never this package's own log.Printf line above - so
		// without this, nobody outside a live adb session can ever tell the ack
		// actually went out, let alone how many bytes it carried. That gap is
		// exactly what made an earlier Voge log unable to confirm or deny this
		// fix; PxcHudConf's reply was already visible this way, this was not.
		s.emitEvent(*response)
	case PxcQueryTime:
		// The dashes that ask this ask once, right after the handshake, so a
		// missed answer is a clock never set rather than one that drifts. The
		// body is JSON here, not the binary stamp above.
		// Marking the question answered first cancels the unsolicited wait
		// started from CLIENT_INFO, so a dash that already syncs (the common
		// Voge path) never receives a second 0x10451.
		s.noteQueryTimeAnswered()
		s.emitEvent(*event)
		body := s.queryTimeAckBody()
		if s.skipDashClockSync {
			logging.Printf("Answering PXC QUERY_TIME with empty body (dash clock sync skipped)")
		} else {
			logging.Printf("Answering PXC QUERY_TIME with %d body bytes", len(body))
		}
		response := &PXCResponse{Command: PxcQueryTimeAck, Body: body}
		if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			break
		}
		// See the matching comment on PxcHuTimeSync above: this is the only way
		// a rider's own diagnostics export can ever show the reply happened.
		s.emitEvent(*response)
	default:
		if event.Command&1 == 0 {
			// Unknown even commands are requests. CFDL26 uses several JSON and
			// binary notifications here and will not open 10921/10920 until ACKed.
			logging.Printf(
				"Acknowledging unhandled PXC request command=0x%x bodyBytes=%d",
				event.Command,
				len(event.Body),
			)
			s.emitEvent(*event)
			response := &PXCResponse{Command: event.Command + 1}
			if err := s.writeResponse(response, conn, nil); err != nil {
				s.emitError(s.connectionError(conn, PxcWriteErr, err))
			}
		} else {
			// Unknown odd commands are responses. Never answer them, otherwise
			// two peers using cmd+1 acknowledgements can create an ACK loop.
			logging.Printf(
				"Ignoring unhandled PXC response command=0x%x bodyBytes=%d",
				event.Command,
				len(event.Body),
			)
			s.emitEvent(*event)
		}
	}
}

func (s *PXCControl) Start() error {
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

func (s *PXCControl) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return // Shutting down
			default:
				logging.Printf("Error accepting connection: %v", err)
				continue
			}
		}

		logging.Printf("New PXC client from %s", conn.RemoteAddr())
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Handling a single TCP connection
func (s *PXCControl) handleConn(conn net.Conn) {
	state := s.connectionState(conn)
	s.tracker.Add(conn)
	defer func() {
		s.releaseConnectionState(conn)
		s.tracker.Remove(conn)
		_ = conn.Close()
		s.wg.Done()
	}()

	reader := bufio.NewReader(conn)

	// Retire the connection before judging the failure, never after. When the
	// dash really goes away every channel fails at once, and whichever one is
	// judged last has to find an empty tracker and end the session. Judging
	// first and removing afterwards lets two dying channels each see the other
	// still listed and call a real death survivable.
	fail := func(errType PxcErrorType, cause error) {
		s.tracker.Remove(conn)
		s.emitError(s.connectionError(conn, errType, cause))
	}

	logging.Printf("Starting PXC Loop")
	defer logging.Printf("Stopping PXC Loop")
	for {
		var request *PXCResponse

		// Read the 16 byte header
		headerBytes := make([]byte, pxcHeaderSize)
		if n, err := io.ReadFull(reader, headerBytes); err != nil {
			if s.isStopping() || state.superseded.Load() {
				return
			}
			fail(PxcDecodeErr, fmt.Errorf("error reading header: %v (read %d bytes: %x)", err, n, headerBytes[:n]))
			return
		}
		if req, err := s.decodeHeader(headerBytes); err != nil {
			fail(PxcDecodeErr, fmt.Errorf("error decoding header: %v", err))
			return
		} else {
			request = req
		}
		s.noteHeard(state)

		// Sanity check
		if request.Magic != request.Size^request.Command {
			logging.Printf("Sanity check failed: %d %d", request.Magic, request.Command)
			return
		}

		// Read event body if size is greater than 0
		var payload []byte
		if request.Size > 0 {
			payload = make([]byte, request.Size-pxcHeaderSize)
			if _, err := io.ReadFull(reader, payload); err != nil {
				if s.isStopping() || state.superseded.Load() {
					return
				}
				fail(PxcDecodeErr, fmt.Errorf("[PXCService] read payload failed from %s: %v", conn.RemoteAddr(), err))
				return
			}
			request.Body = payload
		}

		// Decide what to do with the event
		s.handleEvent(request, conn)
	}
}

func (s *PXCControl) isStopping() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *PXCControl) Stop(ctx context.Context) error {
	logging.Printf("Stopping pxc server")
	// Signal acceptLoop to stop
	s.stopOnce.Do(func() {
		close(s.quit)
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logging.Printf("[TCPService] error closing listener: %v", err)
			}
		}
	})

	// Close all tcp connections
	s.tracker.CloseAll()

	// Wait for all goroutines
	done := make(chan any)
	go func() {
		logging.Printf("Waiting for PXC routines to vacate")
		s.wg.Wait()
		logging.Printf("PXC routines exited")
		close(done)
	}()

	select {
	case <-ctx.Done():
		logging.Printf("PXC ctx timeout")
		return ctx.Err()
	case <-done:
		logging.Printf("PXC stream closed through done channel")
		if s.Errors != nil {
			close(s.Errors)
		}
		if s.Events != nil {
			close(s.Events)
		}
		return nil
	}
}
