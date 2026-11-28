package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/taskid"
	"github.com/jay-waves/sshore/internal/termui"
)

func newApprovalRequest(o options, shellPID int, ops []operation) (requestRecord, error) {
	if err := validateOperations(ops); err != nil {
		return requestRecord{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return requestRecord{}, err
	}
	connection, err := connectionFromOptions(o)
	if err != nil {
		return requestRecord{}, err
	}
	request := requestRecord{Connection: connection, PID: shellPID, ID: taskid.New(), Directory: cwd, Operations: ops}
	if !taskid.Valid(connection.Instance) || request.PID <= 0 {
		return requestRecord{}, fmt.Errorf("invalid source shell identity")
	}
	if err := validateRecentConnection(connection); err != nil {
		return requestRecord{}, err
	}
	err = prepareApprovalDirectory(&request)
	return request, err
}

type approvedOperation struct {
	config resourceConfig
	files  []fileCopy
}

func selectApprovedOperation(console *termui.LocalConsole, input *termui.Input, request requestRecord) (approvedOperation, error) {
	var selected approvedOperation
	var err error
	if request.Operations[0].Kind != "forward" {
		line, err := input.ReadPrefilled("Approve this request? [y/N] ", false, "")
		if err != nil || !termui.ConfirmYes(line) {
			return selected, errors.New("approval cancelled")
		}
	}
	switch request.Operations[0].Kind {
	case "mount", "forward":
		selected.config, err = selectApprovedResource(console, input, &request)
	default:
		if request.Operations[0].LF {
			if err := input.Close(); err != nil {
				return selected, err
			}
			selected.files, err = pickApprovedUpload(console, request)
		} else {
			selected.files, err = selectApprovedFiles(input, request)
		}
	}
	return selected, err
}

type localApproval struct {
	request requestRecord
	source  options
}

func (*localApproval) Error() string { return "local approval requested" }

func executeLocalApproval(request requestRecord, source options) error {
	var selection approvedOperation
	defer func() { closeCopyBatch(selection.files) }()
	var started resourceRecord
	err := termui.RunLocalConsole(func(console *termui.LocalConsole) error {
		input, err := newLocalInput()
		if err != nil {
			return err
		}
		termui.WriteConsoleBody(os.Stdout, approvalDescription(request))
		selection, err = selectApprovedOperation(console, input, request)
		// Release stdin before the SSH broker reads credentials in this console.
		err = errors.Join(err, input.Close())
		if err == nil && selection.config.kind() != "" {
			started, err = launchResourceBackground(selection.config, request.Connection.Directory)
		}
		return err
	})
	if started.Kind != "" {
		reportResourceStartup(started)
	}
	if err == nil && len(selection.files) != 0 {
		err = runApprovedFiles(request, source, selection.files)
	}
	return err
}

func approvalDescription(request requestRecord) string {
	var body strings.Builder
	fmt.Fprintf(&body, "From %s / %s\n", cleanText(request.Connection.Host), cleanText(request.Connection.Session))
	for _, op := range request.Operations {
		switch op.Kind {
		case "mount":
			mode := "read-write"
			if op.ReadOnly {
				mode = "read-only"
			}
			fmt.Fprintf(&body, "\nMount remote directory %s (%s)", cleanText(op.Path), mode)
		case "forward":
			fmt.Fprintf(&body, "\n%s", pendingForwardDescription(op.Forward))
			continue
		case "download":
			if op.Recursive {
				fmt.Fprintf(&body, "\nDownload directory tree %s", cleanText(op.Path))
			} else {
				fmt.Fprintf(&body, "\nDownload %s (%s, %s)", cleanText(op.Path), cleanText(op.Name), displayBytes(op.Size))
			}
		case "upload":
			if op.Recursive {
				fmt.Fprintf(&body, "\nUpload a directory tree into %s", cleanText(op.Path))
			} else {
				fmt.Fprintf(&body, "\nUpload to %s", cleanText(op.Path))
			}
		}
		if op.LocalHint != "" {
			fmt.Fprintf(&body, "\nSuggested local endpoint: %s", cleanText(op.LocalHint))
		}
	}
	return body.String()
}

func prepareApprovalDirectory(r *requestRecord) error {
	if r.Connection.Directory == "" {
		r.Connection.Directory = r.Directory
	}
	for parent := r.Connection.Parent; parent != nil; parent = parent.Parent {
		if !parent.Hosted && parent.Directory == "" {
			parent.Directory = r.Connection.Directory
		}
	}
	info, err := os.Stat(r.Connection.Directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("SSH working directory is no longer a directory")
	}
	return nil
}

func selectApprovedResource(console *termui.LocalConsole, input *termui.Input, r *requestRecord) (resourceConfig, error) {
	op := r.Operations[0]
	if op.Kind == "mount" {
		hint, err := approvalPathHint(op.LocalHint)
		if err != nil {
			return resourceConfig{}, err
		}
		if err := files.CheckMountPlatform(); err != nil {
			return resourceConfig{}, err
		}
		var mountpoint string
		if op.LF {
			if err := input.Close(); err != nil {
				return resourceConfig{}, err
			}
			mountpoint, err = pickLocalMountpoint(context.Background(), console, r.Directory)
			if errors.Is(err, context.Canceled) {
				err = fmt.Errorf("approval cancelled: %w", err)
			}
		} else {
			mountpoint, err = readMountApprovalPath(input, hint)
		}
		if err != nil {
			return resourceConfig{}, err
		}
		return resourceConfig{Mount: &ftpOptions{
			Host: r.Connection.Host, Remote: op.Path, Mountpoint: mountpoint, ReadOnly: op.ReadOnly,
			IOTimeoutNanos: defaultMountIOTimeout.Nanoseconds(),
			ApprovalID:     r.ID, ResumePID: r.PID,
			Connection: &r.Connection,
		}}, nil
	}

	rule := op.Forward
	port, err := readForwardPort(input, rule)
	if err != nil {
		return resourceConfig{}, err
	}
	rule.LocalPort = port
	f := forwardConfig{
		Host: r.Connection.Host, Rule: rule,
		ResumePID: r.PID, ApprovalID: r.ID, Connection: &r.Connection,
	}
	return resourceConfig{Forward: &f}, nil
}

func approvalPathHint(hint string) (string, error) {
	if hint == "" {
		return "", nil
	}
	path, err := sys.ExpandHome(hint)
	if err != nil {
		return "", fmt.Errorf("invalid local path suggestion: %w", err)
	}
	return path, nil
}
