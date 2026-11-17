package main

import (
	"context"
	"errors"
	"fmt"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"os"
	"os/signal"
	"path/filepath"
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
		if _, public := commandHelp[args[0]]; public {
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
		switch args[0] {
		case "--version", "-V", "--help", "-h", "--tldr", "help", "sessions", "inventory":
		default:
			return 1, fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}
	switch args[0] {
	case "exec":
		return runExec(args[1:])
	case "mcp":
		return runMCP(args[1:])
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
	case "cancel-resource":
		if len(args) != 4 {
			return 2, usage("invalid resource cancellation")
		}
		if err := checkPeerVersion(args[1], "client"); err != nil {
			return 1, err
		}
		return 0, cancelResourceIdentity(args[2], args[3])
	case "local-daemon":
		return 0, runLocalDaemon()
	case "launch", "spawn", "attach", "rename", "cancel", "list", "review":
		return runTargetCommand(args)
	case "ftp-worker", "resource-worker":
		return runResourceWorker()
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
	case "forward-watch", "mount-agent", "cp-agent", "bridge", "resume", "daemon", "sessions", "bootstrap", "kill-session", "rename-session", "task-watch", "task-remove", "inventory":
		return 0, serverCommand(args)
	}
	return 2, usage("unknown command: %s", args[0])
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

var errHelp = errors.New("help requested")

// --all selects a snapshot, never a process name or a cached PID.
func parseCancelAll(args []string) (options, bool, error) {
	parsed, err := parseCLI(args, "cancel")
	if !parsed.has("--all") {
		return options{}, false, nil
	}
	if err != nil {
		return options{}, true, err
	}
	o, err := cancelAllOptions(parsed)
	return o, true, err
}

func cancelAllOptions(parsed cliArguments) (options, error) {
	o, err := shellOptions("list", parsed)
	if o.local {
		o.host = ""
	}
	return o, err
}

type reviewOptions struct {
	id   string
	stay bool
}

func parseReview(args []string) (reviewOptions, error) {
	var o reviewOptions
	parsed, err := parseCLI(args, "review")
	if err != nil {
		return o, err
	}
	return reviewArguments(parsed)
}

func reviewArguments(parsed cliArguments) (reviewOptions, error) {
	var o reviewOptions
	o.stay = parsed.has("--stay")
	if len(parsed.operands) == 1 {
		a := strings.TrimPrefix(parsed.operands[0], "review/")
		if !validApprovalID(a) && validSession(a) != nil {
			return o, usage("invalid review name")
		}
		o.id = a
	}
	return o, nil
}

func parseCancelTarget(args []string) (kind, name string, typed bool, err error) {
	if len(args) == 0 {
		return "", "", false, usage("use sshore cancel review/NAME, mount/NAME, forward/NAME, or HOST NAME")
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
		err = usage("peer tasks are read-only here; cancel the resource on its managing computer")
	case "review":
		if !validApprovalID(name) && validSession(name) != nil {
			err = usage("invalid approval ID")
		}
	case "mount", "forward":
	default:
		err = usage("resource ID must be review/NAME, mount/NAME or forward/NAME")
	}
	return
}

// Route task selectors before command semantics; no execution starts while
// selecting a route. Ordinary shell and cancel forms share one parsed result.
func runTargetCommand(args []string) (int, error) {
	command := args[0]
	parsed, err := parseCLI(args[1:], command)
	if err != nil {
		return 2, err
	}
	target, selected, err := resolveShortTask(command, &parsed)
	if err != nil {
		return 2, err
	}
	if !selected && (command == "attach" || command == "cancel" || command == "review") && len(parsed.operands) == 1 && !parsed.has("--local") && !parsed.has("--all") {
		ref := parsed.operands[0]
		if ref != "" && strings.Trim(ref, "0123456789") == "" {
			return 2, usage("list sequence numbers are display-only; select the task by name or short ID")
		}
	}
	if selected && target.kind != "shell" {
		switch command {
		case "cancel":
			return 0, cancelResourceIdentity(target.kind+"/"+target.name, target.identity)
		case "rename":
			return runRenameResource(target.kind, target.name, parsed.operands[1], target.identity)
		case "review":
			parsed.operands = []string{target.requestID}
		}
	}
	if command == "review" {
		a, err := reviewArguments(parsed)
		if err != nil {
			return 2, err
		}
		return executeReview(a)
	}
	if command == "cancel" {
		if parsed.has("--all") {
			o, err := cancelAllOptions(parsed)
			if err != nil {
				return 2, err
			}
			return runCancelAll(o)
		}
		kind, name, typed, err := parseCancelTarget(parsed.operands)
		if err != nil {
			return 2, err
		}
		if typed {
			if len(parsed.values) != 0 || len(parsed.ssh) != 0 {
				return 2, usage("local cancellation accepts exactly one resource ID")
			}
			return runCancelResource(kind, name)
		}
	}
	if command == "rename" {
		if kind, old, typed := strings.Cut(parsed.operands[0], "/"); typed {
			if len(parsed.operands) != 2 || len(parsed.values) != 0 || len(parsed.ssh) != 0 {
				return 2, usage("use rename TYPE/NAME NEW")
			}
			return runRenameResource(kind, old, parsed.operands[1])
		}
	}
	o, err := shellOptions(command, parsed)
	if err != nil {
		return 2, err
	}
	if selected && target.kind == "shell" {
		o.taskIdentity, o.instance = target.identity, target.instance
		if !o.local {
			if target.connection != nil {
				applyConnectionOptions(&o, *target.connection)
			} else if err := applyRecentConnection(&o); err != nil {
				return 1, err
			}
		}
	}
	return executeShellCommand(o)
}

func runReset() error {
	if _, err := persistentMachineIdentity(true); err != nil {
		return err
	}
	// Keep runtime ownership, approval state, task IDs and lock inodes intact.
	lock, err := lockShellCache()
	if err != nil {
		return err
	}
	defer lock.Close()
	identityLock, _, err := lockIdentity()
	if err != nil {
		return err
	}
	defer identityLock.Close()
	for _, name := range []string{"shells", "shell-tree", "connections", "identity"} {
		dir, err := statePath(name)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	fmt.Println("Identity reset; local history and cached snapshots cleared. Active tasks are preserved.")
	return nil
}

// runExec delegates execution and SSH configuration to the system client.
func runExec(args []string) (int, error) {
	parsed, err := parseCLI(args, "exec")
	if err != nil {
		return 2, err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{Host: parsed.operands[0], Command: parsed.operands[1], Confirm: confirmSSHHost, Secret: promptSSHSecret}
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		var exit interface {
			error
			ExitStatus() int
		}
		if errors.As(err, &exit) {
			return exit.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}
