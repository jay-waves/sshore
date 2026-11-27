package main

import (
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
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
	attachments      recentHistory[recentAttachment]
	connections      recentHistory[recentConnection]
	participationDB  *bolt.DB
	participationErr error
	shellMu          sync.Mutex
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
	return h.loadAt(root, file, maxBytes, recentHistoryLimit, validate)
}

func (h *recentHistory[T]) loadAt(root, file string, maxBytes, maxEntries int, validate func(T) error) error {
	path := filepath.Join(root, "connections", file)
	if path == h.path {
		return nil
	}
	if _, err := prepareStateDir(root, "connections"); err != nil {
		return err
	}
	var rows []T
	if err := readStateJSON(path, maxBytes, &rows); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(rows) > maxEntries {
		return fmt.Errorf("%s has %d entries (maximum %d)", path, len(rows), maxEntries)
	}
	for i, row := range rows {
		if err := validate(row); err != nil {
			return fmt.Errorf("entry %d: %w (%s)", i+1, err, path)
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
	Action                                  string
	Key, Host, Name, Instance, Kind, Parent string          `json:",omitempty"`
	Targets                                 []sessionTarget `json:",omitempty"`

	Attachment    *recentAttachment     `json:",omitempty"`
	Connection    *recentConnection     `json:",omitempty"`
	Participant   *participatingShell   `json:",omitempty"`
	Participation *participationRefresh `json:",omitempty"`
}
type metadataReply struct {
	Choice     string            `json:",omitempty"`
	Target     *selectedTask     `json:",omitempty"`
	Connection *recentConnection `json:",omitempty"`

	Error string `json:",omitempty"`
}

func metadataRPC(r metadataRequest) (metadataReply, error) {
	var reply metadataReply
	if activity := accountActivity.Load(); activity != nil {
		reply = activity.handleMetadata(r)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		record, err := ensureLocalDaemonContext(ctx)
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
	var err error
	switch r.Action {
	case "task_by_id", "attach_by_id":
		reply.Target, err = a.selectTaskID(r.Instance, r.Kind, r.Parent, r.Action == "attach_by_id")
	case "task_by_name", "attach_by_name":
		reply.Target, err = a.selectTaskName(r.Name, r.Kind, r.Parent, r.Action == "attach_by_name")
	case "attach_recent":
		reply.Target, err = a.selectDetachedAttach(r.Key, r.Kind)
	default:
		return a.handleShellHistory(r)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
}

func (a *daemonActivity) handleShellHistory(r metadataRequest) (reply metadataReply) {
	s := &a.metadata
	if r.Action == "participation_observe" || r.Action == "participation_state" || r.Action == "participation_forget" || r.Action == "participation_refresh" {
		if err := s.updateParticipation(r); err != nil {
			reply.Error = err.Error()
		}
		return
	}
	s.shellMu.Lock()
	defer s.shellMu.Unlock()
	var err error
	switch r.Action {
	case "attach_remote_choice":
		reply.Choice, err = s.recentRemoteChoice(r.Host, r.Targets)
	case "attach_detach":
		if r.Attachment != nil {
			err = s.rememberDetachedAttachmentLocal(*r.Attachment)
		}
	case "attach_remove":
		err = s.removeRecentDetachLocal(r.Key)
	case "connection_select":
		reply.Connection, err = s.selectRecentConnection(r.Host)
	case "connection_save":
		if r.Connection != nil {
			err = s.rememberConnectionLocal(*r.Connection)
		}
	default:
		err = fmt.Errorf("invalid metadata operation")
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
}

type recentConnection struct {
	Hosted                   bool              `json:",omitempty"`
	RouteTarget              bool              `json:",omitempty"`
	Parent                   *recentConnection `json:",omitempty"`
	RouteToken               string            `json:",omitempty"`
	Instance                 string            `json:",omitempty"`
	Host, Session, Directory string
	SSH                      []string
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
	if err := m.connections.load("recent.json", 128*1024, validateShellHistoryConnection); err != nil {
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
				return fmt.Errorf("invalid saved shell route")
			}
		} else if hop.RouteToken != "" || hop.RouteTarget {
			return fmt.Errorf("shell route parent missing")
		}
		if validSSHDestination(hop.Host) != nil || validSession(hop.Session) != nil {
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

func validateShellHistoryConnection(record recentConnection) error {
	if record.viaShell() {
		return fmt.Errorf("shell history must use a direct SSH connection")
	}
	return validateRecentConnection(record)
}

func (m *daemonMetadata) rememberConnectionLocal(record recentConnection) error {
	if err := validateShellHistoryConnection(record); err != nil {
		return err
	}
	if _, err := m.readRecentConnectionsLocal(); err != nil {
		return err
	}
	return m.connections.promote(*record.clone(), func(previous recentConnection) bool {
		return previous.Host == record.Host
	})
}

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history supplies direct SSH defaults.
type recentAttachment struct {
	Identity   string
	Local      bool
	Name       string
	Connection *recentConnection `json:",omitempty"`
}

func (m *daemonMetadata) rememberDetachedAttachmentLocal(row recentAttachment) error {
	if err := validateRecentAttachment(row); err != nil {
		return err
	}
	if err := m.loadDetachedAttachments(); err != nil {
		return err
	}
	row.Connection = row.Connection.clone()
	return m.attachments.promote(row, func(previous recentAttachment) bool {
		return previous.Identity == row.Identity
	})
}

func (m *daemonMetadata) loadDetachedAttachments() error {
	return m.attachments.load("detached.json", 1024*1024, validateRecentAttachment)
}

func validateRecentAttachment(row recentAttachment) error {
	if row.Identity == "" || len(row.Identity) > 256 || validSession(row.Name) != nil {
		return fmt.Errorf("invalid detached attachment")
	}
	if !row.Local {
		if row.Connection == nil {
			return fmt.Errorf("detached attachment route missing")
		}
		if row.Identity != identityFromUID(row.Connection.Instance) {
			return fmt.Errorf("detached attachment identity does not match route")
		}
		return validateShellHistoryConnection(*row.Connection)
	}
	if row.Connection != nil {
		return fmt.Errorf("local detached attachment has remote route")
	}
	return nil
}

func (m *daemonMetadata) removeRecentDetachLocal(id string) error {
	if err := m.loadDetachedAttachments(); err != nil {
		return err
	}
	rows := m.attachments.rows
	for _, row := range rows {
		if row.Identity == id {
			return m.attachments.replace(slices.DeleteFunc(slices.Clone(rows), func(entry recentAttachment) bool { return entry.Identity == id }))
		}
	}
	return nil
}

func (a *daemonActivity) selectDetachedAttach(parent, kind string) (*selectedTask, error) {
	detached := (taskQuery{Kind: kind, Parent: parent}).recentDetached(a.tasks)
	if len(detached) == 0 {
		return nil, fmt.Errorf("no detached local shells; use sshore shell")
	}
	return &detached[0], nil
}

// The SSH peer supplies eligible roots. Local history only ranks those identities.
func (m *daemonMetadata) recentRemoteChoice(host string, targets []sessionTarget) (string, error) {
	if err := m.loadDetachedAttachments(); err != nil {
		return "", err
	}
	for _, row := range m.attachments.rows {
		if row.Connection == nil || row.Connection.Host != host {
			continue
		}
		for _, target := range targets {
			if target.Instance == row.Connection.Instance {
				return target.Name, nil
			}
		}
	}
	return "", nil
}
