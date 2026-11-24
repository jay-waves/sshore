package main

import (
	"context"
	"fmt"
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
pwd               Show the local console directory
exit              Leave sshore`

func rootConsoleOptions(current options, args []string) (*options, error) {
	command := canonicalCommand(args[0])
	next := &options{command: command, local: true, environment: current.environment, noEscape: current.noEscape}
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
		if err := resolveDetachedAttach(next, ""); err != nil {
			return nil, err
		}
	default:
		return nil, usage("unknown command %q", args[0])
	}
	return next, nil
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

func runRootConsole(current options, panel *clientConsole, problem string, ended sessionEnd) (*options, error) {
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
	color := termui.ColorEnabled(os.Stdout)
	termui.WriteConsoleBody(terminalOutput, formatSessionEnd(ended, current, "local console"))
	termui.WriteConsoleBody(terminalOutput, "No shell attached. Use attach or shell, or type exit to leave.")
	console := panel.open(ctx, terminalOutput, emit, "local", cols, color)
	defer console.editor.Close()
	keyCtx, cancelKeyboard := context.WithCancel(ctx)
	stopKeyboard, err := pty.StartKeyboard(keyCtx, func(data []byte, err error) {
		select {
		case events <- clientEvent{input: data, err: err}:
		case <-keyCtx.Done():
		}
	})
	if err != nil {
		cancelKeyboard()
		return nil, err
	}
	defer func() {
		cancelKeyboard()
		stopKeyboard()
	}()
	results := make(chan string)
	var pendingInventory, queuedInventory string
	loadInventory := func(command string) {
		if pendingInventory != "" {
			queuedInventory = command
			return
		}
		pendingInventory = command
		go func() {
			reply := consoleInventory(message{Name: command, Color: color, Cols: uint16(cols)}, "")
			select {
			case results <- reply.Output:
			case <-ctx.Done():
			}
		}()
	}
	loadInventory("pstree")
	body := "Loading task inventory…"
	if problem != "" {
		body = cleanText(problem) + "\n\n" + body
	}
	console.show(consoleInventoryBody(body, rows))
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case result := <-results:
			completed := pendingInventory
			pendingInventory = ""
			if queuedInventory != "" && queuedInventory != completed {
				command := queuedInventory
				queuedInventory = ""
				loadInventory(command)
				continue
			}
			queuedInventory = ""
			console.show(consoleInventoryBody(result, rows))
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
				if len(args) > 1 && (command == "exit" || command == "help" || command == "list" || command == "pstree" || command == "pwd") {
					console.show("Unexpected argument: " + cleanText(args[1]))
					continue
				}
				switch command {
				case "pwd":
					console.show(cleanText(panel.localDirectory))
				case "exit":
					return nil, nil
				case "help":
					console.show(rootConsoleHelp)
				case "list", "pstree":
					loadInventory(command)
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
