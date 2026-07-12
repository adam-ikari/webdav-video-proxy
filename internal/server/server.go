package server

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/fetcher"
	"github.com/gem/webdav-proxy/internal/marker"
	"github.com/gem/webdav-proxy/internal/preloader"
	"github.com/gem/webdav-proxy/internal/store"
	"github.com/gem/webdav-proxy/internal/webdav"
)

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
