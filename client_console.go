package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/termui"
)

// clientConsole lives with the frontend. Each visible commandConsole owns only
// its editor, pending requests and buffered terminal output.
type clientConsole struct {
	directory, localDirectory string
}

type commandConsole struct {
	output   []byte
	editor   *termui.Console
	targets  []sessionTarget
	attached bool
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
exit              Close the command console`

func openCommandConsole(ctx context.Context, send func(clientEvent), host string, width int, color, attached bool) *commandConsole {
	c := &commandConsole{attached: attached}
	const prefix = "sshore on "
	const suffix = "> "
	host = ansi.Truncate(cleanText(host), max(1, width-len(prefix)-len(suffix)-4), "…")
	prompt := termui.FormatPrompt(color, prefix+host+suffix)
	c.editor = termui.StartConsole(ctx, prompt, width, color, func(e termui.ConsoleEvent) {
		send(clientEvent{consoleSource: c, console: e})
	})
	return c
}

type consoleCompleter struct {
	targets  []sessionTarget
	attached bool
}

func (completion consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	word := prefix
	candidates := []string{"attach", "exit", "help", "shell", "list", "pstree", "pwd"}
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
	c.editor.Show(body, consoleCompleter{targets: c.targets, attached: c.attached})
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
	case "help", "pwd", "exit":
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
		c.show(help)
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
	var err error
	command := canonicalCommand(request.Name)
	switch command {
	case "list", "pstree":
		var inv inventory
		inv, err = collectInventory(command == "pstree")
		writeInventory(&body, command, inv, current, request.Color, int(request.Cols))
	default:
		reply.Output = "Unknown inventory command."
		return reply
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
