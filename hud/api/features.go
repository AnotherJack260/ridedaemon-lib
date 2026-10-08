package api

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

// Dash features: everything CarbitRide 2.4 exchanges with a dash besides the picture -
// turn-by-turn arrows, dash buttons, notifications, Bluetooth state and the car keyboard. The
// wire formats live in hud/net/features_*.go, each with its CarbitRide file:line.
//
// Wiring: create one DashFeatures per MobileSession with NewDashFeatures. The session feeds it
// every PXC event itself, in the order the dash sent them; the app must NOT call
// handlePxcEvent as well, or every button and gate would be handled twice.
//
// Its senders block until the dash answers, like SendPhoneCommand: call them off the main
// thread, from one worker so they keep their order. A sender that returns false without an
// error skipped the command the way CarbitRide would (a gate, the 1 Hz limit, nothing to say).

// Maneuver descriptors for NaviInfo; see NaviInfo for the Android Auto mapping.
const (
	ManeuverUnknown        = net.ManeuverUnknown
	ManeuverDepart         = net.ManeuverDepart
	ManeuverDestination    = net.ManeuverDestination
	ManeuverStraight       = net.ManeuverStraight
	ManeuverTurn           = net.ManeuverTurn
	ManeuverMerge          = net.ManeuverMerge
	ManeuverFork           = net.ManeuverFork
	ManeuverOnRamp         = net.ManeuverOnRamp
	ManeuverOffRamp        = net.ManeuverOffRamp
	ManeuverRoundabout     = net.ManeuverRoundabout
	ManeuverRoundaboutExit = net.ManeuverRoundaboutExit
	ManeuverFerryBoat      = net.ManeuverFerryBoat
	ManeuverFerryTrain     = net.ManeuverFerryTrain
	ManeuverNameChange     = net.ManeuverNameChange

	DirectionUnspecified = net.DirectionUnspecified
	DirectionStraight    = net.DirectionStraight
	DirectionLeft        = net.DirectionLeft
	DirectionRight       = net.DirectionRight
	DirectionSlightLeft  = net.DirectionSlightLeft
	DirectionSlightRight = net.DirectionSlightRight
	DirectionSharpLeft   = net.DirectionSharpLeft
	DirectionSharpRight  = net.DirectionSharpRight
	DirectionKeepLeft    = net.DirectionKeepLeft
	DirectionKeepRight   = net.DirectionKeepRight
	DirectionUTurnLeft   = net.DirectionUTurnLeft
	DirectionUTurnRight  = net.DirectionUTurnRight
)

// What a dash button does (OnDashKey's action).
const (
	KeyActionNone      = net.KeyActionNone
	KeyActionPlay      = net.KeyActionPlay
	KeyActionNext      = net.KeyActionNext
	KeyActionPrev      = net.KeyActionPrev
	KeyActionPause     = net.KeyActionPause
	KeyActionPlayPause = net.KeyActionPlayPause
	KeyActionVoice     = net.KeyActionVoice
	KeyActionBack      = net.KeyActionBack
	KeyActionLandscape = net.KeyActionLandscape
	KeyActionPortrait  = net.KeyActionPortrait
	KeyActionRotate    = net.KeyActionRotate
)

// Bluetooth states for ReportBluetoothState, and the CAR_INFO values OnCarInfo carries.
const (
	BtStatePermissionDenied = net.BtStatePermissionDenied
	BtStateAdapterOff       = net.BtStateAdapterOff
	BtStateNotConnected     = net.BtStateNotConnected
	BtStateConnected        = net.BtStateConnected

	CarInfoOn         = net.CarInfoOn  // drivingMode on, or reversing started (pause music)
	CarInfoOff        = net.CarInfoOff // drivingMode off, or reversing ended (resume)
	CarBluetoothOff   = net.CarBluetoothOff
	CarBluetoothCheck = net.CarBluetoothCheck
	CarBluetooth4131  = net.CarBluetooth4131
)

