package sshconfig

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type optionKind uint8

const (
	scalar optionKind = iota
	boolean
	number
	seconds
	list
	repeated
	command
)

var options = map[string]optionKind{
	"hostname": scalar, "user": scalar, "port": number, "addressfamily": scalar,
	"bindaddress": scalar, "bindinterface": scalar, "connecttimeout": seconds,
	"serveraliveinterval": seconds, "serveralivecountmax": number,
	"proxyjump": scalar, "proxycommand": command, "proxyusefdpass": boolean,
	"identityfile": repeated, "certificatefile": repeated, "identitiesonly": boolean,
	"identityagent": scalar, "addkeystoagent": scalar, "pkcs11provider": scalar,
	"pubkeyauthentication": boolean, "passwordauthentication": boolean,
	"kbdinteractiveauthentication": boolean, "numberofpasswordprompts": number,
	"preferredauthentications": scalar, "batchmode": boolean,
	"stricthostkeychecking": scalar, "hostkeyalias": scalar, "hashknownhosts": boolean,
	"userknownhostsfile": list, "globalknownhostsfile": list, "knownhostscommand": command,
	"revokedhostkeys": scalar, "verifyhostkeydns": boolean,
	"ciphers": scalar, "kexalgorithms": scalar, "macs": scalar, "hostkeyalgorithms": scalar,
	"pubkeyacceptedalgorithms": scalar, "requiredrsasize": number, "compression": boolean,
	"gssapiauthentication": boolean, "gssapikeyexchange": boolean, "gssapidelegatecredentials": boolean,
	"hostbasedauthentication": boolean, "canonicalizehostname": scalar,
	"gssapikexalgorithms": scalar, "hostbasedacceptedalgorithms": scalar,
	"casignaturealgorithms": scalar, "canonicaldomains": list, "canonicalizefallbacklocal": boolean,
	"canonicalizemaxdots": number, "canonicalizepermittedcnames": scalar,
	"forwardagent": scalar, "forwardx11": boolean, "forwardx11trusted": boolean,
	"forwardx11timeout": seconds, "permitlocalcommand": boolean, "localcommand": command,
	"permitremoteopen": list, "tunnel": scalar, "remotecommand": command,
	"loglevel": scalar, "ignoreunknown": scalar,

	// Common settings shared with OpenSSH. sshore owns session channels,
	// environment and forwarding instead of using the configured CLI behavior.
	"sendenv": list, "setenv": list,
	"requesttty": scalar, "sessiontype": scalar, "escapechar": scalar,
	"controlmaster": scalar, "controlpath": scalar, "controlpersist": scalar,
	"clearallforwardings": boolean, "exitonforwardfailure": boolean,
	"localforward": list, "remoteforward": list, "dynamicforward": scalar,
	"checkhostip": boolean, "updatehostkeys": scalar, "tcpkeepalive": boolean,
	"nohostauthenticationforlocalhost": boolean, "ipqos": list,
	"rekeylimit": list, "connectionattempts": number, "visualhostkey": boolean,
}

func optionKey(key string) string {
	switch key {
	case "challengeresponseauthentication":
		return "kbdinteractiveauthentication"
	case "pubkeyacceptedkeytypes":
		return "pubkeyacceptedalgorithms"
	case "hostbasedkeytypes":
		return "hostbasedacceptedalgorithms"
	default:
		return key
	}
}

func normalize(key string, kind optionKind, values []string) ([]string, error) {
	value := values[0]
	switch kind {
	case boolean:
		switch strings.ToLower(value) {
		case "yes", "true":
			return []string{"yes"}, nil
		case "no", "false":
			return []string{"no"}, nil
		default:
			return nil, fmt.Errorf("expected yes or no")
		}
	case number, seconds:
		if kind == seconds && key == "connecttimeout" && strings.EqualFold(value, "none") {
			return []string{"none"}, nil
		}
		n, err := strconv.ParseUint(value, 10, 31)
		if err != nil && kind == seconds {
			parsed, durationErr := time.ParseDuration(strings.ToLower(value))
			if durationErr == nil && parsed >= 0 && parsed%time.Second == 0 && parsed/time.Second <= 1<<31-1 {
				n, err = uint64(parsed/time.Second), nil
			}
		}
		if err != nil || key == "port" && (n == 0 || n > 65535) {
			return nil, fmt.Errorf("invalid numeric value %q", value)
		}
		if key == "requiredrsasize" && n < 1024 {
			return nil, fmt.Errorf("must be at least 1024 bits")
		}
		return []string{strconv.FormatUint(n, 10)}, nil
	}

	switch key {
	case "ignoreunknown":
		patterns := strings.Split(strings.ToLower(value), ",")
		if slices.Contains(patterns, "") {
			return nil, fmt.Errorf("empty pattern")
		}
		return patterns, nil
	case "addressfamily":
		value = strings.ToLower(value)
		if value != "any" && value != "inet" && value != "inet6" {
			return nil, fmt.Errorf("expected any, inet, or inet6")
		}
		return []string{value}, nil
	case "stricthostkeychecking":
		switch strings.ToLower(value) {
		case "yes", "true":
			return []string{"yes"}, nil
		case "no", "false", "off":
			return []string{"no"}, nil
		case "ask", "accept-new":
			return []string{strings.ToLower(value)}, nil
		default:
			return nil, fmt.Errorf("invalid host-key policy %q", value)
		}
	case "forwardagent", "tunnel", "canonicalizehostname":
		if strings.EqualFold(value, "true") {
			return []string{"yes"}, nil
		}
		if strings.EqualFold(value, "false") {
			return []string{"no"}, nil
		}
		if key != "forwardagent" || strings.EqualFold(value, "yes") || strings.EqualFold(value, "no") {
			return []string{strings.ToLower(value)}, nil
		}
	case "loglevel":
		upper := strings.ToUpper(value)
		switch upper {
		case "QUIET", "FATAL", "ERROR", "INFO", "VERBOSE", "DEBUG", "DEBUG1", "DEBUG2", "DEBUG3":
			return []string{upper}, nil
		default:
			return nil, fmt.Errorf("invalid log level %q", value)
		}
	}
	return values, nil
}
