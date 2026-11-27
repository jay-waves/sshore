package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/term"
	"github.com/jay-waves/sshore/internal/termui"
	xterm "golang.org/x/term"
)

// clientConsole lives with the frontend. Each visible commandConsole owns only
// its editor, pending requests and buffered terminal output.
type clientConsole struct {
	directory, localDirectory string
}

type commandConsole struct {
	output     []byte
	editor     *termui.Console
	targets    []sessionTarget
	attached   bool
	generation uint64
	newEditor  func(uint64) *termui.Console
}

const consoleTargetLookup = "console:targets:"
const consoleAttachLookup = "attach:"
const consoleParentLookup = "attach:parent:"

const consoleHelp = `pstree (ps)       Show the task tree
list (ls)         Show task details
pwd               Show the current directory
attach [ID|name]  Resume a session (alias: a)
ID                Resume a task by ID
shell [--pin]     Open a shell (alias: sh)
lf                Browse folders and open a shell
exit              Close the command console`

func openCommandConsole(ctx context.Context, send func(clientEvent), host string, width int, color, attached bool) *commandConsole {
	c := &commandConsole{attached: attached}
	const prefix = "sshore on "
	const suffix = "> "
	host = ansi.Truncate(cleanText(host), max(1, width-len(prefix)-len(suffix)-4), "…")
	prompt := termui.FormatPrompt(color, prefix+host+suffix)
	c.newEditor = func(generation uint64) *termui.Console {
		return termui.StartConsole(ctx, prompt, width, color, func(e termui.ConsoleEvent) {
			send(clientEvent{consoleSource: c, consoleGeneration: generation, console: e})
		})
	}
	c.startEditor()
	return c
}

func (c *commandConsole) startEditor() {
	c.generation++
	c.editor = c.newEditor(c.generation)
}

func (c *commandConsole) stopEditor() {
	if c.editor != nil {
		c.editor.Close()
		c.editor = nil
	}
}

// Move to the end, then clear the whole Readline draft.
func (c *commandConsole) clearInput() {
	c.editor.TryInput([]byte{5, 21})
}

func (c *commandConsole) accepts(event clientEvent) bool {
	return c != nil && c.editor != nil && event.consoleSource == c && event.consoleGeneration == c.generation
}

func (c *commandConsole) resumeAfterPicker(out io.Writer, err error) error {
	if err := writeFull(out, []byte("\x1b[2J\x1b[H")); err != nil {
		return err
	}
	c.startEditor()
	if err != nil && !errors.Is(err, context.Canceled) {
		c.show(err.Error())
	} else {
		c.show("")
	}
	return nil
}

type consoleCompleter struct {
	targets  []sessionTarget
	attached bool
}

func (completion consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	word := prefix
	candidates := []string{"attach", "exit", "help", "lf", "shell", "list", "pstree", "pwd"}
	if completion.attached {
		candidates = append(candidates, "detach")
	}
	for alias, command := range commandAliases {
		if slices.Contains(candidates, command) {
			candidates = append(candidates, alias)
		}
	}
	command, rest, ok := strings.Cut(prefix, " ")
	if ok && canonicalCommand(command) == "attach" {
		word, candidates = rest, nil
		for _, target := range completion.targets {
			candidates = append(candidates, target.Name)
		}
	}
	if !ok || canonicalCommand(command) == "attach" {
		for _, target := range completion.targets {
			candidates = append(candidates, taskShortCode(identityFromUID(target.Instance)))
		}
	}

	slices.Sort(candidates)
	var results [][]rune
	for _, candidate := range slices.Compact(candidates) {
		if suffix, ok := strings.CutPrefix(candidate, word); ok {
			results = append(results, []rune(suffix+" "))
		}
	}
	return results, len([]rune(word))
}

func (c *commandConsole) show(body string) {
	if c.editor != nil {
		c.editor.Show(body, consoleCompleter{targets: c.targets, attached: c.attached})
	}
}

func parseConsoleCommand(line string, attached bool) (cliArguments, error) {
	args := strings.Fields(line)
	if len(args) == 0 {
		return cliArguments{}, nil
	}
	name, operands := canonicalCommand(args[0]), args[1:]
	if _, known := cliCommands[name]; !known && len(args) == 1 && taskIDOperand(args[0]) != "" {
		name, operands = "attach", args
	}
	switch name {
	case "help", "pwd", "exit", "lf":
		if len(operands) != 0 {
			return cliArguments{}, usage("%s does not accept arguments in the command console", name)
		}
		return cliArguments{command: name}, nil
	case "detach":
		if !attached {
			return cliArguments{}, usage("detach requires an attached shell")
		}
	case "attach", "shell", "list", "pstree":
	default:
		return cliArguments{}, usage("unknown command %q", args[0])
	}
	parsed, err := parseCLI(operands, name)
	if err == nil && name == "attach" && (len(parsed.ssh) != 0 || len(parsed.values) != 0) {
		return cliArguments{}, usage("use attach [ID|name] in the command console")
	}
	return parsed, err
}

