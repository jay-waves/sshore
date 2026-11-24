package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

const rootConsoleHelp = `shell [--pin]     Open a shell on this host (alias: sh)
attach [ID|name]  Resume a detached shell
pstree (ps)       Show the shell tree
list (ls)         Show task details
exit              Leave sshore`

func rootConsoleOptions(current options, args []string) (*options, error) {
	command := canonicalCommand(args[0])
	next := &options{command: command, local: current.local, host: current.host,
		server: current.server, serverSet: current.serverSet, ssh: current.ssh,
		sshDirectory: current.sshDirectory, environment: current.environment, noEscape: current.noEscape}
	switch command {
	case "shell":
		parsed, err := parseOptions(args)
		if err != nil {
			return nil, err
		}
		if len(parsed.ssh) != 0 || parsed.serverSet || !parsed.local {
			return nil, usage("use shell [--pin] [:NAME] to open a shell on this host")
		}
		next.command = parsed.command
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
		}
		if next.local {
			if err := resolveDetachedAttach(next, ""); err != nil {
				return nil, err
			}
		}
	default:
		return nil, usage("unknown command %q", args[0])
	}
	if !next.local && current.routeParent != nil {
		if command == "shell" {
			return nil, fmt.Errorf("opening a new shell requires a direct connection to this host")
		}
		next.routeParent, next.routeToken, next.routeTarget = current.routeParent, current.routeToken, true
	}
	return next, nil
}

// Only the SSH callback that actually prompts borrows the terminal. The console
// can still consume Esc while DNS, the handshake or the inventory reply is pending.
func rootConsolePrompt[T any](requests chan<- func(), ask func(context.Context, string) (T, error)) func(context.Context, string) (T, error) {
	return func(ctx context.Context, prompt string) (answer T, err error) {
		done := make(chan struct{})
		select {
		case <-ctx.Done():
			return answer, ctx.Err()
		case requests <- func() {
			answer, err = ask(ctx, prompt)
			close(done)
		}:
			<-done
			return answer, err
		}
	}
}

type rootInventoryResult struct {
	client *sshio.Client
	body   string
	err    error
}

func loadRootConsoleInventory(ctx context.Context, current options, name string, client *sshio.Client, prompts chan<- func(), results chan<- rootInventoryResult) {
	current.command, current.newName = "console-inventory", name
	connection := sshConnectionOptions(current)
	var body string
	var err error
	if client == nil {
		connection.Diagnostic = io.Discard
		connection.Confirm = rootConsolePrompt(prompts, connection.Confirm)
		connection.Secret = rootConsolePrompt(prompts, connection.Secret)
		client, err = sshio.Dial(ctx, connection)
	}
	if err == nil {
		body, err = remoteRootConsoleInventory(ctx, client, connection.Command)
	}
	if err != nil && client != nil {
		client.Close()
		client = nil
	}
	select {
	case results <- rootInventoryResult{client, body, err}:
	case <-ctx.Done():
		if client != nil {
			client.Close()
		}
	}
}

func remoteRootConsoleInventory(ctx context.Context, client *sshio.Client, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	process, err := client.Start(command, io.Discard)
	if err != nil {
		return "", err
	}
	defer process.CloseAndWait()
	if err := process.CloseWrite(); err != nil {
		return "", err
	}
	reply, err := readMessage(process)
	if err != nil {
		return "", err
	}
	if reply.Type != "console_inventory" {
		return "", fmt.Errorf("invalid task inventory response")
	}
	return reply.Output, nil
}

