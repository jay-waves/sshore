package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSSHTransportOwnership(t *testing.T) {
	input := []string{"-p", "2222", "-L", "9999:localhost:9", "-o", "RemoteForward=8888 localhost:8", "-D", "7777", "-J", "jump"}
	original := append([]string(nil), input...)
	filtered := WithoutForwarding(input)
	if !reflect.DeepEqual(filtered, []string{"-p", "2222", "-J", "jump"}) || !reflect.DeepEqual(input, original) {
		t.Fatal(filtered, input)
	}

	session := SessionArgs("work", filtered, "opaque command")
	joined := strings.Join(session, " ")
	if session[len(session)-1] != "opaque command" || !strings.Contains(joined, "SessionType=default") || !strings.Contains(joined, "ControlMaster=no") || !strings.Contains(joined, "ClearAllForwardings=yes") {
		t.Fatal(session)
	}
}

func TestCommandPipes(t *testing.T) {
	if os.Getenv("SSHORE_TEST_PIPE_CHILD") == "1" {
		_, err := io.Copy(os.Stdout, os.Stdin)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCommandPipes$")
	cmd.Env = append(os.Environ(), "SSHORE_TEST_PIPE_CHILD=1")
	pipes, err := StartPipes(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { pipes.Close(); cancel(); cmd.Wait() }()
	payload := "terminal\x00bytes\n"
	if _, err := io.WriteString(pipes, payload); err != nil {
		t.Fatal(err)
	}
	if err := pipes.Input.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(pipes)
	if err != nil || string(got) != payload {
		t.Fatalf("%q: %v", got, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandPipeFailures(t *testing.T) {
	t.Run("occupied input", func(t *testing.T) {
		cmd := exec.Command("unused")
		cmd.Stdin = strings.NewReader("")
		if _, err := OpenPipes(cmd); err == nil {
			t.Fatal("accepted occupied stdin")
		}
		if cmd.Stdout != nil {
			t.Fatal("created stdout after stdin failed")
		}
	})
	t.Run("occupied output", func(t *testing.T) {
		cmd := exec.Command("unused")
		cmd.Stdout = io.Discard
		if _, err := OpenPipes(cmd); err == nil {
			t.Fatal("accepted occupied stdout")
		}
		_, err := cmd.Stdin.Read(make([]byte, 1))
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("child stdin left open: %v", err)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		cmd := exec.Command(filepath.Join(t.TempDir(), "missing-ssh"))
		if _, err := StartPipes(cmd); err == nil {
			t.Fatal("started missing executable")
		}
		_, inputErr := cmd.Stdin.Read(make([]byte, 1))
		_, outputErr := cmd.Stdout.Write([]byte{1})
		if !errors.Is(inputErr, os.ErrClosed) || !errors.Is(outputErr, os.ErrClosed) {
			t.Fatalf("child pipes left open: %v, %v", inputErr, outputErr)
		}
	})
}
