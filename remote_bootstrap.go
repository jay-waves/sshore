package main

// Bootstrap runs in the original authenticated SSH connection. Resolve the
// configured server through PATH, then ~/.local/bin/sshore for the default name;
// explicit server paths are respected. Matching versions execute directly without
// querying GitHub. A mismatched program is never overwritten during bootstrap:
// prepare a checksum/version-verified temporary coordinator using install.sh.
// Its directory is staging, not a PATH installation. Suppress successful installer
// output but retain failure diagnostics. The coordinator negotiates confirmation
// or recovery before replacing the target. A failed remote download requests a
// verified executable from the client over the same authenticated connection.

import (
	"bytes"
	"fmt"
	"strings"
)

const installerURL = "https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh"

// Encode a normal fatal frame before a remote Go executable is available.
// Octal escapes keep binary framing and JSON out of shell interpolation.
func bootstrapFailure(reason string) string {
	return bootstrapFrame(message{Type: "fatal", Error: reason}) + "; exit 1"
}

func bootstrapFrame(m message) string {
	var frame bytes.Buffer
	_ = writeMessage(&frame, m)
	var escaped strings.Builder
	for _, b := range frame.Bytes() {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	return "printf " + shellQuote(escaped.String())
}

// Match compareVersions before downloading a coordinator. Only canonical full
// releases are ordered; failed probes and custom labels proceed to manual recovery.
const newerReleaseAWK = `END {
    release = "(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)"
    if (NR != 1 || $0 !~ ("^sshore " release "$") || local !~ ("^" release "$")) exit 1
    split(local, l, "."); split($2, r, ".")
    for (i = 1; i <= 3; i++) {
        if (length(r[i]) > length(l[i])) exit 0
        if (length(r[i]) < length(l[i])) exit 1
        if (("x" r[i]) > ("x" l[i])) exit 0
        if (("x" r[i]) < ("x" l[i])) exit 1
    }
    exit 1
}`

// The bootstrap runs inside the original SSH connection. A mismatched executable
// is never overwritten here: first prepare a verified, temporary coordinator so
// even an old server can offer an interactive, nonpersistent recovery shell.
func remoteCommand(o options) string {
	if o.server == "" {
		o.server = "sshore"
	}
	mode := "bridge"
	if o.command == "spawn" {
		mode = "spawn"
	} else if o.command == "launch" {
		mode = "new"
	} else if o.command == "attach" {
		mode = "attach"
	} else if o.command == "mount-agent" || o.command == "cp-agent" {
		mode = o.command
	}
	if len(o.approved) > 0 {
		mode = "resume"
	}
	if o.list {
		mode = "sessions"
	}
	var b strings.Builder
	b.WriteString("server=" + shellQuote(o.server) + "; ")
	b.WriteString("if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); ")
	if o.server == "sshore" {
		b.WriteString("else server=\"$HOME/.local/bin/sshore\"; ")
	} else {
		b.WriteString("else :; ")
	}
	b.WriteString("fi; case \"$server\" in /*) ;; *) server=\"$PWD/$server\" ;; esac; ")
	b.WriteString("remote_version=; if [ -e \"$server\" ]; then remote_version=$(\"$server\" --version) || remote_version=; fi; ")
	if o.list {
		b.WriteString("if [ \"$remote_version\" != " + shellQuote("sshore "+version) + " ]; then printf '%s\\n' 'sshore: inventory requires matching remote release; no software changed' >&2; exit 1; fi; exec \"$server\" inventory " + shellQuote(version))
		if o.listDetails {
			b.WriteString(" --verbose")
		}
		return "sh -c " + shellQuote(b.String())
	}
	// Destructive management never enters setup: a kill must not upgrade or end
	// unrelated sessions. The remote command independently checks its version.
	if o.command == "cancel" || o.command == "cancel-resource" || o.command == "rename" {
		b.WriteString("if [ \"$remote_version\" != " + shellQuote("sshore "+version) + " ]; then printf '%s\\n' " + shellQuote("sshore: "+o.command+" requires the installed remote version to match "+version+"; no software changed") + " >&2; exit 1; fi; ")
		if o.command == "cancel-resource" {
			b.WriteString("exec \"$server\" cancel-resource " + shellQuote(version) + " " + shellQuote(o.session) + " " + shellQuote(o.taskIdentity))
			return "sh -c " + shellQuote(b.String())
		}
		actionArgs := shellQuote(o.session)
		if o.command == "rename" {
			actionArgs += " " + shellQuote(o.newName)
		}
		action := o.command
		if action == "cancel" {
			action = "kill"
		}
		b.WriteString("exec \"$server\" " + action + "-session " + actionArgs + " " + shellQuote(version))
		if o.instance != "" {
			b.WriteString(" " + shellQuote(o.instance))
		}
		return "sh -c " + shellQuote(b.String())
	}
	// Check direction before downloading anything. Unknown/custom labels are left
	// to the coordinator; they never authorize an automatic destructive upgrade.
	b.WriteString("if printf '%s\\n' \"$remote_version\" | awk -v local=" + shellQuote(version) + " " + shellQuote(newerReleaseAWK) + "; then printf '%s\\n' " + shellQuote("sshore: remote executable is newer than local "+version+"; update the local client. No downgrade performed.") + " >&2; exit 1; fi; ")
	args := " bootstrap \"$server\" " + shellQuote(mode) + " " + shellQuote(o.session)
	b.WriteString("if [ \"$remote_version\" = " + shellQuote("sshore "+version) + " ]; then exec \"$server\"" + args + "; fi; ")
	b.WriteString("if [ \"$(uname -s)\" != Linux ] || [ \"$(uname -m)\" != x86_64 ]; then " + bootstrapFailure("automatic setup supports Linux amd64 only") + "; fi; ")
	b.WriteString("printf '%s\\n' " + shellQuote("[sshore] Downloading sshore "+version+" and verifying checksum...") + " >&2; ")
	b.WriteString("umask 077; setup_dir=$(mktemp -d \"${TMPDIR:-/tmp}/sshore-setup.XXXXXXXX\") || { " + bootstrapFailure("remote setup: cannot create staging directory; no shells stopped") + "; }; trap 'rm -rf -- \"$setup_dir\"' EXIT; trap 'exit 130' INT; trap 'exit 143' TERM HUP; ")
	b.WriteString("if ! { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 --retry 2 " + shellQuote(installerURL) + " -o \"$setup_dir/install.sh\" && sh \"$setup_dir/install.sh\" --version " + shellQuote(version) + " --bin-dir \"$setup_dir/bin\"; } >\"$setup_dir/install.log\" 2>&1; then ")
	b.WriteString("cat \"$setup_dir/install.log\" >&2; ")
	b.WriteString(bootstrapFrame(message{Type: "setup_download", SoftwareVersion: version, Action: "linux-amd64"}) + "; ")
	b.WriteString("IFS=' ' read -r setup_size setup_hash setup_extra || { " + bootstrapFailure("remote setup: client download unavailable; no shells stopped") + "; }; ")
	b.WriteString("case \"$setup_size\" in ''|*[!0-9]*|?????????*) " + bootstrapFailure("remote setup: invalid transfer size") + ";; esac; ")
	b.WriteString("if [ \"$setup_size\" -le 0 ] || [ \"$setup_size\" -gt 67108864 ] || [ -n \"$setup_extra\" ] || [ \"${#setup_hash}\" -ne 64 ]; then " + bootstrapFailure("remote setup: client download failed or transfer invalid; no shells stopped") + "; fi; ")
	b.WriteString("case \"$setup_hash\" in *[!0-9a-f]*) " + bootstrapFailure("remote setup: invalid transfer checksum") + ";; esac; ")
	b.WriteString("mkdir -p \"$setup_dir/bin\" && head -c \"$setup_size\" > \"$setup_dir/bin/sshore\" || { " + bootstrapFailure("remote setup: transfer failed; no shells stopped") + "; }; ")
	b.WriteString("setup_actual=$(sha256sum < \"$setup_dir/bin/sshore\") || { " + bootstrapFailure("remote setup: checksum tool failed; no shells stopped") + "; }; if [ \"$(wc -c < \"$setup_dir/bin/sshore\")\" -ne \"$setup_size\" ] || [ \"${setup_actual%% *}\" != \"$setup_hash\" ]; then " + bootstrapFailure("remote setup: transferred executable checksum mismatch; no shells stopped") + "; fi; ")
	b.WriteString("chmod 700 \"$setup_dir/bin/sshore\" || { " + bootstrapFailure("remote setup: cannot prepare executable; no shells stopped") + "; }; fi; ")
	b.WriteString("[ \"$(\"$setup_dir/bin/sshore\" --version)\" = " + shellQuote("sshore "+version) + " ] || { " + bootstrapFailure("remote setup: prepared binary version mismatch. No shells stopped.") + "; }; ")
	b.WriteString("\"$setup_dir/bin/sshore\"" + args)
	return "sh -c " + shellQuote(b.String())
}
