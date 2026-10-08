package net

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Dash feature messages: what CarbitRide 2.4 exchanges with a dash besides the picture -
// turn-by-turn, buttons, notifications, Bluetooth state and the car keyboard. The decoders in
// features_*.go read the dash's commands the way CarbitRide's km/* handlers read them; the
// builders write the phone's commands byte for byte as its mm/* senders do. Nothing here
// touches a socket: the replies go out through the C2P dispatch, the commands through
// SendPhoneCommand.
//
// Every feature handler in CarbitRide answers with an empty cmd+1 (sdk/v0.java:50-66: none of
// them sets a reply body), which is exactly what the default branch of handleEvent already
// sends. The one difference is a body that is not JSON: CarbitRide's handler returns
// C.RATE_UNSET_INT and the dispatch answers 0x80000020 instead (sdk/b1.java:129-147); the
// registry (init below) does the same for a flavor-51 dash.

// FeatureHandler decodes one dash-to-phone feature command. reply is the body of the cmd+1
// answer (always nil here); event is a pointer to one of the event types below, or nil when
// CarbitRide would do nothing with the command (an empty body); err means the body was not a
// JSON object, which CarbitRide answers with 0x80000020.
type FeatureHandler = func(body []byte) (reply []byte, event any, err error)

// featureDecoders lists every dash-to-phone feature command this package understands.
var featureDecoders = map[uint32]FeatureHandler{
	PxcNaviConfig:        adapt(DecodeNaviConfig),
	PxcDashButton:        adapt(DecodeDashKey),
	PxcGoHome:            adapt(DecodeGoHome),
	PxcBluetoothCheck:    adapt(DecodeBluetoothCheck),
	PxcCarBluetoothInfo:  adapt(DecodeCarBluetoothInfo),
	PxcCarInfo:           adapt(DecodeCarInfo),
	PxcCarKeyboardText:   adapt(DecodeImeText),
	PxcCarKeyboardAction: adapt(DecodeImeAction),
	PxcImeSelection:      adapt(DecodeImeSelection),
	PxcNotificationGate:  adapt(DecodeNotificationGate),
}

// adapt erases a decoder's event type without turning a nil *T into a non-nil any.
func adapt[T any](decode func([]byte) ([]byte, *T, error)) FeatureHandler {
	return func(body []byte) ([]byte, any, error) {
		reply, event, err := decode(body)
		if event == nil {
			return reply, nil, err
		}
		return reply, event, err
	}
}

// RegisterFeatureHandlers hands every feature decoder to a C2P handler registry, one call per
// command.
func RegisterFeatureHandlers(register func(cmd uint32, handler func(body []byte) (reply []byte, event any, err error))) {
	for cmd, handler := range featureDecoders {
		register(cmd, handler)
	}
}

// The feature decoders answer through the C2P registry. answerRegistered still puts the request
// on the PXC event stream first, exactly as the default branch did, and DashFeatures reads it from
// there (api.MobileSession.relayEvent), so the decoded event is not needed here.
func init() {
	RegisterFeatureHandlers(func(cmd uint32, decode FeatureHandler) {
		if cmd == PxcDashMainPage {
			// 0x10380 is pageswitch.go's: the same empty ack, plus its transport event.
			return
		}
		RegisterC2PHandler(cmd, func(s *PXCControl, req *PXCResponse) ([]byte, error) {
			reply, _, err := decode(req.Body)
			if err != nil && !s.flavor51Defaults() {
				// ponytail: 0x80000020 only where Carbit's lifecycle applies; a CFMOTO-lineage
				// dash keeps the empty cmd+1 the default branch always gave it (CFDL26 sends
				// binary notices there). Widen once a capture shows one of these commands.
				logging.Printf("PXC 0x%x: %v; acked anyway off flavor 51", req.Command, err)
				return reply, nil
			}
			return reply, err
		})
	})
}

// DecodeFeatureCommand decodes one dash command if it is a feature command. ok is false for
// any other command.
func DecodeFeatureCommand(cmd uint32, body []byte) (event any, ok bool, err error) {
	handler, ok := featureDecoders[cmd]
	if !ok {
		return nil, false, nil
	}
	_, event, err = handler(body)
	return event, true, err
}

