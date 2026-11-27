package main

import (
	"cmp"
	"context"
	json "encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	bolt "go.etcd.io/bbolt"
)

const (
	taskActive   = "active"
	taskDetached = "detached"
	taskWaiting  = "waiting"
	taskStarting = "starting"
	taskUnknown  = "unknown"
	taskExited   = "exited"
)

func taskHiddenFromInventory(state string) bool {
	return state == taskStarting || state == taskExited
}

const inventoryStateHelp = `Task states
  R  Attached session or registered resource
  S  Waiting for terminal control
  T  Detached session (can keep running)
  ?  Unknown

Starting and exited tasks are omitted.
`

type resourceView struct {
	ShortID  string `json:"short_id,omitempty"`
	Identity string `json:"identity,omitempty"`
	ID       string `json:"id"`
	Host     string `json:"host"`
	Detail   string `json:"detail"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func (r resourceView) kindAndName() (string, string) {
	kind, name, _ := strings.Cut(r.ID, "/")
	return kind, name
}

type shellView struct {
	HostInfo    hostInfo  `json:"host_info,omitzero"`
	SourceHost  string    `json:"source_host,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	PinUntil    time.Time `json:"pin_until,omitzero"`
	Instance    string    `json:"instance,omitempty"`
	ShortID     string    `json:"short_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Observed    bool      `json:"observed,omitempty"`
	SeenAt      time.Time `json:"seen_at,omitzero"`
	PID         int       `json:"pid"`
	Shell       string    `json:"shell,omitempty"`
	Program     bool      `json:"program,omitempty"`
	ProgramLine string    `json:"program_line,omitempty"`
	Directory   string    `json:"directory"`
}
type peerView struct {
	UID    string `json:"uid"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}
