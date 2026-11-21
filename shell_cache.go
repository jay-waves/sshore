package main

import (
	"fmt"
	"slices"
)

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history remains responsible only for SSH defaults and old clients.
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

func (m *daemonMetadata) detachedAttachmentCandidates(local bool, host string) ([]recentAttachment, error) {
	if err := m.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	var rows []recentAttachment
	for _, row := range m.attachments.rows {
		if row.inScope(local, host) {
			row.Connection = row.Connection.clone()
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func removeRecentDetach(o options) error {
	id := o.taskIdentity
	if id == "" {
		id = remoteShellIdentity(o.host, o.instance)
	}
	if id == "" {
		return nil
	}
	_, err := metadataRPC(metadataRequest{Action: "attach_remove", Key: id})
	return err
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

func resolveDetachedAttach(o *options) error {
	reply, err := metadataRPC(metadataRequest{Action: "attach_get", Local: o.local, Host: o.host})
	if err != nil {
		return err
	}
	var locals []localShellView
	if slices.ContainsFunc(reply.Attachments, func(row recentAttachment) bool { return row.Local }) {
		locals, err = localShells(false)
		if err != nil {
			return err
		}
	}
	selectDetachedAttach(o, reply.Attachments, locals)
	return nil
}

func selectDetachedAttach(o *options, rows []recentAttachment, locals []localShellView) bool {
	detached := make(map[string]localShellView)
	for _, local := range locals {
		if _, exists := detached[local.identity]; !exists && local.State == "detached" {
			detached[local.identity] = local
		}
	}
	for _, row := range rows {
		if !row.inScope(o.local, o.host) {
			continue
		}
		if row.Local {
			if local, ok := detached[row.Identity]; ok {
				o.local, o.hosted = true, local.hosted
				o.host, o.session, o.taskIdentity = "local", local.Name, local.identity
				if local.hosted {
					o.instance = canonicalTaskUID(local.identity)
				}
				return true
			}
		} else if row.Connection != nil {
			o.host, o.session, o.instance = row.Connection.Host, row.Name, row.Connection.Instance
			o.taskIdentity = row.Identity
			applyConnectionOptions(o, *row.Connection)
			return true
		}
	}
	return false
}

// Host selection filters the global order; local shells are selected globally
// or explicitly by identity, without a separate public local scope.
func (r recentAttachment) inScope(local bool, host string) bool {
	if local {
		return r.Local
	}
	if host == "" {
		return true
	}
	return !r.Local && r.Connection != nil && r.Connection.Host == host
}
