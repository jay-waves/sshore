//go:build !linux

package main

import (
	"context"
	"github.com/xtaci/smux"
)

func approvedRemoteTransfer(ctx context.Context, st *smux.Stream, m message) {
	_ = writeMessage(st, message{Type: "error", Error: "file serving requires Linux"})
}
