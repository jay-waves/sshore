package main

import (
	"context"
	"fmt"
	"os"
)

// Keep PID and creation identity together when a process outlives its launcher.
type processIdentity struct {
	PID   int
	Start string
}

func currentSpawnOwner() (processIdentity, error) {
	pid := os.Getppid()
	if pid <= 1 {
		return processIdentity{}, fmt.Errorf("spawn requires a live parent shell")
	}
	start, err := spawnProcessIdentity(pid)
	return processIdentity{PID: pid, Start: start}, err
}

// The task owns this watcher. Stopping it joins any in-flight owner-loss
// callback, so cleanup cannot affect a later attachment or outlive the task.
func superviseSpawnOwner(owner processIdentity, lost func()) func() {
	if owner.PID == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		waitSpawnOwner(ctx, owner)
		if ctx.Err() == nil {
			lost()
		}
	}()
	return func() { cancel(); <-stopped }
}

func waitSpawnOwner(ctx context.Context, owner processIdentity) {
	request := activityRequest{Kind: "owner", Owner: owner}
	if s := accountActivity.Load(); s != nil {
		lease, err := s.registerLease(request)
		if err != nil {
			return
		}
		defer s.remove(lease)
		select {
		case <-ctx.Done():
		case <-lease.expired:
		}
		return
	}
	stop, done, err := remoteActivity(ctx, request)
	if err != nil {
		return
	}
	defer stop()
	<-done
}
