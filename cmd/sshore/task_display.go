package main

import (
	"cmp"
	"os"
	"strings"
)

const localInventoryHost = "\x00local"
const originLabel = "origin"
const originInventoryHost = "\x00" + originLabel

func terminalTaskKind(program bool) string {
	if program {
		return "run"
	}
	return "shell"
}

func taskStateCode(state string) string {
	switch state {
	case taskActive:
		return "R"
	case taskWaiting:
		return "S"
	case taskDetached:
		return "T"
	case taskUnknown:
		return "?"
	default:
		return state
	}
}

func peerTaskIdentity(p peerView) string {
	return "peer-task:" + p.UID
}

func peerKind(name string) string {
	kind, _, _ := strings.Cut(name, "/")
	return kind
}

func taskDisplayID(code, alias string, current bool) string {
	code = cleanText(code)
	if code == "" {
		code = "-"
	}
	suffix := ""
	if alias != "" && alias != code {
		suffix = " (" + cleanText(alias) + ")"
	}
	if current {
		code += "*"
	}
	return code + suffix
}

func hostDisplayLabel(host string) string {
	if host == localInventoryHost {
		return "local"
	}
	if host == originInventoryHost {
		return originLabel
	}
	return cleanText(host)
}

func compareInventoryHosts(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == localInventoryHost:
		return -1
	case b == localInventoryHost:
		return 1
	default:
		return cmp.Compare(a, b)
	}
}

func shellIdentityFromEnvironment() string {
	if uid := os.Getenv("SSHORE_SOCKET"); uid != "" {
		return taskKey(uid)
	}
	return os.Getenv("SSHORE_LOCAL_SESSION")
}
