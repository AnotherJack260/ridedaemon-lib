package net

import (
	"net"
	"sync"
)

type ConnTracker struct {
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool // CloseAll has run: the server is stopping
}

// listenOn is preset when a server was handed one (SetListener), else a new listener on port.
func listenOn(preset net.Listener, port string) (net.Listener, error) {
	if preset != nil {
		return preset, nil
	}
	return net.Listen("tcp", port)
}

func NewConnTracker() *ConnTracker {
	return &ConnTracker{
		connections: make(map[net.Conn]struct{}),
	}
}

// Add tracks conn, or closes it when CloseAll has already run: a dash that connects as the server
// stops reaches its handler after CloseAll, and nothing else would ever end that read.
func (t *ConnTracker) Add(conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = conn.Close()
		return
	}
	t.connections[conn] = struct{}{}
}

func (t *ConnTracker) Remove(conn net.Conn) {
	t.mu.Lock()
	delete(t.connections, conn)
	t.mu.Unlock()
}

// Others counts the connections this tracker is still serving apart from the
// one given. A per-connection failure is only the end of the service when this
// is zero: the bike opens some of these ports more than once (see the note at
// the top of pxc.go) and abandons the channel it has nothing to say on, so the
// first socket to die is routinely not the last one alive.
func (t *ConnTracker) Others(conn net.Conn) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	others := 0
	for c := range t.connections {
		if c != conn {
			others++
		}
	}
	return others
}

func (t *ConnTracker) CloseAll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	for c := range t.connections {
		_ = c.Close()
	}
}
