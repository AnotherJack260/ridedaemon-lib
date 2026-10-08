package net

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Phone-to-car mirroring state.
//
// ECP_P2C_APPSTATUS_BACKGROUND is the one command the official app pushes at the
// phone's own initiative around a mirroring session, and it is the only statement it
// ever makes to the head unit that a mirror now exists. Everything else on this
// channel is dash-originated: the dash asks, the phone answers cmd+1.
//
// From the EasyConn SDK inside the CarbitRide APK (kh/b.java, cmd 131120 = 0x20030),
// the body is
//
//	{"mode":n,"displayRotation":r,"width":w,"height":h,
//	 "enableAccessibility":b,"enableAOAHid":b}
//
// and the app sends it twice per connection:
//
//   - net.easyconn.carman.vf.s.b0(), the moment PXC comes up, with the mode that
//     describes the phone as it is then - normally 2, "not mirroring";
//   - net.easyconn.carman.common.base.n0.setTrueMirror(), which runs when
//     REQ_RV_DATA_START has been answered 113 and the phone has actually started
//     producing frames, with mode 1.
//
// Mode 1 is therefore the phone saying "the picture you are pulling is live". No
// reference implementation sends this - not open-cflink, not open-cfmoto, not
// open-cfmoto-zanderp - and neither did this daemon, which is why it is here: a head
// unit that runs its whole EasyConn client, opens the video socket, pulls every frame
// at 30 Hz and paints none of it is what a UI that was never told the mirror went live
// looks like from this side.
//
// The command is even, so the dash owes it a cmd+1 (0x20031), and y0's own
// getResponseProcessType puts 0x20030 in the SyncExecute group - the official app
// waits for that ack. That makes the ack a free readout: a log with 0x20031 in it says
// this firmware knows the command, whatever the panel does.
const (
	// PxcAppStatus is ECP_P2C_APPSTATUS_BACKGROUND.
	PxcAppStatus uint32 = 0x20030
	// PxcAppStatusAck is the cmd+1 the dash owes it.
	PxcAppStatusAck uint32 = PxcAppStatus + 1
)

// Mode values from kh.b.a(int) and its two call sites in n0.java.
const (
	// AppStatusMirrorLive is "the phone is truly mirroring" - sent once the dash has
	// answered DATA_START and frames are flowing.
	AppStatusMirrorLive = 1
	// AppStatusBackground is "the phone is connected but not mirroring". It is the only
	// mode that overrides the geometry with the phone's upright dimensions, which is
	// what kh.b.a(2) does.
	AppStatusBackground = 2
	// AppStatusOverlay is the third mode the official app can send, when it mirrors
	// underneath its own overlay. Nothing here produces it; it is named so the constant
	// block matches the source it was read from.
	AppStatusOverlay = 3
)

// PhoneScreen is what the official app reads off Display.getRealSize() and
// getRotation() before it builds the body. The host supplies it because a Go library
// linked into an app has no display of its own.
type PhoneScreen struct {
	Width    int
	Height   int
	Rotation int
}

// appStatusState is the per-session half of this file.
type appStatusState struct {
	mu      sync.Mutex
	enabled bool
	screen  PhoneScreen
	sent    map[int]bool
}

// SetAppStatusNotify enables the ECP_P2C_APPSTATUS_BACKGROUND notifications above.
// Opt-in and off by default, for the same reason the page probe was: it puts an
// unsolicited command on a wire that every dashboard painting a picture today has
// never seen from us. Configure it before StartStream.
func (s *PXCControl) SetAppStatusNotify(enabled bool) {
	s.appStatus.mu.Lock()
	defer s.appStatus.mu.Unlock()
	s.appStatus.enabled = enabled
}

// SetPhoneScreen supplies the display metrics the body carries. Zero dimensions are
// accepted and reported as zero rather than guessed: a wrong size stated confidently
// is worse than an honest one, and the field log has to be able to tell them apart.
func (s *PXCControl) SetPhoneScreen(screen PhoneScreen) {
	s.appStatus.mu.Lock()
	defer s.appStatus.mu.Unlock()
	s.appStatus.screen = screen
}

// buildAppStatusBody reproduces kh.b's constructor followed by kh.b.a(mode).
func buildAppStatusBody(screen PhoneScreen, mode int) ([]byte, error) {
	minSide, maxSide := screen.Width, screen.Height
	if minSide > maxSide {
		minSide, maxSide = maxSide, minSide
	}
	landscape := screen.Rotation == 1 || screen.Rotation == 3

	rotation := screen.Rotation
	width, height := minSide, maxSide
	if landscape {
		width, height = maxSide, minSide
	}
	// kh.b.a(2) overrides both: mode 2 describes the phone upright, whatever it is
	// doing at the time.
	if mode == AppStatusBackground {
		rotation, width, height = 0, minSide, maxSide
	}

	// Key order does not matter to a JSON reader, and the names are the ones the SDK
	// writes. enableAccessibility and enableAOAHid are false and will stay false: both
	// describe input paths the official app owns on the phone and this library does not
	// have - claiming either would be advertising for work nothing here can do.
	return json.Marshal(map[string]any{
		"mode":                mode,
		"displayRotation":     rotation,
		"width":               width,
		"height":              height,
		"enableAccessibility": false,
		"enableAOAHid":        false,
	})
}

// SendAppStatus pushes one ECP_P2C_APPSTATUS_BACKGROUND on the phone channel, and only
// there (see phoneChannel). It is a no-op when the notification is not enabled, when the session is
// stopping, or when the same mode has already gone out on this session - a dash that
// says STREAM_START twice must not be told twice that the mirror went live.
func (s *PXCControl) SendAppStatus(mode int) {
	s.appStatus.mu.Lock()
	enabled, screen := s.appStatus.enabled, s.appStatus.screen
	already := s.appStatus.sent[mode]
	if enabled && !already {
		if s.appStatus.sent == nil {
			s.appStatus.sent = map[int]bool{}
		}
		s.appStatus.sent[mode] = true
	}
	s.appStatus.mu.Unlock()

	if !enabled || already || s.isStopping() {
		return
	}

	body, err := buildAppStatusBody(screen, mode)
	if err != nil {
		s.emitError(&PxcError{PxcWriteErr, fmt.Errorf("encode APPSTATUS_BACKGROUND: %w", err), true})
		s.noteAppStatus(mode, err)
		return
	}

	// Phone channel only, through the queue (see p2c.go). No fallback: CarbitRide's t0.q0
	// returns early when its P2C socket is down, and the request channel is where a Zontes
	// hung up on this command (issue #12).
	_, err = s.queueP2C(PxcAppStatus, body, time.Time{}, func(conn net.Conn, err error) {
		logging.Printf("PXC app status: sent 0x%x mode=%d body=%s err=%v", PxcAppStatus, mode, body, err)
		s.noteAppStatus(mode, err)
		if err != nil && conn != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
		}
	})
	if err != nil {
		logging.Printf("PXC app status: mode=%d not sent: %v", mode, err)
		s.noteAppStatus(mode, err)
	}
}

func (s *PXCControl) noteAppStatus(mode int, err error) {
	if s.OnAppStatus == nil {
		return
	}
	s.OnAppStatus(mode, err)
}
