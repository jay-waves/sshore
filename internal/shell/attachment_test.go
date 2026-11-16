package shell

import (
	"encoding/json"
	"github.com/jay-waves/sshore/internal/transport"
	"net"
	"testing"
)

func TestDetachHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		_, body, err := transport.ReadFrame(server)
		if err != nil {
			done <- err
			return
		}
		var m detachMessage
		if err = json.Unmarshal(body, &m); err != nil {
			done <- err
			return
		}
		if m.Type != "detach" {
			done <- net.ErrClosed
			return
		}
		if err = writeControl(server, detachMessage{Type: "detached", Name: "work"}); err != nil {
			done <- err
			return
		}
		_, body, err = transport.ReadFrame(server)
		if err == nil {
			err = json.Unmarshal(body, &m)
			if err == nil && m.Type != "bye" {
				err = net.ErrClosed
			}
		}
		done <- err
	}()
	confirmed := ""
	if err := Detach(client, func(name string) { confirmed = name }); err != nil {
		t.Fatal(err)
	}
	if confirmed != "work" {
		t.Fatal("lost confirmed name")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
