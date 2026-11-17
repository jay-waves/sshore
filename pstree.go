package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
)

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	Identity, Parent, Owner, Lifetime, Host, Name, State string
	Current                                              bool `json:"-"`
	Cached                                               bool
	ShortID, Kind                                        string    `json:",omitempty"`
	SourceHost, SourceMachineID                          string    `json:",omitempty"`
	Remote, Direction                                    string    `json:"-"`
	MachineID                                            string    `json:",omitempty"`
	CheckedAt                                            time.Time `json:",omitempty"`
	OwnerState                                           string    `json:",omitempty"`
}

func writeShellTree(out io.Writer, nodes []shellTreeNode, color bool) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, "No known tasks.")
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
	currentBranch := map[int]bool{}
	for i, n := range nodes {
		if !n.Current {
			continue
		}
		for {
			if currentBranch[i] {
				break
			}
			currentBranch[i] = true
			p, ok := byID[nodes[i].Owner]
			if !ok || p == i {
				break
			}
			i = p
		}
	}
	less := func(a, b int) bool {
		if currentBranch[a] != currentBranch[b] {
			return currentBranch[a]
		}
		uncertainA := nodes[a].CheckedAt.IsZero() || nodes[a].State == "unavailable"
		uncertainB := nodes[b].CheckedAt.IsZero() || nodes[b].State == "unavailable"
		if uncertainA != uncertainB {
			return !uncertainA
		}
		if !nodes[a].CheckedAt.Equal(nodes[b].CheckedAt) {
			return nodes[a].CheckedAt.After(nodes[b].CheckedAt)
		}
		if nodes[a].Kind != nodes[b].Kind {
			return nodes[a].Kind < nodes[b].Kind
		}
		if nodes[a].Name != nodes[b].Name {
			return nodes[a].Name < nodes[b].Name
		}
		return nodes[a].Identity < nodes[b].Identity
	}
	for p := range children {
		sort.SliceStable(children[p], func(i, j int) bool { return less(children[p][i], children[p][j]) })
	}

	roots := append([]int(nil), children[-1]...)
	seen := make(map[int]bool)
	var visit func(int)
	visit = func(i int) {
		if seen[i] {
			return
		}
		seen[i] = true
		for _, c := range children[i] {
			visit(c)
		}
	}
	for _, i := range roots {
		visit(i)
	}
	// Cyclic or incomplete display metadata must not hide local tasks.
	for i := range nodes {
		if !seen[i] {
			roots = append(roots, i)
			visit(i)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return less(roots[i], roots[j]) })
	seen = make(map[int]bool)

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
		kind := n.Kind
		if kind == "" {
			kind = shellTaskKind(n.Lifetime == "parent" || n.Lifetime == "heartbeat")
		}
		code := n.ShortID
		if code == "" {
			code = taskShortCode(n.Identity)
		}
		label := cleanText(kind) + "/" + taskDisplayID(code, n.Name)
		fmt.Fprint(out, termui.Format(color, "90", "%s", prefix+branch), termui.Format(color && n.Current, termui.Success, "%s", label))
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
		for k, c := range children[i] {
			draw(c, prefix+next, k == len(children[i])-1)
		}
	}
	for k, i := range roots {
		draw(i, "", k == len(roots)-1)
	}
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
	nodes := inventoryShellTrees(knownShellTrees(), inv)
	nodes = localShellTreeNodes(nodes, inv)
	nodes = inventoryResourceTrees(nodes, inv)
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
			if n.Cached && endedShellState(nodes[i].State) {
				return
			}
			n.Owner, n.Parent, n.OwnerState = nodes[i].Owner, nodes[i].Parent, nodes[i].OwnerState
			n.SourceHost, n.SourceMachineID = nodes[i].SourceHost, nodes[i].SourceMachineID
			if n.MachineID == "" && nodes[i].MachineID != "" {
				n.MachineID, n.Host = nodes[i].MachineID, nodes[i].Host
			}
			if n.CheckedAt.IsZero() && n.Cached {
				n.CheckedAt = nodes[i].CheckedAt
			}
			if n.State != "attached" {
				n.Parent = ""
			}
			nodes[i] = n
		} else {
			byID[n.Identity] = len(nodes)
			nodes = append(nodes, n)
		}
	}
	machine, hostname := localMachineIdentity()
	now := time.Now()
	for _, s := range inv.LocalShells {
		checked := now
		if s.State == "unavailable" {
			checked = time.Time{}
		}
		merge(shellTreeNode{Identity: s.identity, ShortID: s.ShortID, Host: hostname, MachineID: machine, CheckedAt: checked, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false), Cached: s.State == "unavailable"})
	}
	for _, s := range inv.Shells {

		host := s.Host
		if host == "" {
			host = inv.Host
		}
		id := shellViewTreeIdentity(s, inv)
		machine, hostname := s.MachineID, s.Hostname
		if !s.Historical {
			machine, hostname = inv.MachineID, inv.Hostname
		}
		if hostname != "" {
			host = hostname
		}
		checked := s.ConfirmedAt
		if !s.Historical && s.State != "unavailable" {
			checked = now
		}
		merge(shellTreeNode{Identity: id, ShortID: s.ShortID, Host: host, MachineID: machine, CheckedAt: checked, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true), Cached: s.Historical || s.State == "unavailable"})
	}
	current := os.Getenv("SSHORE_TREE_ID")
	if current == "" {
		current = os.Getenv("SSHORE_LOCAL_SESSION")
	}
	ended := map[string]bool{}
	for _, n := range nodes {
		if endedShellState(n.State) {
			ended[n.Identity] = true
		}
	}
	visible := nodes[:0]
	for _, n := range nodes {
		if !endedShellState(n.State) {
			if ended[n.Owner] {
				n.OwnerState = "ended"
			}
			visible = append(visible, n)
		}
	}
	nodes = visible
	for i := range nodes {
		nodes[i].Current = current != "" && nodes[i].Identity == current
	}
	return nodes
}