// errNotJSONObject is the JSONException every km/* handler turns into 0x80000020.
var errNotJSONObject = errors.New("feature command body is not a JSON object")

// optObject reads a dash body the way CarbitRide's handlers do: decoded as UTF-8, skipped when
// empty (nil, nil), otherwise new JSONObject(body), which fails on anything but an object.
func optObject(body []byte) (map[string]json.RawMessage, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return nil, errNotJSONObject
	}
	return obj, nil
}

// optLong is JSONObject.optLong(key, def): numbers and numeric text, truncated; def when the
// key is missing or holds anything else. coerceJSON is the same rule CLIENT_INFO is read by.
func optLong(obj map[string]json.RawMessage, key string, def int64) int64 {
	raw, ok := obj[key]
	if !ok {
		return def
	}
	value := def
	if json.Unmarshal(raw, &value) != nil {
		value = def
		coerceJSON(reflect.ValueOf(&value).Elem(), raw)
	}
	return value
}

func optInt(obj map[string]json.RawMessage, key string, def int) int {
	return int(optLong(obj, key, int64(def)))
}

// optString is JSONObject.optString(key): text as is, any other value as its JSON text, ""
// when missing.
func optString(obj map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := obj[key]; ok && json.Unmarshal(raw, &value) != nil {
		coerceJSON(reflect.ValueOf(&value).Elem(), raw)
	}
	return value
}

// optBool is JSONObject.optBoolean(key): true for true or the text "true", any case.
func optBool(obj map[string]json.RawMessage, key string) bool {
	var value any
	_ = json.Unmarshal(obj[key], &value)
	text, _ := value.(string)
	return value == true || strings.EqualFold(text, "true")
}

// carbitJSON writes v the way Android's org.json writes every CarbitRide body: keys in
// insertion order (struct order here), '/' escaped as "\/", and < > & left alone.
func carbitJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil // unreachable: every body here is strings, numbers and booleans
	}
	// A '/' can only sit inside a string in this output, so the swap cannot touch structure.
	return bytes.ReplaceAll(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("/"), []byte(`\/`))
}

// FeatureClientInfo is what the feature senders read from CLIENT_INFO (0x10010), as
// ECP_C2P_CLIENT_INFO parses it (ECP_C2P_CLIENT_INFO.java:759-886).
type FeatureClientInfo struct {
	Channel   string // "channel": 33205 sends notifications without the 0x10740 gate
	BtAddress string // "btAddress": the dash's own BT MAC, checked on CAR_INFO bluetooth 4130
	// BluetoothPolicy bit 0: 0 grabs A2DP only over A2DP, 1 also over a headset link
	// (sdk/b0.java:262-272).
	BluetoothPolicy int
	// SupportFunction is supportFunction | supportFunctionEx (:877-886). Bit 0 is the car
	// keyboard (W0(), sdk/b0.java:566-572; ECP_C2P_CLIENT_INFO.java:678-680).
	SupportFunction int64
	// DisablePageInRVMap bits: 1 call, 2 music, 4 message - the phone pages CarbitRide hides
	// from the mirrored screen (sdk/b0.java:633-676, view/VirtualHomeView.java:357-364). They
	// gate nothing on the wire.
	DisablePageInRVMap int
	SupportBTCall      bool // "supportBTCall": gates 0x20080 (u0(), sdk/b0.java:842-845)
}

// DecodeFeatureClientInfo reads the CLIENT_INFO fields above. An empty body reads as zeros.
func DecodeFeatureClientInfo(body []byte) (*FeatureClientInfo, error) {
	obj, err := optObject(body)
	if err != nil {
		return nil, err
	}
	return &FeatureClientInfo{
		Channel:            optString(obj, "channel"),
		BtAddress:          optString(obj, "btAddress"),
		BluetoothPolicy:    optInt(obj, "bluetoothPolicy", 0),
		SupportFunction:    optLong(obj, "supportFunction", 0) | optLong(obj, "supportFunctionEx", 0),
		DisablePageInRVMap: optInt(obj, "disablePageInRVMap", 0),
		SupportBTCall:      optBool(obj, "supportBTCall"),
	}, nil
}
