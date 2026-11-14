//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// The existing SSH transport owns this watcher. EOF or termination removes its
// one lease; a crash leaves a record that expires to unknown, never a live claim.
func watchRemoteTask() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	updates := make(chan remoteTask)
	failure := make(chan error, 1)
	go func() {
		s := bufio.NewScanner(os.Stdin)
		s.Buffer(make([]byte, 1024), maxRemoteTaskSize)
		for s.Scan() {
			var t remoteTask
			if e := json.Unmarshal(s.Bytes(), &t); e != nil {
				failure <- e
				return
			}
			select {
			case updates <- t:
			case <-ctx.Done():
				return
			}
		}
		failure <- s.Err()
	}()
	var first *remoteTask
	defer func() {
		if first != nil {
			_ = removeRemoteTask(first.Key)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-failure:
			return e
		case t := <-updates:
			if e := validateRemoteTask(t); e != nil {
				return e
			}
			if t.State != "active" {
				return fmt.Errorf("watch requires an active resource")
			}
			if first != nil && t != *first {
				return fmt.Errorf("resource identity changed during watch")
			}
			if e := storeRemoteTask(t); e != nil {
				return e
			}
			if first == nil {
				copy := t
				first = &copy
			}
		}
	}
}
