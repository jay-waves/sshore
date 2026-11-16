package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCompletionShellAdapters(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := shellQuote(binary) + " '-test.run=^TestCompletionQueryProcess$' --"
	for _, shell := range []string{"bash", "zsh", "fish", "pwsh"} {
		executable, err := exec.LookPath(shell)
		if err != nil {
			t.Run(shell, func(t *testing.T) { t.Skip("shell unavailable") })
			continue
		}
		t.Run(shell, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a file"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "a dir"), 0700); err != nil {
				t.Fatal(err)
			}
			kind := shell
			if kind == "pwsh" {
				kind = "powershell"
			}
			// A function wraps the test executable without changing the shared PATH.
			var program string
			switch shell {
			case "bash":
				script := strings.ReplaceAll(completionScripts[kind], "command sshore __complete", helper)
				program = script + `
COMP_WORDBREAKS=$' \t\n"\047><=;|&(:'
COMP_LINE='sshore att'; COMP_POINT=${#COMP_LINE}; _sshore_complete; printf '%s\n' "${COMPREPLY[@]}"
COMP_LINE='sshore cp a'; COMP_POINT=${#COMP_LINE}; _sshore_complete; printf '%s\n' "${COMPREPLY[@]}"
COMP_LINE='sshore launch work --download-dir=a'; COMP_POINT=${#COMP_LINE}; _sshore_complete; printf '%s\n' "${COMPREPLY[@]}"
`
			case "zsh":
				script := strings.ReplaceAll(completionScripts[kind], "command sshore __complete", helper)
				// Exercise argument extraction/data handling outside ZLE. Native file and
				// compadd calls are observed; their APIs belong to zsh itself.
				program = `compdef() { :; }
compadd() { print -rl -- "$@"; }
compset() { :; }
_files() { print -r -- 'a file'; print -r -- 'a dir'; }
` + script + `
words=(sshore att); CURRENT=2; _sshore_complete
words=(sshore cp a); CURRENT=3; _sshore_complete
words=(sshore launch work --download-dir=a); CURRENT=4; _sshore_complete
`
			case "fish":
				script := strings.ReplaceAll(completionScripts[kind], "command sshore __complete", helper)
				program = script + `
complete -C 'sshore att'
complete -C 'sshore cp a'
complete -C 'sshore launch work --download-dir=a'
`
			case "pwsh":
				script := strings.ReplaceAll(completionScripts[kind], "@(sshore __complete", "@(& '"+strings.ReplaceAll(binary, "'", "''")+"' '-test.run=^TestCompletionQueryProcess$' --")
				program = script + `
foreach ($line in @('sshore att', 'sshore cp a', 'sshore launch work --download-dir=a')) {
 [System.Management.Automation.CommandCompletion]::CompleteInput($line, $line.Length, $null).CompletionMatches | ForEach-Object { $_.CompletionText }
}
`
			}
			args := []string{"-c", program}
			if shell == "zsh" {
				args = []string{"-f", "-c", program}
			}
			if shell == "pwsh" {
				args = []string{"-NoProfile", "-NonInteractive", "-Command", program}
			}
			cmd := exec.Command(executable, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "SSHORE_COMPLETION_TEST=1", "SSHORE_DISABLED=", "SSHORE_STATE_DIR="+filepath.Join(dir, "state"), "HOME="+dir, "GORACE=atexit_sleep_ms=0")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			text := string(output)
			for _, want := range []string{"attach", "a file", "a dir"} {
				if !strings.Contains(strings.ReplaceAll(text, `\ `, " "), want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			if strings.Contains(text, "Unknown command") || strings.Contains(text, "command not found") {
				t.Fatal(text)
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatal("completion created state", err)
			}
		})
	}
}

func TestCompletionQueryProcess(t *testing.T) {
	if os.Getenv("SSHORE_COMPLETION_TEST") != "1" {
		return
	}
	at := slices.Index(os.Args, "--")
	if at < 0 {
		os.Exit(2)
	}
	if err := runCompletionQuery(os.Args[at+1:], os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
