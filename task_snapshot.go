package main

import (
	"bytes"
	"cmp"
	json "encoding/json/v2"
	"fmt"
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
	Hosted      bool      `json:"-"`
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
	Manager string `json:"manager,omitempty"`
	UID     string `json:"uid"`
	ID      string `json:"id"`
	Detail  string `json:"detail"`
}
type inventory struct {
	LocalHost *hostInfo      `json:"local_host,omitempty"`
	Resources []resourceView `json:"resources"`
	Shells    []shellView    `json:"shells"`
	Peers     []peerView     `json:"peers"`
	Errors    []string       `json:"errors"`
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

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	local := make(map[string]bool, len(resources))
	for _, r := range resources {
		local[canonicalTaskUID(r.Identity)] = true
	}
	return slices.DeleteFunc(peers, func(p peerView) bool { return local[p.UID] })
}

func mergeInventory(inv inventory, observed []participatingShell) inventory {
	// Both renderers prefer a live row over a cached observation of the same task.
	live := make(map[string]bool, len(inv.Shells))
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
func (a *daemonActivity) publishRows(keys ...string) {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	if a.instance == "" {
		return
	}
	if a.pendingRows == nil {
		a.pendingRows = make(map[string]struct{}, len(keys))
	}
	for _, key := range keys {
		a.pendingRows[key] = struct{}{}
	}
	if len(a.pendingRows) == 0 {
		return
	}
	rows := make(map[string]any, len(a.pendingRows))
	for key := range a.pendingRows {
		rows[key] = a.currentRow(key)
	}
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
		for key, row := range rows {
			if row == nil {
				if err := bucket.Delete([]byte(key)); err != nil {
					return err
				}
				continue
			}
			data, err := json.Marshal(row)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		a.pendingRows = nil
		if a.inventoryRetry != nil {
			a.inventoryRetry.Stop()
			a.inventoryRetry = nil
		}
	} else if a.inventoryRetry == nil {
		fmt.Fprintf(a.tasks.log, "publish task inventory: %v\n", err)
		a.inventoryRetry = time.AfterFunc(time.Second, func() { a.publishRows() })
	} else {
		a.inventoryRetry.Reset(time.Second)
	}
}

type liveTaskRow struct {
	Shell    *shellView    `json:"shell,omitempty"`
	Hosted   bool          `json:"hosted,omitempty"`
	Resource *resourceView `json:"resource,omitempty"`
}

func (a *daemonActivity) publishTask(tasks ...daemonTask) {
	keys := make([]string, 0, len(tasks))
	for _, task := range tasks {
		switch t := task.(type) {
		case *tmuxTask:
			keys = append(keys, t.record.UID)
		case *daemonResourceTask:
			keys = append(keys, t.record.UID)
		}
	}
	a.publishRows(keys...)
}

// Resolve each key again on retry: a lease can close or a task can be replaced.
func (a *daemonActivity) currentRow(key string) any {
	if key == "host" {
		return a.inventoryHost
	}
	if peer, ok := strings.CutPrefix(key, "peer/"); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		var newest *activityLease
		for l := range a.leases {
			if l.peer != nil && l.peer.Key == peer && (newest == nil || l.order > newest.order) {
				newest = l
			}
		}
		if newest == nil {
			return nil
		}
		p := newest.peer
		return peerView{Manager: p.Manager, UID: p.UID, ID: p.ID, Detail: p.Detail}
	}
	switch t := a.tasks.lookup(key).(type) {
	case *tmuxTask:
		t.mu.Lock()
		defer t.mu.Unlock()
		view := t.snapshotLocked()
		if !t.expired() && view.State != taskExited {
			return liveTaskRow{Shell: &view, Hosted: t.hosted}
		}
	case *daemonResourceTask:
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.expired() {
			view := viewRecord(t.record)
			return liveTaskRow{Resource: &view}
		}
	}
	return nil
}

func (a *daemonActivity) closeInventory() {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	a.instance = ""
	if a.inventoryRetry != nil {
		a.inventoryRetry.Stop()
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

func currentLiveBucket(tx *bolt.Tx, instance string) *bolt.Bucket {
	bucket := tx.Bucket(liveTasksBucket)
	if instance == "" || bucket == nil || string(bucket.Get([]byte("instance"))) != instance {
		return nil
	}
	return bucket
}

func (inv *inventory) readLiveRow(key, data []byte) error {
	switch {
	case bytes.Equal(key, []byte("host")):
		return json.Unmarshal(data, &inv.LocalHost)
	case bytes.HasPrefix(key, []byte("peer/")):
		return appendLiveRow(&inv.Peers, data)
	case validTaskUID(string(key)):
		var row liveTaskRow
		if err := json.Unmarshal(data, &row); err != nil {
			return err
		}
		switch {
		case row.Shell != nil:
			row.Shell.Hosted = row.Hosted
			inv.Shells = append(inv.Shells, *row.Shell)
		case row.Resource != nil:
			inv.Resources = append(inv.Resources, *row.Resource)
		default:
			return fmt.Errorf("invalid live task record %s", key)
		}
	}
	return nil
}

func (inv *inventory) readPrefix(bucket *bolt.Bucket, prefix []byte) error {
	cursor := bucket.Cursor()
	for key, data := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, data = cursor.Next() {
		if err := inv.readLiveRow(key, data); err != nil {
			return err
		}
	}
	return nil
}

// The setup response advertises only hosted shells owned by the current daemon.
func readHostedShells() ([]shellView, error) {
	record, err := existingDaemonRecord()
	if err != nil {
		return nil, err
	}
	var rows []shellView
	err = historyTransaction(record.Directory, false, func(tx *bolt.Tx) error {
		bucket := currentLiveBucket(tx, record.UID)
		if bucket == nil {
			return nil
		}
		var inv inventory
		if err := bucket.ForEach(inv.readLiveRow); err != nil {
			return err
		}
		for _, shell := range inv.Shells {
			if shell.Hosted {
				rows = append(rows, shell)
			}
		}

		return nil
	})
	return rows, err
}

func readPublishedInventory() (inventory, error) {
	root, instance, err := accountDatabaseLocation(0)
	if err != nil {
		return emptyInventory(), err
	}
	return loadInventory(root, instance)
}

func loadInventory(root, instance string) (inventory, error) {
	inv := emptyInventory()
	var observed []participatingShell
	err := historyTransaction(root, false, func(tx *bolt.Tx) error {
		if bucket := currentLiveBucket(tx, instance); bucket != nil {
			if err := bucket.ForEach(inv.readLiveRow); err != nil {
				return err
			}
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
