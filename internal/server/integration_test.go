package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/store"
)

// makePayload builds a deterministic payload of n bytes.
func makePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('A' + (i % 26))
	}
	return b
}

// integrationUpstream 模拟一个 Range-compliant 的 WebDAV 上游。
// 统计 GET（带 Range）次数，用于验证缓存命中后不再打上游。
func integrationUpstream(t *testing.T, payload []byte, supportsRange bool) (*httptest.Server, *int64) {
	var gets int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("ETag", `"v-int"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		case "GET":
			atomic.AddInt64(&gets, 1)
			if !supportsRange || r.Header.Get("Range") == "" {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.WriteHeader(http.StatusOK)
				w.Write(payload)
				return
			}
			var s, e int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e)
			if e >= int64(len(payload)) {
				e = int64(len(payload)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(payload)))
			w.Header().Set("Content-Length", strconv.FormatInt(e-s+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[s : e+1])
			return
		case "PROPFIND":
			// 列目录：返回一个含视频文件的伪 multistatus
			xml := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/src/movie/</D:href><D:propstat><D:prop>
<D:displayname>movie</D:displayname><D:resourcetype><D:collection/></D:resourcetype>
</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/src/movie/ep01.mkv</D:href><D:propstat><D:prop>
<D:displayname>ep01.mkv</D:displayname><D:getcontentlength>16</D:getcontentlength>
<D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/src/movie/ep02.mkv</D:href><D:propstat><D:prop>
<D:displayname>ep02.mkv</D:displayname><D:getcontentlength>16</D:getcontentlength>
<D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, xml)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

func newIntegrationServer(t *testing.T, upstream string) *Server {
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
		HeadRevalidateSec:     1,
	}
	srv, err := New(cfg, upstream)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// TestIntegrationRangeGetAndCacheHit：Range GET 返回正确字节与 206；
// 同一区间二次 GET 应命中缓存（上游 GET 次数不增加）。块大小设为文件大小以禁用预读干扰。
func TestIntegrationRangeGetAndCacheHit(t *testing.T) {
	payload := makePayload(64)
	upstream, gets := integrationUpstream(t, payload, true)
	// 用 64 字节块 = 整个文件一个块，避免 OnRead 预读引入额外上游请求干扰断言。
	cfg := config.Config{
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		CacheBlockSize:        64,
		CacheMaxSize:          1 << 20,
		CacheHighWatermark:    0.9,
		CacheLowWatermark:     0.7,
		CacheTTL:              99999,
		DefaultMaxConcurrency: 4,
		VideoExts:             ".mkv,.mp4,.ts",
		HeadRevalidateSec:     60,
	}
	srv, err := New(cfg, upstream.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 第一次 Range GET（整个文件）
	req, _ := http.NewRequest("GET", ts.URL+"/src/file.mkv", nil)
	req.Header.Set("Range", "bytes=0-63")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(payload) {
		t.Errorf("body mismatch")
	}
	// 等待可能的异步预读完成（本例 prefetchStart=63+64>=64 不触发，但留余量）
	time.Sleep(50 * time.Millisecond)
	firstGets := atomic.LoadInt64(gets)

	// 第二次同区间：应命中缓存，上游 GET 次数不增加
	req2, _ := http.NewRequest("GET", ts.URL+"/src/file.mkv", nil)
	req2.Header.Set("Range", "bytes=0-63")
	resp2, _ := http.DefaultClient.Do(req2)
	got2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(got2) != string(payload) {
		t.Errorf("second body mismatch")
	}
	if second := atomic.LoadInt64(gets); second != firstGets {
		t.Errorf("cache miss: upstream GETs went %d -> %d (expected no increase)", firstGets, second)
	}
}

// TestIntegrationWholeFileFallback：上游不支持 Range，fetcher 降级整文件流，仍吐正确内容。
func TestIntegrationWholeFileFallback(t *testing.T) {
	payload := makePayload(32)
	upstream, _ := integrationUpstream(t, payload, false)
	srv := newIntegrationServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/src/file.mkv", nil)
	req.Header.Set("Range", "bytes=0-31")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(payload) {
		t.Errorf("whole-file fallback body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestIntegrationPropFindSlowMarking：慢源下影视文件夹被标记（含 slow）。
func TestIntegrationPropFindSlowMarking(t *testing.T) {
	payload := makePayload(16)
	upstream, _ := integrationUpstream(t, payload, true)
	srv := newIntegrationServer(t, upstream.URL)
	// 预置 src 子源为慢源
	_ = srv.st.SaveProfile(store.ProfileRow{
		SubKey:        srv.endpoint + "|src",
		BandwidthMbps: 0.5,
		Friendly:      "unknown",
		SuggestedN:    1,
		IsSlow:        true,
		UpdatedAt:     store.Now(),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("PROPFIND", ts.URL+"/src/movie/", nil)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "·slow·") {
		t.Errorf("expected slow marker in PROPFIND, got: %s", body)
	}
}

// TestIntegrationOrderedLargeFile：多块文件（256 字节，块 4）Range GET 完整且有序。
func TestIntegrationOrderedLargeFile(t *testing.T) {
	payload := makePayload(256)
	upstream, _ := integrationUpstream(t, payload, true)
	srv := newIntegrationServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/src/file.mkv", nil)
	req.Header.Set("Range", "bytes=0-255")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(got) != 256 {
		t.Fatalf("len = %d, want 256", len(got))
	}
	if string(got) != string(payload) {
		t.Errorf("large-file content mismatch")
	}
}

func init() {
	// 集成测试用真实时钟，确保 HEAD 校验间隔逻辑可测
	store.ResetClock()
	_ = time.Second
}
