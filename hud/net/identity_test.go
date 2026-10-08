package net

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// clientInfoReplyFields answers one CLIENT_INFO and returns the reply's fields.
func clientInfoReplyFields(t *testing.T, control *PXCControl, clientInfo string) map[string]json.RawMessage {
	t.Helper()
	got := handlePXCEventAndReadResponses(t, control, &PXCResponse{Command: PxcHudConf, Body: json.RawMessage(clientInfo)}, 1)[0]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &fields); err != nil {
		t.Fatalf("reply %s: %v", got.Body, err)
	}
	return fields
}

// With the echo on, the reply carries the dash's own capability bits the way the official app
// builds them (ECP_C2P_CLIENT_INFO.java:877-890, 1030-1036); with it off, the host's value.
func TestClientInfoSupportFunctionEcho(t *testing.T) {
	for _, tc := range []struct {
		name       string
		echo       bool
		clientInfo string
		wantSF     string // "" means absent
		wantEx     string
	}{
		{"echo off keeps the profile value", false, `{"HUID":"h","supportFunction":4096}`, `7`, ``},
		{"echo of supportFunction", true, `{"HUID":"h","supportFunction":4096}`, `4096`, ``},
		{"echo with none sent is 0", true, `{"HUID":"h"}`, `0`, ``},
		{"Ex is masked and replaces supportFunction", true, `{"HUID":"h","supportFunction":1,"supportFunctionEx":4294967295}`, ``, `4290772959`},
		{"Ex as text", true, `{"HUID":"h","supportFunctionEx":"96"}`, ``, `64`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newHandshakeControl(t)
			profile := int64(7)
			control.PhoneConfig.SupportFunction = &profile
			control.SetEchoSupportFunction(tc.echo)
			fields := clientInfoReplyFields(t, control, tc.clientInfo)
			if got := string(fields["supportFunction"]); got != tc.wantSF {
				t.Fatalf("supportFunction = %q, want %q", got, tc.wantSF)
			}
			if got := string(fields["supportFunctionEx"]); got != tc.wantEx {
				t.Fatalf("supportFunctionEx = %q, want %q", got, tc.wantEx)
			}
		})
	}
}

// speechEngineType is left out unless asked for; asked for the official app's value, it is the
// dash's supportSpeechEngine with 2002 read as 2001 off channel 16206 (CI:922-932).
func TestClientInfoSpeechEngineType(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setting    int
		clientInfo string
		want       string
	}{
		{"not asked for", 0, `{"HUID":"h","supportSpeechEngine":5}`, ``},
		{"official, absent", -1, `{"HUID":"h"}`, `2`},
		{"official, echoed", -1, `{"HUID":"h","supportSpeechEngine":5}`, `5`},
		{"official, 2002 off XF06", -1, `{"HUID":"h","supportSpeechEngine":2002,"channel":"21321"}`, `2001`},
		{"official, 2002 on XF06", -1, `{"HUID":"h","supportSpeechEngine":2002,"channel":"16206"}`, `2002`},
		{"host value", 3, `{"HUID":"h","supportSpeechEngine":5}`, `3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newHandshakeControl(t)
			control.SetSpeechEngineType(tc.setting)
			if got := string(clientInfoReplyFields(t, control, tc.clientInfo)["speechEngineType"]); got != tc.want {
				t.Fatalf("speechEngineType = %q, want %q", got, tc.want)
			}
		})
	}
}

// The identity key arrives the way an Android app holds one, and a supplied key gives the same
// pubkey every session - the point of persisting it.
func TestParseKeyPair(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	pkcs1 := x509.MarshalPKCS1PrivateKey(rsaKey)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	for _, tc := range []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"PKCS#8 DER (PrivateKey.getEncoded)", pkcs8, true},
		{"PKCS#1 DER", pkcs1, true},
		{"PKCS#8 PEM", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), true},
		{"PKCS#1 PEM after a certificate", append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}),
			pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1})...), true},
		{"EC key", ecDER, false},
		{"garbage", []byte("not a key"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kp, err := ParseKeyPair(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseKeyPair() error = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && !kp.PublicKey.Equal(&rsaKey.PublicKey) {
				t.Fatal("parsed a different key")
			}
		})
	}

	kp, _ := ParseKeyPair(pkcs8)
	want, _ := kp.GetPublicEncoded()
	for session := 0; session < 2; session++ {
		control := NewPXCControl(":0", kp, &PhoneConfig{})
		var reply struct{ Pubkey string }
		raw := clientInfoReplyFields(t, control, `{"HUID":"h"}`)["pubkey"]
		if json.Unmarshal(raw, &reply.Pubkey) != nil || reply.Pubkey != want {
			t.Fatalf("session %d pubkey = %s, want the supplied key's", session, raw)
		}
	}
}
