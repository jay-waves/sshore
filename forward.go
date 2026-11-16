package main

// Independent SSH workers own forwarding; no listener belongs to a shell attachment.
import (
	"fmt"
	"net"
	"strconv"
)

func describeForward(port, target int, reverse bool) string {
	listener, service := "your computer", "server"
	if reverse {
		listener, service = service, listener
	}
	return fmt.Sprintf("%s %s -> %s %s", listener, loopback(port), service, loopback(target))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
