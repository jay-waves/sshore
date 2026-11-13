package main

// Exercise the existing readline input loop in a child of the current test executable.
// No SSH, daemons, upgrade simulation, or additional versioned builds are used.

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestSessionPickerPTYHelper(t *testing.T) {
	if os.Getenv("SSHORE_PICKER_TEST") != "1" {
		return
	}
	before, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	input, err := newApprovalInput()
	if err != nil {
		t.Fatal(err)
	}
	selected, pickErr := chooseSetupSession(input, []string{"pikachu", "eevee", "snorlax"})
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal not restored", err)
	}
	if pickErr != nil {
		if !(strings.Contains(pickErr.Error(), "cancelled") || strings.Contains(pickErr.Error(), "Interrupt")) {
			t.Fatal(pickErr)
		}
		fmt.Println("PICKER-CANCELLED")
	} else {
		fmt.Println("PICKER-SELECTED=" + selected)
	}
	// Input must be available to the next owner after the reader closes.
	fmt.Println("PICKER-HANDOFF")
	var reply string
	if _, err := fmt.Fscanln(os.Stdin, &reply); err != nil || reply != "handoff" {
		t.Fatal(reply, err)
	}
	fmt.Println("PICKER-DONE")
}

func TestSessionPickerPTY(t *testing.T) {
	for _, tc := range []struct{ name, keys, want string }{
		{"choose", "#2\r", "PICKER-SELECTED=eevee"},
		{"filter", "/snor\r#3\r", "PICKER-SELECTED=snorlax"},
		{"cancel", "\x03", "PICKER-CANCELLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSessionPickerPTYHelper$")
			cmd.Env = append(os.Environ(), "SSHORE_PICKER_TEST=1", "TERM=xterm-256color", "NO_COLOR=1", "GORACE=atexit_sleep_ms=0")
			master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 90, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer cmd.Process.Kill()
			chunks := make(chan string, 64)
			go func() {
				defer close(chunks)
				buf := make([]byte, 4096)
				for {
					n, err := master.Read(buf)
					if n > 0 {
						chunks <- string(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
			var output strings.Builder
			keysSent, handoff := false, false
			timeout := time.After(10 * time.Second)
			for {
				select {
				case chunk, ok := <-chunks:
					if !ok {
						t.Fatalf("picker exited early: %q", output.String())
					}
					output.WriteString(chunk)
					if !keysSent && strings.Contains(output.String(), "Session (") {
						if err := pty.Setsize(master, &pty.Winsize{Cols: 70, Rows: 18}); err != nil {
							t.Fatal(err)
						}
						if _, err := master.Write([]byte(tc.keys)); err != nil {
							t.Fatal(err)
						}
						keysSent = true
					}
					if !handoff && strings.Contains(output.String(), "PICKER-HANDOFF") {
						if _, err := master.Write([]byte("handoff\n")); err != nil {
							t.Fatal(err)
						}
						handoff = true
					}
					if strings.Contains(output.String(), "PICKER-DONE") {
						if err := cmd.Wait(); err != nil {
							t.Fatal(err, output.String())
						}
						if !strings.Contains(output.String(), tc.want) {
							t.Fatal(output.String())
						}
						return
					}
				case <-timeout:
					t.Fatalf("picker timed out: %q", output.String())
				}
			}
		})
	}
}
