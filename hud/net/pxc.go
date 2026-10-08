package net

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	// maxPxcBodySize bounds what one frame may declare. CarbitRide allocates whatever the
	// header says (sdk/i.java:101-108); a garbled length would take the whole app down here.
	// ponytail: the dash's own bodies are control JSON of a few KB; raise it if one is seen
	// closing on a real frame.
	maxPxcBodySize = 4 << 20
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
	// PxcTboxSerial is the dash stating its T-Box serial, {"tbox":{"sn":...}} (km/d.java).
	PxcTboxSerial uint32 = 0x102b0
	// PxcCarSupportCmds is ECP_C2P_CAR_SUPPORT_CMDS, a JSON object (km/h.java).
	PxcCarSupportCmds uint32 = 0x10470
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

// pxcCannedReplies are the requests the official app answers with a fixed body instead of an
// empty cmd+1. Each is what CarbitRide sends with nothing behind it - no GPS fix offered, no
// network shared - and a dash that parses the body found nothing in an empty one.
var pxcCannedReplies = map[uint32]json.RawMessage{
	// ECP_C2P_CHECK_NETWORK: {"status": the phone shares its network} (km/k.java:20-31).
	0x10410: json.RawMessage(`{"status":false}`),
	// QUERY_GPS: always false in CarbitRide, onReceiveQueryGps has no override
	// (km/m0.java:29-50, sdk/t0.java:590).
	0x10430: json.RawMessage(`{"status":false}`),
	// ECP_C2P_CHECK_APP_NETWORKING_CAPABILITY: 0, m() has no override (km/i.java:20-32).
	0x10750: json.RawMessage(`{"status":0}`),
	// A text the dash wants shown on the phone; answered {} (km/l0.java:22-31).
	0x104E0: json.RawMessage(`{}`),
}

func init() {
	// CarbitRide parses the list and does nothing else with it - ec/b.java has no G() override -
	// so the empty ack is its whole answer; a body that is not a JSON object, null included, is a
	// handler error (km/h.java:22-36). Only for flavor 51, as for the feature commands: any other
	// dash keeps the empty ack the default branch always gave it. The request itself reaches the
	// host's event stream as usual.
	RegisterC2PHandler(PxcCarSupportCmds, func(s *PXCControl, req *PXCResponse) ([]byte, error) {
		if _, err := optObject(req.Body); err != nil && s.flavor51Defaults() {
			return nil, err
		}
		return nil, nil
	})
}

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
	Flavor                   json.RawMessage `json:"flavor"`
	MirrorMode               int             `json:"mirrorMode"`
	PackageName              string          `json:"package_name"`
	ProductType              int             `json:"productType"`
	PxcVersion               string          `json:"pxcVersion"`
	ScreenType               int             `json:"screenType"`
	SdkVersion               string          `json:"sdkVersion"`
	SocketTimeoutPeriodWifi  int             `json:"socketTimeoutPeriodWifi"`
	SteeringMode             int             `json:"steeringMode"`
	SupportBTCall            bool            `json:"supportBTCall"`
	SupportBTSetting         bool            `json:"supportBTSetting"`
	SupportBackDesktop       bool            `json:"supportBackDesktop"`
	SupportBackDesktopNew    bool            `json:"supportBackDesktopNew"`
	SupportConnect           int             `json:"supportConnect"`
	SupportDownloadScreenEvt bool            `json:"supportDownloadScreenEvt"`
	SupportFunction          int             `json:"supportFunction"`
	// Kept raw: the official app tells "sent" from "absent" for these two (CI:887-890, 922-926).
	SupportFunctionEx         json.RawMessage `json:"supportFunctionEx"`
	SupportSpeechEngine       json.RawMessage `json:"supportSpeechEngine"`
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

// decodeLenient fills v from a JSON object the way the official app's opt* getters read
// CLIENT_INFO (ECP_C2P_CLIENT_INFO.java:744-975): a field of an unexpected JSON type is
// coerced where it can be and left at zero where it cannot, instead of failing the whole
// object. An empty body is an empty object, as it is there (:737-743). Only a body that is
// not a JSON object at all is an error.
func decodeLenient(data []byte, v any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	err := json.Unmarshal(data, v)
	var typeErr *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &typeErr) {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	rv := reflect.ValueOf(v).Elem()
	for i := 0; i < rv.NumField(); i++ {
		name, _, _ := strings.Cut(rv.Type().Field(i).Tag.Get("json"), ",")
		raw, ok := fields[name]
		if !ok || json.Unmarshal(raw, rv.Field(i).Addr().Interface()) == nil {
			continue
		}
		coerceJSON(rv.Field(i), raw)
	}
	return nil
}

