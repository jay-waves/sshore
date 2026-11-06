package main

import "strings"

const installerURL = "https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh"

func installCommand() string {
	return "curl -fsSL " + installerURL + " | sh -s -- --version " + shellQuote(version)
}

// The bootstrap runs inside the original SSH connection. A mismatched executable
// is never overwritten here: first prepare a verified, temporary coordinator so
// even an old server can offer an interactive, nonpersistent recovery shell.
func remoteCommand(o options) string {
	mode := "bridge"
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
	b.WriteString("remote_version=; if [ -e \"$server\" ]; then remote_version=$(\"$server\" --version) || exit $?; fi; ")
	// Check direction before downloading anything. Unknown/custom labels are left
	// to the coordinator; they never authorize an automatic destructive upgrade.
	b.WriteString("if printf '%s\\n' \"$remote_version\" | awk -v local=" + shellQuote(version) + " 'BEGIN {split(local,l,\".\")} /^sshore [0-9]+\\.[0-9]+\\.[0-9]+$/ {split($2,r,\".\"); for(i=1;i<=3;i++){if(length(r[i])>length(l[i]))exit 0; if(length(r[i])<length(l[i]))exit 1; if((\"x\" r[i])>(\"x\" l[i]))exit 0; if((\"x\" r[i])<(\"x\" l[i]))exit 1}} {exit 1}'; then printf '%s\\n' " + shellQuote("sshore: remote executable is newer than local "+version+"; update the local client. No downgrade performed.") + " >&2; exit 1; fi; ")
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
