package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type resourceView struct {
	local      bool // Set only for records inspected by this process; never sent over the wire.
	order      uint64
	ShortID    string `json:"short_id,omitempty"`
	Identity   string `json:"identity,omitempty"`
	ID         string `json:"id"`
	Host       string `json:"host"`
	Kind       string `json:"kind"`
	Worker     string `json:"worker"`
	Detail     string `json:"detail"`
	Protocol   string `json:"protocol,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Log        string `json:"log,omitempty"`
	resourceStatus
}
type shellView struct {
	MachineID   string `json:"machine_id,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Lifetime    string `json:"lifetime"`
	Temporary   bool   `json:"temporary,omitempty"`
	Instance    string `json:"instance,omitempty"`
	order       uint64
	ShortID     string    `json:"short_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	ID          string    `json:"id"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
	Unconfirmed bool      `json:"remote_state_unconfirmed,omitempty"`
	Active      bool      `json:"local_connection_active,omitempty"`
	Historical  bool      `json:"historical"`
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
	Direction string `json:"direction,omitempty"`
	UID       string `json:"uid,omitempty"`
	Identity  string `json:"identity,omitempty"`
	order     uint64
	ShortID   string `json:"short_id,omitempty"`
	ID        string `json:"id"`
	Manager   string `json:"manager"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}
type inventory struct {
	MachineID     string           `json:"machine_id,omitempty"`
	Hostname      string           `json:"hostname,omitempty"`
	ShellTree     []shellTreeNode  `json:"shell_tree,omitempty"`
	LocalShells   []localShellView `json:"local_shells,omitempty"`
	Version       string           `json:"version"`
	Host          string           `json:"host,omitempty"`
	RemoteQueried bool             `json:"remote_queried"`
	Resources     []resourceView   `json:"resources"`
	Shells        []shellView      `json:"shells"`
	Peers         []peerView       `json:"peers"`
	Exits         []exitView       `json:"exits"`
	Errors        []string         `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Version: version, Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Exits: []exitView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{local: true, Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Direction: r.Direction, Worker: "unavailable", Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	if r.Kind == "review" {
		v.Worker = "pending"
		v.Transport = "not-started"
		return v
	}
	dir, _ := resourceStateDir(r.Kind)
	v.Log = filepath.Join(dir, r.storageName()+".log")
	v.Protocol = "SSH/TCP"
	if r.Kind == "mount" {
		v.Protocol = "SFTP over SSH"
		v.Direction = "<-"
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
		wg.Go(func() {
			for i := range jobs {
				views[i] = inspectResource(ctx, records[i])
			}
		})
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
	reply, err := daemonResourceRequest(ctx, "/resources/"+r.UID+"/status", nil)
	if err != nil {
		v.LastError = "local worker unavailable or status deadline exceeded"
		return v
	}
	status := reply.Status
	if status.Version != version {
		v.LastError = "incompatible or invalid worker status"
		return v
	}
	v.Worker, v.resourceStatus = "running", status
	return v
}

func collectResourceResults(host string, active []resourceView) []resourceView {
	seen := map[string]bool{}
	for _, v := range active {
		if v.Identity != "" {
			seen[v.Identity] = true
		}
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
			if validSession(name) != nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if privateStatePath(path, false) != nil {
				continue
			}
			f, e := os.Open(path)
			if e != nil {
				continue
			}
			var r resourceResult
			e = json.NewDecoder(io.LimitReader(f, 128*1024)).Decode(&r)
			f.Close()
			if e != nil || r.Record.storageName() != name || r.Record.Kind != kind || time.Since(r.Finished) > 7*24*time.Hour || (host != "" && r.Record.Host != host) {
				continue
			}
			if validTaskUID(r.Record.UID) && seen[resourceIdentity(r.Record)] {
				continue
			}
			if !validTaskUID(r.Record.UID) && seen[kind+"/"+name] {
				continue
			}
			if validSession(r.DisplayName) == nil {
				r.Record.slot, r.Record.Name = name, r.DisplayName
				if !validTaskUID(r.Record.UID) && seen[kind+"/"+r.DisplayName] {
					continue
				}
			}
			v := viewRecord(r.Record)
			v.Worker = "exited"
			v.resourceStatus = r.Status
			rows = append(rows, v)
		}
	}
	slices.SortFunc(rows, func(a, b resourceView) int { return cmp.Compare(a.ID, b.ID) })
	return rows
}
func collectInventory(o options) (inventory, error) {
	inv := emptyInventory()
	inv.Host = o.host
	if o.host != "" {
		var output limitedInventoryBuffer
		err := sshio.Run(context.Background(), sshConnectionOptions(o), nil, &output, os.Stderr)
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
			if warn, routeErr := observeIdentityRoute(inv.MachineID, o.host, o.ssh); routeErr == nil && warn {
				fmt.Fprintln(os.Stderr, "[sshore] Same identity through a new entry point; if this is a clone, run sshore reset there.")
			}
			if err := cacheRemoteShells(o.host, inv, o); err != nil {
				inv.Errors = append(inv.Errors, "could not save shell history: "+cleanText(err.Error()))
			}
		}
	} else {
		hosted, hostedErr := hostedShellInventory(o.listJSON)
		inv.MachineID, inv.Hostname = hosted.MachineID, hosted.Hostname
		inv.ShellTree = hosted.ShellTree
		inv.Shells, inv.Exits = hosted.Shells, hosted.Exits
		inv.Errors = append(inv.Errors, hosted.Errors...)
		if hostedErr != nil {
			inv.Errors = append(inv.Errors, cleanText(hostedErr.Error()))
		}
		for i := range inv.Shells {
			inv.Shells[i].Host = "local"
		}
		local, localErr := nativeShells()
		inv.LocalShells = local
		if localErr != nil {
			inv.Errors = append(inv.Errors, cleanText(localErr.Error()))
		}
		records, err := collectLocalResources("")
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
		inv.Resources = inspectResources(records)
		if o.listJSON {
			inv.Resources = append(inv.Resources, collectResourceResults("", inv.Resources)...)
		}
		snapshots, err := readShellSnapshots()
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
		for _, s := range snapshots {
			if s.Forgotten {
				continue
			}
			inv.Shells = append(inv.Shells, shellView{Lifetime: shellLifetime(s.Temporary, true), Temporary: s.Temporary, Instance: s.Instance, ID: shellTaskName(s.Name, s.Temporary), Host: s.Host, MachineID: s.MachineID, Hostname: s.Hostname, Name: s.Name, State: s.State, ConfirmedAt: s.ConfirmedAt, Unconfirmed: s.Unconfirmed, Active: s.Active, Historical: true})
		}
		inv.Peers, err = collectPeerViews()
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
	}
	if err := numberInventoryResources(&inv); err != nil {
		return inv, err
	}
	if err := numberInventoryShells(&inv); err != nil {
		return inv, err
	}
	if err := numberInventoryPeers(&inv); err != nil {
		return inv, err
	}
	if len(inv.Errors) > 0 {
		return inv, fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return inv, nil
}

func runInventory(o options) (int, error) {
	inv, err := collectInventory(o)
	if o.listJSON {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(inv); encodeErr != nil {
			return 1, encodeErr
		}
	} else {
		printTaskInventory(inv)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func snapshotState(s shellView) string {
	if s.Active {
		return "local active"
	}
	if s.Unconfirmed || s.State == "attached" || s.ConfirmedAt.IsZero() {
		return "unconfirmed"
	}
	return "-"
}

type limitedInventoryBuffer struct{ data []byte }

func (b *limitedInventoryBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 8*1024*1024 {
		return 0, fmt.Errorf("remote inventory exceeds 8 MiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
