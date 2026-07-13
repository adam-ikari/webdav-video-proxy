package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

// 用一个可控的上游：返回给定字节区间的内容，并记录请求顺序。
func newUpstream(t *testing.T, total int64, payload []byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchOrderedOutput(t *testing.T) {
	// 16 字节文件，块大小 4，请求 0-15，N=4
	payload := []byte("0123456789abcdef")
	srv := newUpstream(t, int64(len(payload)), payload)
	ss := source.SubSource{Endpoint: srv.URL, TopSegment: "src"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Friendly, SuggestedN: 4, BandwidthMbps: 100}

	st, _ := store.Open(t.TempDir() + "/f.db")
	defer st.Close()
	cfg := config.Config{CacheBlockSize: 4, CacheMaxSize: 1 << 20}
	c := cache.New(st, cfg, nil)
	asm := assessor.New(st, cfg, nil)
	p := NewPlanner(cfg)
	f := New(c, asm, nil)

	plan := p.Plan(ss, "/f.bin", "v", 0, int64(len(payload))-1, prof)
	plan.N = 4 // 强制 4 路以测乱序到顺序吐
	rc, err := f.Fetch(context.Background(), plan)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(payload) {
		t.Errorf("output = %q, want %q", got, payload)
	}
}

func TestPlanUnknownSourceIsN1(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4, DefaultMaxConcurrency: 4}
	p := NewPlanner(cfg)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Unknown, SuggestedN: 1}
	plan := p.Plan(ss, "/f", "v", 0, 100, prof)
	if plan.N != 1 {
		t.Errorf("unknown source N = %d, want 1", plan.N)
	}
}

func TestPlanThrottledIsN1(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4, DefaultMaxConcurrency: 4}
	p := NewPlanner(cfg)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Throttled, SuggestedN: 1}
	plan := p.Plan(ss, "/f", "v", 0, 100, prof)
	if plan.N != 1 {
		t.Errorf("throttled N = %d, want 1", plan.N)
	}
}

// TestFetchWholeFileFallback：上游忽略 Range 回 200 整文件，fetcher 降级单连接整文件流，
// 仍按 [start,end] 顺序吐出正确内容。
func TestFetchWholeFileFallback(t *testing.T) {
	payload := []byte("0123456789abcdef")
	// 上游不认 Range，永远回 200 整文件
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer srv.Close()
	ss := source.SubSource{Endpoint: srv.URL, TopSegment: "src"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Friendly, SuggestedN: 4, BandwidthMbps: 100}

	st, _ := store.Open(t.TempDir() + "/f.db")
	defer st.Close()
	cfg := config.Config{CacheBlockSize: 4, CacheMaxSize: 1 << 20}
	c := cache.New(st, cfg, nil)
	asm := assessor.New(st, cfg, nil)
	p := NewPlanner(cfg)
	f := New(c, asm, nil)

	plan := p.Plan(ss, "/f.bin", "v", 0, int64(len(payload))-1, prof)
	plan.N = 4
	rc, err := f.Fetch(context.Background(), plan)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(payload) {
		t.Errorf("whole-file fallback output = %q, want %q", got, payload)
	}
}

// TestFetchOrderedOutputUnaligned：Range 起点非块对齐时仍正确吐出。
func TestFetchOrderedOutputUnaligned(t *testing.T) {
	payload := []byte("0123456789abcdef")
	srv := newUpstream(t, int64(len(payload)), payload)
	ss := source.SubSource{Endpoint: srv.URL, TopSegment: "src"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Friendly, SuggestedN: 4, BandwidthMbps: 100}

	st, _ := store.Open(t.TempDir() + "/f.db")
	defer st.Close()
	cfg := config.Config{CacheBlockSize: 4, CacheMaxSize: 1 << 20}
	c := cache.New(st, cfg, nil)
	asm := assessor.New(st, cfg, nil)
	p := NewPlanner(cfg)
	f := New(c, asm, nil)

	// 请求 bytes=2-13（非块对齐起止）
	plan := p.Plan(ss, "/f.bin", "v", 2, 13, prof)
	plan.N = 4
	rc, err := f.Fetch(context.Background(), plan)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	want := payload[2:14]
	if string(got) != string(want) {
		t.Errorf("unaligned output = %q, want %q", got, want)
	}
}