// DisablePageInRVMap bits (read-only, from CLIENT_INFO): phone pages CarbitRide keeps off the
// mirrored screen. They gate nothing on the wire.
const (
	DisableInRVMapCall    = 1
	DisableInRVMapMusic   = 2
	DisableInRVMapMessage = 4
)

// Body-less phone commands to send with SendPhoneCommand as they are: open the dash's BT phone
// page, open its BT settings.
const (
	CmdOpenBtPhone    = int64(net.PxcOpenBtPhone)
	CmdOpenBtSettings = int64(net.PxcOpenBtSettings)
)

// RoundaboutDirection turns a roundabout exit angle (180 straight through, 90 right, 270 left)
// into a Direction* for NaviInfo.
func RoundaboutDirection(degrees float64) int {
	return net.RoundaboutDirection(degrees)
}

// DashFeatureListener receives what the dash asks of the phone. For a DashFeatures from
// NewDashFeatures the calls come one at a time, in the order the dash sent its commands, from the
// session's feature worker - never the main thread. A call may block (call a sender, say): only
// the feature calls after it wait; the session's events and OnEvent do not.
type DashFeatureListener interface {
	// OnNaviConfig: 0x10040, the dash's icon count; 19 (the legacy table) until it says.
	OnNaviConfig(maxNaviIcon int)
	// OnDashKey: 0x10350, already filtered to the types CarbitRide acts on and with next/prev
	// debounced to 500 ms. action is a KeyAction*.
	OnDashKey(code, keyType, action int)
	// OnNotificationGate: 0x10740. SendNotification follows it on its own.
	OnNotificationGate(enabled bool)
	// OnBluetoothCheck: 0x10390, or CAR_INFO bluetooth 4130 against CLIENT_INFO's btAddress (the
	// other three fields "" then). Check the phone's link to carBtMac and ReportBluetoothState.
	OnBluetoothCheck(carBtName, carBtMac, phoneBtName, phoneBtMac string)
	// OnCarBluetoothInfo: 0x10400, the dash's BT identity for auto-pairing.
	OnCarBluetoothInfo(name, address, pin string)
	// OnCarInfo: 0x10460; -1 for a field the dash left out. reversing CarInfoOn pauses music in
	// CarbitRide, CarInfoOff resumes it.
	OnCarInfo(drivingMode, reversing, bluetooth int)
	// OnCarKeyboardText, OnCarKeyboardAction, OnCarKeyboardSelection: 0x104F0, 0x10500 and
	// 0x70000030, the rider typing on the dash. CarbitRide hides the field on keyCode 4 and
	// presses "done" on anything else.
	OnCarKeyboardText(text string)
	OnCarKeyboardAction(actionID, keyCode int)
	OnCarKeyboardSelection(start, stop int)
	// OnGoHome: 0x10380, bring the app's home screen forward.
	OnGoHome()
}

// NaviInfo is one turn-by-turn update. Distances in metres, time in seconds; a negative value
// is sent as 0, as CarbitRide's Google path does (hl/e.java:227-229).
//
// The maneuver is a descriptor; from Android Auto's NavigationType: DEPART -> Depart;
// NAME_CHANGE -> NameChange; KEEP_*, TURN_SLIGHT_*, TURN_NORMAL_*, TURN_SHARP_*, U_TURN_* -> Turn
// with the matching direction; ON_RAMP_* and OFF_RAMP_* -> OnRamp / OffRamp the same way;
// FORK_*, MERGE_* -> Fork / Merge (side, or Unspecified); ROUNDABOUT_ENTER and
// ROUNDABOUT_ENTER_AND_EXIT_CW/CCW -> Roundabout, the direction from RoundaboutDirection(exit
// angle) for the *_WITH_ANGLE ones, clockwise for CW; ROUNDABOUT_EXIT -> RoundaboutExit;
// STRAIGHT -> Straight; FERRY_* -> FerryBoat / FerryTrain; DESTINATION_* -> Destination with
// Left / Right / Straight.
type NaviInfo struct {
	ManeuverKind        int // Maneuver*
	ManeuverDirection   int // Direction*
	RoundaboutClockwise bool
	NextRoad            string
	DistanceToManeuver  int
	RemainingDistance   int
	RemainingTime       int
}

