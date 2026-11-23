package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// History is persisted by the daemon; running clients never coordinate through
// these files. Each domain is loaded once and thereafter served from memory.
type daemonMetadata struct {
	attachments     recentHistory[recentAttachment]
	connections     recentHistory[recentConnection]
	participation   recentHistory[participatingShell]
	treeMu, shellMu sync.Mutex
	treeRoot        string
	trees           map[string][]shellTreeNode
}

// The two histories retain independent orderings and share the same file lifecycle.
const recentHistoryLimit = 32

type recentHistory[T any] struct {
	path     string
	maxBytes int
	rows     []T
}

func (h *recentHistory[T]) load(file string, maxBytes int, validate func(T) error) error {
	return h.loadEntries(file, maxBytes, recentHistoryLimit, validate)
}

func (h *recentHistory[T]) loadEntries(file string, maxBytes, maxEntries int, validate func(T) error) error {
	root, err := statePath("")
	if err != nil {
		return err
	}
	path := filepath.Join(root, "connections", file)
	if path == h.path {
		return nil
	}
	if _, err := prepareStateDir(root, "connections"); err != nil {
		return err
	}
	var rows []T
	if err := readStateJSON(path, maxBytes, &rows); err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(rows) > maxEntries {
		return fmt.Errorf("too many entries in %s", file)
	}
	for _, row := range rows {
		if err := validate(row); err != nil {
			return err
		}
	}
	*h = recentHistory[T]{path: path, maxBytes: maxBytes, rows: rows}
	return nil
}

func (h *recentHistory[T]) promote(row T, sameEntry func(T) bool) error {
	rows := []T{row}
	for _, previous := range h.rows {
		if len(rows) == recentHistoryLimit {
			break
		}
		if !sameEntry(previous) {
			rows = append(rows, previous)
		}
	}
	return h.replace(rows)
}

func (h *recentHistory[T]) replace(rows []T) error {
	root := filepath.Dir(filepath.Dir(h.path))
	if _, err := prepareStateDir(root, "connections"); err != nil {
		return err
	}
	if err := writeBoundedStateJSON(h.path, rows, h.maxBytes); err != nil {
		return err
	}
	h.rows = rows
	return nil
}

type metadataRequest struct {
	OwnerProcess                      *spawnProcessView
	Local, Direct                     bool `json:",omitempty"`
	Action, Key, Host, Name, Instance string
	State                             string
	Tree                              []shellTreeNode
	Lifetime                          []string
	Attachment                        *recentAttachment
	Connection                        *recentConnection
	Participant                       *participatingShell
	Participation                     *participationRefresh
}
type metadataReply struct {
	Target       *selectedTask     `json:",omitempty"`
	Connection   *recentConnection `json:",omitempty"`
	Participants []participatingShell
	Tree         []shellTreeNode
	Error        string
}

func metadataRPC(r metadataRequest) (metadataReply, error) {
	var reply metadataReply
	if !activityAllowed() {
		switch r.Action {
		case "tree_get", "connection_select", "participation_get":
			if _, err := existingDaemonRecord(); os.IsNotExist(err) {
				return reply, nil
			}
		default:
			return reply, fmt.Errorf("daemon activity is disabled in recovery mode")
		}
	}
	if activity := accountActivity.Load(); activity != nil {
		reply = activity.handleMetadata(r)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		record, err := activityDaemon(ctx)
		if err != nil {
			return reply, err
		}
		if err := daemonJSON(ctx, record, "/metadata", r, &reply); err != nil {
			return reply, err
		}
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("%s", reply.Error)
	}
	return reply, nil
}

