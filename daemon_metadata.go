package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// History is persisted by the daemon; running clients never coordinate through
// these files. Each domain is loaded once and thereafter served from memory.
type daemonMetadata struct {
	attachments         recentHistory[recentAttachment]
	connections         recentHistory[recentConnection]
	treeMu, shellMu     sync.Mutex
	treeRoot, shellRoot string
	trees               map[string][]shellTreeNode
	snapshots           map[string]shellSnapshot
}

// The two histories retain independent orderings and share the same file lifecycle.
const recentHistoryLimit = 32

type recentHistory[T any] struct {
	path     string
	maxBytes int
	rows     []T
}

func (h *recentHistory[T]) load(file string, maxBytes int, validate func(T) error) error {
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
	if len(rows) > recentHistoryLimit {
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
	OwnerProcess                             *spawnProcessView
	Local                                    bool `json:",omitempty"`
	Action, Key, Host, Name, State, Instance string
	Tree                                     []shellTreeNode
	Lifetime                                 []string
	Snapshot                                 *shellSnapshot
	Inventory                                *inventory
	Attachment                               *recentAttachment
	Connection                               *recentConnection
}
type metadataReply struct {
	Attachments []recentAttachment
	Connection  *recentConnection `json:",omitempty"`
	Connections []recentConnection
	Tree        []shellTreeNode
	Snapshots   []shellSnapshot
	Found       bool
	Error       string
}

func metadataRPC(r metadataRequest) (metadataReply, error) {
	reply, err := activityRPC(activityRequest{Kind: "metadata", Metadata: &r})
	if err != nil {
		return metadataReply{}, err
	}
	if reply.Metadata == nil {
		return metadataReply{}, fmt.Errorf("daemon metadata response missing")
	}
	if reply.Metadata.Error != "" {
		return *reply.Metadata, fmt.Errorf("%s", reply.Metadata.Error)
	}
	return *reply.Metadata, nil
}
func (a *daemonActivity) handleMetadata(r metadataRequest) (reply metadataReply) {
	s := &a.metadata
	tree := strings.HasPrefix(r.Action, "tree_")
	shell := strings.HasPrefix(r.Action, "shell_") || r.Action == "connection_rename" ||
		r.Action == "connection_select" && r.Host != "" && r.Name != ""
	if !tree {
		s.shellMu.Lock()
		defer s.shellMu.Unlock()
	}
	if shell {
		if err := s.loadShells(); err != nil {
			reply.Error = err.Error()
			return
		}
	}
	// Reconcile shell history and tree evidence under the same locks. Tree
	// operations never take shellMu, so the lock order is always shell then tree.
	if tree || shell && (r.Action != "shell_save" || r.Snapshot != nil && r.Snapshot.State == "exited") {
		s.treeMu.Lock()
		defer s.treeMu.Unlock()
		// Shell history remains readable when tree evidence is unavailable.
		if err := s.loadTrees(); err != nil && tree {
			reply.Error = err.Error()
			return
		}
	}
	var err error
	switch r.Action {
	case "attach_get":
		reply.Attachments, err = a.detachedAttachmentCandidates(r.Local, r.Host)
	case "attach_detach":
		if r.Attachment != nil {
			err = s.rememberDetachedAttachmentLocal(*r.Attachment)
		}
	case "connection_get":
		var records []recentConnection
		records, err = s.readRecentConnectionsLocal()
		for _, record := range records {
			reply.Connections = append(reply.Connections, *record.clone())
		}
	case "connection_select":
		reply.Connection, err = a.selectRecentConnection(r.Host, r.Name, r.Instance)
	case "connection_save":
		if r.Connection != nil {
			err = s.rememberConnectionLocal(*r.Connection)
		}
	case "connection_rename":
		err = a.rememberShellRenameLocal(r.Host, r.Key, r.Name, r.Instance)
	case "tree_get":
		reply.Tree = cloneShellTree(s.treeChain(r.Key))
	case "tree_list":
		reply.Tree = s.knownShellTreesLocal()
	case "tree_save":
		s.saveShellTreeLocal(r.Key, r.Name, r.Tree, r.State, r.OwnerProcess, r.Lifetime...)
	case "tree_remember":
		s.rememberShellTreeLocal(r.Tree)
	case "tree_rename":
		s.renameShellTreeLocal(r.Key, r.Name)
	case "shell_get":
		reply.Snapshots = make([]shellSnapshot, 0, len(s.snapshots))
		err = a.walkShellSnapshotsLocked(func(snapshot shellSnapshot) {
			snapshot.Connection = snapshot.Connection.clone()
			reply.Snapshots = append(reply.Snapshots, snapshot)
		})
		if err != nil {
			reply.Snapshots = nil
		}
	case "shell_save":
		if r.Snapshot != nil {
			snapshot := *r.Snapshot
			snapshot.Connection = snapshot.Connection.clone()
			err = a.saveShellSnapshotLocked(snapshot)
		}
	case "shell_cache":
		if r.Inventory == nil {
			err = fmt.Errorf("inventory missing")
			break
		}
		err = a.cacheRemoteShellsLocked(r.Host, *r.Inventory, r.Connection.clone())
	case "shell_forget":
		reply.Found, err = a.forgetEndedShellLocal(r.Host, r.Name, r.Instance)
	case "shell_cancel":
		err = a.confirmKilledShellLocal(r.Host, r.Name, r.Instance)
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
		if err := readStateJSON(filepath.Join(root, entry.Name()), maxShellTreeBytes, &chain); err != nil || len(chain) == 0 || len(chain) > maxShellTreeNodes {
			continue
		}
		for i := range chain {
			chain[i].Cached = true
		}
		trees[entry.Name()] = chain
	}
	s.trees, s.treeRoot = trees, root
	return nil
}
func (s *daemonMetadata) loadShells() error {
	stateRoot, err := statePath("")
	if err != nil {
		return err
	}
	root := filepath.Join(stateRoot, "shells")
	if s.snapshots != nil && s.shellRoot == root {
		return nil
	}
	if _, err = prepareStateDir(stateRoot, "shells"); err != nil {
		return err
	}
	records := make(map[string]shellSnapshot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		var snapshot shellSnapshot
		if err = readStateJSON(path, maxShellSnapshot, &snapshot); err != nil {
			return err
		}
		if err = snapshot.validateRoute(); err != nil {
			return err
		}
		expected, e := snapshot.cachePath(root)
		if e != nil || expected != path {
			return fmt.Errorf("invalid shell history")
		}
		snapshot.Active = false
		if !endedShellState(snapshot.State) {
			snapshot.Unconfirmed = true
		}
		records[path] = snapshot
	}
	s.snapshots, s.shellRoot = records, root
	return nil
}

const (
	maxShellTreeNodes = 64
	maxShellTreeBytes = 64 * 1024
)

func shellTreeFilename(id string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(id)))
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
	if len(chain) == 0 || len(chain) > maxShellTreeNodes || chain[len(chain)-1].Identity == "" {
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
