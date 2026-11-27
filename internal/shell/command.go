package shell

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FormatCommand shows an argument vector as command text, preserving argument boundaries.
func FormatCommand(argv []string) string {
	words := make([]string, len(argv))
	for i, arg := range argv {
		words[i] = arg
		if arg == "" || strings.ContainsAny(arg, "\"'`") || strings.ContainsFunc(arg, func(r rune) bool {
			return unicode.IsSpace(r) || !unicode.IsPrint(r) || r == utf8.RuneError
		}) {
			words[i] = strconv.Quote(arg)
		}
	}
	return strings.Join(words, " ")
}

// ParseCommand splits literal arguments. Single quotes enclose literal text,
// and double quotes accept Go string escapes.
func ParseCommand(line string) ([]string, error) {
	var argv []string
	for line = strings.TrimSpace(line); line != ""; {
		end := strings.IndexFunc(line, unicode.IsSpace)
		if end < 0 {
			end = len(line)
		}
		arg := line[:end]
		switch line[0] {
		case '"':
			quoted, err := strconv.QuotedPrefix(line)
			if err != nil {
				return nil, errors.New("Close double quotes and use valid escapes inside them.")
			}
			end = len(quoted)
			arg, _ = strconv.Unquote(quoted)
		case '\'':
			value, _, ok := strings.Cut(line[1:], "'")
			if !ok {
				return nil, errors.New("Close single quotes.")
			}
			arg, end = value, len(value)+2
		default:
			if strings.ContainsRune(arg, '"') {
				return nil, errors.New("Put double quotes around the entire argument.")
			}
		}
		next := line[end:]
		line = strings.TrimLeftFunc(next, unicode.IsSpace)
		if line != "" && line == next {
			return nil, errors.New("Separate arguments with spaces.")
		}
		if strings.ContainsRune(arg, 0) || len(argv) == 0 && arg == "" {
			return nil, errors.New("Use a nonempty program name and arguments without NUL.")
		}
		argv = append(argv, arg)
	}
	return argv, nil
}

// QuotePOSIX quotes one argument for a POSIX shell.
func QuotePOSIX(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
