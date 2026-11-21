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
)

type resourceView struct {
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
	MachineID   string `json:"machine_id,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Lifetime    string `json:"lifetime"`
	Temporary   bool   `json:"temporary,omitempty"`
	Instance    string `json:"instance,omitempty"`
	ShortID     string `json:"short_id,omitempty"`
	Host        string `json:"host,omitempty"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	PID         int    `json:"pid"`
	Age         *int64 `json:"age_seconds"`
	Foreground  string `json:"foreground"`
	Directory   string `json:"directory"`
	ProcessTree string `json:"process_tree,omitempty"`
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
	RemoteQueried bool             `json:"remote_queried"` // Legacy JSON key. Inventory reads the local daemon.
	Resources     []resourceView   `json:"resources"`
	Shells        []shellView      `json:"shells"`
	Peers         []peerView       `json:"peers"`
	Errors        []string         `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Version: version, Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Direction: r.Direction, Worker: "unavailable", Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	if log, err := statePath(filepath.Join(r.Kind+"s", r.UID+".log")); err == nil {
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

func managedInventory(ctx context.Context, tasks *daemonTasks, detail string, includeNative bool, host string) inventory {
	inv := emptyInventory()
	if runtime.GOOS == "linux" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	request := taskInventoryRequest{Native: includeNative, Hosted: host == "", Resources: true, Status: true, Detail: detail, Host: host}
	snapshot, workers := tasks.taskInventory(request)
	statuses := make(map[string]resourceStatus)
	ctx, cancel := context.WithTimeout(ctx, resourceStatusTimeout)
	defer cancel()
	probeResourceStatus(ctx, workers, func(row resourceStatusRow) bool {
		statuses[row.UID] = row.Status
		return true
	})
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	if includeNative {
		inv.LocalShells = snapshot.nativeShells()
	}
	inv.Resources = resourceViews(snapshot.Resources, statuses, true)
	return inv
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

type taskListRequest struct {
	Native bool
	Host   string
}

func (a *daemonActivity) listTasks(ctx context.Context, request taskListRequest) inventory {
	inv := managedInventory(ctx, a.tasks, "inventory", request.Native, request.Host)
	if request.Host != "" {
		inv.Host = request.Host
		rows, err := readParticipatingShells(request.Host)
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
		for _, row := range rows {
			inv.Shells = append(inv.Shells, shellView{Host: row.Host, Instance: row.Instance, ID: shellTaskName(row.Name, row.Temporary), Name: row.Name, State: row.State, Temporary: row.Temporary, Lifetime: row.Lifetime, PID: row.PID})
		}
		inv.setTaskReferences()
		return inv
	}
	if request.Native {
		inv.Host = "local"
		for i := range inv.Shells {
			inv.Shells[i].Host = "local"
		}
	}
	inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
	inv.setTaskReferences()
	return inv
}

func daemonListTasks(request taskListRequest) (inventory, error) {
	var reply inventory
	if activity := accountActivity.Load(); activity != nil {
		reply = activity.listTasks(context.Background(), request)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
		defer cancel()
		record, err := activityDaemon(ctx)
		if os.IsNotExist(err) && !activityAllowed() {
			return emptyInventory(), nil
		}
		if err != nil {
			return emptyInventory(), err
		}
		if err := daemonJSON(ctx, record, "/tasks/list", request, &reply); err != nil {
			return emptyInventory(), err
		}
	}
	return reply, nil
}

func collectInventory(o options) (inventory, error) {
	inv, err := daemonListTasks(taskListRequest{Native: o.host == "", Host: o.host})
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	return inv, inv.incompleteError()
}

func (inv inventory) incompleteError() error {
	if len(inv.Errors) > 0 {
		return fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return nil
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
