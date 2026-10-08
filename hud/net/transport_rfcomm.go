package net

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// The classic-Bluetooth bootstrap (CarbitRide's SdpBluetoothUtil, qm/o.java). It runs before any
// Wi-Fi exists: the phone connects to the dash's RFCOMM service and the two agree on a network.
// The phone is the client and the frames are PXC frames (z0.sendCmd / waitResponse over the
// BluetoothSocket streams). Every request waits for exactly one frame back, whatever its command;
// CarbitRide never checks it, so neither does this (the dash's reply commands are unverified).
//
//	0x80000 EBT_CONN_TYPE_P2C       empty; up to 3 tries, 1 s apart (qm/o.java:269-291)
//	0x80010 EBT_P2C_CLIENT_INFO     {phoneType:0, phoneID, phoneName, packageName,
//	                                 netInterface:[{name, addr, mask}]} (rm/b.java)
//	0x80020 (build net, poll)       empty; the reply body is the dash's network JSON:
//	                                 status -1 ask again in 1 s, 2 the phone must open a
//	                                 hotspot, 0/1 done (qm/o.java:404-457)
//	0x80040 EBT_P2C_NOTIFY_AP_INFO  {ssid, pwd, auth, ip} of that hotspot (rm/c.java)
//	0x80000020                      the phone's error text when the hotspot failed (qm/o.java:76-97)
//	0x80030 (build net succeeded)   empty, on a fresh connection after 0x80000 (qm/o.java:546-581)
//
// The Android side owns the socket (BluetoothDevice.createRfcommSocketToServiceRecord with
// RfcommServiceUUID; the device must be bonded) and does the Wi-Fi work the answer asks for.

// RfcommServiceUUID is the dash's RFCOMM service (qm/o.java:58).
const RfcommServiceUUID = "9f03b326-5d75-46f1-9a39-b71f144d1d97"

const (
	rfcommConnect      uint32 = 0x80000
	rfcommClientInfo   uint32 = 0x80010
	rfcommBuildNet     uint32 = 0x80020
	rfcommBuildNetDone uint32 = 0x80030
	rfcommNotifyAP     uint32 = 0x80040

	rfcommConnectTries = 3
)

// RfcommInterface is one of the phone's IPv4 interfaces, as CarbitRide lists them: name, address
// and dotted netmask, loopback, tunnels, usb, rmnet and the like left out (qm/o.java:594-628).
type RfcommInterface struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
	Mask string `json:"mask"`
}

// RfcommClientInfo is EBT_P2C_CLIENT_INFO, in CarbitRide's key order.
type RfcommClientInfo struct {
	PhoneType    int               `json:"phoneType"`   // always 0 (Android)
	PhoneID      string            `json:"phoneID"`     // the persistent phone id
	PhoneName    string            `json:"phoneName"`   // the Bluetooth adapter name
	PackageName  string            `json:"packageName"` // the phone app's package
	NetInterface []RfcommInterface `json:"netInterface"`
}

// RfcommNetwork is the dash's answer to the build-net poll (qm/o.java:404-457, 476-503).
type RfcommNetwork struct {
	Status           int `json:"status"` // 0 already on one network, 1 join the dash's, 2 open a hotspot
	PhoneAPFrequency int `json:"phoneApFrequency"`
	CarNetDeviceInfo struct {
		SSID string `json:"ssid"`
		Pwd  string `json:"pwd"`
		Auth string `json:"auth"` // "WPA" or "WPA2"
		MAC  string `json:"mac"`
		Name string `json:"name"`
		Mode int    `json:"mode"` // 1, 2 join the access point; 8 Wi-Fi Direct to name/mac
	} `json:"carNetDeviceInfo"`
	PackageName string `json:"packagename"`
	Channel     string `json:"channel"`
	Flavor      int    `json:"flavor"`
	HUName      string `json:"HUName"`
	HUID        string `json:"HUID"`
	Raw         []byte `json:"-"`
}

// RfcommClient runs the bootstrap over a stream the host connected.
type RfcommClient struct {
	link io.ReadWriter
	pace time.Duration // CarbitRide's 1 s pauses; tests shrink it
}

func NewRfcommClient(link io.ReadWriter) *RfcommClient {
	return &RfcommClient{link: link, pace: time.Second}
}

