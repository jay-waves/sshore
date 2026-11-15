package termui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func ParsePath(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("path must be UTF-8")
	}
	s := strings.TrimSpace(string(b))
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return "", fmt.Errorf("empty path")
	}
	if s == "~" || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if s == "~" {
			s = home
		} else {
			s = filepath.Join(home, s[2:])
		}
	}
	return s, nil
}
