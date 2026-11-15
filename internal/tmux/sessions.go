package tmux

import (
	"slices"
	"sort"
	"strconv"
)

// SessionOrder is stable, includes current, and never changes caller-owned data.
func SessionOrder(current string, names []string) []string {
	all := append([]string{current}, names...)
	sort.Strings(all)
	return slices.Compact(all)
}
func AdjacentSession(current string, names []string, step int) string {
	all := SessionOrder(current, names)
	i := sort.SearchStrings(all, current)
	return all[(i+step%len(all)+len(all))%len(all)]
}

type SessionChooser struct {
	Names  []string
	Number string
}

// Feed consumes modal input only; trailing bytes after selection are discarded.
func (c *SessionChooser) Feed(data []byte) (selected string, done bool) {
	for _, b := range data {
		switch {
		case b == 27 || b == 3:
			return "", true
		case b == 13 || b == 10:
			if c.Number == "" {
				return "", true
			}
			n, err := strconv.Atoi(c.Number)
			if err == nil && n > 0 && n <= len(c.Names) {
				return c.Names[n-1], true
			}
			c.Number = ""
		case b == 127 || b == 8:
			if len(c.Number) > 0 {
				c.Number = c.Number[:len(c.Number)-1]
			}
		case b >= '0' && b <= '9':
			if len(c.Number) < 3 {
				c.Number += string(b)
			}
		}
	}
	return "", false
}
