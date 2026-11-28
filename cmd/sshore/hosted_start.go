//go:build linux || windows

package main

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/shell"
)

func prepareHostedShell(name, mode, instance, directory string) (localShellRecord, error) {
	if mode == "attach" {
		target, err := resolveHostedShell(name, instance)
		if err != nil {
			return localShellRecord{}, err
		}
		if target.Instance == "" {
			return localShellRecord{}, fmt.Errorf("shell %s does not exist; check known IDs with sshore list", name)
		}
		record, err := existingDaemonRecord()
		if err != nil {
			return localShellRecord{}, err
		}
		record.UID = target.Instance
		return record, checkPeerVersion(record.Version, "daemon")
	}
	if isShortTaskID(name) {
		name = ""
	}
	executable, err := os.Executable()
	if err != nil {
		return localShellRecord{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return localShellRecord{}, err
	}
	cwd, err = shell.ResolveStartupDirectory(cwd, directory)
	if err != nil {
		return localShellRecord{}, err
	}
	return createHostedShell(context.Background(), executable, name, cwd, os.Environ())
}

func reportHostedShell(record localShellRecord) error {
	var serverIP string
	if fields := strings.Fields(os.Getenv("SSH_CONNECTION")); len(fields) == 4 {
		if addr, err := netip.ParseAddr(fields[2]); err == nil {
			serverIP = addr.Unmap().String()
		}
	}
	if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Host: &hostAnnouncement{Info: localHostInfo(), SSHServer: serverIP}, Instance: record.UID, LocalRequest: &localDaemonRequest{Record: record}}); err != nil {
		return err
	}
	if rows, err := readHostedShells(); err == nil {
		_ = json.MarshalWrite(os.Stdout, rows)
	}
	return nil
}

func retentionHostedShell(m message) error {
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore shell")
	}
	c, err := dialHostedTask(context.Background(), sock, requestTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	return requestShellRetention(c, m)
}

func chooseDetachedHostedShell() (sessionTarget, error) {
	rows, err := queryRegisteredTasks(taskQuery{Kind: "hosted", Detached: true})
	if err != nil {
		return sessionTarget{}, err
	}
	targets, err := attachmentChoices(rows)
	if err != nil {
		return sessionTarget{}, err
	}
	switch len(targets) {
	case 0:
		return sessionTarget{}, fmt.Errorf("no detached shells; use sshore shell HOST to create one")
	case 1:
		return targets[0], nil
	}
	if err := writeMessage(os.Stdout, message{Type: "session_choice", SoftwareVersion: version, Targets: targets}); err != nil {
		return sessionTarget{}, err
	}
	reply, err := readMessage(os.Stdin)
	if err != nil {
		return sessionTarget{}, err
	}
	if reply.Type == "session_select" {
		for _, shell := range targets {
			if reply.Name == shell.Name {
				return shell, nil
			}
		}
	}
	return sessionTarget{}, fmt.Errorf("invalid shell selection")
}

func serveRemoteShell(mode, name, instance, directory string) (result error) {
	if mode != "new" && mode != "attach" {
		return fmt.Errorf("invalid remote service")
	}
	if mode == "attach" && name == "" {
		name = instance
	}
	if name != "" {
		if err := validSession(name); err != nil {
			return err
		}
	}
	// Fatal setup replies are framed; no human text ever enters protocol stdout.
	defer func() {
		if result != nil {
			_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: result.Error()})
		}
	}()
	if mode == "attach" && name == "" {
		selected, err := chooseDetachedHostedShell()
		if err != nil {
			return err
		}
		name = selected.Instance
	}
	record, err := prepareHostedShell(name, mode, instance, directory)
	if err != nil {
		return err
	}
	return reportHostedShell(record)
}

func dialHostedTask(ctx context.Context, ref string, timeout time.Duration) (net.Conn, error) {
	r, err := existingDaemonRecord()
	if err != nil {
		return nil, err
	}
	if !validTaskUID(ref) {
		return nil, fmt.Errorf("invalid task ID")
	}
	r.UID = ref
	return dialLocalSessionContext(ctx, r, time.Now().Add(timeout))
}
