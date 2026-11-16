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

	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/termui"
)

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	Identity, Parent, Owner, Lifetime, Host, Name, State string
	Current, Cached                                      bool
	MachineID                                            string    `json:",omitempty"`
	CheckedAt                                            time.Time `json:",omitempty"`
	OwnerState                                           string    `json:",omitempty"`
}

func writeShellTree(out io.Writer, nodes []shellTreeNode, color bool) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, "No known shells.")
		return
	}
	nodes = shellTreeHostLabels(nodes)
	now := time.Now()
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
	// Partition only ownership roots by host; cross-host children keep their owner.
	groups := make(map[string][]int)
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
	addRoot := func(i int) {
		groups[nodes[i].Host] = append(groups[nodes[i].Host], i)
		visit(i)
	}
	for _, i := range children[-1] {
		addRoot(i)
	}
	// Incomplete or cyclic metadata must not hide shells.
	for i := range nodes {
		if !seen[i] {
			addRoot(i)
		}
	}
	hosts := make([]string, 0, len(groups))
	for host := range groups {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	type treeRow struct {
		prefix, label, host, state, checked string
		header, current, warning            bool
	}
	var rows []treeRow
	seen = make(map[int]bool)
	for _, host := range hosts {
		members := make(map[int]bool)
		var inspect func(int)
		inspect = func(i int) {
			if members[i] || seen[i] {
				return
			}
			members[i] = true
			for _, c := range children[i] {
				inspect(c)
			}
		}
		for _, i := range groups[host] {
			inspect(i)
		}
		attached := false
		for i := range members {
			attached = attached || nodes[i].State == "attached"
		}
		var defaults []string
		if attached {
			defaults = append(defaults, "attached")
		}
		hostLabel := cleanText(host)
		if hostLabel == "" {
			hostLabel = "unknown"
		}
		rows = append(rows, treeRow{label: "HOST  " + hostLabel, state: strings.Join(defaults, " · "), header: true, checked: "CHECKED"}, treeRow{prefix: "│"})
		var draw func(int, string, string, bool)
		draw = func(i int, prefix, parentHost string, last bool) {
			if seen[i] {
				return
			}
			seen[i] = true
			n := nodes[i]
			branch, next := "├─ ", "│  "
			if last {
				branch, next = "└─ ", "   "
			}
			name := n.Name
			if n.Lifetime != "" {
				name = shellTaskName(name, n.Lifetime != "persistent")
			}
			r := treeRow{prefix: prefix + branch, label: cleanText(name), current: n.Current, checked: shellCheckedAge(n.CheckedAt, now)}
			if !n.Cached {
				r.checked = "now"
			}
			if n.Host != parentHost {
				r.host = "  @" + cleanText(n.Host)
				if n.Host == "" {
					r.host += "unknown"
				}
			}
			var state []string
			if n.State != "attached" {
				value := cleanText(n.State)
				if value == "" {
					value = "unknown"
				}
				state = append(state, value)
				r.warning = n.State != "detached" && n.State != "exited"
			}
			if _, known := byID[n.Owner]; !known && (n.Lifetime == "parent" || n.Lifetime == "heartbeat") {
				switch n.OwnerState {
				case "external": // An unmanaged launch context is normal, not missing metadata.
				case "ended":
					state = append(state, "owner ended")
				default:
					state = append(state, "owner?")
					r.warning = true
				}
			}
			if n.Parent != "" && n.Parent != n.Owner {
				source := "unknown"
				if p, ok := byID[n.Parent]; ok {
					source = nodes[p].Host + " / " + nodes[p].Name
				}
				state = append(state, "attach via "+cleanText(source))
			}
			r.state = strings.Join(state, " · ")
			rows = append(rows, r)
			for k, c := range children[i] {
				draw(c, prefix+next, n.Host, k == len(children[i])-1)
			}
		}
		for k, i := range groups[host] {
			draw(i, "", host, k == len(groups[host])-1)
		}
		rows = append(rows, treeRow{})
	}
	width, stateWidth, checkedWidth := 0, 0, len("CHECKED")
	for _, r := range rows {
		state := r.state
		if r.current {
			if state != "" {
				state = "← current · " + state
			} else {
				state = "← current"
			}
		}
		stateWidth = max(stateWidth, (readline.Runes{}).WidthAll([]rune(state)))
		checkedWidth = max(checkedWidth, len(r.checked))
		if n := (readline.Runes{}).WidthAll([]rune(r.prefix + r.label + r.host)); n > width {
			width = n
		}
	}
	for _, r := range rows[:len(rows)-1] {
		label := r.label
		if r.header && color {
			label = "\x1b[1m" + label + "\x1b[22m"
		}
		if r.current {
			label = termui.Format(color, termui.Success, "%s", label)
		}
		fmt.Fprint(out, termui.Format(color, "90", "%s", r.prefix), label, termui.Format(color, "90", "%s", r.host))
		if r.state != "" || r.current {
			fmt.Fprint(out, strings.Repeat(" ", width-(readline.Runes{}).WidthAll([]rune(r.prefix+r.label+r.host))+2))
			if r.current {
				fmt.Fprint(out, termui.Format(color, termui.Success, "← current"))
				if r.state != "" {
					fmt.Fprint(out, " · ")
				}
			}
			level := termui.Color("90")
			if r.warning {
				level = termui.Warning
			}
			fmt.Fprint(out, termui.Format(color, level, "%s", r.state))
		}
		if r.checked != "" {
			state := r.state
			if r.current {
				if state != "" {
					state = "← current · " + state
				} else {
					state = "← current"
				}
			}
			padding := stateWidth - (readline.Runes{}).WidthAll([]rune(state)) + 2
			if r.state == "" && !r.current {
				padding += width - (readline.Runes{}).WidthAll([]rune(r.prefix+r.label+r.host)) + 2
			}
			fmt.Fprint(out, strings.Repeat(" ", padding+checkedWidth-len(r.checked)), termui.Format(color, "90", "%s", r.checked))
		}
		fmt.Fprintln(out)
	}
}

