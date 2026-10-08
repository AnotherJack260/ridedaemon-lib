package net

import (
	"math"
	"time"
)

// Turn-by-turn on the dash: 0x10040 tells the phone how many arrow icons the dash has, the
// phone answers with 0x20110 frames while navigating and 0x201E0 when guidance starts or stops.
const (
	// PxcNaviConfig is ECP_C2P_ENABLE_DOWNLOAD_PHONE_HUD {maxNaviIcon, supportFunction}
	// (km/t.java:20-54).
	PxcNaviConfig uint32 = 0x10040
	// PxcHudInfo is ECP_P2C_PHONE_HUDINFO, one turn-by-turn frame (mm/c0.java:160-197).
	PxcHudInfo uint32 = 0x20110
	// PxcNaviStatus is ECP_P2C_NAVI_STATUS {"status":bool} (mm/x.java:26-42).
	PxcNaviStatus uint32 = 0x201E0
)

// DefaultMaxNaviIcon is the icon count CarbitRide assumes until the dash sends 0x10040, reset
// on every connection (sdk/b0.java:362). It selects the legacy table.
const DefaultMaxNaviIcon = 19

// NaviConfig is 0x10040. MaxNaviIcon is -1 when the body has none, in which case CarbitRide
// keeps the icon count it had (km/t.java:41-43). The supportFunction bits it reads are stored
// and never read again in 2.4 (km/t.java:33-52, sdk/b0.java:479-493).
type NaviConfig struct {
	MaxNaviIcon     int
	SupportFunction int
}

// DecodeNaviConfig decodes 0x10040. An empty body still enables the HUD (km/t.java:48-52).
func DecodeNaviConfig(body []byte) ([]byte, *NaviConfig, error) {
	obj, err := optObject(body)
	if err != nil {
		return nil, nil, err
	}
	config := &NaviConfig{MaxNaviIcon: -1, SupportFunction: optInt(obj, "supportFunction", 0)}
	if _, has := obj["maxNaviIcon"]; has {
		config.MaxNaviIcon = optInt(obj, "maxNaviIcon", 0) // has() then optInt(): junk reads as 0
	}
	return nil, config, nil
}

// Maneuver descriptors, the vocabulary NaviIcon is keyed by. A maneuver is a kind, a
// direction and, for roundabouts, the way the roundabout turns. They map one to one onto the
// Google Navigation SDK maneuvers CarbitRide's icon tables are written for
// (com/google/android/libraries/mapsplatform/turnbyturn/model/Maneuver.java:9-74).
const (
	ManeuverUnknown = iota
	ManeuverDepart
	ManeuverDestination // direction Left / Right for the side, anything else for ahead
	ManeuverStraight
	ManeuverTurn
	ManeuverMerge
	ManeuverFork
	ManeuverOnRamp
	ManeuverOffRamp
	ManeuverRoundabout // entering; the direction is the exit taken, Unspecified if unknown
	ManeuverRoundaboutExit
	ManeuverFerryBoat
	ManeuverFerryTrain
	ManeuverNameChange
)

const (
	DirectionUnspecified = iota
	DirectionStraight
	DirectionLeft
	DirectionRight
	DirectionSlightLeft
	DirectionSlightRight
	DirectionSharpLeft
	DirectionSharpRight
	DirectionKeepLeft
	DirectionKeepRight
	DirectionUTurnLeft  // counterclockwise
	DirectionUTurnRight // clockwise
)

