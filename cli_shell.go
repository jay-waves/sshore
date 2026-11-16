package main

import "strings"

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
	flags := []cliFlag{{"--local", false}, {"--server", true}}
	switch o.command {
	case "launch", "spawn", "attach":
		flags = append(flags, cliFlag{"--download-dir", true}, cliFlag{"--quiet", false}, cliFlag{"-q", false}, cliFlag{"--no-escape", false})
		if o.command != "attach" {
			flags = append(flags, cliFlag{"--detach", false})
		}
	case "list":
		flags = append(flags, cliFlag{"--json", false}, cliFlag{"--verbose", false})
	}
	parsed, err := parseCLI(args[1:], flags, true, o.command == "launch" || o.command == "spawn" || o.command == "attach")
	if err != nil {
		if strings.Contains(err.Error(), "unsupported option: --name") {
			return o, usage("use %s --local NAME to name a local shell, or %s HOST NAME", o.command, o.command)
		}
		return o, err
	}
	positional := parsed.operands
	o.ssh, o.local = parsed.ssh, parsed.has("--local")
	o.serverSet = parsed.has("--server")
	if o.serverSet {
		o.server = parsed.values["--server"]
	}
	o.downloadDir = parsed.values["--download-dir"]
	o.quiet, o.noEscape = parsed.has("--quiet") || parsed.has("-q"), parsed.has("--no-escape")
	if parsed.has("--quiet") && parsed.has("-q") {
		return o, usage("specify --quiet only once")
	}
	o.detached = parsed.has("--detach")
	o.listJSON, o.listDetails = parsed.has("--json"), parsed.has("--verbose")
	if (o.command == "launch" || o.command == "spawn") && len(positional) == 0 {
		o.local = true
	}
	if o.local {
		o.host = "local"
		if len(o.ssh) > 0 || o.serverSet || o.downloadDir != "" {
			return o, usage("local shells do not accept SSH or transfer options")
		}
	} else if len(positional) > 0 {
		o.host = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 && !o.list {
		o.session = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 && o.command == "rename" {
		o.newName = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 {
		return o, usage("unexpected argument: %s", positional[0])
	}

	if o.listJSON && o.listDetails {
		return o, usage("--json and --verbose are mutually exclusive")
	}
	if o.host == "" && o.list && len(o.ssh) == 0 && o.server == "sshore" {
		if o.quiet || o.noEscape || o.downloadDir != "" {
			return o, usage("list does not accept shell or transfer options")
		}
		return o, nil
	}
	if o.host != "" || o.command != "attach" {
		if err := validSSHDestination(o.host); err != nil {
			return o, err
		}
	}
	if o.server == "" || strings.ContainsAny(o.server, "\r\n\x00") {
		return o, usage("invalid server path")
	}
	if o.list && (o.session != "" || o.downloadDir != "" || o.quiet || o.noEscape) {
		return o, usage("list accepts SSH options and --server, not shell or transfer options")
	}
	if o.list {
		return o, nil
	}
	if o.command == "rename" {
		if o.session == "" || o.newName == "" || o.downloadDir != "" || o.quiet || o.noEscape {
			return o, usage("rename requires HOST OLD NEW and accepts only SSH options and --server")
		}
		if err := validSession(o.newName); err != nil {
			return o, err
		}
	}
	if o.command == "cancel" && (o.session == "" || o.downloadDir != "" || o.quiet || o.noEscape) {
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
	taskIdentity          string
	instance              string
	serverSet             bool
	sshDirectory          string
}