func formatSessionEnd(ended sessionEnd, current options, destination string) string {
	if ended.title == "" {
		return ""
	}
	var notice strings.Builder
	notice.WriteString("[sshore] " + ended.title)
	if ended.detail != "" {
		notice.WriteString("\n  " + ended.detail)
	}
	if destination != "" {
		notice.WriteString("\nNow in: " + cleanText(destination))
	}
	if len(ended.retained) == 0 {
		if ended.exit {
			notice.WriteString("\nNo pinned shells remain from this subtree.")
		}
		return notice.String()
	}
	notice.WriteString("\nPinned shells become independent roots:")
	const shown = 8
	for _, child := range ended.retained[:min(shown, len(ended.retained))] {
		host := child.Host
		if host == "" {
			host = current.host
			if current.local {
				host = "here"
			}
		}
		name := taskDisplayID(taskShortCode(identityFromUID(child.Instance)), child.Name)
		endpoint := sshio.FormatEndpoint(host, name)
		pin := "pinned indefinitely"
		if !child.PinUntil.IsZero() {
			pin = "pinned until " + child.PinUntil.Local().Format(time.DateTime)
		}
		fmt.Fprintf(&notice, "\n  %s · %s", cleanText(endpoint), pin)
	}
	if more := len(ended.retained) - shown; more > 0 {
		fmt.Fprintf(&notice, "\n  … and %d more", more)
	}
	return notice.String()
}

func runRootConsole(current options, problem string, ended sessionEnd, returnTo string) (*options, error) {
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
	hint := "No shell attached. Use attach or shell, or type exit to leave."
	if returnTo != "" {
		hint = "Use exit or Esc to return to " + cleanText(returnTo) + "."
	}
	termui.WriteConsoleBody(terminalOutput, formatSessionEnd(ended, current, "temporary session"))
	termui.WriteConsoleBody(terminalOutput, hint)
	console := &commandConsole{}
	console.start(ctx, terminalOutput, emit, host, cols, color)
	defer console.editor.Close()
	startKeyboard := func() (func(), error) {
		keyCtx, cancel := context.WithCancel(ctx)
		stop, err := pty.StartKeyboard(keyCtx, func(data []byte, err error) {
			select {
			case events <- clientEvent{input: data, err: err}:
			case <-keyCtx.Done():
			}
		})
		if err != nil {
			cancel()
			return nil, err
		}
		return func() { cancel(); stop() }, nil
	}
	stopKeyboard, err := startKeyboard()
	if err != nil {
		return nil, err
	}
	defer func() {
		if stopKeyboard != nil {
			stopKeyboard()
		}
	}()
	var remote *sshio.Client
	defer func() {
		if remote != nil {
			remote.Close()
		}
	}()
	prompts := make(chan func())
	results := make(chan rootInventoryResult)
	var pendingInventory, queuedInventory string
	loadInventory := func(command string) {
		if pendingInventory != "" {
			queuedInventory = command
			return
		}
		pendingInventory = command
		if current.local {
			go func() {
				reply := consoleInventory(message{Name: command, Color: color, Cols: uint16(cols)}, "")
				select {
				case results <- rootInventoryResult{body: reply.Output}:
				case <-ctx.Done():
				}
			}()
			return
		}
		go loadRootConsoleInventory(ctx, current, command, remote, prompts, results)
	}
	body := ""
	if returnTo == "" {
		if current.local || problem == "" && current.routeParent == nil {
			loadInventory("pstree")
			body = "Loading task inventory…"
		} else {
			body = "Use pstree or list to show this host's tasks."
		}
	}
	if problem != "" {
		body = cleanText(problem) + "\n\n" + body
	}
	console.show(consoleInventoryBody(body, rows))
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case ask := <-prompts:
			stopKeyboard()
			ask()
			stopKeyboard, err = startKeyboard()
			if err != nil {
				return nil, err
			}
		case result := <-results:
			completed := pendingInventory
			pendingInventory = ""
			remote = result.client
			if queuedInventory != "" && (queuedInventory != completed || result.err != nil) {
				command := queuedInventory
				queuedInventory = ""
				loadInventory(command)
				continue
			}
			queuedInventory = ""
			if result.err != nil {
				result.body = "Task inventory unavailable: " + cleanText(result.err.Error())
			}
			console.show(consoleInventoryBody(result.body, rows))
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
					if returnTo != "" {
						console.show(hint)
					} else {
						console.show(rootConsoleHelp)
					}
				case "list", "pstree":
					if !current.local && current.routeParent != nil {
						console.show("Showing this host's tasks requires a direct SSH connection.")
						continue
					}
					loadInventory(command)
				default:
					if returnTo != "" {
						console.show(hint)
						continue
					}
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
