package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/termui"
)

type commandConsole struct {
	output        []byte
	editor        *termui.Console
	color         bool
	attach        string
	targets       []sessionTarget
	targetWarning string
}

const consoleHelp = `list (ls)         Show task inventory on this host
ps (pstree)       Show the task tree on this host
attach (a) <ID|name>  Switch session (Tab completes)
sh [--pin]        Open a nested shell (alias: shell)
detach [DURATION] Pin and detach the current shell
exit              Return to the current shell`

func (c *commandConsole) start(ctx context.Context, out io.Writer, send func(clientEvent), host string, width int, color bool) {
	c.color = color
	title := termui.Render(color, termui.Heading, "sshore commands")
	keys := termui.Render(color, termui.Muted, "(Tab: next, Esc: return)")
	fmt.Fprintf(out, "%s  %s\r\n\r\n", title, keys)
	termui.WriteConsoleBody(out, termui.Render(color, termui.Muted, "Loading pstree…"))
	const prefix, suffix = "sshore on ", "> "
	host = ansi.Truncate(cleanText(host), max(1, width-len(prefix)-len(suffix)-4), "…")
	prompt := termui.FormatPrompt(color, prefix+host+suffix)
	c.editor = termui.StartConsole(ctx, prompt, width, color, func(e termui.ConsoleEvent) {
		send(clientEvent{consoleSource: c, console: e})
	})
}

type consoleCompleter func() []sessionTarget

func (targets consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	word := prefix
	candidates := []string{"attach", "detach", "exit", "help", "sh", "list", "pstree"}
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
			writeTaskInventoryWidth(&body, inv, current, request.Color, int(request.Cols))
		}
	case "ps", "pstree":
		var rows []taskTreeRow
		rows, err = readTaskTree()
		if err == nil || len(rows) > 0 {
			writeShellTree(&body, rows, current, request.Color)
		}
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
