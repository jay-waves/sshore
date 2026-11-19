package main

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
)

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	OwnerProcess                                         *spawnProcessView `json:",omitempty"`
	Identity, Parent, Owner, Lifetime, Host, Name, State string
	Current                                              bool `json:"-"`
	Cached                                               bool
	ShortID, Kind                                        string    `json:",omitempty"`
	SourceHost, SourceMachineID                          string    `json:",omitempty"`
	Remote, Direction                                    string    `json:"-"`
	MachineID                                            string    `json:",omitempty"`
	CheckedAt                                            time.Time `json:",omitempty"`
}

func (n shellTreeNode) clone() shellTreeNode {
	if n.OwnerProcess != nil {
		owner := *n.OwnerProcess
		n.OwnerProcess = &owner
	}
	return n
}

func cloneShellTree(chain []shellTreeNode) []shellTreeNode {
	cloned := slices.Clone(chain)
	for i := range cloned {
		cloned[i] = cloned[i].clone()
	}
	return cloned
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

func writeShellTree(out io.Writer, nodes []shellTreeNode, machine string, color bool) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, "No known tasks.")
		return
	}
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
	clear(marks)
	for i, owner := range owners {
		bucket := owner + 1
		children[bucket] = append(children[bucket], i)
		if !nodes[i].Current {
			continue
		}
		for p := i; p >= 0 && marks[p] == 0; p = owners[p] {
			marks[p] = 1
		}
	}
	compare := func(a, b int) int {
		if marks[a] != marks[b] {
			return cmp.Compare(marks[b], marks[a])
		}
		uncertainA := nodes[a].CheckedAt.IsZero() || nodes[a].State == "unavailable"
		uncertainB := nodes[b].CheckedAt.IsZero() || nodes[b].State == "unavailable"
		if uncertainA != uncertainB {
			if uncertainA {
				return 1
			}
			return -1
		}
		return cmp.Or(nodes[b].CheckedAt.Compare(nodes[a].CheckedAt),
			cmp.Compare(nodes[a].Kind, nodes[b].Kind),
			cmp.Compare(nodes[a].Name, nodes[b].Name),
			cmp.Compare(nodes[a].Identity, nodes[b].Identity))
	}
	for _, siblings := range children {
		slices.SortStableFunc(siblings, compare)
	}

	var prefix []byte
	var draw func([]int)
	draw = func(siblings []int) {
		for index, i := range siblings {
			branch, next := "├─ ", "│  "
			if index == len(siblings)-1 {
				branch, next = "└─ ", "   "
			}
			writeShellTreeRow(out, nodes[i], color, prefix, branch)
			if len(children[i+1]) != 0 {
				depth := len(prefix)
				prefix = append(prefix, next...)
				draw(children[i+1])
				prefix = prefix[:depth]
			}
		}
	}
	draw(children[0])
}

func writeShellTreeRow(out io.Writer, n shellTreeNode, color bool, prefix []byte, branch string) {
	kind := n.Kind
	if kind == "" {
		kind = shellTaskKind(n.Lifetime == "parent" || n.Lifetime == "heartbeat")
	}
	code := n.ShortID
	if code == "" {
		code = taskShortCode(n.Identity)
	}
	label := cleanText(kind) + "/" + taskDisplayID(code, n.Name)
	if kind == "external" && n.OwnerProcess != nil {
		label = fmt.Sprintf("%s (pid %d)", cleanText(n.Name), n.OwnerProcess.PID)
	}
	fmt.Fprint(out, termui.Format(color, "90", "%s%s", prefix, branch), termui.Format(color && n.Current, termui.Success, "%s", label))
	if n.State == "unavailable" {
		fmt.Fprint(out, termui.Format(color, termui.Warning, " ?"))
	}
	if n.Remote != "" {
		fmt.Fprint(out, termui.Format(color, "90", "  %s %s", n.Direction, cleanText(n.Remote)))
	}
	if n.Current {
		marker := "*current"
		if color {
			marker = "\x1b[1m" + marker + "\x1b[22m"
		}
		fmt.Fprint(out, termui.Format(color, termui.Success, "  %s", marker))
	}
	fmt.Fprintln(out)
}

func runPSTree(args []string) (int, error) {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return 0, showHelp([]string{"pstree"}, os.Stdout)
		}
	}
	_, err := parseCLI(args, "pstree")
	if err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	inv, err := localTreeInventory()
	nodes := localShellTreeNodes(inv)
	nodes = inventoryResourceTrees(nodes, inv)
	writeShellTree(os.Stdout, nodes, inv.MachineID, termui.ColorEnabled(os.Stdout))
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func currentShellTree() []shellTreeNode {
	id := os.Getenv("SSHORE_TREE_ID")
	if id == "" {
		return nil
	}
	return shellTreeFor(id)
}

