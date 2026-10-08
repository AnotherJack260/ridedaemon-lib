package api

import "testing"

// The control API refuses what it cannot use instead of failing later on the wire.
func TestControlAPIRefusesWhatItCannotUse(t *testing.T) {
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()

	if err := session.SetPhoneIdentity(&PhoneIdentity{RSAKey: []byte("not a key")}); err == nil {
		t.Error("an unreadable identity key was accepted")
	}
	if err := session.SetPhoneIdentity(&PhoneIdentity{Model: "SM-S918B"}); err != nil {
		t.Errorf("a plain identity was refused: %v", err)
	}
	if err := session.SetTLSCertificate([]byte("cert"), []byte("key")); err == nil {
		t.Error("an unreadable TLS certificate was accepted")
	}
	if err := session.ReleaseDash(); err == nil {
		t.Error("ReleaseDash succeeded with no session running")
	}
}
