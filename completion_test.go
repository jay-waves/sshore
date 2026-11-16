package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func completionValues(r completionResult) []string {
	var values []string
	for _, c := range r.candidates {
		values = append(values, c.value)
	}
	return values
}

func TestCompletionContexts(t *testing.T) {
	catalog := func() completionCatalog {
		return completionCatalog{
			hosts: []string{"work"},
			tasks: []completionTask{{"shell", "localjob", ""}, {"shell", "remotejob", "work"}, {"mount", "data", ""}, {"review", "request", ""}},
		}
	}
	for _, tc := range []struct {
		words        []string
		current      string
		want, absent []string
		mode, prefix string
	}{
		{nil, "", []string{"completion", "launch", "attach"}, nil, "", ""},
		{[]string{"completion"}, "", []string{"bash", "zsh", "fish", "powershell"}, nil, "", ""},
		{[]string{"attach"}, "", []string{"work"}, []string{"1", "2", "3", "data", "4"}, "", ""},
		{[]string{"attach", "work"}, "", []string{"remotejob"}, []string{"localjob", "1"}, "", ""},
		{[]string{"attach", "--local"}, "", []string{"localjob"}, []string{"remotejob", "work"}, "", ""},
		{[]string{"launch", "work"}, "", nil, []string{"remotejob", "1"}, "", ""},
		{[]string{"cancel"}, "", []string{"mount/data", "review/request"}, []string{"localjob", "1", "2", "3", "4"}, "", ""},
		{[]string{"review"}, "", []string{"request"}, []string{"1", "2", "3", "4"}, "", ""},
		{[]string{"rename"}, "", []string{"work", "mount/data", "review/request"}, []string{"1", "2"}, "", ""},
		{[]string{"rename", "work"}, "", []string{"remotejob"}, []string{"localjob", "2"}, "", ""},
		{[]string{"list", "--json"}, "--", nil, []string{"--json", "--verbose"}, "", ""},
		{[]string{"launch", "-q"}, "-", nil, []string{"-q", "--quiet"}, "", ""},
		{[]string{"mount", "--no-watch"}, "--", nil, []string{"--server"}, "", ""},
		{[]string{"launch", "--local"}, "--", nil, []string{"--server", "--download-dir", "--"}, "", ""},
		{[]string{"attach"}, "--", nil, []string{"--no-login", "--detach"}, "", ""},
		{[]string{"launch", "work", "--download-dir"}, "", nil, nil, "dirs", ""},
		{[]string{"launch", "work"}, "--download-dir=folder", nil, nil, "dirs", "--download-dir="},
		{[]string{"mount"}, "wo", []string{"work:"}, []string{"work"}, "", ""},
		{[]string{"mnt", "work:/src"}, "", nil, []string{"origin:"}, "dirs", ""},
		{[]string{"mount", "work:/src", "dst"}, "", nil, nil, "", ""},
		{[]string{"mount", "/srv/project"}, "", []string{"origin:"}, []string{"work"}, "", ""},
		{[]string{"cp"}, "", []string{"work:", "origin:"}, nil, "files", ""},
		{[]string{"cp"}, "work:/", nil, []string{"work:"}, "none", ""},
		{[]string{"forward"}, "", []string{"work:", "origin:"}, nil, "", ""},
		{[]string{"mount", "work:", "--"}, "-", []string{"-p", "-i"}, []string{"-L", "-R", "-D"}, "", ""},
		{[]string{"launch", "work", "--"}, "-", []string{"-L", "-R", "-D"}, nil, "", ""},
		{[]string{"launch", "work", "--", "-i"}, "", nil, nil, "files", ""},
		{[]string{"launch", "work", "--"}, "-ikey", nil, nil, "files", "-i"},
	} {
		t.Run(strings.Join(append(slices.Clone(tc.words), tc.current), " "), func(t *testing.T) {
			r := completeCLI(tc.words, tc.current, catalog)
			values := completionValues(r)
			for _, v := range tc.want {
				if !slices.Contains(values, v) {
					t.Fatalf("missing %q: %+v", v, r)
				}
			}
			for _, v := range tc.absent {
				if slices.Contains(values, v) {
					t.Fatalf("unexpected %q: %+v", v, r)
				}
			}
			if r.mode != tc.mode || r.prefix != tc.prefix {
				t.Fatalf("mode/prefix: %+v", r)
			}
		})
	}
}

func TestCompletionUsesCLIConstraints(t *testing.T) {
	for command := range commandHelp {
		spec, ok := commandSpec(command)
		if !ok {
			t.Fatalf("no grammar for %s", command)
		}
		for _, name := range spec.flags {
			option, ok := cliOptions[name]
			if !ok {
				t.Fatalf("undefined %s", name)
			}
			spelling := name
			if option.alias != "" {
				spelling = option.alias
			}
			_, _, found := spec.option(spelling)
			if !found {
				t.Fatalf("missing alias %s", spelling)
			}
		}
	}
	for _, args := range [][]string{{"-q", "--quiet"}, {"--quiet", "-q"}, {"--local", "--server=x"}} {
		if _, err := parseCLI(args, "launch"); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"--verbose"}, {"--json"}} {
		if _, err := parseCLI(args, "pstree"); err == nil {
			t.Fatalf("pstree accepted %v", args)
		}
	}
}

func TestCompletionLiteralBashWords(t *testing.T) {
	for _, tc := range []struct {
		line    string
		words   []string
		current string
	}{
		{"sshore ", nil, ""},
		{"sshore launch --download-dir='a b", []string{"launch"}, "--download-dir=a b"},
		{"sshore cp a\\ b work:", []string{"cp", "a b"}, "work:"},
		{"echo x; sshore attach work jo", []string{"attach", "work"}, "jo"},
		{"sshore launch work -- -i key", []string{"launch", "work", "--", "-i"}, "key"},
		{"sshore $(touch marker) ", []string{""}, ""},
	} {
		words, current := completionLine(tc.line)
		if !reflect.DeepEqual(words, tc.words) && !(len(words) == 0 && len(tc.words) == 0) || current != tc.current {
			t.Fatalf("%q -> %q, %q", tc.line, words, current)
		}
	}
}

