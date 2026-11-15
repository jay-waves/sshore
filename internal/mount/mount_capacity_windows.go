//go:build windows

package mount

import "github.com/winfsp/go-winfsp"

func (a *ftpWindowsAttributes) GetVolumeInfo(ref *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	s, err := ftpRemoteCapacity(a.backend.client, a.backend.root)
	if err != nil {
		return ftpWindowsError(err)
	}
	if err := a.ftpWindowsBehaviour.GetVolumeInfo(ref, info); err != nil {
		return err
	}
	info.TotalSize = s.Blocks * s.Frsize
	info.FreeSize = s.Bavail * s.Frsize
	return nil
}

func (a *ftpWindowsAttributes) SetVolumeLabel(ref *winfsp.FileSystemRef, label string, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	if err := a.ftpWindowsBehaviour.SetVolumeLabel(ref, label, info); err != nil {
		return err
	}
	return a.GetVolumeInfo(ref, info)
}
