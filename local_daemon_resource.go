package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Mount workers receive only startup/control messages through the daemon.
// Forwards run here and directly own their SSH child; payloads stay in SSH.
func runDaemonResource(c net.Conn, request localDaemonRequest) (result error) {
	enc := json.NewEncoder(c)
	defer func() {
		if result != nil {
			_ = enc.Encode(resourceStartup{Error: result.Error()})
		}
	}()
	dec := json.NewDecoder(io.LimitReader(c, 1<<20))
	var config resourceConfig
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := dec.Decode(&config); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Time{})
	if request.Kind == "forward" {
		if config.Forward == nil || config.Forward.Name != request.Record.Name {
			return fmt.Errorf("invalid forward startup")
		}
	} else {
		o, err := parseFTPOptions(config.Args)
		if err != nil {
			return err
		}
		if config.Forward != nil || o.name != request.Record.Name {
			return fmt.Errorf("invalid mount startup")
		}
	}
	dir, err := resourceStateDir(request.Kind)
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, request.Record.Name+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	if err = privateStatePath(logPath, false); err != nil {
		return err
	}
	if config.Forward != nil {
		_, err := runConfiguredResource(config, dec, c, log, request.Env, request.Record.Directory, func() { c.Close() })
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "resource-worker")
	detachResourceWorker(cmd)
	cmd.Dir = request.Record.Directory
	cmd.Env = request.Env
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	if err = cmd.Start(); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		input.Close()
		output.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	if err = json.NewEncoder(input).Encode(config); err != nil {
		return err
	}
	child := json.NewDecoder(output)
	var startup resourceStartup
	if err = child.Decode(&startup); err != nil {
		return err
	}
	if err = enc.Encode(startup); err != nil {
		return err
	}
	if startup.Error != "" {
		return nil
	}
	var ack string
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = dec.Decode(&ack); err != nil {
		return err
	}
	if ack != "commit" {
		return fmt.Errorf("invalid resource commit")
	}
	if err = json.NewEncoder(input).Encode(ack); err != nil {
		return err
	}
	if err = child.Decode(&ack); err != nil {
		return err
	}
	if ack != "started" {
		return fmt.Errorf("resource not started")
	}
	// From this point the worker owns the task even if the acknowledgement is lost.
	committed = true
	_ = enc.Encode(ack)
	c.Close()
	input.Close()
	output.Close()
	_ = cmd.Wait()
	return nil
}
