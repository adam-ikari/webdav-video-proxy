package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "bytes=0-3" {
			t.Errorf("Range header = %q, want bytes=0-3", rng)
		}
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "abcd")
	}))
	defer srv.Close()
	c := NewClient()
	body, total, err := c.GetRange(srv.URL, "/file.mkv", 0, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "abcd" {
		t.Errorf("data = %q, want abcd", data)
	}
}

func TestHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		w.Header().Set("ETag", `"etag-xyz"`)
		w.Header().Set("Last-Modified", "Wed, 11 Jul 2026 00:00:00 GMT")
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient()
	etag, lastMod, size, err := c.Head(srv.URL, "/file.mkv")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if etag != `"etag-xyz"` {
		t.Errorf("etag = %q", etag)
	}
	if size != 12345 {
		t.Errorf("size = %d, want 12345", size)
	}
	if lastMod == "" {
		t.Error("lastMod empty")
	}
}

func TestPropFind(t *testing.T) {
	xml := `<?xml version="1.0"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/d/movie.mkv</D:href>
    <D:propstat><D:prop>
      <D:displayname>movie.mkv</D:displayname>
      <D:getcontentlength>1000</D:getcontentlength>
      <D:resourcetype/>
    </D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
  </D:response>
  <D:response>
    <D:href>/d/sub/</D:href>
    <D:propstat><D:prop>
      <D:displayname>sub</D:displayname>
      <D:resourcetype><D:collection/></D:resourcetype>
    </D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
  </D:response>
</D:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("method = %q, want PROPFIND", r.Method)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, xml)
	}))
	defer srv.Close()
	c := NewClient()
	entries, err := c.PropFind(srv.URL, "/d/", 1)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// 找到目录项
	var dir *Entry
	for i := range entries {
		if entries[i].IsDir {
			dir = &entries[i]
		}
	}
	if dir == nil || dir.DisplayName != "sub" {
		t.Errorf("dir entry = %+v", dir)
	}
	if !strings.Contains(dir.Href, "/sub/") {
		t.Errorf("dir href = %q", dir.Href)
	}
}
