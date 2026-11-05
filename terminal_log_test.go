package main

import (
	"os"
	"testing"
)

func TestLogFormatting(t *testing.T) {
	for _, level := range []logColor{logInfo, logSuccess, logWarning, logError} {
		plain := formatLog(false, level, "[sshore #%d] %s\r\n", 7, "done")
		if plain != "[sshore #7] done\r\n" {
			t.Fatal(plain)
		}
		colored := formatLog(true, level, "[sshore #%d] %s\r\n", 7, "done")
		if colored != "\x1b["+string(level)+"m"+plain+"\x1b[39m" {
			t.Fatal(colored)
		}
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
	logf(f, logError, "sshore: %s\n", "failed")
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sshore: failed\n" {
		t.Fatalf("%q", b)
	}
}
