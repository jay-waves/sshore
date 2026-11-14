package main

// Shared transport-option parsing keeps mount and forward authentication and
// jump-host behavior aligned. Explicit forwarding flags belong to the resource,
// not this parser. Local OpenSSH configuration remains trusted policy.
import "strings"

func takeTransportOption(args []string, i *int) ([]string, bool, error) {
	a := args[*i]
	switch a {
	case "-C", "-v", "-vv", "-vvv", "-4", "-6":
		return []string{a}, true, nil
	}
	if len(a) >= 2 && a[0] == '-' && strings.ContainsRune("piJFloE", rune(a[1])) {
		if len(a) > 2 {
			return []string{a[:2], a[2:]}, true, nil
		}
		*i++
		if *i >= len(args) || (args[*i] == "" || args[*i] == "--") {
			return nil, true, usage("missing value for %s", a)
		}
		return []string{a, args[*i]}, true, nil
	}
	return nil, false, nil
}

// splitSSHOptions reserves -- for the SSH-only suffix. Operands and product
// flags must precede it; a suffix never becomes an SSH destination or command.
func splitSSHOptions(args []string, nativeForwarding bool) ([]string, []string, error) {
	for at, arg := range args {
		if arg != "--" {
			continue
		}
		suffix := args[at+1:]
		var ssh []string
		if len(suffix) == 0 {
			return nil, nil, usage("expected SSH options after --")
		}
		for i := 0; i < len(suffix); i++ {
			a := suffix[i]
			if nativeForwarding && len(a) >= 2 && a[0] == '-' && strings.ContainsRune("LRD", rune(a[1])) {
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
				continue
			}
			parsed, ok, err := takeTransportOption(suffix, &i)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				return nil, nil, usage("unsupported SSH option after --: %s; put sshore arguments before --", a)
			}
			ssh = append(ssh, parsed...)
		}
		return args[:at], ssh, nil
	}
	return args, nil, nil
}

func misplacedSSHOption(a string) error {
	if len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune("piJFloELRDCv46", rune(a[1])) {
		return usage("SSH option %s must follow -- (example: sshore new HOST -- -J jump)", a)
	}
	return nil
}
