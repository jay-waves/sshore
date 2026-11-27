package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
)

func runPstree(_ cliArguments) (int, error) {
	inv, err := collectInventory()
	writePstree(os.Stdout, inv, shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout))
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func writePstree(out io.Writer, inv inventory, current string, color bool) {
	rows := normalizeTaskTree(taskTreeNodes(inv))
	if len(rows) != 0 || len(inv.Errors) == 0 {
		writeShellTree(out, rows, current, color)
	}
}

func terminalTreeName(program bool, name, command string) string {
	if program && isShortTaskID(name) {
		return command
	}
	return name
}

// A snapshot lists parents before children. Owner is the parent's one-based
// row index, or zero for a root. It carries display fields only.
type taskTreeRow struct {
	Identity, Kind, Code, Name, State, Remote string
	Owner                                     int
}

func normalizeTaskTree(nodes []shellTreeNode) []taskTreeRow {
	owners := shellTreeOwners(nodes)
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

	byHost := make(map[string][]int)
	for _, i := range children[0] {
		host := cmp.Or(nodes[i].Host, localInventoryHost)
		byHost[host] = append(byHost[host], i)
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	slices.SortFunc(hosts, compareInventoryHosts)

	rows := make([]taskTreeRow, 0, len(nodes)+len(hosts))
	var appendRows func([]int, int, string)
	appendRows = func(siblings []int, owner int, parentHost string) {
		for _, i := range siblings {
			n := nodes[i]
			row := taskTreeRow{Identity: n.Identity, Kind: n.Kind, Code: n.displayCode(), Name: n.Name, State: n.State, Owner: owner}
			host := cmp.Or(n.Host, localInventoryHost)
			if host != parentHost {
				row.Remote = hostDisplayLabel(host)
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows), host)
		}
	}
	for _, host := range hosts {
		rows = append(rows, taskTreeRow{Code: hostDisplayLabel(host)})
		appendRows(byHost[host], len(rows), host)
	}
	return rows
}

func shellTreeOwners(nodes []shellTreeNode) []int {
	byID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		if n.Identity != "" {
			byID[n.Identity] = i
		}
	}
	owners := make([]int, len(nodes))
	for i, n := range nodes {
		owner, found := byID[n.Owner]
		if !found || owner == i {
			owner = -1
		}
		owners[i] = owner
	}
	return owners
}

func (n shellTreeNode) displayCode() string {
	if n.ShortID != "" {
		return n.ShortID
	}
	return taskShortCode(n.Identity)
}

// The local registry is authoritative. For other shells, show the newest
// authenticated observation of each UID, even when two SSH aliases reach it.
func taskTreeNodes(inv inventory) []shellTreeNode {
	nodes := make([]shellTreeNode, 0, len(inv.Shells)+len(inv.Resources)+len(inv.Peers))
	visible := make(map[string]int)
	add := func(n shellTreeNode) {
		if taskHiddenFromInventory(n.State) {
			return
		}
		if i, found := visible[n.Identity]; found {
			if !nodes[i].SeenAt.IsZero() && (n.SeenAt.IsZero() || n.SeenAt.After(nodes[i].SeenAt)) {
				nodes[i] = n
			}
			return
		}
		visible[n.Identity] = len(nodes)
		nodes = append(nodes, n)
	}
	for _, s := range inv.Shells {
		n := shellTreeNode{Kind: terminalTaskKind(s.Program), Identity: identityFromUID(s.Instance), ShortID: s.ShortID, Owner: s.Owner, Name: terminalTreeName(s.Program, s.Name, s.Shell), State: s.State}
		if s.Observed {
			n.Host, n.SeenAt = s.Host, s.SeenAt
		}
		add(n)
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		add(shellTreeNode{Kind: kind, Identity: r.Identity, ShortID: r.ShortID, Name: name, State: taskActive})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(p.ID, "/")
		identity := peerTaskIdentity(p)
		add(shellTreeNode{Kind: kind, Identity: identity, ShortID: taskShortCode(identity), Host: incomingInventoryHost, Name: name, State: taskActive})
	}
	return nodes
}

type shellTreeNode struct {
	Kind, Host, Identity, ShortID, Owner, Name, State string
	SeenAt                                            time.Time
}

func writeShellTree(out io.Writer, rows []taskTreeRow, current string, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No tasks.")
		return
	}
	children := make([][]int, len(rows)+1)
	for i, row := range rows {
		owner := row.Owner
		if owner < 0 || owner > i {
			owner = 0
		}
		children[owner] = append(children[owner], i)
	}
	var writeBranch func(int, string, string)
	writeBranch = func(i int, prefix, indent string) {
		row := rows[i]
		fmt.Fprintln(out, prefix+shellTreeLabel(row, current != "" && row.Identity == current, color))
		kids := children[i+1]
		for j, child := range kids {
			prefix, childIndent := termui.TreeBranch(color, indent, j == len(kids)-1)
			writeBranch(child, prefix, childIndent)
		}
	}
	for _, root := range children[0] {
		writeBranch(root, "", "")
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool) string {
	label := taskDisplayID(n.Code, n.Name, current)
	if n.Kind != "" {
		label = cleanText(n.Kind) + "/" + label
	}
	if n.Remote != "" {
		label += " on " + cleanText(n.Remote)
	}

	state := cleanText(taskStateCode(n.State))
	if state == "" {
		return termui.Render(color && current, termui.Accent, label)
	}
	style := termui.Muted
	if n.State == taskUnknown {
		style = termui.Warning
	}
	return termui.Render(color && current, termui.Accent, label) + "  " +
		termui.Render(color, style, state)
}
