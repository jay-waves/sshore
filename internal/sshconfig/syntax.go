// Package sshconfig resolves the SSH client settings used by sshore.
package sshconfig

import (
	"fmt"
	"strings"
)

// Option is a configuration keyword and its unquoted operands.
type Option struct {
	Key    string
	Values []string
}

// ParseOption reads a single ssh_config line or -o argument. The empty option
// represents a comment or a blank line.
func ParseOption(line string) (Option, error) {
	line = strings.TrimLeft(line, " \t")
	if line == "" || line[0] == '#' {
		return Option{}, nil
	}
	if strings.ContainsAny(line, "\x00\r\n") {
		return Option{}, fmt.Errorf("unexpected control character")
	}
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return Option{}, fmt.Errorf("%s requires an argument", line)
	}
	key := strings.ToLower(line[:end])
	rest := strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	fields, err := splitWords(rest)
	if err != nil {
		return Option{}, fmt.Errorf("%s: %w", key, err)
	}
	if key == "" || len(fields) == 0 {
		return Option{}, fmt.Errorf("%s requires an argument", key)
	}
	return Option{Key: key, Values: fields}, nil
}

func splitWords(line string) ([]string, error) {
	var fields []string
	for len(line) != 0 && line[0] != '#' {
		var word strings.Builder
		var quote byte
		i := 0
		for ; i < len(line); i++ {
			c := line[i]
			if quote == 0 && (c == ' ' || c == '\t') {
				break
			}
			if c == quote {
				quote = 0
				continue
			}
			if quote == 0 && (c == '"' || c == '\'') {
				quote = c
				continue
			}
			if c == '\\' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\'') {
				i++
				c = line[i]
			}
			word.WriteByte(c)
		}
		if quote != 0 {
			return nil, fmt.Errorf("unterminated quoted argument")
		}
		fields = append(fields, word.String())
		line = strings.TrimLeft(line[i:], " \t")
	}
	return fields, nil
}

// wildcard implements the SSH pattern syntax. A slash is an ordinary
// character, so filesystem glob matching would give different Host results.
func wildcard(pattern, value string) bool {
	var suffix string
	retry := -1
	for offset := 0; offset < len(value); {
		switch {
		case len(pattern) != 0 && pattern[0] == '*':
			pattern = pattern[1:]
			suffix, retry = pattern, offset
		case len(pattern) != 0 && (pattern[0] == '?' || pattern[0] == value[offset]):
			pattern, offset = pattern[1:], offset+1
		case retry >= 0:
			retry++
			pattern, offset = suffix, retry
		default:
			return false
		}
	}
	return strings.Trim(pattern, "*") == ""
}

func matchPatterns(patterns []string, value string) bool {
	matched := false
	for _, pattern := range patterns {
		plain, negative := strings.CutPrefix(pattern, "!")
		if wildcard(plain, value) {
			if negative {
				return false
			}
			matched = true
		}
	}
	return matched
}
