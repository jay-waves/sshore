package files

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func defaultMountpoint(_ string) (string, func(), error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return "", nil, err
	}
	for c := byte('Z'); c >= 'D'; c-- {
		if drives&(1<<uint(c-'A')) == 0 {
			return string(c) + ":", func() {}, nil
		}
	}
	return "", nil, fmt.Errorf("no free drive letter; specify a mountpoint")
}
