package net

// PxcDashButton is ECP_C2P_SEND_BTN_EVENT_TO_PHONE {code, type}: a button on the dash or the
// handlebar (km/v0.java:19-39).
const PxcDashButton uint32 = 0x10350

// PxcGoHome is 0x10380, the dash asking for the phone app's home screen (km/e1.java:10-18).
const PxcGoHome uint32 = 0x10380

// What a button does in CarbitRide: the runnable in sdk/u.java:56-127, through the actions in
// net/easyconn/carman/l.java:31-161.
const (
	KeyActionNone      = iota // a code CarbitRide maps to an empty action (0x1010, 0x1020, 0x1050, 0x1051, 0x1070) or does not know
	KeyActionPlay             // 0x1040: resume, if nothing is playing (l.java:121-135)
	KeyActionNext             // 0x1041, debounced (l.java:108-119)
	KeyActionPrev             // 0x1042, debounced (l.java:145-156)
	KeyActionPause            // 0x1043 and 0x1044: pause, if playing (l.java:70-81)
	KeyActionPlayPause        // 0x1047 (l.java:87-98)
	KeyActionVoice            // 0x1030: the voice assistant (l.java:158-161, HomeActivity.java:1246-1252)
	KeyActionBack             // 0x1080 (l.java:31-68)
	KeyActionLandscape        // 0x1090: force landscape (u.java:106-108)
	KeyActionPortrait         // 0x1091: force portrait (u.java:109-111)
	KeyActionRotate           // 0x1092: toggle landscape/portrait (u.java:112-119)
)

// KeyDebounceMillis is the minimum gap between two next/prev presses; they share one clock
// and a press inside the gap is dropped without moving it (sdk/u.java:83-98).
const KeyDebounceMillis = 500

// DashKeyActs reports the button types CarbitRide acts on, 2 and 4; anything else is refused
// before the action table (sdk/t.java:23-33, ec/b.java:657-668). What the two types mean is not
// visible in the code: t.c() only turns them into -95 and -93.
func DashKeyActs(keyType int) bool {
	return keyType == 2 || keyType == 4
}

// DashKey is 0x10350 as the dash sent it.
type DashKey struct {
	Code int
	Type int
}

// DecodeDashKey decodes 0x10350; an empty body is ignored (km/v0.java:25-38).
func DecodeDashKey(body []byte) ([]byte, *DashKey, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &DashKey{Code: optInt(obj, "code", 0), Type: optInt(obj, "type", 0)}, nil
}

// DashKeyAction maps a button code to what CarbitRide does with it.
func DashKeyAction(code int) int {
	switch code {
	case 0x1040:
		return KeyActionPlay
	case 0x1041:
		return KeyActionNext
	case 0x1042:
		return KeyActionPrev
	case 0x1043, 0x1044:
		return KeyActionPause
	case 0x1047:
		return KeyActionPlayPause
	case 0x1030:
		return KeyActionVoice
	case 0x1080:
		return KeyActionBack
	case 0x1090:
		return KeyActionLandscape
	case 0x1091:
		return KeyActionPortrait
	case 0x1092:
		return KeyActionRotate
	}
	return KeyActionNone
}

// GoHome is 0x10380: the dash asks the phone app to bring its home screen forward. It carries
// no body; CarbitRide starts its home activity (km/e1.java:10-18, ec/b.java:838-850, :336-340).
type GoHome struct{}

// DecodeGoHome decodes 0x10380.
func DecodeGoHome([]byte) ([]byte, *GoHome, error) {
	return nil, &GoHome{}, nil
}
