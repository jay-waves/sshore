package main

// One readline editor owns stdin during setup or local approval and closes before
// SSH takes over. Input history is disabled. Paths are treated as a single string,
// including spaces, Unicode, optional surrounding quotes and home expansion;
// there is no shell command substitution or wildcard expansion. Completion occurs
// at the end or before a closing quote to preserve the suffix. Bounded menu display
// and selection are in approval_completion.go; color never enters candidate paths.
// Windows hooks use checked console APIs and native VT when available, restoring
// console state on close. Legacy ANSI translation receives only uncolored text.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/chzyer/readline"
)

// One editor owns stdin throughout approval; it is closed before reconnecting.
type approvalInput struct {
	editor        *readline.Instance
	paths         bool
	color         bool
	restoreOutput func() error
	completion    pathCompletion
}

func newApprovalInput() (*approvalInput, error) {
	p := &approvalInput{}
	config := &readline.Config{
		AutoComplete: p, HistoryLimit: -1, DisableAutoSaveHistory: true,
		InterruptPrompt: "^C", EOFPrompt: "",
	}
	config.FuncFilterInputRune = p.filterCompletionKey
	config.Listener = readline.FuncListener(p.completionChanged)
	restore, ansi := configureApprovalTerminal(config)
	p.restoreOutput = restore
	p.color = ansi && terminalLogColor(os.Stdout)
	editor, err := readline.NewEx(config)
	if err != nil {
		_ = restore()
		return nil, err
	}
	p.editor = editor
	return p, nil
}

func (p *approvalInput) Read(prompt string, paths bool) (string, error) {
	p.paths = paths
	p.completion = pathCompletion{}
	p.editor.SetPrompt(formatApprovalPrompt(p.color, prompt))
	// readline ignores errors from EnterRawMode. Check our Windows hook before
	// starting its reader; repeated entry is idempotent.
	if runtime.GOOS == "windows" {
		if err := p.editor.Config.FuncMakeRaw(); err != nil {
			return "", fmt.Errorf("prepare confirmation input: %w", err)
		}
	}
	line, err := p.editor.Readline()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func formatApprovalPrompt(color bool, prompt string) string {
	return formatLog(color, logInfo, "%s", prompt)
}

func (p *approvalInput) Close() error {
	if p.editor == nil {
		return nil
	}
	err := p.editor.Close()
	p.editor = nil
	if p.restoreOutput != nil {
		err = errors.Join(err, p.restoreOutput())
		p.restoreOutput = nil
	}
	return err
}

func (p *approvalInput) Do(line []rune, pos int) ([][]rune, int) {
	if !p.paths {
		return nil, 0
	}
	return p.completePath(line, pos)
}

// Treat the input as one path, including spaces, rather than shell words.
// Completion is offered at the end (or immediately before a closing quote),
// so inserting a candidate cannot corrupt a filename suffix after the cursor.
func completeLocalPath(line []rune, pos int) ([][]rune, int) {
	if pos < 0 || pos > len(line) {
		return nil, 0
	}
	prefix := string(line[:pos])
	quote := rune(0)
	if len(prefix) > 0 && (prefix[0] == '\'' || prefix[0] == '"') {
		quote = rune(prefix[0])
		prefix = prefix[1:]
	}
	if pos != len(line) && !(quote != 0 && pos == len(line)-1 && line[pos] == quote) {
		return nil, 0
	}
	dir, base := filepath.Split(prefix)
	searchDir := dir
	if searchDir == "" {
		searchDir = "."
	} else {
		var err error
		searchDir, err = localPath([]byte(searchDir))
		if err != nil {
			return nil, 0
		}
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil, 0
	}
	sep := string(os.PathSeparator)
	if runtime.GOOS == "windows" && strings.Contains(dir, "/") && !strings.Contains(dir, `\`) {
		sep = "/"
	}
	var candidates [][]rune
	for _, entry := range entries {
		name := entry.Name()
		if strings.IndexFunc(name, unicode.IsControl) >= 0 || !strings.HasPrefix(name, base) {
			continue
		}
		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(searchDir, name)); err == nil {
				isDir = info.IsDir()
			}
		}
		suffix := strings.TrimPrefix(name, base)
		if isDir {
			suffix += sep
		}
		candidates = append(candidates, []rune(suffix))
	}
	return candidates, len([]rune(base))
}
