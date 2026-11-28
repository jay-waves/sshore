package release

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
	"net/http"
	"os"
	"runtime"
	"strings"
)

const maxAssetSize = 64 << 20

var assetClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || req.URL.Scheme != via[0].URL.Scheme {
		return fmt.Errorf("invalid release download redirect")
	}
	return nil
}}

// OpenBinary returns the deployment binary and its uncompressed size in bytes.
func OpenBinary(ctx context.Context, baseURL, version, target, binary string) (io.ReadCloser, int64, error) {
	if runtime.GOOS == target && runtime.GOARCH == "amd64" {
		exe, err := os.Executable()
		if err != nil {
			return nil, 0, err
		}
		file, err := os.Open(exe)
		if err != nil {
			return nil, 0, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, 0, err
		}
		return file, info.Size(), nil
	}
	if !Valid(version) {
		return nil, 0, fmt.Errorf("cross-platform remote deployment requires a published release")
	}
	data, err := downloadBinary(ctx, baseURL, version, target, binary)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func downloadBinary(ctx context.Context, baseURL, version, target, binary string) ([]byte, error) {
	extension := ".tar.gz"
	if target == "windows" {
		extension = ".zip"
	}
	name := "sshore_" + version + "_" + target + "_amd64" + extension
	base := baseURL + "/v" + version + "/"
	manifest, err := readAsset(ctx, base+"SHA256SUMS", 16<<10)
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
	archive, err := readAsset(ctx, base+name, maxAssetSize)
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
				return readBounded(r, maxAssetSize)
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
				return readBounded(tr, maxAssetSize)
			}
		}
	}
	return nil, fmt.Errorf("release archive %s lacks %s", name, binary)
}

func readAsset(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	response, err := assetClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	return readBounded(response.Body, limit)
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("release asset exceeds %d bytes", limit)
	}
	return data, err
}
