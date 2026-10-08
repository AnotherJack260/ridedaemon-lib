package net

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	stdnet "net"
	"testing"
	"time"
)

type adbPacket struct {
	cmd, arg0, arg1 uint32
	payload         []byte
}

func writeADB(t *testing.T, w io.Writer, cmd, arg0, arg1 uint32, payload []byte) {
	t.Helper()
	p := make([]byte, adbHeaderSize)
	binary.LittleEndian.PutUint32(p[0:], cmd)
	binary.LittleEndian.PutUint32(p[4:], arg0)
	binary.LittleEndian.PutUint32(p[8:], arg1)
	binary.LittleEndian.PutUint32(p[12:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(p[20:], cmd^0xffffffff)
	if _, err := w.Write(append(p, payload...)); err != nil {
		t.Fatal(err)
	}
}

func readADB(t *testing.T, r io.Reader) adbPacket {
	t.Helper()
	h := make([]byte, adbHeaderSize)
	if _, err := io.ReadFull(r, h); err != nil {
		t.Fatalf("adb header: %v", err)
	}
	p := adbPacket{binary.LittleEndian.Uint32(h[0:]), binary.LittleEndian.Uint32(h[4:]), binary.LittleEndian.Uint32(h[8:]),
		make([]byte, binary.LittleEndian.Uint32(h[12:]))}
	if _, err := io.ReadFull(r, p.payload); err != nil {
		t.Fatalf("adb payload: %v", err)
	}
	var sum uint32
	for _, b := range p.payload {
		sum += uint32(b)
	}
	if binary.LittleEndian.Uint32(h[16:]) != sum || binary.LittleEndian.Uint32(h[20:]) != p.cmd^0xffffffff {
		t.Fatalf("adb packet 0x%x: bad checksum or magic", p.cmd)
	}
	return p
}

// A head unit reaches the PXC server through the adb tunnel: CNXN with jadbd's banner, OPEN
// tcp:10922 to the phone's own listener, the handshake both ways, then CLSE.
func TestPXCOverTheADBTunnel(t *testing.T) {
	control := NewPXCControl("127.0.0.1:0", nil, nil)
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = control.Stop(ctx)
	})
	host, link := stdnet.Pipe()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	device := NewADBDevice(link)
	device.Dial = func(service string) (stdnet.Conn, error) {
		if service != "tcp:10922" {
			return nil, errors.New("nothing there")
		}
		return stdnet.Dial("tcp", control.listener.Addr().String())
	}
	banner := make(chan string, 1)
	device.OnBanner = func(b string) { banner <- b }
	served := make(chan error, 1)
	go func() { served <- device.Serve() }()

	writeADB(t, host, adbCNXN, 0x01000001, 4096, []byte("host::features=shell_v2\x00"))
	h := make([]byte, adbHeaderSize+len(adbBanner))
	if _, err := io.ReadFull(host, h); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h); got != "434e584e01000001000004001d000000f50a0000bcb1a7b16465766963653a3a726f2e70726f647563742e6e616d653d6a61646264" {
		t.Fatalf("CNXN reply = %s", got)
	}
	if b := <-banner; b != "host::features=shell_v2" {
		t.Fatalf("banner = %q", b)
	}

	writeADB(t, host, adbOPEN, 7, 0, []byte("tcp:1\x00"))
	if p := readADB(t, host); p.cmd != adbCLSE || p.arg0 != 0 || p.arg1 != 7 {
		t.Fatalf("OPEN of nothing answered 0x%x %d %d", p.cmd, p.arg0, p.arg1)
	}
	writeADB(t, host, adbOPEN, 7, 0, []byte("tcp:10922\x00"))
	okay := readADB(t, host)
	if okay.cmd != adbOKAY || okay.arg1 != 7 || okay.arg0 == 0 {
		t.Fatalf("OPEN answered 0x%x %d %d", okay.cmd, okay.arg0, okay.arg1)
	}
	local := okay.arg0

	writeADB(t, host, adbWRTE, 7, local, pxcFrame(PxcHandshake, nil))
	var reply []byte
	for got := 0; got < 2; got++ { // our write's OKAY and the server's answer, in either order
		p := readADB(t, host)
		switch {
		case p.cmd == adbOKAY && p.arg0 == local && p.arg1 == 7:
		case p.cmd == adbWRTE && p.arg0 == local && p.arg1 == 7:
			reply = p.payload
		default:
			t.Fatalf("unexpected adb packet 0x%x %d %d", p.cmd, p.arg0, p.arg1)
		}
	}
	if hex.EncodeToString(reply) != "01000100100000001100010000000000" {
		t.Fatalf("PXC answer through adb = %x", reply)
	}
	writeADB(t, host, adbOKAY, 7, local, nil)
	writeADB(t, host, adbCLSE, 7, local, nil)
	waitForPXCConnections(t, control, 0)

	_ = host.Close() // unplugged
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return when the link closed")
	}
}