func runPSTree(args []string) (int, error) {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return 0, showHelp([]string{"pstree"}, os.Stdout)
		}
	}
	parsed, err := parseCLI(args, "pstree")
	if err != nil {
		return 2, err
	}
	o, err := shellOptions("list", parsed)
	if err != nil {
		return 2, err
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
			if n.Cached && endedShellState(nodes[i].State) {
				return
			}
			n.Owner, n.Parent, n.OwnerState = nodes[i].Owner, nodes[i].Parent, nodes[i].OwnerState
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
		merge(shellTreeNode{Identity: s.identity, Host: hostname, MachineID: machine, CheckedAt: checked, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false), Cached: s.State == "unavailable"})
	}
	for _, s := range inv.Shells {

		host := s.Host
		if host == "" {
			host = inv.Host
		}
		id := "remote:" + s.Instance
		if s.Instance == "" {
			id = host + ":" + s.Name
		}
		machine, hostname := s.MachineID, s.Hostname
		if !s.Historical {
			machine, hostname = inv.MachineID, inv.Hostname
		}
		if hostname != "" {
			host = hostname
		}
		checked := s.ConfirmedAt
		if !s.Historical {
			checked = now
		}
		merge(shellTreeNode{Identity: id, Host: host, MachineID: machine, CheckedAt: checked, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true), Cached: s.Historical})
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
	dir, err := stateDir("shell-tree")
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
	path, err := shellTreePath(id)
	if err != nil {
		return nil
	}
	return readShellTree(path)
}
func saveShellTree(id, name string, chain []shellTreeNode, state string, lifetime ...string) {
	if id == "" {
		return
	}
	path, err := shellTreePath(id)
	if err != nil {
		return
	}
	previous := readShellTree(path)
	machine, host := localMachineIdentity()
	self := shellTreeNode{Identity: id, Host: host, MachineID: machine, CheckedAt: time.Now(), Name: name, State: state}
	if len(previous) > 0 && previous[len(previous)-1].Identity == id {
		old := previous[len(previous)-1]
		self.Owner, self.Lifetime, self.OwnerState = old.Owner, old.Lifetime, old.OwnerState
	}
	chain = append([]shellTreeNode(nil), chain...)
	seen := map[string]bool{id: true}
	valid := state == "attached" && len(chain) < 64
	for _, n := range chain {
		if n.Identity == "" || seen[n.Identity] || len(n.Identity) > 256 || len(n.Parent) > 256 || len(n.Owner) > 256 || len(n.Host) > 256 || (n.MachineID != "" && !validMachineID(n.MachineID)) || len(n.Name) > 256 || len(n.State) > 256 || !validShellLifetime(n.Lifetime) {
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
		_ = writeStateFile(path, data)
	}
}

func knownShellTrees() []shellTreeNode {
	dir, err := stateDir("shell-tree")
	if err != nil {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	nodes := map[string]shellTreeNode{}
	// Locally owned records supersede copies carried by descendants.
	for _, path := range paths {
		for _, n := range readShellTree(path) {
			if old, ok := nodes[n.Identity]; !ok || (!endedShellState(old.State) && (old.Cached || endedShellState(n.State))) {
				nodes[n.Identity] = n
			}
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
	path, err := shellTreePath(id)
	if err != nil {
		return nil
	}
	return readShellTree(path)
}
func rememberShellTree(chain []shellTreeNode) {
	if len(chain) == 0 || len(chain) > 64 {
		return
	}
	chain = append([]shellTreeNode(nil), chain...)
	for i := range chain {
		chain[i].Cached = true
		chain[i].Current = false
	}
	path, err := shellTreePath(chain[len(chain)-1].Identity)
	if err == nil {
		existing := readShellTree(path)
		// A local daemon owns its record; the client must not replace it with a cache.
		if len(existing) > 0 && (!existing[len(existing)-1].Cached || endedShellState(existing[len(existing)-1].State)) {
			return
		}
		_ = writeStateJSON(path, chain)
	}
}

func renameShellTree(id, name string) {
	chain := shellTreeFor(id)
	if len(chain) == 0 {
		return
	}
	chain[len(chain)-1].Name = name
	path, err := shellTreePath(id)
	if err == nil {
		_ = writeStateJSON(path, chain)
	}
}

func recordRemoteShellTreeState(s shellSnapshot) error {
	id := "remote:" + s.Instance
	if s.Instance == "" {
		id = s.Host + ":" + s.Name
	}
	path, err := shellTreePath(id)
	if err != nil {
		return err
	}
	chain := readShellTree(path)
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
	return writeStateJSON(path, []shellTreeNode{self})
}

// Names are presentation only. Known machine IDs unify aliases; legacy records
// without an ID retain their names rather than guessing identity from similarity.
func shellTreeHostLabels(nodes []shellTreeNode) []shellTreeNode {
	nodes = append([]shellTreeNode(nil), nodes...)
	names := map[string]string{}
	checked := map[string]time.Time{}
	keys := make([]string, len(nodes))
	for i, n := range nodes {
		key := "name:" + n.Host
		if validMachineID(n.MachineID) {
			key = "id:" + n.MachineID
		}
		keys[i] = key
		name := cleanText(n.Host)
		if name == "" {
			name = "unknown"
		}
		if old, ok := names[key]; !ok || n.CheckedAt.After(checked[key]) || (n.CheckedAt.Equal(checked[key]) && name < old) {
			names[key], checked[key] = name, n.CheckedAt
		}
	}
	counts, used := map[string]int{}, map[string]bool{}
	var order []string
	for key, name := range names {
		counts[name]++
		used[name] = true
		order = append(order, key)
	}
	sort.Strings(order)
	for _, key := range order {
		base := names[key]
		if counts[base] < 2 {
			continue
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		for size := 4; size <= len(hash); size++ {
			name := base + "-" + hash[:size]
			if !used[name] {
				names[key], used[name] = name, true
				break
			}
		}
	}
	for i := range nodes {
		nodes[i].Host = names[keys[i]]
	}
	return nodes
}
