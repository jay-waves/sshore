package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/jay-waves/sshore/internal/termui"
)

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	Identity, Parent, Owner, Lifetime, Host, Name, State string
	Current, Cached                                      bool
}

func writeShellTree(out io.Writer, nodes []shellTreeNode, color bool) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, "No known shells.")
		return
	}
	byID := make(map[string]int)
	for i, n := range nodes {
		if n.Identity != "" {
			byID[n.Identity] = i
		}
	}
	children := make(map[int][]int)
	for i, n := range nodes {
		parent := -1
		if p, ok := byID[n.Owner]; ok && p != i {
			parent = p
		}
		children[parent] = append(children[parent], i)
	}
	for p := range children {
		sort.SliceStable(children[p], func(i, j int) bool {
			a, b := nodes[children[p][i]], nodes[children[p][j]]
			if a.Host != b.Host {
				return a.Host < b.Host
			}
			return a.Name < b.Name
		})
	}
	seen := make(map[int]bool)
	var draw func(int, string, bool)
	draw = func(i int, prefix string, last bool) {
		if seen[i] {
			return
		}
		seen[i] = true
		n := nodes[i]
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		level := termui.Info
		state := cleanText(n.State)
		if n.Cached {
			state += " (cached)"
			level = termui.Warning
		}
		name := n.Name
		if n.Lifetime != "" {
			name = shellTaskName(name, n.Lifetime != "persistent")
		}
		label := cleanText(n.Host) + " / " + cleanText(name)
		if n.Lifetime != "" {
			state += " · " + cleanText(n.Lifetime)
		}
		_, ownerKnown := byID[n.Owner]
		if !ownerKnown && (n.Lifetime == "parent" || n.Lifetime == "heartbeat") {
			state += " · owner outside known shells"
		}
		if n.Parent != "" && n.Parent != n.Owner {
			source := "unknown"
			if p, ok := byID[n.Parent]; ok {
				source = nodes[p].Host + " / " + nodes[p].Name
			}
			state += " · attach via " + cleanText(source)
		}
		if n.Current {
			label += " ← current"
			level = termui.Success
		}
		fmt.Fprintf(out, "%s%s%s  %s\n", prefix, branch, termui.Format(color, level, "%s", label), state)
		for k, c := range children[i] {
			draw(c, prefix+next, k == len(children[i])-1)
		}
	}
	fmt.Fprintln(out, "Shells (branches: spawn owner context; attach via: interaction only)")
	for k, i := range children[-1] {
		draw(i, "", k == len(children[-1])-1)
	}
	// Corrupt or incomplete metadata must not hide cyclic components.
	for i := range nodes {
		if !seen[i] {
			draw(i, "", true)
		}
	}
}

func runPSTree(args []string) (int, error) {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return 0, showHelp([]string{"pstree"}, os.Stdout)
		}
	}
	o, err := parseOptions(append([]string{"list"}, args...))
	if err != nil {
		return 2, err
	}
	if o.listJSON || o.listDetails {
		return 2, usage("pstree does not accept --json or --verbose")
	}
	if o.local {
		o.host = ""
	}
	inv, err := collectInventory(o)
	nodes := knownShellTrees()
	if inv.RemoteQueried {
		nodes = inv.ShellTree
	}
	nodes = inventoryShellTrees(nodes, inv)
	writeShellTree(os.Stdout, nodes, termui.ColorEnabled(os.Stdout))
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Live inventory refreshes state, but does not turn cached relationship metadata
// into verified process ancestry. Instance identities avoid duplicate rows.
func inventoryShellTrees(nodes []shellTreeNode, inv inventory) []shellTreeNode {
	byID := map[string]int{}
	for i, n := range nodes {
		byID[n.Identity] = i
	}
	merge := func(n shellTreeNode) {
		if i, ok := byID[n.Identity]; ok {
			n.Owner, n.Parent = nodes[i].Owner, nodes[i].Parent
			if n.State != "attached" {
				n.Parent = ""
			}
			nodes[i] = n
		} else {
			byID[n.Identity] = len(nodes)
			nodes = append(nodes, n)
		}
	}
	for _, s := range inv.LocalShells {
		merge(shellTreeNode{Identity: s.identity, Host: "local", Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false), Cached: s.State == "unavailable"})
	}
	for _, s := range inv.Shells {
		if s.State == "renamed" {
			continue
		}
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		id := "remote:" + s.Instance
		if s.Instance == "" {
			id = host + ":" + s.Name
		}
		merge(shellTreeNode{Identity: id, Host: host, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true), Cached: s.Historical})
	}
	current := os.Getenv("SSHORE_TREE_ID")
	if current == "" {
		current = os.Getenv("SSHORE_LOCAL_SESSION")
	}
	for i := range nodes {
		nodes[i].Current = current != "" && nodes[i].Identity == current
	}
	return nodes
}
