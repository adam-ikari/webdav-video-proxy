package preloader

import (
	"testing"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
)

func TestNextEpisodeNaturalSort(t *testing.T) {
	files := []string{"show.S01E01.mkv", "show.S01E03.mkv", "show.S01E02.mkv"}
	got := nextEpisode(files, "show.S01E01.mkv")
	if got != "show.S01E02.mkv" {
		t.Errorf("next = %q, want show.S01E02.mkv", got)
	}
}

func TestNextEpisodeNone(t *testing.T) {
	files := []string{"a.mkv", "b.mkv"}
	if got := nextEpisode(files, "b.mkv"); got != "" {
		t.Errorf("next = %q, want empty", got)
	}
}

func TestOnReadSchedulesPrefetch(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4, DefaultMaxConcurrency: 4}
	p := New(nil, cfg, nil)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	// 不应 panic；无 fetcher 时静默跳过
	p.OnRead(ss, "/m.mkv", "v", 0, 100)
	// 节流：连续调用不应堆积无限任务
	for i := 0; i < 100; i++ {
		p.OnRead(ss, "/m.mkv", "v", int64(i), 100)
	}
	if len(p.pending()) > 64 {
		t.Errorf("pending = %d, should be bounded", len(p.pending()))
	}
}

func TestOnProgressPrefetchNext(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4}
	p := New(nil, cfg, nil)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	// 70% 触发预取下一集（无 fetcher 不 panic）
	p.OnProgress(ss, "/dir", "/dir/current.mkv", 0.7)
}
