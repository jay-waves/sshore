package main

// ftp mounts an SFTP directory through a dedicated system OpenSSH connection.
// It does not attach to, create, or own a persistent sshore PTY session. SSH
// options are deliberately limited to transport/authentication settings; mount
// lifetime and writes must not inherit backgrounding or forwarding options.

import (
	"runtime"
	"strings"
)

type ftpOptions struct {
	host, remote, mountpoint string
	ssh                      []string
	readOnly                 bool
	directIO                 bool
	foreground               bool
	name                     string
}

const ftpHelp = `Usage: sshore ftp HOST [REMOTE_DIRECTORY [MOUNTPOINT]] [options]
       sshore ftp --list
       sshore ftp --stop NAME

Mount a remote directory for local applications using SFTP (not plain FTP).
Starts a named local background mount after SSH authentication succeeds.
Defaults: remote home directory; ~/mnt/NAME on Linux, free drive on Windows.
Use sshore list to find mount paths and sshore stop mount/NAME to unmount.
--list and --stop NAME remain convenience aliases.
With --foreground, Ctrl-C requests a graceful unmount (10s grace).
A second Ctrl-C while waiting forces disconnect and can interrupt writes.
A busy mount stays connected; close files and retry. Uses a separate SSH connection.
Requires WinFsp on Windows or FUSE on Linux. Missing WinFsp prints its install URL.
No persistent write-back cache. Background mounts publish remote inventory only;
local paths/control tokens are not shared. Sync failure does not fail the mount.

Options:
  --name NAME                   Local mount session name (default: derived from HOST)
  --foreground                  Keep this mount in the terminal
  --read-only                   Reject changes through this mount
  --direct-io                   Linux: disable kernel file read caching
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -C, -v, -vv, -vvv, -4, -6    Pass through to system OpenSSH
  -h, --help                    Show command help
  --                            End options

Examples:
  sshore ftp work
  sshore ftp --list
  sshore ftp --stop work
  sshore ftp work /home/alice/project ./remote-project
  sshore ftp work /home/alice/project X:
`

func parseFTPOptions(args []string) (ftpOptions, error) {
	var o ftpOptions
	var positional []string
	literal := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			return o, usage("empty ftp argument")
		}
		if literal {
			positional = append(positional, a)
			continue
		}
		switch a {
		case "--":
			literal = true
		case "-h", "--help":
			return o, errHelp
		case "--foreground":
			o.foreground = true
		case "--name":
			i++
			if i >= len(args) {
				return o, usage("missing value for --name")
			}
			o.name = args[i]
			if e := validSession(o.name); e != nil {
				return o, e
			}
		case "--direct-io":
			o.directIO = true
		case "--read-only":
			o.readOnly = true
		case "-C", "-v", "-vv", "-vvv", "-4", "-6":
			o.ssh = append(o.ssh, a)
		case "-p", "-i", "-J", "-F", "-l", "-o":
			i++
			if i >= len(args) || args[i] == "" {
				return o, usage("missing value for %s", a)
			}
			o.ssh = append(o.ssh, a, args[i])
		default:
			if strings.HasPrefix(a, "-") {
				return o, usage("unsupported ftp option: %s", a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) < 1 || len(positional) > 3 {
		return o, usage("ftp requires HOST [REMOTE_DIRECTORY [MOUNTPOINT]]")
	}
	if o.directIO && runtime.GOOS != "linux" {
		return o, usage("--direct-io is supported only on Linux")
	}
	o.host = positional[0]
	o.remote = "."
	if len(positional) > 1 {
		o.remote = positional[1]
	}
	if len(positional) > 2 {
		o.mountpoint = positional[2]
	}
	if o.name == "" {
		o.name = ftpDefaultName(o.host)
	}
	if strings.HasPrefix(o.host, "-") || strings.ContainsAny(o.host, "\r\n\x00") {
		return o, usage("invalid SSH destination")
	}
	if strings.ContainsRune(o.remote, 0) || strings.ContainsRune(o.mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}

func ftpDefaultName(host string) string {
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	var b strings.Builder
	for _, c := range host {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
		if b.Len() >= 48 {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "mount"
	}
	return name
}
