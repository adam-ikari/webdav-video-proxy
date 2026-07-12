package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/store"
)

// newTestServer 构造一个指向给定 upstream 的 Server，用临时缓存目录。
func newTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cfg := config.Config{
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		CacheBlockSize:        4,
		CacheMaxSize:          1 << 20,
		CacheHighWatermark:    0.9,
		CacheLowWatermark:     0.7,
		CacheTTL:              99999,
		DefaultMaxConcurrency: 4,
		VideoExts:             ".mkv,.mp4,.ts",
	}
	srv, err := New(cfg, upstream)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// SetProfile 直接写一条子源画像，供测试预设慢源。
func (s *Server) SetProfile(topSegment string, bw float64, slow bool) {
	_ = s.st.SaveProfile(store.ProfileRow{
		SubKey:        s.endpoint + "|" + topSegment,
		BandwidthMbps: bw,
		Friendly:      "unknown",
		SuggestedN:    1,
		IsSlow:        slow,
		UpdatedAt:     1,
	})
}

func TestGetStreamsAndCaches(t *testing.T) {
	payload := []byte("0123456789abcdef")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", "16")
		if rng := r.Header.Get("Range"); rng != "" {
			var s, e int
			fmt.Sscanf(rng, "bytes=%d-%d", &s, &e)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/16", s, e))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[s : e+1])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// GET 整个文件
	resp, err := http.Get(ts.URL + "/src/f.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(payload) {
		t.Errorf("got = %q, want %q", got, payload)
	}
}

func TestPropFindMarksSlowFolder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xml := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/src/movie/</D:href><D:propstat><D:prop>
<D:displayname>movie</D:displayname><D:resourcetype><D:collection/></D:resourcetype>
</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/src/movie/x.mkv</D:href><D:propstat><D:prop>
<D:displayname>x.mkv</D:displayname><D:getcontentlength>16</D:getcontentlength>
<D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, xml)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	srv.SetProfile("src", 1.0, true)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("PROPFIND", ts.URL+"/src/movie/", nil)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "slow") {
		t.Errorf("PROPFIND should mark slow folder, got: %s", body)
	}
}
