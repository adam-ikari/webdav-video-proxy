package assessor

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

// EnsureProfile：子源首次出现/被动样本为 0 时，主动探测一次给起步画像。
// 冷启动优先单路探速；多连接对照按 ProbeMinIntervalSec 节流，不周期轰炸。
// 实际探测路径由 server 在有文件时调 ProbeMultiConnection；本方法只确保画像存在。
func (a *Assessor) EnsureProfile(ss source.SubSource) {
	row, ok, _ := a.st.GetProfile(ss.Key())
	if ok && !a.expired(row.UpdatedAt) {
		return // 已有有效画像
	}
	if !a.canProbe(ss.Key()) {
		return // 节流：距上次主动探测不足最小间隔
	}
	a.markProbed(ss.Key())
}

// canProbe 节流：同一子源两次主动探测间隔 >= ProbeMinIntervalSec。
func (a *Assessor) canProbe(subKey string) bool {
	row, ok, _ := a.st.GetProfile(subKey)
	if !ok {
		return true
	}
	return nowSec()-row.UpdatedAt >= a.cfg.ProbeMinIntervalSec
}

func (a *Assessor) markProbed(subKey string) {
	row, ok, _ := a.st.GetProfile(subKey)
	if !ok {
		row = store.ProfileRow{SubKey: subKey}
	}
	row.SubKey = subKey
	row.UpdatedAt = nowSec()
	_ = a.st.SaveProfile(row)
}

// ProbeMultiConnection 对给定路径用 1 路 vs N 路拉同样大小块，判定友好度。
// 返回 (friendly, n1Throughput, nNThroughput)。由 server 在有文件时调用，
// 调用方拿到结果后调 a.st.SaveProfile 落库。
func (a *Assessor) ProbeMultiConnection(ctx context.Context, ss source.SubSource, path string, probeBytes int64) (Friendliness, float64, float64) {
	n1 := a.measureThroughput(ctx, ss, path, probeBytes, 1)
	nN := a.measureThroughput(ctx, ss, path, probeBytes, a.cfg.DefaultMaxConcurrency)
	switch {
	case nN >= n1*1.5:
		return Friendly, n1, nN
	case nN < n1*0.9 || nN == 0:
		return Throttled, n1, nN
	default:
		return Unfriendly, n1, nN
	}
}

// measureThroughput 用 N 路并发拉 probeBytes，返回吞吐 MB/s。
func (a *Assessor) measureThroughput(ctx context.Context, ss source.SubSource, path string, bytes int64, n int) float64 {
	start := time.Now()
	chunk := bytes / int64(n)
	if chunk <= 0 {
		chunk = bytes
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			body, _, err := a.cli.GetRange(ss.Endpoint, path, off, off+chunk-1)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, body)
			body.Close()
		}(int64(i) * chunk)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(bytes) / 1024 / 1024 / elapsed
}
