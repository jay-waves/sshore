package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type resourceView struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Kind       string `json:"kind"`
	Worker     string `json:"worker"`
	Detail     string `json:"detail"`
	Protocol   string `json:"protocol,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Log        string `json:"log,omitempty"`
	resourceStatus
}
type sessionView struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	PID        int    `json:"pid"`
	Age        *int64 `json:"age_seconds"`
	Foreground string `json:"foreground"`
	Directory  string `json:"directory"`
}
type exitView struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}
type peerView struct {
	ID      string `json:"id"`
	Manager string `json:"manager"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
}
type inventory struct {
	Version   string         `json:"version"`
	Resources []resourceView `json:"resources"`
	Sessions  []sessionView  `json:"sessions"`
	Peers     []peerView     `json:"peers"`
	Exits     []exitView     `json:"exits"`
	Errors    []string       `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Version: version, Resources: []resourceView{}, Sessions: []sessionView{}, Peers: []peerView{}, Exits: []exitView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Worker: "unavailable", Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	if r.Kind == "approval" {
		v.Worker = "pending"
		v.Transport = "not-started"
		return v
	}
	dir, _ := resourceStateDir(r.Kind)
	v.Log = filepath.Join(dir, r.Name+".log")
	v.Protocol = "SSH/TCP"
	if r.Kind == "mount" {
		v.Protocol = "SFTP over SSH"
		v.Mountpoint = r.Mountpoint
		v.ReadOnly = r.ReadOnly
		v.Detail = r.Mountpoint + " -> " + r.Remote
	}
	return v
}
func inspectResources(records []resourceRecord) []resourceView {
	views := make([]resourceView, len(records))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	jobs := make(chan int)
	for n := 0; n < min(8, len(records)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				views[i] = inspectResource(ctx, records[i])
			}
		}()
	}
	for i := range records {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return views
}
func inspectResource(ctx context.Context, r resourceRecord) resourceView {
	v := viewRecord(r)
	if r.Kind == "approval" {
		return v
	}
	resp, err := resourceHTTPContext(ctx, r.Address, r.Token, "GET", "/status", nil)
	if err != nil {
		v.LastError = "local worker unavailable or status deadline exceeded"
		return v
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		v.LastError = "worker status request rejected"
		return v
	}
	var status resourceStatus
	if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&status) != nil || status.Version != version {
		v.LastError = "incompatible or invalid worker status"
		return v
	}
	v.Worker, v.resourceStatus = "running", status
	return v
}

func collectResourceResults(host string, active []resourceView) []resourceView {
	seen := map[string]bool{}
	for _, v := range active {
		seen[v.ID] = true
	}
	var rows []resourceView
	for _, kind := range []string{"mount", "forward"} {
		dir, e := resourceStateDir(kind)
		if e != nil {
			continue
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json.result") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".json.result")
			if validSession(name) != nil || seen[kind+"/"+name] {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if privateState(path, false) != nil {
				continue
			}
			f, e := os.Open(path)
			if e != nil {
				continue
			}
			var r resourceResult
			e = json.NewDecoder(io.LimitReader(f, 128*1024)).Decode(&r)
			f.Close()
			if e != nil || r.Record.Name != name || r.Record.Kind != kind || time.Since(r.Finished) > 7*24*time.Hour || (host != "" && r.Record.Host != host) {
				continue
			}
			v := viewRecord(r.Record)
			v.Worker = "exited"
			v.resourceStatus = r.Status
			rows = append(rows, v)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}
func printDetailedInventory(inv inventory) {
	for _, v := range inv.Resources {
		fmt.Printf("%s · %s\n  worker=%s transport=%s target=%s\n", cleanText(v.ID), cleanText(v.Host), cleanText(v.Worker), cleanText(v.Transport), cleanText(v.Target))
		fmt.Printf("  %s · %s\n", cleanText(v.Protocol), cleanText(v.Detail))
		if v.Kind == "mount" {
			fmt.Printf("  read-only=%t\n", v.ReadOnly)
		}
		if v.LastError != "" {
			fmt.Printf("  last error: %s\n", cleanText(v.LastError))
		}
		if !v.LastOperation.IsZero() {
			fmt.Printf("  last SFTP reply: %s\n", v.LastOperation.Format(time.RFC3339))
		}
		if v.Log != "" {
			fmt.Printf("  log: %s\n", cleanText(v.Log))
		}
	}
	for _, v := range inv.Sessions {
		fmt.Printf("%s [%s] PID %d · %s · %s\n", cleanText(v.Name), cleanText(v.State), v.PID, cleanText(v.Foreground), cleanText(v.Directory))
	}
	for _, v := range inv.Peers {
		fmt.Printf("%s [%s] managed by %s · %s\n", cleanText(v.ID), cleanText(v.State), cleanText(v.Manager), cleanText(v.Detail))
	}
	for _, v := range inv.Exits {
		fmt.Printf("%s exited %s: %s\n", cleanText(v.Name), v.Time.Format(time.RFC3339), cleanText(v.Reason))
	}
}
func runInventory(o options) (int, error) {
	inv := emptyInventory()
	records, e := collectLocalResources(o.host)
	if e != nil {
		inv.Errors = append(inv.Errors, cleanText(e.Error()))
	}
	inv.Resources = inspectResources(records)
	inv.Resources = append(inv.Resources, collectResourceResults(o.host, inv.Resources)...)
	var remote inventory
	if o.host != "" {
		cmd := sshCommand(o)
		cmd.Stdin = os.Stdin
		cmd.Stderr = os.Stderr
		var output limitedInventoryBuffer
		cmd.Stdout = &output
		if e = cmd.Run(); e == nil {
			e = json.Unmarshal(output.data, &remote)
			if e == nil && remote.Version != version {
				e = fmt.Errorf("inventory release mismatch")
			}
		}
	} else if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		remote, e = localSessionInventory()
	} else {
		remote = emptyInventory()
		remote.Peers, e = collectPeerViews()
	}
	if e != nil {
		inv.Errors = append(inv.Errors, cleanText(e.Error()))
	} else {
		inv.Sessions = append(inv.Sessions, remote.Sessions...)
		inv.Peers = append(inv.Peers, remote.Peers...)
		inv.Exits = append(inv.Exits, remote.Exits...)
		inv.Errors = append(inv.Errors, remote.Errors...)
	}
	if o.listJSON {
		if e := json.NewEncoder(os.Stdout).Encode(inv); e != nil {
			return 1, e
		}
	} else {
		printDetailedInventory(inv)
	}
	if len(inv.Errors) > 0 {
		return 1, fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return 0, nil
}

type limitedInventoryBuffer struct{ data []byte }

func (b *limitedInventoryBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 8*1024*1024 {
		return 0, fmt.Errorf("remote inventory exceeds 8 MiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
