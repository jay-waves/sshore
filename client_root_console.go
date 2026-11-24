package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/pty"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

const rootConsoleHelp = `sh [--pin]        Open a shell on this host
attach [ID|name]  Resume a detached shell
ls / ps           Show tasks on this host
exit              Leave sshore`

func rootConsoleOptions(current options, args []string) (*options, error) {
	command := canonicalCommand(args[0])
	next := &options{command: command, local: current.local, host: current.host,
		server: current.server, serverSet: current.serverSet, ssh: current.ssh,
		sshDirectory: current.sshDirectory, environment: current.environment, quiet: current.quiet, noEscape: current.noEscape}
	switch command {
	case "sh":
		parsed, err := parseOptions(args)
		if err != nil {
			return nil, err
		}
		if len(parsed.ssh) != 0 || parsed.serverSet || parsed.detached || !parsed.local {
			return nil, usage("use sh [--pin] [:NAME] to open a shell on this host")
		}
		next.session, next.pin = parsed.session, parsed.pin
		next.noLogin, next.noProfile = parsed.noLogin, parsed.noProfile
	case "attach":
		if len(args) > 2 {
			return nil, usage("use attach [ID|name]")
		}
		if len(args) == 2 {
			if err := validSession(args[1]); err != nil {
				return nil, err
			}
			next.session = args[1]
			if next.local {
				for _, target := range localSessionTargets("", message{}).Targets {
					if target.Name == next.session || target.Instance == next.session {
						next.taskIdentity, next.instance, next.hosted = identityFromUID(target.Instance), target.Instance, target.Hosted
						break
					}
				}
			}
		} else if err := resolveDetachedAttach(next); err != nil {
			return nil, err
		}
	default:
		return nil, usage("unknown command %q", args[0])
	}
	if !next.local && current.routeParent != nil {
		if command == "sh" {
			return nil, fmt.Errorf("opening a new shell requires a direct connection to this host")
		}
		next.routeParent, next.routeToken, next.routeTarget = current.routeParent, current.routeToken, true
	}
	return next, nil
}

func rootConsoleInventory(ctx context.Context, current options, name string, color bool, cols int) string {
	if current.local {
		return consoleInventory(message{Name: name, Color: color, Cols: uint16(cols)}, "").Output
	}
	if current.routeParent != nil {
		return "Showing this host's tasks requires a direct SSH connection."
	}
	query := current
	query.command, query.newName = "console-inventory", name
	connection := sshConnectionOptions(query)
	// An automatic inventory refresh shares the prompt with keyboard input.
	connection.Confirm, connection.Secret = nil, nil
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	process, err := sshio.Start(ctx, connection, io.Discard)
	if err != nil {
		return "Task inventory unavailable: " + cleanText(err.Error())
	}
	defer process.CloseAndWait()
	_ = process.CloseWrite()
	reply, err := readMessage(process)
	if err != nil || reply.Type != "console_inventory" {
		return "Invalid task inventory response."
	}
	return reply.Output
}

func runRootConsole(current options, problem string) (*options, error) {
	current = directShellOptions(current)
	ctx, _, stopSignals := newClientSignals(false)
	defer stopSignals()
	restore, err := pty.RawConsole()
	if err != nil {
		return nil, err
	}
	defer restore()
	terminalOutput, flush := pty.OutputWriter()
	defer flush()
	screen := pty.InputState{}
	if err := screen.BeginConsole(terminalOutput); err != nil {
		return nil, err
	}
	defer screen.EndConsole(terminalOutput)
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	events := make(chan clientEvent, 32)
	emit := func(event clientEvent) {
		select {
		case events <- event:
		case <-ctx.Done():
		}
	}
	host := current.host
	if current.local {
		host = "local"
	}
	color := termui.ColorEnabled(os.Stdout)
	termui.WriteConsoleBody(terminalOutput, "No shell attached. Use attach or sh, or type exit to leave.")
	console := &commandConsole{}
	console.start(ctx, terminalOutput, emit, host, cols, color)
	defer console.editor.Close()
	stopKeyboard, err := pty.StartKeyboard(ctx, func(data []byte, err error) {
		emit(clientEvent{input: data, err: err})
	})
	if err != nil {
		return nil, err
	}
	// Cancel publishers before joining the keyboard reader.
	defer func() {
		stopSignals()
		stopKeyboard()
	}()
	body := rootConsoleInventory(ctx, current, "pstree", color, cols)
	if problem != "" {
		body = cleanText(problem) + "\n\n" + body
	}
	console.show(consoleInventoryBody(body, rows))
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case event := <-events:
			switch {
			case event.err != nil || event.console.Closed:
				return nil, nil
			case len(event.input) != 0:
				if string(event.input) == "\x1b" || !console.editor.TryInput(event.input) {
					return nil, nil
				}
			case event.console.Output != nil:
				if err := writeFull(terminalOutput, event.console.Output); err != nil {
					return nil, err
				}
			case event.console.Line != nil:
				args := strings.Fields(*event.console.Line)
				if len(args) == 0 {
					console.show("")
					continue
				}
				command := canonicalCommand(args[0])
				if len(args) > 1 && (command == "exit" || command == "help" || command == "list" || command == "pstree") {
					console.show("Unexpected argument: " + cleanText(args[1]))
					continue
				}
				switch command {
				case "exit":
					return nil, nil
				case "help":
					console.show(rootConsoleHelp)
				case "list", "pstree":
					body := rootConsoleInventory(ctx, current, canonicalCommand(args[0]), color, cols)
					console.show(consoleInventoryBody(body, rows))
				default:
					next, err := rootConsoleOptions(current, args)
					if err == nil {
						return next, nil
					}
					console.show(err.Error())
				}
			}
		}
	}
}
