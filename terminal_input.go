package main

import (
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// ReadConsole returns UTF-16. Decode it without os.File.Read's Windows Ctrl-Z
// EOF conversion, preserving job-control keys and split surrogate pairs.
type consoleInput struct {
	readUnits func([]uint16) (int, error)
	bytes     []byte
	high      uint16
	end       error
}

func (r *consoleInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.bytes) == 0 {
		if r.end != nil {
			return 0, r.end
		}
		var units [2048]uint16
		n, err := r.readUnits(units[:])
		if err != nil {
			r.end = err
		} else if n == 0 {
			r.end = io.EOF
		}
		for _, u := range units[:n] {
			if r.high != 0 {
				if u >= 0xdc00 && u <= 0xdfff {
					r.bytes = utf8.AppendRune(r.bytes, utf16.DecodeRune(rune(r.high), rune(u)))
					r.high = 0
					continue
				}
				r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
				r.high = 0
			}
			if u >= 0xd800 && u <= 0xdbff {
				r.high = u
			} else if u >= 0xdc00 && u <= 0xdfff {
				r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
			} else {
				r.bytes = utf8.AppendRune(r.bytes, rune(u))
			}
		}
		if r.end != nil && r.high != 0 {
			r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
			r.high = 0
		}
	}
	n := copy(p, r.bytes)
	r.bytes = r.bytes[n:]
	return n, nil
}
