package main

import "strings"

const installerURL = "https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh"

func installCommand() string {
	return "curl -fsSL " + installerURL + " | sh -s -- --version " + shellQuote(version)
}

// Probe inside the same authenticated SSH session. Only the user can choose to
// run the printed installer; this command never downloads or writes anything.
// An explicit sh wrapper also works when the remote login shell is not POSIX.
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
	b.WriteString("if ! command -v \"$server\" >/dev/null 2>&1; then ")
	if o.server == "sshore" {
		b.WriteString("if [ -x \"$HOME/.local/bin/sshore\" ] && [ ! -d \"$HOME/.local/bin/sshore\" ]; then server=\"$HOME/.local/bin/sshore\"; else ")
	}
	lines := []string{
		"sshore: remote executable not found or not executable: " + cleanText(o.server),
		"Log in with ordinary SSH, then run this command on the REMOTE Linux host:",
		"  " + installCommand(),
		"This is a suggestion only; no installation has been performed.",
	}
	if o.server != "sshore" {
		lines = append(lines, "The installer defaults to ~/.local/bin/sshore. Adjust --server to that path, or install into your chosen directory.")
	}
	b.WriteString("printf '%s\\n'")
	for _, line := range lines {
		b.WriteString(" " + shellQuote(line))
	}
	b.WriteString(" >&2; exit 127; ")
	if o.server == "sshore" {
		b.WriteString("fi; ")
	}
	b.WriteString("fi; exec \"$server\" " + mode)
	if !o.list {
		b.WriteString(" " + shellQuote(o.session))
	}
	return "sh -c " + shellQuote(b.String())
}