// PhoneNotification is one notification for the dash. IconPNG may be empty.
type PhoneNotification struct {
	AppName string
	Title   string
	Text    string
	IconPNG []byte
}

// DashFeatures keeps what the dash said about its features and sends the phone's side.
type DashFeatures struct {
	send     func(cmd uint32, body []byte) (*PhoneCommandReply, error)
	now      func() time.Time // the phone's wall clock, in its own zone
	zoneID   string
	listener DashFeatureListener

	mu            sync.Mutex
	maxNaviIcon   int
	notifyEnabled bool
	info          net.FeatureClientInfo
	lastNavi      time.Time
	lastSkip      time.Time // last next/prev press that went through

	// The session's PXC events waiting for HandlePxcEvent; see enqueue.
	queueMu  sync.Mutex
	queue    []queuedPxcEvent
	draining bool
}

type queuedPxcEvent struct {
	timeMs  int64
	command int
	payload []byte
}

// NewDashFeatures binds the features to a session, which from then on feeds them every PXC event
// itself, in order (see DashFeatureListener). Do not call handlePxcEvent on the result: the
// session already does. listener may be nil. A second call for the same session replaces the
// first DashFeatures, which then hears nothing more.
//
// ponytail: CarbitRide resets the icon count and the notification gate when the phone channel
// reconnects (sdk/b0.java:354-367); here they last for the session, since it is the same dash.
func NewDashFeatures(session *MobileSession, listener DashFeatureListener) *DashFeatures {
	cfg := session.cfg
	f := &DashFeatures{
		send: func(cmd uint32, body []byte) (*PhoneCommandReply, error) {
			return session.SendPhoneCommand(int64(cmd), body, 0)
		},
		now: func() time.Time {
			// The same clock the session tells the dash the time with (PXCControl.hostNow):
			// Go's local zone is UTC on Android.
			if cfg.TimeZoneID == "" {
				return time.Now()
			}
			return time.Now().In(time.FixedZone(cfg.TimeZoneID, cfg.TimeZoneOffsetSeconds))
		},
		zoneID:      cfg.TimeZoneID,
		listener:    listener,
		maxNaviIcon: net.DefaultMaxNaviIcon,
	}
	session.features.Store(f)
	return f
}

