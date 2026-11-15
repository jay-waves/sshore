package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const recentConnectionLimit = 32

type recentConnection struct {
	Host, Session, Server, Directory string
	SSH                              []string
}

func readRecentConnections() ([]recentConnection, error) {
	dir, err := stateDir("connections")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "recent.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []recentConnection
	if err = json.NewDecoder(io.LimitReader(f, 128*1024)).Decode(&records); err != nil {
		return nil, fmt.Errorf("cannot read recent connections: %w", err)
	}
	if len(records) > recentConnectionLimit {
		return nil, fmt.Errorf("too many recent connections")
	}
	for _, r := range records {
		if validSSHDestination(r.Host) != nil || validSession(r.Session) != nil || r.Server == "" || strings.ContainsAny(r.Server, "\x00\r\n") {
			return nil, fmt.Errorf("invalid recent connection")
		}
		if r.Directory != "" && !filepath.IsAbs(r.Directory) {
			return nil, fmt.Errorf("invalid recent connection directory")
		}
		if len(r.SSH) > 0 {
			if _, _, err = splitSSHOptions(append([]string{"--"}, r.SSH...), false); err != nil {
				return nil, err
			}
		}
	}
	return records, nil
}

func resolveRecentAttach(o *options) error {
	if o.command != "attach" {
		return nil
	}
	records, err := readRecentConnections()
	if err != nil {
		return err
	}
	for _, r := range records {
		if o.host != "" && o.host != r.Host {
			continue
		}
		o.host = r.Host
		if o.session == "" {
			o.session = r.Session
		}
		if !o.serverSet {
			o.server = r.Server
		}
		if len(o.ssh) == 0 {
			o.ssh = append([]string(nil), r.SSH...)
			o.sshDirectory = r.Directory
		}
		return nil
	}
	if o.host == "" {
		return usage("no recent connection; use sshore attach HOST [NAME] or sshore launch HOST")
	}
	return nil
}

func rememberConnection(o options) error {
	if o.plain || o.host == "" || o.session == "" {
		return nil
	}
	records, err := readRecentConnections()
	if err != nil {
		return err
	}
	// Forwarding flags belong to the original connection and must never replay
	// merely because the user selected a recent shell.
	ssh := forwardSSHOptions(o.ssh)
	directory := o.sshDirectory
	if len(ssh) == 0 {
		directory = ""
	} else if directory == "" {
		directory, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	server := o.server
	if server == "" {
		server = "sshore"
	}
	record := recentConnection{Directory: directory, Host: o.host, Session: o.session, Server: server, SSH: ssh}
	next := []recentConnection{record}
	for _, old := range records {
		if old.Host != o.host && len(next) < recentConnectionLimit {
			next = append(next, old)
		}
	}
	dir, err := stateDir("connections")
	if err != nil {
		return err
	}
	return writeStateJSON(filepath.Join(dir, "recent.json"), next)
}

// rememberShellRename changes cached labels only after a successful remote rename.
// It does not reorder recent hosts or treat rename as an attachment confirmation.
func rememberShellRename(host, old, name string) error {
	if validSSHDestination(host) != nil || validSession(old) != nil || validSession(name) != nil {
		return fmt.Errorf("invalid shell rename identity")
	}
	if old == name {
		return nil
	}
	snapshotErr := renameShellSnapshot(host, old, name)
	records, err := readRecentConnections()
	if err != nil {
		return errors.Join(snapshotErr, err)
	}
	changed := false
	for i := range records {
		if records[i].Host == host && records[i].Session == old {
			records[i].Session = name
			changed = true
		}
	}
	if changed {
		dir, e := stateDir("connections")
		if e != nil {
			return errors.Join(snapshotErr, e)
		}
		err = writeStateJSON(filepath.Join(dir, "recent.json"), records)
	}
	return errors.Join(snapshotErr, err)
}
