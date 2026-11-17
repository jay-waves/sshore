package main

import "fmt"

// Display context only: this process is never registered or adopted as a task.
type spawnProcessView struct {
	PID                          int
	Start, Name, MachineID, Host string
}

func validSpawnProcessView(p *spawnProcessView) bool {
	return p != nil && p.PID > 1 && p.Start != "" && len(p.Start) <= 128 && len(p.Name) <= 256 && len(p.Host) <= 256 && validMachineID(p.MachineID)
}

func describeSpawnOwner(owner spawnOwner) *spawnProcessView {
	if owner.PID <= 1 || owner.Start == "" {
		return nil
	}
	machine, host := localMachineIdentity()
	p := &spawnProcessView{PID: owner.PID, Start: owner.Start, MachineID: machine, Host: host}
	p.Name = verifiedSpawnProcessName(p)
	return p
}

func verifiedSpawnProcessName(p *spawnProcessView) string {
	start, err := spawnProcessIdentity(p.PID)
	if err != nil || start != p.Start {
		return ""
	}
	name, err := spawnProcessName(p.PID)
	if err != nil {
		return ""
	}
	// Identity may change while reading the display name.
	start, err = spawnProcessIdentity(p.PID)
	if err != nil || start != p.Start {
		return ""
	}
	return name
}

func externalSpawnParents(nodes []shellTreeNode, machine string) []shellTreeNode {
	visible := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		visible[n.Identity] = true
	}
	// Copy so augmenting display context cannot change cached ownership metadata.
	result := append([]shellTreeNode(nil), nodes...)
	for i := range nodes {
		n := &result[i]
		p := n.OwnerProcess
		spawn := n.Kind == "spawn" || n.Kind == "" && (n.Lifetime == "parent" || n.Lifetime == "heartbeat")
		if !spawn || visible[n.Owner] || !validSpawnProcessView(p) {
			continue
		}
		id := fmt.Sprintf("external:%s:%d:%s", p.MachineID, p.PID, p.Start)
		n.Owner = id
		if !visible[id] {
			name, state := "unavailable", "unavailable"
			if p.MachineID == machine {
				if current := verifiedSpawnProcessName(p); current != "" {
					name, state = current, "active"
				}
			} else {
				// Never query a foreign PID in the local process namespace.
				if p.Name != "" {
					name = p.Name
				}
				name += " on " + p.Host
			}
			result = append(result, shellTreeNode{Identity: id, Kind: "external", Name: name, State: state, OwnerProcess: p})
			visible[id] = true
		}
	}
	return result
}
