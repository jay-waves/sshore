package main

import (
	"fmt"
	"os"
	"time"
)

// Creation identity prevents a reused parent PID from extending a spawn's life.
type spawnOwner struct {
	PID   int
	Start string
}

func currentSpawnOwner() (spawnOwner, error) {
	pid := os.Getppid()
	if pid <= 1 {
		return spawnOwner{}, fmt.Errorf("spawn requires a live parent shell")
	}
	start, err := spawnProcessIdentity(pid)
	return spawnOwner{PID: pid, Start: start}, err
}

func watchSpawnOwner(owner spawnOwner, done <-chan struct{}, end func()) {
	if owner.PID == 0 {
		return
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		start, err := spawnProcessIdentity(owner.PID)
		if err != nil || start != owner.Start {
			end()
			return
		}
		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

func shellTaskName(name string, temporary bool) string {
	if temporary {
		return "spawn/" + name
	}
	return "launch/" + name
}

// Lifetime is fixed by startup; attachment state never promotes a spawn.
func shellLifetime(temporary, remote bool) string {
	if !temporary {
		return "persistent"
	}
	if remote {
		return "heartbeat"
	}
	return "parent"
}

func validShellLifetime(value string) bool {
	return value == "" || value == "persistent" || value == "parent" || value == "heartbeat"
}
