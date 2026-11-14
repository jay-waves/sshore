package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pkg/sftp"
)

type copyEndpoint struct {
	host, path   string
	pick, stream bool
}
type copyOptions struct {
	sources []copyEndpoint
	target  copyEndpoint
	host    string
	upload  bool
	ssh     []string
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	if value == "-" {
		return copyEndpoint{stream: true}, nil
	}
	if originPlaceholder(value) {
		return copyEndpoint{pick: true}, nil
	}
	if strings.HasPrefix(value, "origin:") {
		return copyEndpoint{path: strings.TrimPrefix(value, "origin:")}, nil
	}
	// A drive prefix and paths containing a separator before ':' are local.
	if len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
		return copyEndpoint{path: value}, nil
	}
	bracket := false
	for i, c := range value {
		if c == '[' {
			bracket = true
		}
		if c == ']' {
			bracket = false
		}
		if !bracket && (c == '/' || c == '\\') {
			break
		}
		if c == ':' && !bracket {
			host, err := endpointHost(value[:i])
			if err != nil {
				return copyEndpoint{}, err
			}
			remote := value[i+1:]
			if remote == "" {
				remote = "."
			}
			return copyEndpoint{host: host, path: remote}, nil
		}
	}
	if value == "" {
		return copyEndpoint{}, usage("empty copy path")
	}
	return copyEndpoint{path: value}, nil
}

func parseCopyCommand(args []string) (copyOptions, error) {
	var o copyOptions
	var err error
	args, o.ssh, err = splitSSHOptions(args, false)
	if err != nil {
		return o, err
	}
	if len(args) < 2 {
		return o, usage("use cp SOURCE... TARGET [-- SSH_OPTIONS]")
	}
	var endpoints []copyEndpoint
	for _, a := range args {
		if a != "-" && strings.HasPrefix(a, "-") {
			return o, usage("prefix filenames beginning with '-' with ./")
		}
		e, err := parseCopyEndpoint(a)
		if err != nil {
			return o, err
		}
		endpoints = append(endpoints, e)
	}
	o.sources, o.target = endpoints[:len(endpoints)-1], endpoints[len(endpoints)-1]
	o.upload = o.target.host != ""
	if o.upload {
		o.host = o.target.host
	}
	for _, source := range o.sources {
		if (source.pick || source.stream || o.target.stream) && len(o.sources) != 1 {
			return o, usage("selection and standard streams require exactly one source")
		}
		if o.upload {
			if source.host != "" {
				return o, usage("remote-to-remote copies are not supported")
			}
		} else {
			if source.host == "" {
				return o, usage("cp requires a remote source or destination")
			}
			if o.host != "" && o.host != source.host {
				return o, usage("all sources must use the same SSH destination")
			}
			o.host = source.host
		}
	}
	if len(o.sources) > maxApprovalItems {
		return o, usage("at most %d files per copy", maxApprovalItems)
	}
	return o, nil
}

