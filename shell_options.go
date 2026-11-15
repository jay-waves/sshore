package main

import "strings"

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected launch, spawn, list, attach, cancel or rename; see sshore --help")
	}
	o.command = args[0]
	if o.command == "shell" {
		o.command = "spawn"
	}
	switch o.command {
	case "launch", "spawn", "attach", "cancel", "rename":
	case "list":
		o.list = true
	default:
		return o, usage("choose an action: sshore launch HOST [NAME], sshore spawn HOST [NAME], sshore list HOST, sshore attach HOST [NAME], sshore cancel HOST NAME, or sshore rename HOST OLD NEW")
	}
	var err error
	var positional []string
	named := false
	args, o.ssh, err = splitSSHOptions(args[1:], true)
	if err != nil {
		return o, err
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			return o, usage("empty argument")
		}
		if strings.HasPrefix(a, "--") {
			if key, value, ok := strings.Cut(a, "="); ok {
				switch key {
				case "--server", "--download-dir", "--name":
				default:
					return o, usage("%s does not accept a value", key)
				}
				args = append(append(append([]string{}, args[:i]...), key, value), args[i+1:]...)
				a = key
			}
		}
		if err := misplacedSSHOption(a); err != nil {
			return o, err
		}
		switch a {
		case "--server", "--download-dir", "--name":
			i++
			if i == len(args) || args[i] == "" {
				return o, usage("missing value for %s", a)
			}
			switch a {
			case "--server":
				o.server = args[i]
				o.serverSet = true
			case "--name":
				if (o.command != "launch" && o.command != "spawn") || named {
					return o, usage("--name is only supported once by launch or spawn")
				}
				named = true
				o.local = true
				o.session = args[i]
			case "--download-dir":
				o.downloadDir = args[i]
			}
		case "--json", "--verbose":
			if !o.list {
				return o, usage("%s is only supported by list", a)
			}
			if a == "--json" {
				o.listJSON = true
			} else {
				o.listDetails = true
			}
		case "--detach":
			if o.command != "launch" && o.command != "spawn" {
				return o, usage("--detach is only supported by launch or spawn")
			}
			o.detached = true
		case "--local":
			o.local = true
		case "-q", "--quiet":
			o.quiet = true
		case "--no-escape":
			o.noEscape = true
		case "-h", "--help":
			return o, errHelp

		default:
			if strings.HasPrefix(a, "-") {
				return o, usage("unexpected argument: %s", a)
			}
			positional = append(positional, a)
		}
	}
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
	if len(positional) > 0 && !o.list && !named {
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
	if named {
		if err := validSession(o.session); err != nil {
			return o, err
		}
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
