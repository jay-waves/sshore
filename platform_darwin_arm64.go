package main

import "errors"

func systemMachineID() string { return "" }
func checkPlatform() error    { return nil }
func runLocalInstaller(string) error {
	return errors.New("install the experimental macOS client manually")
}
func stopSpawnTerminal(int, string) error { return errors.New("hosted shells require Linux") }
