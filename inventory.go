package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

type resourceView struct {
	local      bool   // Set only for records inspected by this process; never sent over the wire.
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

func (r resourceView) kindAndName() (string, string) {
	kind := r.Kind
	if kind == "" {
		kind, _, _ = strings.Cut(r.ID, "/")
	}
	return kind, strings.TrimPrefix(r.ID, kind+"/")
}

type shellView struct {
	MachineID   string    `json:"machine_id,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	Lifetime    string    `json:"lifetime"`
	Temporary   bool      `json:"temporary,omitempty"`
	Instance    string    `json:"instance,omitempty"`
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
	ProcessTree string    `json:"process_tree,omitempty"`
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
	if log, err := statePath(filepath.Join(r.Kind+"s", r.storageName()+".log")); err == nil {
		v.Log = log
	}
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

// Read only registered tasks here. Historical and peer observations are
// collected separately and cannot become part of this snapshot.
func readTaskInventory(request taskInventoryRequest) (taskInventorySnapshot, map[string]resourceStatus, error) {
	timeout := daemonRequestTimeout
	if request.Status && !request.Native && !request.Hosted {
		timeout = resourceStatusTimeout
	}
	if request.Detail == "health" {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	statuses := make(map[string]resourceStatus)
	var snapshot taskInventorySnapshot
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return snapshot, nil, nil
	}
	if err != nil {
		return snapshot, nil, err
	}
	req, err := daemonRequest(ctx, record, "/tasks/snapshot", request)
	if err != nil {
		return snapshot, nil, err
	}
	resp, err := daemonDo(req)
	if err != nil {
		return snapshot, nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&snapshot); err != nil {
		return snapshot, nil, err
	}
	if request.Status {
		for range snapshot.Resources {
			var row resourceStatusRow
			if dec.Decode(&row) != nil {
				break
			}
			statuses[row.UID] = row.Status
		}
	}
	return snapshot, statuses, nil
}

func (snapshot taskInventorySnapshot) nativeShells() []localShellView {
	rows := make([]localShellView, 0, len(snapshot.Native))
	for identity, view := range snapshot.Native {
		view.identity, view.ShortID = identity, taskShortCode(identity)
		rows = append(rows, view)
	}
	sortLocalShells(rows)
	return rows
}

func collectManagedInventory(detailed, includeNative bool) inventory {
	return managedInventory(nil, detailed, includeNative)
}

func managedInventory(tasks *daemonTasks, detailed, includeNative bool) inventory {
	inv := emptyInventory()
	if runtime.GOOS == "linux" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	detail := "inventory"
	if detailed {
		detail = ""
	}
	request := taskInventoryRequest{Native: includeNative, Hosted: true, Resources: true, Status: true, Detail: detail}
	var snapshot taskInventorySnapshot
	var statuses map[string]resourceStatus
	var err error
	if tasks == nil {
		snapshot, statuses, err = readTaskInventory(request)
	} else {
		var workers []*daemonResourceTask
		snapshot, workers = tasks.taskInventory(request)
		statuses = make(map[string]resourceStatus)
		ctx, cancel := context.WithTimeout(context.Background(), resourceStatusTimeout)
		defer cancel()
		probeResourceStatus(ctx, workers, func(row resourceStatusRow) bool {
			statuses[row.UID] = row.Status
			return true
		})
	}
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
		return inv
	}
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	if includeNative {
		inv.LocalShells = snapshot.nativeShells()
	}
	inv.Resources = resourceViews(snapshot.Resources, statuses, true)
	if detailed {
		inv.Resources = append(inv.Resources, collectResourceResults(inv.Resources)...)
	}
	return inv
}

func (inv *inventory) collectPeers() {
	peers, err := collectPeerViews()
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	inv.Peers = excludeLocalResourcePeers(peers, inv.Resources)
}

func (inv *inventory) collectShellHistory() {
	inv.Exits = hostedExitHistory()
	if inv.Hostname != "" {
		inv.ShellTree = knownShellTrees()
	}
}

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	if len(peers) == 0 || len(resources) == 0 {
		return peers
	}
	// Resource labels contain a slash, so they can share the index with UIDs.
	localIDs := make(map[string]bool, len(resources))
	for _, resource := range resources {
		if resource.Worker == "exited" {
			continue
		}
		localIDs[resource.ID] = true
		if uid := canonicalTaskUID(resource.Identity); uid != "" {
			localIDs[uid] = true
		}
	}
	manager := taskManagerLabel()
	filtered := peers[:0]
	for _, p := range peers {
		resourceID := peerResourceID(p.ID)
		if resourceID != p.ID {
			if validTaskUID(p.UID) {
				if localIDs[p.UID] {
					continue
				}
			} else if p.Manager == manager && localIDs[resourceID] {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	return filtered
}

func collectInventory(o options) (inventory, error) {
	if o.host != "" {
		return collectRemoteInventory(o).finalize()
	}

	inv := collectManagedInventory(o.listJSON, true)
	if o.listJSON {
		inv.collectShellHistory()
	}
	for i := range inv.Shells {
		inv.Shells[i].Host = "local"
	}
	snapshots, err := readShellSnapshots()
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	for _, s := range snapshots {
		if s.Forgotten {
			continue
		}
		unconfirmed := s.Unconfirmed || s.State == "attached" && !s.Active
		inv.Shells = append(inv.Shells, shellView{Lifetime: shellLifetime(s.Temporary, true), Temporary: s.Temporary, Instance: s.Instance, ID: shellTaskName(s.Name, s.Temporary), Host: s.Host, MachineID: s.MachineID, Hostname: s.Hostname, Name: s.Name, State: s.State, ConfirmedAt: s.ConfirmedAt, Unconfirmed: unconfirmed, Active: s.Active, Historical: true})
	}
	inv.collectPeers()
	return inv.finalize()
}

func collectRemoteInventory(o options) inventory {
	inv := emptyInventory()
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
		return inv
	}
	inv.Host, inv.RemoteQueried = o.host, true
	if warn, routeErr := observeIdentityRoute(inv.MachineID, o.host, o.ssh); routeErr == nil && warn {
		fmt.Fprintln(os.Stderr, "[sshore] Same identity through a new entry point; if this is a clone, run sshore reset there.")
	}
	if err := cacheRemoteShells(o.host, inv, o); err != nil {
		inv.Errors = append(inv.Errors, "could not save shell history: "+cleanText(err.Error()))
	}
	return inv
}

func (inv inventory) finalize() (inventory, error) {
	inv.setTaskReferences()
	if len(inv.Errors) > 0 {
		return inv, fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return inv, nil
}

func (s shellView) identity(defaultHost string) string {
	if s.Host != "" {
		return remoteShellIdentity(s.Host, s.Instance)
	}
	return remoteShellIdentity(defaultHost, s.Instance)
}

func (inv *inventory) setTaskReferences() {
	for i := range inv.Shells {
		shell := &inv.Shells[i]
		shell.ShortID = taskShortCode(shell.identity(inv.Host))
	}
	for i := range inv.Resources {
		resource := &inv.Resources[i]
		resource.ShortID = taskShortCode(resource.Identity)
	}
	slices.SortStableFunc(inv.Resources, func(a, b resourceView) int {
		if order := cmp.Compare(a.ShortID, b.ShortID); order != 0 {
			return order
		}
		return cmp.Compare(taskUID(a.Identity), taskUID(b.Identity))
	})
	for i := range inv.Peers {
		peer := &inv.Peers[i]
		peer.ShortID = taskShortCode(peerTaskIdentity(*peer))
	}
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