// coerceJSON is JSONObject.optString / optInt / optBoolean: anything reads as text, numbers
// and numeric text read as integers truncated the way Number.intValue() truncates (so a
// supportFunction above 2^31 still decodes on the 32-bit ABI), and only true or "true" is true.
func coerceJSON(field reflect.Value, raw json.RawMessage) {
	var value any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return
	}
	text, isText := value.(string)
	number, isNumber := value.(float64)
	if isText {
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			number, isNumber = parsed, true
		}
	}
	switch field.Kind() {
	case reflect.String:
		if !isText {
			text = string(bytes.TrimSpace(raw))
		}
		field.SetString(text)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if isNumber {
			field.SetInt(int64(number))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if isNumber && number >= 0 {
			field.SetUint(uint64(number))
		}
	case reflect.Bool:
		field.SetBool(value == true || strings.EqualFold(text, "true"))
	}
}

// rawLong reads one raw JSON field the way JSONObject.optLong / getInt do: a number, or text
// holding one, truncated as Number.longValue() truncates. ok is false for anything else,
// absence included.
func rawLong(raw json.RawMessage) (value int64, ok bool) {
	var n json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	return int64(f), err == nil
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
	//
	// A pointer so the reply can leave it out: the official app omits it when it echoes
	// supportFunctionEx instead (CI:1030-1036). The host's profile value otherwise.
	SupportFunction   *int64 `json:"supportFunction,omitempty"`
	SupportFunctionEx *int64 `json:"supportFunctionEx,omitempty"`
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
	// speechEngineType is the extra the official app adds to this reply (ec/b.java:564-567); sent
	// only when the host asks for it (SetSpeechEngineType).
	SpeechEngineType *int `json:"speechEngineType,omitempty"`
}

// supportFunctionExMask is CI.r(): the bits the official app never echoes back in
// supportFunctionEx (ECP_C2P_CLIENT_INFO.java:486-488, 1035).
const supportFunctionExMask = 0xFFBFFFDF

// echoSupportFunction answers the dash's capabilities with its own, as the official app does
// (CI:877-890, 1030-1036): supportFunctionEx & 0xFFBFFFDF and no supportFunction when the dash
// sent an Ex; its supportFunction otherwise, 0 when it sent none (b0.O() for this build).
func echoSupportFunction(conf *HUDConfig, pc *PhoneConfig) {
	if ex, ok := rawLong(conf.SupportFunctionEx); ok {
		masked := ex & supportFunctionExMask
		pc.SupportFunction, pc.SupportFunctionEx = nil, &masked
		return
	}
	sf := int64(conf.SupportFunction)
	pc.SupportFunction, pc.SupportFunctionEx = &sf, nil
}

// carbitSpeechEngineType is the value the official app puts in speechEngineType: the dash's
// supportSpeechEngine, 2 when absent, and 2002 read as 2001 except on channel 16206 (XF06)
// (CI:922-932, ec/b.java:566).
func carbitSpeechEngineType(conf *HUDConfig) int {
	engine, ok := rawLong(conf.SupportSpeechEngine)
	if !ok {
		engine = 2
	}
	if engine == 2002 && conf.Channel != "16206" {
		engine = 2001
	}
	return int(engine)
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

	connections        sync.Map    // map[net.Conn]*pxcConnectionState
	tlsConfig          *tls.Config // what the phone serves on 0x107C0; nil: stay plaintext
	tlsOn              atomic.Bool // this session switched to TLS; new phone channels follow
	heartbeatInterval  time.Duration
	proactiveHeartbeat bool
	idleProbe          bool
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

	clientInfoMu sync.Mutex

	// What the CLIENT_INFO reply echoes of the dash's own; see SetEchoSupportFunction and
	// SetSpeechEngineType.
	echoSupportFunction bool
	speechEngineType    int

	// noFlavor51Lifecycle turns off what a flavor-51 dash gets by default; see carbitLifecycle.
	noFlavor51Lifecycle bool
	ecFrontOnce         sync.Once

	// The phone channel and its one sender; see p2c.go and phoneChannel.
	phoneMu    sync.Mutex
	phone      net.Conn
	p2cOnce    sync.Once
	p2cQueue   chan *p2cRequest
	p2cReplies chan p2cFrame
	// p2cTimeout is the dash's own reply timeout, from CLIENT_INFO; zero until it says.
	p2cTimeout atomic.Int64
	// lastHeardNanos is the last time the dash said anything on any PXC connection.
	lastHeardNanos atomic.Int64
	// HeardElsewhere, when set before Start, reports the last time the dash said anything
	// outside PXC - a video poll - so a dash still pulling frames is never judged dead.
	HeardElsewhere func() time.Time
	// OnTransport, when set before Start, receives this package's typed transport events
	// (emitTransport). The host sees them as EventSourceTransport events with this cmd.
	OnTransport func(cmd int, payload []byte)

	// mcuLite answers the MCU-Lite family on this port when the host enabled it (SetMCULite).
	mcuLite *MCULite
}

