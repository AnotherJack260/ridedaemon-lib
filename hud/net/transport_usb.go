package net

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// USB AOA EasyConn. When the head unit switches the phone into accessory mode
// (manufacturer/model "Carbit"/"CarbitDriverOversea", nec/AccessoryManager.java:68-98), CarbitRide
// opens the accessory and hands its file descriptor to libjadbd.so: a small adbd. The head unit is
// the adb host. It opens adb streams to "tcp:<port>" and jadbd connects each to 127.0.0.1:<port>
// on the phone, where the ordinary PXC and media servers listen (nec/AccessoryManager.java:181-189,
// then ec/v.S0() starts the listeners; no EC init on 10930). Everything - PXC, the media control
// channel, the video, or an MCU-Lite session - is the Wi-Fi protocol, tunnelled. The servers see
// their peer as 127.0.0.1 (Protocol.DEFAULT_HOST, which q0.java:116-121 uses for a 5 s timeout).
//
// From libjadbd.so's strings: packets CNXN/OPEN/OKAY/WRTE/CLSE, the banner
// "device::ro.product.name=jadbd", services "tcp:" and "localabstract:", old and new adb versions.
// Unverified: how jadbd reads the head unit's UUID ("AOATransport:setUuid", reported through
// JAdbd.onUuidReceived to ClientVerify.setCarUUID); the host's CNXN banner is handed to OnBanner
// so a field log can show what carries it.

const (
	adbCNXN uint32 = 0x4e584e43
	adbOPEN uint32 = 0x4e45504f
	adbOKAY uint32 = 0x59414b4f
	adbCLSE uint32 = 0x45534c43
	adbWRTE uint32 = 0x45545257

	adbHeaderSize = 24
	// adbVersion skips the payload checksum; older hosts (0x01000000) still get one, since
	// every packet sent here carries it.
	adbVersion = 0x01000001
	// adbMaxData is what this side offers and accepts. adb's own MAX_PAYLOAD is 1 MiB; 256 KiB is
	// plenty for a video frame and bounds what a garbled length can allocate.
	adbMaxData = 256 * 1024
	// adbBanner is jadbd's CNXN reply.
	adbBanner = "device::ro.product.name=jadbd"
	// adbInbound bounds the host's WRTEs waiting for one stream's local socket. adb's flow control
	// keeps one in flight until its OKAY (and the banner offers no delayed ack); more waiting than
	// this means the socket has stopped taking them.
	adbInbound = 16
	// adbWriteTimeout bounds one write to a stream's local socket; one that takes nothing for that
	// long has stopped, and its stream is closed - with whatever was queued behind it, a CLSE too.
	adbWriteTimeout = 10 * time.Second
)

// ADBDevice serves the head unit's adb streams over one accessory link.
type ADBDevice struct {
	link io.ReadWriteCloser
	// Dial connects one stream to the phone-side service. Nil: "tcp:<port>" to 127.0.0.1 and
	// "localabstract:<name>", as jadbd does.
	Dial func(service string) (net.Conn, error)
	// OnBanner receives the head unit's CNXN banner.
	OnBanner func(banner string)
	// writeTimeout overrides adbWriteTimeout in tests.
	writeTimeout time.Duration

	writeMu sync.Mutex
	mu      sync.Mutex
	maxData uint32 // what the host accepts per WRTE
	streams map[uint32]*adbStream
	nextID  uint32
}

type adbStream struct {
	local, remote uint32
	conn          net.Conn
	ready         chan struct{} // the host's OKAY: the next WRTE may go
	inbound       chan []byte   // the host's WRTEs for deliver; nil asks it to close the stream
	done          chan struct{}
}

func NewADBDevice(link io.ReadWriteCloser) *ADBDevice {
	return &ADBDevice{link: link, maxData: 4096, streams: map[uint32]*adbStream{}}
}

