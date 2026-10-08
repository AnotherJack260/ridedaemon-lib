package api

import (
	"github.com/charliecharlieO-o/ridedaemon-go/hud/core"
	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

// Control-plane API: what the host can tell the dash, or about itself, beyond the video. The
// transport events these produce are listed in hud/core (TransportCmd*) and hud/net (Pxc*).

// ReleaseDash hands the dash back its own UI the way the official app does when the rider
// disconnects: switchEc2Background (0x20370), then STOP_SERVICE (0x20150), on the phone channel,
// each waiting at most a second for the dash's answer. Call it when the rider stops, before
// StopSession, off the main thread. Returns STOP_SERVICE's outcome; an error is informational,
// StopSession still applies.
func (ms *MobileSession) ReleaseDash() error {
	return ms.hud.ReleaseDash()
}

// SetAppForeground tells the library whether the app's own UI is in the phone's foreground. The
// official app announces its mirroring state with 0x20020 instead of 0x20030 while its UI shows,
// and again whenever that changes. Call it on every change; it takes effect at once. It only
// matters for a dash that gets those announcements (flavor 51, or AppStatusNotifyEnabled).
func (ms *MobileSession) SetAppForeground(foreground bool) {
	ms.hud.SetAppForeground(foreground)
}

// SetFlavor51Lifecycle controls what a flavor-51 dash (CarbitRide's family, the Zontes 350D among
// them) gets without the profile asking: the APPSTATUS announcements AppStatusNotifyEnabled
// turns on for any dash, switchEc2Front (0x201D0) when the dash starts the stream, and no reply
// to 0x102B0. On by default; false restores the earlier wire. Call it before StartSession.
func (ms *MobileSession) SetFlavor51Lifecycle(enabled bool) {
	ms.hud.SetFlavor51Lifecycle(enabled)
}

// PhoneIdentity is who the phone says it is to the dash, in the CLIENT_INFO reply. The app keeps
// these values across sessions and passes the same ones every time - the official app does, so a
// dash that remembers phones recognises this one - and the library never writes anything. Any
// field left at its zero value keeps the default every earlier build sent.
type PhoneIdentity struct {
	// Package is the package the reply names, e.g. "net.easyconn.easyride.wws" for a flavor-51
	// dash. Default "com.cfmoto.cfmotointernational".
	Package string
	// PhoneUUID is a stable id for this phone; generate it once and keep it. Default: a new UUID
	// for every MobileSession.
	PhoneUUID string
	// RSAKey is the phone's RSA private key, PEM or DER, PKCS#8 (PrivateKey.getEncoded()) or
	// PKCS#1; 1024 bits like the official app's. Generate it once and keep it. Default: a new
	// key every session.
	RSAKey []byte
	// Brand, Model, SdkVersion and BluetoothName are Build.BRAND, Build.MODEL,
	// Build.VERSION.SDK_INT and the Bluetooth adapter name. Defaults "google", "Pixel 6a", 36,
	// "Pixel 6a".
	Brand         string
	Model         string
	SdkVersion    int
	BluetoothName string
	// EchoSupportFunction answers with the dash's own supportFunction - or its supportFunctionEx
	// & 0xFFBFFFDF - as the official app does, instead of MobileConfig.SupportFunction. It claims
	// every capability the dash offers, so turn it on only for a profile that can back them.
	EchoSupportFunction bool
	// SpeechEngineType adds "speechEngineType" to the reply: 0 leaves it out (default), -1 sends
	// what the official app sends (the dash's supportSpeechEngine, 2 when absent), any positive
	// value is sent as given.
	SpeechEngineType int
}

// SetPhoneIdentity applies id to the CLIENT_INFO reply. Call it before StartSession; it errors
// on a session already running or a key it cannot read.
func (ms *MobileSession) SetPhoneIdentity(id *PhoneIdentity) error {
	if id == nil {
		return nil
	}
	return ms.hud.SetPhoneIdentity(core.PhoneIdentity{
		Package:             id.Package,
		PhoneUUID:           id.PhoneUUID,
		RSAKey:              append([]byte(nil), id.RSAKey...),
		Brand:               id.Brand,
		Model:               id.Model,
		SdkVersion:          id.SdkVersion,
		BluetoothName:       id.BluetoothName,
		EchoSupportFunction: id.EchoSupportFunction,
		SpeechEngineType:    id.SpeechEngineType,
	})
}

// SetIdleProbe makes the phone channel's keepalive carry the official app's cmd-0 probe on its
// first idle tick after a command and every 7th after it; the dash answers 0x80000010. Off by
// default. Call it before StartSession.
func (ms *MobileSession) SetIdleProbe(enabled bool) {
	ms.hud.SetIdleProbe(enabled)
}

// SetTLSCertificate supplies the certificate the phone serves when a dash asks for TLS on PXC
// (0x107C0): the request channel and the phone channels opened after it switch to TLS 1.2/1.3
// with the phone as server, as the official app does. cert is the chain as PEM or one DER
// certificate; key its private key as PEM or DER (PKCS#8, PKCS#1 or EC). For a PKCS12 file,
// open it with java.security.KeyStore("PKCS12") and pass getCertificate(alias).encoded and
// getKey(alias, password).encoded. Without a certificate the dash is answered as before and the
// session stays plaintext (transport event 0x107C0 payload [0] says it asked). Call it before
// StartSession.
func (ms *MobileSession) SetTLSCertificate(cert []byte, key []byte) error {
	parsed, err := net.ParseTLSCertificate(cert, key)
	if err != nil {
		return err
	}
	return ms.hud.SetTLSCertificate(parsed)
}