// exchange writes one request and reads one frame back (z0.sendCmd, z0.waitResponse).
func (c *RfcommClient) exchange(cmd uint32, body []byte) (*PXCResponse, error) {
	if err := c.send(cmd, body); err != nil {
		return nil, err
	}
	return readPXCFrame(c.link)
}

func (c *RfcommClient) send(cmd uint32, body []byte) error {
	_, err := c.link.Write(pxcFrame(cmd, body))
	return err
}

// connect opens the bootstrap: 0x80000, up to three times, with CarbitRide's 1 s pause after
// each try - the answered one included - before anything else is sent.
func (c *RfcommClient) connect() error {
	var err error
	for try := 0; try < rfcommConnectTries; try++ {
		_, err = c.exchange(rfcommConnect, nil)
		time.Sleep(c.pace)
		if err == nil {
			return nil
		}
		logging.Printf("RFCOMM 0x%x: %v", rfcommConnect, err)
	}
	return fmt.Errorf("RFCOMM connect: %w", err)
}

// BuildNetwork connects, sends the phone's identity and polls until the dash says what network to
// use (qm/o.java:233-303). A status 2 answer leaves the phone to open a hotspot and report it on
// this same stream with NotifyAPInfo, or NotifyAPFailed.
func (c *RfcommClient) BuildNetwork(info RfcommClientInfo) (*RfcommNetwork, error) {
	if err := c.connect(); err != nil {
		return nil, err
	}
	info.PhoneType = 0
	if info.NetInterface == nil {
		info.NetInterface = []RfcommInterface{} // JSONArray: [] even when empty
	}
	body, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	if _, err := c.exchange(rfcommClientInfo, body); err != nil {
		return nil, fmt.Errorf("RFCOMM client info: %w", err)
	}
	for {
		reply, err := c.exchange(rfcommBuildNet, nil)
		if err != nil {
			return nil, fmt.Errorf("RFCOMM build net: %w", err)
		}
		if len(reply.Body) == 0 {
			return nil, errors.New("RFCOMM build net: the dash answered with no network")
		}
		network := RfcommNetwork{Status: -100} // optInt("status", -100)
		if err := decodeLenient(reply.Body, &network); err != nil {
			// A JSONException only logs and asks again (qm/o.java:453-455).
			logging.Printf("RFCOMM build net: %v", err)
			continue
		}
		if network.Status == -1 {
			time.Sleep(c.pace) // not ready yet
			continue
		}
		network.Raw = reply.Body
		return &network, nil
	}
}

// NotifyAPInfo reports the hotspot the phone opened for a status 2 answer and waits for the
// dash's reply (qm/o.java:461-484).
func (c *RfcommClient) NotifyAPInfo(ssid, pwd, auth, ip string) error {
	body, _ := json.Marshal(struct {
		SSID string `json:"ssid"`
		Pwd  string `json:"pwd"`
		Auth string `json:"auth"`
		IP   string `json:"ip"`
	}{ssid, pwd, auth, ip})
	_, err := c.exchange(rfcommNotifyAP, body)
	return err
}

// NotifyAPFailed tells the dash the hotspot could not be opened: 0x80000020 with the reason as
// its body, nothing awaited (qm/o.java:486-491).
func (c *RfcommClient) NotifyAPFailed(reason string) error {
	return c.send(PxcStatusError, []byte(reason))
}

// NotifyNetworkReady tells the dash the network is up, on a fresh connection: 0x80000 once, then
// 0x80030 (qm/o.java:546-581).
func (c *RfcommClient) NotifyNetworkReady() error {
	if _, err := c.exchange(rfcommConnect, nil); err != nil {
		return fmt.Errorf("RFCOMM connect: %w", err)
	}
	_, err := c.exchange(rfcommBuildNetDone, nil)
	return err
}

// readPXCFrame reads one PXC frame off a stream, checked as the PXC server checks its own.
func readPXCFrame(r io.Reader) (*PXCResponse, error) {
	header := make([]byte, pxcHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	frame, err := new(PXCControl).decodeHeader(header) // decodeHeader reads no state
	if err != nil {
		return nil, err
	}
	if err := checkPXCHeader(frame); err != nil {
		return nil, err
	}
	if frame.Size > pxcHeaderSize {
		frame.Body = make([]byte, frame.Size-pxcHeaderSize)
		if _, err := io.ReadFull(r, frame.Body); err != nil {
			return nil, err
		}
	}
	return frame, nil
}
