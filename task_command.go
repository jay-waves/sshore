package main

import (
	"fmt"
	"strconv"
)

// A bare number denotes a task ID; named remote destinations remain available
// with the normal HOST NAME form. Resolution never falls back to a name.
func runTaskID(args []string) (int, bool, error) {
	if len(args) < 2 {
		return 0, false, nil
	}
	switch args[0] {
	case "attach", "cancel", "review", "rename":
	default:
		return 0, false, nil
	}
	id, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return 0, false, nil
	}
	if id == 0 {
		return 2, true, usage("task IDs start at 1")
	}
	if args[0] == "rename" {
		if len(args) != 3 {
			return 2, true, usage("rename ID requires NEW")
		}
	} else if len(args) != 2 {
		return 0, false, nil
	}
	rows, err := localShells()
	if err != nil {
		return 1, true, err
	}
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		if args[0] == "review" {
			return 2, true, usage("task %d is a shell, not a review", id)
		}
		o := options{local: true, host: "local", session: row.Name, command: args[0], taskIdentity: row.identity}
		if args[0] == "rename" {
			o.newName = args[2]
		}
		code, err := runLocalCommand(o)
		return code, true, err
	}
	if code, found, err := runResourceTaskID(id, args); found {
		return code, true, err
	}
	if code, found, err := runRemoteShellID(id, args); found {
		return code, true, err
	}
	peers, err := collectPeerViews()
	if err != nil {
		return 1, true, err
	}
	inv := inventory{Peers: peers}
	if err := numberInventoryPeers(&inv); err != nil {
		return 1, true, err
	}
	for _, p := range inv.Peers {
		if p.TaskID == id {
			return 2, true, usage("task %d is read-only here; manage it on %s", id, cleanText(p.Manager))
		}
	}
	return 1, true, fmt.Errorf("task %d is unavailable; refresh with sshore list", id)
}
