package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

func endedShellState(state string) bool {
	return state == "exited" || state == "renamed"
}

// A snapshot lists parents before children. Owner is the parent's one-based
// row index, or zero for a root. It carries display fields only.
type taskTreeRow struct {
	Identity, Code, Name, Kind, State string
	Remote, Direction                 string `json:",omitempty"`
	PID, Owner                        int    `json:",omitempty"`
}

func (a *daemonActivity) taskTreeSnapshot(ctx context.Context) ([]taskTreeRow, error) {
	// Copy each source independently before probing. Metadata supplies context
	// for registered rows, and leases supply the incoming rows.
	tree, err := a.metadata.knownShellTrees()
	incoming := a.incomingPeers()
	inv := managedInventory(ctx, a.tasks, "health", true, "")
	if inv.Hostname == "" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	inv.Host = inv.Hostname
	inv.ShellTree = tree
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	for _, entry := range incoming {
		inv.Peers = append(inv.Peers, entry.view())
	}
	nodes := inventoryResourceTrees(localShellTreeNodes(inv), inv)
	return normalizeTaskTree(nodes, inv.MachineID), inv.incompleteError()
}

func normalizeTaskTree(nodes []shellTreeNode, machine string) []taskTreeRow {
	nodes, owners := shellTreeOwners(nodes, machine)
	// Each node has at most one owner. Cut a display edge at each cycle so
	// every component has a root, even when cached relationships disagree.
	marks := make([]int, len(nodes))
	for i := range nodes {
		walk := i + 1
		for p := i; p >= 0; p = owners[p] {
			if marks[p] != 0 {
				if marks[p] == walk {
					// Give the same cycle a root regardless of inventory order.
					root := p
					for member := owners[p]; member != p; member = owners[member] {
						if nodes[member].Identity < nodes[root].Identity {
							root = member
						}
					}
					owners[root] = -1
				}
				break
			}
			marks[p] = walk
		}
	}
	// Bucket zero holds roots; the rest hold each owner's children.
	children := make([][]int, len(nodes)+1)
	for i, owner := range owners {
		bucket := owner + 1
		children[bucket] = append(children[bucket], i)
	}
	compare := func(a, b int) int {
		return cmp.Or(cmp.Compare(nodes[a].displayCode(), nodes[b].displayCode()),
			cmp.Compare(nodes[a].Identity, nodes[b].Identity))
	}
	for _, siblings := range children {
		slices.SortStableFunc(siblings, compare)
	}

	rows := make([]taskTreeRow, 0, len(nodes))
	var appendRows func([]int, int)
	appendRows = func(siblings []int, owner int) {
		for _, i := range siblings {
			n := nodes[i]
			row := taskTreeRow{Identity: n.Identity, Code: n.displayCode(), Name: n.Name, Kind: n.Kind, State: n.State, Remote: n.Remote, Direction: n.Direction, Owner: owner}
			if row.Kind == "" {
				row.Kind = shellTaskKind(n.Lifetime == "parent" || n.Lifetime == "heartbeat")
			}
			if row.Kind == "external" && n.OwnerProcess != nil {
				row.PID = n.OwnerProcess.PID
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows))
		}
	}
	appendRows(children[0], 0)
	return rows
}

// Build display edges directly. Synthetic parents share the lookup with
// registered tasks and never change a shell's saved owner.
func shellTreeOwners(nodes []shellTreeNode, machine string) ([]shellTreeNode, []int) {
	byID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		if n.Identity != "" {
			byID[n.Identity] = i
		}
	}
	owners := make([]int, len(nodes))
	for i, n := range nodes {
		owner, found := byID[n.Owner]
		if !found {
			p := n.OwnerProcess
			spawn := n.Kind == "spawn" || n.Kind == "" && (n.Lifetime == "parent" || n.Lifetime == "heartbeat")
			if spawn && validSpawnProcessView(p) {
				id := fmt.Sprintf("external:%s:%d:%s", p.MachineID, p.PID, p.Start)
				owner, found = byID[id]
				if !found {
					owner, found = len(nodes), true
					byID[id] = owner
					nodes = append(nodes, externalSpawnNode(id, machine, p))
					owners = append(owners, -1)
				}
			}
		}
		if !found || owner == i {
			owner = -1
		}
		owners[i] = owner
	}
	return nodes, owners
}

func (n shellTreeNode) displayCode() string {
	if n.ShortID != "" {
		return n.ShortID
	}
	return taskShortCode(n.Identity)
}

