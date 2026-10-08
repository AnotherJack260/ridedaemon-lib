package net

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	stdnet "net"
	"sync/atomic"
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

// A stream whose local socket stops reading stalls only itself: its WRTE waits unacked while
// another stream's data still goes through and is acked, and a CLSE right behind a WRTE still
// lets that WRTE's bytes reach the socket first.
func TestAStalledADBStreamDoesNotStallTheLink(t *testing.T) {
	host, link := stdnet.Pipe()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	services := make(chan stdnet.Conn, 2)
	device := NewADBDevice(link)
	device.Dial = func(string) (stdnet.Conn, error) {
		phoneEnd, serviceEnd := stdnet.Pipe()
		services <- serviceEnd
		return phoneEnd, nil
	}
	go func() { _ = device.Serve() }()
	writeADB(t, host, adbCNXN, 0x01000001, 4096, []byte("host::\x00"))
	readADB(t, host)
	open := func(id uint32) (uint32, stdnet.Conn) {
		writeADB(t, host, adbOPEN, id, 0, []byte("tcp:10920\x00"))
		okay := readADB(t, host)
		if okay.cmd != adbOKAY || okay.arg1 != id {
			t.Fatalf("OPEN answered 0x%x", okay.cmd)
		}
		service := <-services
		t.Cleanup(func() { _ = service.Close() })
		_ = service.SetDeadline(time.Now().Add(3 * time.Second))
		return okay.arg0, service
	}
	stalled, _ := open(1) // its service never reads
	live, service := open(2)

	writeADB(t, host, adbWRTE, 1, stalled, []byte("stuck"))
	writeADB(t, host, adbWRTE, 2, live, []byte("hello"))
	got := make([]byte, 5)
	if _, err := io.ReadFull(service, got); err != nil || string(got) != "hello" {
		t.Fatalf("the live stream got %q, %v", got, err)
	}
	if p := readADB(t, host); p.cmd != adbOKAY || p.arg0 != live || p.arg1 != 2 {
		t.Fatalf("after the live stream's write: 0x%x %d %d", p.cmd, p.arg0, p.arg1)
	}

	writeADB(t, host, adbWRTE, 2, live, []byte("bye"))
	writeADB(t, host, adbCLSE, 2, live, nil)
	if _, err := io.ReadFull(service, got[:3]); err != nil || string(got[:3]) != "bye" {
		t.Fatalf("the write before CLSE got %q, %v", got[:3], err)
	}
	if p := readADB(t, host); p.cmd != adbOKAY || p.arg0 != live {
		t.Fatalf("after the last write: 0x%x %d %d", p.cmd, p.arg0, p.arg1)
	}
	if _, err := service.Read(got); !errors.Is(err, io.EOF) {
		t.Fatalf("the closed stream's socket: %v", err)
	}
}

// A stream whose local socket takes nothing for the write timeout is closed, and so is not kept,
// socket and worker, until the whole link ends; the host is told with a CLSE, even when it had
// already queued its own behind the stalled write.
func TestAStalledADBStreamIsClosed(t *testing.T) {
	host, link := stdnet.Pipe()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	device := NewADBDevice(link)
	device.writeTimeout = 50 * time.Millisecond
	device.Dial = func(string) (stdnet.Conn, error) {
		phoneEnd, serviceEnd := stdnet.Pipe() // the service end never reads
		t.Cleanup(func() { _ = serviceEnd.Close() })
		return phoneEnd, nil
	}
	go func() { _ = device.Serve() }()
	writeADB(t, host, adbCNXN, 0x01000001, 4096, []byte("host::\x00"))
	readADB(t, host)
	writeADB(t, host, adbOPEN, 1, 0, []byte("tcp:10922\x00"))
	local := readADB(t, host).arg0

	writeADB(t, host, adbWRTE, 1, local, []byte("stuck"))
	writeADB(t, host, adbCLSE, 1, local, nil)
	if p := readADB(t, host); p.cmd != adbCLSE || p.arg0 != local || p.arg1 != 1 {
		t.Fatalf("after the stalled write: 0x%x %d %d, want CLSE", p.cmd, p.arg0, p.arg1)
	}
	if device.stream(local) != nil {
		t.Fatal("the stalled stream is still open")
	}
}

// failingLink is an accessory link whose writes start failing while its reads still block.
type failingLink struct {
	stdnet.Conn
	failing atomic.Bool
}

func (l *failingLink) Write(b []byte) (int, error) {
	if l.failing.Load() {
		return 0, errors.New("accessory write failed")
	}
	return l.Conn.Write(b)
}

// A link that fails a write is closed whichever stream's worker was writing - here the OKAY for a
// WRTE - so Serve ends and closes every stream, instead of leaving the stream and its socket
// behind while Serve waits on a read that never comes.
func TestAFailedADBWriteEndsTheLink(t *testing.T) {
	host, end := stdnet.Pipe()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	link := &failingLink{Conn: end}
	services := make(chan stdnet.Conn, 1)
	device := NewADBDevice(link)
	device.Dial = func(string) (stdnet.Conn, error) {
		phoneEnd, serviceEnd := stdnet.Pipe()
		services <- serviceEnd
		return phoneEnd, nil
	}
	served := make(chan error, 1)
	go func() { served <- device.Serve() }()
	writeADB(t, host, adbCNXN, 0x01000001, 4096, []byte("host::\x00"))
	readADB(t, host)
	writeADB(t, host, adbOPEN, 1, 0, []byte("tcp:10922\x00"))
	local := readADB(t, host).arg0
	service := <-services
	defer service.Close()
	_ = service.SetDeadline(time.Now().Add(3 * time.Second))

	link.failing.Store(true)
	writeADB(t, host, adbWRTE, 1, local, []byte("hi"))
	if _, err := io.ReadFull(service, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve kept waiting on a link that can no longer be written")
	}
	if _, err := service.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the stream's socket outlived the link: %v", err)
	}
}
