package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/xtaci/smux"
)

func TestSessionDisconnectReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{smux.ErrTimeout, "timeout"},
		{context.Canceled, "interrupted"},
		{io.EOF, "connection closed"},
		{io.ErrUnexpectedEOF, "connection closed"},
		{net.ErrClosed, "connection closed"},
		{fmt.Errorf("detach response timed out"), "timeout"},
		{fmt.Errorf("connection reset by peer"), "connection error"},
	} {
		if got := sessionDisconnectReason(fmt.Errorf("connection lost: %w", tc.err)); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
}
