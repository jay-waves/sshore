package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected launch, spawn, list, attach, cancel or rename; see sshore --help")
	}
	o.command = args[0]
	switch o.command {
	case "launch", "spawn", "attach", "cancel", "rename":
	case "list":
		o.list = true
	default:
		return o, usage("choose an action: sshore launch HOST [NAME], sshore spawn HOST [NAME], sshore list HOST, sshore attach HOST [NAME], sshore cancel HOST NAME, or sshore rename HOST OLD NEW")
	}
	parsed, err := parseCLI(args[1:], o.command)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported option: --name") {
			return o, usage("use %s --local NAME to name a local shell, or %s HOST NAME", o.command, o.command)
		}
		return o, err
	}
	return shellOptions(o.command, parsed)
}

func shellOptions(command string, parsed cliArguments) (options, error) {
	o := options{command: command, list: command == "list", server: parsed.value("--server", "sshore")}
	o.ssh, o.local = parsed.ssh, parsed.has("--local")
	o.serverSet = parsed.has("--server")
	o.downloadDir = parsed.values["--download-dir"]
	o.quiet, o.noEscape = parsed.has("--quiet"), parsed.has("--no-escape")
	o.detached = parsed.has("--detach")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	o.listJSON, o.listDetails = parsed.has("--json"), parsed.has("--verbose")
	if (o.command == "launch" || o.command == "spawn") && len(parsed.operands) == 0 {
		o.local = true
	}
	firstName := 1
	o.host = parsed.operand(0, "")
	if o.local {
		o.host, firstName = "local", 0
		if len(o.ssh) > 0 || o.serverSet || o.downloadDir != "" {
			return o, usage("local shells do not accept SSH or transfer options")
		}
	}
	if !o.list {
		o.session = parsed.operand(firstName, "")
	}
	if command == "rename" {
		o.newName = parsed.operand(firstName+1, "")
	}

	if o.host == "" && o.list && len(o.ssh) == 0 && o.server == "sshore" {
		return o, nil
	}
	if o.host != "" || o.command != "attach" {
		if err := validSSHDestination(o.host); err != nil {
			return o, err
		}
	}
	if o.list {
		return o, nil
	}
	if o.command == "rename" {
		if o.session == "" || o.newName == "" {
			return o, usage("rename requires HOST OLD NEW and accepts only SSH options and --server")
		}
		if err := validSession(o.newName); err != nil {
			return o, err
		}
	}
	if o.command == "cancel" && (o.session == "") {
		return o, usage("cancel requires HOST NAME and accepts only SSH options and --server")
	}
	if o.session == "" {
		return o, nil
	}
	return o, validSession(o.session)
}

type options struct {
	temporary             bool
	owner                 spawnOwner
	host, session, server string
	command               string
	newName               string
	downloadDir           string
	list, quiet, noEscape bool
	listJSON, listDetails bool
	ssh                   []string
	approved              []operation
	approvalID            string
	resumePID             int
	stay                  bool
	plain                 bool
	local                 bool
	detached              bool
	noLogin               bool
	noProfile             bool
	taskIdentity          string
	instance              string
	serverSet             bool
	sshDirectory          string
}

func executeShellCommand(o options) (int, error) {
	var err error
	if o.command == "spawn" {
		o.temporary = true
		o.owner, err = currentSpawnOwner()
		if err != nil {
			return 1, err
		}
	}
	if o.local {
		return runLocalCommand(o)
	}
	if err = resolveRecentAttach(&o); err != nil {
		return 1, err
	}
	o.downloadDir, err = prepareDownloadDir(o.downloadDir)
	if err != nil {
		return 1, err
	}
	if o.list {
		return runInventory(o)
	}
	return runClient(o)
}

func prepareDownloadDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("download destination is not a directory: %s", path)
	}
	return path, nil
}
