package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

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

type recentAttachment struct {
	Host, Instance string
}

func validateRecentAttachment(row recentAttachment) error {
	if !validTaskUID(row.Instance) || validSSHDestination(row.Host) != nil {
		return fmt.Errorf("invalid detached attachment")
	}
	return nil
}

// The SSH peer supplies eligible roots. Local history only ranks those identities.
func recentRemoteChoice(host string, targets []sessionTarget) (string, error) {
	rows, err := accountHistoryRows(attachmentBucket, validateRecentAttachment)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Host != host {
			continue
		}
		for _, target := range targets {
			if target.Instance == row.Instance {
				return target.Name, nil
			}
		}
	}
	return "", nil
}
