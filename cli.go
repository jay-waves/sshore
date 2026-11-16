package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type cliFlag struct {
	name  string
	value bool
}

type cliArguments struct {
	operands []string
	values   map[string]string
	ssh      []string
}

func (a cliArguments) has(name string) bool { _, ok := a.values[name]; return ok }

// Only the suffix after -- belongs to OpenSSH. Product options are singular;
// repeated SSH options remain valid and retain their original order.
func parseCLI(args []string, flags []cliFlag, sshAllowed, nativeForwarding bool) (cliArguments, error) {
	a := cliArguments{values: make(map[string]string)}
	var err error
	if sshAllowed {
		args, a.ssh, err = splitSSHOptions(args, nativeForwarding)
		if err != nil {
			return a, err
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" {
			return a, usage("empty argument")
		}
		if arg == "--help" || arg == "-h" {
			return a, errHelp
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			a.operands = append(a.operands, arg)
			continue
		}
		if err := misplacedSSHOption(arg); err != nil {
			return a, err
		}
		name, value, attached := strings.Cut(arg, "=")
		found := false
		for _, flag := range flags {
			if flag.name != name {
				continue
			}
			found = true
			if a.has(name) {
				return a, usage("%s may only be specified once", name)
			}
			if flag.value {
				if !attached {
					i++
					if i == len(args) || strings.HasPrefix(args[i], "--") {
						return a, usage("missing value for %s", name)
					}
					value = args[i]
				}
				if value == "" {
					return a, usage("missing value for %s", name)
				}
			} else if attached {
				return a, usage("%s does not accept a value", name)
			}
			a.values[name] = value
			break
		}
		if !found {
			return a, usage("unsupported option: %s", name)
		}
	}
	return a, nil
}

func run(args []string) (code int, resultErr error) {
	defer func() {
		if errors.Is(resultErr, errHelp) && len(args) > 0 {
			code, resultErr = 0, showHelp(args[:1], os.Stdout)
		}
	}()
	if os.Getenv("SSHORE_ASKPASS") == "1" {
		if os.Getenv("SSHORE_DISABLED") == "1" {
			return 1, fmt.Errorf("file access disabled in recovery")
		}
		return runResourceAskpass(args)
	}
	if len(args) > 0 {
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
	case "launch", "spawn", "attach", "rename":
		return runShellCommand(args)
	case "ftp-worker", "resource-worker":
		return runResourceWorker()
	case "mount":
		return runMount(args[1:])
	case "cancel":
		return runCancel(args[1:])
	case "list":
		return runShellCommand(args)
	case "review":
		if code, handled, err := runTaskID(args); handled {
			return code, err
		}
		return runReview(args[1:])
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
	case "forward-watch", "mount-agent", "bridge", "resume", "daemon", "sessions", "bootstrap", "kill-session", "rename-session", "task-watch", "task-remove", "inventory":
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
	parsed, err := parseCLI(args, []cliFlag{{"--all", false}, {"--local", false}, {"--server", true}}, true, false)
	if !parsed.has("--all") {
		return options{}, false, nil
	}
	if err != nil {
		return options{}, true, err
	}
	filtered := append([]string{"list"}, parsed.operands...)
	if parsed.has("--local") {
		filtered = append(filtered, "--local")
	}
	if parsed.has("--server") {
		filtered = append(filtered, "--server", parsed.values["--server"])
	}
	if len(parsed.ssh) > 0 {
		filtered = append(filtered, "--")
		filtered = append(filtered, parsed.ssh...)
	}
	o, err := parseOptions(filtered)
	if o.local {
		o.host = ""
	}
	return o, true, err
}

type reviewOptions struct {
	id   string
	stay bool
}

func parseReview(args []string) (reviewOptions, error) {
	var o reviewOptions
	parsed, err := parseCLI(args, []cliFlag{{"--stay", false}}, false, false)
	if err != nil {
		return o, err
	}
	o.stay = parsed.has("--stay")
	if len(parsed.operands) > 1 {
		return o, usage("usage: sshore review [NAME] [--stay]")
	}
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
