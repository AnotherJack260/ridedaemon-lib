package net

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

type KeyPair struct {
	rsa.PrivateKey
}

func GenKeyPair() (*KeyPair, error) {
	// Generate RSA keypair (1024 bits to match protocol)
	if p, err := rsa.GenerateKey(rand.Reader, 1024); err != nil {
		return nil, err
	} else {
		return &KeyPair{*p}, nil
	}
}

// ParseKeyPair reads the phone's persistent RSA identity key, which the official app keeps from
// one session to the next (RSAUtil.getPublicKey, nec/utils/RSAUtil.java:114-138) so a dash sees
// the same phone every time. PEM or DER, PKCS#8 (what java.security.PrivateKey.getEncoded()
// returns) or PKCS#1. The official app's is 1024 bits; any RSA size is accepted.
func ParseKeyPair(b []byte) (*KeyPair, error) {
	key, err := parsePrivateKey(b)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the phone identity key must be RSA, not %T", key)
	}
	return &KeyPair{*rsaKey}, nil
}

// parsePrivateKey reads a private key the way an Android app holds one: the first PEM block whose
// type names a private key, or DER; PKCS#8, PKCS#1 or SEC1.
func parsePrivateKey(b []byte) (any, error) {
	for rest := b; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			b = block.Bytes
			break
		}
	}
	if key, err := x509.ParsePKCS8PrivateKey(b); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(b); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(b); err == nil {
		return key, nil
	}
	return nil, errors.New("not a PKCS#8, PKCS#1 or SEC1 private key, in PEM or DER")
}

func (kp *KeyPair) GetPrivateEncoded() (string, error) {
	// Encode private key as PKCS#8 DER
	prvDER, err := x509.MarshalPKCS8PrivateKey(kp.PrivateKey)
	if err != nil {
		return "", err
	}
	prvB64 := base64.StdEncoding.EncodeToString(prvDER)
	return prvB64, nil
}

func (kp *KeyPair) GetPublicEncoded() (string, error) {
	// Encode public key as X.509 SubjectPublicKeyInfo (PKIX) DER
	pubDER, err := x509.MarshalPKIXPublicKey(&kp.PublicKey)
	if err != nil {
		return "", err
	}
	pubB64 := base64.StdEncoding.EncodeToString(pubDER)
	return pubB64, nil
}

// LegacyPrivateEncryptPKCS1v15 replicates Java's
// Cipher.getInstance("RSA/ECB/PKCS1Padding").init(ENCRYPT_MODE, privateKey)
//
// It applies PKCS#1 v1.5 *encryption* padding (type 0x02) and then
// does RSA exponentiation with the private exponent d.
//
// ⚠ This is ONLY for legacy compatibility. Do NOT use in new designs.
func LegacyPrivateEncryptPKCS1v15(prv *rsa.PrivateKey, msg []byte) ([]byte, error) {
	k := (prv.N.BitLen() + 7) / 8 // modulus size in bytes

	// PKCS#1 v1.5: len(M) <= k - 11
	if len(msg) > k-11 {
		return nil, errors.New("message too long for RSA PKCS#1 v1.5")
	}

	em := make([]byte, k)
	em[0] = 0x00
	em[1] = 0x01 // block type 1 = "signature" style padding (what legacy Java is doing)

	// PS = 0xFF bytes, length >= 8
	psLen := k - len(msg) - 3
	if psLen < 8 {
		return nil, errors.New("message too long: PS would be < 8 bytes")
	}

	ps := em[2 : 2+psLen]
	for i := range ps {
		ps[i] = 0xFF
	}

	em[2+psLen] = 0x00
	copy(em[3+psLen:], msg)

	// c = m^d mod n
	m := new(big.Int).SetBytes(em)
	c := new(big.Int).Exp(m, prv.D, prv.N)
	out := c.Bytes()

	// left-pad with zeros to full length k
	if len(out) < k {
		padded := make([]byte, k)
		copy(padded[k-len(out):], out)
		out = padded
	}

	return out, nil
}

