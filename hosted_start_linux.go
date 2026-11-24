//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func prepareHostedShell(name, mode, instance, executable string, gate *os.File) error {
	defer gate.Close()
	if mode == "new" && isShortTaskID(name) {
		name = ""
	}
	var record localShellRecord
	var err error
	if mode == "attach" {
		target, err := resolveHostedShell(name, instance)
		if err != nil {
			return err
		}
		if target.Instance == "" {
			return fmt.Errorf("shell %s does not exist; check known IDs with sshore list", name)
		}
		record, err = existingDaemonRecord()
		if err != nil {
			return err
		}
		record.UID = target.Instance
	} else {
		var cwd string
		cwd, err = os.Getwd()
		if err == nil {
			record, err = createHostedShell(context.Background(), executable, name, cwd, os.Environ())
		}
	}
	if err != nil {
		return err
	}
	if err := checkPeerVersion(record.Version, "daemon"); err != nil {
		return err
	}
	gate.Close()
	return writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: record.UID, LocalRequest: &localDaemonRequest{Record: record}})
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
