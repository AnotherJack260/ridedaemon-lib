package net

// Bluetooth plumbing. The dash says which Bluetooth device it is (0x10390, 0x10400) and how its
// link stands (0x10460); the phone checks its own link to that device and reports the result
// (0x20380 + 0x20430), and asks the dash to route music over A2DP when it starts playing
// (0x20210).
const (
	// PxcBluetoothCheck is ECP_C2P_BLUETOOTH_CONNECTED {carBtName, carBtMac, phoneBtName,
	// phoneBtMac} (km/b.java:20-39).
	PxcBluetoothCheck uint32 = 0x10390
	// PxcCarBluetoothInfo is ECP_C2P_CAR_BLUETOOTH_INFO {name, address, pin}, which starts
	// CarbitRide's auto-pair (km/e.java:20-39, ec/b.java:669-676).
	PxcCarBluetoothInfo uint32 = 0x10400
	// PxcCarInfo is ECP_C2P_CAR_INFO {drivingMode, reversing, bluetooth} (km/f.java:31-64).
	PxcCarInfo uint32 = 0x10460

	// PxcBtState is ECP_P2C_NOTIFY_FUNCTION_STATE {state, phoneBtMac} (mm/y.java:30-50).
	PxcBtState uint32 = 0x20430
	// PxcBtNotConnected has no body and no name in the code; CarbitRide sends it right before
	// every 0x20430 that is not BtStateConnected (Y(), ec/b.java:1649-1651, called at :496-528 and
	// :1276-1440). The name is ours (unverified).
	PxcBtNotConnected uint32 = 0x20380
	// PxcAcquireBtA2dp is ECP_P2C_ACQUIRE_BT_A2DP {"sendPlayEvent":false} (mm/a.java:19-37).
	PxcAcquireBtA2dp uint32 = 0x20210
	// PxcBtCallout is ECP_P2C_CAR_CALLOUT_BY_BLUETOOTH {type:0, name, number}: the dash dials
	// over its own HFP link (ECP_P2C_CAR_CALLOUT_BY_BLUETOOTH.java:45-61). Only for a dash with
	// supportBTCall; the Zontes 350D reports false.
	PxcBtCallout uint32 = 0x20080
	// PxcOpenBtPhone and PxcOpenBtSettings carry no body: the dash opens its BT phone page
	// (view/PhoneCard.java:49-53) or its BT settings (MirrorAutoPairView.java:67). Send them
	// with SendPhoneCommand as they are.
	PxcOpenBtPhone    uint32 = 0x20250
	PxcOpenBtSettings uint32 = 0x202A0
)

// The 0x20430 states (ec/b.java:496-528, :1239-1440).
const (
	BtStatePermissionDenied = 4096 // 0x1000: BLUETOOTH_CONNECT not granted
	BtStateAdapterOff       = 4097 // 0x1001: the phone's Bluetooth is off
	BtStateNotConnected     = 4098 // 0x1002: not linked to the dash's carBtMac after ~2 s of checks
	BtStateConnected        = 4099 // 0x1003: linked to carBtMac (CarbitRide looks on HID host, A2DP, headset and HID device)
)

// CAR_INFO values (ec/b.java:762-800). drivingMode and reversing: 4112 on, 4113 off.
// bluetooth: 4128 is the dash's Bluetooth closed (MirrorAutoPairView.java:142-144); 4130 makes
// CarbitRide run the 0x10390 check against CLIENT_INFO's btAddress, and both 4130 and 4131
// dismiss its pairing prompt. What 4130 and 4131 themselves mean is unverified.
const (
	CarInfoOn         = 4112 // 0x1010
	CarInfoOff        = 4113 // 0x1011
	CarBluetoothOff   = 4128 // 0x1020
	CarBluetoothCheck = 4130 // 0x1022
	CarBluetooth4131  = 4131 // 0x1023
)

// BluetoothCheck is 0x10390. CarbitRide ignores it when carBtMac is empty (ec/b.java:498-501).
type BluetoothCheck struct {
	CarBtName, CarBtMac, PhoneBtName, PhoneBtMac string
}

// DecodeBluetoothCheck decodes 0x10390; an empty body is ignored.
func DecodeBluetoothCheck(body []byte) ([]byte, *BluetoothCheck, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &BluetoothCheck{
		CarBtName:   optString(obj, "carBtName"),
		CarBtMac:    optString(obj, "carBtMac"),
		PhoneBtName: optString(obj, "phoneBtName"),
		PhoneBtMac:  optString(obj, "phoneBtMac"),
	}, nil
}

// CarBluetoothInfo is 0x10400, the dash's own Bluetooth identity for pairing.
type CarBluetoothInfo struct {
	Name, Address, Pin string
}

// DecodeCarBluetoothInfo decodes 0x10400; an empty body is ignored.
func DecodeCarBluetoothInfo(body []byte) ([]byte, *CarBluetoothInfo, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &CarBluetoothInfo{
		Name:    optString(obj, "name"),
		Address: optString(obj, "address"),
		Pin:     optString(obj, "pin"),
	}, nil
}

// CarInfo is 0x10460. A field the dash left out is -1.
type CarInfo struct {
	DrivingMode, Reversing, Bluetooth int
}

// DecodeCarInfo decodes 0x10460; an empty body is ignored.
func DecodeCarInfo(body []byte) ([]byte, *CarInfo, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &CarInfo{
		DrivingMode: optInt(obj, "drivingMode", -1),
		Reversing:   optInt(obj, "reversing", -1),
		Bluetooth:   optInt(obj, "bluetooth", -1),
	}, nil
}

// BuildBtStateBody builds a 0x20430 body; a missing MAC is sent as "" (mm/y.java:36-49).
func BuildBtStateBody(state int, phoneBtMac string) []byte {
	return carbitJSON(struct {
		State      int    `json:"state"`
		PhoneBtMac string `json:"phoneBtMac"`
	}{state, phoneBtMac})
}

// AcquireBtA2dpBody is the 0x20210 body: sendPlayEvent is never set to anything but false
// (mm/a.java:14-17, sdk/b0.java:78-86).
const AcquireBtA2dpBody = `{"sendPlayEvent":false}`

// AcquireBtA2dpAllowed is b0.R0(), the bluetoothPolicy gate on 0x20210 (sdk/b0.java:262-272):
// policy bit 0 clear needs A2DP to be connected, set accepts a headset link too.
func AcquireBtA2dpAllowed(bluetoothPolicy int, a2dpConnected, headsetConnected bool) bool {
	if bluetoothPolicy&1 == 0 {
		return a2dpConnected
	}
	return a2dpConnected || headsetConnected
}

// BuildBtCalloutBody builds a 0x20080 body (ECP_P2C_CAR_CALLOUT_BY_BLUETOOTH.java:50-61). type is
// always 0.
func BuildBtCalloutBody(name, number string) []byte {
	return carbitJSON(struct {
		Type   int    `json:"type"`
		Name   string `json:"name"`
		Number string `json:"number"`
	}{0, name, number})
}
