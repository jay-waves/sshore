package ssh

import (
	"context"
	"net"
	"reflect"
	"sync"
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

}

func TestForwardResolvedBindings(t *testing.T) {
	// Inputs match ssh -G output for -L/-D with omitted, empty and explicit binds.
	for _, tc := range []struct {
		name, value, network string
		gateway              bool
		want                 string
	}{
		{"default-loopback", "8080", "tcp", false, "127.0.0.1:8080"},
		{"ipv4-loopback", "8080", "tcp4", false, "127.0.0.1:8080"},
		{"ipv6-loopback", "8080", "tcp6", false, "[::1]:8080"},
		{"gateway", "8080", "tcp", true, ":8080"},
		{"explicit-empty", "[]:8080", "tcp", false, ":8080"},
		{"explicit-wildcard", "[*]:8080", "tcp6", false, ":8080"},
		{"gateway-preserves-bind", "[127.0.0.1]:8080", "tcp", true, "127.0.0.1:8080"},
		{"ipv6-explicit", "[::1]:8080", "tcp6", false, "[::1]:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forwardListenAddress(tc.value, tc.network, tc.gateway)
			if err != nil || got != tc.want {
				t.Fatalf("binding=%q, err=%v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestReverseForwardDialCancelledWithClient(t *testing.T) {
	previous := net.DefaultResolver
	started := make(chan struct{})
	var once sync.Once
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	defer func() { net.DefaultResolver = previous }()
	client := &Client{config: &connectionConfig{network: "tcp"}, done: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		conn, err := client.dialForwardTarget("pending-forward.invalid:8080")
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resolver did not start")
	}
	close(client.done)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled dial succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("forward dial outlived its client")
	}
}
