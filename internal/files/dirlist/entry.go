// Package dirlist describes file listings shared by local and remote browsers.
package dirlist

import (
	"io/fs"
	"strings"
)

type Entry struct {
	Name  string
	IsDir bool
}

func FromDirEntries(entries []fs.DirEntry, err error) ([]Entry, error) {
	if err != nil {
		return nil, err
	}
	list := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") {
			list = append(list, Entry{Name: name, IsDir: entry.IsDir()})
		}
	}
	return list, nil
}
