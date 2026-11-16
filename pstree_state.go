package main

import (
	"crypto/sha256"
	"sort"
	"time"

	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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
