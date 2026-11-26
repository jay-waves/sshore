package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// The terminal owns a kill-on-close job; Terminal.Close ends its process tree.
func reportBootstrapCgroup() {}

func daemonTaskCgroup() (string, error) { return "", nil }

// Private validates the path type and applies a protected current-user DACL.
func privateStatePath(path string, directory bool) error {
	native, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(native, `\\?\`) && !strings.HasPrefix(native, `\\.\`) {
		if strings.HasPrefix(native, `\\`) {
			native = `\\?\UNC\` + native[2:]
		} else {
			native = `\\?\` + native
		}
	}
	name, err := windows.UTF16PtrFromString(native)
	if err != nil {
		return err
	}
	// Keep validation and ACL mutation on one object; never follow a reparse point.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "secure", Path: path, Err: err}
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return fmt.Errorf("invalid state path: %s", path)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	current, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	// Reapplying an inheritable directory DACL walks its children. Those native
	// security handles can prevent a concurrent atomic metadata replacement.
	// The auto-inherited bit does not change this protected, explicit ACL.
	want, got := sd.String(), current.String()
	if got == want || got == strings.Replace(want, "D:P", "D:PAI", 1) {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("secure state ACL: %w", err)
	}
	return nil
}

// checkPlatform uses the real kernel version, independent of manifest shims.
func checkPlatform() error {
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 || (version.MajorVersion == 10 && version.BuildNumber < 22000) {
		return fmt.Errorf("Windows 11 / Server 2025 or newer required")
	}
	return nil
}
