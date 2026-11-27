package main

import (
	"bytes"
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

func (snapshot taskInventorySnapshot) nativeShells() []localShellView {
	rows := snapshot.Native
	for i := range rows {
		rows[i].ShortID = taskShortCode(rows[i].Identity)
	}
	sortLocalShells(rows)
	return rows
}

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	local := make(map[string]bool, len(resources))
	for _, r := range resources {
		local[canonicalTaskUID(r.Identity)] = true
	}
	return slices.DeleteFunc(peers, func(p peerView) bool { return local[p.UID] })
}

func (a *daemonActivity) liveInventory() inventory {
	query := taskInventoryRequest{Native: true, Hosted: true, Resources: true}
	snapshot := a.tasks.taskInventory(query)
	inv := emptyInventory()
	inv.LocalHost = a.hostInfo.Load()
	inv.Shells = snapshot.Shells
	inv.LocalShells = snapshot.nativeShells()
	inv.Resources = snapshot.Resources
	inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
	return inv
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

var inventoryBucket = []byte("live-inventory")
var inventoryKey = []byte("account")

type publishedInventory struct {
	Instance  string
	Inventory inventory
}

func (a *daemonActivity) publishInventory() {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	if a.instance == "" {
		return
	}
	inv := a.liveInventory()
	slices.SortFunc(inv.Resources, func(a, b resourceView) int { return strings.Compare(a.Identity, b.Identity) })
	slices.SortFunc(inv.Peers, func(a, b peerView) int { return strings.Compare(a.UID, b.UID) })
	data, err := json.Marshal(publishedInventory{Instance: a.instance, Inventory: inv})
	if err != nil || bytes.Equal(a.published, data) {
		return
	}
	if err := historyTransaction(a.root, true, func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(inventoryBucket)
		if err != nil {
			return err
		}
		return bucket.Put(inventoryKey, data)
	}); err == nil {
		a.published = data
	}
}

func (a *daemonActivity) clearInventory() {
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	a.instance = ""
	if a.published == nil {
		return
	}
	if err := historyTransaction(a.root, true, func(tx *bolt.Tx) error {
		if bucket := tx.Bucket(inventoryBucket); bucket != nil {
			return bucket.Delete(inventoryKey)
		}
		return nil
	}); err != nil {
		fmt.Fprintln(a.tasks.log, "clear task inventory:", err)
	}
}

// Periodic sampling covers changes that do not have a publication boundary,
// including pin expiry and directory reports. Equal snapshots incur no write.
func (a *daemonActivity) refreshInventory(done <-chan struct{}) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			a.publishInventory()
		}
	}
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
	var live publishedInventory
	err := historyTransaction(root, false, func(tx *bolt.Tx) error {
		if tx == nil {
			return nil
		}
		if bucket := tx.Bucket(inventoryBucket); bucket != nil && instance != "" {
			if data := bucket.Get(inventoryKey); data != nil {
				if err := json.Unmarshal(data, &live); err != nil {
					return err
				}
			}
		}
		var err error
		observed, err = participatingShells(tx)
		return err
	})
	if live.Instance != "" && live.Instance == instance {
		inv = live.Inventory
	}
	if err != nil {
		return mergeInventory(inv, nil), fmt.Errorf("read task inventory: %w", err)
	}
	return mergeInventory(inv, observed), nil
}
