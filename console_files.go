package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path"
	"strings"

	"github.com/jay-waves/sshore/internal/files/dirlist"
	"github.com/jay-waves/sshore/internal/termui"
)

type consoleFilePicker struct {
	input    chan termui.KeyInput
	cancel   context.CancelFunc
	requests map[string]*consoleFileRequest
}

type consoleFileRequest struct {
	ctx    context.Context
	dir    string
	parent string
	reply  chan message
}

type consoleFileResult struct {
	path string
	err  error
}

type consoleFileOutput func([]byte) (int, error)

func (w consoleFileOutput) Write(b []byte) (int, error) { return w(b) }

func startConsoleFilePicker(ctx context.Context, directory, host string, attached bool, emit func(clientEvent)) *consoleFilePicker {
	ctx, cancel := context.WithCancel(ctx)
	picker := &consoleFilePicker{
		input: make(chan termui.KeyInput, 32), cancel: cancel,
		requests: make(map[string]*consoleFileRequest),
	}
	var options termui.LFOptions
	if !attached {
		options = termui.LocalFileSelection(directory, "Open shell", true)
	} else {
		windows := len(directory) >= 3 && directory[1] == ':' && strings.ContainsRune("/\\", rune(directory[2]))
		if windows {
			directory = strings.ReplaceAll(directory, "\\", "/")
		}
		options = pairedFileSelection(directory, "Open shell", host, true, func(ctx context.Context, dir, parent string) ([]dirlist.Entry, error, []dirlist.Entry, error) {
			request := consoleFileRequest{ctx: ctx, dir: dir, parent: parent, reply: make(chan message, 1)}
			emit(clientEvent{filePicker: picker, fileRequest: &request})
			select {
			case reply := <-request.reply:
				if files := reply.ConsoleFiles; files != nil {
					return files.Files, listingError(reply.Error), files.ParentFiles, listingError(files.ParentError)
				}
				return nil, listingError(reply.Error), nil, nil
			case <-ctx.Done():
				return nil, ctx.Err(), nil, ctx.Err()
			}
		})
		if windows {
			options.Parent = func(dir string) string {
				if len(dir) == 3 {
					return dir
				}
				parent := path.Dir(dir)
				if len(parent) == 2 && parent[1] == ':' {
					parent += "/"
				}
				return parent
			}
		}
	}
	options.Input = picker.input
	options.Output = consoleFileOutput(func(b []byte) (int, error) {
		emit(clientEvent{filePicker: picker, fileOutput: bytes.Clone(b)})
		return len(b), nil
	})
	go func() {
		dir, err := termui.PickLF(ctx, options)
		emit(clientEvent{filePicker: picker, fileResult: &consoleFileResult{path: dir, err: err}})
	}()
	return picker
}

func (p *consoleFilePicker) send(data []byte) {
	select {
	case p.input <- termui.KeyInput{Data: data}:
	default:
		p.cancel()
	}
}

func consoleFileListing(request message) message {
	files := &consoleFilePayload{}
	reply := message{Type: "console_files", Action: request.Action, ConsoleFiles: files}
	files.Files, reply.Error = listingEntries(os.ReadDir(request.Directory))
	var parent string
	if request.ConsoleFiles != nil {
		parent = request.ConsoleFiles.ParentDirectory
	}
	if parent != "" {
		files.ParentFiles, files.ParentError = listingEntries(os.ReadDir(parent))
	}
	body, _ := json.Marshal(reply)
	if len(body) > maxControlMessage && parent != "" {
		files.ParentFiles, files.ParentError = nil, "parent directory listing is too large"
		body, _ = json.Marshal(reply)
	}
	if len(body) > maxControlMessage {
		files.Files, reply.Error = nil, "directory listing is too large"
	}
	return reply
}