// HandlePxcEvent reads one PXC event (OnEvent source 2) and reports whether it was one of the
// dash's feature commands. timeMs is OnEvent's time; the button debounce runs on it.
//
// The session calls this itself for a DashFeatures made by NewDashFeatures: the app must not
// call it too, or each command is handled twice. It is exported for a host that feeds the
// features some other way.
func (f *DashFeatures) HandlePxcEvent(timeMs int64, command int, payload []byte) bool {
	cmd := uint32(command)
	if cmd == net.PxcHudConf {
		// CLIENT_INFO: the gates below read it. Every one is taken, as CarbitRide re-reads each.
		if info, err := net.DecodeFeatureClientInfo(payload); err == nil {
			f.mu.Lock()
			f.info = *info
			f.mu.Unlock()
		}
		return false
	}
	event, ok, err := net.DecodeFeatureCommand(cmd, payload)
	if !ok || err != nil || event == nil {
		return ok
	}
	l := f.listener
	if l == nil {
		l = noListener{}
	}
	switch e := event.(type) {
	case *net.NaviConfig:
		f.mu.Lock()
		if e.MaxNaviIcon >= 0 {
			f.maxNaviIcon = e.MaxNaviIcon // km/t.java:41-43, ec/b.java:684-687
		}
		maxIcon := f.maxNaviIcon
		f.mu.Unlock()
		l.OnNaviConfig(maxIcon)
	case *net.DashKey:
		action := net.DashKeyAction(e.Code)
		if !net.DashKeyActs(e.Type) || ((action == net.KeyActionNext || action == net.KeyActionPrev) && !f.skipAllowed(timeMs)) {
			return true
		}
		l.OnDashKey(e.Code, e.Type, action)
	case *net.NotificationGate:
		f.mu.Lock()
		f.notifyEnabled = e.Enabled
		f.mu.Unlock()
		l.OnNotificationGate(e.Enabled)
	case *net.BluetoothCheck:
		if e.CarBtMac != "" { // ec/b.java:498-501
			l.OnBluetoothCheck(e.CarBtName, e.CarBtMac, e.PhoneBtName, e.PhoneBtMac)
		}
	case *net.CarBluetoothInfo:
		l.OnCarBluetoothInfo(e.Name, e.Address, e.Pin)
	case *net.CarInfo:
		l.OnCarInfo(e.DrivingMode, e.Reversing, e.Bluetooth)
		if e.Bluetooth == net.CarBluetoothCheck { // ec/b.java:783-786
			if mac := f.clientInfo().BtAddress; mac != "" {
				l.OnBluetoothCheck("", mac, "", "")
			}
		}
	case *net.ImeText:
		l.OnCarKeyboardText(e.Text)
	case *net.ImeAction:
		l.OnCarKeyboardAction(e.ActionID, e.KeyCode)
	case *net.ImeSelection:
		l.OnCarKeyboardSelection(e.Start, e.Stop)
	case *net.GoHome:
		l.OnGoHome()
	}
	return true
}

// enqueue hands one PXC event to HandlePxcEvent on this DashFeatures' worker: in order, one at a
// time, and without ever blocking the caller (relayEvent), whatever the listener does. The worker
// exists only while there is something to handle.
//
// ponytail: unbounded queue; a listener that never returns grows it by the dash's traffic (about
// a heartbeat a second) for the rest of the session. Bound it if that ever matters.
func (f *DashFeatures) enqueue(timeMs int64, command int, payload []byte) {
	f.queueMu.Lock()
	f.queue = append(f.queue, queuedPxcEvent{timeMs, command, payload})
	start := !f.draining
	f.draining = true
	f.queueMu.Unlock()
	if start {
		go f.drain()
	}
}

func (f *DashFeatures) drain() {
	for {
		f.queueMu.Lock()
		if len(f.queue) == 0 {
			f.draining = false
			f.queueMu.Unlock()
			return
		}
		e := f.queue[0]
		f.queue = f.queue[1:]
		f.queueMu.Unlock()
		f.HandlePxcEvent(e.timeMs, e.command, e.payload)
	}
}

// skipAllowed is the shared next/prev debounce (sdk/u.java:83-98).
func (f *DashFeatures) skipAllowed(timeMs int64) bool {
	at := time.UnixMilli(timeMs)
	if timeMs <= 0 {
		at = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if at.Sub(f.lastSkip) < net.KeyDebounceMillis*time.Millisecond {
		return false
	}
	f.lastSkip = at
	return true
}

func (f *DashFeatures) clientInfo() net.FeatureClientInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info
}

// MaxNaviIcon is the icon count the dash stated, or 19.
func (f *DashFeatures) MaxNaviIcon() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxNaviIcon
}

// NotificationsEnabled is SendNotification's gate: the dash sent 0x10740 {enabled:1}, or its
// channel is 33205 (NotificationService.java:338).
func (f *DashFeatures) NotificationsEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifyEnabled || f.info.Channel == net.NotificationOpenChannel
}

// DisablePageInRVMap is CLIENT_INFO's disablePageInRVMap; see the DisableInRVMap* bits.
func (f *DashFeatures) DisablePageInRVMap() int {
	return f.clientInfo().DisablePageInRVMap
}

