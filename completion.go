package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type completionCandidate struct{ value, description string }
type completionResult struct {
	mode, prefix string
	candidates   []completionCandidate
}

func (r *completionResult) add(value, description string) {
	if value == "" || len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return
	}
	r.candidates = append(r.candidates, completionCandidate{value, cleanText(description)})
}
func (r completionResult) write(w io.Writer, current string) error {
	if r.mode == "" {
		r.mode = "none"
	}
	if _, err := fmt.Fprintf(w, "%s\t:%s\t:%s\n", r.mode, r.prefix, current); err != nil {
		return err
	}
	sort.Slice(r.candidates, func(i, j int) bool { return r.candidates[i].value < r.candidates[j].value })
	seen := map[string]bool{}
	for _, c := range r.candidates {
		if seen[c.value] || !strings.HasPrefix(c.value, current) {
			continue
		}
		seen[c.value] = true
		if _, err := fmt.Fprintf(w, "%s\t%s\n", c.value, c.description); err != nil {
			return err
		}
	}
	return nil
}

func completeCLI(words []string, current string, catalog func() completionCatalog) completionResult {
	r := completionResult{}
	if len(words) == 0 {
		for name := range commandHelp {
			r.add(name, "command")
		}
		r.add("help", "command")
		root, _ := commandSpec("")
		for _, name := range root.flags {
			r.add(name, "option")
			if alias := cliOptions[name].alias; alias != "" {
				r.add(alias, name)
			}
		}
		return r
	}
	command := words[0]
	spec, ok := commandSpec(command)
	if !ok {
		return r
	}
	parsed, err := scanCLI(words[1:], command, true)
	if err != nil {
		return r
	}
	if parsed.sshMode {
		return completeSSH(parsed.ssh, current, spec.native)
	}
	valueOption, valuePrefix := parsed.pending, ""
	if name, _, attached := strings.Cut(current, "="); attached {
		if canonical, option, found := spec.option(name); found && option.kind != cliBool {
			valueOption, valuePrefix = canonical, name+"="
		}
	}
	if valueOption != "" {
		switch cliOptions[valueOption].kind {
		case cliPath:
			r.mode = "files"
		case cliDirectory:
			r.mode = "dirs"
		}
		r.prefix = valuePrefix
		return r
	}
	if strings.HasPrefix(current, "-") {
		for _, name := range spec.flags {
			option := cliOptions[name]
			blocked := parsed.has(name) || (name == "--direct-io" && validateCLIValue(name, "", option) != nil)
			for used := range parsed.values {
				blocked = blocked || slices.Contains(option.conflicts, used) || slices.Contains(cliOptions[used].conflicts, name)
			}
			if blocked {
				continue
			}
			r.add(name, "option")
			if option.alias != "" {
				r.add(option.alias, name)
			}
		}
		if spec.ssh && !parsed.has("--local") {
			r.add("--", "SSH options")
		}
		return r
	}
	n := len(parsed.operands)
	if max := spec.maxOperands(command, parsed); max >= 0 && n >= max {
		return r
	}
	if command == "help" && n == 0 {
		for name := range commandHelp {
			r.add(name, "command")
		}
		return r
	}
	if command == "completion" && n == 0 {
		for name := range completionScripts {
			r.add(name, "shell")
		}
		return r
	}
	if command == "cp" {
		r.mode = "files"
	}
	if command == "mount" && n == 1 {
		r.add("origin:", "request a mount on the attached client")
	}
	if command == "mount" && n == 2 {
		r.mode = "dirs"
		return r
	}
	if command == "forward" && strings.Contains(current, ":") {
		return r
	}
	if command == "cp" {
		if endpoint, err := parseCopyEndpoint(current); err == nil && (endpoint.host != "" || originEndpoint(current)) {
			// Remote directory enumeration must never be triggered by Tab.
			r.mode = "none"
			return r
		}
	}
	local := parsed.has("--local")
	hostPosition := n == 0 && !local && slices.Contains([]string{"launch", "spawn", "attach", "list", "pstree", "cancel", "rename", "mount"}, command)
	endpoint := command == "cp" || command == "forward" && n < 2
	tasks := slices.Contains([]string{"attach", "cancel", "rename", "review"}, command)
	if !hostPosition && !endpoint && !tasks {
		return r
	}
	c := catalog()
	if hostPosition || endpoint {
		for _, host := range c.hosts {
			if endpoint {
				if strings.Contains(host, ":") {
					continue
				}
				host += ":"
			}
			r.add(host, "host")
		}
	}
	if endpoint {
		r.add("origin:", "client endpoint")
	}
	if !tasks || parsed.has("--all") {
		return r
	}
	for _, t := range c.tasks {
		if command == "attach" || command == "rename" {
			if t.kind != "shell" {
				continue
			}
		}
		if command == "review" && t.kind != "review" {
			continue
		}
		description := t.kind + " (cached; revalidated on use)"
		if n == 0 && !local {
			if id := c.ids[t.identity]; id != 0 {
				r.add(strconv.FormatUint(id, 10), description)
			}
			if command == "review" && t.kind == "review" {
				r.add(t.name, description)
			}
			if command == "cancel" && t.kind != "shell" {
				r.add(t.kind+"/"+t.name, description)
			}
		}
		if t.kind == "shell" && ((local && n == 0 && t.host == "") || (!local && n == 1 && t.host != "" && t.host == parsed.operands[0])) {
			r.add(t.name, description)
		}
	}
	return r
}