func (a *daemonActivity) handleMetadata(r metadataRequest) (reply metadataReply) {
	s := &a.metadata
	tree := strings.HasPrefix(r.Action, "tree_")
	if tree {
		s.treeMu.Lock()
		defer s.treeMu.Unlock()
		if err := s.loadTrees(); err != nil {
			reply.Error = err.Error()
			return
		}
	} else {
		s.shellMu.Lock()
		defer s.shellMu.Unlock()
	}
	var err error
	switch r.Action {
	case "attach_by_id":
		reply.Target, err = a.resolveBareAttachID(r.Instance)
	case "attach_recent":
		reply.Target, err = a.selectDetachedAttach(r.Local, r.Host)
	case "attach_detach":
		if r.Attachment != nil {
			err = s.rememberDetachedAttachmentLocal(*r.Attachment)
		}
	case "attach_remove":
		err = s.removeRecentDetachLocal(r.Key)
	case "connection_select":
		reply.Connection, err = s.selectRecentConnection(r.Host, r.Instance, r.Direct)
	case "connection_save":
		if r.Connection != nil {
			err = s.rememberConnectionLocal(*r.Connection)
		}
	case "participation_get":
		reply.Participants, err = s.readParticipatingShellsLocal(r.Host)
	case "participation_observe", "participation_state", "participation_forget", "participation_rename", "participation_refresh":
		err = s.updateParticipation(r)
	case "tree_get":
		reply.Tree = cloneShellTree(s.treeChain(r.Key))
	case "tree_save":
		err = s.saveShellTreeLocal(r.Key, r.Name, r.Tree, r.State, r.OwnerProcess, r.Lifetime...)
	case "tree_remember":
		err = s.rememberShellTreeLocal(r.Tree)
	case "tree_rename":
		s.renameShellTreeLocal(r.Key, r.Name)
	case "tree_remote_exit":
		err = s.recordRemoteShellTreeExitLocal(r.Host, r.Name, r.Instance)
	default:
		err = fmt.Errorf("invalid metadata operation")
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
}
func (s *daemonMetadata) loadTrees() error {
	stateRoot, err := statePath("")
	if err != nil {
		s.trees = nil
		return err
	}
	root := filepath.Join(stateRoot, "shell-tree")
	if s.trees != nil && s.treeRoot == root {
		return nil
	}
	s.trees = nil
	if _, err = prepareStateDir(stateRoot, "shell-tree"); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	trees := make(map[string][]shellTreeNode)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var chain []shellTreeNode
		if err := readStateJSON(filepath.Join(root, entry.Name()), maxShellTreeBytes, &chain); err != nil {
			return err
		}
		if !validSavedShellTree(chain) {
			return fmt.Errorf("invalid shell tree in %s", entry.Name())
		}
		for i := range chain {
			chain[i].Cached = true
		}
		trees[entry.Name()] = chain
	}
	s.trees, s.treeRoot = trees, root
	return nil
}

const (
	maxShellTreeNodes = 64
	maxShellTreeBytes = 64 * 1024
)

func shellTreeFilename(id string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(id)))
}

func validShellTreeState(state string) bool {
	switch state {
	case taskStarting, taskActive, taskDetached, taskUnknown, taskExited:
		return true
	default:
		return false
	}
}

func validSavedShellTree(chain []shellTreeNode) bool {
	return len(chain) > 0 && len(chain) <= maxShellTreeNodes && chain[len(chain)-1].Identity != "" &&
		!slices.ContainsFunc(chain, func(node shellTreeNode) bool { return !validShellTreeState(node.State) })
}

func (s *daemonMetadata) treeChain(id string) []shellTreeNode {
	chain := s.trees[shellTreeFilename(id)]
	if len(chain) != 0 && chain[len(chain)-1].Identity == id {
		return chain
	}
	return nil
}

// The final node owns the record. The caller supplies an owned slice.
// Daemon-owned process display data is read-only and can be shared.
func (s *daemonMetadata) writeTreeLocal(chain []shellTreeNode) error {
	if !validSavedShellTree(chain) {
		return fmt.Errorf("invalid shell tree")
	}
	file := shellTreeFilename(chain[len(chain)-1].Identity)
	path := filepath.Join(s.treeRoot, file)
	data, err := marshalBoundedStateJSON(path, chain, maxShellTreeBytes)
	if err != nil {
		return err
	}
	// Preserve observed ownership in the running daemon if persistence fails.
	s.trees[file] = chain
	return writeStateFile(path, data)
}

// Only the owning node supplies shell-history evidence.
func (s *daemonMetadata) treeNode(id string) (shellTreeNode, bool) {
	chain := s.treeChain(id)
	if len(chain) != 0 {
		return chain[len(chain)-1], true
	}
	return shellTreeNode{}, false
}

