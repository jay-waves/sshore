package ssh

import (
	"context"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/wire"
)

func TestSSHTransportOwnership(t *testing.T) {
	input := []string{"-p", "2222", "-L", "9999:localhost:9", "-o", "RemoteForward=8888 localhost:8", "-D", "7777", "-J", "jump"}
	original := append([]string(nil), input...)
	filtered := WithoutForwarding(input)
	if !reflect.DeepEqual(filtered, []string{"-p", "2222", "-J", "jump"}) || !reflect.DeepEqual(input, original) {
		t.Fatal(filtered, input)
	}

}

func TestForwardRejectsInvalidPorts(t *testing.T) {
	for _, rule := range []Forward{{Port: 0, Target: 80}, {Port: 80, Target: 65536}} {
		client := &Client{}
		if client.startForwarding(rule) == nil {
			t.Fatal("invalid rule accepted", rule)
		}
	}
}

func TestReverseForwardListenerCancelsPendingDial(t *testing.T) {
	previous := net.DefaultResolver
	started := make(chan struct{})
	var once sync.Once
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	defer func() { net.DefaultResolver = previous }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Timeout: 15 * time.Second}
	forward := wire.StartForwardListener(listener, func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", "pending-forward.invalid:8080")
	})
	defer forward.Stop()
	peer, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resolver did not start")
	}
	done := make(chan struct{})
	go func() {
		_ = forward.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward dial outlived its client")
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("cancelled dial left the accepted connection open")
	} else if err, ok := err.(net.Error); ok && err.Timeout() {
		t.Fatal("cancelled dial left the accepted connection open")
	}
}