// sendCmd sends one command and turns the dash's refusal into an error.
func (f *DashFeatures) sendCmd(cmd uint32, body []byte) error {
	reply, err := f.send(cmd, body)
	if err != nil {
		return err
	}
	if reply != nil {
		if status := uint32(reply.Command); status == net.PxcStatusUnsupported || status == net.PxcStatusError {
			return fmt.Errorf("phone command 0x%x: the dash refused it (0x%x)", cmd, status)
		}
	}
	return nil
}

func (f *DashFeatures) sendIf(allowed bool, cmd uint32, body []byte) (bool, error) {
	if !allowed {
		return false, nil
	}
	err := f.sendCmd(cmd, body)
	return err == nil, err
}

// SendNaviStatus sends 0x201E0: true when guidance starts, false after SendNaviEnd when it
// stops (MapboxDataHelper.java:164-183).
func (f *DashFeatures) SendNaviStatus(navigating bool) error {
	return f.sendCmd(net.PxcNaviStatus, net.BuildNaviStatusBody(navigating))
}

// SendNaviInfo sends one 0x20110 frame. Like CarbitRide it sends at most one a second and drops
// the rest (MapboxDataHelper.java:447-452), and skips a frame with nothing in it; either way it
// returns false, so just send the next update. The icon is picked for the dash's 0x10040
// maxNaviIcon and the distance to the maneuver floored to 50 m.
func (f *DashFeatures) SendNaviInfo(info *NaviInfo) (bool, error) {
	if info == nil {
		return false, errors.New("SendNaviInfo: nil NaviInfo")
	}
	now := f.now()
	f.mu.Lock()
	if now.Sub(f.lastNavi) < time.Second {
		f.mu.Unlock()
		return false, nil
	}
	f.lastNavi = now
	maxIcon := f.maxNaviIcon
	f.mu.Unlock()

	frame := net.HudInfo{
		NaviIcon:                     net.NaviIcon(info.ManeuverKind, info.ManeuverDirection, info.RoundaboutClockwise, maxIcon),
		NextRoad:                     info.NextRoad,
		RoadRemainingDistance:        net.FloorNaviDistance(info.DistanceToManeuver),
		DestinationRemainingDistance: max(info.RemainingDistance, 0),
		DestinationRemainingTime:     max(info.RemainingTime, 0),
	}
	return f.sendIf(!frame.Empty(), net.PxcHudInfo, net.BuildHudInfoBody(frame, now, f.zoneID))
}

// SendNaviEnd sends the end frame, 0x20110 with status 1, when guidance stops
// (GoogleMapNaviPresentation.java:483-487, :558-567). Then SendNaviStatus(false).
//
// ponytail: CarbitRide also withdraws any 0x20110 still queued (:561); the 1 Hz limit keeps at
// most one queued here.
func (f *DashFeatures) SendNaviEnd() error {
	return f.sendCmd(net.PxcHudInfo, net.BuildHudInfoBody(net.HudInfo{Status: 1}, f.now(), f.zoneID))
}

// SendNotification sends 0x20420 when the dash enabled notifications (or is channel 33205) and
// the text is not empty (SystemStatusMsgManager.java:248-264, NotificationService.java:334-339).
// Which apps to forward is the app's choice; see net.NotificationText for CarbitRide's list.
func (f *DashFeatures) SendNotification(n *PhoneNotification) (bool, error) {
	if n == nil {
		return false, errors.New("SendNotification: nil PhoneNotification")
	}
	body := net.BuildNotificationBody(n.IconPNG, n.AppName, n.Title, net.NotificationText(n.Text),
		f.now().Format(net.NotificationDateLayout))
	return f.sendIf(f.NotificationsEnabled() && n.Text != "", net.PxcNotification, body)
}

