package net

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
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
