package api

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	stdnet "net"
	"testing"
	"time"
)

// pipeStream is the Stream an app would write around a socket's streams.
type pipeStream struct{ conn stdnet.Conn }

func (p pipeStream) Read(max int) ([]byte, error) {
	buf := make([]byte, max)
	n, err := p.conn.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	return nil, err
}
func (p pipeStream) Write(data []byte) error { _, err := p.conn.Write(data); return err }
func (p pipeStream) Close() error            { return p.conn.Close() }

type chunkStream struct{ chunks [][]byte }

func (c *chunkStream) Read(int) ([]byte, error) {
	if len(c.chunks) == 0 {
		return nil, nil
	}
	b := c.chunks[0]
	c.chunks = c.chunks[1:]
	return b, nil
}
func (c *chunkStream) Write([]byte) error { return nil }
func (c *chunkStream) Close() error       { return nil }

// A Stream that hands back more than was asked keeps the rest for the next Read, and an empty
// result is the end of the stream.
func TestStreamAdapter(t *testing.T) {
	rw := &streamRW{s: &chunkStream{chunks: [][]byte{[]byte("hello"), []byte("!")}}}
	got, err := io.ReadAll(io.LimitReader(rw, 6))
	if err != nil || string(got) != "hello!" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := rw.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last chunk: %v", err)
	}
}

type silentCallback struct{}

func (silentCallback) OnError(string, bool)            {}
func (silentCallback) OnEvent(int64, int, int, []byte) {}
func (silentCallback) OnStopped()                      {}

type liteCallback struct{ mirrors chan [3]int }

func (c liteCallback) OnClientInfo(string, string, int, int, int) {}
func (c liteCallback) OnMirrorConfig(width, height, fps, _, _ int, _, _ float64) {
	c.mirrors <- [3]int{width, height, fps}
}
func (c liteCallback) OnGPSDue()     {}
func (c liteCallback) OnWeatherDue() {}

// adbHost is a fake head unit on the far end of the accessory link.
type adbHost struct {
	t       *testing.T
	conn    stdnet.Conn
	packets chan [3]uint32 // cmd, arg0, arg1 of everything but WRTE
	data    map[uint32][]byte
	wrte    chan [2][]byte // host stream id (LE), payload
}

func newADBHost(t *testing.T, conn stdnet.Conn) *adbHost {
	h := &adbHost{t: t, conn: conn, packets: make(chan [3]uint32, 64), data: map[uint32][]byte{}, wrte: make(chan [2][]byte, 64)}
	go func() {
		header := make([]byte, 24)
		for {
			if _, err := io.ReadFull(conn, header); err != nil {
				close(h.packets)
				return
			}
			cmd, arg0, arg1 := binary.LittleEndian.Uint32(header[0:]), binary.LittleEndian.Uint32(header[4:]), binary.LittleEndian.Uint32(header[8:])
			payload := make([]byte, binary.LittleEndian.Uint32(header[12:]))
			if _, err := io.ReadFull(conn, payload); err != nil {
				close(h.packets)
				return
			}
			if cmd == 0x45545257 { // WRTE: acked straight away, as an adb host does
				h.send(0x59414b4f, arg1, arg0, nil)
				h.wrte <- [2][]byte{binary.LittleEndian.AppendUint32(nil, arg1), payload}
				continue
			}
			h.packets <- [3]uint32{cmd, arg0, arg1}
		}
	}()
	return h
}

