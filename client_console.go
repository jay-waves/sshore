package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/chzyer/readline"
)

type commandConsole struct {
	alternate bool
	output    []byte
	cancel    context.CancelFunc
	input     chan []byte
	prompts   chan consolePrompt
	attach    string
	targets   []sessionTarget
}

func (c *commandConsole) closeSequence() string {
	if c.alternate {
		return "\x1b[<1u\x1b[2J\x1b[H"
	}
	return "\x1b[<1u\x1b[?1049l"
}

func (c *commandConsole) draw(out io.Writer, body string) {
	fmt.Fprintf(out, "\x1b[2J\x1b[Hsshore commands  (Esc to return)\r\n\r\n%s\r\n\r\n", cleanTextMultiline(body))
}

func cleanTextMultiline(body string) string {
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = cleanText(lines[i])
	}
	return strings.Join(lines, "\r\n")
}

type consolePrompt struct {
	body    string
	targets []sessionTarget
}

type consoleEventWriter func([]byte)

func (w consoleEventWriter) Write(p []byte) (int, error) {
	w(bytes.Clone(p))
	return len(p), nil
}

const consoleHelp = `sshore list              Show task inventory
sshore ps                Show the local task tree
sshore attach <ID|name>  Switch session (Tab completes)
sshore spawn             Open a nested shell
help                     Show commands
exit                     Return to the current shell`

func (c *commandConsole) start(ctx context.Context, send func(clientEvent), width int) {
	ctx, c.cancel = context.WithCancel(ctx)
	c.input = make(chan []byte, 32)
	c.prompts = make(chan consolePrompt, 1)
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-c.input:
				if _, err := writer.Write(data); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer reader.Close()
		var targets []sessionTarget
		out := consoleEventWriter(func(p []byte) { send(clientEvent{consoleSource: c, consoleOutput: p}) })
		rl, err := readline.NewEx(&readline.Config{
			Prompt: "sshore> ", Stdin: reader, Stdout: out, Stderr: out,
			ForceUseInteractive: true, HistoryLimit: 100,
			FuncGetWidth: func() int { return max(20, width) },
			FuncMakeRaw:  func() error { return nil }, FuncExitRaw: func() error { return nil },
			FuncOnWidthChanged:  func(func()) {},
			FuncFilterInputRune: func(r rune) (rune, bool) { return r, r != readline.CharCtrlZ },
			AutoComplete:        consoleCompleter(func() []sessionTarget { return targets }),
		})
		if err != nil {
			send(clientEvent{consoleSource: c, consoleClosed: true})
			return
		}
		defer rl.Close()
		stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case prompt := <-c.prompts:
				targets = prompt.targets
				c.draw(out, prompt.body)
			}
			line, err := rl.Readline()
			if err != nil {
				send(clientEvent{consoleSource: c, consoleClosed: true})
				return
			}
			send(clientEvent{consoleSource: c, consoleLine: &line})
		}
	}()
}

type consoleCompleter func() []sessionTarget

func (targets consoleCompleter) Do(line []rune, pos int) ([][]rune, int) {
	prefix := string(line[:pos])
	word := prefix
	candidates := []string{"exit", "help", "sshore"}
	if rest, ok := strings.CutPrefix(prefix, "sshore "); ok {
		word = strings.TrimLeft(rest, " ")
		candidates = []string{"attach", "list", "ps", "pstree", "spawn"}
		if selector, ok := strings.CutPrefix(word, "attach "); ok {
			word = strings.TrimLeft(selector, " ")
			candidates = nil
			for _, target := range targets() {
				candidates = append(candidates, target.Name, taskShortCode("task:"+target.Instance))
			}
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
	c.prompts <- consolePrompt{body: body, targets: c.targets}
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
