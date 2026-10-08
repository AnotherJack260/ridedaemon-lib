package net

// The car keyboard: the phone opens a text field on the dash (0x202C0), keeps its text and
// selection in step (0x20300, 0x70000030) and closes it (0x202D0); the dash sends back what the
// rider typed (0x104F0), its editor action (0x10500) and its selection (0x70000030). CarbitRide
// does all of it only for a dash whose CLIENT_INFO supportFunction has bit 0 set (W0(),
// sdk/b0.java:566-572), and the dash side only reaches a field that is registered.
const (
	// PxcCarKeyboardText is 0x104F0 {text} (km/a0.java:16-36).
	PxcCarKeyboardText uint32 = 0x104F0
	// PxcCarKeyboardAction is 0x10500 {actionId, keyCode} (km/z.java:16-37). CarbitRide hides the
	// field on keyCode 4 and presses "done" on anything else (BacKMirrorTools.java:170-188).
	PxcCarKeyboardAction uint32 = 0x10500
	// PxcImeSelection is ECP_P2C_INPUT_SELECTION {start, stop}. The dash sends the same number
	// back (km/y.java:17-38).
	PxcImeSelection uint32 = 0x70000030
	// PxcImeStart is ECP_P2C_INPUT_START (mm/v.java:66-86).
	PxcImeStart uint32 = 0x202C0
	// PxcImeText is the phone's text {text} (mm/w.java:22-40).
	PxcImeText uint32 = 0x20300
	// PxcImeEnd closes the field; no body (mm/t.java:12-15, BacKMirrorTools.java stopSafeDriveOverlay).
	PxcImeEnd uint32 = 0x202D0
)

// SupportsCarKeyboard is the IME gate: supportFunction bit 0 (ECP_C2P_CLIENT_INFO.java:678-680).
func SupportsCarKeyboard(supportFunction int64) bool {
	return supportFunction&1 == 1
}

// ImeText is 0x104F0.
type ImeText struct{ Text string }

// DecodeImeText decodes 0x104F0; an empty body is ignored.
func DecodeImeText(body []byte) ([]byte, *ImeText, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &ImeText{Text: optString(obj, "text")}, nil
}

// ImeAction is 0x10500.
type ImeAction struct{ ActionID, KeyCode int }

// DecodeImeAction decodes 0x10500; an empty body is ignored.
func DecodeImeAction(body []byte) ([]byte, *ImeAction, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &ImeAction{ActionID: optInt(obj, "actionId", 0), KeyCode: optInt(obj, "keyCode", 0)}, nil
}

// ImeSelection is 0x70000030 from the dash.
type ImeSelection struct{ Start, Stop int }

// DecodeImeSelection decodes the dash's 0x70000030; an empty body is ignored. Stop is read from
// "stop" as the phone writes it; CarbitRide itself passes "start" twice (km/y.java:30), so
// whether a dash ever fills "stop" is unverified.
func DecodeImeSelection(body []byte) ([]byte, *ImeSelection, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &ImeSelection{Start: optInt(obj, "start", 0), Stop: optInt(obj, "stop", 0)}, nil
}

// BuildImeStartBody builds a 0x202C0 body. minLines and maxLines are always 1 and 3; a
// maxLength of 0 is sent as 1024 (mm/v.java:34-60, :72-80). CarbitRide's imeOptions is 6 for a
// TextView field and 3 for its own input objects (BacKMirrorTools.java registSoftInput).
func BuildImeStartBody(inputType, imeOptions int, rawText string, maxLength int) []byte {
	if maxLength == 0 {
		maxLength = 1024
	}
	return carbitJSON(struct {
		InputType  int    `json:"inputType"`
		ImeOptions int    `json:"imeOptions"`
		RawText    string `json:"rawText"`
		MinLines   int    `json:"minLines"`
		MaxLines   int    `json:"maxLines"`
		MaxLength  int    `json:"maxLength"`
	}{inputType, imeOptions, rawText, 1, 3, maxLength})
}

// BuildImeTextBody builds a 0x20300 body (mm/w.java:28-39).
func BuildImeTextBody(text string) []byte {
	return carbitJSON(struct {
		Text string `json:"text"`
	}{text})
}

// BuildImeSelectionBody builds the phone's 0x70000030 body. A negative start is sent as 0; stop
// goes as given, because mm/u.b() tests the old value instead of the new one (mm/u.java:26-40).
func BuildImeSelectionBody(start, stop int) []byte {
	return carbitJSON(struct {
		Start int `json:"start"`
		Stop  int `json:"stop"`
	}{max(start, 0), stop})
}
