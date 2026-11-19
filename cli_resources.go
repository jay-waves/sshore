package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/jay-waves/sshore/internal/termui"
)

func runMount(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteMountRequest(args)
		if err != nil {
			return 2, err
		}
		op.Path, err = absoluteLocalPath(op.Path)
		if err != nil {
			return 1, err
		}
		info, err := os.Stat(op.Path)
		if err != nil {
			return 1, err
		}
		if !info.IsDir() {
			return 1, fmt.Errorf("mount source must be a directory")
		}
		return 0, requestOperations("mount", []operation{op})
	}

	o, err := parseFTPOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"mount"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	return executeMount(o)
}

func runCopy(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		kind, paths, hint, err := remoteCopyRequest(args)
		if err != nil {
			return 2, err
		}
		ops, err := prepareFileRequest(kind, paths, hint)
		if err != nil {
			return 1, err
		}
		return 0, requestOperations("cp", ops)
	}
	o, err := parseCopyCommand(args)
	if err != nil {
		return 2, err
	}
	return executeCopyCommand(o)
}

func runEndpointForward(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteForwardRequest(args)
		if err != nil {
			return 2, err
		}
		return 0, requestOperations("forward", []operation{op})
	}
	f, foreground, err := parseForwardCommand(args)
	if err != nil {
		return 2, err
	}
	return executeEndpointForward(f, foreground)
}

func executeEndpointForward(f forwardConfig, foreground bool) (int, error) {
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if f.Port == 0 || f.Target == 0 {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		defer input.Close()
		label := "Local listening port"
		remote := f.Target
		if f.Reverse {
			label = "Local service port"
			remote = f.Port
		}
		termui.Log(os.Stdout, termui.Info, "Remote %s:%d. Submit local port to forward.\n", cleanText(f.Host), remote)
		port, err := readForwardPort(input, label)
		if err != nil {
			return 1, err
		}
		if f.Reverse {
			f.Target = port
		} else {
			f.Port = port
		}
		if err = input.Close(); err != nil {
			return 1, err
		}
	}
	if foreground {
		return runForwardForeground(f)
	}
	return startForwardSession(f)
}
