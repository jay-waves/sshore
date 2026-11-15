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
type shellView struct {
	Host        string    `json:"host,omitempty"`
	ID          string    `json:"id"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
	Unconfirmed bool      `json:"remote_state_unconfirmed,omitempty"`
	Active      bool      `json:"local_connection_active,omitempty"`
	Historical  bool      `json:"historical"`
	ProcessTree string    `json:"process_tree,omitempty"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	PID         int       `json:"pid"`
	Age         *int64    `json:"age_seconds"`
	Foreground  string    `json:"foreground"`
	Directory   string    `json:"directory"`
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
	Version       string         `json:"version"`
	Host          string         `json:"host,omitempty"`
	RemoteQueried bool           `json:"remote_queried"`
	Resources     []resourceView `json:"resources"`
	Shells        []shellView    `json:"shells"`
	Peers         []peerView     `json:"peers"`
	Exits         []exitView     `json:"exits"`
	Errors        []string       `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Version: version, Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Exits: []exitView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Worker: "unavailable", Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	if r.Kind == "review" {
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
	if r.Kind == "review" {
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
func runInventory(o options) (int, error) {
	inv := emptyInventory()
	inv.Host = o.host
	if o.host != "" {
		fmt.Fprintf(os.Stderr, "[sshore] Querying remote account on %s via SSH...\n", cleanText(o.host))
		cmd := sshCommand(o)
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
		var output limitedInventoryBuffer
		cmd.Stdout = &output
		err := cmd.Run()
		if err == nil {
			err = json.Unmarshal(output.data, &inv)
		}
		if err == nil && inv.Version != version {
			err = fmt.Errorf("inventory release mismatch")
		}
		if err != nil {
			inv = emptyInventory()
			inv.Host = o.host
			inv.Errors = append(inv.Errors, "remote query failed: "+cleanText(err.Error()))
		} else {
			inv.Host, inv.RemoteQueried = o.host, true
			if err := cacheRemoteShells(o.host, inv); err != nil {
				inv.Errors = append(inv.Errors, "could not save shell history: "+cleanText(err.Error()))
			}
		}
	} else {
		records, err := collectLocalResources("")
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
		inv.Resources = inspectResources(records)
		if o.listDetails || o.listJSON {
			inv.Resources = append(inv.Resources, collectResourceResults("", inv.Resources)...)
		}
		snapshots, err := readShellSnapshots()
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
		for _, s := range snapshots {
			inv.Shells = append(inv.Shells, shellView{ID: "shell/" + s.Name, Host: s.Host, Name: s.Name, State: s.State, ConfirmedAt: s.ConfirmedAt, Unconfirmed: s.Unconfirmed, Active: s.Active, Historical: true})
		}
		inv.Peers, err = collectPeerViews()
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
	}
	if o.listJSON {
		if err := json.NewEncoder(os.Stdout).Encode(inv); err != nil {
			return 1, err
		}
	} else {
		printInventory(inv, o.listDetails)
	}
	if len(inv.Errors) > 0 {
		return 1, fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return 0, nil
}

func snapshotState(s shellView, now time.Time) string {
	if s.ConfirmedAt.IsZero() {
		return "remote state unconfirmed"
	}
	age := now.Sub(s.ConfirmedAt)
	elapsed := "just now"
	if age >= time.Minute {
		elapsed = fmt.Sprintf("%d minutes ago", int(age/time.Minute))
	}
	history := fmt.Sprintf("last state: %s · %s", cleanText(s.State), elapsed)
	if s.Active {
		return "local connection active · " + history
	}
	if s.Unconfirmed || s.State == "attached" {
		return "remote state unconfirmed · " + history
	}
	if age < time.Minute && s.State == "detached" {
		return "detached · just confirmed"
	}
	return history
}

func printInventory(inv inventory, details bool) {
	if inv.RemoteQueried {
		fmt.Printf("Remote account on %s · queried now\n", cleanText(inv.Host))
		for _, s := range inv.Shells {
			fmt.Printf("  shell/%s [%s]%s\n", cleanText(s.Name), cleanText(s.State), sessionAgePortable(s.Age))
			if details {
				fmt.Printf("    %s · %s\n", cleanText(s.Foreground), cleanText(s.Directory))
				for _, line := range strings.Split(strings.TrimSuffix(s.ProcessTree, "\n"), "\n") {
					if line != "" {
						fmt.Println("    " + cleanText(line))
					}
				}
			}
		}
		if len(inv.Shells) == 0 {
			fmt.Println("  No shells reported.")
		}
		if len(inv.Resources) > 0 {
			fmt.Println("\nResources managed on this remote computer:")
			printResourceRows(inv.Resources, details)
		}
		if details {
			for _, e := range inv.Exits {
				fmt.Printf("  shell/%s exited %s: %s\n", cleanText(e.Name), e.Time.Format(time.RFC3339), cleanText(e.Reason))
			}
		}
	} else if inv.Host == "" {
		fmt.Println("Local inventory · remote not queried; known records only")
		fmt.Println("Worker running confirms the local worker only; target service health is unchecked.")
		hosts := map[string]bool{}
		for _, r := range inv.Resources {
			hosts[r.Host] = true
		}
		for _, s := range inv.Shells {
			hosts[s.Host] = true
		}
		sorted := make([]string, 0, len(hosts))
		for host := range hosts {
			sorted = append(sorted, host)
		}
		sort.Strings(sorted)
		for _, host := range sorted {
			fmt.Printf("\n%s\n", cleanText(host))
			for _, s := range inv.Shells {
				if s.Host == host {
					fmt.Printf("  shell/%s · %s\n", cleanText(s.Name), snapshotState(s, time.Now()))
					if details && !s.ConfirmedAt.IsZero() {
						fmt.Printf("    last confirmation: %s\n", s.ConfirmedAt.Format(time.RFC3339))
					}
				}
			}
			var rows []resourceView
			for _, r := range inv.Resources {
				if r.Host == host {
					rows = append(rows, r)
				}
			}
			printResourceRows(rows, details)
		}
		if len(sorted) == 0 {
			fmt.Println("No local resources or known shells.")
		}
	}
	if len(inv.Peers) > 0 {
		fmt.Println("\nResources managed by other computers (read-only, advisory):")
		for _, p := range inv.Peers {
			fmt.Printf("  %s [%s] · manager %s · %s\n", cleanText(p.ID), cleanText(p.State), cleanText(p.Manager), cleanText(p.Detail))
		}
	}
}
func printResourceRows(rows []resourceView, details bool) {
	for _, r := range rows {
		fmt.Printf("  %s [%s] · %s\n", cleanText(r.ID), cleanText(r.Worker), cleanText(r.Detail))
		if details {
			fmt.Printf("    transport=%s target=%s protocol=%s read-only=%t\n", cleanText(r.Transport), cleanText(r.Target), cleanText(r.Protocol), r.ReadOnly)
			if r.LastError != "" {
				fmt.Printf("    last error: %s\n", cleanText(r.LastError))
			}
			if !r.LastOperation.IsZero() {
				fmt.Printf("    last SFTP reply: %s\n", r.LastOperation.Format(time.RFC3339))
			}
			if r.Log != "" {
				fmt.Printf("    log: %s\n", cleanText(r.Log))
			}
		}
	}
}
func sessionAgePortable(age *int64) string {
	if age == nil {
		return ""
	}
	return fmt.Sprintf(" · %ds old", *age)
}

type limitedInventoryBuffer struct{ data []byte }

func (b *limitedInventoryBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 8*1024*1024 {
		return 0, fmt.Errorf("remote inventory exceeds 8 MiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
