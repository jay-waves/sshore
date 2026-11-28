package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jay-waves/sshore"
	"github.com/jay-waves/sshore/internal/release"
)

func runInstall(parsed cliArguments) (int, error) {
	var target string
	if len(parsed.operands) != 0 {
		ref := parsed.operands[0]
		host, path, err := splitEndpoint(ref)
		if err != nil {
			return 2, err
		}
		if host != "here" || strings.HasPrefix(ref, ":") {
			return 2, usage("install applies to this machine; use .: or .:PATH")
		}
		target = path
	}
	if target != "" && (!filepath.IsAbs(target) || strings.ContainsAny(target, "\x00\r\n")) {
		return 2, usage("installation path must be absolute and contain no line breaks")
	}
	apply := parsed.has("--apply")
	if apply && target == "" {
		return 2, usage("applying a prepared release requires .:PATH")
	}
	if err := requireLocalMaintenance("install"); err != nil {
		return 1, err
	}
	if apply {
		return 0, installRelease(target)
	}
	return 0, release.RunInstaller(target, sshore.InstallerScripts)
}

func installRelease(target string) error {
	startup, err := daemonFileLock("startup")
	if err != nil {
		return err
	}
	defer startup.Close()

	replacement, err := release.PrepareReplacement(target, version)
	if err != nil {
		return err
	}
	defer replacement.Discard()
	installed := replacement.InstalledVersion

	record, err := existingDaemonRecord()
	running := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if running && record.Version != installed {
		return fmt.Errorf("stop the local daemon using its sshore %s client before installing", cleanText(record.Version))
	}
	if installed != "" && installed != version {
		if err := stopInstalledDeployments(target, installed); err != nil {
			return err
		}
	}
	if running {
		command := exec.Command(target, "kill", "--all")
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("stopping the local sshore %s daemon: %w", installed, err)
		}
	}
	// A direct daemon invocation can race even while we hold the startup lock.
	owner, err := daemonFileLock("owner")
	if err != nil {
		return fmt.Errorf("cannot take ownership for installation: %w", err)
	}
	defer owner.Close()
	return replacement.Publish()
}