// C2PHandler answers one request the dash sends on PXC (an even command), for a command the
// switch in handleEvent does not name. It returns the body of the cmd+1 reply - nil for an empty
// one, which is what CarbitRide answers nearly every command it handles with (v0.d(),
// sdk/v0.java:50-66). An error is answered with a header-only 0x80000020 (handlerError), as
// CarbitRide answers a handler that failed, typically on a body that is not JSON.
//
// It runs on the connection's reader goroutine before the reply is written: it must not block.
// The request already reached the event stream; emitTransport is for a decoded, typed event.
type C2PHandler func(s *PXCControl, req *PXCResponse) (reply []byte, err error)

var (
	c2pHandlersMu sync.RWMutex
	c2pHandlers   = map[uint32]C2PHandler{}
)

// switchCommands are the requests handleEvent's switch answers itself, ahead of the registry.
// Keep it in step with that switch.
var switchCommands = map[uint32]bool{
	PxcHandshake: true, PxcChannelCarData: true, PxcHudConf: true, PxcStartTLS: true,
	PxcSpeedConf: true, PxcClientSet: true, PxcHeartbeat: true, PxcStatusUnsupported: true,
	PxcStatusError: true, PxcHuTimeSync: true, PxcQueryTime: true,
}

// RegisterC2PHandler makes h the answer to dash command cmd. Call it from an init() in the file
// that owns the command. A command the dispatch switch answers itself (switchCommands) or an odd
// one - a reply, which nothing answers - would never reach h, so registering one panics, as
// registering a command twice does: both are programming errors.
func RegisterC2PHandler(cmd uint32, h C2PHandler) {
	if switchCommands[cmd] || cmd&1 != 0 {
		panic(fmt.Sprintf("C2P handler for 0x%x would never run: handleEvent answers it itself or it is a reply", cmd))
	}
	c2pHandlersMu.Lock()
	defer c2pHandlersMu.Unlock()
	if _, dup := c2pHandlers[cmd]; dup {
		panic(fmt.Sprintf("C2P handler for 0x%x registered twice", cmd))
	}
	c2pHandlers[cmd] = h
}

func c2pHandler(cmd uint32) C2PHandler {
	c2pHandlersMu.RLock()
	defer c2pHandlersMu.RUnlock()
	return c2pHandlers[cmd]
}

// handlerError is CarbitRide's answer to a C2P handler that failed: 0x80000020 with NO body.
//
// When v0.d() returns non-zero, b1.a() calls om.c.a(code, throwable), then om.c.sendCmd()
// (sdk/b1.java:129-144). om.c.a() builds {"errCode","errMsg"} but stores it with
// this.mCmdResp.j(...) - the response buffer (om/c.java:23-35) - while z0.sendCmd() writes
// mCmdBaseReq, the request buffer (sdk_communication/z0.java:85-91). om.c never fills mCmdBaseReq
// (it has no preRequest()), and i.o() writes a 16-byte header with size 16 for a null body
// (sdk_communication/i.java:149-163). The cmd+1 is not sent either: v0.d() returns before it
// writes f78456d (sdk/v0.java:50-66). So the wire carries exactly {0x80000020, 16, 0x80000030, 0}.
// (A 0x80000020 the phone sends as a request, the RFCOMM bootstrap's failure text, does carry its
// body: qm/o.java sets it in preRequest(); see transport_rfcomm.go.)
func handlerError() *PXCResponse { return &PXCResponse{Command: PxcStatusError} }

