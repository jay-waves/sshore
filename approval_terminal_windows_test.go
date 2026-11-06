package main

import (
	"errors"
	"testing"

	"github.com/chzyer/readline"
	"golang.org/x/sys/windows"
)

func TestApprovalConsoleRestore(t *testing.T) {
	const original = windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_EXTENDED_FLAGS
	mode := uint32(original)
	var failure error
	config := &readline.Config{}
	configureApprovalConsole(config,
		func(saved *uint32) error { *saved = mode; return nil },
		func(next uint32) error {
			if failure != nil {
				return failure
			}
			mode = next
			return nil
		})
	for i := 0; i < 2; i++ {
		if err := config.FuncMakeRaw(); err != nil {
			t.Fatal(err)
		}
		if err := config.FuncMakeRaw(); err != nil || mode != windows.ENABLE_EXTENDED_FLAGS {
			t.Fatalf("repeated entry: mode %x, error %v", mode, err)
		}
		failure = errors.New("restore failed")
		if err := config.FuncExitRaw(); !errors.Is(err, failure) {
			t.Fatalf("lost restore failure: %v", err)
		}
		failure = nil
		if err := config.FuncExitRaw(); err != nil || mode != original {
			t.Fatalf("restore: mode %x, error %v", mode, err)
		}
		if err := config.FuncExitRaw(); err != nil {
			t.Fatalf("close after successful restore: %v", err)
		}
	}
}
