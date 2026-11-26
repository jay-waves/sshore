package main

import (
	"fmt"
	"slices"
	"time"
)

// Participation records only tasks opened through this client account.
type participatingShell struct {
	Host, Instance, Name string
	Owner                string
	Shell                string `json:",omitempty"`
	Program              bool   `json:",omitempty"`
	State                string
	PID                  int
	Pinned               bool
	PinUntil             time.Time
	Attachment           string
	ChangedAt            time.Time
}

type participationRefresh struct {
	Shells []shellView
	Since  time.Time
}

func reconcileParticipatingShells(host string, shells []shellView, since time.Time) error {
	refresh := participationRefresh{Shells: shells, Since: since}
	_, err := metadataRPC(metadataRequest{Action: "participation_refresh", Host: host, Participation: &refresh})
	return err
}

func forgetParticipatingShell(host, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_forget", Host: host, Instance: instance})
	return err
}

func observeParticipatingShell(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_observe", Participant: &row})
	return err
}

func setParticipatingShellState(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_state", Participant: &row})
	return err
}

func validateParticipatingShell(row participatingShell) error {
	switch {
	case validSSHDestination(row.Host) != nil:
		return fmt.Errorf("remote shell has an invalid SSH destination")
	case !validTaskUID(row.Instance):
		return fmt.Errorf("remote shell has an invalid task ID")
	case row.Owner != "" && row.Owner != identityFromUID(canonicalTaskUID(row.Owner)):
		return fmt.Errorf("remote shell has an invalid parent task ID")
	case validSession(row.Name) != nil:
		return fmt.Errorf("remote shell has an invalid session name")
	case row.PID <= 0:
		return fmt.Errorf("remote shell has invalid PID %d (expected a positive PID)", row.PID)
	case !validTaskUID(row.Attachment):
		return fmt.Errorf("remote shell has an invalid attachment ID")
	}
	switch row.State {
	case taskActive, taskWaiting, taskDetached, taskUnknown:
		return nil
	default:
		return fmt.Errorf("remote shell has invalid state %.40q (expected %s, %s, %s or %s)", row.State, taskActive, taskWaiting, taskDetached, taskUnknown)
	}
}

func (m *daemonMetadata) loadParticipation() error {
	return m.participation.loadEntries("participation.json", 1024*1024, 4096, validateParticipatingShell)
}

func (m *daemonMetadata) readParticipatingShellsLocal(host string) ([]participatingShell, error) {
	if err := m.loadParticipation(); err != nil {
		return nil, err
	}
	var rows []participatingShell
	for _, row := range m.participation.rows {
		if host == "" || row.Host == host {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (m *daemonMetadata) updateParticipation(r metadataRequest) error {
	if err := m.loadParticipation(); err != nil {
		return err
	}
	rows := slices.Clone(m.participation.rows)
	now := time.Now()
	switch r.Action {
	case "participation_observe", "participation_state":
		if r.Participant == nil {
			return fmt.Errorf("missing participating shell")
		}
		row := *r.Participant
		if err := validateParticipatingShell(row); err != nil {
			return err
		}
		row.ChangedAt = now
		i := slices.IndexFunc(rows, func(old participatingShell) bool {
			return old.Host == row.Host && old.Instance == row.Instance
		})
		switch {
		case r.Action == "participation_state":
			if i < 0 || rows[i].Attachment != row.Attachment {
				return nil
			}
			if row.State == taskDetached {
				rows[i].Name = row.Name
			}
			rows[i].State, rows[i].ChangedAt = row.State, now
			rows[i].Pinned, rows[i].PinUntil = row.Pinned, row.PinUntil
		case i >= 0:
			rows[i] = row
		case len(rows) == 4096:
			return fmt.Errorf("participating shell limit reached")
		default:
			rows = append(rows, row)
		}
	case "participation_forget":
		if !validTaskUID(r.Instance) || validSSHDestination(r.Host) != nil {
			return fmt.Errorf("invalid participating shell identity")
		}
		i := slices.IndexFunc(rows, func(row participatingShell) bool {
			return row.Host == r.Host && row.Instance == r.Instance
		})
		if i >= 0 {
			rows = slices.Delete(rows, i, i+1)
		}
	case "participation_refresh":
		if r.Participation == nil || validSSHDestination(r.Host) != nil {
			return fmt.Errorf("invalid participating shell refresh")
		}
		for _, view := range r.Participation.Shells {
			if !validTaskUID(view.Instance) || view.Owner != "" && view.Owner != identityFromUID(canonicalTaskUID(view.Owner)) {
				return fmt.Errorf("invalid shell or parent ID in participation refresh")
			}
		}
		rows = refreshParticipation(rows, r.Host, *r.Participation, now)
	}
	if slices.Equal(rows, m.participation.rows) {
		return nil
	}
	return m.participation.replace(rows)
}

func refreshParticipation(rows []participatingShell, host string, refresh participationRefresh, now time.Time) []participatingShell {
	observed := make(map[string]shellView, len(refresh.Shells))
	for _, view := range refresh.Shells {
		observed[view.Instance] = view
	}
	updated := rows[:0]
	for _, row := range rows {
		if row.Host == host && row.ChangedAt.Before(refresh.Since) {
			view, found := observed[row.Instance]
			if !found {
				continue
			}
			if validSession(view.Name) == nil && view.PID > 0 && (view.State == taskActive || view.State == taskWaiting || view.State == taskDetached) {
				row.Owner = view.Owner
				row.Name, row.State, row.PID = view.Name, view.State, view.PID
				row.Shell, row.Program = view.Shell, view.Program
				row.Pinned, row.PinUntil, row.ChangedAt = view.Pinned, view.PinUntil, now
			}
		}
		updated = append(updated, row)
	}
	return updated
}
