package release

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAssetDownloadReportsStartBeforeHTTPAndTracksBody(t *testing.T) {
	const name, body = "release.zip", "release payload"
	for _, size := range []int64{int64(len(body)), -1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var started atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !started.Load() {
					t.Error("HTTP request started before the download announcement")
				}
				if size >= 0 {
					w.Header().Set("Content-Length", fmt.Sprint(size))
				} else if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			tracked := false
			progress := AssetProgress{
				Start: func(asset string) {
					if asset != name {
						t.Fatalf("announced %q, want %q", asset, name)
					}
					started.Store(true)
				},
				Transfer: func(asset string, total int64, source io.Reader, read func(io.Reader) error) error {
					tracked = true
					if asset != name || total != size {
						t.Fatalf("tracked %q (%d bytes), want %q (%d bytes)", asset, total, name, size)
					}
					return read(source)
				},
			}
			data, err := readAsset(t.Context(), server.URL+"/", name, int64(len(body)), progress)
			if err != nil || string(data) != body || !tracked {
				t.Fatalf("body %q, tracked %t, error %v", data, tracked, err)
			}
		})
	}
}
