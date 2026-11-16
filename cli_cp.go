package main

import (
	"runtime"
	"strings"
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
	parsed, err := parseCLI(args, nil, true, false)
	if err != nil {
		return o, err
	}
	args, o.ssh = parsed.operands, parsed.ssh
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
