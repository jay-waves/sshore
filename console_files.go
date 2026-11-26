package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

type consoleFilePicker struct {
	input    chan termui.LFInput
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

func (c *commandConsole) startFilePicker(ctx context.Context, directory, host string, emit func(clientEvent)) {
	ctx, cancel := context.WithCancel(ctx)
	picker := &consoleFilePicker{
		input: make(chan termui.LFInput, 32), cancel: cancel,
		requests: make(map[string]*consoleFileRequest),
	}
	c.picker = picker
	var options termui.LFOptions
	if !c.attached {
		options = localFileSelection(directory, "Open shell", true)
	} else {
		windows := len(directory) >= 3 && directory[1] == ':' && strings.ContainsRune("/\\", rune(directory[2]))
		if windows {
			directory = strings.ReplaceAll(directory, "\\", "/")
		}
		options = pairedFileSelection(directory, "Open shell", host, true, func(ctx context.Context, dir, parent string) ([]directoryEntry, error, []directoryEntry, error) {
			request := consoleFileRequest{ctx: ctx, dir: dir, parent: parent, reply: make(chan message, 1)}
			emit(clientEvent{consoleSource: c, filePicker: picker, fileRequest: &request})
			select {
			case reply := <-request.reply:
				return reply.Files, listingError(reply.Error), reply.ParentFiles, listingError(reply.ParentError)
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
		emit(clientEvent{consoleSource: c, filePicker: picker, fileOutput: bytes.Clone(b)})
		return len(b), nil
	})
	go func() {
		dir, err := termui.PickLF(ctx, options)
		emit(clientEvent{consoleSource: c, filePicker: picker, fileResult: &consoleFileResult{path: dir, err: err}})
	}()
}

func (p *consoleFilePicker) send(data []byte) {
	select {
	case p.input <- termui.LFInput{Data: data}:
	default:
		p.cancel()
	}
}

func (c *commandConsole) finishFilePicker(out io.Writer, result *consoleFileResult) error {
	c.picker.cancel()
	c.picker = nil
	if err := writeFull(out, []byte("\x1b[2J\x1b[H")); err != nil {
		return err
	}
	if result.err != nil && !errors.Is(result.err, context.Canceled) {
		c.show(result.err.Error())
	} else {
		c.show("")
	}
	return nil
}

func consoleFileListing(request message) message {
	reply := message{Type: "console_files", Action: request.Action}
	reply.Files, reply.Error = listingEntries(os.ReadDir(request.Directory))
	if request.ParentDirectory != "" {
		reply.ParentFiles, reply.ParentError = listingEntries(os.ReadDir(request.ParentDirectory))
	}
	body, _ := json.Marshal(reply)
	if len(body) > maxControlMessage && request.ParentDirectory != "" {
		reply.ParentFiles, reply.ParentError = nil, "parent directory listing is too large"
		body, _ = json.Marshal(reply)
	}
	if len(body) > maxControlMessage {
		reply.Files, reply.Error = nil, "directory listing is too large"
	}
	return reply
}
