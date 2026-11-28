package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/jay-waves/sshore/internal/release"
	"github.com/jay-waves/sshore/internal/shell"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/pkg/sftp/v2"
)

const ClientDirectory = ".sshore/clients"
const ResetMarker = ".reset"

var ErrMissing = errors.New("remote deployment is absent")

func openDeploymentFiles(ctx context.Context, client *sshio.Client) (_ *sftp.Client, _ func(), result error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if result != nil {
			_ = session.Close()
		}
	}()
	in, err := session.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := session.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		return nil, nil, fmt.Errorf("open SFTP for remote deployment: %w", err)
	}
	ftp, err := sftp.NewClientPipe(ctx, out, in)
	if err != nil {
		return nil, nil, err
	}
	return ftp, func() {
		_ = ftp.Close()
		_ = session.Close()
	}, nil
}

func Prepare(ctx context.Context, client *sshio.Client, id, version, releaseBase string, install bool,
	progress release.AssetProgress, track func(int64, io.Reader, func(io.Reader) error) error) (home string, uploaded bool, err error) {
	ftp, closeFiles, err := openDeploymentFiles(ctx, client)
	if err != nil {
		return "", false, err
	}
	defer closeFiles()
	home, err = ftp.RealPath(".")
	if err != nil {
		return "", false, err
	}
	platform, err := remoteDeploymentPlatform(home)
	if err != nil {
		return "", false, err
	}
	relative := platform.binaryPath(id, version)
	destination := path.Join(home, relative)
	exists, err := deploymentExists(ftp, destination)
	if err != nil {
		return "", false, err
	}
	if !exists {
		if !install {
			return "", false, ErrMissing
		}
		dir := home
		for part := range strings.SplitSeq(path.Dir(relative), "/") {
			dir = path.Join(dir, part)
			if err := ensureDeploymentDirectory(ftp, dir, platform.os == "windows"); err != nil {
				return "", false, err
			}
		}
		marker := path.Join(path.Dir(destination), ResetMarker)
		if err := ftp.RemoveFile(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		source, size, err := release.OpenBinary(ctx, releaseBase, version, platform.os, platform.binary, progress)
		if err != nil {
			return "", false, err
		}
		defer source.Close()
		if err := track(size, source, func(reader io.Reader) error {
			return uploadDeployment(ftp, reader, destination, platform.os == "windows")
		}); err != nil {
			return "", false, err
		}
		return home, true, nil
	}
	return home, false, nil
}

type deploymentPlatform struct {
	os, binary string
}

func remoteDeploymentPlatform(home string) (deploymentPlatform, error) {
	drive := strings.TrimPrefix(home, "/")
	if len(drive) >= 3 && drive[1:3] == ":/" {
		return deploymentPlatform{"windows", "sshore.exe"}, nil
	}
	if !strings.HasPrefix(home, "/") {
		return deploymentPlatform{}, fmt.Errorf("unsupported SSH home path %q", home)
	}
	return deploymentPlatform{"linux", "sshore"}, nil
}

func (p deploymentPlatform) binaryPath(id, version string) string {
	return path.Join(ClientDirectory, id, version, p.binary)
}

func Command(home, id, version, command string) (string, error) {
	p, err := remoteDeploymentPlatform(home)
	if err != nil {
		return "", err
	}
	binary := p.binaryPath(id, version)
	if p.os == "windows" {
		binary = `.\` + strings.ReplaceAll(binary, "/", `\`)
	} else {
		binary = shell.QuotePOSIX(path.Join(home, binary))
	}
	return binary + " " + command, nil
}

func ensureDeploymentDirectory(ftp *sftp.Client, dir string, windows bool) error {
	info, err := ftp.LStat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// Another first connection may create the same directory.
		createErr := ftp.Mkdir(dir, 0700)
		info, err = ftp.LStat(dir)
		if err != nil && createErr != nil {
			return createErr
		}
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || !windows && info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("deployment directory is not private: %s", dir)
	}
	return nil
}

func deploymentExists(ftp *sftp.Client, name string) (bool, error) {
	info, err := ftp.LStat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("deployment target is not a regular file: %s", name)
	}
	return true, nil
}

func uploadDeployment(ftp *sftp.Client, source io.Reader, destination string, windows bool) error {
	var nonce [32]byte
	rand.Read(nonce[:])
	temporary := destination + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := ftp.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	defer ftp.Remove(temporary)
	_, copyErr := io.Copy(f, source)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return err
	}
	if !windows {
		if err := ftp.Chmod(temporary, 0700); err != nil {
			return err
		}
	}
	if err := ftp.Rename(temporary, destination); err != nil {
		if exists, checkErr := deploymentExists(ftp, destination); checkErr != nil || !exists {
			return err
		}
	}
	return nil
}

func Remove(ctx context.Context, client *sshio.Client, id, version string) error {
	ftp, closeFiles, err := openDeploymentFiles(ctx, client)
	if err != nil {
		return err
	}
	defer closeFiles()
	home, err := ftp.RealPath(".")
	if err != nil {
		return err
	}
	platform, err := remoteDeploymentPlatform(home)
	if err != nil {
		return err
	}
	target := path.Join(home, platform.binaryPath(id, version))
	if err := ftp.RemoveFile(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove remote server: %w", err)
	}
	return nil
}
