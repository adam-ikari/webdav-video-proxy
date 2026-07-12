package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

func newTestCache(t *testing.T) (*Cache, *store.Store) {
	t.Helper()
	store.SetClock(func() int64 { return 1000 })
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close(); store.ResetClock() })
	cfg := config.Config{CacheBlockSize: 8, CacheMaxSize: 64, CacheHighWatermark: 0.9, CacheLowWatermark: 0.7, CacheTTL: 99999}
	c := New(st, cfg, nil)
	return c, st
}

func TestPutGetBlock(t *testing.T) {
	c, _ := newTestCache(t)
	key := CacheKey{SS: source.SubSource{Endpoint: "e", TopSegment: "s"}, FilePath: "/m.mkv", Version: "v"}
	if err := c.Put(key, 0, []byte("block0!!")); err != nil { // 8 bytes = block size
		t.Fatalf("Put: %v", err)
	}
	data, hit, err := c.Get(key, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !hit {
		t.Fatal("expected hit")
	}
	if string(data) != "block0!!" {
		t.Errorf("data = %q", data)
	}
}

func TestMissOnAbsent(t *testing.T) {
	c, _ := newTestCache(t)
	key := CacheKey{SS: source.SubSource{Endpoint: "e", TopSegment: "s"}, FilePath: "/m.mkv", Version: "v"}
	_, hit, _ := c.Get(key, 5)
	if hit {
		t.Fatal("expected miss")
	}
}

func TestVersionMismatchMiss(t *testing.T) {
	c, _ := newTestCache(t)
	key := CacheKey{SS: source.SubSource{Endpoint: "e", TopSegment: "s"}, FilePath: "/m.mkv", Version: "v1"}
	c.Put(key, 0, []byte("block0!!"))
	key2 := key
	key2.Version = "v2"
	_, hit, _ := c.Get(key2, 0)
	if hit {
		t.Fatal("different version should miss")
	}
}

func TestAcquireProtectsFromEviction(t *testing.T) {
	c, _ := newTestCache(t)
	key := CacheKey{SS: source.SubSource{Endpoint: "e", TopSegment: "s"}, FilePath: "/m.mkv", Version: "v"}
	for i := int64(0); i < 8; i++ {
		c.Put(key, i, []byte("xxxxxxxx"))
	}
	rel := c.Acquire(key, 0)
	c.Put(key, 8, []byte("yyyyyyyy")) // 触发淘汰
	ok, _ := c.Has(key, 0)
	if !ok {
		t.Fatal("acquired block was evicted")
	}
	rel()
}

func TestEvictorRespectsWatermarks(t *testing.T) {
	c, _ := newTestCache(t)
	key := CacheKey{SS: source.SubSource{Endpoint: "e", TopSegment: "s"}, FilePath: "/m.mkv", Version: "v"}
	for i := int64(0); i < 8; i++ {
		c.Put(key, i, []byte("xxxxxxxx"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartEvictor(ctx)
	// 高水位 0.9*64≈57，当前 64 已超；淘汰应降到低水位 0.7*64≈44 以下。
	// low 取与 evict.go 相同的截断公式 int64(float64(maxSize)*lowWatermark)；
	// 用变量承载以避免 Go 对非整浮点常量 int64 转换的编译期拒绝。
	var lowWM float64 = 0.7
	low := int64(lowWM * 64)
	for i := 0; i < 200; i++ {
		if c.TotalSize() <= low {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.TotalSize() > low {
		t.Errorf("evictor did not drain to low watermark: size=%d", c.TotalSize())
	}
}
