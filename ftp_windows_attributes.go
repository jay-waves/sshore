//go:build windows

package main

// gofs supplies handle sharing, directory buffers and native rename bookkeeping,
// but its default SetBasicInfo rejects every request. This wrapper adds SFTP mtime,
// atime and read-only mode updates. DOS archive/hidden/system flags and creation/
// change times are not stored by SFTP v3; they remain synthetic, as with many
// network mounts. Path tracking is serialized with local rename/close callbacks;
// concurrent remote changes remain subject to ordinary SFTP path semantics.

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"
)

type ftpWindowsBehaviour interface {
	winfsp.BehaviourBase
	winfsp.BehaviourGetSecurityByName
	winfsp.BehaviourCreate
	winfsp.BehaviourOverwrite
	winfsp.BehaviourReadDirectory
	winfsp.BehaviourGetFileInfo
	winfsp.BehaviourGetSecurity
	winfsp.BehaviourGetVolumeInfo
	winfsp.BehaviourSetVolumeLabel
	winfsp.BehaviourSetFileSize
	winfsp.BehaviourRead
	winfsp.BehaviourWrite
	winfsp.BehaviourFlush
	winfsp.BehaviourCanDelete
	winfsp.BehaviourCleanup
	winfsp.BehaviourRename
	winfsp.BehaviourDefaultOptions
}

type ftpWindowsAttributes struct {
	ftpWindowsBehaviour
	backend *ftpFS
	mu      sync.Mutex
	names   map[uintptr]string
}

func (a *ftpWindowsAttributes) Open(ref *winfsp.FileSystemRef, name string, opts, access uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, e := a.ftpWindowsBehaviour.Open(ref, name, opts, access, info)
	if e == nil {
		a.names[h] = name
	}
	return h, e
}
func (a *ftpWindowsAttributes) Create(ref *winfsp.FileSystemRef, name string, opts, access, attrs uint32, sd *windows.SECURITY_DESCRIPTOR, size uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, e := a.ftpWindowsBehaviour.Create(ref, name, opts, access, attrs, sd, size, info)
	if e == nil {
		a.names[h] = name
	}
	return h, e
}
func (a *ftpWindowsAttributes) Close(ref *winfsp.FileSystemRef, h uintptr) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.names, h)
	a.ftpWindowsBehaviour.Close(ref, h)
}
func (a *ftpWindowsAttributes) Rename(ref *winfsp.FileSystemRef, h uintptr, old, new string, replace bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	source, ok := a.names[h]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}
	if e := a.ftpWindowsBehaviour.Rename(ref, h, old, new, replace); e != nil {
		return e
	}
	// Handles to an overwritten target must not apply path-based attributes to
	// the replacement. Their existing data handles remain under gofs ownership.
	if source != new {
		for handle, name := range a.names {
			if name == new {
				delete(a.names, handle)
			}
		}
	}
	for handle, name := range a.names {
		if name == source {
			a.names[handle] = new
		} else if strings.HasPrefix(name, source+`\`) {
			a.names[handle] = new + strings.TrimPrefix(name, source)
		}
	}
	return nil
}
func ftpWindowsTime(v uint64) time.Time {
	t := windows.Filetime{LowDateTime: uint32(v), HighDateTime: uint32(v >> 32)}
	return time.Unix(0, t.Nanoseconds())
}
func (a *ftpWindowsAttributes) SetBasicInfo(ref *winfsp.FileSystemRef, h uintptr, flags winfsp.SetBasicInfoFlags, attrs uint32, creation, access, write, change uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.backend.writable(); e != nil {
		return ftpWindowsError(e)
	}
	name, ok := a.names[h]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}
	// Verify the underlying gofs handle before using its tracked pathname.
	if e := a.GetFileInfo(ref, h, info); e != nil {
		return e
	}
	name, e := ftpWindowsPath(name)
	if e != nil {
		return e
	}
	p, e := a.backend.remotePath(name)
	if e != nil {
		return ftpWindowsError(e)
	}
	stat, e := a.backend.client.Stat(p)
	if e != nil {
		return ftpWindowsError(e)
	}
	if flags&winfsp.SetBasicInfoAttributes != 0 && !stat.IsDir() {
		mode := stat.Mode().Perm()
		if attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			mode &^= 0222
		} else {
			mode |= 0200
		}
		if mode != stat.Mode().Perm() {
			if e = a.backend.client.Chmod(p, os.FileMode(mode)); e != nil {
				return ftpWindowsError(e)
			}
		}
	}
	if flags&(winfsp.SetBasicInfoLastAccessTime|winfsp.SetBasicInfoLastWriteTime) != 0 {
		at, mt := stat.ModTime(), stat.ModTime()
		if raw, ok := stat.Sys().(*sftp.FileStat); ok {
			at = time.Unix(int64(raw.Atime), 0)
		}
		if flags&winfsp.SetBasicInfoLastAccessTime != 0 {
			at = ftpWindowsTime(access)
		}
		if flags&winfsp.SetBasicInfoLastWriteTime != 0 {
			mt = ftpWindowsTime(write)
		}
		if e = a.backend.client.Chtimes(p, at, mt); e != nil {
			return ftpWindowsError(e)
		}
	}
	return a.GetFileInfo(ref, h, info)
}
