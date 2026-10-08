package net

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	stdnet "net"
	"strings"
	"testing"
	"time"
)

// selfSigned is a throwaway certificate, as PEM, the way a host would supply one.
func selfSigned(t *testing.T) (certPEM, keyPEM, certDER, keyDER []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ridedaemon-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ = x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), certDER, keyDER
}

// startTLSControl serves PXC on a real loopback port, as the phone does on 10922.
func startTLSControl(t *testing.T, withCert bool) *PXCControl {
	t.Helper()
	control := NewPXCControl("127.0.0.1:0", nil, &PhoneConfig{})
	if withCert {
		certPEM, keyPEM, _, _ := selfSigned(t)
		cert, err := ParseTLSCertificate(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		control.SetTLSCertificate(cert)
	}
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = control.Stop(ctx)
	})
	return control
}

func dialDash(t *testing.T, control *PXCControl, connType uint32) stdnet.Conn {
	t.Helper()
	conn, err := stdnet.Dial("tcp", control.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	writeDashFrame(t, conn, connType, nil)
	if got := readPXCResponse(t, conn); got.Command != connType+1 {
		t.Fatalf("connection type 0x%x answered 0x%x", connType, got.Command)
	}
	return conn
}

func dashTLS(t *testing.T, conn stdnet.Conn) *tls.Conn {
	t.Helper()
	secure := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := secure.Handshake(); err != nil {
		t.Fatalf("dash-side handshake: %v", err)
	}
	return secure
}

// The request channel answers 00 00 00 02 in plaintext and then speaks TLS, with the phone as
// server; a phone channel opened after it is acked in plaintext and then speaks TLS too
// (km/u.java:19-29, sdk/b1.java:163-167, sdk/x0.java:175).
func TestStartTLSSwitchesTheRequestAndPhoneChannels(t *testing.T) {
	control := startTLSControl(t, true)
	var transport [][]byte
	control.OnTransport = func(cmd int, payload []byte) {
		if cmd == int(PxcStartTLS) {
			transport = append(transport, payload)
		}
	}

	ctrl := dialDash(t, control, PxcHandshake)
	writeDashFrame(t, ctrl, PxcStartTLS, nil)
	if got := readPXCResponse(t, ctrl); got.Command != PxcStartTLS+1 || string(got.Body) != "\x00\x00\x00\x02" {
		t.Fatalf("0x107C0 answered 0x%x %x", got.Command, got.Body)
	}
	secureCtrl := dashTLS(t, ctrl)
	writeDashFrame(t, secureCtrl, PxcHeartbeat, nil)
	if got := readPXCResponse(t, secureCtrl); got.Command != PxcHeartbeatOk {
		t.Fatalf("heartbeat over TLS answered 0x%x", got.Command)
	}

	secureData := dashTLS(t, dialDash(t, control, PxcChannelCarData))
	waitForPhoneChannel(t, control, nil) // a TLS 1.3 client is done before the server is
	results := sendAsync(control, 0x20110, []byte(`{"status":0}`), 0)
	if got := readPXCResponse(t, secureData); got.Command != 0x20110 || string(got.Body) != `{"status":0}` {
		t.Fatalf("phone command over TLS: 0x%x %s", got.Command, got.Body)
	}
	writeDashFrame(t, secureData, 0x20111, nil)
	if r := awaitResult(t, results); r.err != nil || r.reply.Command != 0x20111 {
		t.Fatalf("phone command result = %+v", r)
	}
	if len(transport) != 1 || string(transport[0]) != "\x01" {
		t.Fatalf("TLS transport events = %v, want one [1]", transport)
	}

	// A new request channel starts plaintext, and so does the next phone channel (h0.java:104-112).
	dialDash(t, control, PxcHandshake)
	old := control.phoneChannel()
	plainData := dialDash(t, control, PxcChannelCarData)
	waitForPhoneChannel(t, control, old)
	results = sendAsync(control, 0x20110, nil, 0)
	if got := readPXCResponse(t, plainData); got.Command != 0x20110 {
		t.Fatalf("plaintext phone command: 0x%x", got.Command)
	}
	writeDashFrame(t, plainData, 0x20111, nil)
	awaitResult(t, results)
}

// pipelinedDash writes its ClientHello in the same segment as 0x107C0, without waiting for the
// answer, and reads the plaintext answer off the socket before TLS sees it.
type pipelinedDash struct {
	stdnet.Conn
	request []byte // written in front of the first write
	reply   []byte // the plaintext 0x107C1, read before anything else
}

func (c *pipelinedDash) Write(b []byte) (int, error) {
	if c.request != nil {
		frame := append(c.request, b...)
		c.request = nil
		_, err := c.Conn.Write(frame)
		return len(b), err
	}
	return c.Conn.Write(b)
}

func (c *pipelinedDash) Read(b []byte) (int, error) {
	if c.reply == nil {
		c.reply = make([]byte, pxcHeaderSize+len(pxcStartTLSOK))
		if _, err := io.ReadFull(c.Conn, c.reply); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(b)
}

// Bytes the dash sent right behind its request were already read into the connection's buffer;
// they must reach the handshake, not be lost or parsed as a PXC frame.
func TestStartTLSKeepsWhatTheDashSentBehindTheRequest(t *testing.T) {
	control := startTLSControl(t, true)
	ctrl := dialDash(t, control, PxcHandshake)
	request := (&PXCResponse{Command: PxcStartTLS}).frameForTest()
	dash := &pipelinedDash{Conn: ctrl, request: request}
	secure := dashTLS(t, dash)
	if got := readPXCResponse(t, bytesReader(dash.reply)); got.Command != PxcStartTLS+1 {
		t.Fatalf("0x107C0 answered 0x%x", got.Command)
	}
	writeDashFrame(t, secure, PxcHeartbeat, nil)
	if got := readPXCResponse(t, secure); got.Command != PxcHeartbeatOk {
		t.Fatalf("heartbeat over TLS answered 0x%x", got.Command)
	}
}

// Without a certificate the dash gets the empty 0x107C1 it always got and the session stays
// plaintext; the host hears that it asked.
func TestStartTLSWithoutACertificateStaysPlaintext(t *testing.T) {
	control := startTLSControl(t, false)
	var transport [][]byte
	control.OnTransport = func(cmd int, payload []byte) { transport = append(transport, payload) }

	ctrl := dialDash(t, control, PxcHandshake)
	writeDashFrame(t, ctrl, PxcStartTLS, nil)
	if got := readPXCResponse(t, ctrl); got.Command != PxcStartTLS+1 || len(got.Body) != 0 {
		t.Fatalf("0x107C0 answered 0x%x %x", got.Command, got.Body)
	}
	writeDashFrame(t, ctrl, PxcHeartbeat, nil)
	if got := readPXCResponse(t, ctrl); got.Command != PxcHeartbeatOk {
		t.Fatalf("plaintext heartbeat answered 0x%x", got.Command)
	}
	if len(transport) != 1 || string(transport[0]) != "\x00" {
		t.Fatalf("transport events = %v, want one [0]", transport)
	}
}

// A dash that asks and then does not speak TLS loses that connection, with one clear error.
func TestAFailedTLSSwitchIsReportedOnce(t *testing.T) {
	control := startTLSControl(t, true)
	ctrl := dialDash(t, control, PxcHandshake)
	writeDashFrame(t, ctrl, PxcStartTLS, nil)
	readPXCResponse(t, ctrl)
	writeDashFrame(t, ctrl, PxcHeartbeat, nil) // plaintext where a ClientHello belongs

	err := readPXCError(t, control)
	if !containsAll(err.Error(), "TLS after 0x107c1") {
		t.Fatalf("error = %v", err)
	}
	select {
	case err := <-control.Errors:
		t.Fatalf("a second error for the same connection: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// ParseTLSCertificate takes the certificate and key as PEM or as DER, the way an Android app
// gets them out of a KeyStore.
func TestParseTLSCertificateTakesPEMOrDER(t *testing.T) {
	certPEM, keyPEM, certDER, keyDER := selfSigned(t)
	for _, tc := range []struct {
		name      string
		cert, key []byte
		ok        bool
	}{
		{"PEM", certPEM, keyPEM, true},
		{"DER", certDER, keyDER, true},
		{"mixed", certDER, keyPEM, true},
		{"key that does not match", certPEM, func() []byte { _, k, _, _ := selfSigned(t); return k }(), false},
		{"garbage", []byte("x"), []byte("y"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseTLSCertificate(tc.cert, tc.key); (err == nil) != tc.ok {
				t.Fatalf("error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func (r *PXCResponse) frameForTest() []byte { return encodeFrame(r) }

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// waitForPhoneChannel waits for a phone channel other than previous.
func waitForPhoneChannel(t *testing.T, control *PXCControl, previous stdnet.Conn) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); control.phoneChannel() == nil || control.phoneChannel() == previous; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the phone channel never came up")
		}
	}
}
