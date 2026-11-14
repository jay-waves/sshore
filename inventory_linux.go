package main

// Inventory is a read-only, exact-release endpoint. It never enters setup or
// installs software. Session data and peer inventory remain advisory snapshots.
import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func localSessionInventory() (inventory, error) {
	inv := emptyInventory()
	dir, e := runtimeDir()
	if e != nil {
		return inv, e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return inv, e
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}
		c, e := net.DialTimeout("unix", filepath.Join(dir, entry.Name()), time.Second)
		if e != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(c, message{Type: "status"}); e != nil {
			c.Close()
			continue
		}
		m, e := readMessage(c)
		c.Close()
		if e != nil || m.Type != "session" {
			continue
		}
		state := "detached"
		if m.Attached {
			state = "attached"
		}
		inv.Sessions = append(inv.Sessions, sessionView{Name: cleanText(m.Name), State: state, PID: m.PID, Age: m.AgeSeconds, Foreground: cleanText(m.Foreground), Directory: cleanText(m.Directory)})
	}
	for _, r := range readSessionExits(dir) {
		inv.Exits = append(inv.Exits, exitView{Name: r.Name, Reason: r.Reason, Time: r.At})
	}
	inv.Peers, e = collectPeerViews()
	return inv, e
}
