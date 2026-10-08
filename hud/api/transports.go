package api

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Alternative transports: MCU-Lite dashes, USB AOA and the classic-Bluetooth bootstrap. Android
// owns every physical link and hands it over as a Stream; see hud/net/transport_*.go for the
// protocols and the CarbitRide code they follow.

// Stream is a byte stream the app owns: a BluetoothSocket's streams, or a USB accessory's
// ParcelFileDescriptor streams. Read blocks until at least one byte is available and returns at
// most max bytes; an empty result or an error ends the stream. Write writes all of data. Close
// must unblock a pending Read.
type Stream interface {
	Read(max int) ([]byte, error)
	Write(data []byte) error
	Close() error
}

// streamRW is a Stream as an io.ReadWriteCloser.
type streamRW struct {
	s    Stream
	rest []byte
}

func (r *streamRW) Read(p []byte) (int, error) {
	if len(r.rest) == 0 {
		b, err := r.s.Read(len(p))
		if err != nil {
			return 0, err
		}
		if len(b) == 0 {
			return 0, io.EOF
		}
		r.rest = b
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func (r *streamRW) Write(p []byte) (int, error) {
	if err := r.s.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *streamRW) Close() error { return r.s.Close() }

// ---- USB AOA ----

// StartSessionOverUSB runs the session over a USB accessory link instead of Wi-Fi. The head unit
// tunnels its PXC, media and video connections through adb on the accessory; the library serves
// adb on the stream, opens the usual listeners (no EC init on 10930) and returns once the dash
// has completed the PXC handshake, like StartSession. Everything after that - events, PushFrame,
// SendPhoneCommand, MCU-Lite - is the Wi-Fi session unchanged.
//
// The stream is closed when the session stops; the session ends when the stream does. The
// head unit's adb banner is reported once as a transport event, core.TransportCmdUSBBanner.
func (ms *MobileSession) StartSessionOverUSB(accessory Stream) error {
	if accessory == nil {
		return errors.New("no accessory stream")
	}
	link := &streamRW{s: accessory}
	device := net.NewADBDevice(link)
	device.OnBanner = func(banner string) {
		ms.relayEvent(core.HudEvent{
			Source: core.EventSourceTransport,
			Time:   time.Now(),
			Cmd:    core.TransportCmdUSBBanner,
			Data:   []byte(banner),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ms.cfg.StartupTimeoutSec)*time.Second)
	defer cancel()
	err := ms.hud.StartStreamOverLink(ctx, func() error {
		go func() {
			logging.Printf("USB accessory link ended: %v", device.Serve())
		}()
		return nil
	})
	if err != nil {
		_ = link.Close()
		return err
	}
	go func() {
		<-ms.hud.Done()
		_ = link.Close()
	}()
	return nil
}

// ---- MCU-Lite ----

// MCULiteCallback hears from an MCU-Lite dash. Each call runs on its own goroutine.
type MCULiteCallback interface {
	// OnClientInfo: the dash identified itself (0x1080010). supportFunction bit 1 asks for
	// weather, bit 2 for GPS.
	OnClientInfo(huid, uuid string, channel, flavor, supportFunction int)
	// OnMirrorConfig: capture JPEG stills at width x height (already multiples of 16) with this
	// quality, about fps a second, and hand each to PushMCULiteJPEG. dpi 0 leaves it to the phone;
	// the screen size is in inches.
	OnMirrorConfig(width, height, fps, quality, dpi int, screenWidth, screenHeight float64)
	// OnGPSDue: every 60 s while frames flow, for a dash that asked for GPS: answer with
	// SetMCULiteGPS. OnWeatherDue: every 4 h for one that asked for weather (SetMCULiteWeather);
	// CarbitRide 2.4 never answers it.
	OnGPSDue()
	OnWeatherDue()
}

// EnableMCULite makes the session serve MCU-Lite dashes beside the car family. Call it before
// StartSession / StartSessionOverUSB. Without it MCU-Lite frames are answered as before.
func (ms *MobileSession) EnableMCULite(cb MCULiteCallback) error {
	m, err := ms.hud.EnableMCULite()
	if err != nil {
		return err
	}
	if cb == nil {
		return nil
	}
	m.OnClientInfo = func(c net.MCULiteClientInfo) {
		go cb.OnClientInfo(c.HUID, c.UUID, int(c.Channel), int(c.Flavor), int(c.SupportFunction))
	}
	m.OnMirrorConfig = func(c net.MCULiteMirrorConfig) {
		go cb.OnMirrorConfig(int(c.Width), int(c.Height), int(c.Fps), int(c.Quality), int(c.Dpi),
			float64(c.ScreenWidth), float64(c.ScreenHeight))
	}
	m.OnGPSDue = func() { go cb.OnGPSDue() }
	m.OnWeatherDue = func() { go cb.OnWeatherDue() }
	return nil
}

// PushMCULiteJPEG offers the MCU-Lite dash a new still; it is resent on every frame credit until
// the next one. Keep each under 25000 bytes, as CarbitRide does by lowering the quality.
func (ms *MobileSession) PushMCULiteJPEG(jpeg []byte) {
	if m := ms.hud.MCULite(); m != nil {
		m.PushJPEG(jpeg)
	}
}

// SetMCULiteGPS queues one GPS frame, sent ahead of the picture. Every value is a string as
// CarbitRide sends it: time is SystemClock.uptimeMillis(), status false when there is no fix.
func (ms *MobileSession) SetMCULiteGPS(status bool, latitude, longitude, altitude, speed, course, time string) {
	if m := ms.hud.MCULite(); m != nil {
		m.SetGPS(net.MCULiteGPS{Status: status, Latitude: latitude, Longitude: longitude,
			Altitude: altitude, Speed: speed, Course: course, Time: time})
	}
}

// SetMCULiteWeather queues one weather frame, sent ahead of the picture.
func (ms *MobileSession) SetMCULiteWeather(adCode, city, humidity, province, reportTime, temperature, weather, windDirection, windPower string) {
	if m := ms.hud.MCULite(); m != nil {
		m.SetWeather(net.MCULiteWeather{AdCode: adCode, City: city, Humidity: humidity,
			Province: province, ReportTime: reportTime, Temperature: temperature, Weather: weather,
			WindDirection: windDirection, WindPower: windPower})
	}
}

// ---- Classic-Bluetooth (RFCOMM) bootstrap ----

// RfcommServiceUUID is the dash's RFCOMM service: connect a bonded BluetoothDevice with
// createRfcommSocketToServiceRecord(UUID.fromString(RfcommServiceUUID)).
const RfcommServiceUUID = net.RfcommServiceUUID

// RfcommBootstrap agrees on a Wi-Fi network with a dash over RFCOMM, before any Wi-Fi exists.
type RfcommBootstrap struct {
	link   *streamRW
	client *net.RfcommClient
	ifaces []net.RfcommInterface
}

// RfcommNetwork is the dash's answer. Status 0: the phone is already on the dash's network, wait
// for mDNS. 1: join it - Mode 1 or 2 the access point SSID/Password/Auth ("WPA", "WPA2"), Mode 8
// Wi-Fi Direct to the peer DeviceName/MAC. 2: open a hotspot on PhoneAPFrequency and report it
// with NotifyAPInfo. JSON is the whole answer as the dash sent it.
type RfcommNetwork struct {
	Status           int
	PhoneAPFrequency int
	SSID             string
	Password         string
	Auth             string
	MAC              string
	DeviceName       string
	Mode             int
	PackageName      string
	Channel          string
	Flavor           int
	HUName           string
	HUID             string
	JSON             string
}

// NewRfcommBootstrap runs the bootstrap over a connected RFCOMM socket's streams.
func NewRfcommBootstrap(link Stream) *RfcommBootstrap {
	rw := &streamRW{s: link}
	return &RfcommBootstrap{link: rw, client: net.NewRfcommClient(rw)}
}

// AddNetInterface lists one of the phone's IPv4 interfaces in the client info, before
// BuildNetwork: name ("wlan0"), address and dotted mask. CarbitRide leaves out loopback, tun, usb,
// rmnet, ccmni, dummy and similar interfaces.
func (b *RfcommBootstrap) AddNetInterface(name, addr, mask string) {
	b.ifaces = append(b.ifaces, net.RfcommInterface{Name: name, Addr: addr, Mask: mask})
}

// BuildNetwork sends the phone's identity - its persistent id, its Bluetooth name and its package
// - and blocks until the dash says which network to use. Call it off the main thread.
func (b *RfcommBootstrap) BuildNetwork(phoneID, phoneName, packageName string) (*RfcommNetwork, error) {
	n, err := b.client.BuildNetwork(net.RfcommClientInfo{
		PhoneID: phoneID, PhoneName: phoneName, PackageName: packageName, NetInterface: b.ifaces,
	})
	if err != nil {
		return nil, err
	}
	d := n.CarNetDeviceInfo
	return &RfcommNetwork{
		Status: n.Status, PhoneAPFrequency: n.PhoneAPFrequency,
		SSID: d.SSID, Password: d.Pwd, Auth: d.Auth, MAC: d.MAC, DeviceName: d.Name, Mode: d.Mode,
		PackageName: n.PackageName, Channel: n.Channel, Flavor: n.Flavor, HUName: n.HUName,
		HUID: n.HUID, JSON: string(n.Raw),
	}, nil
}

// NotifyAPInfo reports the hotspot opened for a status 2 answer, on the same socket.
func (b *RfcommBootstrap) NotifyAPInfo(ssid, password, auth, ip string) error {
	return b.client.NotifyAPInfo(ssid, password, auth, ip)
}

// NotifyAPFailed tells the dash the hotspot could not be opened.
func (b *RfcommBootstrap) NotifyAPFailed(reason string) error {
	return b.client.NotifyAPFailed(reason)
}

// NotifyNetworkReady tells the dash the network is up. CarbitRide does it on a fresh RFCOMM
// connection: create a new RfcommBootstrap for it.
func (b *RfcommBootstrap) NotifyNetworkReady() error {
	return b.client.NotifyNetworkReady()
}

// Close closes the stream.
func (b *RfcommBootstrap) Close() error { return b.link.Close() }
