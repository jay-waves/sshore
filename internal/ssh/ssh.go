package ssh

import "strings"

// Retain transport/authentication settings without replaying unrelated forwards.
func WithoutForwarding(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-L" || args[i] == "-R" || args[i] == "-D" {
			i++
			continue
		}
		if args[i] == "-o" && i+1 < len(args) {
			fields := strings.FieldsFunc(args[i+1], func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
			key := ""
			if len(fields) > 0 {
				key = strings.ToLower(fields[0])
			}
			if key == "localforward" || key == "remoteforward" || key == "dynamicforward" {
				i++
				continue
			}
		}
		out = append(out, args[i])
	}
	return out
}
