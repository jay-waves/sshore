package main

import (
	"errors"
	"os"

	"github.com/jay-waves/sshore/internal/termui"
)

// Overridden by release builds using -ldflags '-X main.version=...'.
var version = "0.7.1"

func main() {
	code := 1
	err := checkPlatform()
	if err == nil {
		code, err = run(os.Args[1:])
	}
	if err != nil {
		termui.Log(os.Stderr, termui.Error, "sshore: %s\n", cleanText(err.Error()))
		var e *usageError
		if errors.As(err, &e) {
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
