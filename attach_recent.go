package main

import "fmt"

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

// The caller holds shellMu across both history reads.
func (a *daemonActivity) detachedAttachmentCandidates(local bool, host string) ([]recentAttachment, error) {
	m := &a.metadata
	if err := m.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	var rows []recentAttachment
	hasRemote := false
	for _, row := range m.attachments.rows {
		if !row.inScope(local, host) {
			continue
		}
		rows = append(rows, row)
		hasRemote = hasRemote || !row.Local
	}
	if hasRemote {
		if err := m.loadShells(); err != nil {
			return nil, err
		}
		m.treeMu.Lock()
		defer m.treeMu.Unlock()
		_ = m.loadTrees()
		return matchDetachedSnapshots(rows, a.walkShellSnapshotsLocked)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows, nil
}

func resolveDetachedAttach(o *options) error {
	reply, err := metadataRPC(metadataRequest{Action: "attach_get", Local: o.local, Host: o.host})
	if err != nil {
		return err
	}
	var locals []localShellView
	if len(reply.Attachments) > 0 && reply.Attachments[0].Local {
		locals, err = localShells(false)
		if err != nil {
			return err
		}
	}
	selectDetachedAttach(o, reply.Attachments, locals)
	return nil
}

func matchDetachedSnapshots(rows []recentAttachment, walk func(func(shellSnapshot)) error) ([]recentAttachment, error) {
	type key struct{ host, identity string }
	observed := make(map[key]shellSnapshot, len(rows))
	for _, row := range rows {
		if !row.Local && row.Connection != nil {
			observed[key{row.Connection.Host, row.Identity}] = shellSnapshot{}
		}
	}
	if err := walk(func(snapshot shellSnapshot) {
		if snapshot.State != "detached" || snapshot.Unconfirmed || snapshot.Forgotten || snapshot.Active {
			return
		}
		k := key{snapshot.Host, remoteShellIdentity(snapshot.Host, snapshot.Instance)}
		old, wanted := observed[k]
		if wanted && (old.Name == "" || snapshot.ConfirmedAt.After(old.ConfirmedAt) ||
			snapshot.ConfirmedAt.Equal(old.ConfirmedAt) && snapshot.Name < old.Name) {
			observed[k] = snapshot
		}
	}); err != nil {
		return nil, err
	}
	candidates := rows[:0]
	for _, row := range rows {
		if !row.Local {
			if row.Connection == nil {
				continue
			}
			name := observed[key{row.Connection.Host, row.Identity}].Name
			if name == "" {
				continue
			}
			route := row.Connection.clone()
			route.Session = name
			row.Name, row.Connection = name, route
		}
		candidates = append(candidates, row)
	}
	return candidates, nil
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
