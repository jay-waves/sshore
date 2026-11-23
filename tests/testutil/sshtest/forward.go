package sshtest

import (
	"context"
	"fmt"
	"io"
	"net"

	cryptossh "golang.org/x/crypto/ssh"
)

// serveForward relays one explicitly enabled direct-tcpip channel.
func serveForward(ctx context.Context, channel cryptossh.NewChannel, track func(net.Conn) bool) {
	var request struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if cryptossh.Unmarshal(channel.ExtraData(), &request) != nil {
		channel.Reject(cryptossh.ConnectionFailed, "invalid socket request")
		return
	}
	var dialer net.Dialer
	remote, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(request.Host, fmt.Sprint(request.Port)))
	if err != nil {
		channel.Reject(cryptossh.ConnectionFailed, err.Error())
		return
	}
	if !track(remote) {
		return
	}
	defer remote.Close()
	stream, requests, err := channel.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	go cryptossh.DiscardRequests(requests)
	done := make(chan struct{})
	go func() { io.Copy(remote, stream); remote.Close(); close(done) }()
	io.Copy(stream, remote)
	stream.Close()
	<-done
}
