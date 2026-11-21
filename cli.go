package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sshio "github.com/jay-waves/sshore/internal/ssh"
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
		case "--version", "-V", "--help", "-h", "--tldr", "help", remoteServiceCommand, "pstree", "reset", "list":
		default:
			allowed = false
		}
		if !allowed {
			return 1, disabledFeaturesError()
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}

	switch args[0] {
	case "ping":
		return runPing(args[1:])
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
	case "install":
		return runInstall(args[1:])
	case remoteServiceCommand:
		return 0, runRemoteService(args[1:])
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
	case "launch", "spawn", "attach", "rename", "kill", "list":
		return runTargetCommand(args)
	case "mount":
		return runMount(args[1:])
	case "pstree":
		return runPSTree(args[1:])
	case "forward":
		return runEndpointForward(args[1:])
	case "cp":
		return runCopy(args[1:])
	case "copy-id":
		return runCopyID(args[1:])
	case "detach":
		if len(args) != 1 {
			return 2, usage("detach takes no arguments")
		}
		if identity := os.Getenv("SSHORE_LOCAL_SESSION"); identity != "" {
			return 0, detachLocalShell(identity)
		}
		return 0, detachHelper()
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

// Route task selectors before command semantics; no execution starts while
// selecting a route. Ordinary shell and kill forms share one parsed result.
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
	if selected && target.Kind != "shell" {
		switch command {
		case "kill":
			return withDaemonActivity(func() (int, error) { return 0, stopResource(target.Kind, target.Name, target.Identity) })
		case "rename":
			return withDaemonActivity(func() (int, error) {
				return runRenameResource(target.Kind, target.Name, parsed.operands[1], target.Identity)
			})
		}
	}
	if command == "kill" && parsed.has("--all") {
		o, err := killAllOptions(parsed)
		if err != nil {
			return 2, err
		}
		return runKillAll(o)
	}
	o, err := shellOptions(command, parsed)
	if err != nil {
		return 2, err
	}
	if selected && target.Kind == "shell" {
		o.taskIdentity, o.instance = target.Identity, target.Instance
		o.hosted = target.Hosted
		if target.Connection != nil {
			applyConnectionOptions(&o, *target.Connection)
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