func runCopy(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, _, _, err := remoteCopyRequest(args); err != nil {
			return 2, err
		}
		return 0, serverCommand(append([]string{"cp"}, args...))
	}
	o, err := parseCopyCommand(args)
	if err != nil {
		return 2, err
	}
	if o.upload && o.sources[0].pick {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		logf(os.Stdout, logInfo, "Copy to %s:%s; submitting the source starts the copy.\n", cleanText(o.host), cleanText(o.target.path))
		source, err := readCopyPath(input, "Local file (Tab; Enter starts): ", "", func(p string) (string, error) {
			f, e := openSending(p)
			if e == nil {
				f.close()
			}
			return p, e
		})
		closeErr := input.Close()
		if err != nil {
			return 1, err
		}
		if closeErr != nil {
			return 1, closeErr
		}
		o.sources = []copyEndpoint{{path: source}}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exe := sshExecutable()
	cmd := exec.CommandContext(ctx, exe, sftpSSHArgs(o.host, o.ssh)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	if err = cmd.Start(); err != nil {
		return 1, err
	}
	defer func() { cancel(); _ = stdin.Close(); _ = stdout.Close(); _ = cmd.Wait() }()
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close(); _ = stdout.Close() })
	defer stop()
	client, err := newFTPSFTP(stdout, stdin)
	if err != nil {
		return 1, err
	}
	defer func() { _ = stdout.Close(); _ = stdin.Close(); _ = client.Close() }()
	if o.sources[0].stream {
		stopInput := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
		defer stopInput()
	}
	if o.sources[0].stream || o.target.stream {
		err = executeStreamCopy(client, o, os.Stdin, os.Stdout)
	} else {
		err = executeCopy(client, o)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Streams refer to this process, independently of origin endpoint selection.
func executeStreamCopy(client *sftp.Client, o copyOptions, input io.Reader, output io.Writer) error {
	if o.upload {
		if err := safeName(path.Base(o.target.path)); err != nil {
			return err
		}
		if _, err := client.Lstat(o.target.path); err == nil {
			return fmt.Errorf("stdin requires a new destination filename: %s", o.target.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return copyUploadReader(client, input, o.target.path, -1)
	}
	source := o.sources[0].path
	info, err := client.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("only ordinary files are supported")
	}
	return copyDownloadTo(client, source, output, info.Size(), nil)
}

func executeCopy(client *sftp.Client, o copyOptions) error {
	sourceStat, targetStat, targetLstat := client.Lstat, os.Stat, os.Lstat
	base, join := path.Base, filepath.Join
	transfer := copyDownload
	if o.upload {
		sourceStat, targetStat, targetLstat = os.Lstat, client.Stat, client.Lstat
		base, join = filepath.Base, path.Join
		transfer = copyUpload
	}

	// Preflight the full batch and collect all destination choices before writing.
	type item struct {
		source, target string
		size           int64
	}
	var items []item
	var input *approvalInput
	if o.target.pick {
		var err error
		input, err = newLocalInput()
		if err != nil {
			return err
		}
		defer input.Close()
		logf(os.Stdout, logInfo, "Copy from %s; submitting all destinations starts the batch. Ctrl-C cancels.\n", cleanText(o.host))
	}
	targetDir := false
	if !o.target.pick {
		info, err := targetStat(o.target.path)
		if err == nil {
			targetDir = info.IsDir()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(o.sources) > 1 && !targetDir {
			return fmt.Errorf("multiple sources require a destination directory")
		}
	}
	seen := map[string]bool{}
	for _, source := range o.sources {
		name := base(source.path)
		info, err := sourceStat(source.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("only ordinary files are supported")
		}
		if err = safeName(name); err != nil {
			return err
		}
		target := o.target.path
		if targetDir {
			target = join(target, name)
		}
		if input != nil {
			logf(os.Stdout, logInfo, "Copy %s:%s · %s\n", cleanText(o.host), cleanText(source.path), displayBytes(info.Size()))
			target, err = readCopyPath(input, "Save as (Tab; Enter accepts): ", "", func(p string) (string, error) { return downloadDestination(p, name) })
			if err != nil {
				return err
			}
		}
		_, err = targetLstat(target)
		if err == nil {
			return fmt.Errorf("destination already exists: %s", target)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if seen[target] {
			return fmt.Errorf("duplicate destination: %s", target)
		}
		seen[target] = true
		items = append(items, item{source.path, target, info.Size()})
	}
	if input != nil {
		if err := input.Close(); err != nil {
			return err
		}
	}
	for i, item := range items {
		err := transfer(client, item.source, item.target, item.size, nil)
		if err != nil {
			return fmt.Errorf("%d completed, copy failed, %d not started: %w", i, len(items)-i-1, err)
		}
		logf(os.Stderr, logSuccess, "Copied %s -> %s\n", cleanText(item.source), cleanText(item.target))
	}
	return nil
}

const copyHelp = `Usage: sshore cp SOURCE... TARGET [-- SSH_OPTIONS]

On your computer, use HOST:PATH for the remote side and a local path for the
other. HOST is an SSH alias or user@host; bracket IPv6. Relative remote paths
start in the remote account's login directory. An empty remote path means ".".
origin:PATH explicitly names a local path. Use ./ to disambiguate filenames with
a colon or leading hyphen. Windows drive paths are local.
A bare - is stdin in SOURCE position or stdout in TARGET position, for local
commands with exactly one remote file. Stdin requires a new destination filename,
not a directory; EOF completes the upload. Data streams directly without local
spooling. Stdout contains only file bytes; errors go to stderr. A failed stdout
copy may already have emitted partial data. Use ./- or origin:- for a file named -.
Standard streams are not available in remote approval requests.
At most 16 regular files; no symlinks, recursive directories, overwrites, metadata
copying, local-only or remote-to-remote copies. Multiple sources require a target
directory. A single target may be a directory or a new filename.
Local copies use an independent SFTP connection; no shell session, remote sshore
installation, FUSE or WinFsp is required. File destinations must support hard links.

origin: and : are equivalent placeholders for a locally selected path.
On your computer, they open a local selector. Inside a remote sshore shell,
exactly one side must name origin: (or :): cp origin: DIRECTORY requests an upload;
cp FILE... origin: requests downloads. A filled origin:PATH is an editable local
review suggestion; Enter accepts it. Remote requests detach; run sshore review
locally. origin means the current connection's client, never the invoking shell.
A suggested path never skips review. Relative local hints use the review working
directory. The remote upload target must be a directory.
Tab completes paths. Submitting all valid paths starts copying without an extra
yes prompt. Ctrl-C during selection cancels before any copy starts.
Batches execute sequentially; a failure keeps completed files and stops the rest.
Interrupted copies remove temporary files when possible; forced termination may
leave .sshore-*.part files. Files are published atomically without overwriting.

SSH options apply only to local direct copies, and only after --:
-p PORT, -i KEY, -J JUMP, -F CONFIG, -l USER, -o OPTION, -E FILE,
-C, -v/-vv/-vvv, -4/-6. -L/-R/-D are not accepted.
-h, --help shows this help.

Examples:
  sshore cp work:/tmp/report.txt - | less
  generate-report | sshore cp - work:/tmp/report.txt
  sshore cp report.zip work:/tmp/
  sshore cp work:/tmp/report.zip ./downloads/
  sshore cp a.zip b.zip work:/tmp/
  sshore cp report.zip origin:        Remote request: choose a local destination
  sshore cp : .                      Remote request: choose a local source
`