// ReportBluetoothState answers OnBluetoothCheck with a BtState*: 0x20380 first unless the state
// is BtStateConnected, then 0x20430 {state, phoneBtMac} (ec/b.java:496-528, :1636-1651).
// phoneBtMac is the one OnBluetoothCheck carried.
func (f *DashFeatures) ReportBluetoothState(state int, phoneBtMac string) error {
	var first error
	if state != net.BtStateConnected {
		first = f.sendCmd(net.PxcBtNotConnected, nil)
	}
	// Both go out whatever the first one met, as CarbitRide queues them independently.
	if err := f.sendCmd(net.PxcBtState, net.BuildBtStateBody(state, phoneBtMac)); first == nil {
		first = err
	}
	return first
}

// AcquireBtA2dp sends 0x20210 when the phone starts playing music or the mirror starts
// (MusicServiceManager.java:75-82, ec/v.java:849-851), if CLIENT_INFO's bluetoothPolicy allows
// it for the links that are up (sdk/b0.java:262-272, :386-406). CarbitRide waits at most 1 s
// for the answer before it plays.
func (f *DashFeatures) AcquireBtA2dp(a2dpConnected, headsetConnected bool) (bool, error) {
	allowed := net.AcquireBtA2dpAllowed(f.clientInfo().BluetoothPolicy, a2dpConnected, headsetConnected)
	return f.sendIf(allowed, net.PxcAcquireBtA2dp, []byte(net.AcquireBtA2dpBody))
}

// SendBtCallout sends 0x20080, asking a dash with supportBTCall to dial over its own Bluetooth.
// The Zontes 350D reports supportBTCall false, so it is always skipped there.
func (f *DashFeatures) SendBtCallout(name, number string) (bool, error) {
	return f.sendIf(f.clientInfo().SupportBTCall, net.PxcBtCallout, net.BuildBtCalloutBody(name, number))
}

// StartCarKeyboard opens a text field on the dash (0x202C0). imeOptions is 6 for a text field in
// CarbitRide (3 for its search boxes); maxLength 0 means 1024. Follow it with
// SendCarKeyboardText and SendCarKeyboardSelection, as CarbitRide does. All four car keyboard
// senders are skipped unless CLIENT_INFO supportFunction has bit 0.
func (f *DashFeatures) StartCarKeyboard(inputType, imeOptions int, rawText string, maxLength int) (bool, error) {
	return f.sendCarKeyboard(net.PxcImeStart, net.BuildImeStartBody(inputType, imeOptions, rawText, maxLength))
}

// SendCarKeyboardText sends the field's text (0x20300).
func (f *DashFeatures) SendCarKeyboardText(text string) (bool, error) {
	return f.sendCarKeyboard(net.PxcImeText, net.BuildImeTextBody(text))
}

// SendCarKeyboardSelection sends the field's selection (0x70000030).
func (f *DashFeatures) SendCarKeyboardSelection(start, stop int) (bool, error) {
	return f.sendCarKeyboard(net.PxcImeSelection, net.BuildImeSelectionBody(start, stop))
}

// EndCarKeyboard closes the field on the dash (0x202D0).
func (f *DashFeatures) EndCarKeyboard() (bool, error) {
	return f.sendCarKeyboard(net.PxcImeEnd, nil)
}

func (f *DashFeatures) sendCarKeyboard(cmd uint32, body []byte) (bool, error) {
	return f.sendIf(net.SupportsCarKeyboard(f.clientInfo().SupportFunction), cmd, body)
}

type noListener struct{}

func (noListener) OnNaviConfig(int)                                {}
func (noListener) OnDashKey(int, int, int)                         {}
func (noListener) OnNotificationGate(bool)                         {}
func (noListener) OnBluetoothCheck(string, string, string, string) {}
func (noListener) OnCarBluetoothInfo(string, string, string)       {}
func (noListener) OnCarInfo(int, int, int)                         {}
func (noListener) OnCarKeyboardText(string)                        {}
func (noListener) OnCarKeyboardAction(int, int)                    {}
func (noListener) OnCarKeyboardSelection(int, int)                 {}
func (noListener) OnGoHome()                                       {}
