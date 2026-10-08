package net

import (
	"encoding/binary"
	"strings"
)

const (
	// PxcNotificationGate is 0x10740 {enabled}: 1 lets the phone push notifications
	// (km/t0.java:20-41).
	PxcNotificationGate uint32 = 0x10740
	// PxcNotification is ECP_P2C_PHONE_NOTIFICATION_MESSAGE: an icon prefix, then JSON
	// (mm/d0.java:98-156).
	PxcNotification uint32 = 0x20420
	// NotificationOpenChannel is the dash channel CarbitRide sends notifications to without
	// waiting for 0x10740 (NotificationService.java:338).
	NotificationOpenChannel = "33205"
	// NotificationDateLayout is EasyDriveProp.DATEFORMATE, "yyyy-MM-dd HH:mm:ss", in the phone's
	// zone (TimeUtils.formatTime, SystemStatusMsgManager.java:259).
	NotificationDateLayout = "2006-01-02 15:04:05"
)

// NotificationGate is 0x10740. CarbitRide keeps it until the connection ends; it starts false
// on every connection (sdk/b0.java:463-465, :366).
type NotificationGate struct {
	Enabled bool
}

// DecodeNotificationGate decodes 0x10740: enabled only when "enabled" reads as the integer 1.
// An empty body changes nothing.
func DecodeNotificationGate(body []byte) ([]byte, *NotificationGate, error) {
	obj, err := optObject(body)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	return nil, &NotificationGate{Enabled: optInt(obj, "enabled", 0) == 1}, nil
}

// NotificationText is the message text as CarbitRide puts it in "context": newlines removed
// (SystemStatusMsgManager.java:62). CarbitRide sends nothing when the text is empty (:55-58).
//
// ponytail: the per-app clean-ups after that (WeChat "[n条]" counters and red packets, QQ title
// counters, "[n 条]" on SMS and WhatsApp, :64-112) are Chinese-locale text munging, not
// protocol; and which apps are forwarded at all (WeChat com.tencent.mm, QQ com.tencent.mobileqq, SMS apps
// com.android.mms / com.android.mms.service / com.samsung.android.messaging /
// com.google.android.apps.messaging / com.hihonor.mms, com.whatsapp; only with tickerText or
// category "call", :33, :235-237, NotificationService.java:334) is the app's call.
func NotificationText(text string) string {
	return strings.ReplaceAll(text, "\n", "")
}

// notificationJSON is mm/d0.preRequest()'s object (mm/d0.java:144-149). appName is null, and
// so absent, when CarbitRide cannot resolve the app's label (SystemStatusMsgManager.java:
// 187-195); title is "" rather than null (:59-61).
type notificationJSON struct {
	AppName  string `json:"appName,omitempty"`
	Title    string `json:"title"`
	Context  string `json:"context"`
	DateTime string `json:"dateTime"`
}

// BuildNotificationBody builds a 0x20420 body: 0x00 when there is no icon, otherwise 0x01, a
// little-endian u16 1, a little-endian u32 PNG length and the PNG (mm/d0.java:98-119,
// ByteUtils.java:31-43), then the JSON with nothing between them (:121-130).
func BuildNotificationBody(iconPNG []byte, appName, title, text, dateTime string) []byte {
	body := []byte{0x00}
	if len(iconPNG) > 0 {
		body = make([]byte, 7, 7+len(iconPNG))
		body[0] = 0x01
		binary.LittleEndian.PutUint16(body[1:], 1)
		binary.LittleEndian.PutUint32(body[3:], uint32(len(iconPNG)))
		body = append(body, iconPNG...)
	}
	return append(body, carbitJSON(notificationJSON{appName, title, text, dateTime})...)
}
