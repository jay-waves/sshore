package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

type commandConsole struct {
	output  []byte
	editor  *termui.Console
	color   bool
	confirm bool
	attach  string
	targets []sessionTarget
}

const consoleHelp = `list (ls)         Show task inventory on this host
ps (pstree)       Show the task tree on this host
attach (a) <ID|name>  Switch session (Tab completes)
spawn (sh)        Open a nested shell
launch (mux)      Open an independent shell on this host
detach (d)        Detach from the current shell
exit              Return to the current shell`

func (c *commandConsole) start(ctx context.Context, out io.Writer, send func(clientEvent), width int, color bool, confirmation bool) {
	c.color = color
	if !confirmation {
		title := termui.Render(color, termui.Heading, "sshore commands")
		keys := termui.Render(color, termui.Muted, "(Tab: next, Esc: return)")
		fmt.Fprintf(out, "%s  %s\r\n\r\n", title, keys)
		termui.WriteConsoleBody(out, termui.Render(color, termui.Muted, "Loading sessions…"))
	}
	prompt := termui.FormatPrompt(color, "sshore> ")
	c.editor = termui.StartConsole(ctx, prompt, width, color, func(e termui.ConsoleEvent) {
		send(clientEvent{consoleSource: c, consoleOutput: e.Output, consoleLine: e.Line, consoleClosed: e.Closed})
	})
}

type consoleCompleter func() []sessionTarget

func (targets consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	word := prefix
	candidates := []string{"attach", "detach", "exit", "help", "launch", "list", "pstree", "spawn"}
	for alias, command := range commandAliases {
		if slices.Contains(candidates, command) {
			candidates = append(candidates, alias)
		}
	}
	command, rest, ok := strings.Cut(prefix, " ")
	if ok && canonicalCommand(command) == "attach" {
		word, candidates = rest, nil
		for _, target := range targets() {
			candidates = append(candidates, target.Name, taskShortCode("task:"+target.Instance))
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
	targets := c.targets
	c.editor.Show(body, consoleCompleter(func() []sessionTarget { return targets }))
}

// showConfirmation presents a temporary choice through the console editor.
func (c *commandConsole) showConfirmation(problem string) {
	c.confirm = true
	body := termui.StatusLine(c.color, termui.Warning, "Detaching may close this terminal window.") +
		"\nThis spawn may end when its original parent exits."
	if problem != "" {
		body += "\n\n" + termui.Render(c.color, termui.Warning, problem)
	}
	prompt := termui.FormatPrompt(c.color, "Detach? [y/N] ")
	c.editor.Replace(body, prompt)
}

func consoleTargetSummary(targets []sessionTarget, rows int) string {
	targets = slices.Clone(targets)
	slices.SortFunc(targets, func(a, b sessionTarget) int { return cmp.Compare(a.Instance, b.Instance) })
	var lines []string
	for _, t := range targets[:min(max(1, rows-8), len(targets))] {
		lines = append(lines, taskShortCode("task:"+t.Instance)+"  "+t.Name)
	}
	if len(lines) == 0 {
		return "No detached sessions here."
	}
	if len(lines) < len(targets) {
		lines = append(lines, "… more sessions available with Tab completion")
	}
	return strings.Join(lines, "\n")
}

func resolveConsoleTarget(targets []sessionTarget, selector string) (sessionTarget, string) {
	var selected sessionTarget
	for _, t := range targets {
		if t.Name != selector && !(len(selector) >= 2 && strings.HasPrefix(t.Instance, strings.ToLower(selector))) {
			continue
		}
		if selected.Instance != "" {
			return sessionTarget{}, "Ambiguous session. Use its full name or a longer ID."
		}
		selected = t
	}
	if selected.Instance == "" {
		return selected, "Session unavailable."
	}
	return selected, ""
}

const consoleMoreRows = "… more rows in the standalone command on this host"

func consoleInventory(request message, current string) message {
	reply := message{Type: "console_inventory", Action: request.Action}
	var body bytes.Buffer
	var err error
	switch request.Name {
	case "list":
		var inv inventory
		inv, err = daemonListTasks(taskListRequest{Native: true})
		if err == nil {
			err = inv.incompleteError()
		}
		writeTaskInventoryWidth(&body, inv, current, request.Color, int(request.Cols))
	case "ps", "pstree":
		var rows []taskTreeRow
		rows, err = readTaskTree()
		writeShellTree(&body, rows, current, request.Color)
	default:
		reply.Output = "Unknown inventory command."
		return reply
	}
	if err != nil {
		fmt.Fprintln(&body, cleanText(err.Error()))
	}
	reply.Output = strings.TrimRight(body.String(), "\n")
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
