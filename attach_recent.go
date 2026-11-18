package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history remains responsible only for SSH defaults and old clients.
type recentAttachment struct {
	Identity   string
	Local      bool
	Name       string
	Connection *recentConnection `json:",omitempty"`
}

func attachmentMRULocal(promote *recentAttachment) ([]recentAttachment, error) {
	dir, err := stateDir("connections")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "detached.json")
	hub := &accountActivity.Load().metadata
	rows := hub.attachments
	if hub.attachmentPath != path {
		rows = nil
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			if err := privateStatePath(path, false); err != nil {
				return nil, err
			}
			if err := json.NewDecoder(io.LimitReader(f, 1024*1024)).Decode(&rows); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if len(rows) > recentConnectionLimit {
			return nil, fmt.Errorf("too many detached attachments")
		}
		for _, row := range rows {
			if err := validateRecentAttachment(row); err != nil {
				return nil, err
			}
		}
		hub.attachmentPath, hub.attachments = path, rows
	}
	if promote == nil {
		return cloneAttachments(rows), nil
	}
	if err := validateRecentAttachment(*promote); err != nil {
		return nil, err
	}
	next := []recentAttachment{*promote}
	for _, row := range rows {
		if row.Identity != promote.Identity && len(next) < recentConnectionLimit {
			next = append(next, row)
		}
	}
	if err := writeStateJSON(path, next); err != nil {
		return nil, err
	}
	hub.attachments = cloneAttachments(next)
	return cloneAttachments(next), nil
}

func validateRecentAttachment(row recentAttachment) error {
	if row.Identity == "" || len(row.Identity) > 256 || validSession(row.Name) != nil {
		return fmt.Errorf("invalid detached attachment")
	}
	if !row.Local {
		if row.Connection == nil {
			return fmt.Errorf("detached attachment route missing")
		}
		if row.Identity != remoteShellIdentity(row.Connection.Host, row.Connection.Instance) {
			return fmt.Errorf("detached attachment identity does not match route")
		}
		return validateRecentConnection(*row.Connection)
	}
	if row.Connection != nil {
		return fmt.Errorf("local detached attachment has remote route")
	}
	return nil
}

func rememberDetachedAttachment(o options) error {
	row := recentAttachment{Identity: o.taskIdentity, Local: o.local, Name: o.session}
	if row.Identity == "" {
		row.Identity = remoteShellIdentity(o.host, o.instance)
	}
	if !o.local {
		connection, err := connectionFromOptions(o)
		if err != nil {
			return err
		}
		row.Connection = &connection
	}
	_, err := metadataRPC(metadataRequest{Action: "attach_detach", Attachment: &row})
	return err
}

func resolveDetachedAttach(o *options) (bool, error) {
	reply, err := metadataRPC(metadataRequest{Action: "attach_get"})
	if err != nil {
		return false, err
	}
	if len(reply.Attachments) == 0 {
		return false, nil
	}
	var locals []localShellView
	if o.host == "" || o.local {
		locals, err = localShells()
		if err != nil {
			return false, err
		}
	}
	var snapshots []shellSnapshot
	if !o.local {
		snapshots, err = readShellSnapshots()
		if err != nil {
			return false, err
		}
	}
	return selectDetachedAttach(o, reply.Attachments, locals, snapshots), nil
}

func selectDetachedAttach(o *options, rows []recentAttachment, locals []localShellView, snapshots []shellSnapshot) bool {
	for _, row := range rows {
		if !row.inScope(*o) {
			continue
		}
		if row.Local {
			for _, local := range locals {
				if local.identity != row.Identity || local.State != "detached" {
					continue
				}
				o.local, o.hosted = true, local.hosted
				o.host, o.session, o.taskIdentity = "local", local.Name, local.identity
				if local.hosted {
					o.instance = strings.TrimPrefix(local.identity, "task:")
				}
				return true
			}
		} else if !o.local && row.Connection != nil {
			for _, snapshot := range snapshots {
				if remoteShellIdentity(snapshot.Host, snapshot.Instance) != row.Identity || snapshot.Host != row.Connection.Host || snapshot.State != "detached" || snapshot.Unconfirmed || snapshot.Forgotten || snapshot.Active {
					continue
				}
				o.host, o.session, o.instance = snapshot.Host, snapshot.Name, snapshot.Instance
				route := row.Connection.clone()
				route.Session = snapshot.Name
				applyConnectionOptions(o, route)
				return true
			}
		}
	}
	return false
}

// Host selection filters the global order; local shells are selected globally
// or explicitly by identity, without a separate public local scope.
func (r recentAttachment) inScope(o options) bool {
	if o.local {
		return r.Local
	}
	if o.host == "" {
		return true
	}
	return !r.Local && r.Connection != nil && r.Connection.Host == o.host
}

func cloneAttachments(rows []recentAttachment) []recentAttachment {
	result := append([]recentAttachment(nil), rows...)
	for i := range result {
		if result[i].Connection != nil {
			route := result[i].Connection.clone()
			result[i].Connection = &route
		}
	}
	return result
}
