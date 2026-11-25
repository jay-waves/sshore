package main

import "errors"

func systemMachineID() string { return "" }
func checkPlatform() error    { return nil }
func runLocalInstaller(string) error {
	return errors.New("install the experimental macOS client manually")
}
func reportBootstrapCgroup() {}

func daemonTaskCgroup() (string, error) { return "", nil }

func serveRemoteRequest(remoteServiceRequest) error {
	return errors.New("remote services run on the Linux server")
}
