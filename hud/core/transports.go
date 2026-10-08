package core

import (
	"context"
	"errors"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

// EnableMCULite makes this session answer an MCU-Lite dash (hud/net/transport_mcu_lite.go) on
// the same ports as the car family, and returns it for the host's callbacks and stills. Call it
// before StartStream; a session without it answers MCU-Lite frames exactly as before.
func (hud *CfmotoHUD) EnableMCULite() (*net.MCULite, error) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if hud.running {
		return nil, errors.New("already running")
	}
	if hud.mcuLite == nil {
		hud.mcuLite = net.NewMCULite()
	}
	return hud.mcuLite, nil
}

// MCULite is the session's MCU-Lite server, nil unless EnableMCULite was called.
func (hud *CfmotoHUD) MCULite() *net.MCULite {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	return hud.mcuLite
}

// StartStreamOverLink opens the reverse servers without the EC init on 10930 and runs afterListen
// once they listen; the dash comes to them through a link the host owns (the USB adb tunnel).
// Like StartStream, it returns once the dash has completed the PXC handshake.
func (hud *CfmotoHUD) StartStreamOverLink(ctx context.Context, afterListen func() error) error {
	return hud.startStream(ctx, nil, afterListen)
}

// TransportCmdUSBBanner carries the head unit's adb CNXN banner, once per USB session. Payload:
// the banner text. Logged so a field capture can show where jadbd finds the head unit's UUID.
// ponytail: 0x100 and up for the alternative transports, clear of the TransportCmd block in cfmoto.go.
const TransportCmdUSBBanner = 0x100
