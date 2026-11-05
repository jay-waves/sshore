package main

import (
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
	editor *readline.Instance
	paths  bool
}

func newApprovalInput() (*approvalInput, error) {
	p := &approvalInput{}
	editor, err := readline.NewEx(&readline.Config{
		AutoComplete: p, HistoryLimit: -1, DisableAutoSaveHistory: true,
		InterruptPrompt: "^C", EOFPrompt: "",
	})
	if err != nil {
		return nil, err
	}
	p.editor = editor
	return p, nil
}

func (p *approvalInput) Read(prompt string, paths bool) (string, error) {
	p.paths = paths
	p.editor.SetPrompt(formatLog(terminalLogColor(os.Stdout), logInfo, "%s", prompt))
	line, err := p.editor.Readline()
	if err != nil {
		return "", fmt.Errorf("approval cancelled; request remains pending: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func (p *approvalInput) Close() error {
	if p.editor == nil {
		return nil
	}
	err := p.editor.Close()
	p.editor = nil
	return err
}

func (p *approvalInput) Do(line []rune, pos int) ([][]rune, int) {
	if !p.paths {
		return nil, 0
	}
	return completeLocalPath(line, pos)
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
