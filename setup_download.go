package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"strconv"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/wire"
)

const maxSetupDownload = 64 * 1024 * 1024
const releaseDownloadURL = "https://github.com/jay-waves/sshore/releases/download"

func localSetupBinary(ctx context.Context) ([]byte, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("invalid release download redirect")
		}
		return nil
	}}
	defer client.CloseIdleConnections()
	return fetchSetupBinary(ctx, client, releaseDownloadURL, filepath.Join(root, "sshore", "releases"), version)
}

func fetchSetupBinary(ctx context.Context, client *http.Client, base, cache, release string) ([]byte, error) {
	if _, ok := releaseSeries(release); !ok {
		return nil, fmt.Errorf("automatic download requires a full published release")
	}
	dir := filepath.Join(cache, release)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := privateStatePath(dir, true); err != nil {
		return nil, err
	}
	name := "sshore_" + release + "_linux_amd64.tar.gz"
	archivePath := filepath.Join(dir, name)
	manifestPath := filepath.Join(dir, "SHA256SUMS")
	manifest, manifestErr := readSetupCache(manifestPath, 256*1024)
	archive, archiveErr := readSetupCache(archivePath, maxSetupDownload)
	if manifestErr == nil && archiveErr == nil {
		if binary, err := verifiedSetupArchive(manifest, archive, name); err == nil {
			return binary, nil
		}
	}
	url := strings.TrimRight(base, "/") + "/v" + release + "/"
	manifest, err := downloadSetupFile(ctx, client, url+"SHA256SUMS", 256*1024)
	if err != nil {
		return nil, fmt.Errorf("download sshore %s checksums: %w", release, err)
	}
	archive, err = downloadSetupFile(ctx, client, url+name, maxSetupDownload)
	if err != nil {
		return nil, fmt.Errorf("download sshore %s Linux amd64: %w", release, err)
	}
	binary, err := verifiedSetupArchive(manifest, archive, name)
	if err != nil {
		return nil, err
	}
	if err = writeStateFile(archivePath, archive); err != nil {
		return nil, fmt.Errorf("cache release: %w", err)
	}
	if err = writeStateFile(manifestPath, manifest); err != nil {
		return nil, fmt.Errorf("cache checksums: %w", err)
	}
	return binary, nil
}

func readSetupCache(path string, limit int64) ([]byte, error) {
	if err := privateStatePath(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readSetupLimited(f, limit)
}

func readSetupLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("release download exceeds size limit")
	}
	return b, err
}

func downloadSetupFile(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		retry := err != nil
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				var data []byte
				data, err = readSetupLimited(resp.Body, limit)
				resp.Body.Close()
				if err == nil || int64(len(data)) > limit {
					return data, err
				}
				retry = true
			} else {
				resp.Body.Close()
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
				if resp.StatusCode == http.StatusNotFound {
					return nil, fmt.Errorf("matching release asset unavailable (HTTP 404); publish/install this exact version or update the local client")
				}
				retry = resp.StatusCode == 429 || resp.StatusCode >= 500
			}
		}
		if !retry || attempt >= 2 {
			return nil, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func verifiedSetupArchive(manifest, archive []byte, name string) ([]byte, error) {
	var expected string
	for line := range strings.SplitSeq(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if expected != "" {
				return nil, fmt.Errorf("duplicate release checksum")
			}
			expected = fields[0]
		}
	}
	digest, err := hex.DecodeString(expected)
	sum := sha256.Sum256(archive)
	if err != nil || len(digest) != sha256.Size || !bytes.Equal(digest, sum[:]) {
		return nil, fmt.Errorf("release checksum verification failed")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, maxSetupDownload+1))
	var binary []byte
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Name != "sshore" {
			continue
		}
		if binary != nil || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxSetupDownload {
			return nil, fmt.Errorf("invalid release executable")
		}
		binary, err = readSetupLimited(reader, maxSetupDownload)
		if err != nil {
			return nil, err
		}
	}
	if len(binary) == 0 {
		return nil, fmt.Errorf("release executable missing")
	}
	return binary, nil
}

func sendSetupBinary(w io.Writer, binary []byte) error {
	if len(binary) == 0 || len(binary) > maxSetupDownload {
		return fmt.Errorf("invalid setup executable size")
	}
	if _, err := fmt.Fprintf(w, "%d %x\n", len(binary), sha256.Sum256(binary)); err != nil {
		return err
	}
	return wire.WriteFull(w, binary)
}

// Shell read and head consume exactly the header and file, leaving the same
// authenticated stream positioned at the coordinator's next framed message.
func relaySetupBinary(dst io.Writer, src io.Reader) error {
	var header []byte
	for len(header) < 100 {
		var b [1]byte
		if _, err := io.ReadFull(src, b[:]); err != nil {
			return err
		}
		header = append(header, b[0])
		if b[0] != '\n' {
			continue
		}
		if string(header) == "0 -\n" {
			return wire.WriteFull(dst, header)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 2 {
			break
		}
		size, err := strconv.ParseInt(fields[0], 10, 64)
		digest, hashErr := hex.DecodeString(fields[1])
		if err != nil || size <= 0 || size > maxSetupDownload || hashErr != nil || len(digest) != sha256.Size {
			break
		}
		if err := wire.WriteFull(dst, header); err != nil {
			return err
		}
		_, err = io.CopyN(dst, src, size)
		return err
	}
	return fmt.Errorf("invalid setup transfer header")
}
