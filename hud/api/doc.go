// Package api is what the Android app binds (gomobile, hudlib.aar).
//
// # OnEvent numbers
//
// MobileCallback.OnEvent(time, source, command, payload). Switch on source first: each source
// numbers its commands on its own, and the spaces overlap (transport 16 is not media 0x10).
//
//	source 2  PXC            command is the PXC frame's, payload its body: the dash's frames, and
//	                         the replies this side sent with a body or a status (0x80000020...)
//	source 3  media control  command is the 10921 command (0x10 CAPTURE_CONFIG, 0x20 touch, ...)
//	source 4  transport      this library's own decisions, numbered below
//
// Transport events (source 4). The library's own count up from 1; the alternative transports'
// start at 0x100; an event that reports a dash request carries that request's PXC command, all of
// them 0x10000 or more. No two share a number.
//
//	cmd      constant / meaning                        payload
//	1        core.TransportCmdVideoFraming             [supportExtendProtocol, plain framing applied]
//	2        core.TransportCmdVideoPulls               [net.VideoPullPhase, u64 BE pulls]
//	3        core.TransportCmdPageSwitchProbe          [step, ok, u32 BE command]
//	4        core.TransportCmdAppStatusNotify          [mode, 1 if written]
//	5        core.TransportCmdKeyframeNeeded           u32 BE access units dropped
//	16       TransportCmdCaptureConfig                 CaptureConfig as JSON
//	0x100    core.TransportCmdUSBBanner                the head unit's adb banner text
//	0x10380  net.PxcDashMainPage: show the home page   empty (act on OnGoHome, see below)
//	0x103F0  net.PxcSwitchToAppPage: open a page       u32 BE type, u32 BE channel
//	0x10490  net.PxcDashUiFront: dash UI in front      empty
//	0x106F0  net.PxcDashDisconnect: dash disconnects   empty
//	0x107C0  net.PxcStartTLS: dash asked for TLS       [1] switched, [0] no certificate set
//
// DashFeatures (NewDashFeatures) is fed the feature commands by the session itself; MCU-Lite reports
// through MCULiteCallback, not OnEvent.
//
// # One request, one action
//
// The dash asking for the phone's home screen (0x10380) reaches the app three ways: the PXC event
// (source 2, 0x10380, the dash's frame), the transport event 0x10380, and
// DashFeatureListener.OnGoHome. Act on OnGoHome only; the two events report the same request for
// field logs and must not trigger the home screen again. An app that makes no DashFeatures acts on
// the transport event instead. For the other dash requests in the table, act on the transport
// event; their PXC event is the same request, for logs.
package api