// Display-only attachment snapshots; never used for authorization or signaling.
func shellTreePath(id string) (string, error) {
	dir, err := statePath("shell-tree")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(id)))), nil
}
func readShellTree(path string) []shellTreeNode {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var nodes []shellTreeNode
	if json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&nodes) != nil || len(nodes) > 64 {
		return nil
	}
	return nodes
}
func currentShellTree() []shellTreeNode {
	id := os.Getenv("SSHORE_TREE_ID")
	if id == "" {
		return nil
	}
	return shellTreeFor(id)
}

func saveShellTree(id, name string, chain []shellTreeNode, state string, lifetime ...string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_save", Key: id, Name: name, Tree: chain, State: state, Lifetime: lifetime})
}
func saveShellTreeLocal(id, name string, chain []shellTreeNode, state string, lifetime ...string) {
	if id == "" {
		return
	}
	previous := readTreeLocal(id)
	machine, host := localMachineIdentity()
	self := shellTreeNode{Identity: id, ShortID: canonicalTaskCode(id), Host: host, MachineID: machine, CheckedAt: time.Now(), Name: name, State: state}
	if len(previous) > 0 && previous[len(previous)-1].Identity == id {
		old := previous[len(previous)-1]
		self.Owner, self.Lifetime, self.OwnerState = old.Owner, old.Lifetime, old.OwnerState
	}
	chain = append([]shellTreeNode(nil), chain...)
	seen := map[string]bool{id: true}
	valid := state == "attached" && len(chain) < 64
	for _, n := range chain {
		if n.Identity == "" || seen[n.Identity] || len(n.Identity) > 256 || len(n.Parent) > 256 || len(n.Owner) > 256 || len(n.Host) > 256 || (n.MachineID != "" && !validMachineID(n.MachineID)) || len(n.Name) > 256 || len(n.State) > 256 || len(n.SourceHost) > 256 || (n.SourceMachineID != "" && !validMachineID(n.SourceMachineID)) || (n.ShortID != "" && !isShortTaskID(n.ShortID)) || !validShellLifetime(n.Lifetime) {
			valid = false
			break
		}
		seen[n.Identity] = true
	}
	if !valid {
		chain = nil
	}
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
			if self.Owner == "" {
				self.OwnerState = "external"
			}
		}
	}
	// Keep the original owner's context across detach and attachment elsewhere.
	seen = map[string]bool{id: true}
	for _, n := range chain {
		seen[n.Identity] = true
	}
	oldByID := map[string]shellTreeNode{}
	for _, n := range previous {
		oldByID[n.Identity] = n
	}
	for owner := self.Owner; owner != "" && !seen[owner] && len(chain) < 63; {
		n, ok := oldByID[owner]
		if !ok {
			break
		}
		chain = append(chain, n)
		seen[owner] = true
		owner = n.Owner
	}
	for i := range chain {
		chain[i].Current = false
		chain[i].Cached = true
	}
	chain = append(chain, self)
	if data, err := json.Marshal(chain); err == nil && len(data) <= 64*1024 {
		_ = writeTreeLocal(id, chain)
	}
}

