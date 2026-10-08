package net

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestReadECResponseHandlesFragmentedTCPPacket(t *testing.T) {
	service := &ECService{}
	packet := service.encodePayload(&ECResponse{
		Code:      initOk,
		Separator: 0x70,
		Body:      []byte("{}\n"),
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		_, _ = server.Write(packet[:7])
		_, _ = server.Write(packet[7:])
	}()

	response, err := readECResponse(client)
	if err != nil {
		t.Fatalf("readECResponse() error = %v", err)
	}
	if response.Code != initOk {
		t.Fatalf("response code = %d, want %d", response.Code, initOk)
	}
}

func TestInitStreamCmdWithConnUsesProvidedConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	service := &ECService{ip: "192.0.2.1", port: "10930", phoneType: "Android", packageName: "test"}
	go func() {
		header := make([]byte, 16)
		if _, err := io.ReadFull(server, header); err != nil {
			return
		}
		bodyLen := int(header[4]) | int(header[5])<<8
		_, _ = io.ReadFull(server, make([]byte, bodyLen-len(header)))
		response := service.encodePayload(&ECResponse{
			Code: initOk, Separator: 0x70, Body: []byte("{}\n"),
		})
		_, _ = server.Write(response)
	}()

	if err := service.InitStreamCmdWithConn(context.Background(), client); err != nil {
		t.Fatalf("InitStreamCmdWithConn() error = %v", err)
	}
}

// The official app reads the init reply by its length (sdk/s.java:852-866); a body without the
// trailing newline used to fail the whole start.
func TestECResponseNeedsNoTrailingNewline(t *testing.T) {
	for _, body := range []string{`{"status":"true"}`, "{\"status\":\"true\"}\n"} {
		packet := (&ECService{}).encodePayload(&ECResponse{Code: initOk, Separator: 0x70, Body: []byte(body)})
		response, err := (&ECService{}).decodePayload(packet)
		if err != nil || string(response.Body) != `{"status":"true"}` {
			t.Fatalf("body %q: %v %q", body, err, response.Body)
		}
	}
}

// A start stopped while the dash sits on the init must not wait out the read deadline: the
// cancel closes the socket and the init returns the cause at once.
func TestInitStreamCmdEndsWhenItsContextIsCancelled(t *testing.T) {
	client, dash := net.Pipe()
	defer dash.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&ECService{}).InitStreamCmdWithConn(ctx, client) }()
	header := make([]byte, 16)
	if _, err := io.ReadFull(dash, header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(dash, make([]byte, int(header[4])|int(header[5])<<8-16)); err != nil {
		t.Fatal(err)
	}
	// the init is out and the phone is waiting for an answer that never comes
	stopped := errors.New("stopped")
	cancel(stopped)
	select {
	case err := <-done:
		if !errors.Is(err, stopped) {
			t.Fatalf("init returned %v, want the cancel cause", err)
		}
	case <-time.After(time.Second):
		t.Fatal("init ignored the cancel")
	}
	if _, err := client.Write([]byte{0}); err == nil {
		t.Fatal("the EC init socket was left open")
	}
}
