package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func run(args []string) (code int, resultErr error) {
	defer func() {
		if errors.Is(resultErr, errHelp) && len(args) > 0 {
			code, resultErr = 0, showHelp(args[:1], os.Stdout)
		}
	}()
	if len(args) > 0 && args[0] == "__complete" {
		return 0, runCompletionQuery(args[1:], os.Stdout)
	}
	if os.Getenv("SSHORE_ASKPASS") == "1" {
		if os.Getenv("SSHORE_DISABLED") == "1" {
			return 1, fmt.Errorf("file access disabled in recovery")
		}
		return runResourceAskpass(args)
	}
	if len(args) > 0 {
		args = append([]string(nil), args...)
		args[0] = canonicalCommand(args[0])
		if _, public := commandHelp[args[0]]; public {
			for _, arg := range args[1:] {
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
		case "--version", "-V", "--help", "-h", "help", "sessions", "inventory":
		default:
			return 1, fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}
	switch args[0] {
	case "reset":
		if len(args) != 1 {
			return 2, usage("reset takes no arguments")
		}
		return 0, runReset()
	case "completion":
		return 0, runCompletion(args[1:], os.Stdout)
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

func parseTaskID(args []string) (uint64, bool, error) {
	if len(args) < 2 {
		return 0, false, nil
	}
	switch args[0] {
	case "attach", "cancel", "review", "rename":
	default:
		return 0, false, nil
	}
	id, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return 0, false, nil
	}
	if id == 0 {
		return 0, true, usage("task IDs start at 1")
	}
	if args[0] == "rename" {
		if len(args) != 3 {
			return 0, true, usage("rename ID requires NEW")
		}
	} else if len(args) != 2 {
		return 0, false, nil
	}
	return id, true, nil
}

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
	if !parsed.has("--all") {
		if code, handled, err := runTaskID(args); handled {
			return code, err
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
	o, err := shellOptions(command, parsed)
	if err != nil {
		return 2, err
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
