package sys

import (
	"os"
	"strings"
)

// MachineID reads the OS identifier and returns empty when unavailable.
func MachineID() string {
	for _, path := range []string{"/var/lib/dbus/machine-id", "/etc/machine-id"} {
		if data, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	return ""
}
