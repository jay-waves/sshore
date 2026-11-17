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

func TestForwardRejectsInvalidPorts(t *testing.T) {
	for _, rule := range []Forward{{Port: 0, Target: 80}, {Port: 80, Target: 65536}} {
		client := &Client{config: &connectionConfig{options: Options{Forward: &rule}}}
		if client.StartForwarding() == nil {
			t.Fatal("invalid rule accepted", rule)
		}
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