// Serve runs until the link fails, then closes the link and every stream.
func (d *ADBDevice) Serve() error {
	defer d.closeAll()
	header := make([]byte, adbHeaderSize)
	for {
		if _, err := io.ReadFull(d.link, header); err != nil {
			return err
		}
		cmd := binary.LittleEndian.Uint32(header[0:])
		arg0 := binary.LittleEndian.Uint32(header[4:])
		arg1 := binary.LittleEndian.Uint32(header[8:])
		size := binary.LittleEndian.Uint32(header[12:])
		if binary.LittleEndian.Uint32(header[20:]) != cmd^0xffffffff {
			return fmt.Errorf("adb: bad packet magic for 0x%x", cmd)
		}
		if size > adbMaxData {
			return fmt.Errorf("adb: packet 0x%x of %d bytes", cmd, size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(d.link, payload); err != nil {
			return err
		}
		switch cmd {
		case adbCNXN:
			d.mu.Lock()
			d.maxData = min(max(arg1, 4096), adbMaxData)
			d.mu.Unlock()
			if d.OnBanner != nil {
				d.OnBanner(strings.TrimRight(string(payload), "\x00"))
			}
			if err := d.send(adbCNXN, min(arg0, adbVersion), adbMaxData, []byte(adbBanner)); err != nil {
				return err
			}
		case adbOPEN:
			if err := d.open(arg0, strings.TrimRight(string(payload), "\x00")); err != nil {
				return err
			}
		case adbOKAY:
			if s := d.stream(arg1); s != nil {
				select {
				case s.ready <- struct{}{}:
				default:
				}
			}
		case adbWRTE:
			s := d.stream(arg1)
			if s == nil {
				if err := d.send(adbCLSE, 0, arg0, nil); err != nil {
					return err
				}
				continue
			}
			// The stream's own writer takes it: a local socket that stops reading stalls only its
			// stream, never the link every other stream shares.
			select {
			case s.inbound <- payload:
			default:
				logging.Printf("adb: stream %d stopped taking data; closing it", s.local)
				d.closeStream(s, true)
			}
		case adbCLSE:
			if s := d.stream(arg1); s != nil {
				select {
				case s.inbound <- nil: // after what the host wrote before it
				default:
					d.closeStream(s, false)
				}
			}
		default:
			logging.Printf("adb: ignoring packet 0x%x", cmd)
		}
	}
}

func (d *ADBDevice) open(remote uint32, service string) error {
	dial := d.Dial
	if dial == nil {
		dial = dialLocalService
	}
	conn, err := dial(service)
	if err != nil {
		logging.Printf("adb: open %q: %v", service, err)
		return d.send(adbCLSE, 0, remote, nil)
	}
	d.mu.Lock()
	d.nextID++
	s := &adbStream{local: d.nextID, remote: remote, conn: conn, ready: make(chan struct{}, 1),
		inbound: make(chan []byte, adbInbound), done: make(chan struct{})}
	d.streams[s.local] = s
	d.mu.Unlock()
	if err := d.send(adbOKAY, s.local, remote, nil); err != nil {
		return err
	}
	go d.pump(s)
	go d.deliver(s)
	return nil
}

// deliver writes the host's WRTEs on one stream to its local socket, in order, answering each with
// OKAY once the socket has taken it - which is what keeps the host from sending the next.
func (d *ADBDevice) deliver(s *adbStream) {
	for {
		select {
		case payload := <-s.inbound:
			if payload == nil {
				d.closeStream(s, false)
				return
			}
			timeout := adbWriteTimeout
			if d.writeTimeout > 0 {
				timeout = d.writeTimeout
			}
			_ = s.conn.SetWriteDeadline(time.Now().Add(timeout))
			if _, err := s.conn.Write(payload); err != nil {
				d.closeStream(s, true)
				return
			}
			if d.send(adbOKAY, s.local, s.remote, nil) != nil {
				return // the link is failing; Serve closes everything
			}
		case <-s.done:
			return
		}
	}
}

// pump copies what the phone-side service writes to the host, one WRTE at a time, each waiting
// for the host's OKAY as adb's flow control requires.
func (d *ADBDevice) pump(s *adbStream) {
	d.mu.Lock()
	buf := make([]byte, d.maxData)
	d.mu.Unlock()
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			if d.send(adbWRTE, s.local, s.remote, buf[:n]) != nil {
				break
			}
			select {
			case <-s.ready:
			case <-s.done:
				return
			}
		}
		if err != nil {
			break
		}
	}
	d.closeStream(s, true)
}

func (d *ADBDevice) stream(local uint32) *adbStream {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streams[local]
}

// closeStream retires a stream once; tell says the host has not closed it and must be told.
func (d *ADBDevice) closeStream(s *adbStream, tell bool) {
	d.mu.Lock()
	_, live := d.streams[s.local]
	delete(d.streams, s.local)
	d.mu.Unlock()
	if !live {
		return
	}
	close(s.done)
	_ = s.conn.Close()
	if tell {
		_ = d.send(adbCLSE, s.local, s.remote, nil)
	}
}

func (d *ADBDevice) closeAll() {
	_ = d.link.Close()
	d.mu.Lock()
	streams := make([]*adbStream, 0, len(d.streams))
	for _, s := range d.streams {
		streams = append(streams, s)
	}
	d.mu.Unlock()
	for _, s := range streams {
		d.closeStream(s, false)
	}
}

// send writes one packet: the checksum is filled in for every host version.
func (d *ADBDevice) send(cmd, arg0, arg1 uint32, payload []byte) error {
	packet := make([]byte, adbHeaderSize, adbHeaderSize+len(payload))
	var sum uint32
	for _, b := range payload {
		sum += uint32(b)
	}
	binary.LittleEndian.PutUint32(packet[0:], cmd)
	binary.LittleEndian.PutUint32(packet[4:], arg0)
	binary.LittleEndian.PutUint32(packet[8:], arg1)
	binary.LittleEndian.PutUint32(packet[12:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(packet[16:], sum)
	binary.LittleEndian.PutUint32(packet[20:], cmd^0xffffffff)
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.link.Write(append(packet, payload...))
	return err
}

// dialLocalService is jadbd's createTcpSocket / createLocalSock.
func dialLocalService(service string) (net.Conn, error) {
	if port, ok := strings.CutPrefix(service, "tcp:"); ok {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	if name, ok := strings.CutPrefix(service, "localabstract:"); ok {
		return net.Dial("unix", "@"+name)
	}
	return nil, errors.New("unsupported adb service")
}
