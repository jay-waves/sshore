package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/jay-waves/sshore/internal/termui"
)

type commandConsole struct {
	output  []byte
	editor  *termui.Console
	attach  string
	targets []sessionTarget
}

const consoleTitle = "sshore commands  (Tab: next, Esc: return)"

func (c *commandConsole) draw(out io.Writer, body string) {
	termui.DrawConsole(out, consoleTitle, body)
}

const consoleHelp = `list              Show task inventory
ps                Show the local task tree
attach <ID|name>  Switch session (Tab completes)
spawn             Open a nested shell
launch            Open an independent shell on this host
cp <source> <dest> Copy files after detaching
forward <from> <to> Start a background forward after detaching
exit              Return to the current shell`

func (c *commandConsole) start(ctx context.Context, send func(clientEvent), width int) {
	c.editor = termui.StartConsole(ctx, consoleTitle, "sshore> ", width, func(e termui.ConsoleEvent) {
		send(clientEvent{consoleSource: c, consoleOutput: e.Output, consoleLine: e.Line, consoleClosed: e.Closed})
	})
}

type consoleCompleter func() []sessionTarget

func (targets consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	if pos == len(line) {
		args, endpoint, quote, _ := scanConsoleResource(prefix)
		if len(args) > 0 && args[0] == "cp" && !slices.Contains(args, "--") {
			if path, ok := strings.CutPrefix(endpoint, "self:"); ok {
				return completeConsoleCopyPath(path, quote)
			}
		}
	}
	word := prefix
	candidates := []string{"attach", "cp", "exit", "forward", "help", "launch", "list", "ps", "pstree", "spawn"}
	if rest, ok := strings.CutPrefix(prefix, "attach "); ok {
		word, candidates = rest, nil
		for _, target := range targets() {
			candidates = append(candidates, target.Name, taskShortCode("task:"+target.Instance))
		}
	}
	if command, rest, ok := strings.Cut(prefix, " "); ok && (command == "cp" || command == "forward") {
		word = rest
		if i := strings.LastIndexByte(rest, ' '); i >= 0 {
			word = rest[i+1:]
		}
		candidates = nil
		if strings.HasPrefix(word, "-") && !slices.Contains(strings.Fields(rest), "--") {
			spec, _ := commandSpec(command)
			candidates = slices.DeleteFunc(spec.flags, func(flag string) bool { return flag == "--foreground" })
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

func consoleInventory(command string, rows int) string {
	var body bytes.Buffer
	var err error
	if command == "list" {
		var inv inventory
		inv, err = collectInventory(options{})
		writeTaskInventory(&body, inv)
	} else {
		var inv inventory
		inv, err = localTreeInventory()
		writeShellTree(&body, inventoryResourceTrees(localShellTreeNodes(inv), inv), inv.MachineID, false)
	}
	if err != nil {
		fmt.Fprintln(&body, err)
	}
	lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
	limit := max(1, rows-8)
	if len(lines) > limit {
		lines = append(lines[:limit:limit], "… more rows in the standalone command")
	}
	return strings.Join(lines, "\n")
}

// consoleResourceArgs groups quoted paths without interpreting shell syntax.
func consoleResourceArgs(line string) ([]string, error) {
	args, word, quote, haveWord := scanConsoleResource(line)
	if quote != 0 {
		return nil, usage("unclosed quote")
	}
	if haveWord {
		args = append(args, word)
	}
	return args, nil
}

func scanConsoleResource(line string) ([]string, string, rune, bool) {
	var args []string
	var word strings.Builder
	var quote rune
	haveWord := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, haveWord = r, true
		case unicode.IsSpace(r):
			if haveWord {
				args = append(args, word.String())
				word.Reset()
				haveWord = false
			}
		default:
			word.WriteRune(r)
			haveWord = true
		}
	}
	return args, word.String(), quote, haveWord
}

func completeConsoleCopyPath(path string, quote rune) ([][]rune, int) {
	input := []rune(path)
	suffixes, offset := termui.CompleteLocalPath(input, len(input))
	for i, raw := range suffixes {
		suffix := string(raw)
		directory := len(suffix) != 0 && os.IsPathSeparator(suffix[len(suffix)-1])
		switch {
		case quote != 0:
			mark := string(quote)
			other := "'"
			if quote == rune(other[0]) {
				other = `"`
			}
			suffix = strings.ReplaceAll(suffix, mark, mark+other+mark+other+mark) + mark
		case strings.IndexFunc(suffix, func(r rune) bool {
			return unicode.IsSpace(r) || r == 39 || r == 34
		}) >= 0:
			suffix = "'" + strings.ReplaceAll(suffix, "'", `'"'"'`) + "'"
		}
		if !directory {
			suffix += " "
		}
		suffixes[i] = []rune(suffix)
	}
	return suffixes, offset
}

// Execute only after the active attachment has confirmed detach and the
// frontend has restored the terminal. The CLI then owns any SSH prompts.
type consoleResourceCommand struct {
	run func() (int, error)
}

func (c *consoleResourceCommand) Error() string { return "run console resource command" }

func prepareConsoleResource(args []string) (*consoleResourceCommand, error) {
	if args[0] == "cp" {
		o, err := parseCopyCommand(args[1:])
		if err != nil {
			return nil, err
		}
		for _, endpoint := range append(slices.Clone(o.sources), o.target) {
			if endpoint.pick || endpoint.stream {
				return nil, usage("console cp requires explicit paths and file endpoints")
			}
		}
		return &consoleResourceCommand{run: func() (int, error) { return executeCopyCommand(o) }}, nil
	}
	f, foreground, err := parseForwardCommand(args[1:])
	if err != nil {
		return nil, err
	}
	if foreground || f.Port == 0 || f.Target == 0 {
		return nil, usage("console forward requires both ports and runs in the background")
	}
	return &consoleResourceCommand{run: func() (int, error) { return executeEndpointForward(f, false) }}, nil
}
