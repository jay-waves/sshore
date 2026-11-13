package main

// Session age comes from the daemon's monotonic shell lifetime, not the client's
// clock or socket mtime. Missing ages (unstarted sessions) have no suffix.
import "fmt"

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