// Only registered local shells become rows. Saved snapshots contribute
// relationship metadata and can supply the source of an incoming attachment.
func localShellTreeNodes(inv inventory) []shellTreeNode {
	type treeEntry struct {
		saved      *shellTreeNode
		visibleRow int // One-based index in nodes; zero means no visible row.
	}
	byID := make(map[string]treeEntry, len(inv.ShellTree))
	for i := range inv.ShellTree {
		n := &inv.ShellTree[i]
		byID[n.Identity] = treeEntry{saved: n}
	}
	count := len(inv.LocalShells) + len(inv.Shells)
	nodes := make([]shellTreeNode, 0, count)
	now := time.Now()
	add := func(n shellTreeNode) {
		if endedShellState(n.State) {
			return
		}
		entry := byID[n.Identity]
		var old shellTreeNode
		if entry.saved != nil {
			old = *entry.saved
		}
		n.Cached = n.State == "unavailable"
		if n.Cached {
			if endedShellState(old.State) {
				return
			}
			if n.CheckedAt.IsZero() {
				n.CheckedAt = old.CheckedAt
			}
		} else {
			n.CheckedAt = now
		}
		n.Owner, n.OwnerProcess = old.Owner, old.OwnerProcess
		if n.State == "attached" {
			n.Parent, n.SourceHost, n.SourceMachineID = old.Parent, old.SourceHost, old.SourceMachineID
		}
		if n.MachineID == "" && old.MachineID != "" {
			n.MachineID, n.Host = old.MachineID, old.Host
		}
		if entry.visibleRow != 0 {
			i := entry.visibleRow - 1
			if n.strongerEvidenceThan(nodes[i]) {
				nodes[i] = n
			}
			return
		}
		nodes = append(nodes, n)
		entry.visibleRow = len(nodes)
		byID[n.Identity] = entry
	}
	for _, s := range inv.LocalShells {
		add(shellTreeNode{Identity: s.identity, ShortID: s.ShortID, Host: inv.Hostname, MachineID: inv.MachineID, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false), Remote: s.ConnectedHost, Direction: "->"})
	}
	for _, s := range inv.Shells {
		host := inv.Hostname
		if host == "" {
			host = s.Host
			if host == "" {
				host = inv.Host
			}
		}
		code := s.ShortID
		if code == "" {
			code = taskShortCode(s.identity(inv.Host))
		}
		add(shellTreeNode{Identity: shellViewTreeIdentity(s, inv), ShortID: code, Host: host, MachineID: inv.MachineID, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true)})
	}
	for i := range nodes {
		n := &nodes[i]
		if n.State != "attached" {
			continue
		}
		if n.SourceHost != "" && validMachineID(n.SourceMachineID) {
			if n.SourceMachineID != inv.MachineID {
				n.Remote, n.Direction = n.SourceHost, "<-"
			}
			continue
		}
		// A legacy snapshot may identify its source shell without endpoint metadata.
		source := byID[n.Parent]
		if source.visibleRow != 0 || source.saved == nil {
			continue
		}
		old := source.saved
		if old.Host != "" && !endedShellState(old.State) && old.MachineID != inv.MachineID && (old.MachineID != "" || old.Host != inv.Hostname) {
			n.Remote, n.Direction = old.Host, "<-"
		}
	}
	return nodes
}

func inventoryResourceTrees(nodes []shellTreeNode, inv inventory) []shellTreeNode {
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		direction := "<-"
		if kind == "forward" {
			direction = r.Direction
			if direction != "<-" && direction != "->" {
				direction = "?"
			}
		}
		if r.Worker == "exited" {
			continue
		}
		nodes = append(nodes, shellTreeNode{Identity: r.Identity, ShortID: r.ShortID, Kind: kind, State: r.Worker, Name: name, Remote: r.Host, Direction: direction})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		direction := "?"
		if kind == "mount" || p.Direction == "<-" {
			direction = "->"
		} else if p.Direction == "->" {
			direction = "<-"
		}
		nodes = append(nodes, shellTreeNode{Identity: peerTaskIdentity(p), ShortID: p.ShortID, Kind: kind, Name: name, Remote: p.Manager, Direction: direction})
	}

	return nodes
}

func shellViewTreeIdentity(s shellView, inv inventory) string {
	if s.Instance != "" {
		return remoteTreeIdentity(s.Instance)
	}
	host := s.Host
	if host == "" {
		host = inv.Host
	}
	return host + ":" + s.Name
}