func saveShellTree(id, name string, chain []shellTreeNode, state string, lifetime ...string) {
	saveShellTreeWithProcess(id, name, chain, state, nil, lifetime...)
}
func saveShellTreeWithProcess(id, name string, chain []shellTreeNode, state string, owner *spawnProcessView, lifetime ...string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_save", Key: id, Name: name, Tree: chain, State: state, Lifetime: lifetime, OwnerProcess: owner})
}

func (m *daemonMetadata) saveShellTreeLocal(id, name string, chain []shellTreeNode, state string, ownerProcess *spawnProcessView, lifetime ...string) {
	if id == "" {
		return
	}
	previous := m.treeChain(id)
	machine, host := localMachineIdentity()
	self := shellTreeNode{Identity: id, ShortID: canonicalTaskCode(id), Host: host, MachineID: machine, CheckedAt: time.Now(), Name: name, State: state}
	if len(previous) > 0 {
		old := previous[len(previous)-1]
		self.Owner, self.Lifetime = old.Owner, old.Lifetime
		self.OwnerProcess = old.OwnerProcess
	}
	// A nil node reserves an identity already present in the outgoing chain.
	contextNodes := map[string]*shellTreeNode{id: nil}
	if state != "attached" || len(chain) >= maxShellTreeNodes {
		chain = nil
	}
	for _, n := range chain {
		if _, included := contextNodes[n.Identity]; included || !n.validAttachmentContext() {
			chain = nil
			clear(contextNodes)
			contextNodes[id] = nil
			break
		}
		contextNodes[n.Identity] = nil
	}
	chain = cloneShellTree(chain)
	if len(chain) > 0 {
		self.Parent = chain[len(chain)-1].Identity
	}
	if state == "attached" && len(lifetime) >= 3 && len(lifetime[1]) <= 256 && validMachineID(lifetime[2]) {
		self.SourceHost, self.SourceMachineID = lifetime[1], lifetime[2]
	}

	// Only initial startup establishes lifetime ownership. Attachment metadata
	// remains display-only and can never transfer the actual process/SSH lease.
	if self.Lifetime == "" && len(lifetime) > 0 && validShellLifetime(lifetime[0]) {
		self.Lifetime = lifetime[0]
		if self.Lifetime == "parent" || self.Lifetime == "heartbeat" {
			self.Owner = self.Parent
			if len(previous) == 0 && validSpawnProcessView(ownerProcess) {
				self.OwnerProcess = ownerProcess
				self = self.clone()
			}
		}
	}
	// Keep the original owner's context across detach and attachment elsewhere.
	if _, included := contextNodes[self.Owner]; self.Owner != "" && !included && len(chain) < maxShellTreeNodes-1 {
		for i := len(previous) - 1; i >= 0; i-- {
			candidate := &previous[i]
			if _, included := contextNodes[candidate.Identity]; !included {
				contextNodes[candidate.Identity] = candidate
			}
		}
	}
	for owner := self.Owner; owner != "" && len(chain) < maxShellTreeNodes-1; {
		n := contextNodes[owner]
		if n == nil {
			break
		}
		chain = append(chain, *n)
		delete(contextNodes, owner)
		owner = n.Owner
	}
	for i := range chain {
		chain[i].Current = false
		chain[i].Cached = true
	}
	chain = append(chain, self)
	_ = m.writeTreeLocal(chain)
}

func (n shellTreeNode) validAttachmentContext() bool {
	return n.Identity != "" && len(n.Identity) <= 256 && len(n.Parent) <= 256 &&
		len(n.Owner) <= 256 && len(n.Host) <= 256 && len(n.Name) <= 256 &&
		len(n.State) <= 256 && len(n.SourceHost) <= 256 &&
		(n.MachineID == "" || validMachineID(n.MachineID)) &&
		(n.SourceMachineID == "" || validMachineID(n.SourceMachineID)) &&
		(n.ShortID == "" || isShortTaskID(n.ShortID)) && validShellLifetime(n.Lifetime)
}

func (n shellTreeNode) strongerEvidenceThan(old shellTreeNode) bool {
	ended, oldEnded := endedShellState(n.State), endedShellState(old.State)
	if ended != oldEnded {
		return ended
	}
	if n.Cached != old.Cached {
		return !n.Cached
	}
	return n.CheckedAt.After(old.CheckedAt)
}

func knownShellTrees() []shellTreeNode {
	r, _ := metadataRPC(metadataRequest{Action: "tree_list"})
	return r.Tree
}
func (m *daemonMetadata) knownShellTreesLocal() []shellTreeNode {
	byID := make(map[string]int, len(m.trees))
	result := make([]shellTreeNode, 0, len(m.trees))
	// Visit files in a fixed order so otherwise equal evidence has a stable tie.
	for _, name := range slices.Sorted(maps.Keys(m.trees)) {
		chain := m.trees[name]
		for _, n := range chain {
			if i, ok := byID[n.Identity]; ok {
				if n.strongerEvidenceThan(result[i]) {
					result[i] = n
				}
				continue
			}
			byID[n.Identity] = len(result)
			result = append(result, n)
		}
	}
	for i := range result {
		result[i] = result[i].clone()
		result[i].Cached = true
	}
	return result
}

