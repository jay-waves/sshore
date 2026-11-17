package main

import "net"

// Replace the interactive launcher with an in-process request. The existing
// handoff owns confirmation, rollback and the remote spawn lifetime connection.
func serveShortcutSpawn(p *attachment, done <-chan struct{}, serve func(net.Conn)) {
	client, server := net.Pipe()
	defer client.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		defer server.Close()
		serve(server)
	}()
	go func() {
		select {
		case <-done:
			client.Close()
		case <-finished:
		}
	}()
	accepted := false
	for {
		m, err := readMessage(client)
		if err != nil {
			if !accepted {
				_ = p.controlMessage(message{Type: "spawn_child_error", Error: "spawn handoff ended before confirmation"})
			}
			return
		}
		if m.Type == "handoff_accepted" {
			accepted = true
		}
		if m.Error != "" {
			_ = p.controlMessage(message{Type: "spawn_child_error", Error: m.Error})
			return
		}
	}
}
