package main

import (
	"context"
	"errors"
	"fmt"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func run(args []string) (code int, resultErr error) {
	defer func() {
		if errors.Is(resultErr, errHelp) && len(args) > 0 {
			code, resultErr = 0, showHelp(args[:1], os.Stdout)
		}
	}()
	if len(args) > 0 {
		args = append([]string(nil), args...)
		args[0] = canonicalCommand(args[0])
		if cliCommands[args[0]].help != "" {
			if len(args) == 2 && args[1] == "--tldr" {
				return 0, showTLDR(args[0], os.Stdout)
			}
			for i, arg := range args[1:] {
				if args[0] == "exec" && i > 0 {
					break
				}
				if arg == "--" {
					break
				}
				if arg == "--help" || arg == "-h" {
					return 0, showHelp(args[:1], os.Stdout)
				}
			}
		}
	}
	if os.Getenv("SSHORE_DISABLED") == "1" && len(args) > 0 {
		allowed := true
		switch args[0] {
		case "--version", "-V", "--help", "-h", "--tldr", "help", "sessions", "inventory", "pstree", "reset":
		case "list":
			o, err := parseOptions(args)
			if err != nil {
				return 2, err
			}
			allowed = o.local || o.host == ""
		default:
			allowed = false
		}
		if !allowed {
			return 1, fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}

	switch args[0] {
	case "exec":
		return runExec(args[1:])
	case "reset":
		if len(args) != 1 {
			return 2, usage("reset takes no arguments")
		}
		return 0, runReset()
	case "--tldr":
		if len(args) != 1 {
			return 2, usage("usage: sshore --tldr or sshore COMMAND --tldr")
		}
		return 0, showTLDR("", os.Stdout)
	case "help":
		return 0, showHelp(args[1:], os.Stdout)
	case "-h", "--help":
		return 0, showHelp(nil, os.Stdout)
	case "-V", "--version":
		if len(args) != 1 {
			return 2, usage("--version takes no arguments")
		}
		fmt.Println("sshore", version)
		return 0, nil
	case "kill-resource":
		if len(args) != 4 {
			return 2, usage("invalid resource cancellation")
		}
		if err := checkPeerVersion(args[1], "client"); err != nil {
			return 1, err
		}
		return 0, killResourceIdentity(args[2], args[3])
	case "install":
		return runInstall(args[1:])
	case "daemon-admin":
		if len(args) != 1 {
			return 2, usage("daemon-admin takes no arguments")
		}
		return 0, serveDaemonAdmin(os.Stdin, os.Stdout)
	case "daemon":
		parsed, err := parseCLI(args[1:], "daemon")
		if err != nil {
			return 2, err
		}
		if len(parsed.operands) == 0 {
			return 0, runDaemon()
		}
		if parsed.operands[0] != "mount" {
			return 2, usage("usage: sshore daemon [mount]")
		}
		return runMountDaemon()
	case "launch", "spawn", "attach", "rename", "kill", "list", "review":
		return runTargetCommand(args)
	case "mount":
		return runMount(args[1:])
	case "pstree":
		return runPSTree(args[1:])
	case "forward":
		return runEndpointForward(args[1:])
	case "cp":
		return runCopy(args[1:])
	case "detach":
		if len(args) != 1 {
			return 2, usage("detach takes no arguments")
		}
		if identity := os.Getenv("SSHORE_LOCAL_SESSION"); identity != "" {
			return 0, detachLocalShell(identity)
		}
		return 0, serverCommand(args)
	case "forward-watch", "mount-agent", "cp-agent", "bridge", "resume", "sessions", "kill-session", "rename-session", "task-watch", "task-remove", "inventory":
		return 0, serverCommand(args)
	}
	return 2, usage("unknown command: %s", args[0])
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

var errHelp = errors.New("help requested")

func killAllOptions(parsed cliArguments) (options, error) {
	o, err := shellOptions("list", parsed)
	if o.local {
		o.host = ""
	}
	return o, err
}

type reviewOptions struct {
	id string
}

func reviewArguments(parsed cliArguments) (reviewOptions, error) {
	var o reviewOptions
	if len(parsed.operands) == 1 {
		a := strings.TrimPrefix(parsed.operands[0], "review/")
		if !validRequestID(a) && validSession(a) != nil {
			return o, usage("invalid review name")
		}
		o.id = a
	}
	return o, nil
}

func parseKillTarget(args []string) (kind, name string, typed bool, err error) {
	if len(args) == 0 {
		return "", "", false, usage("use sshore kill review/NAME, mount/NAME, forward/NAME, or HOST NAME")
	}
	kind, name, typed = strings.Cut(args[0], "/")
	if !typed {
		return
	}
	if len(args) != 1 {
		err = usage("local cancellation accepts exactly one resource ID")
		return
	}
	switch kind {
	case "peer":
		err = usage("peer tasks are read-only here; kill the resource on its managing computer")
	case "review":
		if !validRequestID(name) && validSession(name) != nil {
			err = usage("invalid approval ID")
		}
	case "mount", "forward":
	default:
		err = usage("resource ID must be review/NAME, mount/NAME or forward/NAME")
	}
	return
}

// Route task selectors before command semantics; no execution starts while
// selecting a route. Ordinary shell and kill forms share one parsed result.
func runTargetCommand(args []string) (int, error) {
	command := args[0]
	parsed, err := parseCLI(args[1:], command)
	if err != nil {
		return 2, err
	}
	if (command == "attach" || command == "kill" || command == "review") && len(parsed.operands) == 1 && !parsed.local && !parsed.has("--all") && !parsed.explicitHost {
		ref := parsed.operands[0]
		if ref != "" && !isTaskIDPrefix(ref) && strings.Trim(ref, "0123456789") == "" {
			return 2, usage("list sequence numbers are display-only; select the task by name or short ID")
		}
	}
	operandCount := len(parsed.operands)
	target, selected, err := resolveShortTask(command, &parsed)
	if err != nil {
		return 2, err
	}
	if command == "attach" && !parsed.explicitHost && operandCount != 0 && !(operandCount == 1 && selected) {
		return 2, usage("use attach ID or attach HOST:ID; run sshore list for available shells")
	}

	if selected && target.kind != "shell" {
		switch command {
		case "kill":
			return withDaemonActivity(func() (int, error) { return 0, killResourceIdentity(target.kind+"/"+target.name, target.identity) })
		case "rename":
			return withDaemonActivity(func() (int, error) {
				return runRenameResource(target.kind, target.name, parsed.operands[1], target.identity)
			})
		case "review":
			parsed.operands = []string{canonicalTaskUID(target.identity)}
		}
	}
	if command == "review" {
		a, err := reviewArguments(parsed)
		if err != nil {
			return 2, err
		}
		return withDaemonActivity(func() (int, error) { return executeReview(a) })
	}
	if command == "kill" {
		if parsed.has("--all") {
			o, err := killAllOptions(parsed)
			if err != nil {
				return 2, err
			}
			return runKillAll(o)
		}
		kind, name, typed, err := parseKillTarget(parsed.operands)
		if err != nil {
			return 2, err
		}
		if typed {
			if len(parsed.values) != 0 || len(parsed.ssh) != 0 {
				return 2, usage("local cancellation accepts exactly one resource ID")
			}
			return withDaemonActivity(func() (int, error) { return runKillResource(kind, name) })
		}
	}
	if command == "rename" {
		if kind, old, typed := strings.Cut(parsed.operands[0], "/"); typed {
			if len(parsed.operands) != 2 || len(parsed.values) != 0 || len(parsed.ssh) != 0 {
				return 2, usage("use rename TYPE/NAME NEW")
			}
			return withDaemonActivity(func() (int, error) { return runRenameResource(kind, old, parsed.operands[1]) })
		}
	}
	if !parsed.explicitHost && (command == "rename" || command == "kill" && !(selected && target.host == "")) {
		return 2, usage("use %s HOST:ID or %s self:ID", command, command)
	}
	o, err := shellOptions(command, parsed)
	if err != nil {
		return 2, err
	}
	if selected && target.kind == "shell" {
		o.taskIdentity, o.instance = target.identity, target.instance
		o.hosted = target.hosted
		if !o.local {
			if target.snapshot != nil && target.snapshot.Connection != nil {
				applyConnectionOptions(&o, *target.snapshot.Connection)
			} else if err := applyRecentConnection(&o); err != nil {
				return 1, err
			}
		}
	}
	return withDaemonActivity(func() (int, error) { return executeShellCommand(o) })
}

// runExec delegates execution and SSH configuration to the system client.
type exitStatusError interface {
	error
	ExitStatus() int
}

func runExec(args []string) (int, error) {
	parsed, err := parseCLI(args, "exec")
	if err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{Host: parsed.operands[0], Command: parsed.operands[1], Confirm: confirmSSHHost, Secret: promptSSHSecret}
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if exit, ok := errors.AsType[exitStatusError](err); ok {
			return exit.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}
