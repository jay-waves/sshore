package termui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chzyer/readline"
)

func TestFormatApprovalPrompt(t *testing.T) {
	const prompt = "Type update to END listed old shells/tasks and install; Enter for a recovery shell: "
	if got := formatInputPrompt(false, prompt); got != prompt {
		t.Fatalf("uncolored prompt = %q", got)
	}
	got := formatInputPrompt(true, prompt)
	if want := Format(true, Info, "%s", prompt); got != want {
		t.Fatalf("colored prompt = %q, want %q", got, want)
	}
}

func TestCompleteLocalPath(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, name := range []string{"tank_v7585.tar.gz", "tank notes.txt", "中文文件.txt"} {
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir("tank dir", 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input  string
		want   []string
		length int
	}{
		{"tank_", []string{"v7585.tar.gz"}, 5},
		{"tank ", []string{"dir" + string(os.PathSeparator), "notes.txt"}, 5},
		{"中文", []string{"文件.txt"}, 2},
		{`"tank n`, []string{"otes.txt"}, 6},
		{"missing/file", nil, 4},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, length := completeLocalPath([]rune(tc.input), len([]rune(tc.input)))
			var actual []string
			for _, s := range got {
				actual = append(actual, string(s))
			}
			if !reflect.DeepEqual(actual, tc.want) {
				t.Fatalf("got %q, want %q", actual, tc.want)
			}
			if len(got) > 0 && length != tc.length {
				t.Fatalf("length %d, want %d", length, tc.length)
			}
		})
	}
	line := []rune(`"tank n"`)
	got, _ := completeLocalPath(line, len(line)-1)
	if len(got) != 1 || string(got[0]) != "otes.txt" {
		t.Fatal(got)
	}
	if got, _ := completeLocalPath([]rune("tank_notes"), 5); len(got) != 0 {
		t.Fatal("completed inside filename", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, root)
	if err != nil {
		t.Skip("temporary directory is on a different volume")
	}
	homePath := "~/" + filepath.Join(rel, "tank_")
	got, _ = completeLocalPath([]rune(homePath), len([]rune(homePath)))
	if len(got) != 1 || string(got[0]) != "v7585.tar.gz" {
		t.Fatal("home expansion", got)
	}
	abs := filepath.Join(root, "tank_")
	got, _ = completeLocalPath([]rune(abs), len([]rune(abs)))
	if len(got) != 1 || string(got[0]) != "v7585.tar.gz" {
		t.Fatal("absolute path", got)
	}
	p := &Input{}
	if got, _ := p.Do([]rune("tank_"), 5); len(got) != 0 {
		t.Fatal("confirmation has path completion")
	}
}

func TestLocalPathHomeAndUnicode(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, want string }{
		{"~", home},
		{"~/中文文件.txt", filepath.Join(home, "中文文件.txt")},
		{`"中文 文件.txt"`, "中文 文件.txt"},
		{`literal\u4e2d.txt`, `literal\u4e2d.txt`},
	} {
		got, err := ParsePath([]byte(tc.input))
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}

func TestCompletionListing(t *testing.T) {
	candidates := [][]rune{[]rune("folder/"), []rune("file.txt")}
	text, fits := completionListing(candidates, "", 80, 24, true)
	if !fits || !strings.Contains(text, "\x1b[36mfolder/\x1b[39m") || strings.Contains(text, "\x1b[36mfile.txt") {
		t.Fatalf("directory-only color: %q", text)
	}
	text, _ = completionListing(candidates, "", 80, 24, false)
	if strings.Contains(text, "\x1b") {
		t.Fatal("uncolored output contains escapes", text)
	}
	for _, size := range [][2]int{{80, 6}, {10, 24}} {
		text, fits = completionListing(candidates, "long-prefix-", size[0], size[1], true)
		if fits || !strings.Contains(text, "2 matches;") || strings.Contains(text, "folder") {
			t.Fatalf("small terminal should show hint: %q", text)
		}
	}
	text, fits = completionListing(make([][]rune, 200), "", 80, 24, true)
	if fits || text != "200 matches; type more to narrow the list.\n" {
		t.Fatal(text)
	}
}

func TestCompletionSelectionPreservesPath(t *testing.T) {
	p := &Input{paths: true, completion: pathCompletion{
		line: []rune(`"folder/文"`), pos: len([]rune(`"folder/文`)),
		candidates: [][]rune{[]rune("件 a.txt"), []rune("件 b.txt")}, selected: -1,
	}}
	for _, want := range []string{`"folder/文件 a.txt"`, `"folder/文件 b.txt"`, `"folder/文件 a.txt"`} {
		p.Do(nil, 0)
		line, pos, ok := p.completionChanged(nil, 0, readline.CharTab)
		if !ok || string(line) != want || pos != len([]rune(want))-1 {
			t.Fatalf("selection: %q at %d, want %q", string(line), pos, want)
		}
	}
	if _, process := p.filterCompletionKey(readline.CharEnter); process {
		t.Fatal("selection submitted upload")
	}
	if key, _ := p.filterCompletionKey(readline.CharEnter); key != readline.CharEnter {
		t.Fatal("second Enter did not submit")
	}
}

func TestRedirectedLogsArePlain(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	Log(f, Error, "sshore: %s\n", "failed")
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sshore: failed\n" {
		t.Fatalf("%q", b)
	}
}

func TestLogUnicodeAndColor(t *testing.T) {
	text := "[sshore] 已保存 /tmp/中文文件.txt\n"
	if got := Format(false, Success, "%s", text); got != text {
		t.Fatalf("plain log: %q", got)
	}
	if got := Format(true, Success, "%s", text); got != "\x1b[32m"+text+"\x1b[39m" {
		t.Fatalf("colored log: %q", got)
	}
}
