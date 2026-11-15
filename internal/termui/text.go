package termui

import (
	"strings"
	"unicode"
)

func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\ufffd'
		}
		return r
	}, s)
}
