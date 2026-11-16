package main

import (
	"github.com/jay-waves/sshore/internal/files"
	"runtime"
	"strings"
	"time"
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
	// Windows drive prefixes are local only on Windows. On Linux, a single
	// letter can be a valid SSH host alias.
	if runtime.GOOS == "windows" && len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
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
	parsed, err := parseCLI(args, "cp")
	if err != nil {
		return o, err
	}
	args, o.ssh = parsed.operands, parsed.ssh
	var endpoints []copyEndpoint
	for _, a := range args {
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

type ftpOptions struct {
	approval                  *files.MountApproval
	ioTimeout, startupTimeout time.Duration
	host, remote, mountpoint  string
	ssh                       []string
	readOnly                  bool
	directIO                  bool
	foreground                bool
	stats                     bool
	noWatch                   bool
	server                    string
	name                      string
}

func parseFTPOptions(args []string) (ftpOptions, error) {
	parsed, err := parseCLI(args, "mount")
	if err != nil {
		return ftpOptions{}, err
	}
	for _, value := range parsed.operands {
		if originEndpoint(value) {
			return ftpOptions{}, usage("origin: mount requests require an attached sshore shell; run mount HOST DIRECTORY locally")
		}
	}
	o := ftpOptions{
		host: parsed.operand(0, ""), remote: parsed.operand(1, "."), mountpoint: parsed.operand(2, ""),
		ssh: parsed.ssh, name: parsed.values["--name"], server: parsed.value("--server", "sshore"),
		foreground: parsed.has("--foreground"), noWatch: parsed.has("--no-watch"),
		readOnly: parsed.has("--read-only"), stats: parsed.has("--stats"), directIO: parsed.has("--direct-io"),
		ioTimeout: parsed.duration("--io-timeout", 60*time.Second), startupTimeout: parsed.duration("--startup-timeout", 0),
	}
	if err := validSSHDestination(o.host); err != nil {
		return o, err
	}
	if strings.ContainsRune(o.remote, 0) || strings.ContainsRune(o.mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}
