package assessor

import (
	"sort"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
	"github.com/gem/webdav-proxy/internal/webdav"
)

type Friendliness string

const (
	Friendly   Friendliness = "friendly"
	Unfriendly Friendliness = "unfriendly"
	Throttled  Friendliness = "throttled"
	Unknown    Friendliness = "unknown"
)

type Profile struct {
	SubSource     source.SubSource
	BandwidthMbps float64
	Friendly      Friendliness
	SuggestedN    int
	IsSlow        bool
	UpdatedAt     int64
}

type Assessor struct {
	st  *store.Store
	cfg config.Config
	cli *webdav.Client
}

func New(st *store.Store, cfg config.Config, cli *webdav.Client) *Assessor {
	if cli == nil {
		cli = webdav.NewClient()
	}
	return &Assessor{st: st, cfg: cfg, cli: cli}
}

// GetProfile 读画像。缺失或过期回退 Unknown + N=1。
func (a *Assessor) GetProfile(ss source.SubSource) Profile {
	row, ok, err := a.st.GetProfile(ss.Key())
	if err != nil || !ok {
		return a.unknown(ss)
	}
	if a.expired(row.UpdatedAt) {
		return a.unknown(ss)
	}
	return Profile{
		SubSource:     ss,
		BandwidthMbps: row.BandwidthMbps,
		Friendly:      Friendliness(row.Friendly),
		SuggestedN:    row.SuggestedN,
		IsSlow:        row.IsSlow,
		UpdatedAt:     row.UpdatedAt,
	}
}

func (a *Assessor) unknown(ss source.SubSource) Profile {
	return Profile{SubSource: ss, Friendly: Unknown, SuggestedN: 1, IsSlow: false}
}

func (a *Assessor) expired(updatedAt int64) bool {
	if a.cfg.ProfileMaxAgeSec <= 0 {
		return false // 未配置上限：画像不过期
	}
	return nowSec()-updatedAt > a.cfg.ProfileMaxAgeSec
}

// RecordSample 被动回灌：追加样本，重算带宽档位/慢源标记/建议并发度。
func (a *Assessor) RecordSample(ss source.SubSource, throughputMbps float64) {
	key := ss.Key()
	_ = a.st.AppendSample(key, throughputMbps)
	samples, _ := a.st.GetSamples(key, a.cfg.ProfileMaxSamples)
	bw := median(samples)
	prev, _, _ := a.st.GetProfile(key)
	friendly := Friendliness(prev.Friendly)
	if friendly == "" {
		friendly = Unknown
	}
	suggestedN := a.suggestedN(friendly)
	isSlow := bw < a.cfg.SlowSourceThresholdMbps
	_ = a.st.SaveProfile(store.ProfileRow{
		SubKey:        key,
		BandwidthMbps: bw,
		Friendly:      string(friendly),
		SuggestedN:    suggestedN,
		IsSlow:        isSlow,
		UpdatedAt:     nowSec(),
	})
}

func (a *Assessor) suggestedN(f Friendliness) int {
	if f == Friendly {
		return a.cfg.DefaultMaxConcurrency
	}
	return 1 // unfriendly/throttled/unknown 都保守单连接
}

// MarkThrottled §3 遇 429 调用：把该子源友好度降级为 throttled，N=1。
func (a *Assessor) MarkThrottled(ss source.SubSource) {
	row, ok, _ := a.st.GetProfile(ss.Key())
	if !ok {
		row = store.ProfileRow{SubKey: ss.Key()}
	}
	row.SubKey = ss.Key()
	row.Friendly = string(Throttled)
	row.SuggestedN = 1
	row.UpdatedAt = nowSec()
	_ = a.st.SaveProfile(row)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64{}, xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// nowSec 默认走 store 注入时钟（与 store.SetClock 同源），保证画像过期判定
// 与样本落库时间一致、可被测试固定。仍为包级变量以便局部替换。
var nowSec = func() int64 { return store.Now() }
