package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	sftp "github.com/pkg/sftp/v2"
)

func newApprovedFile(op operation, localPath string, opened *files.Source) fileCopy {
	if op.Kind == "upload" {
		return fileCopy{source: localPath, target: "/target", opened: opened}
	}
	return fileCopy{source: "/source", target: localPath, downloadSize: op.Size}
}

func (f fileCopy) approvedName(op operation) string {
	if f.opened != nil {
		return f.opened.Name()
	}
	if op.Kind == "upload" && f.source != "" {
		return filepath.Base(f.source)
	}
	return op.Name
}

func (f fileCopy) approvedLocalPath(op operation) string {
	if op.Kind == "upload" {
		return f.source
	}
	return f.target
}

func pickApprovedUpload(console *termui.LocalConsole, request requestRecord) ([]fileCopy, error) {
	op := request.Operations[0]
	picker := localFileSelection(request.Directory, "Choose source to upload", op.Recursive)
	name, err := console.PickLF(context.Background(), picker)
	if err != nil {
		return nil, err
	}
	var source *files.Source
	if op.Recursive {
		err = checkCopyDirectory(name, false)
	} else {
		source, err = files.OpenSource(name)
	}
	if err != nil {
		return nil, err
	}
	return []fileCopy{newApprovedFile(op, name, source)}, nil
}

// The caller closes all opened sources, including on a partial selection.
func selectApprovedFiles(input reviewPromptReader, r requestRecord) ([]fileCopy, error) {
	approvedFiles := make([]fileCopy, 0, len(r.Operations))
	destinations := make(map[string]bool)
	for _, op := range r.Operations {
		hint, err := approvalPathHint(op.LocalHint)
		if err != nil {
			return approvedFiles, err
		}
		if op.Recursive {
			prompt, initial := "Save directory as: ", filepath.Join(r.Directory, op.Name)
			if op.Kind == "upload" {
				prompt, initial = "Local directory to upload: ", r.Directory
			}
			if hint != "" {
				initial = hint
			}
			name, err := readApprovalPath(input, prompt, initial, func(name string) (string, error) {
				if err := checkCopyDirectory(name, op.Kind == "download"); err != nil {
					return "", err
				}
				if op.Kind == "download" && destinations[name] {
					return "", fmt.Errorf("destination already selected: %s", name)
				}
				return name, nil
			})
			if err != nil {
				return approvedFiles, err
			}
			approvedFiles = append(approvedFiles, newApprovedFile(op, name, nil))
			if op.Kind == "download" {
				destinations[name] = true
			}
			continue
		}
		switch op.Kind {
		case "download":
			dest := filepath.Join(r.Directory, op.Name)
			if hint != "" {
				dest = hint
			}
			termui.Log(os.Stdout, "", termui.Info, "Download %s · %s\n", cleanText(op.Path), displayBytes(op.Size))
			path, e := readApprovalPath(input, "Save as: ", dest, func(path string) (string, error) {
				path, err := files.DownloadDestination(path, op.Name)
				if err == nil && destinations[path] {
					return "", fmt.Errorf("destination already selected: %s", path)
				}
				return path, err
			})
			if e != nil {
				return approvedFiles, e
			}
			approvedFiles = append(approvedFiles, newApprovedFile(op, path, nil))
			destinations[path] = true
		case "upload":
			termui.Log(os.Stdout, "", termui.Info, "Upload → %s\n", cleanText(op.Path))
			var source *files.Source
			path, e := readApprovalPath(input, "Local file to upload: ", hint, func(path string) (string, error) {
				f, e := files.OpenSource(path)
				if e != nil {
					return "", e
				}
				source = f
				return path, nil
			})
			if e != nil {
				return approvedFiles, e
			}
			approvedFiles = append(approvedFiles, newApprovedFile(op, path, source))
		}
	}
	return approvedFiles, nil
}

func runApprovedFiles(r requestRecord, source options, approvedFiles []fileCopy) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	target, err := approvedFileOptions(r.Connection, &source)
	if err != nil {
		return err
	}
	open := func(op operation, name string, size int64) (net.Conn, error) {
		request := approvedResourceRequest(r.Connection.Session, r.PID, r.ID, op)
		request.FileName, request.Size = name, size
		return openResourceEndpoint(ctx, &target, request, nil)
	}
	return executeApprovedFiles(ctx, open, r.Operations, approvedFiles)
}

