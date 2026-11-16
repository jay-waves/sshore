package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func launchLocalDaemon() (localShellRecord, error) {
	var empty localShellRecord
	exe, err := os.Executable()
	if err != nil {
		return empty, err
	}
	cmd := exec.Command(exe, "local-daemon")
	detachLocalDaemon(cmd)
	dir, err := stateDir("daemon")
	if err != nil {
		return empty, err
	}
	logPath := filepath.Join(dir, "local.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return empty, err
	}
	defer log.Close()
	if err = privateState(logPath, false); err != nil {
		return empty, err
	}
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return empty, err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return empty, err
	}
	defer output.Close()
	if err = cmd.Start(); err != nil {
		return empty, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = cmd.Process.Kill()
		}
		go cmd.Wait()
	}()
	request := localShellRecord{Name: "daemon", Version: version, Token: resourceToken()}
	if err = json.NewEncoder(input).Encode(request); err != nil {
		return empty, err
	}
	result := make(chan localStartup, 1)
	go func() {
		var reply localStartup
		if e := json.NewDecoder(output).Decode(&reply); e != nil {
			reply.Error = e.Error()
		}
		result <- reply
	}()
	select {
	case reply := <-result:
		if reply.Error != "" {
			return empty, fmt.Errorf("start local daemon: %s", reply.Error)
		}
		path, err := daemonPath()
		if err != nil {
			return empty, err
		}
		r, err := readLocalRecord(path, "daemon")
		if err != nil {
			return empty, err
		}
		if r != reply.Record || r.Token != request.Token || r.Version != version {
			return empty, fmt.Errorf("local daemon changed during startup")
		}
		if err = json.NewEncoder(input).Encode("ready"); err != nil {
			return empty, err
		}
		committed = true
		return r, nil
	case <-time.After(10 * time.Second):
		return empty, fmt.Errorf("local daemon startup timed out")
	}
}