// googleManeuver maps a descriptor onto the Google Maneuver code. clockwise is the way a
// roundabout turns (left-hand traffic) and only matters for roundabouts; a U-turn's
// "clockwise" is DirectionUTurnRight.
func googleManeuver(kind, direction int, clockwise bool) int {
	side := func(left, right int) int {
		switch direction {
		case DirectionLeft, DirectionSlightLeft, DirectionSharpLeft, DirectionKeepLeft, DirectionUTurnLeft:
			return left
		case DirectionRight, DirectionSlightRight, DirectionSharpRight, DirectionKeepRight, DirectionUTurnRight:
			return right
		}
		return 0
	}
	// The turn and both ramp families list their maneuvers in the same order: unspecified,
	// left, right, keep l/r, slight l/r, sharp l/r, u-turn clockwise, counterclockwise.
	family := func(unspecified int) int {
		switch direction {
		case DirectionLeft:
			return unspecified + 1
		case DirectionRight:
			return unspecified + 2
		case DirectionKeepLeft:
			return unspecified + 3
		case DirectionKeepRight:
			return unspecified + 4
		case DirectionSlightLeft:
			return unspecified + 5
		case DirectionSlightRight:
			return unspecified + 6
		case DirectionSharpLeft:
			return unspecified + 7
		case DirectionSharpRight:
			return unspecified + 8
		case DirectionUTurnRight:
			return unspecified + 9
		case DirectionUTurnLeft:
			return unspecified + 10
		}
		return unspecified
	}
	switch kind {
	case ManeuverDepart:
		return 1
	case ManeuverDestination:
		switch direction {
		case DirectionLeft:
			return 3
		case DirectionRight:
			return 4
		}
		return 2
	case ManeuverStraight:
		return 5
	case ManeuverTurn:
		return family(5) // unspecified reads as STRAIGHT
	case ManeuverMerge:
		if m := side(17, 18); m != 0 {
			return m
		}
		return 16
	case ManeuverFork:
		return side(19, 20)
	case ManeuverOnRamp:
		return family(21)
	case ManeuverOffRamp:
		return family(32)
	case ManeuverRoundabout:
		code := 43 // ROUNDABOUT_CLOCKWISE; every counterclockwise twin is the next code
		switch direction {
		case DirectionStraight:
			code = 45
		case DirectionLeft:
			code = 47
		case DirectionRight:
			code = 49
		case DirectionSlightLeft, DirectionKeepLeft:
			code = 51
		case DirectionSlightRight, DirectionKeepRight:
			code = 53
		case DirectionSharpLeft:
			code = 55
		case DirectionSharpRight:
			code = 57
		case DirectionUTurnLeft, DirectionUTurnRight:
			code = 59
		}
		return code + boolInt(!clockwise)
	case ManeuverRoundaboutExit:
		return 61 + boolInt(!clockwise)
	case ManeuverFerryBoat:
		return 63
	case ManeuverFerryTrain:
		return 64
	case ManeuverNameChange:
		return 65
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The icon tables, GoogleMapNaviPresentation.getNaviIconType() (GoogleMapNaviPresentation.java:
// 80-475) indexed by Google Maneuver code 0..65. The legacy table is what every dash gets until
// it states maxNaviIcon >= 80; the three extended ones are picked by getMaxNaviInt() (:64-72).
var (
	naviIconsLegacy = [66]int{
		9, 9, 15, 15, 15, 9, 2, 3, 4, 5, // 0-9
		4, 5, 6, 7, 0, 8, 0, 4, 5, 4, // 10-19
		5, 9, 2, 3, 4, 5, 4, 5, 6, 7, // 20-29
		0, 9, 9, 4, 5, 4, 5, 4, 5, 6, // 30-39  (31: jadx shows "case 31: break" -> 9)
		7, 0, 8, 0, 11, 0, 0, 0, 0, 0, // 40-49
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // 50-59
		0, 0, 12, 0, 0, 9, // 60-65
	}
	naviIcons80 = [66]int{
		61, 61, 15, 15, 15, 9, 2, 3, 51, 52, // 0-9
		4, 5, 6, 7, 60, 8, 0, 51, 52, 51, // 10-19
		52, 9, 2, 3, 51, 52, 4, 5, 6, 7, // 20-29
		60, 8, 9, 51, 52, 51, 52, 4, 5, 6, // 30-39
		7, 60, 8, 58, 11, 64, 56, 62, 54, 63, // 40-49
		55, 0, 79, 0, 80, 0, 77, 0, 78, 65, // 50-59
		57, 0, 12, 28, 0, 9, // 60-65
	}
	naviIcons152 = [66]int{
		61, 61, 15, 15, 15, 9, 2, 3, 51, 52, // 0-9
		4, 5, 6, 7, 60, 8, 0, 114, 120, 51, // 10-19
		52, 9, 2, 3, 51, 52, 4, 5, 6, 7, // 20-29
		60, 8, 9, 114, 120, 51, 52, 4, 5, 6, // 30-39
		7, 60, 8, 58, 0, 64, 56, 62, 54, 63, // 40-49
		55, 0, 79, 0, 80, 0, 77, 0, 78, 65, // 50-59
		57, 0, 0, 28, 0, 9, // 60-65
	}
	naviIcons161 = [66]int{
		61, 61, 15, 15, 15, 9, 2, 3, 51, 52, // 0-9
		4, 5, 6, 7, 60, 8, 157, 114, 120, 51, // 10-19
		52, 9, 2, 3, 51, 52, 4, 5, 6, 7, // 20-29
		60, 8, 9, 114, 120, 51, 52, 4, 5, 6, // 30-39
		7, 60, 8, 58, 158, 64, 56, 62, 54, 63, // 40-49
		55, 156, 79, 153, 80, 154, 77, 155, 78, 65, // 50-59
		57, 159, 160, 28, 161, 9, // 60-65
	}
)

// NaviIcon is the naviIcon CarbitRide sends for a maneuver to a dash that stated maxNaviIcon
// (DefaultMaxNaviIcon when it never did): getNaviIconType(maneuver, getMaxNaviInt(max)), as
// sendHudByPxc() calls it (GoogleMapNaviPresentation.java:491). That path never clamps an id
// above the dash's maximum; the table choice already keeps it in range.
func NaviIcon(kind, direction int, clockwise bool, maxNaviIcon int) int {
	maneuver := googleManeuver(kind, direction, clockwise)
	switch {
	case maxNaviIcon >= 161:
		return naviIcons161[maneuver]
	case maxNaviIcon >= 152:
		return naviIcons152[maneuver]
	case maxNaviIcon >= 80:
		return naviIcons80[maneuver]
	}
	return naviIconsLegacy[maneuver]
}

// RoundaboutDirection turns a roundabout exit angle into the direction NaviIcon wants, the way
// CarbitRide's Mapbox path does (MapboxDataHelper.roundaboutModifier, MapboxDataHelper.java:
// 309-333): rounded to 45 degrees, 180 straight through, 90 right, 270 left. Unspecified for
// an angle outside 0..360. Whether Android Auto's roundabout exit angle is measured the same
// way is unverified.
func RoundaboutDirection(degrees float64) int {
	switch math.Floor(degrees/45+0.5) * 45 { // Math.round(float)
	case 0, 45:
		return DirectionSharpRight
	case 90:
		return DirectionRight
	case 135:
		return DirectionSlightRight
	case 180:
		return DirectionStraight
	case 225:
		return DirectionSlightLeft
	case 270:
		return DirectionLeft
	case 315, 360:
		return DirectionSharpLeft
	}
	return DirectionUnspecified
}

// FloorNaviDistance is the distance to the next maneuver as CarbitRide's Mapbox path sends it:
// 0 when there is none, 50 below 50 m, otherwise floored to 50 m
// (MapboxDataHelper.getDistanceRemainingInt, MapboxDataHelper.java:202-211).
func FloorNaviDistance(meters int) int {
	switch {
	case meters <= 0:
		return 0
	case meters < 50:
		return 50
	}
	return meters / 50 * 50
}

// HudInfo is one 0x20110 frame's content. Status is 0 while navigating and 1 for the end
// frame. RoadRemainingDistance is sent as given; FloorNaviDistance it first.
type HudInfo struct {
	Status                       int
	NaviIcon                     int
	NextRoad                     string
	RoadRemainingDistance        int
	DestinationRemainingDistance int
	DestinationRemainingTime     int // seconds
}

// Empty is mm/c0.a.o() inverted: CarbitRide does not send a navigating frame with nothing in
// it (mm/c0.java:118-120, GoogleMapNaviPresentation.java:505-507). The end frame skips the check.
func (h HudInfo) Empty() bool {
	return h.NaviIcon == 0 && h.NextRoad == "" && h.RoadRemainingDistance == 0 &&
		h.DestinationRemainingDistance == 0 && h.DestinationRemainingTime == 0
}

// hudInfoJSON is mm/c0.preRequest()'s object, in its put order (mm/c0.java:167-190).
// currentRoad and naviDestination are never filled in 2.4 and Android drops a null put, so
// they never appear; nextRoad appears only when there is a road. carDirection, the camera
// fields and roadRemainingTime have no setter and stay 0; signalIntensity is always 2
// (mm/c0.java:110-112, f73941m never set).
type hudInfoJSON struct {
	Status                       int    `json:"status"`
	CarDirection                 int    `json:"carDirection"`
	CameraType                   int    `json:"cameraType"`
	CameraSpeed                  int    `json:"cameraSpeed"`
	CameraDistance               int    `json:"cameraDistance"`
	NaviIcon                     int    `json:"naviIcon"`
	NextRoad                     string `json:"nextRoad,omitempty"`
	RoadRemainingDistance        int    `json:"roadRemainingDistance"`
	RoadRemainingTime            int    `json:"roadRemainingTime"`
	DestinationRemainingDistance int    `json:"destinationRemainingDistance"`
	DestinationRemainingTime     int    `json:"destinationRemainingTime"`
	ArriveTimeZone               string `json:"arriveTimeZone"`
	ArriveTime                   int64  `json:"arriveTime"`
	SignalIntensity              int    `json:"signalIntensity"`
}

// BuildHudInfoBody builds a 0x20110 body. now is the phone's clock; zoneID its IANA zone
// (TimeZone.getDefault().getID()), which falls back as the QUERY_TIME reply does. arriveTime
// is now plus the remaining time when there is any (mm/c0.java:181-188).
//
// ponytail: CarbitRide's Google path cuts a road over 94 bytes to 31 chars and then sets the
// full name again on the next line (GoogleMapNaviPresentation.java:492-500), so it is sent whole.
func BuildHudInfoBody(h HudInfo, now time.Time, zoneID string) []byte {
	arrive := now.UnixMilli()
	if h.DestinationRemainingTime > 0 {
		arrive += int64(h.DestinationRemainingTime) * 1000
	}
	return carbitJSON(hudInfoJSON{
		Status:                       h.Status,
		NaviIcon:                     h.NaviIcon,
		NextRoad:                     h.NextRoad,
		RoadRemainingDistance:        h.RoadRemainingDistance,
		DestinationRemainingDistance: h.DestinationRemainingDistance,
		DestinationRemainingTime:     h.DestinationRemainingTime,
		ArriveTimeZone:               queryTimeZoneID(now, zoneID),
		ArriveTime:                   arrive,
		SignalIntensity:              2,
	})
}

// BuildNaviStatusBody builds a 0x201E0 body (mm/x.java:32-39).
func BuildNaviStatusBody(navigating bool) []byte {
	return carbitJSON(struct {
		Status bool `json:"status"`
	}{navigating})
}
