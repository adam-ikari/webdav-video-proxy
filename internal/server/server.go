package server

import (
	"context"
	"net/http"
	"os"
	"sort"
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

	ctx    context.Context
	cancel context.CancelFunc
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

func (s *Server) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.st.Close()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleAll)
	return mux
}

// StartBackground 启缓存淘汰 worker。
func (s *Server) StartBackground(ctx context.Context) {
	// 用传入 ctx 派生一个可被 Close 取消的 ctx，供后台预取 goroutine 感知停机（I4-A）。
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.cache.StartEvictor(s.ctx)
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
	s.pruneMeta()
	s.metaMu.Unlock()
	return v, sz
}

// prefetchNextEpisode 在当前文件播放到 70% 时预取下一集首段（I4）。
// 列出当前文件所在目录（结果缓存 HeadRevalidateSec），按文件名自然排序算下一集，
// 交 preloader.NextPrefetch 预取。下一集用其自己的 version（见 tryPrefetch，I4-B）。
func (s *Server) prefetchNextEpisode(ss source.SubSource, currentFile string) {
	if s.pre == nil || currentFile == "" {
		return
	}
	// 停机感知：Close 会取消 s.ctx，预取 goroutine 不再打上游（I4-A）。
	if s.ctx != nil && s.ctx.Err() != nil {
		return
	}
	dir := pathDir(currentFile)
	key := ss.Key() + "|" + dir
	now := store.Now()
	s.dirMu.Lock()
	de, ok := s.dirMap[key]
	if ok && now-de.listedAt < s.cfg.HeadRevalidateSec && len(de.files) > 0 {
		s.dirMu.Unlock()
		s.tryPrefetch(ss, de.files, currentFile)
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
	s.pruneDir()
	s.dirMu.Unlock()
	s.tryPrefetch(ss, files, currentFile)
}

// tryPrefetch 计算下一集并触发预取。下一集用其自己的 version（HEAD 解析），
// 绝不用当前文件的 version——否则缓存键不匹配（预取作废）且两文件 version 恰都为
// "unknown" 时会跨文件串字节（I4-B 修正）。
func (s *Server) tryPrefetch(ss source.SubSource, files []string, currentFile string) {
	next := preloader.NextEpisode(files, pathBase(currentFile))
	if next == "" {
		return
	}
	dir := pathDir(currentFile)
	nextPath := dir + "/" + next
	// 解析下一集自己的 version（同样走低频 HEAD 缓存，不额外打上游）。
	nextVersion, _ := s.resolveVersion(ss, nextPath)
	s.pre.NextPrefetch(ss, nextPath, nextVersion)
}

// pruneMeta 在 metaMap 超过上限时淘汰最旧的一批（I4-A：防止长期运行 OOM）。
func (s *Server) pruneMeta() {
	cap := s.cfg.MetaCacheMaxEntries
	if cap <= 0 || len(s.metaMap) <= cap {
		return
	}
	type kv struct {
		k  string
		ts int64
	}
	ents := make([]kv, 0, len(s.metaMap))
	for k, m := range s.metaMap {
		ents = append(ents, kv{k, m.checkedAt})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].ts < ents[j].ts })
	// 淘汰最旧的 25%，腾出空间
	drop := len(ents) - cap + cap/4
	if drop < 1 {
		drop = 1
	}
	for i := 0; i < drop && i < len(ents); i++ {
		delete(s.metaMap, ents[i].k)
	}
}

// pruneDir 同理对 dirMap。
func (s *Server) pruneDir() {
	cap := s.cfg.MetaCacheMaxEntries
	if cap <= 0 || len(s.dirMap) <= cap {
		return
	}
	type kv struct {
		k  string
		ts int64
	}
	ents := make([]kv, 0, len(s.dirMap))
	for k, d := range s.dirMap {
		ents = append(ents, kv{k, d.listedAt})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].ts < ents[j].ts })
	drop := len(ents) - cap + cap/4
	if drop < 1 {
		drop = 1
	}
	for i := 0; i < drop && i < len(ents); i++ {
		delete(s.dirMap, ents[i].k)
	}
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
