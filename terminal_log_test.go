package main

import (
	"os"
	"testing"
)

func TestRedirectedLogsArePlain(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	logf(f, logError, "sshore: %s\n", "failed")
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
	if got := formatLog(false, logSuccess, "%s", text); got != text {
		t.Fatalf("plain log: %q", got)
	}
	if got := formatLog(true, logSuccess, "%s", text); got != "\x1b[32m"+text+"\x1b[39m" {
		t.Fatalf("colored log: %q", got)
	}
	if got := cleanText("中文\x1b[31m\r\n"); got != "中文�[31m��" {
		t.Fatalf("unsafe terminal text: %q", got)
	}
}