func LegacyPublicDecryptPKCS1v15(pub *rsa.PublicKey, ciphertext []byte) ([]byte, error) {
	k := (pub.N.BitLen() + 7) / 8 // modulus size in bytes

	// If ciphertext is longer than modulus size, it's invalid
	if len(ciphertext) > k {
		return nil, errors.New("ciphertext too long for modulus")
	}

	// Left-pad ciphertext if it's shorter than k (lost leading 0x00)
	if len(ciphertext) < k {
		tmp := make([]byte, k)
		copy(tmp[k-len(ciphertext):], ciphertext)
		ciphertext = tmp
	}

	// m = c^e mod n
	c := new(big.Int).SetBytes(ciphertext)
	m := new(big.Int).Exp(c, big.NewInt(int64(pub.E)), pub.N)
	em := m.Bytes()

	// Left-pad EM to full k bytes
	if len(em) < k {
		tmp := make([]byte, k)
		copy(tmp[k-len(em):], em)
		em = tmp
	}

	if em[0] != 0x00 {
		return nil, errors.New("invalid PKCS#1 padding: first byte not 0x00")
	}

	bt := em[1]
	if bt != 0x01 && bt != 0x02 {
		return nil, fmt.Errorf("invalid PKCS#1 padding: unexpected block type %02x", bt)
	}

	// Find the 0x00 separator after padding string
	i := 2
	for ; i < len(em); i++ {
		if em[i] == 0x00 {
			break
		}
	}
	if i >= len(em) {
		return nil, errors.New("invalid PKCS#1 padding: no 0x00 separator")
	}

	// For BT=2 (encryption), PS must be at least 8 bytes of non-zero random
	// For BT=1 (signature style), PS is 0xFF bytes (we won't strictly enforce)
	if i < 10 { // 2 bytes header + at least 8 bytes PS
		return nil, errors.New("invalid PKCS#1 padding: PS too short")
	}

	// Message is everything after the 0x00 separator
	return em[i+1:], nil
}

// TLS on PXC (0x107C0).
//
// A dash with enableSockServerAuth asks for it with 0x107C0 on its request channel. CarbitRide
// answers 00 00 00 02 in plaintext, then turns that socket into the server end of a TLS session
// (TLSv1.2/1.3, no client certificate), and does the same to every phone channel opened after it,
// right after the plaintext 0x20001; a new request channel starts plaintext again (km/u.java:
// 19-29, sdk/b1.java:163-167, sdk/x0.java:175, sdk/e1.java:57-75, sdk/h0.java:104-112).
//
// Its certificate is a PKCS12 inside its APK. This library ships none: the host supplies one
// (SetTLSCertificate), and without it the dash gets the empty 0x107C1 every earlier build sent.
// The XOR of video lengths with a token that comes with it (base/q0.java:191-208) is not done:
// the token this side announces in CLIENT_INFO is always 0, which makes that XOR a no-op.

// PxcStartTLS is the dash asking for TLS on PXC (km/u.java).
const PxcStartTLS uint32 = 0x107C0

// pxcStartTLSOK is the reply body that says yes (km/u.java:22-27).
var pxcStartTLSOK = []byte{0, 0, 0, 2}

// tlsHandshakeTimeout bounds the switch; e1 uses the socket's own timeout (sdk/e1.java:20-31).
const tlsHandshakeTimeout = 10 * time.Second

// ParseTLSCertificate reads the certificate the phone serves PXC TLS with: the certificate chain
// as PEM blocks or one DER certificate, and its private key as PEM or DER (PKCS#8, PKCS#1, SEC1).
// A PKCS12 is opened by the host (java.security.KeyStore) and passed as these two.
func ParseTLSCertificate(cert, key []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(asPEM(cert, "CERTIFICATE"), asPEM(key, "PRIVATE KEY"))
}

// asPEM leaves PEM alone and wraps DER; tls.X509KeyPair tries every key encoding on the bytes.
func asPEM(b []byte, blockType string) []byte {
	if block, _ := pem.Decode(b); block != nil {
		return b
	}
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: b})
}

// SessionTLS is the configuration the dash's media control socket is served with, or nil while
// this session is plaintext: CarbitRide upgrades that socket as it accepts it once PXC has
// switched (common/base/t0.java:163-167, h0.s()), and a new request channel turns it off again.
// The video socket stays plaintext, as it does there.
func (s *PXCControl) SessionTLS() *tls.Config {
	if s.tlsOn.Load() {
		return s.tlsConfig
	}
	return nil
}

