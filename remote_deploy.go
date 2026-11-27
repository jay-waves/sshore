package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/pkg/sftp/v2"
)

const deploymentDirectory = ".sshore/clients"
const maxDeploymentAsset = 64 << 20

var deploymentReleaseBase = "https://github.com/jay-waves/sshore/releases/download"

var deploymentHTTP = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || req.URL.Scheme != via[0].URL.Scheme {
		return fmt.Errorf("invalid release download redirect")
	}
	return nil
}}

var errRemoteDeploymentMissing = errors.New("remote deployment is absent")

// The binary's startup location selects state for service, daemon and CLI.
var deploymentCoordinates = sync.OnceValues(func() (directory, clientID string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	dir := filepath.Dir(exe)
	parent := filepath.Dir(dir)
	id := filepath.Base(parent)
	if filepath.Base(dir) != version || !validMachineID(id) {
		return "", ""
	}
	account, err := user.Current()
	if err != nil {
		return "", ""
	}
	root := filepath.Join(account.HomeDir, filepath.FromSlash(deploymentDirectory))
	if !sameDeploymentRoot(filepath.Dir(parent), root) {
		home, err := filepath.EvalSymlinks(account.HomeDir)
		if err != nil || !sameDeploymentRoot(filepath.Dir(parent), filepath.Join(home, filepath.FromSlash(deploymentDirectory))) {
			return "", ""
		}
	}
	return dir, id
})

func sameDeploymentRoot(path, root string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(path, root)
	}
	return path == root
}

func deployedStateRoot() string {
	if dir, _ := deploymentCoordinates(); dir != "" {
		return filepath.Join(dir, "state")
	}
	return ""
}

func deploymentClientID() (string, error) {
	if _, id := deploymentCoordinates(); id != "" {
		return id, nil
	}
	daemon, err := daemonLocation()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(daemon)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := privateStatePath(root, true); err != nil {
		return "", err
	}
	lock, err := lockStateFile(filepath.Join(root, "deployment-id.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	name := filepath.Join(root, "deployment-id")
	id, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		value := resourceToken()[:32]
		return value, writeStateFile(name, []byte(value))
	}
	if err != nil {
		return "", err
	}
	if !validMachineID(string(id)) {
		return "", fmt.Errorf("invalid deployment ID in %s", name)
	}
	return string(id), nil
}

func prepareRemoteDeployment(ctx context.Context, client *sshio.Client, id string, install bool) (home string, uploaded bool, err error) {
	session, err := client.NewSession()
	if err != nil {
		return "", false, err
	}
	defer session.Close()
	in, err := session.StdinPipe()
	if err != nil {
		return "", false, err
	}
	out, err := session.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		return "", false, fmt.Errorf("open SFTP for remote deployment: %w", err)
	}
	ftp, err := sftp.NewClientPipe(ctx, out, in)
	if err != nil {
		return "", false, err
	}
	defer ftp.Close()
	home, err = ftp.RealPath(".")
	if err != nil {
		return "", false, err
	}
	platform, err := remoteDeploymentPlatform(home)
	if err != nil {
		return "", false, err
	}
	relative := platform.binaryPath(id)
	destination := path.Join(home, relative)
	exists, err := deploymentExists(ftp, destination)
	if err != nil {
		return "", false, err
	}
	if !exists {
		if !install {
			return "", false, errRemoteDeploymentMissing
		}
		dir := home
		for part := range strings.SplitSeq(path.Dir(relative), "/") {
			dir = path.Join(dir, part)
			if err := ensureDeploymentDirectory(ftp, dir, platform.os == "windows"); err != nil {
				return "", false, err
			}
		}
		source, err := deploymentSource(ctx, platform.os, platform.binary)
		if err != nil {
			return "", false, err
		}
		defer source.Close()
		if err := uploadDeployment(ftp, source, destination, platform.os == "windows"); err != nil {
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

func (p deploymentPlatform) binaryPath(id string) string {
	return path.Join(deploymentDirectory, id, version, p.binary)
}

func deploymentCommand(home, id string) (string, error) {
	p, err := remoteDeploymentPlatform(home)
	if err != nil {
		return "", err
	}
	binary := p.binaryPath(id)
	if p.os == "windows" {
		binary = `.\` + strings.ReplaceAll(binary, "/", `\`)
	} else {
		binary = shellQuote(path.Join(home, binary))
	}
	return binary + " " + remoteServiceCommand, nil
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
	temporary := destination + "." + resourceToken() + ".tmp"
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

func deploymentSource(ctx context.Context, target, binary string) (io.ReadCloser, error) {
	if runtime.GOOS == target && runtime.GOARCH == "amd64" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return os.Open(exe)
	}
	if _, ok := releaseParts(version); !ok {
		return nil, fmt.Errorf("cross-platform remote deployment requires a published release")
	}
	data, err := downloadDeployment(ctx, target, binary)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func downloadDeployment(ctx context.Context, target, binary string) ([]byte, error) {
	extension := ".tar.gz"
	if target == "windows" {
		extension = ".zip"
	}
	name := "sshore_" + version + "_" + target + "_amd64" + extension
	base := deploymentReleaseBase + "/v" + version + "/"
	manifest, err := readDeploymentAsset(ctx, base+"SHA256SUMS", 16<<10)
	if err != nil {
		return nil, err
	}
	var expected string
	for line := range strings.SplitSeq(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			if expected != "" {
				return nil, fmt.Errorf("duplicate release checksum for %s", name)
			}
			expected = fields[0]
		}
	}
	if len(expected) != 2*sha256.Size {
		return nil, fmt.Errorf("release checksum missing for %s", name)
	}
	archive, err := readDeploymentAsset(ctx, base+name, maxDeploymentAsset)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), expected) {
		return nil, fmt.Errorf("release checksum mismatch for %s", name)
	}
	if target == "windows" {
		z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range z.File {
			if f.Name == binary && f.Mode().IsRegular() {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return readBoundedDeployment(r, maxDeploymentAsset)
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			entry, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if entry.Name == binary && entry.Typeflag == tar.TypeReg {
				return readBoundedDeployment(tr, maxDeploymentAsset)
			}
		}
	}
	return nil, fmt.Errorf("release archive %s lacks %s", name, binary)
}

func readDeploymentAsset(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	response, err := deploymentHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	return readBoundedDeployment(response.Body, limit)
}

func readBoundedDeployment(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("release asset exceeds %d bytes", limit)
	}
	return data, err
}
