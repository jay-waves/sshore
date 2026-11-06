package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func forwardHelper(args []string) error {
	m, err := parseForward(args)
	if err != nil {
		return err
	}
	if m.Action == "add" {
		return requestOperations([]operation{{Kind: "forward", Port: m.Port}})
	}
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore session")
	}
	c, err := net.DialTimeout("unix", sock, forwardTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * forwardTimeout))
	if err = writeMessage(c, m); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return fmt.Errorf("forward request interrupted: %w", err)
	}
	if reply.Type != "forward_result" || reply.Error != "" {
		return fileResponseError(reply)
	}
	if m.Action == "list" {
		if len(reply.Ports) == 0 {
			fmt.Println("No dynamic forwards")
		}
		for _, port := range reply.Ports {
			fmt.Printf("%s -> remote %s\n", loopback(port), loopback(port))
		}
	} else {
		logf(os.Stderr, logSuccess, "[sshore] %s %s -> remote %s\n", cleanText(reply.Name), loopback(m.Port), loopback(m.Port))
	}
	return nil
}

func (s *session) forward(c net.Conn, m message) error {
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no client attached")
	}
	select {
	case p.forwardRequests <- struct{}{}:
		defer func() { <-p.forwardRequests }()
	default:
		return fmt.Errorf("too many pending forward requests")
	}
	st, err := p.mux.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(forwardTimeout))
	if err = writeMessage(st, m); err != nil {
		return err
	}
	result, err := readMessage(st)
	if err != nil {
		return err
	}
	if result.Type != "forward_result" {
		return fmt.Errorf("unexpected forward response")
	}
	_ = c.SetWriteDeadline(time.Now().Add(forwardTimeout))
	return writeMessage(c, result)
}
