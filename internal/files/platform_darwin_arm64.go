package files

import "errors"

var errMountUnsupported = errors.New("local mounts are unsupported on macOS")

func CheckMountPlatform() error                 { return errMountUnsupported }
func ValidateMountpoint(string) error           { return errMountUnsupported }
func mountFTP(*ftpFS, string) (ftpMount, error) { return nil, errMountUnsupported }