func knownShellTrees() []shellTreeNode {
	r, _ := metadataRPC(metadataRequest{Action: "tree_list"})
	return r.Tree
}
func knownShellTreesLocal() []shellTreeNode {
	nodes := map[string]shellTreeNode{}
	// Merge evidence before live inventory; file order must not decide state.
	for _, chain := range accountActivity.Load().metadata.trees {
		for _, n := range chain {
			if old, ok := nodes[n.Identity]; ok {
				if endedShellState(old.State) != endedShellState(n.State) {
					if endedShellState(old.State) {
						continue
					}
				} else if old.Cached != n.Cached {
					if !old.Cached {
						continue
					}
				} else if !n.CheckedAt.After(old.CheckedAt) {
					continue
				}
			}
			nodes[n.Identity] = n
		}
	}
	result := make([]shellTreeNode, 0, len(nodes))
	for _, n := range nodes {
		n.Cached = true
		result = append(result, n)
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
func rememberShellTreeLocal(chain []shellTreeNode) {
	if len(chain) == 0 || len(chain) > 64 {
		return
	}
	chain = append([]shellTreeNode(nil), chain...)
	for i := range chain {
		chain[i].Cached = true
		chain[i].Current = false
	}
	_, err := shellTreePath(chain[len(chain)-1].Identity)
	if err == nil {
		existing := readTreeLocal(chain[len(chain)-1].Identity)
		// A local daemon owns its record; the client must not replace it with a cache.
		if len(existing) > 0 && (!existing[len(existing)-1].Cached || endedShellState(existing[len(existing)-1].State)) {
			return
		}
		_ = writeTreeLocal(chain[len(chain)-1].Identity, chain)
	}
}

func renameShellTree(id, name string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_rename", Key: id, Name: name})
}
func renameShellTreeLocal(id, name string) {
	chain := readTreeLocal(id)
	if len(chain) == 0 {
		return
	}
	chain[len(chain)-1].Name = name
	_, err := shellTreePath(id)
	if err == nil {
		_ = writeTreeLocal(chain[len(chain)-1].Identity, chain)
	}
}

func recordRemoteShellTreeState(s shellSnapshot) error {
	_, err := metadataRPC(metadataRequest{Action: "tree_state", Snapshot: &s})
	return err
}
func recordRemoteShellTreeStateLocal(s shellSnapshot) error {
	id := remoteTreeIdentity(s.Instance)
	if s.Instance == "" {
		id = s.Host + ":" + s.Name
	}
	_, err := shellTreePath(id)
	if err != nil {
		return err
	}
	chain := readTreeLocal(id)
	self := shellTreeNode{Identity: id, Host: s.Host, Name: s.Name, Cached: true}
	for _, n := range chain {
		if n.Identity == id {
			self = n
			break
		}
	}
	self.State, self.Parent, self.Current = s.State, "", false
	self.CheckedAt = s.ConfirmedAt
	if s.MachineID != "" {
		self.MachineID, self.Host = s.MachineID, s.Hostname
	}
	// Retain terminal evidence so older copies in descendant snapshots cannot
	// resurrect this instance after its list history expires or is forgotten.
	return writeTreeLocal(id, []shellTreeNode{self})
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
	inv, err := hostedShellInventory(false)
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	local, err := nativeShells()
	inv.LocalShells = local
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	records, err := collectLocalResources("")
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	} else {
		inv.Resources = inspectResources(records)
	}
	inv.MachineID, inv.Hostname = localMachineIdentity()
	inv.Host = inv.Hostname
	incoming, err := collectIncomingTasks()
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	inv.Peers = incoming
	for _, number := range []func(*inventory) error{numberInventoryShells, numberInventoryResources, numberInventoryPeers} {
		if err := number(&inv); err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
		}
	}
	if len(inv.Errors) > 0 {
		return inv, fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return inv, nil
}

func localShellTreeNodes(nodes []shellTreeNode, inv inventory) []shellTreeNode {
	local := map[string]bool{}
	for _, s := range inv.LocalShells {
		local[s.identity] = true
	}
	for _, s := range inv.Shells {
		if !s.Historical && !endedShellState(s.State) {
			local[shellViewTreeIdentity(s, inv)] = true
		}
	}
	byID := map[string]shellTreeNode{}
	for _, n := range nodes {
		byID[n.Identity] = n
	}
	var result []shellTreeNode
	for _, n := range nodes {
		if !local[n.Identity] {
			continue
		}
		if n.State == "attached" {
			if n.SourceHost != "" && validMachineID(n.SourceMachineID) {
				if n.SourceMachineID != inv.MachineID {
					n.Remote, n.Direction = n.SourceHost, "<-"
				}
			} else if source, ok := byID[n.Parent]; ok && !local[n.Parent] && source.Host != "" {
				// Legacy snapshots can identify a source shell without endpoint metadata.
				if source.MachineID != inv.MachineID && (source.MachineID != "" || source.Host != inv.Hostname) {
					n.Remote, n.Direction = source.Host, "<-"
				}
			}
		}
		result = append(result, n)
	}
	return result
}

func inventoryResourceTrees(nodes []shellTreeNode, inv inventory) []shellTreeNode {
	for _, r := range inv.Resources {
		direction := "<-"
		if r.Kind == "review" {
			direction = "<-"
		}
		if r.Kind == "forward" {
			direction = r.Direction
			if direction != "<-" && direction != "->" {
				direction = "?"
			}
		}
		checked := time.Now()
		if r.Worker == "unavailable" {
			checked = time.Time{}
		}
		if r.Worker == "exited" {
			continue
		}
		nodes = append(nodes, shellTreeNode{Identity: r.Identity, ShortID: r.ShortID, Kind: r.Kind, CheckedAt: checked, State: r.Worker, Name: strings.TrimPrefix(r.ID, r.Kind+"/"), Remote: r.Host, Direction: direction})
	}
	for _, p := range inv.Peers {
		name := p.ID
		if strings.HasPrefix(name, "peer/") {
			_, name, _ = strings.Cut(strings.TrimPrefix(name, "peer/"), "/")
		}
		direction := "?"
		if peerKind(name) == "mount" || p.Direction == "<-" {
			direction = "->"
		} else if p.Direction == "->" {
			direction = "<-"
		}
		nodes = append(nodes, shellTreeNode{Identity: peerTaskIdentity(p), ShortID: p.ShortID, Kind: peerKind(name), Name: peerName(name), Remote: p.Manager, Direction: direction, CheckedAt: time.Now()})
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
