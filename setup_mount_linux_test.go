package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	transportio "github.com/jay-waves/sshore/internal/transport"
)

func TestHelperAutomaticSetup(t *testing.T) {
	e := newEnv(t)
	binary, err := os.ReadFile(e.bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"mount-agent", "cp-agent"} {
		for _, tc := range []struct {
			name, installed, answer string
			success                 bool
			downloads               int
		}{
			{"missing", "", "", true, 1},
			{"matching", version, "", true, 0},
			{"older", "0.0.1", "update", true, 1},
			{"declined", "0.0.1", "cancel", false, 1},
			{"newer", "999.0.0", "", false, 0},
			{"unordered", "custom", "update", false, 1},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				target := filepath.Join(home, "bin", "sshore")
				if tc.installed == version {
					os.MkdirAll(filepath.Dir(target), 0700)
					if err := os.WriteFile(target, binary, 0700); err != nil {
						t.Fatal(err)
					}
				} else if tc.installed != "" {
					writeBootstrapStub(t, target, tc.installed)
				}
				original, _ := os.ReadFile(target)
				tools := filepath.Join(home, "tools")
				os.Mkdir(tools, 0700)
				if err := os.WriteFile(filepath.Join(tools, "curl"), []byte("#!/bin/sh\necho offline >&2\nexit 22\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "sh", "-c", remoteCommand(options{command: mode, server: target}))
				cmd.Env = append(e.env, "HOME="+home, "PATH="+tools+":/usr/bin:/bin", "TMPDIR="+home)
				var diagnostics bytes.Buffer
				cmd.Stderr = &diagnostics
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				output, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { input.Close(); output.Close(); cmd.Process.Kill(); cmd.Wait() }()
				downloads := 0
				err = negotiateSetupUsing(ctx, output, input, &options{command: mode, host: "fixture"}, setupClientUI{
					Log:      io.Discard,
					Download: func(context.Context) ([]byte, error) { downloads++; return binary, nil },
					Choose: func(m message) (string, error) {
						if tc.answer == "" {
							t.Fatalf("unexpected prompt: %+v", m)
						}
						return tc.answer, nil
					},
				})
				if downloads != tc.downloads {
					t.Fatalf("downloads=%d want=%d", downloads, tc.downloads)
				}
				if !tc.success {
					if err == nil {
						t.Fatal("setup unexpectedly authorized")
					}
					input.Close()
					output.Close()
					_ = cmd.Wait()
					after, _ := os.ReadFile(target)
					if !bytes.Equal(original, after) {
						t.Fatal("failed setup changed installation")
					}
					return
				}
				if err != nil {
					t.Fatalf("setup: %v", err)
				}
				wire := &transportio.Pipe{Reader: output, Writer: input, Closers: []io.Closer{input, output}}
				var data io.ReadWriteCloser = wire
				if mode == "mount-agent" {
					mux, stream, _, err := files.OpenMux(wire, version)
					if err != nil {
						t.Fatal(err)
					}
					defer mux.Close()
					data = stream
				}
				client, err := files.NewSFTP(data, data)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Stat(target); err != nil {
					t.Fatal(err)
				}
				client.Close()
				input.Close()
				output.Close()
				_ = cmd.Wait()
				installed, err := executableVersion(target)
				if err != nil || installed != version {
					t.Fatalf("installed=%s %v", installed, err)
				}
				if strings.Contains(diagnostics.String(), "DATA RACE") {
					t.Fatal(diagnostics.String())
				}
				if staged, _ := filepath.Glob(filepath.Join(home, "sshore-setup.*")); len(staged) != 0 {
					t.Fatalf("staging retained: %v", staged)
				}
				if sockets, _ := filepath.Glob(filepath.Join(e.dir, "*.sock")); len(sockets) != 0 {
					t.Fatalf("mount created a persistent shell: %v", sockets)
				}
			})
		}
	}
}
