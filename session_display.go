package main

import (
	"fmt"
	"strings"
)

// Session age comes from the daemon lifetime, not the client clock or socket mtime.
// Missing ages (unstarted sessions) have no suffix.
func sessionAge(seconds *int64) string {
	if seconds == nil {
		return ""
	}
	n := max(*seconds, 0)
	for _, unit := range []struct {
		seconds int64
		label   string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if n >= unit.seconds || unit.seconds == 1 {
			return fmt.Sprintf(" · started %d%s ago", n/unit.seconds, unit.label)
		}
	}
	return ""
}

// Display only the trailing two components of deep directories. Home is shown
// as ~, and a rune limit bounds terminal output without splitting UTF-8.
func shortDirectory(path, home string) string {
	if path == "" {
		return "—"
	}
	path = cleanText(path)
	home = strings.TrimSuffix(cleanText(home), "/")
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 3 || (len(parts) > 2 && parts[0] != "~") {
		path = "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	r := []rune(path)
	if len(r) > 40 {
		path = "…" + string(r[len(r)-39:])
	}
	return path
}
