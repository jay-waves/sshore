package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

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