// Consume input and display built-in responses before dispatching work.
func (c *commandConsole) readCommand(line, directory string) (cliArguments, bool) {
	parsed, err := parseConsoleCommand(line, c.attached)
	switch {
	case errors.Is(err, errHelp) || parsed.command == "help" && err == nil:
		help := consoleHelp
		if c.attached {
			help += "\ndetach [DURATION] Pin and detach the current shell"
		}
		c.show(help + "\n\n" + strings.TrimRight(inventoryStateHelp, "\n"))
	case err != nil:
		c.show(err.Error())
	case parsed.command == "pwd":
		c.show(cleanText(directory))
	case parsed.command == "":
		c.show("")
	default:
		return parsed, true
	}
	return cliArguments{}, false
}

const consoleMoreRows = "… more rows in the standalone command on this host"

func consoleInventory(request message, current string) message {
	reply := message{Type: "console_inventory", Action: request.Action}
	var body bytes.Buffer
	command := canonicalCommand(request.Name)
	if command != "list" && command != "pstree" {
		reply.Output = "Unknown inventory command."
		return reply
	}
	inv, err := collectInventory()
	if command == "list" {
		writeListWidth(&body, inv, current, request.Color, int(request.Cols))
	} else {
		writePstree(&body, inv, current, request.Color)
	}
	reply.Output = strings.TrimRight(body.String(), "\n")
	if err != nil {
		reply.Output = strings.TrimRight(cleanText(err.Error())+"\n\n"+reply.Output, "\n")
	}
	// Leave room for JSON escaping and control metadata in one frame.
	const outputLimit = 8 * 1024
	if len(reply.Output) > outputLimit {
		i := strings.LastIndexByte(reply.Output[:outputLimit-len(consoleMoreRows)], '\n')
		if i < 0 {
			reply.Output = consoleMoreRows
		} else {
			reply.Output = reply.Output[:i] + "\n" + consoleMoreRows
		}
	}
	return reply
}

func consoleInventoryBody(body string, rows int) string {
	limit := max(1, rows-8)
	lines := strings.SplitN(body, "\n", limit+1)
	if len(lines) > limit {
		lines[limit] = consoleMoreRows
	}
	return strings.Join(lines, "\n")
}

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
		name := taskDisplayID(taskShortCode(identityFromUID(child.Instance)), child.Name, false)
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
	defer console.stopEditor()
	var picker *consoleFilePicker
	defer func() {
		if picker != nil {
			picker.cancel()
		}
	}()
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
	var escape <-chan time.Time
	results := make(chan string)
	var pendingInventory, queuedInventory string
	loadInventory := func(command string) {
		if pendingInventory != "" {
			queuedInventory = command
			return
		}
		pendingInventory = command
		go func() {
			reply := consoleInventory(message{Name: command, terminalDisplay: terminalDisplay{Color: color, Cols: uint16(cols)}}, "")
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
		case <-escape:
			escape = nil
			console.clearInput()
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
			if event.filePicker != nil {
				if event.filePicker != picker {
					continue
				}
				switch {
				case event.fileOutput != nil:
					if err := writeFull(terminalOutput, event.fileOutput); err != nil {
						return nil, err
					}
				case event.fileResult != nil:
					if event.fileResult.err == nil {
						next, err := rootConsoleOptions(current, cliArguments{command: "shell", startupDirectory: event.fileResult.path})
						if err == nil {
							return next, nil
						}
						event.fileResult.err = err
					}
					picker.cancel()
					picker = nil
					if err := console.resumeAfterPicker(terminalOutput, event.fileResult.err); err != nil {
						return nil, err
					}
				}
				continue
			}
			if picker != nil && len(event.input) != 0 {
				picker.send(event.input)
				continue
			}
			if event.consoleSource != nil && !console.accepts(event) {
				continue
			}
			switch {
			case event.err != nil || event.console.Closed:
				return nil, nil
			case len(event.input) != 0:
				data := event.input
				if escape != nil {
					escape = nil
					data = append([]byte{27}, data...)
				}
				if len(data) == 1 && data[0] == 27 {
					escape = time.After(50 * time.Millisecond)
					continue
				}
				if !console.editor.TryInput(data) {
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
				case "lf":
					escape = nil
					console.stopEditor()
					picker = startConsoleFilePicker(ctx, panel.localDirectory, "", false, emit)
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
