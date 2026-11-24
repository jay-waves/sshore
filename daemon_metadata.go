package main

import (
	"context"
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
	attachments   recentHistory[recentAttachment]
	connections   recentHistory[recentConnection]
	participation recentHistory[participatingShell]
	shellMu       sync.Mutex
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
	Targets                                 []sessionTarget `json:",omitempty"`
	Action, Key, Host, Name, Instance, Kind string

	Attachment    *recentAttachment
	Connection    *recentConnection
	Participant   *participatingShell
	Participation *participationRefresh
}
type metadataReply struct {
	Choice       string            `json:",omitempty"`
	Target       *selectedTask     `json:",omitempty"`
	Connection   *recentConnection `json:",omitempty"`
	Participants []participatingShell

	Error string
}

func metadataRPC(r metadataRequest) (metadataReply, error) {
	var reply metadataReply
	if !activityAllowed() {
		switch r.Action {
		case "connection_select", "participation_get":
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
	s.shellMu.Lock()
	defer s.shellMu.Unlock()
	var err error
	switch r.Action {
	case "attach_by_id":
		reply.Target, err = a.resolveBareAttachID(r.Instance)
	case "attach_recent":
		reply.Target, err = a.selectDetachedAttach(r.Key, r.Kind)
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
	case "participation_get":
		reply.Participants, err = s.readParticipatingShellsLocal(r.Host)
	case "participation_observe", "participation_state", "participation_forget", "participation_rename", "participation_refresh":
		err = s.updateParticipation(r)
	default:
		err = fmt.Errorf("invalid metadata operation")
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
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
	candidates := (taskQuery{Kind: "shell"}).registered(a.tasks)
	for i := range routes {
		route := &routes[i]
		if validTaskUID(route.Instance) && !route.viaShell() {
			candidates = append(candidates, selectedTask{Kind: "shell", Identity: identityFromUID(route.Instance), Instance: route.Instance, Connection: route})
		}
	}
	target, err := matchTask(ref, candidates)
	if err == nil && target.Identity == "" {
		return nil, usage("unknown task %s; use sshore list or specify HOST:ID", ref)
	}
	return &target, err
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
