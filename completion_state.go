package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type completionTask struct{ kind, name, host, identity string }
type completionCatalog struct {
	hosts []string
	tasks []completionTask
	ids   map[string]uint64
}

// Read hints only: no Dir(), daemon requests, locks, expiry cleanup or ID allocation.
// All records are untrusted suggestions; execution revalidates the selected task.
func readCompletionJSON(path string, value any) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, 1024*1024)).Decode(value) == nil
}

func completionRecords(directory string, read func(string)) {
	path, err := statePath(directory)
	if err != nil {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	entries, _ := f.ReadDir(256)
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			read(filepath.Join(path, entry.Name()))
		}
	}
}

func loadCompletionCatalog() completionCatalog {
	c := completionCatalog{ids: map[string]uint64{}}
	path, err := statePath("tasks/ids.json")
	if err == nil {
		var index taskIDIndex
		if readCompletionJSON(path, &index) {
			c.ids = index.IDs
		}
	}
	path, err = statePath("connections/recent.json")
	if err == nil {
		var recent []recentConnection
		if readCompletionJSON(path, &recent) {
			for _, r := range recent {
				if validSSHDestination(r.Host) == nil {
					c.hosts = append(c.hosts, r.Host)
				}
			}
		}
	}
	completionRecords("local-shells", func(path string) {
		var r localShellRecord
		if readCompletionJSON(path, &r) && validSession(r.Name) == nil && r.Token != "" {
			c.tasks = append(c.tasks, completionTask{"shell", r.Name, "", r.identity()})
		}
	})
	completionRecords("shells", func(path string) {
		var r shellSnapshot
		if !readCompletionJSON(path, &r) || validSSHDestination(r.Host) != nil || validSession(r.Name) != nil {
			return
		}
		c.hosts = append(c.hosts, r.Host)
		if r.State == "detached" || r.State == "attached" {
			c.tasks = append(c.tasks, completionTask{"shell", r.Name, r.Host, remoteShellIdentity(r.Host, r.Instance)})
		}
	})
	for _, kind := range []string{"mount", "forward"} {
		completionRecords(kind+"s", func(path string) {
			var r resourceRecord
			if readCompletionJSON(path, &r) && validSession(r.Name) == nil && r.Kind == kind {
				c.tasks = append(c.tasks, completionTask{kind, r.Name, "", resourceIdentity(r)})
			}
		})
	}
	completionRecords("approvals", func(path string) {
		var r approvalRecord
		if readCompletionJSON(path, &r) && validApprovalID(r.ID) && (validSession(r.DisplayName()) == nil || validApprovalID(r.DisplayName())) && time.Since(r.Created) < approvalTTL && !r.Created.After(time.Now()) {
			c.tasks = append(c.tasks, completionTask{"review", r.DisplayName(), "", resourceIdentity(resourceRecord{Kind: "review", Token: r.ID})})
		}
	})
	if home, err := os.UserHomeDir(); err == nil {
		c.hosts = append(c.hosts, completionSSHHosts(filepath.Join(home, ".ssh", "config"))...)
	}
	return c
}

// Only literal Host aliases are hints. Never run ssh -G, Match exec, proxies or
// configuration substitutions. Includes are bounded and read without execution.
func completionSSHHosts(path string) []string {
	var hosts []string
	seen := map[string]bool{}
	var read func(string, int)
	read = func(path string, depth int) {
		path, _ = filepath.Abs(path)
		if depth > 4 || seen[path] || len(seen) >= 32 {
			return
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		scanner := bufio.NewScanner(io.LimitReader(f, 256*1024))
		for scanner.Scan() {
			fields := strings.Fields(strings.ReplaceAll(strings.SplitN(scanner.Text(), "#", 2)[0], "=", " "))
			if len(fields) < 2 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "host":
				for _, host := range fields[1:] {
					host = strings.Trim(host, "\"'")
					if !strings.ContainsAny(host, "*?!%$") && validSSHDestination(host) == nil {
						hosts = append(hosts, host)
					}
				}
			case "include":
				for _, pattern := range fields[1:] {
					pattern = strings.Trim(pattern, "\"'")
					if strings.ContainsAny(pattern, "%$") {
						continue
					}
					if strings.HasPrefix(pattern, "~/") {
						home, _ := os.UserHomeDir()
						pattern = filepath.Join(home, pattern[2:])
					}
					if !filepath.IsAbs(pattern) {
						home, _ := os.UserHomeDir()
						pattern = filepath.Join(home, ".ssh", pattern)
					}
					paths, _ := filepath.Glob(pattern)
					for _, child := range paths {
						if len(seen) >= 32 {
							break
						}
						read(child, depth+1)
					}
				}
			}
		}
	}
	read(path, 0)
	return hosts
}
