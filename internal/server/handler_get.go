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

	// 一致性校验（低频 HEAD，I6）：HeadRevalidateSec 内复用上次 version/size，
	// 不每次请求都打上游 HEAD。version 未知时降级为 "unknown" 继续单路取流。
	version, size := s.resolveVersion(ss, rest)

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

	// 缓存全命中检查（按块，直接用 Get 而非 Has+Get 两段式，避免 Has 与 Get 之间
	// 块被淘汰器移除导致的静默截断）。任一块缺失则 allHit=false，走 Fetch 回填。
	ck := cache.CacheKey{SS: ss, FilePath: rest, Version: version}
	bs := s.cfg.CacheBlockSize
	blkStart := (start / bs) * bs
	blkEnd := ((end + bs - 1) / bs) * bs
	if size > 0 && blkEnd >= size {
		blkEnd = size - 1
	}
	cachedBlocks := map[int64][]byte{}
	allHit := true
	for off := blkStart; off <= blkEnd && allHit; off += bs {
		data, ok, _ := s.cache.Get(ck, off/bs)
		if !ok || data == nil {
			allHit = false
			break
		}
		cachedBlocks[off] = data
	}

	w.Header().Set("Accept-Ranges", "bytes")
	if r.Header.Get("Range") != "" && size > 0 {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else if size <= 0 {
		// Size unknown (HEAD failed) and no usable Range: return 200 with empty body.
		w.WriteHeader(http.StatusOK)
		return
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}

	if allHit {
		// 全命中：Acquire 所有相关块引用，防止淘汰器在吐出期间移除（I3），
		// 再用已取到的 cachedBlocks 吐出（无需二次 Get，杜绝 Has/Get 间的 TOCTOU）。
		var releases []cache.ReleaseFunc
		for off := range cachedBlocks {
			releases = append(releases, s.cache.Acquire(ck, off/bs))
		}
		flushCachedBlocks(w, cachedBlocks, start, end, bs)
		for _, rel := range releases {
			rel()
		}
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
		if ratio >= 0.7 {
			// 到 70%：预取下一集首段（I4）。后台进行，不阻塞当前响应（当前响应已近完成）。
			go s.prefetchNextEpisode(ss, rest, version)
		}
	}
}

// flushCachedBlocks 把已取到的命中块按 [start,end] 截取后吐出。
func flushCachedBlocks(w http.ResponseWriter, blocks map[int64][]byte, start, end, bs int64) {
	for off := (start / bs) * bs; off <= end; off += bs {
		data, ok := blocks[off]
		if !ok || data == nil {
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
