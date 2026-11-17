package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonHostedTaskRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("e", 32)
	shell := &remoteShell{tasks: tasks, instance: id, name: "hosted", done: make(chan struct{})}
	if err := tasks.register(id, shell); err != nil {
		t.Fatal(err)
	}
	records, err := hostedTaskRecords()
	if err != nil || len(records) != 1 || records[0].ID != id {
		t.Fatalf("hosted inventory: %+v %v", records, err)
	}
	for range 2 {
		c, err := dialHostedTask(context.Background(), id, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMessage(c, message{Type: "status", Action: "health"}); err != nil {
			t.Fatal(err)
		}
		m, err := readMessage(c)
		c.Close()
		if err != nil || m.Instance != id || m.Name != "hosted" {
			t.Fatalf("hosted route: %+v %v", m, err)
		}
	}
	close(shell.done)
	if c, err := dialHostedTask(context.Background(), id, time.Second); err == nil {
		c.Close()
		t.Fatal("ended hosted task accepted a stream")
	}
}