// emitTransport hands the host one typed transport event; see OnTransport.
func (s *PXCControl) emitTransport(cmd int, payload []byte) {
	if s.OnTransport != nil {
		s.OnTransport(cmd, payload)
	}
}

// answerRegistered runs a registered handler and writes its reply on the connection it came in on.
func (s *PXCControl) answerRegistered(h C2PHandler, event *PXCResponse, conn net.Conn) {
	s.emitEvent(*event)
	body, err := h(s, event)
	response := &PXCResponse{Command: event.Command + 1, Body: body}
	if err != nil {
		logging.Printf("PXC 0x%x: %v; answering 0x%x", event.Command, err, PxcStatusError)
		response = handlerError()
	}
	if err := s.writeResponse(response, conn, nil); err != nil {
		s.emitError(s.connectionError(conn, PxcWriteErr, err))
	} else if response.Body != nil || response.Command != event.Command+1 {
		// As for QUERY_TIME: the only way a rider's log shows what the dash was told.
		s.emitEvent(*response)
	}
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
	// superseded marks a connection closed on purpose - a phone channel the dash replaced with a
	// newer one, or one whose TLS switch failed and was reported - so its closing is not reported
	// as a failure.
	superseded atomic.Bool
	// retired is why this side closed the connection (retire); its read loop reports that.
	retired atomic.Pointer[error]
	// reader is the connection's buffered reader, owned by its read loop; a TLS switch takes over
	// what it has buffered.
	reader *bufio.Reader
	// answered: the dash has answered a command of ours on this phone channel.
	answered atomic.Bool
	// dashBeats: the dash keeps this channel alive with heartbeats of its own.
	dashBeats atomic.Bool
	// mcuWatched: MCULite.watch has a watcher on this connection.
	mcuWatched atomic.Bool
	// opening is the first command the dash sent on this connection; its top bits say which
	// EasyConn family the connection belongs to (serveOtherFamily).
	opening atomic.Uint32
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

// SetEchoSupportFunction answers CLIENT_INFO with the dash's own supportFunction[Ex] the way the
// official app does (echoSupportFunction), instead of the host's profile value. Off by default:
// it claims every capability the dash offers, which the official app can back and a host may
// not. Configure it before Start.
func (s *PXCControl) SetEchoSupportFunction(echo bool) {
	s.echoSupportFunction = echo
}

// SetSpeechEngineType adds speechEngineType to the CLIENT_INFO reply: 0 leaves it out (as every
// earlier build did), a negative value sends what the official app sends
// (carbitSpeechEngineType), a positive one is sent as given. Configure it before Start.
func (s *PXCControl) SetSpeechEngineType(engine int) {
	s.speechEngineType = engine
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

// hudConfig is the dash's CLIENT_INFO, or nil before it, for readers off the connection that
// stored it.
func (s *PXCControl) hudConfig() *HUDConfig {
	s.clientInfoMu.Lock()
	defer s.clientInfoMu.Unlock()
	return s.HudConfig
}

// dashChannel is CLIENT_INFO's channel, "" before it.
func (s *PXCControl) dashChannel() string {
	if conf := s.hudConfig(); conf != nil {
		return conf.Channel
	}
	return ""
}

// flavor51 reports a dash whose CLIENT_INFO says flavor 51: CarbitRide's own family (the Zontes
// JCDZ21 among them), the only one known to be built against the official app's phone side.
func (s *PXCControl) flavor51() bool {
	conf := s.hudConfig()
	return conf != nil && strings.Trim(string(bytes.TrimSpace(conf.Flavor)), `"`) == "51"
}

// SetFlavor51Lifecycle controls what a flavor-51 dash gets without any profile asking: the
// official app's APPSTATUS announcements and its switch to the mirror page at stream start
// (carbitLifecycle), and its silence on 0x102B0. On by default; false restores the wire every
// earlier build put in front of that dash. Configure it before Start.
func (s *PXCControl) SetFlavor51Lifecycle(enabled bool) {
	s.noFlavor51Lifecycle = !enabled
}

// flavor51Defaults reports a flavor-51 dash the host has not opted out of SetFlavor51Lifecycle.
func (s *PXCControl) flavor51Defaults() bool {
	return !s.noFlavor51Lifecycle && s.flavor51()
}

// carbitLifecycle reports whether this dash gets the official app's unsolicited lifecycle
// commands: a profile asked for them (SetAppStatusNotify), or the dash is flavor 51. Every
// other dashboard paints today without ever having seen one of them from us.
func (s *PXCControl) carbitLifecycle() bool {
	s.appStatus.mu.Lock()
	enabled := s.appStatus.enabled
	s.appStatus.mu.Unlock()
	return enabled || s.flavor51Defaults()
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
	now := time.Now().UnixNano()
	state.lastRxNanos.Store(now)
	state.beatsSince.Store(0)
	s.lastHeardNanos.Store(now)
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

// retire closes a connection this side can no longer trust: a write the dash did not take within
// its own timeout, or a phone command it left unanswered that long, after which a late answer could
// be taken for the next command's (PXC has no transaction ids). A retired phone channel stops being
// the phone channel at once, and the dash opens a new one as it does after a Wi-Fi blip. Otherwise
// it goes the way a dead connection goes: its read loop ends and reports it through connectionError,
// fatal only when nothing else serves the dash, with cause in place of the socket's complaint.
func (s *PXCControl) retire(conn net.Conn, cause error) {
	if !s.connectionState(conn).retired.CompareAndSwap(nil, &cause) {
		return
	}
	s.phoneMu.Lock()
	if s.phone == conn {
		s.phone = nil
	}
	s.phoneMu.Unlock()
	logging.Printf("PXC: closing a connection: %v", cause)
	_ = conn.Close()
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

	// Set public key
	if pubK, err := s.KeyPair.GetPublicEncoded(); err != nil {
		return err
	} else {
		s.PhoneConfig.Pubkey = pubK
	}

	// Encrypt HUID. An empty or unencryptable one is sent as "", which is what the official
	// app does (RSAUtil.encryptHUID, RSAUtil.java:91-105) - it used to end the session here.
	s.PhoneConfig.EncryptedHUID = ""
	if s.HudConfig.HUID == "" {
		return nil
	}
	if enc, err := LegacyPrivateEncryptPKCS1v15(&s.KeyPair.PrivateKey, []byte(s.HudConfig.HUID)); err != nil {
		logging.Printf("PXC CLIENT_INFO: HUID not encrypted (%v); sending it empty", err)
	} else {
		s.PhoneConfig.EncryptedHUID = base64.StdEncoding.EncodeToString(enc)
	}
	return nil
}

func (s *PXCControl) writeResponse(res *PXCResponse, conn net.Conn, raw *[]byte) error {
	state := s.connectionState(conn)
	state.writeMu.Lock()
	defer state.writeMu.Unlock()

	var frame []byte
	if raw == nil && res != nil {
		frame = encodeFrame(res)
	} else if raw != nil {
		frame = *raw
	} else {
		return nil
	}
	// Bounded by the dash's own timeout: a dash that stops reading would otherwise hold this lock,
	// and the phone command queue behind it, for as long as the socket lives. A frame cut short
	// leaves nothing worth keeping on the socket, so it is retired.
	timeout := s.p2cReplyTimeout()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(frame); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			s.retire(conn, fmt.Errorf("the dash took nothing written to it for %s: %w", timeout, err))
		}
		return err
	}
	return nil
}

// encodeFrame fills in res's header and returns header and body as one frame: one write, as the
// official app flushes them (sdk/i.java:149-163) - four header writes on a TCP_NODELAY socket went
// out as four segments.
func encodeFrame(res *PXCResponse) []byte {
	res.Token = 0 // Padding is always 0 for responses
	res.Size = pxcHeaderSize + uint32(len(res.Body))
	res.Magic = res.Size ^ res.Command
	frame := make([]byte, pxcHeaderSize, int(res.Size))
	binary.LittleEndian.PutUint32(frame[0:], res.Command)
	binary.LittleEndian.PutUint32(frame[4:], res.Size)
	binary.LittleEndian.PutUint32(frame[8:], res.Magic)
	binary.LittleEndian.PutUint32(frame[12:], res.Token)
	return append(frame, res.Body...)
}

// pxcFrame is encodeFrame for a frame that is not a PXCResponse yet: {cmd, size, size^cmd, 0}
// little-endian, size counting the 16-byte header (sdk/i.java:144-163).
func pxcFrame(cmd uint32, body []byte) []byte {
	return encodeFrame(&PXCResponse{Command: cmd, Body: body})
}

// checkPXCHeader is the official app's check on a frame it reads: it closes the socket on a bad
// magic or a size under the header (sdk/i.java:97-105, MagicError / LengthError). Size 0 stays a
// header-only frame, as it always was here, and a size no frame could have is refused too.
func checkPXCHeader(h *PXCResponse) error {
	if h.Magic != h.Size^h.Command {
		return fmt.Errorf("bad PXC frame magic 0x%x for command 0x%x size %d", h.Magic, h.Command, h.Size)
	}
	if h.Size != 0 && (h.Size < pxcHeaderSize || h.Size-pxcHeaderSize > maxPxcBodySize) {
		return fmt.Errorf("bad PXC frame size %d for command 0x%x", h.Size, h.Command)
	}
	return nil
}

// Keep the dash's request channel (CAR_CTRL) alive with empty 0x70000000 frames, for firmware
// that never initiates heartbeats and tears down a channel gone quiet (CFMOTO 800NK / MTX800
// need both channels kept, see 15df75c). The phone channel gets its beat from the P2C sender,
// the only place CarbitRide beats (sdk/z.java:223-225, sdk/x0.java:192-209).
//
// CarbitRide never writes a command on the request channel; a dash built for it keeps that
// channel alive itself, and a phone command there is the issue-#12 class of bug. So a beat is
// skipped once the dash has shown its own keepalive on this channel.
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
					if state.dashBeats.Load() {
						continue
					}
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
	s.connectionState(conn).opening.CompareAndSwap(0, event.Command)
	if pxcIsReply(event.Command) && s.isPhoneConn(conn) {
		s.deliverP2CReply(conn, event)
	}
	switch event.Command { // every even case is in switchCommands
	case PxcHandshake:
		// Named before it is answered, not after: a write that fails here produces an error whose
		// whole value is saying WHICH channel could not be answered.
		s.nameChannel(conn, "CAR_CTRL")
		// A new request channel starts plaintext, and so do phone channels opened after it until
		// it asks again (sdk/h0.java:104-112).
		s.tlsOn.Store(false)
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
		if s.tlsOn.Load() {
			// The plaintext 0x20001, then TLS, before anything of ours goes out on it: CarbitRide's
			// P2C sender switches its socket when it starts (sdk/x0.java:175, h0.java:122-133).
			if !s.switchToTLS(conn, response) {
				return
			}
		} else if err := s.writeResponse(response, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			return
		}
		if !s.setPhoneChannel(conn) {
			s.connectionState(conn).superseded.Store(true)
			_ = conn.Close()
			return
		}
		s.startP2C()
		// The official app's first APPSTATUS goes out as soon as this channel is up
		// (b0.r -> ec/v -> t0.q0), before any capture is negotiated.
		s.announceAppStatus()
	case PxcHudConf:
		s.answerClientInfo(event, conn)
	case PxcStartTLS:
		s.answerStartTLS(event, conn)
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
			if err := decodeLenient(event.Body, &request); err != nil {
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
		s.connectionState(conn).dashBeats.Store(true)
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
		if s.serveOtherFamily(event, conn) {
			// A connection the dash opened as MCU-Lite, MCU or VM is answered from that family's
			// table, as CarbitRide answers it (transport_families.go); everything below is the car's.
		} else if h := c2pHandler(event.Command); h != nil && event.Command&1 == 0 {
			s.answerRegistered(h, event, conn)
		} else if event.Command == PxcTboxSerial && s.flavor51Defaults() {
			// CarbitRide answers nothing to 0x102B0, not even an error (v0.f(),
			// sdk/v0.java:77-80): a stray 0x102B1 can read as the answer to the dash's next
			// request. Only for flavor 51: the body reads like a CFMOTO T-Box notice, and CFDL26
			// waits for cmd+1 on the notices it sends.
			logging.Printf("PXC 0x%x from a flavor-51 dash: not answered, as the official app does not", event.Command)
			s.emitEvent(*event)
		} else if event.Command&1 == 0 {
			// Unknown even commands are requests. CFDL26 uses several JSON and
			// binary notifications here and will not open 10921/10920 until ACKed.
			//
			// The official app answers a command it has no handler for with 0x80000010
			// (sdk/b1.java:103-113) and every one it handles with cmd+1, empty unless it is
			// in pxcCannedReplies (sdk/v0.java:50-66). The empty cmd+1 is kept for the rest
			// anyway: the CFDL26 notifications above cannot be enumerated, and every command
			// a Carbit-built dash has been seen sending is one the official app handles.
			body := pxcCannedReplies[event.Command]
			logging.Printf(
				"Acknowledging unhandled PXC request command=0x%x bodyBytes=%d replyBytes=%d",
				event.Command,
				len(event.Body),
				len(body),
			)
			s.emitEvent(*event)
			response := &PXCResponse{Command: event.Command + 1, Body: body}
			if err := s.writeResponse(response, conn, nil); err != nil {
				s.emitError(s.connectionError(conn, PxcWriteErr, err))
			} else if body != nil {
				// As for QUERY_TIME: the only way a rider's log can show the body went out.
				s.emitEvent(*response)
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

// answerClientInfo handles CLIENT_INFO. The reply is built one CLIENT_INFO at a time - a dash
// opening two request channels at once must not have one reply marshalled while the other is
// still filling PhoneConfig - and written outside the lock.
func (s *PXCControl) answerClientInfo(event *PXCResponse, conn net.Conn) {
	s.emitEvent(*event)
	reply, err := s.clientInfoReply(event.Body)
	if err != nil {
		s.emitError(&PxcError{PxcHudCfgErr, err, true})
		return
	}
	response := &PXCResponse{Command: PxcPhoneConf, Body: reply}
	if err = s.writeResponse(response, conn, nil); err != nil {
		s.emitError(s.connectionError(conn, PxcWriteErr, err))
		return
	}
	s.emitEvent(*response)
	s.maybeScheduleProactiveQueryTime(conn)
	// A phone channel opened before CLIENT_INFO could not tell a flavor-51 dash yet.
	s.announceAppStatus()
}

func (s *PXCControl) clientInfoReply(body []byte) ([]byte, error) {
	s.clientInfoMu.Lock()
	defer s.clientInfoMu.Unlock()
	if s.HudConfig == nil {
		// Read HudConfig and store it, leniently (see decodeLenient): a field of a type
		// this struct did not expect used to end the session right after the handshake.
		var conf HUDConfig
		if err := decodeLenient(body, &conf); err != nil {
			return nil, err
		}
		s.HudConfig = &conf
		s.noteSocketTimeout(conf.SocketTimeoutPeriodWifi)
		// Set encrypted huid to phone config
		if err := s.buildPC(); err != nil {
			return nil, err
		}
		if s.echoSupportFunction {
			echoSupportFunction(&conf, s.PhoneConfig)
		}
		s.PhoneConfig.SpeechEngineType = nil
		if engine := s.speechEngineType; engine != 0 {
			if engine < 0 {
				engine = carbitSpeechEngineType(&conf)
			}
			s.PhoneConfig.SpeechEngineType = &engine
		}
	} else {
		// The dash sends CLIENT_INFO again when it reopens 0x10000 after a Wi-Fi blip, and
		// waits on its connect page for the reply. The official app answers every one
		// (sdk/b0.java:435-448, ECP_C2P_CLIENT_INFO.java:1058); this ignored all but the
		// first. ponytail: the first one's config is kept - same dash, same HUID; re-parse
		// if a dash is ever seen changing it mid-session.
		logging.Printf("PXC CLIENT_INFO again; answering it with this session's reply")
	}
	return json.Marshal(s.PhoneConfig)
}

// SetListener makes Start serve ln instead of listening on its port, for a host or a test that
// owns the socket. Stop closes it. Configure it before Start.
func (s *PXCControl) SetListener(ln net.Listener) { s.listener = ln }

func (s *PXCControl) Start() error {
	ln, err := listenOn(s.listener, s.port)
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
		go s.handleConn(&pxcConn{Conn: conn})
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
	state.reader = reader

	// Retire the connection before judging the failure, never after. When the
	// dash really goes away every channel fails at once, and whichever one is
	// judged last has to find an empty tracker and end the session. Judging
	// first and removing afterwards lets two dying channels each see the other
	// still listed and call a real death survivable.
	fail := func(errType PxcErrorType, cause error) {
		if retired := state.retired.Load(); retired != nil {
			errType, cause = PxcTimeoutErr, *retired
		}
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

		// Sanity check. This used to close silently on a bad magic and crash the app on a size
		// of 1-15, which underflowed make(); both now close and say so. See checkPXCHeader.
		if err := checkPXCHeader(request); err != nil {
			fail(PxcDecodeErr, err)
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