type inventory struct {
	LocalHost   *hostInfo        `json:"local_host,omitempty"`
	LocalShells []localShellView `json:"local_shells,omitempty"`
	Resources   []resourceView   `json:"resources"`
	Shells      []shellView      `json:"shells"`
	Peers       []peerView       `json:"peers"`
	Errors      []string         `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Detail: r.Detail}
	if r.Kind == "mount" {
		v.ReadOnly = r.ReadOnly
		v.Detail = sshio.FormatEndpoint(r.Host, r.Remote) + " -> " + r.Mountpoint
	}
	return v
}

// Read only registered tasks here. Historical and peer observations are
// collected separately and cannot become part of this snapshot.
func readTaskInventory(request taskInventoryRequest) (taskInventorySnapshot, error) {
	var snapshot taskInventorySnapshot
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err == nil {
		err = daemonJSON(context.Background(), record, "/tasks/snapshot", request, &snapshot)
	}
	return snapshot, err
}

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	local := make(map[string]bool, len(resources))
	for _, r := range resources {
		local[canonicalTaskUID(r.Identity)] = true
	}
	return slices.DeleteFunc(peers, func(p peerView) bool { return local[p.UID] })
}

func mergeInventory(inv inventory, observed []participatingShell) inventory {
	// Both renderers prefer a live row over a cached observation of the same task.
	live := make(map[string]bool, len(inv.LocalShells)+len(inv.Shells))
	for _, shell := range inv.LocalShells {
		if uid := canonicalTaskUID(shell.Identity); uid != "" && !taskHiddenFromInventory(shell.State) {
			live[uid] = true
		}
	}
	for _, shell := range inv.Shells {
		if !shell.Observed && shell.Instance != "" && !taskHiddenFromInventory(shell.State) {
			live[shell.Instance] = true
		}
	}
	for _, row := range observed {
		if !live[row.Instance] {
			inv.Shells = append(inv.Shells, shellView{HostInfo: row.HostInfo, Host: row.Host, Owner: row.Owner, Instance: row.Instance, Name: row.Name, State: row.State, Observed: true, SeenAt: row.ChangedAt, Pinned: row.Pinned, PinUntil: row.PinUntil, PID: row.PID, Shell: row.Shell, Program: row.Program})
		}
	}
	inv.setTaskReferences()
	return inv
}

func collectInventory() (inventory, error) {
	inv, err := readPublishedInventory()
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

func (inv *inventory) setTaskReferences() {
	for i := range inv.Shells {
		shell := &inv.Shells[i]
		shell.ShortID = taskShortCode(identityFromUID(shell.Instance))
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
}

var liveTasksBucket = []byte("live-task-rows")

// Serialize projection and commit so delayed notifications always read the latest state.
func (a *daemonActivity) publishRow(key string, project func() any) {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	if a.instance == "" {
		return
	}
	row := project()
	err := historyTransaction(a.root, true, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(liveTasksBucket)
		if bucket != nil && string(bucket.Get([]byte("instance"))) != a.instance {
			if err := tx.DeleteBucket(liveTasksBucket); err != nil {
				return err
			}
			bucket = nil
		}
		if bucket == nil {
			var err error
			bucket, err = tx.CreateBucket(liveTasksBucket)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte("instance"), []byte(a.instance)); err != nil {
				return err
			}
		}
		if row == nil {
			return bucket.Delete([]byte(key))
		}
		data, err := json.Marshal(row)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), data)
	})
	if err == nil {
		if timer := a.inventoryRetry[key]; timer != nil {
			timer.Stop()
			delete(a.inventoryRetry, key)
		}
		return
	}
	if a.inventoryRetry == nil {
		a.inventoryRetry = make(map[string]*time.Timer)
	}
	if timer := a.inventoryRetry[key]; timer != nil {
		timer.Reset(time.Second)
	} else {
		fmt.Fprintf(a.tasks.log, "publish task inventory: %v\n", err)
		a.inventoryRetry[key] = time.AfterFunc(time.Second, func() { a.publishRow(key, project) })
	}
}

func (a *daemonActivity) publishTask(task daemonTask) {
	switch t := task.(type) {
	case *tmuxTask:
		key := "local/" + t.record.UID
		if t.hosted {
			key = "shell/" + t.record.UID
		}
		a.publishRow(key, func() any {
			t.mu.Lock()
			defer t.mu.Unlock()
			view := t.snapshotLocked()
			if t.expired() || view.State == taskExited {
				return nil
			}
			if t.hosted {
				return view
			}
			return localView(t.record, view)
		})
	case *daemonResourceTask:
		uid := t.record.UID
		a.publishRow("resource/"+uid, func() any {
			t, ok := a.tasks.lookup(uid).(*daemonResourceTask)
			if !ok {
				return nil
			}
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.expired() {
				return nil
			}
			return viewRecord(t.record)
		})
	}
}

func (a *daemonActivity) publishPeer(key string) {
	a.publishRow("peer/"+key, func() any {
		a.mu.Lock()
		defer a.mu.Unlock()
		var newest *activityLease
		for l := range a.leases {
			if l.peer != nil && l.peer.Key == key && (newest == nil || l.order > newest.order) {
				newest = l
			}
		}
		if newest == nil {
			return nil
		}
		p := newest.peer
		return peerView{UID: p.UID, ID: p.ID, Detail: p.Detail}
	})
}

func (a *daemonActivity) closeInventory() {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	a.instance = ""
	for _, timer := range a.inventoryRetry {
		timer.Stop()
	}
}

func appendLiveRow[T any](rows *[]T, data []byte) error {
	var row T
	if err := json.Unmarshal(data, &row); err != nil {
		return err
	}
	*rows = append(*rows, row)
	return nil
}

func readLiveRows(bucket *bolt.Bucket, instance string, inv *inventory) error {
	if instance == "" || bucket == nil || string(bucket.Get([]byte("instance"))) != instance {
		return nil
	}
	return bucket.ForEach(func(key, data []byte) error {
		kind, _, _ := strings.Cut(string(key), "/")
		switch kind {
		case "host":
			return json.Unmarshal(data, &inv.LocalHost)
		case "local":
			return appendLiveRow(&inv.LocalShells, data)
		case "shell":
			return appendLiveRow(&inv.Shells, data)
		case "resource":
			return appendLiveRow(&inv.Resources, data)
		case "peer":
			return appendLiveRow(&inv.Peers, data)
		}
		return nil
	})
}

func readPublishedInventory() (inventory, error) {
	inv := emptyInventory()
	root, err := statePath("")
	if err != nil {
		return inv, err
	}
	record, err := daemonRecordWithin(0)
	if err != nil && !os.IsNotExist(err) {
		return inv, err
	}
	return loadInventory(root, record.UID)
}

func loadInventory(root, instance string) (inventory, error) {
	inv := emptyInventory()
	var observed []participatingShell
	err := historyTransaction(root, false, func(tx *bolt.Tx) error {
		if err := readLiveRows(tx.Bucket(liveTasksBucket), instance, &inv); err != nil {
			return err
		}
		inv.Peers = excludeLocalResourcePeers(inv.Peers, inv.Resources)
		var err error
		observed, err = participatingShells(tx)
		return err
	})
	if err != nil {
		return mergeInventory(inv, nil), fmt.Errorf("read task inventory: %w", err)
	}
	return mergeInventory(inv, observed), nil
}
