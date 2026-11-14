package main

// Local subprocess fixture only: no SSH server, account sessions or upgrades.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwardReconnectAndCancel(t *testing.T) {
	root := t.TempDir()
	attempts := filepath.Join(root, "attempts")
	stub := filepath.Join(root, "ssh")
	script := "#!/bin/sh\ncount=0\n[ ! -f " + shellQuote(attempts) + " ] || count=$(cat " + shellQuote(attempts) + ")\ncount=$((count+1))\nprintf '%s' \"$count\" > " + shellQuote(attempts) + "\nprintf '%s\\n' \"$@\" >> " + shellQuote(filepath.Join(root, "args")) + "\nprintf 'sshore-forward-ready\\n'\nif [ \"$count\" = 1 ]; then sleep 0.1; exit 17; fi\nexec cat >/dev/null\n"
	if e := os.WriteFile(stub, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("SSHORE_SSH", stub)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	dir, e := resourceStateDir("forward")
	if e != nil {
		t.Fatal(e)
	}
	ack := make(chan error, 1)
	ack <- nil
	s := &resourceService{record: resourceRecord{Kind: "forward", Name: "web", Host: "work", Version: version}, recordPath: filepath.Join(dir, "web.json"), ack: ack, stop: make(chan resourceStopRequest), abandoned: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, e := runForwardWorker(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, Retries: 1}, s)
		done <- e
	}()
	stopped := false
	defer func() {
		if !stopped {
			select {
			case s.stop <- resourceStopRequest{reply: make(chan error, 1)}:
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(attempts)
		if string(b) == "2" && s.statusSnapshot().Transport == "connected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("forward did not reconnect", s.statusSnapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	reply := make(chan error, 1)
	s.stop <- resourceStopRequest{reply: reply}
	select {
	case e := <-reply:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stalled")
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	stopped = true
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	if !strings.Contains(string(args), "BatchMode=yes\n-o\nStrictHostKeyChecking=yes") {
		t.Fatal(string(args))
	}
}

func TestForwardRetryBoundaries(t *testing.T) {
	for _, scenario := range []string{"initial-failure", "exhausted", "cancel-backoff"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			attempts := filepath.Join(root, "attempts")
			stub := filepath.Join(root, "ssh")
			body := "#!/bin/sh\nprintf x >> " + shellQuote(attempts) + "\n"
			if scenario != "initial-failure" {
				body += "printf 'sshore-forward-ready\\n'\nsleep 0.05\n"
			}
			body += "exit 17\n"
			if e := os.WriteFile(stub, []byte(body), 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("SSHORE_SSH", stub)
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
			dir, e := resourceStateDir("forward")
			if e != nil {
				t.Fatal(e)
			}
			ack := make(chan error, 1)
			ack <- nil
			s := &resourceService{record: resourceRecord{Kind: "forward", Name: "web", Host: "work", Version: version}, recordPath: filepath.Join(dir, "web.json"), ack: ack, stop: make(chan resourceStopRequest), abandoned: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				_, e := runForwardWorker(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, Retries: 1}, s)
				done <- e
			}()
			if scenario == "cancel-backoff" {
				deadline := time.Now().Add(time.Second)
				for s.statusSnapshot().Transport != "reconnecting" {
					if time.Now().After(deadline) {
						t.Fatal("missing retry backoff")
					}
					time.Sleep(time.Millisecond)
				}
				reply := make(chan error, 1)
				s.stop <- resourceStopRequest{reply: reply}
				select {
				case <-reply:
				case <-time.After(time.Second):
					t.Fatal("backoff cancellation stalled")
				}
			}
			select {
			case e := <-done:
				if (e == nil) != (scenario == "cancel-backoff") {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retry limit ignored")
			}
			b, _ := os.ReadFile(attempts)
			want := 1
			if scenario == "exhausted" {
				want = 2
			}
			if len(b) != want {
				t.Fatal("unexpected attempts", string(b))
			}
		})
	}
}
