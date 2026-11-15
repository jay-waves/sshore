//go:build !linux

package main

import "github.com/xtaci/smux"

func approvedRemoteTransfer(st *smux.Stream, m message) {
	_ = writeMessage(st, message{Type: "error", Error: "file serving requires Linux"})
}
