package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// History is persisted by the daemon; running clients never coordinate through
// these files. Each domain is loaded once and thereafter served from memory.
type daemonMetadata struct {
	attachments         []recentAttachment
	attachmentPath      string
	connections         []recentConnection
	connectionRoot      string
	treeMu, shellMu     sync.Mutex
	treeRoot, shellRoot string
	trees               map[string][]shellTreeNode
	snapshots           map[string]shellSnapshot
}
type metadataRequest struct {
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
func (s *daemonMetadata) handle(r metadataRequest) (reply metadataReply) {
	tree := len(r.Action) >= 5 && r.Action[:5] == "tree_"
	mu := &s.shellMu
	if tree {
		mu = &s.treeMu
	}
	mu.Lock()
	defer mu.Unlock()
	var err error
	if tree {
		err = s.loadTrees()
	} else if r.Action != "connection_get" && r.Action != "connection_save" && r.Action != "attach_get" && r.Action != "attach_detach" {
		err = s.loadShells()
	}
	if err != nil {
		reply.Error = err.Error()
		return
	}
	switch r.Action {
	case "attach_get", "attach_detach":
		reply.Attachments, err = attachmentMRULocal(r.Attachment)
	case "connection_get":
		reply.Connections, err = readRecentConnectionsLocal()
	case "connection_save":
		if r.Connection != nil {
			err = rememberConnectionLocal(*r.Connection)
		}
	case "connection_rename":
		err = rememberShellRenameLocal(r.Host, r.Key, r.Name)
	case "tree_get":
		reply.Tree = readTreeLocal(r.Key)
	case "tree_list":
		reply.Tree = knownShellTreesLocal()
	case "tree_save":
		saveShellTreeLocal(r.Key, r.Name, r.Tree, r.State, r.Lifetime...)
	case "tree_remember":
		rememberShellTreeLocal(r.Tree)
	case "tree_rename":
		renameShellTreeLocal(r.Key, r.Name)
	case "tree_state":
		if r.Snapshot != nil {
			err = recordRemoteShellTreeStateLocal(*r.Snapshot)
		}
	case "shell_get":
		reply.Snapshots, err = readShellSnapshotsLocked()
	case "shell_save":
		if r.Snapshot != nil {
			err = saveShellSnapshotLocked(*r.Snapshot)
		}
	case "shell_cache":
		if r.Inventory == nil {
			err = fmt.Errorf("inventory missing")
			break
		}
		var routes []recentConnection
		if r.Connection != nil {
			routes = append(routes, *r.Connection)
		}
		err = cacheRemoteShellsLocked(r.Host, *r.Inventory, routes...)
	case "shell_rename":
		err = renameShellSnapshotLocked(r.Host, r.Key, r.Name)
	case "shell_forget":
		reply.Found, err = forgetEndedShellLocal(r.Host, r.Name, r.Instance)
	case "shell_cancel":
		err = confirmKilledShellLocal(r.Host, r.Name, r.Instance)
	default:
		err = fmt.Errorf("invalid metadata operation")
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
}
func (s *daemonMetadata) loadTrees() error {
	root, err := statePath("shell-tree")
	if err != nil {
		return err
	}
	if s.trees != nil && s.treeRoot == root {
		return nil
	}
	if _, err = stateDir("shell-tree"); err != nil {
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
		path := filepath.Join(root, entry.Name())
		if privateStatePath(path, false) != nil {
			continue
		}
		chain := readShellTree(path)
		for i := range chain {
			chain[i].Cached = true
		}
		trees[path] = chain
	}
	s.trees, s.treeRoot = trees, root
	return nil
}
func (s *daemonMetadata) loadShells() error {
	root, err := statePath("shells")
	if err != nil {
		return err
	}
	if s.snapshots != nil && s.shellRoot == root {
		return nil
	}
	if _, err = stateDir("shells"); err != nil {
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
		if err = privateStatePath(path, false); err != nil {
			return err
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if len(data) > maxShellSnapshot {
			return fmt.Errorf("shell history is too large")
		}
		var snapshot shellSnapshot
		if e = json.Unmarshal(data, &snapshot); e != nil {
			return e
		}
		expected, e := snapshot.cachePath()
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
func readTreeLocal(id string) []shellTreeNode {
	path, err := shellTreePath(id)
	if err != nil {
		return nil
	}
	return append([]shellTreeNode(nil), accountActivity.Load().metadata.trees[path]...)
}
func writeTreeLocal(id string, chain []shellTreeNode) error {
	path, err := shellTreePath(id)
	if err != nil {
		return err
	}
	accountActivity.Load().metadata.trees[path] = append([]shellTreeNode(nil), chain...)
	return writeStateJSON(path, chain)
}
