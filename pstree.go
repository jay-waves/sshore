package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/x/ansi"

	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/termui"
)

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	OwnerProcess                                         *spawnProcessView `json:",omitempty"`
	Identity, Parent, Owner, Lifetime, Host, Name, State string
	Cached                                               bool
	ShortID, Kind                                        string    `json:",omitempty"`
	SourceHost, SourceMachineID                          string    `json:",omitempty"`
	Remote, Direction                                    string    `json:"-"`
	MachineID                                            string    `json:",omitempty"`
	CheckedAt                                            time.Time `json:",omitempty"`
}

// Display context only: this process is never registered or adopted as a task.
type spawnProcessView struct {
	PID                          int
	Start, Name, MachineID, Host string
}

func validSpawnProcessView(p *spawnProcessView) bool {
	return p != nil && p.PID > 1 && p.Start != "" && len(p.Start) <= 128 && len(p.Name) <= 256 && len(p.Host) <= 256 && validMachineID(p.MachineID)
}

func describeSpawnOwner(owner processIdentity) *spawnProcessView {
	if owner.PID <= 1 || owner.Start == "" {
		return nil
	}
	machine, host := localMachineIdentity()
	p := &spawnProcessView{PID: owner.PID, Start: owner.Start, MachineID: machine, Host: host}
	p.Name = proc.VerifiedName(p.PID, p.Start)
	return p
}

func externalSpawnNode(id, machine string, p *spawnProcessView) shellTreeNode {
	name, state := "unavailable", "unavailable"
	if p.MachineID == machine {
		if current := proc.VerifiedName(p.PID, p.Start); current != "" {
			name, state = current, "active"
		}
	} else {
		// Never query a foreign PID in the local process namespace.
		if p.Name != "" {
			name = p.Name
		}
		name += " on " + p.Host
	}
	return shellTreeNode{Identity: id, Kind: "external", Name: name, State: state, OwnerProcess: p}
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

func writeShellTree(out io.Writer, rows []taskTreeRow, current string, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No known tasks.")
		return
	}
	branchStyle := lipgloss.NewStyle().PaddingRight(1)
	if color {
		branchStyle = branchStyle.Foreground(ansi.BrightBlack)
	}
	trees := make([]*tree.Tree, len(rows))
	var roots []*tree.Tree
	for i, row := range rows {
		selected := current != "" && row.Identity == current && row.Kind != "external"
		t := tree.Root(shellTreeLabel(row, selected, color)).Enumerator(func(children tree.Children, index int) string {
			return "│\n" + tree.RoundedEnumerator(children, index)
		}).
			EnumeratorStyle(branchStyle).IndenterStyle(branchStyle).
			ItemStyle(lipgloss.NewStyle().PaddingTop(1))
		trees[i] = t
		if row.Owner > 0 && row.Owner <= i {
			trees[row.Owner-1].Child(t)
		} else {
			roots = append(roots, t)
		}
	}
	for i, root := range roots {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, root)
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool) string {
	label := cleanText(n.Kind) + "/" + taskDisplayID(n.Code, n.Name)
	if n.Kind == "external" && n.PID > 0 {
		label = fmt.Sprintf("%s (pid %d)", cleanText(n.Name), n.PID)
	}
	var out strings.Builder
	fmt.Fprint(&out, termui.Render(color && current, termui.Accent, label))
	if n.State == "unavailable" {
		fmt.Fprint(&out, termui.Render(color, termui.Warning, " ?"))
	}
	if n.Remote != "" {
		fmt.Fprint(&out, termui.Render(color, termui.Muted, "  "+cleanText(n.Direction)+" "+cleanText(n.Remote)))
	}
	if current {
		fmt.Fprint(&out, termui.Render(color, termui.Success, "  *current"))
	}
	return out.String()
}

func readTaskTree() ([]taskTreeRow, error) {
	if activity := accountActivity.Load(); activity != nil {
		return activity.taskTreeSnapshot(context.Background())
	}
	reply, _, err := readTaskInventory(taskInventoryRequest{Detail: "tree"})
	if reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply.Tree, err
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
	rows, err := readTaskTree()
	writeShellTree(os.Stdout, rows, shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout))
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func shellIdentityFromEnvironment() string {
	if id := os.Getenv("SSHORE_TREE_ID"); id != "" {
		return id
	}
	return os.Getenv("SSHORE_LOCAL_SESSION")
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

func (m *daemonMetadata) knownShellTrees() ([]shellTreeNode, error) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if err := m.loadTrees(); err != nil {
		return nil, err
	}
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
	return result, nil
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

func rememberShellExit(o options) error {
	detachErr := removeRecentDetach(o)
	if o.local || !validTaskUID(o.instance) {
		return detachErr
	}
	return errors.Join(detachErr, recordRemoteShellExit(o.host, o.session, o.instance), forgetParticipatingShell(o.host, o.instance))
}

func recordRemoteShellExit(host, name, instance string) error {
	if !validTaskUID(instance) {
		return fmt.Errorf("invalid remote shell instance")
	}
	_, err := metadataRPC(metadataRequest{Action: "tree_remote_exit", Host: host, Name: name, Instance: instance})
	return err
}

func (m *daemonMetadata) recordRemoteShellTreeExitLocal(host, name, instance string) error {
	if !validTaskUID(instance) {
		return fmt.Errorf("invalid remote shell instance")
	}
	id := remoteTreeIdentity(instance)
	self, found := m.treeNode(id)
	if !found {
		if name == "" {
			name = taskShortCode(id)
		}
		self = shellTreeNode{Identity: id, Host: host, Name: name, Cached: true}
	}
	self.State, self.Parent = "exited", ""
	self.CheckedAt = time.Now()
	return m.writeTreeLocal([]shellTreeNode{self})
}

func remoteTreeIdentity(instance string) string {
	if validTaskUID(instance) {
		return "task:" + instance
	}
	return "remote:" + instance
}
