package main

// Exercise the existing readline input loop in a child of the current test executable.
// No SSH, daemons, upgrade simulation, or additional versioned builds are used.

import (
	"context"
	"errors"
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
					if !keysSent && strings.Contains(output.String(), "Shell (") {
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

func TestSSHUnlockPTYHelper(t *testing.T) {
	mode := os.Getenv("SSHORE_UNLOCK_TEST")
	if mode == "" {
		return
	}
	before, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if mode == "cancel" {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	}
	defer cancel()
	unlock := unlockSSHKey
	if mode == "broker" || mode == "cancel" {
		env, stop, err := startSpawnAuth()
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		unlock = brokerSSHPassphrase(env)
	}
	secret, err := unlock(ctx, "fixture-key")
	if mode == "cancel" {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unlock cancellation: %v", err)
		}
	} else if err != nil || string(secret) != "  quiet secret  " {
		clear(secret)
		t.Fatal("secret changed or unlock failed", err)
	}
	clear(secret)
	// Broker cancellation returns as soon as HTTP is cancelled. Its handler must
	// promptly release the terminal before the next owner is allowed to read.
	deadline := time.Now().Add(time.Second)
	for {
		after, err := term.GetState(int(os.Stdin.Fd()))
		if err == nil && reflect.DeepEqual(before, after) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unlock did not restore terminal", err)
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Println("UNLOCK-HANDOFF")
	var reply string
	if _, err := fmt.Fscanln(os.Stdin, &reply); err != nil || reply != "handoff" {
		t.Fatal("input not released", err)
	}
	fmt.Println("UNLOCK-DONE")
}

func TestSSHUnlockPTY(t *testing.T) {
	for _, mode := range []string{"direct", "broker", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSSHUnlockPTYHelper$")
			cmd.Env = append(os.Environ(), "SSHORE_UNLOCK_TEST="+mode, "TERM=xterm", "GORACE=atexit_sleep_ms=0")
			master, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer cmd.Process.Kill()
			var output strings.Builder
			buf := make([]byte, 4096)
			typed, handoff := false, false
			for {
				n, readErr := master.Read(buf)
				output.Write(buf[:n])
				if !typed && strings.Contains(output.String(), "Passphrase for fixture-key:") {
					if mode != "cancel" {
						_, err = master.Write([]byte("  quiet secret  \r"))
						if err != nil {
							t.Fatal(err)
						}
					}
					typed = true
				}
				if !handoff && strings.Contains(output.String(), "UNLOCK-HANDOFF") {
					_, err = master.Write([]byte("handoff\n"))
					if err != nil {
						t.Fatal(err)
					}
					handoff = true
				}
				if readErr != nil {
					break
				}
			}
			if err = cmd.Wait(); err != nil {
				t.Fatalf("unlock helper failed: %v\n%s", err, &output)
			}
			if strings.Contains(output.String(), "quiet") || strings.Contains(output.String(), "secret") {
				t.Fatal("passphrase echoed to terminal")
			}
			if !strings.Contains(output.String(), "UNLOCK-DONE") {
				t.Fatal("unlock did not finish")
			}
		})
	}
}
