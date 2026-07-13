package server

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/fetcher"
	"github.com/gem/webdav-proxy/internal/marker"
	"github.com/gem/webdav-proxy/internal/preloader"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
	"github.com/gem/webdav-proxy/internal/webdav"
)

// fileMeta 缓存某文件最近一次 HEAD 校验得到的 version/size，用于低频校验（I6）：
// 同一文件在 HeadRevalidateSec 内不重复向上游发 HEAD，直接复用上次版本。
type fileMeta struct {
	version   string
	size      int64
	checkedAt int64
}

// dirEntry 缓存某目录的文件名列表，供下一集预取计算用。
type dirEntry struct {
	files    []string
	listedAt int64
}

type Server struct {
	cfg      config.Config
	endpoint string // 单上游端点（首版只支持一个 endpoint）
	st       *store.Store
	cache    *cache.Cache
	asm      *assessor.Assessor
	fetch    *fetcher.Fetcher
	plan     *fetcher.Planner
	pre      *preloader.Preloader
	mark     *marker.Marker
	cli      *webdav.Client

	metaMu  sync.Mutex
	metaMap map[string]fileMeta // key = subkey + "|" + filepath

	dirMu  sync.Mutex
	dirMap map[string]dirEntry // key = subkey + "|" + dirpath
}

// New 装配全部单元。endpoint 为上游 WebDAV 根 URL。
func New(cfg config.Config, endpoint string) (*Server, error) {
	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(cfg.CacheDir + "/index.db")
	if err != nil {
		return nil, err
	}
	c := cache.New(st, cfg, nil)
	asm := assessor.New(st, cfg, nil)
	f := fetcher.New(c, asm, nil)
	p := fetcher.NewPlanner(cfg)
	pre := preloader.New(f, cfg, asm)
	m := marker.New(parseExts(cfg.VideoExts))
	marker.ConfigureGlobal(parseExts(cfg.VideoExts)) // 包级 IsVideoFile 也用配置的扩展名（M5）
	return &Server{
		cfg:      cfg,
		endpoint: endpoint,
		st:       st,
		cache:    c,
		asm:      asm,
		fetch:    f,
		plan:     p,
		pre:      pre,
		mark:     m,
		cli:      webdav.NewClient(),
		metaMap:  map[string]fileMeta{},
		dirMap:   map[string]dirEntry{},
	}, nil
}

func (s *Server) Close() error { return s.st.Close() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleAll)
	return mux
}

// StartBackground 启缓存淘汰 worker。
func (s *Server) StartBackground(ctx context.Context) {
	s.cache.StartEvictor(ctx)
}

// resolveVersion 低频 HEAD 校验（I6）：同一文件在 HeadRevalidateSec 内复用上次 version/size，
// 不重复向上游发 HEAD；过期或首次则发 HEAD 并缓存结果。返回 version/size。
func (s *Server) resolveVersion(ss source.SubSource, rest string) (version string, size int64) {
	key := ss.Key() + "|" + rest
	now := store.Now()
	s.metaMu.Lock()
	if m, ok := s.metaMap[key]; ok && now-m.checkedAt < s.cfg.HeadRevalidateSec {
		s.metaMu.Unlock()
		return m.version, m.size
	}
	s.metaMu.Unlock()

	etag, _, sz, err := s.cli.Head(s.endpoint, rest)
	if err != nil {
		// HEAD 失败：若有过期记录则复用（宁可偶尔吐旧版本也不阻断主路径），否则 version 未知
		s.metaMu.Lock()
		if m, ok := s.metaMap[key]; ok {
			s.metaMu.Unlock()
			return m.version, m.size
		}
		s.metaMu.Unlock()
		return "unknown", 0
	}
	v := etag
	if v == "" {
		v = "unknown"
	}
	s.metaMu.Lock()
	s.metaMap[key] = fileMeta{version: v, size: sz, checkedAt: now}
	s.metaMu.Unlock()
	return v, sz
}

// prefetchNextEpisode 在当前文件播放到 70% 时预取下一集首段（I4）。
// 列出当前文件所在目录（结果缓存 HeadRevalidateSec），按文件名自然排序算下一集，
// 交 preloader.NextPrefetch 预取。version 复用当前文件版本（下一集同目录同源，近似）。
func (s *Server) prefetchNextEpisode(ss source.SubSource, currentFile, version string) {
	if s.pre == nil || currentFile == "" {
		return
	}
	dir := pathDir(currentFile)
	key := ss.Key() + "|" + dir
	now := store.Now()
	s.dirMu.Lock()
	de, ok := s.dirMap[key]
	if ok && now-de.listedAt < s.cfg.HeadRevalidateSec && len(de.files) > 0 {
		s.dirMu.Unlock()
		s.tryPrefetch(ss, de.files, currentFile, version)
		return
	}
	s.dirMu.Unlock()

	// 列目录（depth 1）。失败则放弃预取（不阻断主路径）。
	entries, err := s.cli.PropFind(s.endpoint, dir, 1)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e.DisplayName)
		}
	}
	s.dirMu.Lock()
	s.dirMap[key] = dirEntry{files: files, listedAt: now}
	s.dirMu.Unlock()
	s.tryPrefetch(ss, files, currentFile, version)
}

// tryPrefetch 计算下一集并触发预取。
func (s *Server) tryPrefetch(ss source.SubSource, files []string, currentFile, version string) {
	next := preloader.NextEpisode(files, pathBase(currentFile))
	if next == "" {
		return
	}
	dir := pathDir(currentFile)
	s.pre.NextPrefetch(ss, dir+"/"+next, version)
}

// pathDir 返回文件的目录部分（含前导斜杠）。
func pathDir(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// pathBase 返回文件名部分。
func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parseExts "a,b" -> []string{".a",".b"}（补点）
func parseExts(s string) []string {
	parts := strings.Split(s, ",")
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, ".") {
			p = "." + p
		}
		out = append(out, strings.ToLower(p))
	}
	return out
}
