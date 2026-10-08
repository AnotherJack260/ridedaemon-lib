package net

import (
	"net"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// The EasyConn families other than the car one share the PXC port; CarbitRide tells them apart by
// the connection type the dash opens with (sdk/h0.java:104-407) and gives each its own handler
// table on the same reader (sdk/b1.java). Here they are told apart by the command's top bits:
//
//	0x101/0x102/0x103 LHU   no handlers; the channel openings get the default ack
//	0x104/0x105       VM    PXCForVM (sdk/g0.java, sm/*): request channel 0x1050000
//	0x106/0x107       MCU   PXCForMCU (sdk/d0.java, lm/*): request channel 0x1060000
//	0x108/0x109       MCU-Lite, only when the host enabled it (transport_mcu_lite.go)
//
// CarbitRide 2.4 does nothing with any MCU or VM request (none of their callbacks is overridden in
// ec/b.java), so those are answered exactly as it answers them and reach the host as events.

// serveOtherFamily answers one of those requests and reports whether it did. Odd frames, the
// channel openings and everything outside these families are left to the default branch, so the
// car family's bytes on the wire do not change.
func (s *PXCControl) serveOtherFamily(event *PXCResponse, conn net.Conn) bool {
	if event.Command&1 != 0 {
		return false
	}
	var reply *PXCResponse
	switch event.Command >> 16 {
	case 0x108, 0x109:
		if s.mcuLite == nil {
			return false
		}
		reply = s.mcuLite.answer(s, event, conn)
	case 0x105:
		reply = answerVM(event)
	case 0x106:
		reply = answerMCU(event)
	}
	if reply == nil {
		return false
	}
	logging.Printf("PXC 0x%x answered 0x%x bodyBytes=%d replyBytes=%d", event.Command, reply.Command, len(event.Body), len(reply.Body))
	s.emitEvent(*event)
	if err := s.writeResponse(reply, conn, nil); err != nil {
		s.emitError(s.connectionError(conn, PxcWriteErr, err))
	} else if len(reply.Body) > 0 || reply.Command != event.Command+1 {
		// As for QUERY_TIME: the only way a rider's log shows what the dash was told.
		s.emitEvent(*reply)
	}
	return true
}

// answerVM is PXCForVM's table (sdk/g0.java:13-22).
func answerVM(event *PXCResponse) *PXCResponse {
	ack := &PXCResponse{Command: event.Command + 1}
	switch event.Command {
	case 0x1050000: // the VM request channel opening
		return nil
	case 0x1050010: // sm/i: {"version"} -> whether the phone launches its virtual machine
		if !isJSONObjectOrEmpty(event.Body) {
			return handlerError()
		}
		// ponytail: CarbitRide reports the dash's last auto_start_vm (sm/b, a stored preference);
		// RideLink has no virtual machine, so it is always false.
		ack.Body = []byte(`{"useVirtualMachine":false}`)
		return ack
	case 0x105001a, 0x105001e: // sm/e page icons {"page":[..]}, sm/g page {"page":n}
		if !isJSONObjectOrEmpty(event.Body) {
			return handlerError()
		}
		return ack
	case 0x1050012, 0x1050014, 0x1050016, 0x1050018, 0x105001c, 0x1050020: // sm/d b c f h a
		return ack
	}
	return &PXCResponse{Command: PxcStatusUnsupported}
}

// answerMCU is PXCForMCU's table (sdk/d0.java:12-16).
func answerMCU(event *PXCResponse) *PXCResponse {
	switch event.Command {
	case 0x1060000: // the MCU request channel opening
		return nil
	case 0x1060010: // lm/d {"project_type" 0..3, "protocol_ver"}; anything else is an error
		var info struct {
			ProjectType int `json:"project_type"`
		}
		if len(event.Body) > 0 && (decodeLenient(event.Body, &info) != nil || info.ProjectType < 0 || info.ProjectType > 3) {
			return handlerError()
		}
	case 0x1060020, 0x1060030: // lm/b, lm/c: raw MCU data, handed to a callback that drops it
	default:
		return &PXCResponse{Command: PxcStatusUnsupported}
	}
	return &PXCResponse{Command: event.Command + 1}
}

// isJSONObjectOrEmpty is what new JSONObject(body) accepts, an empty body being skipped there.
func isJSONObjectOrEmpty(body []byte) bool {
	return len(body) == 0 || decodeLenient(body, &struct{}{}) == nil
}