func runCompletion(args []string, out io.Writer) error {
	parsed, err := parseCLI(args, "completion")
	if err != nil {
		return err
	}
	script, ok := completionScripts[parsed.operands[0]]
	if !ok {
		return usage("unsupported completion shell: %s", parsed.operands[0])
	}
	_, err = io.WriteString(out, script)
	return err
}

// The adapter passes the current word with a ':' sentinel so Windows native
// argument marshalling cannot drop an empty final word. Candidates are data only.
func runCompletionQuery(args []string, out io.Writer) error {
	if os.Getenv("SSHORE_DISABLED") == "1" {
		return nil
	}
	if len(args) < 2 {
		return nil
	}
	var words []string
	var current string
	switch args[0] {
	case "words":
		if !strings.HasPrefix(args[1], ":") {
			return nil
		}
		current, words = args[1][1:], args[2:]
	case "bash":
		if len(args) != 2 {
			return nil
		}
		words, current = completionLine(args[1])
	default:
		return nil
	}
	if len(words) > 128 || len(current) > 4096 || strings.IndexFunc(current, unicode.IsControl) >= 0 {
		return nil
	}
	return completeCLI(words, current, loadCompletionCatalog).write(out, current)
}

// Parse only literal shell words up to the cursor. Never expand variables,
// substitutions or globs. Bash splits : and = in COMP_WORDS, so use its input
// text instead. Unsupported shell expressions produce no candidates.
func completionLine(line string) ([]string, string) {
	if len(line) > 32*1024 {
		return []string{""}, ""
	}
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped, started := false, false
	for _, ch := range line {
		if escaped {
			word.WriteRune(ch)
			escaped = false
			started = true
			continue
		}
		if ch == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			started = true
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			started = true
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case ';', '|', '&':
			words = nil
			word.Reset()
			started = false
		case '$', '`', '(', ')', '<', '>', '\n':
			return []string{""}, ""
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if len(words) == 0 {
		return []string{""}, ""
	}
	return words[1:], word.String()
}

func completeSSH(words []string, current string, native bool) completionResult {
	r := completionResult{}
	flags := sshOptionKinds(native)
	_, pending, err := scanSSHOptions(words, native, true)
	if err != nil || pending != cliBool {
		if err == nil && pending == cliPath {
			r.mode = "files"
		}
		return r
	}
	if name, _, kind, ok := sshOption(current, flags); ok && kind == cliPath && len(current) > 2 {
		r.mode = "files"
		r.prefix = name
		return r
	}
	for name := range flags {
		if strings.HasPrefix(name, current) {
			r.add(name, "SSH option")
		}
	}
	return r
}
