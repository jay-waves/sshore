package main

// Inventory is a read-only, exact-release endpoint. It never enters setup or
// installs software. Session data and peer inventory remain advisory snapshots.
import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func localSessionInventory(details ...bool) (inventory, error) {
	detailed := len(details) > 0 && details[0]
	inv := emptyInventory()
	dir, e := runtimeDir()
	if e != nil {
		return inv, e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return inv, e
	}
	paths := map[string]bool{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sock") {
			paths[filepath.Join(dir, entry.Name())] = true
		}
	}
	processes, e := findSessionDaemons("/proc")
	if e != nil {
		return inv, e
	}
	for _, p := range processes {
		if p.socket == "" {
			inv.Errors = append(inv.Errors, fmt.Sprintf("shell daemon %d has no readable endpoint", p.pid))
		} else {
			paths[p.socket] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	action := "health"
	if detailed {
		action = ""
	}
	for _, path := range ordered {
		live, err := sessionIdentityLive(path)
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
			continue
		}
		if !live {
			continue
		}
		c, e := net.DialTimeout("unix", path, time.Second)
		if e != nil {
			inv.Errors = append(inv.Errors, "shell unavailable: "+cleanText(filepath.Base(path)))
			continue
		}
		c.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(c, message{Type: "status", Action: action}); e != nil {
			c.Close()
			inv.Errors = append(inv.Errors, "shell status request failed: "+cleanText(filepath.Base(path)))
			continue
		}
		m, e := readMessage(c)
		c.Close()
		if e != nil || m.Type != "session" || validSession(m.Name) != nil {
			inv.Errors = append(inv.Errors, "invalid shell status: "+cleanText(filepath.Base(path)))
			continue
		}
		state := "detached"
		if m.Attached {
			state = "attached"
		}
		inv.Shells = append(inv.Shells, shellView{Temporary: m.Temporary, Instance: m.Instance, ID: shellTaskName(cleanText(m.Name), m.Temporary), Name: cleanText(m.Name), State: state, PID: m.PID, Age: m.AgeSeconds, Foreground: cleanText(m.Foreground), Directory: cleanText(m.Directory)})
		if detailed {
			inv.Shells[len(inv.Shells)-1].ProcessTree = sessionProcessTree("/proc", m.PID)
		}
	}
	for _, r := range readSessionExits(dir) {
		if time.Since(r.At) > 7*24*time.Hour {
			continue
		}
		inv.Exits = append(inv.Exits, exitView{Name: r.Name, Reason: r.Reason, Time: r.At})
	}
	records, e := collectLocalResources("")
	if e != nil {
		return inv, e
	}
	inv.Resources = inspectResources(records)
	if detailed {
		inv.Resources = append(inv.Resources, collectResourceResults("", inv.Resources)...)
	}
	inv.Peers, e = collectPeerViews()
	return inv, e
}
