package net

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeResponse now builds its frame with pxcFrame; the bytes and its side effects on the
// response are what they were.
func TestPxcFrameIsByteIdentical(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	for _, tc := range []struct {
		res                 PXCResponse
		want                string
		wantSize, wantMagic uint32
	}{
		{PXCResponse{Command: 0x10451, Body: []byte("ab"), Token: 9}, "510401001200000043040100000000006162", 18, 0x10443},
		{PXCResponse{Command: PxcHeartbeatOk}, "01000070100000001100007000000000", 16, 0x70000011},
	} {
		conn := &countingConn{}
		res := tc.res
		if err := control.writeResponse(&res, conn, nil); err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(conn.writes[0]); got != tc.want {
			t.Fatalf("0x%x on the wire = %s, want %s", tc.res.Command, got, tc.want)
		}
		if res.Size != tc.wantSize || res.Magic != tc.wantMagic || res.Token != 0 {
			t.Fatalf("0x%x left as size %d magic 0x%x token %d", res.Command, res.Size, res.Magic, res.Token)
		}
		if !bytes.Equal(pxcFrame(tc.res.Command, tc.res.Body), conn.writes[0]) {
			t.Fatal("pxcFrame differs from what writeResponse wrote")
		}
	}
}

// The header check moved into checkPXCHeader with its wording intact: failures are grouped
// downstream by this text.
func TestCheckPXCHeaderKeepsItsWording(t *testing.T) {
	for _, tc := range []struct {
		h    PXCResponse
		want string
	}{
		{PXCResponse{Command: PxcHeartbeat, Size: 16, Magic: 0x1234}, "bad PXC frame magic 0x1234 for command 0x70000000 size 16"},
		{PXCResponse{Command: PxcHeartbeat, Size: 5, Magic: 5 ^ PxcHeartbeat}, "bad PXC frame size 5 for command 0x70000000"},
		{PXCResponse{Command: PxcHeartbeat, Size: 0, Magic: PxcHeartbeat}, ""},
	} {
		err := checkPXCHeader(&tc.h)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Fatalf("checkPXCHeader = %q, want %q", got, tc.want)
		}
	}
}
