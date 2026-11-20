package main

import (
	"errors"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

// Overridden by release builds using -ldflags '-X main.version=...'.
var version = "0.7.25"

func main() {
	code := 1
	err := checkPlatform()
	if err == nil {
		code, err = run(os.Args[1:])
	}
	if err != nil {
		for line := range strings.SplitSeq(err.Error(), "\n") {
			termui.Log(os.Stderr, termui.Error, "sshore: %s\n", cleanText(line))
		}
		if _, ok := errors.AsType[*usageError](err); ok {
			code = 2
			termui.Log(os.Stderr, termui.Info, "Try 'sshore --help'.\n")
		} else if code == 0 {
			code = 1
		}
	}
	if code < 0 || code > 255 {
		code = 1
	}
	os.Exit(code)
}