func (h *adbHost) send(cmd, arg0, arg1 uint32, payload []byte) {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:], cmd)
	binary.LittleEndian.PutUint32(p[4:], arg0)
	binary.LittleEndian.PutUint32(p[8:], arg1)
	binary.LittleEndian.PutUint32(p[12:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(p[20:], cmd^0xffffffff)
	_, _ = h.conn.Write(append(p, payload...))
}

// open opens a stream to service and returns the device's id for it.
func (h *adbHost) open(id uint32, service string) uint32 {
	h.t.Helper()
	h.send(0x4e45504f, id, 0, []byte(service+"\x00"))
	for p := range h.packets {
		if p[0] == 0x59414b4f && p[2] == id {
			return p[1]
		}
	}
	h.t.Fatalf("no OKAY for %s", service)
	return 0
}

// await collects what the device wrote on stream id until it holds n bytes, and takes them.
func (h *adbHost) await(id uint32, n int) []byte {
	h.t.Helper()
	timeout := time.After(5 * time.Second)
	for len(h.data[id]) < n {
		select {
		case w := <-h.wrte:
			sid := binary.LittleEndian.Uint32(w[0])
			h.data[sid] = append(h.data[sid], w[1]...)
		case <-timeout:
			h.t.Fatalf("stream %d: %d of %d bytes", id, len(h.data[id]), n)
		}
	}
	got := h.data[id][:n]
	h.data[id] = h.data[id][n:]
	return got
}

func pxc(cmd uint32, body []byte) []byte {
	size := uint32(16 + len(body))
	f := make([]byte, 16, size)
	binary.LittleEndian.PutUint32(f[0:], cmd)
	binary.LittleEndian.PutUint32(f[4:], size)
	binary.LittleEndian.PutUint32(f[8:], size^cmd)
	return append(f, body...)
}

// An MCU-Lite dash over USB, end to end through a real session: adb on the accessory stream,
// the PXC handshake on the phone's own listener, the session ready on ClientInfo, the mirror
// config reaching the host, and a still pulled through the data port.
func TestMCULiteOverUSB(t *testing.T) {
	for _, port := range []string{":10920", ":10921", ":10922"} {
		ln, err := stdnet.Listen("tcp", port)
		if err != nil {
			t.Skipf("the session ports are busy on this machine: %v", err)
		}
		_ = ln.Close()
	}
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 5, 2, 1, 1), silentCallback{})
	if err != nil {
		t.Fatal(err)
	}
	mirrors := make(chan [3]int, 1)
	if err := session.EnableMCULite(liteCallback{mirrors}); err != nil {
		t.Fatal(err)
	}
	hostEnd, deviceEnd := stdnet.Pipe()
	defer hostEnd.Close()
	started := make(chan error, 1)
	go func() { started <- session.StartSessionOverUSB(pipeStream{deviceEnd}) }()
	defer session.StopSession()

	host := newADBHost(t, hostEnd)
	host.send(0x4e584e43, 0x01000001, 256*1024, []byte("host::\x00"))
	if p := <-host.packets; p[0] != 0x4e584e43 {
		t.Fatalf("CNXN answered 0x%x", p[0])
	}
	ctrl := host.open(1, "tcp:10922")
	host.send(0x45545257, 1, ctrl, pxc(0x1080000, nil))
	if got := host.await(1, 16); binary.LittleEndian.Uint32(got) != 0x1080001 {
		t.Fatalf("C2P opening answered %x", got)
	}
	phone := host.open(2, "tcp:10922")
	host.send(0x45545257, 2, phone, pxc(0x1090000, nil))
	if got := host.await(2, 16); binary.LittleEndian.Uint32(got) != 0x1090001 {
		t.Fatalf("P2C opening answered %x", got)
	}
	clientInfo, _ := hex.DecodeString("0a05322e312e301203312e301a09757569642d3132333420c9a601280730333a0270774080d095ffbc314a064855313233345001")
	host.send(0x45545257, 1, ctrl, pxc(0x1080010, clientInfo))
	header := host.await(1, 16)
	if binary.LittleEndian.Uint32(header) != 0x1080011 {
		t.Fatalf("ClientInfo answered %x", header)
	}
	rly := host.await(1, int(binary.LittleEndian.Uint32(header[4:]))-16)
	if err := <-started; err != nil {
		t.Fatalf("StartSessionOverUSB: %v", err)
	}

	// width 64, height 32, 500 fps, JPEG
	host.send(0x45545257, 1, ctrl, pxc(0x1080020, []byte{0x08, 0x40, 0x10, 0x20, 0x18, 0xf4, 0x03, 0x20, 0x01}))
	if got := host.await(1, 16+9); binary.LittleEndian.Uint32(got) != 0x1080021 {
		t.Fatalf("MirrorConfig answered %x", got)
	}
	select {
	case m := <-mirrors:
		if m != [3]int{64, 32, 500} {
			t.Fatalf("OnMirrorConfig %v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("OnMirrorConfig never came")
	}
	still := []byte{0xFF, 0xD8, 0xFF, 0xE0, 9}
	session.PushMCULiteJPEG(still)
	data := host.open(3, "tcp:10920")
	host.send(0x45545257, 3, data, []byte{1})
	frame := host.await(3, 21+len(still))
	token := rly[bytes.IndexByte(rly, 0x20)+1:] // field 4, a varint
	key, _ := binary.Uvarint(token)
	want := append([]byte(nil), still...)
	for i, k := range binary.LittleEndian.AppendUint32(nil, uint32(key)) {
		want[i] ^= k
	}
	if frame[0] != 0 || binary.LittleEndian.Uint32(frame[1:]) != uint32(21+len(still)) || !bytes.Equal(frame[21:], want) {
		t.Fatalf("data frame = %x", frame)
	}

	if err := session.StopSession(); err != nil {
		t.Fatal(err)
	}
	for range host.packets { // the accessory stream is closed with the session
	}
}