// Selection preserves the approved operation order. Each file opens its own scoped endpoint.
func executeApprovedFiles(ctx context.Context, open func(operation, string, int64) (net.Conn, error), ops []operation, batch []fileCopy) error {
	if len(ops) != len(batch) {
		return fmt.Errorf("approved copy selection does not match request")
	}
	completed, err := transferCopyBatch(batch, func(i int, file fileCopy) error {
		op := ops[i]
		if len(batch) > 1 {
			termui.Log(os.Stderr, "", termui.Info, "[sshore] File %d/%d: %s %s\n", i+1, len(batch), op.Kind, cleanText(file.approvedName(op)))
		}
		return executeApprovedFile(ctx, open, op, file)
	})
	if err != nil {
		termui.Log(os.Stderr, "", termui.Error, "[sshore] Transfer: %d done, 1 failed/cancelled, %d not started. Completed files kept.\n", completed, len(batch)-completed-1)
		for i, file := range batch {
			state := "not started"
			if i < completed {
				state = "completed"
			} else if i == completed {
				state = "failed/cancelled"
			}
			termui.Log(os.Stderr, "", termui.Info, "[sshore] %s: %s (%s)\n", state, cleanText(file.approvedName(ops[i])), cleanText(file.approvedLocalPath(ops[i])))
		}
		termui.Log(os.Stderr, "", termui.Info, "[sshore] Cleanup may leave .sshore-*.part downloads or partial uploads.\n")
		return err
	}
	if len(batch) > 1 {
		termui.Log(os.Stderr, "", termui.Success, "[sshore] Transferred %d/%d files.\n", len(batch), len(batch))
	}
	return nil
}

func executeApprovedFile(ctx context.Context, open func(operation, string, int64) (net.Conn, error), remote operation, file fileCopy) error {
	remote.LocalHint = ""
	remote.LF = false
	if remote.Recursive {
		stream, err := open(remote, "", 0)
		if err != nil {
			return err
		}
		defer stream.Close()
		upload := remote.Kind == "upload"
		source, target := "/", file.target
		if upload {
			source, target = file.source, "/"
		}
		return withCopySFTP(ctx, stream, func(client *sftp.Client) error {
			options := copyOptions{upload: upload, sources: []copyEndpoint{{path: source}}, target: copyEndpoint{path: target}}
			return executeRecursiveCopy(ctx, client, options, !upload)
		})
	}
	name := file.approvedName(remote)
	var filename, result string
	var size int64
	switch remote.Kind {
	case "upload":
		if file.opened == nil {
			return fmt.Errorf("approved upload has no opened source; request again")
		}
		filename, size = name, file.opened.Size()
		result = "Uploaded " + file.source + " -> " + remote.Path
	case "download":
		result = "Saved " + file.target
	default:
		return fmt.Errorf("invalid transfer kind")
	}
	stream, err := open(remote, filename, size)
	if err != nil {
		return err
	}
	defer stream.Close()
	err = withCopySFTP(ctx, stream, func(client *sftp.Client) error {
		completed := new(atomic.Int64)
		defer reportTransferProgress(remote.Kind, name, file.size(), completed)()
		return file.transfer(ctx, client, completed)
	})
	if err != nil {
		return err
	}
	termui.Log(os.Stderr, "", termui.Success, "[sshore] %s\n", cleanText(result))
	return nil
}

func reportTransferProgress(kind, name string, size int64, completed *atomic.Int64) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		progress := transferProgress{at: time.Now()}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				termui.Log(os.Stderr, "", termui.Info, "[sshore] %s %s: %s\n", kind, cleanText(name), progress.sample(now, completed.Load(), size))
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// Rate is a time-smoothed estimate, never a promise of remote persistence.
type transferProgress struct {
	at    time.Time
	bytes int64
	rate  float64
}

func (p *transferProgress) sample(now time.Time, done, total int64) string {
	elapsed := now.Sub(p.at).Seconds()
	if elapsed > 0 {
		rate := float64(max(int64(0), done-p.bytes)) / elapsed
		if p.at.IsZero() {
			rate = 0
		}
		if p.rate == 0 {
			p.rate = rate
		} else {
			p.rate = .3*rate + .7*p.rate
		}
	}
	p.at, p.bytes = now, done
	eta := "unknown"
	if done >= total {
		eta = "0s"
	} else if p.rate >= 1 {
		seconds := float64(total-done) / p.rate
		if seconds < float64((1<<63-1)/int64(time.Second)) {
			eta = (time.Duration(seconds) * time.Second).String()
		}
	}
	return fmt.Sprintf("%s/%s · %s/s · remaining %s", displayBytes(done), displayBytes(total), displayBytes(int64(p.rate)), eta)
}