func TestCompletionOfflineState(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	home := filepath.Join(root, "home")
	t.Setenv("SSHORE_STATE_DIR", state)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out bytes.Buffer
	if err := runCompletionQuery([]string{"words", ":", "attach"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("completion created state", err)
	}
	write := func(name string, value any) {
		path := filepath.Join(state, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(value)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	local := localShellRecord{Name: "localjob", Token: strings.Repeat("a", 64)}
	write("local-shells/localjob.json", local)
	write("connections/recent.json", []recentConnection{{Host: "recent", Session: "job"}})
	write("shells/job.json", shellSnapshot{Host: "work", Name: "job", State: "detached", Instance: "instance"})
	write("shells/ended.json", shellSnapshot{Host: "work", Name: "ended", State: "exited"})
	write("mounts/data.json", resourceRecord{Kind: "mount", Name: "data", Token: "secret"})
	write("approvals/pending.json", approvalRecord{ID: strings.Repeat("b", 32), Name: "pending", Created: time.Now()})
	write("approvals/expired.json", approvalRecord{ID: strings.Repeat("c", 32), Name: "expired", Created: time.Now().Add(-2 * approvalTTL)})
	write("tasks/ids.json", taskIDIndex{Next: 8, IDs: map[string]uint64{local.identity(): 7}, Codes: map[string]string{local.identity(): "aa9bec"}})
	config := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(config, "config"), []byte("Host configured alias\nHost wildcard* !excluded\nMatch exec touch-marker\nInclude extra\n"), 0600)
	_ = os.WriteFile(filepath.Join(config, "extra"), []byte("Host included\nInclude config\n"), 0600)
	before := map[string]string{}
	filepath.WalkDir(state, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			before[p] = string(b)
		}
		return e
	})
	out.Reset()
	if err := runCompletionQuery([]string{"words", ":", "attach"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"configured\t", "alias\t", "included\t", "recent\t", "work\t", "aa9bec\t"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "7\t") || strings.Contains(out.String(), local.Token) || strings.Contains(out.String(), "wildcard*") {
		t.Fatal("private/invalid data leaked", out.String())
	}
	c := loadCompletionCatalog()
	for _, task := range c.tasks {
		if task.name == "expired" || task.name == "ended" {
			t.Fatal(task)
		}
	}
	after := map[string]string{}
	filepath.WalkDir(state, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			after[p] = string(b)
		}
		return e
	})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("completion mutated state")
	}
	t.Setenv("SSHORE_DISABLED", "1")
	out.Reset()
	if err := runCompletionQuery([]string{"words", ":", "attach"}, &out); err != nil || out.Len() != 0 {
		t.Fatal("recovery exposed candidates", err, &out)
	}
}

func TestCommandAliasCompletion(t *testing.T) {
	catalog := func() completionCatalog { return completionCatalog{hosts: []string{"work"}} }
	for alias, command := range commandAliases {
		for _, words := range [][]string{nil, {"help"}} {
			if !slices.Contains(completionValues(completeCLI(words, "", catalog)), alias) {
				t.Fatalf("missing alias %s in %v", alias, words)
			}
		}
		for _, current := range []string{"", "--", "--server="} {
			var got, want bytes.Buffer
			if err := completeCLI([]string{alias}, current, catalog).write(&got, current); err != nil {
				t.Fatal(err)
			}
			if err := completeCLI([]string{command}, current, catalog).write(&want, current); err != nil {
				t.Fatal(err)
			}
			if got.String() != want.String() {
				t.Fatalf("different completion for %s %s", alias, current)
			}
		}
	}
}

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

func TestShortTaskCompletionScopes(t *testing.T) {
	local, remote, mount, review := completionTask{"shell", "local", ""}, completionTask{"shell", "remote", "work"}, completionTask{"mount", "data", ""}, completionTask{"review", "request", ""}
	catalog := func() completionCatalog {
		return completionCatalog{tasks: []completionTask{local, remote, mount, review}, codes: map[completionTask]string{local: "111111", remote: "222222", mount: "aaaaaa", review: "bbbbbb"}}
	}
	for _, tc := range []struct {
		words        []string
		want, absent []string
	}{
		{[]string{"cancel"}, []string{"111111", "222222", "mount/aaaaaa", "review/bbbbbb"}, nil},
		{[]string{"rename"}, []string{"mount/aaaaaa", "review/bbbbbb"}, nil},
		{[]string{"attach"}, []string{"111111", "222222"}, []string{"aaaaaa", "bbbbbb"}},
		{[]string{"review"}, []string{"bbbbbb", "review/bbbbbb"}, []string{"111111", "222222", "aaaaaa"}},
		{[]string{"cancel", "--local"}, []string{"111111"}, []string{"222222", "aaaaaa", "bbbbbb"}},
		{[]string{"cancel", "work"}, []string{"222222"}, []string{"111111", "aaaaaa", "bbbbbb"}},
	} {
		values := completionValues(completeCLI(tc.words, "", catalog))
		for _, want := range tc.want {
			if !slices.Contains(values, want) {
				t.Fatalf("%v missing %s: %v", tc.words, want, values)
			}
		}
		for _, absent := range tc.absent {
			if slices.Contains(values, absent) {
				t.Fatalf("%v suggests %s", tc.words, absent)
			}
		}
	}
}
