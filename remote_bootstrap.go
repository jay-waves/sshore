package main

// Bootstrap runs in the original authenticated SSH connection. Resolve the
// configured server through PATH, then ~/.local/bin/sshore for the default name;
// explicit server paths are respected. Matching versions execute directly without
// querying GitHub. A mismatched program is never overwritten during bootstrap:
// prepare a checksum/version-verified temporary coordinator using install.sh.
// Its directory is staging, not a PATH installation. Suppress successful installer
// output but retain failure diagnostics. The coordinator negotiates confirmation
// or recovery before replacing the target. Preparation needs remote HTTPS access
// and a matching published asset; custom builds may need manual installation.

import "strings"

const installerURL = "https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh"

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
	mode := "bridge"
	if o.command == "new" || o.command == "attach" {
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
	// Destructive management never enters setup: a kill must not upgrade or end
	// unrelated sessions. The remote command independently checks its version.
	if o.command == "kill" || o.command == "rename" {
		b.WriteString("if [ \"$remote_version\" != " + shellQuote("sshore "+version) + " ]; then printf '%s\\n' " + shellQuote("sshore: "+o.command+" requires the installed remote version to match "+version+"; no software changed") + " >&2; exit 1; fi; ")
		actionArgs := shellQuote(o.session)
		if o.command == "rename" {
			actionArgs += " " + shellQuote(o.newName)
		}
		b.WriteString("exec \"$server\" " + o.command + "-session " + actionArgs + " " + shellQuote(version))
		return "sh -c " + shellQuote(b.String())
	}
	// Check direction before downloading anything. Unknown/custom labels are left
	// to the coordinator; they never authorize an automatic destructive upgrade.
	b.WriteString("if printf '%s\\n' \"$remote_version\" | awk -v local=" + shellQuote(version) + " " + shellQuote(newerReleaseAWK) + "; then printf '%s\\n' " + shellQuote("sshore: remote executable is newer than local "+version+"; update the local client. No downgrade performed.") + " >&2; exit 1; fi; ")
	args := " bootstrap \"$server\" " + shellQuote(mode) + " " + shellQuote(o.session)
	b.WriteString("if [ \"$remote_version\" = " + shellQuote("sshore "+version) + " ]; then exec \"$server\"" + args + "; fi; ")
	b.WriteString("printf '%s\\n' " + shellQuote("[sshore] Downloading sshore "+version+" and verifying checksum...") + " >&2; ")
	b.WriteString("umask 077; setup_dir=$(mktemp -d \"${TMPDIR:-/tmp}/sshore-setup.XXXXXXXX\") || exit 1; trap 'rm -rf -- \"$setup_dir\"' EXIT; trap 'exit 130' INT; trap 'exit 143' TERM HUP; ")
	b.WriteString("curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 180 " + shellQuote(installerURL) + " -o \"$setup_dir/install.sh\" || exit 1; ")
	b.WriteString("if ! sh \"$setup_dir/install.sh\" --version " + shellQuote(version) + " --bin-dir \"$setup_dir/bin\" >\"$setup_dir/install.log\" 2>&1; then cat \"$setup_dir/install.log\" >&2; exit 1; fi; ")
	b.WriteString("[ \"$(\"$setup_dir/bin/sshore\" --version)\" = " + shellQuote("sshore "+version) + " ] || exit 1; ")
	b.WriteString("\"$setup_dir/bin/sshore\"" + args)
	return "sh -c " + shellQuote(b.String())
}