// SetTLSCertificate is what the phone serves when a dash asks for TLS. Configure it before Start.
func (s *PXCControl) SetTLSCertificate(cert tls.Certificate) {
	s.tlsConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
	}
}

// pxcConn is an accepted PXC socket that can switch to TLS part-way through while staying the
// same key for everything that tracks it.
type pxcConn struct {
	net.Conn // the socket itself: addresses, deadlines, Close
	secure   atomic.Pointer[tls.Conn]
}

func (c *pxcConn) Read(b []byte) (int, error) {
	if t := c.secure.Load(); t != nil {
		return t.Read(b)
	}
	return c.Conn.Read(b)
}

func (c *pxcConn) Write(b []byte) (int, error) {
	if t := c.secure.Load(); t != nil {
		return t.Write(b)
	}
	return c.Conn.Write(b)
}

// prefixedConn reads what the connection's reader had already buffered before the socket.
type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// startTLS makes c the server end of a TLS session over its socket, the handshake reading pending
// (when not nil) before the socket, and handshakes now, within tlsHandshakeTimeout.
func (c *pxcConn) startTLS(config *tls.Config, pending io.Reader) error {
	r := io.Reader(c.Conn)
	if pending != nil {
		r = io.MultiReader(pending, c.Conn)
	}
	secure := tls.Server(&prefixedConn{Conn: c.Conn, r: r}, config)
	_ = c.Conn.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
	defer func() { _ = c.Conn.SetDeadline(time.Time{}) }()
	if err := secure.Handshake(); err != nil {
		return err
	}
	c.secure.Store(secure)
	return nil
}

// answerStartTLS answers 0x107C0; see the note above.
func (s *PXCControl) answerStartTLS(event *PXCResponse, conn net.Conn) {
	s.emitEvent(*event)
	if s.tlsConfig == nil {
		logging.Printf("PXC: the dash asked for TLS (0x%x) and the app supplied no certificate; answering an empty 0x%x and staying plaintext",
			PxcStartTLS, PxcStartTLS+1)
		s.emitTransport(int(PxcStartTLS), []byte{0})
		if err := s.writeResponse(&PXCResponse{Command: PxcStartTLS + 1}, conn, nil); err != nil {
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
		}
		return
	}
	reply := &PXCResponse{Command: PxcStartTLS + 1, Body: pxcStartTLSOK}
	if !s.switchToTLS(conn, reply) {
		return
	}
	s.tlsOn.Store(true)
	s.emitEvent(*reply)
	s.emitTransport(int(PxcStartTLS), []byte{1})
}

// switchToTLS writes reply in plaintext and makes conn the server end of a TLS session, both
// under the connection's write lock so no other frame can slip between them. Bytes the dash sent
// behind its request are already in the connection's reader and reach the handshake first. On
// failure the connection is reported and closed; it reports whether conn is now TLS.
func (s *PXCControl) switchToTLS(conn net.Conn, reply *PXCResponse) bool {
	state := s.connectionState(conn)
	err := func() error {
		c, ok := conn.(*pxcConn)
		if !ok || state.reader == nil {
			return errors.New("this connection cannot switch to TLS")
		}
		state.writeMu.Lock()
		defer state.writeMu.Unlock()
		if _, err := c.Conn.Write(encodeFrame(reply)); err != nil {
			return err
		}
		buffered, _ := state.reader.Peek(state.reader.Buffered())
		if err := c.startTLS(s.tlsConfig, bytes.NewReader(append([]byte(nil), buffered...))); err != nil {
			return err
		}
		state.reader.Reset(c)
		return nil
	}()
	if err == nil {
		logging.Printf("PXC: %v is TLS now", conn.RemoteAddr())
		return true
	}
	s.emitError(s.connectionError(conn, PxcWriteErr, fmt.Errorf("TLS after 0x%x: %w", reply.Command, err)))
	state.superseded.Store(true) // reported here; its read loop ends without a second report
	_ = conn.Close()
	return false
}
