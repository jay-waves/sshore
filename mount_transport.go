package main

import (
	"io"

	"github.com/pkg/sftp"
	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/mount"
)

func mountSSHArgs(o ftpOptions) []string {
	if o.noWatch {
		return sftpSSHArgs(o.host, o.ssh)
	}
	server := o.server
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; server=" + shellQuote(server) + "; if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); "
	if server == "sshore" {
		command += "else server=\"$HOME/.local/bin/sshore\"; "
	} else {
		command += "else :; "
	}
	command += "fi; if [ ! -x \"$server\" ]; then printf '%s\\n' 'sshore: mount notifications require remote sshore; install the matching release or use --no-watch' >&2; exit 1; fi; exec \"$server\" mount-agent"
	args := append(ownedSSHArgs("default"), "-o", "ClearAllForwardings=yes")
	args = append(args, o.ssh...)
	return append(args, "--", o.host, "sh -c "+shellQuote(command))
}

func openFTPMux(c io.ReadWriteCloser) (*smux.Session, *smux.Stream, *smux.Stream, error) {
	return mount.OpenMux(c, version)
}
func newFTPSFTP(r io.Reader, w io.WriteCloser) (*sftp.Client, error) { return mount.NewSFTP(r, w) }
