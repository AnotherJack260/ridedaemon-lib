package api

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
// turns on for any dash, and switchEc2Front (0x201D0) when the dash starts the stream. On by
// default; false restores the earlier wire. Call it before StartSession.
func (ms *MobileSession) SetFlavor51Lifecycle(enabled bool) {
	ms.hud.SetFlavor51Lifecycle(enabled)
}
