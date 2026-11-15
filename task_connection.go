package main

import (
	"fmt"
	"os"
	"time"

	"github.com/xtaci/smux"
)

func showConnectedTasks(control *smux.Stream, o options) error {
	if err := streamMessage(control, message{Type: "inventory"}); err != nil {
		return err
	}
	_ = control.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer control.SetReadDeadline(time.Time{})
	inv := emptyInventory()
	inv.Host, inv.RemoteQueried = o.host, true
	for count := 0; ; count++ {
		if count > 10000 {
			return fmt.Errorf("too many tasks")
		}
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		if m.Inventory == nil || (m.Type != "inventory_row" && m.Type != "inventory_done") {
			return fmt.Errorf("invalid task inventory")
		}
		inv.Shells = append(inv.Shells, m.Inventory.Shells...)
		inv.Resources = append(inv.Resources, m.Inventory.Resources...)
		inv.Peers = append(inv.Peers, m.Inventory.Peers...)
		inv.Errors = append(inv.Errors, m.Inventory.Errors...)
		if m.Type == "inventory_done" {
			break
		}
	}
	if err := cacheRemoteShells(o.host, inv); err != nil {
		return err
	}
	shells := inv.Shells[:0]
	for _, s := range inv.Shells {
		if s.Instance != o.instance {
			shells = append(shells, s)
		}
	}
	inv.Shells = shells
	records, err := collectLocalResources(o.host)
	if err != nil {
		return err
	}
	inv.Resources = append(inv.Resources, inspectResources(records)...)
	// A local resource may also advertise itself as a peer on this host.
	peers := inv.Peers[:0]
	for _, p := range inv.Peers {
		duplicate := false
		for _, r := range records {
			if p.Manager == taskManagerLabel() && p.ID == r.Kind+"/"+r.Name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			peers = append(peers, p)
		}
	}
	inv.Peers = peers
	if err := numberInventoryShells(&inv); err != nil {
		return err
	}
	if err := numberInventoryResources(&inv); err != nil {
		return err
	}
	if err := numberInventoryPeers(&inv); err != nil {
		return err
	}
	if len(inv.Shells)+len(inv.Resources)+len(inv.Peers) > 0 {
		fmt.Fprintln(os.Stderr, "Background tasks:")
		writeTaskInventory(os.Stderr, inv, false)
	}
	for _, err := range inv.Errors {
		fmt.Fprintln(os.Stderr, "Task list: "+cleanText(err))
	}
	return nil
}