type recentConnection struct {
	Hosted                           bool              `json:",omitempty"`
	RouteTarget                      bool              `json:",omitempty"`
	Parent                           *recentConnection `json:",omitempty"`
	RouteToken                       string            `json:",omitempty"`
	Instance                         string            `json:",omitempty"`
	Host, Session, Server, Directory string
	SSH                              []string
}

func (r recentConnection) viaShell() bool { return r.Hosted || r.Parent != nil }

const maxConnectionHops = 64

func (c *recentConnection) clone() *recentConnection {
	if c == nil {
		return nil
	}
	cloned := *c
	for hop := &cloned; hop != nil; hop = hop.Parent {
		hop.SSH = append([]string(nil), hop.SSH...)
		if hop.Parent != nil {
			parent := *hop.Parent
			hop.Parent = &parent
		}
	}
	return &cloned
}

func (m *daemonMetadata) readRecentConnectionsLocal() ([]recentConnection, error) {
	if err := m.connections.load("recent.json", 128*1024, validateRecentConnection); err != nil {
		return nil, fmt.Errorf("cannot read recent connections: %w", err)
	}
	return m.connections.rows, nil
}

func validateRecentConnection(r recentConnection) error {
	for depth, hop := 0, &r; hop != nil; depth, hop = depth+1, hop.Parent {
		if depth >= maxConnectionHops {
			return fmt.Errorf("too many connection hops")
		}
		if hop.Hosted && (hop.Host != "local" || !validTaskUID(hop.Instance) || len(hop.SSH) != 0 || hop.Parent != nil) {
			return fmt.Errorf("invalid hosted shell connection")
		}
		if hop.Parent != nil {
			token := hop.RouteToken
			if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" || hop.Instance == "" || hop.Parent.Instance == "" {
				return fmt.Errorf("invalid saved spawn route")
			}
		} else if hop.RouteToken != "" || hop.RouteTarget {
			return fmt.Errorf("spawn route parent missing")
		}
		if validSSHDestination(hop.Host) != nil || validSession(hop.Session) != nil || hop.Server == "" || strings.ContainsAny(hop.Server, "\x00\r\n") {
			return fmt.Errorf("invalid recent connection")
		}
		if hop.Directory != "" && !filepath.IsAbs(hop.Directory) {
			return fmt.Errorf("invalid recent connection directory")
		}
		if len(hop.SSH) > 0 {
			if _, _, err := parseSSHOptions(hop.SSH); err != nil {
				return err
			}
		}
	}
	return nil
}

// A bare ID resolves among registered local shells and known direct SSH routes.
func (a *daemonActivity) resolveBareAttachID(ref string) (*selectedTask, error) {
	if !isTaskIDPrefix(ref) {
		return nil, fmt.Errorf("invalid task ID")
	}
	records, err := a.metadata.readRecentConnectionsLocal()
	if err != nil {
		return nil, err
	}
	if err := a.metadata.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	routes := slices.Clone(records)
	for _, row := range a.metadata.attachments.rows {
		if row.Connection != nil {
			routes = append(routes, *row.Connection)
		}
	}
	snapshot, _ := a.tasks.taskInventory(taskInventoryRequest{Native: true, Hosted: true, Detail: "health"})
	target, err := selectTask(snapshot, "", ref, routes)
	return &target, err
}

func (m *daemonMetadata) rememberConnectionLocal(record recentConnection) error {
	if err := validateRecentConnection(record); err != nil {
		return err
	}
	if _, err := m.readRecentConnectionsLocal(); err != nil {
		return err
	}
	return m.connections.promote(*record.clone(), func(previous recentConnection) bool {
		return sameRecentEndpoint(previous, record)
	})
}

// Sibling routes can have different capabilities into the same target account.
func sameRecentEndpoint(a, b recentConnection) bool {
	if a.Host != b.Host || (a.Parent == nil) != (b.Parent == nil) {
		return false
	}
	if a.Parent == nil {
		return true
	}
	for p, q := a.Parent, b.Parent; p != nil && q != nil; p, q = p.Parent, q.Parent {
		if p.Host != q.Host || p.Instance != q.Instance || p.Server != q.Server ||
			p.Directory != q.Directory || !slices.Equal(p.SSH, q.SSH) || (p.Parent == nil) != (q.Parent == nil) {
			return false
		}
	}
	return a.Server == b.Server && a.Directory == b.Directory && slices.Equal(a.SSH, b.SSH)
}
