package main

import (
	"errors"

	"github.com/jay-waves/sshore/internal/files"
)

func systemMachineID() string { return "" }
func checkPlatform() error    { return nil }
func runLocalInstaller(string) error {
	return errors.New("install the experimental macOS client manually")
}
func prepareFTPPaths(*ftpOptions) (func(), error) { return nil, files.CheckMountPlatform() }
func stopSpawnTerminal(int, string) error         { return errors.New("local shells are unsupported on macOS") }
