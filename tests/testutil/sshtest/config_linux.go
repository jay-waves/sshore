package sshtest

import (
	"crypto/ed25519"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func startAgent(t testing.TB, dir string, private ed25519.PrivateKey) string {
	t.Helper()
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, c)
			mu.Unlock()
			workers.Go(func() { defer c.Close(); _ = agent.ServeAgent(ring, c) })
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-accepted
		mu.Lock()
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return path
}

func writeConfig(t testing.TB, dir, address, agentPath string, key cryptossh.PublicKey, prelude string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("[127.0.0.1]:"+port+" "+string(cryptossh.MarshalAuthorizedKey(key))), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "identity")
	if err := os.WriteFile(keyPath+".pub", cryptossh.MarshalAuthorizedKey(key), 0600); err != nil {
		t.Fatal(err)
	}
	output := fmt.Sprintf("hostname 127.0.0.1\nuser fixture\nport %s\nidentityagent %s\nidentityfile %s\nidentitiesonly yes\nstricthostkeychecking yes\nuserknownhostsfile %s\n", port, agentPath, keyPath, known)
	shim := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\n[ \"$1\" = -G ] || exit 90\n" + prelude + "\nprintf '%s' " + quote(output) + "\n"
	if err := os.WriteFile(shim, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return shim
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
