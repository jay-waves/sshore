package files

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func defaultMountpoint(_ string) (string, func(), error) {
	drive, err := availableDrive(0)
	return drive, func() {}, err
}

func availableDrive(excluded uint32) (string, error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return "", err
	}
	for c := byte('Z'); c >= 'D'; c-- {
		if (drives|excluded)&(1<<uint(c-'A')) == 0 {
			return string(c) + ":", nil
		}
	}
	return "", fmt.Errorf("no free drive letter; specify a mountpoint")
}
