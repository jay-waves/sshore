package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFormatApprovalPrompt(t *testing.T) {
	const prompt = "Type update to END listed old sessions/tasks and install; Enter for a recovery shell: "
	if got := formatApprovalPrompt(false, prompt); got != prompt {
		t.Fatalf("uncolored prompt = %q", got)
	}
	got := formatApprovalPrompt(true, prompt)
	if runtime.GOOS == "windows" {
		if got != prompt || strings.Contains(got, "\x1b") {
			t.Fatalf("Windows readline must receive a plain prompt: %q", got)
		}
	} else if want := formatLog(true, logInfo, "%s", prompt); got != want {
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
	p := &approvalInput{}
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
		got, err := localPath([]byte(tc.input))
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}
