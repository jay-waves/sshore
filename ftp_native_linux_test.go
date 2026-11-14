//go:build linux

package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestFTPOpenSSHSync(t *testing.T) {
	var executable string
	for _, p := range []string{"/usr/libexec/openssh/sftp-server", "/usr/lib/openssh/sftp-server"} {
		if _, e := os.Stat(p); e == nil {
			executable = p
			break
		}
	}
	if executable == "" {
		t.Skip("OpenSSH sftp-server is not installed")
	}
	cmd := exec.Command(executable)
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	in, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { in.Close(); out.Close(); cmd.Process.Kill(); cmd.Wait() }()
	client, e := newFTPSFTP(out, in)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	backend := &ftpFS{client: client, root: t.TempDir()}
	h, e := backend.open("saved", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if _, e = h.Write([]byte("saved remotely")); e != nil {
		t.Fatal(e)
	}
	if e = h.Sync(); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(backend.root + "/saved")
	if e != nil || string(data) != "saved remotely" {
		t.Fatalf("%q %v", data, e)
	}
}
