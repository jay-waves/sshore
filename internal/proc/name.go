package proc

// VerifiedName returns a best-effort display name for a live process with the
// given start token. An empty result means its name or identity was unavailable.
// Use the result only for display.
func VerifiedName(pid int, start string) string {
	if current, err := LiveIdentity(pid); err != nil || current != start {
		return ""
	}
	name, err := processName(pid)
	if err != nil {
		return ""
	}
	// Identity may change while reading the name.
	if current, err := LiveIdentity(pid); err != nil || current != start {
		return ""
	}
	return name
}
