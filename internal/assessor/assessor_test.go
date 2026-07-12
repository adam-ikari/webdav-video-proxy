package assessor

import (
	"path/filepath"
	"testing"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

func newAssessor(t *testing.T) (*Assessor, *store.Store) {
	t.Helper()
	store.SetClock(func() int64 { return 1000 })
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close(); store.ResetClock() })
	cfg := config.Config{
		SlowSourceThresholdMbps: 2.0,
		DefaultMaxConcurrency:   4,
		ProbeMinIntervalSec:    3600,
		ProfileMaxSamples:       20,
		ProfileMaxAgeSec:        6 * 3600,
	}
	return New(st, cfg, nil), st
}

func TestGetProfileUnknownWhenAbsent(t *testing.T) {
	a, _ := newAssessor(t)
	ss := source.SubSource{Endpoint: "e", TopSegment: "新源"}
	p := a.GetProfile(ss)
	if p.Friendly != Unknown {
		t.Errorf("Friendly = %v, want unknown", p.Friendly)
	}
	if p.SuggestedN != 1 {
		t.Errorf("SuggestedN = %d, want 1 (conservative)", p.SuggestedN)
	}
}

func TestRecordSampleUpdatesBandwidthAndSlow(t *testing.T) {
	a, _ := newAssessor(t)
	ss := source.SubSource{Endpoint: "e", TopSegment: "百度网盘"}
	// 记几个慢样本
	for i := 0; i < 3; i++ {
		a.RecordSample(ss, 1.0) // 1 MB/s < 2 阈值 → 慢
	}
	p := a.GetProfile(ss)
	if !p.IsSlow {
		t.Errorf("expected slow, got %+v", p)
	}
	if p.SuggestedN != 1 {
		t.Errorf("slow unknown should be N=1, got %d", p.SuggestedN)
	}
}

func TestRecordSampleFastNotSlow(t *testing.T) {
	a, _ := newAssessor(t)
	ss := source.SubSource{Endpoint: "e", TopSegment: "阿里云盘"}
	for i := 0; i < 3; i++ {
		a.RecordSample(ss, 30.0)
	}
	p := a.GetProfile(ss)
	if p.IsSlow {
		t.Errorf("expected not slow, got %+v", p)
	}
}

func TestMarkThrottledSetsFriendly(t *testing.T) {
	a, _ := newAssessor(t)
	ss := source.SubSource{Endpoint: "e", TopSegment: "百度网盘"}
	a.MarkThrottled(ss)
	p := a.GetProfile(ss)
	if p.Friendly != Throttled {
		t.Errorf("Friendly = %v, want throttled", p.Friendly)
	}
	if p.SuggestedN != 1 {
		t.Errorf("throttled should be N=1")
	}
}

func TestProfileExpiresToUnknown(t *testing.T) {
	a, _ := newAssessor(t)
	ss := source.SubSource{Endpoint: "e", TopSegment: "阿里云盘"}
	a.RecordSample(ss, 30.0)
	// 推进时钟超过 ProfileMaxAgeSec
	store.SetClock(func() int64 { return 1000 + 7*3600 })
	p := a.GetProfile(ss)
	if p.Friendly != Unknown {
		t.Errorf("expired profile should be unknown, got %v", p.Friendly)
	}
}
