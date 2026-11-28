package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
)

const resourceWorkerExitGrace = 15 * time.Second

func runResourceWorker(ctx context.Context, config resourceConfig, enc *jsontext.Encoder, dec *jsontext.Decoder, log io.Writer, request localDaemonRequest, ready func(context.Context, resourceRecord, <-chan error, func() error) error) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", config.kind())
	sys.DetachWorker(cmd)
	cmd.Dir = request.Record.Directory
	cmd.Env = request.Env
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	// Keep the read end independent of Cmd.Wait so a control request can
	// consume the worker's final reply after it exits.
	output, childOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer output.Close()
	defer childOutput.Close()
	cmd.Stdout = childOutput
	childInput := jsontext.NewEncoder(input)
	child := jsontext.NewDecoder(output)
	owner, releaseWorker := context.WithCancel(ctx)
	defer releaseWorker()
	if err = cmd.Start(); err != nil {
		return err
	}
	childOutput.Close()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
		releaseWorker()
	}()
	killWorker := func() {
		_ = cmd.Process.Kill()
		_ = input.Close()
		_ = output.Close()
	}
	context.AfterFunc(owner, func() {
		input.Close()
		timer := time.NewTimer(resourceWorkerExitGrace)
		defer timer.Stop()
		select {
		case <-exited:
		case <-timer.C:
			killWorker()
		}
	})
	var control sync.Mutex
	defer func() {
		releaseWorker()
		<-exited
		// The worker has exited. Let any control call consume its final reply
		// before closing the read end.
		control.Lock()
		output.Close()
		control.Unlock()
	}()
	// Watch the initiator before writing to the worker, so cancellation also
	// interrupts a blocked configuration write.
	configured := make(chan struct{})
	commit := awaitResourceCommit(func(value any) error {
		for {
			frame, err := dec.ReadValue()
			if err != nil {
				return err
			}
			if config.Mount == nil || frame.Kind() != '{' {
				return json.Unmarshal(frame, value)
			}
			<-configured
			if err := childInput.WriteValue(frame); err != nil {
				return err
			}
		}
	}, releaseWorker)
	if err = json.MarshalEncode(childInput, config); err != nil {
		close(configured)
		return err
	}
	close(configured)
	// The worker marks its startup result. Earlier frames belong to the
	// CLI and worker's path-selection exchange.
	for {
		frame, err := child.ReadValue()
		if err != nil {
			return err
		}
		if frame.Kind() == '"' && string(frame) == resourceWorkerStartupFrame {
			break
		}
		if frame.Kind() != '{' {
			return errors.New("invalid resource worker frame")
		}
		if err = enc.WriteValue(frame); err != nil {
			return err
		}
	}
	var startup resourceStartup
	if err = json.UnmarshalDecode(child, &startup); err != nil {
		return err
	}
	if startup.Error != "" {
		return json.MarshalEncode(enc, startup)
	}
	stop := func() error {
		if config.Forward != nil {
			releaseWorker()
			return nil
		}
		control.Lock()
		defer control.Unlock()
		// A hung worker cannot keep daemon task control blocked on its pipe.
		timer := time.AfterFunc(resourceStopTimeout, killWorker)
		defer timer.Stop()
		if err := json.MarshalEncode(childInput, "kill"); err != nil {
			return err
		}
		var reply resourceReply
		if err := json.UnmarshalDecode(child, &reply); err != nil {
			return fmt.Errorf("resource worker stopped without a stop result: %w", err)
		}
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		releaseWorker()
		return nil
	}
	if err = ready(owner, startup.resourceRecord, commit, stop); err != nil {
		return err
	}
	<-exited
	return nil
}

const resourceWorkerStartupFrame = `"startup"`

func writeResourceWorkerStartup(enc *jsontext.Encoder, result resourceStartup) error {
	if err := enc.WriteValue(jsontext.Value(resourceWorkerStartupFrame)); err != nil {
		return err
	}
	return json.MarshalEncode(enc, result)
}

func resourceWorkerReader(r io.Reader) func(any) error {
	// The daemon encodes one JSON record per line. Bound each record so a
	// long-lived control stream never exhausts a cumulative input budget.
	input := bufio.NewScanner(r)
	input.Buffer(nil, daemonBodyLimit)
	return func(value any) error {
		if input.Scan() {
			return json.Unmarshal(input.Bytes(), value)
		}
		if err := input.Err(); err != nil {
			return err
		}
		return io.EOF
	}
}

func runResourceWorkerCommand(kind string) (code int, resultErr error) {
	read := resourceWorkerReader(os.Stdin)
	enc := jsontext.NewEncoder(os.Stdout)
	ready := false
	defer func() {
		if !ready && resultErr != nil {
			_ = writeResourceWorkerStartup(enc, resourceStartup{Error: cleanText(resultErr.Error())})
		}
	}()
	var config resourceConfig
	if err := read(&config); err != nil {
		return 1, err
	}
	if config.kind() != kind || !validTaskUID(config.UID) {
		return 1, fmt.Errorf("%s worker requires a registered %s configuration", kind, kind)
	}
	publish := func(record resourceRecord) error {
		err := writeResourceWorkerStartup(enc, resourceStartup{resourceRecord: record})
		ready = err == nil
		return err
	}
	if kind == "mount" {
		return runMountWorker(config, read, enc, publish)
	}
	return 0, runForwardWorker(config, read, publish)
}

func runForwardWorker(config resourceConfig, read func(any) error, publish func(resourceRecord) error) error {
	f := *config.Forward
	if err := validateForwardConfig(f); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var unused string
		_ = read(&unused)
		cancel()
	}()
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	service := &resourceService{
		record: resourceRecord{UID: config.UID, Kind: "forward", Name: f.Name, Host: f.Host, Direction: f.direction()},
		auth:   config.Auth, directory: dir,
	}
	return runForward(ctx, f, service, func() error { return publish(service.record) })
}
