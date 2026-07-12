package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/source"
)

func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD":
		s.handleGet(w, r)
	case "PROPFIND":
		s.handlePropFind(w, r)
	case "OPTIONS":
		w.Header().Set("DAV", "1")
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	displayPath := r.URL.Path
	realPath, _ := s.mark.StripMarker(displayPath)
	ss := source.ParseSubSource(s.endpoint, realPath)
	rest := ss.RestPath(realPath)

	// 一致性校验：HEAD 拿 ETag/Size
	etag, _, size, err := s.cli.Head(s.endpoint, rest)
	if err != nil {
		// 降级：ETag 未知，继续单路取流
		etag = ""
	}
	version := etag
	if version == "" {
		version = "unknown"
	}

	// 解析 Range（如 "bytes=0-1023" 或 "bytes=0-"）
	start, end := int64(0), size-1
	if cr := r.Header.Get("Range"); cr != "" {
		if strings.HasPrefix(cr, "bytes=") {
			rng := strings.TrimPrefix(cr, "bytes=")
			if i := strings.Index(rng, "-"); i >= 0 {
				start, _ = strconv.ParseInt(rng[:i], 10, 64)
				if rng[i+1:] != "" {
					end, _ = strconv.ParseInt(rng[i+1:], 10, 64)
				}
			}
		}
	}
	if size > 0 && end >= size {
		end = size - 1
	}

	// 缓存全命中检查（按块）
	ck := cache.CacheKey{SS: ss, FilePath: rest, Version: version}
	bs := s.cfg.CacheBlockSize
	blkStart := (start / bs) * bs
	blkEnd := ((end + bs - 1) / bs) * bs
	if size > 0 && blkEnd >= size {
		blkEnd = size - 1
	}
	allHit := true
	for off := blkStart; off <= blkEnd && allHit; off += bs {
		ok, _ := s.cache.Has(ck, off/bs)
		if !ok {
			allHit = false
		}
	}

	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)

	if allHit {
		flushCached(w, s.cache, ck, start, end, bs)
		s.pre.OnRead(ss, rest, version, end, size) // 顺序预读触发
		return
	}

	prof := s.asm.GetProfile(ss)
	plan := s.plan.Plan(ss, rest, version, start, end, prof)
	rc, err := s.fetch.Fetch(r.Context(), plan)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
	s.pre.OnRead(ss, rest, version, end, size)
	if size > 0 {
		ratio := float64(end) / float64(size)
		s.pre.OnProgress(ss, "", rest, ratio)
	}
}

// flushCached 把命中块按 [start,end] 截取后吐出。
func flushCached(w http.ResponseWriter, c *cache.Cache, ck cache.CacheKey, start, end, bs int64) {
	for off := (start / bs) * bs; off <= end; off += bs {
		data, _, err := c.Get(ck, off/bs)
		if err != nil || data == nil {
			continue
		}
		segStart := int64(0)
		if off < start {
			segStart = start - off
		}
		seg := data[segStart:]
		if off+int64(len(seg))-1 > end {
			seg = seg[:end-off+1]
		}
		_, _ = w.Write(seg)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
