package main

import (
	"slices"
	"strings"
)

// -- separates product operands from normalized OpenSSH option/value pairs.
func splitSSHOptions(args []string, nativeForwarding bool) ([]string, []string, error) {
	at := slices.Index(args, "--")
	if at < 0 {
		return args, nil, nil
	}
	suffix := args[at+1:]
	if len(suffix) == 0 {
		return nil, nil, usage("expected SSH options after --")
	}
	valueFlags := "piJFloE"
	if nativeForwarding {
		valueFlags += "LRD"
	}
	var ssh []string
	for i := 0; i < len(suffix); i++ {
		a := suffix[i]
		switch a {
		case "-C", "-v", "-vv", "-vvv", "-4", "-6":
			ssh = append(ssh, a)
			continue
		}
		if len(a) < 2 || a[0] != '-' || !strings.ContainsRune(valueFlags, rune(a[1])) {
			return nil, nil, usage("unsupported SSH option after --: %s; put sshore arguments before --", a)
		}
		value := a[2:]
		if value == "" {
			i++
			if i == len(suffix) {
				return nil, nil, usage("missing value for %s", a)
			}
			value = suffix[i]
		}
		if value == "" || value == "--" {
			return nil, nil, usage("missing value for %s", a[:2])
		}
		ssh = append(ssh, a[:2], value)
	}
	return args[:at], ssh, nil
}

func misplacedSSHOption(a string) error {
	if len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune("piJFloELRDCv46", rune(a[1])) {
		return usage("SSH option %s must follow -- (example: sshore launch HOST -- -J jump)", a)
	}
	return nil
}
