package fetcher

import (
	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
)

type FetchPlan struct {
	SS                source.SubSource
	FilePath, Version string
	Start, End        int64
	N                 int
	BlockSize         int64
}

type Planner struct {
	cfg config.Config
}

func NewPlanner(cfg config.Config) *Planner {
	return &Planner{cfg: cfg}
}

// Plan 产拉取计划。未知/不友好/风控 → N=1；友好 → N=SuggestedN。
// 首块探速由 Fetcher 执行时动态调整，Plan 只给初始 N。
//
// 已知下游隐患：assessor.GetProfile 在 probe 落库前可能返回来自
// EnsureProfile/markProbed 的退化行（SuggestedN=0）。此处显式兜底：
// SuggestedN<=0 一律视作 N=1，绝不让 N=0 抵达 fetcher。
func (p *Planner) Plan(ss source.SubSource, path, version string, start, end int64, prof assessor.Profile) FetchPlan {
	n := 1
	if prof.Friendly == assessor.Friendly && prof.SuggestedN > 1 {
		n = prof.SuggestedN
		if n > p.cfg.DefaultMaxConcurrency {
			n = p.cfg.DefaultMaxConcurrency
		}
	}
	if n <= 0 { // 兜底：退化行 SuggestedN=0
		n = 1
	}
	return FetchPlan{
		SS:        ss,
		FilePath:  path,
		Version:   version,
		Start:     start,
		End:       end,
		N:         n,
		BlockSize: p.cfg.CacheBlockSize,
	}
}
