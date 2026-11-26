package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/term"
	"github.com/jay-waves/sshore/internal/termui"
	xterm "golang.org/x/term"
)

func rootConsoleOptions(current options, command cliArguments) (*options, error) {
	if command.command == "attach" {
		next := &options{command: "attach", local: true, environment: current.environment,
			noEscape: current.noEscape, session: cmp.Or(command.task.ref, command.operand(0))}
		return next, resolveDetachedAttach(next, "")
	}
	if command.remoteCommand != "" {
		return nil, usage("run sh HOST -- COMMAND from your shell")
	}
	next, err := shellOptions(command, selectedTask{})
	if err != nil {
		return nil, err
	}
	if !next.local {
		return nil, usage("use shell [here:PATH] [--name NAME] to open a shell on this host")
	}
	next.environment, next.noEscape = current.environment, current.noEscape
	return &next, nil
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
	restore, err := term.RawConsole()
	if err != nil {
		return nil, err
	}
	defer restore()
	terminalOutput, flush := term.OutputWriter()
	defer flush()
	screen := term.InputState{}
	if err := screen.BeginConsole(terminalOutput); err != nil {
		return nil, err
	}
	defer screen.EndConsole(terminalOutput)
	cols, rows, err := xterm.GetSize(int(os.Stdout.Fd()))
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
	console := openCommandConsole(ctx, emit, "local", cols, color, false)
	defer console.editor.Close()
	keyCtx, cancelKeyboard := context.WithCancel(ctx)
	stopKeyboard, err := term.StartKeyboard(keyCtx, func(data []byte, err error) {
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
				command, ok := console.readCommand(*event.console.Line, panel.localDirectory)
				if !ok {
					continue
				}
				switch command.command {
				case "exit":
					return nil, nil
				case "list", "pstree":
					loadInventory(command.command)
					continue
				}
				next, err := rootConsoleOptions(current, command)
				if err == nil {
					return next, nil
				}
				console.show(err.Error())
			}
		}
	}
}
