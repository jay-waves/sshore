package main

// mount exposes an SFTP directory through a dedicated system OpenSSH connection.
// It does not attach to, create, or own a persistent sshore PTY session. SSH
// options are deliberately limited to transport/authentication settings; mount
// lifetime and writes must not inherit backgrounding or forwarding options.

import (
	"runtime"
	"strings"
	"time"
)

type ftpOptions struct {
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
	o := ftpOptions{ioTimeout: 60 * time.Second, server: "sshore"}
	parsed, err := parseCLI(args, []cliFlag{
		{"--name", true}, {"--server", true}, {"--foreground", false}, {"--no-watch", false},
		{"--read-only", false}, {"--io-timeout", true}, {"--startup-timeout", true}, {"--stats", false}, {"--direct-io", false},
	}, true, false)
	if err != nil {
		return o, err
	}
	positional := parsed.operands
	o.ssh = parsed.ssh
	o.name = parsed.values["--name"]
	if parsed.has("--name") {
		if err := validSession(o.name); err != nil {
			return o, err
		}
	}
	if parsed.has("--server") {
		o.server = parsed.values["--server"]
		if strings.ContainsAny(o.server, "\x00\r\n") {
			return o, usage("invalid server path")
		}
	}
	o.foreground, o.noWatch = parsed.has("--foreground"), parsed.has("--no-watch")
	o.readOnly, o.stats, o.directIO = parsed.has("--read-only"), parsed.has("--stats"), parsed.has("--direct-io")
	for _, timeout := range []struct {
		name   string
		target *time.Duration
	}{{"--io-timeout", &o.ioTimeout}, {"--startup-timeout", &o.startupTimeout}} {
		if !parsed.has(timeout.name) {
			continue
		}
		d, err := time.ParseDuration(parsed.values[timeout.name])
		if err != nil || d < 0 {
			return o, usage("%s requires a nonnegative duration", timeout.name)
		}
		*timeout.target = d
	}
	if o.noWatch && parsed.has("--server") {
		return o, usage("--server requires file notifications; remove --no-watch or --server")
	}
	if len(positional) < 1 || len(positional) > 3 {
		return o, usage("mount requires HOST [REMOTE_DIRECTORY [MOUNTPOINT]]")
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
	if err := validSSHDestination(o.host); err != nil {
		return o, err
	}
	if strings.ContainsRune(o.remote, 0) || strings.ContainsRune(o.mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}