func shellTreeFor(id string) []shellTreeNode {
	r, _ := metadataRPC(metadataRequest{Action: "tree_get", Key: id})
	return r.Tree
}
func rememberShellTree(chain []shellTreeNode) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_remember", Tree: chain})
}
func (m *daemonMetadata) rememberShellTreeLocal(chain []shellTreeNode) {
	if len(chain) == 0 || len(chain) > maxShellTreeNodes {
		return
	}
	// A local daemon owns its record; the client must not replace it with a cache.
	if own, found := m.treeNode(chain[len(chain)-1].Identity); found && (!own.Cached || endedShellState(own.State)) {
		return
	}
	chain = cloneShellTree(chain)
	for i := range chain {
		chain[i].Cached = true
		chain[i].Current = false
	}
	_ = m.writeTreeLocal(chain)
}

func renameShellTree(id, name string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_rename", Key: id, Name: name})
}
func (m *daemonMetadata) renameShellTreeLocal(id, name string) {
	chain := slices.Clone(m.treeChain(id))
	if len(chain) == 0 {
		return
	}
	chain[len(chain)-1].Name = name
	_ = m.writeTreeLocal(chain)
}

func (m *daemonMetadata) recordRemoteShellTreeStateLocal(s shellSnapshot) error {
	if m.trees == nil {
		if err := m.loadTrees(); err != nil {
			return err
		}
	}
	id := remoteTreeIdentity(s.Instance)
	if s.Instance == "" {
		id = s.Host + ":" + s.Name
	}
	self, found := m.treeNode(id)
	if !found {
		self = shellTreeNode{Identity: id, Host: s.Host, Name: s.Name, Cached: true}
	}
	self.State, self.Parent, self.Current = s.State, "", false
	self.CheckedAt = s.ConfirmedAt
	if s.MachineID != "" {
		self.MachineID, self.Host = s.MachineID, s.Hostname
	}
	// Retain terminal evidence so older copies in descendant snapshots cannot
	// resurrect this instance after its list history expires or is forgotten.
	return m.writeTreeLocal([]shellTreeNode{self})
}

func remoteTreeIdentity(instance string) string {
	if validTaskUID(instance) {
		return "task:" + instance
	}
	return "remote:" + instance
}

// Collect hosted Unix shells and native local terminals; saved remote shells and
// peer advertisements never become local process nodes.
func localTreeInventory() (inventory, error) {
	inv := collectManagedInventory(false, true)
	if inv.Hostname == "" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	inv.ShellTree = knownShellTrees()
	inv.Host = inv.Hostname
	incoming, err := collectIncomingTasks()
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	inv.Peers = incoming
	return inv.finalize()
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
	current := os.Getenv("SSHORE_TREE_ID")
	if current == "" {
		current = os.Getenv("SSHORE_LOCAL_SESSION")
	}
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
		n.Current = current != "" && n.Identity == current
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
		add(shellTreeNode{Identity: s.identity, ShortID: s.ShortID, Host: inv.Hostname, MachineID: inv.MachineID, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false)})
	}
	for _, s := range inv.Shells {
		if s.Historical {
			continue
		}
		host := inv.Hostname
		if host == "" {
			host = s.Host
			if host == "" {
				host = inv.Host
			}
		}
		add(shellTreeNode{Identity: shellViewTreeIdentity(s, inv), ShortID: s.ShortID, Host: host, MachineID: inv.MachineID, CheckedAt: s.ConfirmedAt, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true)})
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
	now := time.Now()
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		direction := "<-"
		if kind == "forward" {
			direction = r.Direction
			if direction != "<-" && direction != "->" {
				direction = "?"
			}
		}
		checked := now
		if r.Worker == "unavailable" {
			checked = time.Time{}
		}
		if r.Worker == "exited" {
			continue
		}
		nodes = append(nodes, shellTreeNode{Identity: r.Identity, ShortID: r.ShortID, Kind: kind, CheckedAt: checked, State: r.Worker, Name: name, Remote: r.Host, Direction: direction})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		direction := "?"
		if kind == "mount" || p.Direction == "<-" {
			direction = "->"
		} else if p.Direction == "->" {
			direction = "<-"
		}
		nodes = append(nodes, shellTreeNode{Identity: peerTaskIdentity(p), ShortID: p.ShortID, Kind: kind, Name: name, Remote: p.Manager, Direction: direction, CheckedAt: now})
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
