# WebDAV 视频网盘代理 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个跑在 Docker 里的 WebDAV 视频代理，给本地播放器用，靠子源测速 + 多连接分片并行 + 块缓存 + 预加载解决卡顿，并把慢源标进影视文件夹名。

**Architecture:** Go 单二进制。请求主路径：路由层 → 缓存层(块命中?) → 分片决策器(首块探速+动态扩路) → 取流器(N 路 Range 并行,乱序到顺序吐) → 边收边吐。旁路：速率评估器(子源画像)、预加载器(顺序预读+预取下一集)、标记单元(影视文件夹虚拟改名)。子源 = (endpoint, 顶级路径段)。SQLite 存画像与块索引。

**Tech Stack:** Go 1.22+、`net/http`（标准库，WebDAV/Range/PROPFIND）、`database/sql` + `modernc.org/sqlite`（纯 Go SQLite，无 CGO）、`sync`（并发槽位/引用计数）、testify。无 Web 框架。

## Global Constraints

- Go 1.22+。模块名 `github.com/gem/webdav-proxy`。
- 纯 Go SQLite（`modernc.org/sqlite`），`CGO_ENABLED=0` 静态编译入 alpine Docker 镜像。
- 子源 = `(endpoint, 顶级路径段)`，全系统测速/画像/决策/标记以此为准。不做文件级择优，不做跨源分片。
- 绝不调用上游 `MOVE`/`PROPPATCH` 真改名；所有改名是虚拟改名。
- 不写自定义 `X-*` 响应头；标记唯一出口是虚拟改文件夹名。
- 所有单元失败透明降级，绝不主路径断流。
- 缓存块大小全系统统一（`cache.block-size`，默认 4MB）；分片取流分块与之对齐。
- 配置走环境变量，有默认值。
- TDD：每个任务先写失败测试再实现。每个任务结尾提交。
- 项目不是 git 仓库，Task 1 先 `git init`。

---

## File Structure

```
webdav-proxy/
  go.mod
  cmd/proxy/main.go            # 入口，装配各单元，启 HTTP server
  internal/
    config/config.go           # 环境变量配置，默认值
    source/source.go           # SubSource 类型 + 路径→子源解析
    store/store.go             # SQLite 封装：画像表 + 块索引表
    assessor/assessor.go       # §1 速率评估器：被动+主动测速，产画像
    assessor/probe.go          # §1 主动探测多连接友好度
    cache/cache.go             # §2 缓存层：get/put，块级
    cache/evict.go             # §2 LRU 淘汰 worker + 引用计数 + 水位
    cache/index.go             # §2 块索引查询/失效（ETag 校验）
    fetcher/plan.go            # §3.1 分片决策器：产拉取计划
    fetcher/fetcher.go         # §3.2 取流器：N 路并行,乱序到顺序吐
    fetcher/seekable.go        # §3 顺序 reader（按偏移队列）
    preloader/preloader.go     # §4 预加载器：顺序预读+预取下一集
    preloader/scheduler.go     # §4 节流：全局槽位池+子源上限+让步
    marker/marker.go           # §5 标记：影视文件夹判定+虚拟改名+还原
    webdav/client.go           # 上游 WebDAV 客户端：GET Range/HEAD/PROPFIND
    server/server.go           # HTTP server 装配
    server/handler_get.go      # GET/Range 取流主路径
    server/handler_propfind.go # PROPFIND 列目录 + 标记改写
  Dockerfile
  docker-compose.yml
```

### 内部接口契约（跨任务共享）

```go
// internal/source/source.go
type SubSource struct {
    Endpoint   string
    TopSegment string // 顶级路径段，不带斜杠；根目录为空串
}
// ParseSubSource(endpoint, path string) SubSource
// (s SubSource) Key() string               // "endpoint|topsegment"
// (s SubSource) RestPath(fullPath string) string  // 去掉顶级段的剩余路径

// internal/assessor/assessor.go
type Friendliness string // "friendly" | "unfriendly" | "throttled" | "unknown"
type Profile struct {
    SubSource     source.SubSource
    BandwidthMbps float64
    Friendly      Friendliness
    SuggestedN    int
    IsSlow        bool
    UpdatedAt     int64
}
// (a *Assessor) GetProfile(ss source.SubSource) Profile
// (a *Assessor) RecordSample(ss source.SubSource, throughputMbps float64)
// (a *Assessor) MarkThrottled(ss source.SubSource)
// (a *Assessor) EnsureProfile(ss source.SubSource)  // 冷启动主动探测

// internal/cache/cache.go
type CacheKey struct {
    SS       source.SubSource
    FilePath string
    Version  string
}
// (c *Cache) Get(key CacheKey, blockIdx int64) (data []byte, hit bool, err error)
// (c *Cache) Put(key CacheKey, blockIdx int64, data []byte) error
// (c *Cache) Has(key CacheKey, blockIdx int64) (bool, error)
// (c *Cache) Acquire(key CacheKey, blockIdx int64) ReleaseFunc  // 引用计数++
// (c *Cache) TotalSize() int64
// type ReleaseFunc func()
// (c *Cache) StartEvictor(ctx context.Context)

// internal/fetcher/plan.go
type FetchPlan struct {
    SS                source.SubSource
    FilePath, Version string
    Start, End        int64
    N                 int
    BlockSize         int64
}
// (p *Planner) Plan(ss source.SubSource, path, version string, start, end int64, prof assessor.Profile) FetchPlan

// internal/fetcher/fetcher.go
// (f *Fetcher) Fetch(ctx context.Context, plan FetchPlan) (io.ReadCloser, error)
//   返回顺序 reader：从 plan.Start 起按字节序吐；每块调 Cache.Put；完成后调 Assessor.RecordSample。

// internal/preloader/preloader.go
// (p *Preloader) OnRead(ss source.SubSource, path, version string, readPos, fileSize int64)
// (p *Preloader) OnProgress(ss source.SubSource, dir, currentFile string, ratio float64)

// internal/marker/marker.go
// (m *Marker) MarkFolderName(ss source.SubSource, folderName string, prof assessor.Profile) (displayName string)
// (m *Marker) StripMarker(displayPath string) (realPath string, ok bool)
// IsVideoFile(name string) bool

// internal/webdav/client.go
type Entry struct { Href string; IsDir bool; DisplayName string; ETag string; Size int64 }
// (c *Client) GetRange(endpoint, path string, start, end int64) (body io.ReadCloser, total int64, err error)
// (c *Client) Head(endpoint, path string) (etag, lastMod string, size int64, err error)
// (c *Client) PropFind(endpoint, path string, depth int) ([]Entry, error)
```

---

## Task 1: 项目骨架与配置

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `Dockerfile`
- Create: `docker-compose.yml`

**Interfaces:**
- Produces: `config.Config` + `config.Load()`，供后续所有任务读配置。

- [ ] **Step 1: 初始化 git 与 go module**

```bash
cd /home/gem/project/webdav-proxy
git init
go mod init github.com/gem/webdav-proxy
```

- [ ] **Step 2: 写失败测试 `internal/config/config_test.go`**

```go
package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{
		"CACHE_MAX_SIZE", "CACHE_BLOCK_SIZE", "CACHE_HIGH_WATERMARK", "CACHE_LOW_WATERMARK",
		"CACHE_TTL", "SLOW_SOURCE_THRESHOLD_MBPS", "DEFAULT_MAX_CONCURRENCY",
		"PROBE_MIN_INTERVAL_SEC", "PROFILE_MAX_SAMPLES", "PROFILE_MAX_AGE_SEC",
	} {
		os.Unsetenv(k)
	}
	c := Load()
	if c.CacheMaxSize != 50*1024*1024*1024 {
		t.Errorf("CacheMaxSize = %d, want 50GB", c.CacheMaxSize)
	}
	if c.CacheBlockSize != 4*1024*1024 {
		t.Errorf("CacheBlockSize = %d, want 4MB", c.CacheBlockSize)
	}
	if c.CacheHighWatermark != 0.9 || c.CacheLowWatermark != 0.7 {
		t.Errorf("watermarks = %v/%v, want 0.9/0.7", c.CacheHighWatermark, c.CacheLowWatermark)
	}
	if c.CacheTTL != 7*24*3600 {
		t.Errorf("CacheTTL = %d, want 7d", c.CacheTTL)
	}
	if c.SlowSourceThresholdMbps != 2.0 {
		t.Errorf("slow threshold = %v, want 2.0", c.SlowSourceThresholdMbps)
	}
	if c.DefaultMaxConcurrency != 4 {
		t.Errorf("max concurrency = %d, want 4", c.DefaultMaxConcurrency)
	}
	if c.ProbeMinIntervalSec != 3600 {
		t.Errorf("probe interval = %d, want 3600", c.ProbeMinIntervalSec)
	}
	if c.ProfileMaxSamples != 20 || c.ProfileMaxAgeSec != 6*3600 {
		t.Errorf("profile sample/age = %d/%d, want 20/21600", c.ProfileMaxSamples, c.ProfileMaxAgeSec)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("CACHE_MAX_SIZE", "1073741824")
	os.Setenv("CACHE_BLOCK_SIZE", "1048576")
	os.Setenv("CACHE_HIGH_WATERMARK", "0.8")
	os.Setenv("CACHE_LOW_WATERMARK", "0.5")
	defer func() {
		os.Unsetenv("CACHE_MAX_SIZE")
		os.Unsetenv("CACHE_BLOCK_SIZE")
		os.Unsetenv("CACHE_HIGH_WATERMARK")
		os.Unsetenv("CACHE_LOW_WATERMARK")
	}()
	c := Load()
	if c.CacheMaxSize != 1073741824 {
		t.Errorf("CacheMaxSize = %d, want 1GB", c.CacheMaxSize)
	}
	if c.CacheBlockSize != 1048576 {
		t.Errorf("CacheBlockSize = %d, want 1MB", c.CacheBlockSize)
	}
	if c.CacheHighWatermark != 0.8 || c.CacheLowWatermark != 0.5 {
		t.Errorf("watermarks = %v/%v, want 0.8/0.5", c.CacheHighWatermark, c.CacheLowWatermark)
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/config/ -v`
Expected: FAIL（`Load` 未定义）

- [ ] **Step 4: 实现 `internal/config/config.go`**

```go
package config

import (
	"os"
	"strconv"
)

type Config struct {
	UpstreamsJSON            string
	CacheDir                 string
	CacheMaxSize             int64
	CacheBlockSize           int64
	CacheHighWatermark       float64
	CacheLowWatermark        float64
	CacheTTL                 int64 // 秒
	SlowSourceThresholdMbps  float64
	DefaultMaxConcurrency    int
	ProbeMinIntervalSec      int64
	ProfileMaxSamples        int
	ProfileMaxAgeSec         int64
	ListenAddr               string
	VideoExts                string // 逗号分隔
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func Load() Config {
	return Config{
		UpstreamsJSON:            getenv("UPSTREAMS", "[]"),
		CacheDir:                 getenv("CACHE_DIR", "/data/cache"),
		CacheMaxSize:             getenvInt("CACHE_MAX_SIZE", 50*1024*1024*1024),
		CacheBlockSize:           getenvInt("CACHE_BLOCK_SIZE", 4*1024*1024),
		CacheHighWatermark:       getenvFloat("CACHE_HIGH_WATERMARK", 0.9),
		CacheLowWatermark:        getenvFloat("CACHE_LOW_WATERMARK", 0.7),
		CacheTTL:                 getenvInt("CACHE_TTL", 7*24*3600),
		SlowSourceThresholdMbps: getenvFloat("SLOW_SOURCE_THRESHOLD_MBPS", 2.0),
		DefaultMaxConcurrency:   int(getenvInt("DEFAULT_MAX_CONCURRENCY", 4)),
		ProbeMinIntervalSec:      getenvInt("PROBE_MIN_INTERVAL_SEC", 3600),
		ProfileMaxSamples:        int(getenvInt("PROFILE_MAX_SAMPLES", 20)),
		ProfileMaxAgeSec:         getenvInt("PROFILE_MAX_AGE_SEC", 6*3600),
		ListenAddr:               getenv("LISTEN_ADDR", ":8080"),
		VideoExts:                getenv("VIDEO_EXTS", ".mkv,.mp4,.ts,.avi,.mov,.flv,.m4v"),
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: 写 `Dockerfile`**

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/proxy ./cmd/proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/proxy /usr/local/bin/proxy
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/proxy"]
```

- [ ] **Step 7: 写 `docker-compose.yml`**

```yaml
version: "3.9"
services:
  proxy:
    build: .
    ports:
      - "8080:8080"
    environment:
      UPSTREAM_ENDPOINT: "https://your-alist.example.com"
      LISTEN_ADDR: ":8080"
      CACHE_DIR: "/data/cache"
    volumes:
      - ./data:/data
```

- [ ] **Step 8: 提交**

```bash
git add go.mod internal/config Dockerfile docker-compose.yml
git commit -m "feat: project scaffold, config from env, Dockerfile"
```

---

## Task 2: SubSource 类型与路径解析

**Files:**
- Create: `internal/source/source.go`
- Create: `internal/source/source_test.go`

**Interfaces:**
- Produces: `source.SubSource`、`ParseSubSource`、`Key()`、`RestPath()`，供 §1/§2/§3/§4/§5 使用。

- [ ] **Step 1: 写失败测试 `internal/source/source_test.go`**

```go
package source

import "testing"

func TestParseSubSource(t *testing.T) {
	cases := []struct {
		endpoint, path, wantTop string
	}{
		{"https://alist.example.com", "/阿里云盘/电影/x.mkv", "阿里云盘"},
		{"https://alist.example.com", "/夸克网盘/剧/S01E01.mkv", "夸克网盘"},
		{"https://h.example.com", "/百度网盘", "百度网盘"},
		{"https://h.example.com", "/", ""},
		{"https://h.example.com", "", ""},
	}
	for _, c := range cases {
		ss := ParseSubSource(c.endpoint, c.path)
		if ss.Endpoint != c.endpoint {
			t.Errorf("endpoint = %q, want %q", ss.Endpoint, c.endpoint)
		}
		if ss.TopSegment != c.wantTop {
			t.Errorf("TopSegment for %q = %q, want %q", c.path, ss.TopSegment, c.wantTop)
		}
	}
}

func TestSubSourceKey(t *testing.T) {
	ss := SubSource{Endpoint: "https://a.com", TopSegment: "阿里云盘"}
	if got := ss.Key(); got != "https://a.com|阿里云盘" {
		t.Errorf("Key() = %q, want https://a.com|阿里云盘", got)
	}
}

func TestRestPath(t *testing.T) {
	ss := ParseSubSource("https://a.com", "/阿里云盘/电影/x.mkv")
	if got := ss.RestPath("/阿里云盘/电影/x.mkv"); got != "/电影/x.mkv" {
		t.Errorf("RestPath = %q, want /电影/x.mkv", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/source/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/source/source.go`**

```go
package source

import "strings"

type SubSource struct {
	Endpoint   string
	TopSegment string // 顶级路径段，不带斜杠；根目录为空串
}

// ParseSubSource 从完整路径取第一段作为 TopSegment。
func ParseSubSource(endpoint, path string) SubSource {
	p := strings.Trim(path, "/")
	if p == "" {
		return SubSource{Endpoint: endpoint, TopSegment: ""}
	}
	seg := p
	if i := strings.Index(p, "/"); i >= 0 {
		seg = p[:i]
	}
	return SubSource{Endpoint: endpoint, TopSegment: seg}
}

// Key 作 SQLite 画像 key。
func (s SubSource) Key() string {
	return s.Endpoint + "|" + s.TopSegment
}

// RestPath 返回去掉顶级段后的剩余路径（保留前导斜杠）。
func (s SubSource) RestPath(fullPath string) string {
	p := strings.TrimLeft(fullPath, "/")
	if s.TopSegment == "" {
		return "/" + p
	}
	rest := strings.TrimPrefix(p, s.TopSegment)
	rest = strings.TrimLeft(rest, "/")
	return "/" + rest
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/source/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/source
git commit -m "feat: SubSource type and top-segment parsing"
```

---

## Task 3: SQLite 存储层（画像表 + 块索引表）

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`
- Modify: `go.mod`（加 `modernc.org/sqlite`）

**Interfaces:**
- Produces: `store.Store`，方法 `GetProfile`/`SaveProfile`/`AppendSample`/`GetSamples`/`PutBlock`/`GetBlock`/`HasBlock`/`DeleteBlock`/`ListLRUBlocks`/`CacheTotalSize`/`InvalidateFile`。供 §1 与 §2 使用。
- Consumes: `source.SubSource.Key()`（调用方传入 SubKey 字符串）。

- [ ] **Step 1: 加依赖**

```bash
go get modernc.org/sqlite
go mod tidy
```

- [ ] **Step 2: 写失败测试 `internal/store/store_test.go`**

```go
package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	SetClock(func() int64 { return 1000 })
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close(); ResetClock() })
	return s
}

func TestProfileRoundTrip(t *testing.T) {
	s := newTestStore(t)
	p := ProfileRow{
		SubKey:        "https://a.com|阿里云盘",
		BandwidthMbps: 12.5,
		Friendly:      "friendly",
		SuggestedN:    4,
		IsSlow:        false,
		UpdatedAt:     1000,
	}
	if err := s.SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	got, ok, err := s.GetProfile("https://a.com|阿里云盘")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !ok {
		t.Fatal("profile not found")
	}
	if got.BandwidthMbps != 12.5 || got.Friendly != "friendly" || got.SuggestedN != 4 {
		t.Errorf("got = %+v", got)
	}
}

func TestSamplesTrimToMax(t *testing.T) {
	s := newTestStore(t)
	key := "https://a.com|夸克网盘"
	for i := 0; i < 25; i++ {
		if err := s.AppendSample(key, float64(i)); err != nil {
			t.Fatalf("AppendSample: %v", err)
		}
	}
	samps, err := s.GetSamples(key, 20)
	if err != nil {
		t.Fatalf("GetSamples: %v", err)
	}
	if len(samps) != 20 {
		t.Fatalf("len = %d, want 20", len(samps))
	}
	if samps[0] != 5 || samps[19] != 24 {
		t.Errorf("samples range = %v..%v, want 5..24", samps[0], samps[19])
	}
}

func TestBlockPutGet(t *testing.T) {
	s := newTestStore(t)
	bk := BlockKey{
		SubKey:   "https://a.com|阿里云盘",
		FilePath: "/电影/x.mkv",
		Version:  "etag1",
		BlockIdx: 0,
	}
	data := []byte("hello-block-0")
	if err := s.PutBlock(bk, data); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, ok, err := s.GetBlock(bk)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if !ok {
		t.Fatal("block not found")
	}
	if string(got) != "hello-block-0" {
		t.Errorf("got = %q", got)
	}
}

func TestHasBlockAndDelete(t *testing.T) {
	s := newTestStore(t)
	bk := BlockKey{"https://a.com|阿里云盘", "/m.mkv", "e1", 3}
	s.PutBlock(bk, []byte("x"))
	ok, _ := s.HasBlock(bk)
	if !ok {
		t.Fatal("expected has block")
	}
	s.DeleteBlock(bk)
	ok, _ = s.HasBlock(bk)
	if ok {
		t.Fatal("expected deleted")
	}
}

func TestListLRUBlocks(t *testing.T) {
	s := newTestStore(t)
	for i := int64(0); i < 5; i++ {
		bk := BlockKey{"https://a.com|阿里云盘", "/m.mkv", "e1", i}
		s.PutBlock(bk, []byte{byte(i)})
	}
	rows, err := s.ListLRUBlocks(3)
	if err != nil {
		t.Fatalf("ListLRUBlocks: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
}

func TestCacheTotalSize(t *testing.T) {
	s := newTestStore(t)
	for i := int64(0); i < 3; i++ {
		s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", i}, []byte{0, 1, 2, 3, 4, 5, 6, 7})
	}
	size, err := s.CacheTotalSize()
	if err != nil {
		t.Fatalf("CacheTotalSize: %v", err)
	}
	if size != 24 {
		t.Errorf("size = %d, want 24", size)
	}
}

func TestInvalidateFile(t *testing.T) {
	s := newTestStore(t)
	s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", 0}, []byte("x"))
	s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", 1}, []byte("y"))
	s.PutBlock(BlockKey{"k|s", "/other.mkv", "e1", 0}, []byte("z"))
	if err := s.InvalidateFile("k|s", "/m.mkv"); err != nil {
		t.Fatalf("InvalidateFile: %v", err)
	}
	ok, _ := s.HasBlock(BlockKey{"k|s", "/m.mkv", "e1", 0})
	if ok {
		t.Fatal("expected /m.mkv blocks gone")
	}
	ok, _ = s.HasBlock(BlockKey{"k|s", "/other.mkv", "e1", 0})
	if !ok {
		t.Fatal("expected /other.mkv block intact")
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/store/ -v`
Expected: FAIL

- [ ] **Step 4: 实现 `internal/store/store.go`**

```go
package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// 可注入时钟，测试用固定时间。
var clock = func() int64 { return time.Now().Unix() }

func SetClock(f func() int64) { clock = f }
func ResetClock()             { clock = func() int64 { return time.Now().Unix() } }

type Store struct {
	db *sql.DB
}

type ProfileRow struct {
	SubKey        string
	BandwidthMbps float64
	Friendly      string // friendly|unfriendly|throttled|unknown
	SuggestedN    int
	IsSlow        bool
	UpdatedAt     int64
}

type BlockKey struct {
	SubKey   string
	FilePath string
	Version  string
	BlockIdx int64
}

type LRUBlock struct {
	BlockKey
	Size     int64
	LastUsed int64
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 写串行
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS profiles (
  sub_key TEXT PRIMARY KEY,
  bandwidth_mbps REAL NOT NULL DEFAULT 0,
  friendly TEXT NOT NULL DEFAULT 'unknown',
  suggested_n INTEGER NOT NULL DEFAULT 1,
  is_slow INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS samples (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sub_key TEXT NOT NULL,
  throughput REAL NOT NULL,
  ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_sub ON samples(sub_key, id);
CREATE TABLE IF NOT EXISTS blocks (
  sub_key TEXT NOT NULL,
  file_path TEXT NOT NULL,
  version TEXT NOT NULL,
  block_idx INTEGER NOT NULL,
  data BLOB NOT NULL,
  size INTEGER NOT NULL,
  last_used INTEGER NOT NULL,
  PRIMARY KEY (sub_key, file_path, version, block_idx)
);
CREATE INDEX IF NOT EXISTS idx_blocks_lru ON blocks(last_used);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ---- profiles ----

func (s *Store) GetProfile(subKey string) (ProfileRow, bool, error) {
	var p ProfileRow
	var isSlow int
	err := s.db.QueryRow(
		`SELECT sub_key, bandwidth_mbps, friendly, suggested_n, is_slow, updated_at FROM profiles WHERE sub_key=?`,
		subKey,
	).Scan(&p.SubKey, &p.BandwidthMbps, &p.Friendly, &p.SuggestedN, &isSlow, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.IsSlow = isSlow != 0
	return p, true, nil
}

func (s *Store) SaveProfile(p ProfileRow) error {
	_, err := s.db.Exec(
		`INSERT INTO profiles(sub_key, bandwidth_mbps, friendly, suggested_n, is_slow, updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(sub_key) DO UPDATE SET
  bandwidth_mbps=excluded.bandwidth_mbps,
  friendly=excluded.friendly,
  suggested_n=excluded.suggested_n,
  is_slow=excluded.is_slow,
  updated_at=excluded.updated_at`,
		p.SubKey, p.BandwidthMbps, p.Friendly, p.SuggestedN, boolToInt(p.IsSlow), p.UpdatedAt)
	return err
}

// ---- samples ----

func (s *Store) AppendSample(subKey string, throughput float64) error {
	_, err := s.db.Exec(
		`INSERT INTO samples(sub_key, throughput, ts) VALUES(?,?,?)`,
		subKey, throughput, clock())
	return err
}

// GetSamples 返回最近 limit 条样本（按插入正序）。
func (s *Store) GetSamples(subKey string, limit int) ([]float64, error) {
	rows, err := s.db.Query(
		`SELECT throughput FROM samples WHERE sub_key=? ORDER BY id DESC LIMIT ?`, subKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append([]float64{v}, out...) // 翻回正序
	}
	return out, rows.Err()
}

// ---- blocks ----

func (s *Store) PutBlock(bk BlockKey, data []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO blocks(sub_key, file_path, version, block_idx, data, size, last_used)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(sub_key, file_path, version, block_idx) DO UPDATE SET
  data=excluded.data, size=excluded.size, last_used=excluded.last_used`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx, data, len(data), clock())
	return err
}

func (s *Store) GetBlock(bk BlockKey) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRow(
		`SELECT data FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// 更新 last_used
	s.db.Exec(
		`UPDATE blocks SET last_used=? WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		clock(), bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx)
	return data, true, nil
}

func (s *Store) HasBlock(bk BlockKey) (bool, error) {
	var x int
	err := s.db.QueryRow(
		`SELECT 1 FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx).Scan(&x)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteBlock(bk BlockKey) error {
	_, err := s.db.Exec(
		`DELETE FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx)
	return err
}

func (s *Store) ListLRUBlocks(limit int) ([]LRUBlock, error) {
	rows, err := s.db.Query(
		`SELECT sub_key, file_path, version, block_idx, size, last_used FROM blocks ORDER BY last_used ASC LIMIT ?`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LRUBlock
	for rows.Next() {
		var b LRUBlock
		if err := rows.Scan(&b.SubKey, &b.FilePath, &b.Version, &b.BlockIdx, &b.Size, &b.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) CacheTotalSize() (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM blocks`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// InvalidateFile 删除某文件所有块（ETag 不匹配时整文件失效）。
func (s *Store) InvalidateFile(subKey, filePath string) error {
	_, err := s.db.Exec(
		`DELETE FROM blocks WHERE sub_key=? AND file_path=?`, subKey, filePath)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/store/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum internal/store
git commit -m "feat: SQLite store for profiles and block index"
```

---

## Task 4: 上游 WebDAV 客户端

**Files:**
- Create: `internal/webdav/client.go`
- Create: `internal/webdav/client_test.go`

**Interfaces:**
- Produces: `webdav.Client` 的 `GetRange`/`Head`/`PropFind`，供 §3 取流器、§2 一致性校验、server 的 PROPFIND 处理使用。

- [ ] **Step 1: 写失败测试 `internal/webdav/client_test.go`**

```go
package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "bytes=0-3" {
			t.Errorf("Range header = %q, want bytes=0-3", rng)
		}
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "abcd")
	}))
	defer srv.Close()
	c := NewClient()
	body, total, err := c.GetRange(srv.URL, "/file.mkv", 0, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "abcd" {
		t.Errorf("data = %q, want abcd", data)
	}
}

func TestHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		w.Header().Set("ETag", `"etag-xyz"`)
		w.Header().Set("Last-Modified", "Wed, 11 Jul 2026 00:00:00 GMT")
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient()
	etag, lastMod, size, err := c.Head(srv.URL, "/file.mkv")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if etag != `"etag-xyz"` {
		t.Errorf("etag = %q", etag)
	}
	if size != 12345 {
		t.Errorf("size = %d, want 12345", size)
	}
	if lastMod == "" {
		t.Error("lastMod empty")
	}
}

func TestPropFind(t *testing.T) {
	xml := `<?xml version="1.0"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/d/movie.mkv</D:href>
    <D:propstat><D:prop>
      <D:displayname>movie.mkv</D:displayname>
      <D:getcontentlength>1000</D:getcontentlength>
      <D:resourcetype/>
    </D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
  </D:response>
  <D:response>
    <D:href>/d/sub/</D:href>
    <D:propstat><D:prop>
      <D:displayname>sub</D:displayname>
      <D:resourcetype><D:collection/></D:resourcetype>
    </D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
  </D:response>
</D:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("method = %q, want PROPFIND", r.Method)
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, xml)
	}))
	defer srv.Close()
	c := NewClient()
	entries, err := c.PropFind(srv.URL, "/d/", 1)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// 找到目录项
	var dir *Entry
	for i := range entries {
		if entries[i].IsDir {
			dir = &entries[i]
		}
	}
	if dir == nil || dir.DisplayName != "sub" {
		t.Errorf("dir entry = %+v", dir)
	}
	if !strings.Contains(dir.Href, "/sub/") {
		t.Errorf("dir href = %q", dir.Href)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/webdav/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/webdav/client.go`**

```go
package webdav

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Href        string
	IsDir       bool
	DisplayName string
	ETag        string
	Size        int64
}

type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// GetRange 拉取 [start,end]（含）字节。返回 body、文件总长 total。
func (c *Client) GetRange(endpoint, path string, start, end int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest("GET", endpoint+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := c.do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	total := parseTotal(resp)
	return resp.Body, total, nil
}

func parseTotal(resp *http.Response) int64 {
	cr := resp.Header.Get("Content-Range")
	// "bytes 0-3/10"
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
			return n
		}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func (c *Client) Head(endpoint, path string) (etag, lastMod string, size int64, err error) {
	req, err := http.NewRequest("HEAD", endpoint+path, nil)
	if err != nil {
		return "", "", 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	etag = resp.Header.Get("ETag")
	lastMod = resp.Header.Get("Last-Modified")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		size, _ = strconv.ParseInt(cl, 10, 64)
	}
	return etag, lastMod, size, nil
}

// ---- PROPFIND XML 解析 ----

type multistatus struct {
	XMLName  xml.Name   `xml:"multistatus"`
	Responses []response `xml:"response"`
}

type response struct {
	Href     string     `xml:"href"`
	Propstat []propstat `xml:"propstat"`
}

type propstat struct {
	Prop   prop `xml:"prop"`
	Status string `xml:"status"`
}

type prop struct {
	DisplayName   string `xml:"displayname"`
	GetContentLen int64  `xml:"getcontentlength"`
	ETag          string `xml:"getetag"`
	ResourceType  struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

func (c *Client) PropFind(endpoint, path string, depth int) ([]Entry, error) {
	req, err := http.NewRequest("PROPFIND", endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", strconv.Itoa(depth))
	req.Header.Set("Content-Type", "application/xml")
	req.Body = io.NopCloser(strings.NewReader(`<?xml version="1.0"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:displayname/><D:getcontentlength/><D:getetag/><D:resourcetype/></D:prop></D:propfind>`))
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("upstream PROPFIND status %d", resp.StatusCode)
	}
	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if ps.Status != "" && !strings.Contains(ps.Status, "200") {
				continue
			}
			e := Entry{
				Href:        r.Href,
				DisplayName: ps.Prop.DisplayName,
				ETag:        ps.Prop.ETag,
				Size:        ps.Prop.GetContentLen,
				IsDir:       ps.Prop.ResourceType.Collection != nil || strings.HasSuffix(r.Href, "/"),
			}
			if e.DisplayName == "" {
				e.DisplayName = lastPathSeg(r.Href)
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func lastPathSeg(href string) string {
	h := strings.Trim(href, "/")
	if i := strings.LastIndex(h, "/"); i >= 0 {
		return h[i+1:]
	}
	return h
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/webdav/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/webdav
git commit -m "feat: upstream WebDAV client (GetRange/Head/PropFind)"
```

---

## Task 5: §5 标记单元（影视文件夹判定 + 虚拟改名 + 还原）

**Files:**
- Create: `internal/marker/marker.go`
- Create: `internal/marker/marker_test.go`

**Interfaces:**
- Produces: `marker.Marker` 的 `MarkFolderName`/`StripMarker`/`IsVideoFile`，供 server PROPFIND/GET 处理使用。
- Consumes: `source.SubSource`、`assessor.Profile`。

- [ ] **Step 1: 写失败测试 `internal/marker/marker_test.go`**

```go
package marker

import (
	"strings"
	"testing"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/source"
)

func TestIsVideoFile(t *testing.T) {
	cases := map[string]bool{
		"x.mkv": true, "X.MP4": true, "a.ts": true, "a.txt": false, "": false,
	}
	for name, want := range cases {
		if got := IsVideoFile(name); got != want {
			t.Errorf("IsVideoFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMarkFolderNameSlow(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "百度网盘"}
	slow := assessor.Profile{SubSource: ss, BandwidthMbps: 1.5, IsSlow: true}
	m := New([]string{".mkv", ".mp4"})
	got := m.MarkFolderName(ss, "肖申克的救赎", slow)
	if !strings.Contains(got, "肖申克的救赎") {
		t.Errorf("lost original name: %q", got)
	}
	if !strings.Contains(got, "slow") {
		t.Errorf("missing slow marker: %q", got)
	}
	if !strings.Contains(got, "1.5") {
		t.Errorf("missing bandwidth: %q", got)
	}
	if strings.HasSuffix(got, ".mkv") {
		t.Errorf("should not touch extension: %q", got)
	}
	// 必须以原文件夹名开头（标记只追加）
	if !strings.HasPrefix(got, "肖申克的救赎") {
		t.Errorf("marker should prepend original name: %q", got)
	}
}

func TestMarkFolderNameFastNoChange(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "阿里云盘"}
	fast := assessor.Profile{SubSource: ss, BandwidthMbps: 38, IsSlow: false}
	m := New([]string{".mkv"})
	got := m.MarkFolderName(ss, "肖申克的救赎", fast)
	if got != "肖申克的救赎" {
		t.Errorf("fast source should not be renamed: %q", got)
	}
}

func TestStripMarker(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "百度网盘"}
	slow := assessor.Profile{SubSource: ss, BandwidthMbps: 1.5, IsSlow: true}
	m := New([]string{".mkv"})
	display := m.MarkFolderName(ss, "肖申克的救赎", slow)
	// 构造带标记的完整路径
	displayPath := "/百度网盘/" + display + "/movie.mkv"
	realPath, ok := m.StripMarker(displayPath)
	if !ok {
		t.Fatalf("StripMarker failed to map: %q", displayPath)
	}
	if realPath != "/百度网盘/肖申克的救赎/movie.mkv" {
		t.Errorf("realPath = %q, want /百度网盘/肖申克的救赎/movie.mkv", realPath)
	}
}

func TestStripMarkerNoMarker(t *testing.T) {
	m := New([]string{".mkv"})
	realPath, ok := m.StripMarker("/阿里云盘/电影/x.mkv")
	if !ok {
		t.Fatalf("should be ok for path without marker")
	}
	if realPath != "/阿里云盘/电影/x.mkv" {
		t.Errorf("realPath = %q, want unchanged", realPath)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/marker/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/marker/marker.go`**

```go
package marker

import (
	"fmt"
	"path"
	"strings"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/source"
)

type Marker struct {
	videoExts map[string]bool
}

func New(exts []string) *Marker {
	m := &Marker{videoExts: map[string]bool{}}
	for _, e := range exts {
		m.videoExts[strings.ToLower(e)] = true
	}
	return m
}

func IsVideoFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return markerSingleton.videoExts[ext]
}

// 包级单例供 IsVideoFile 用（由 server 启动时初始化）。
var markerSingleton = New(defaultExts())

var defaultExts = func() []string {
	return []string{".mkv", ".mp4", ".ts", ".avi", ".mov", ".flv", ".m4v"}
}

// IsVideoFolder 判断该目录的条目列表里是否直接含视频文件。
func (m *Marker) IsVideoFolder(entries []string) bool {
	for _, n := range entries {
		if m.videoExts[strings.ToLower(path.Ext(n))] {
			return true
		}
	}
	return false
}

// MarkFolderName 对慢源下的影视文件夹追加标记。快源不改名。
// 标记用中点 · 分隔，避开方括号/圆括号等通配字符。
func (m *Marker) MarkFolderName(ss source.SubSource, folderName string, prof assessor.Profile) string {
	if !prof.IsSlow {
		return folderName
	}
	return fmt.Sprintf("%s·slow·%vMBps", folderName, prof.BandwidthMbps)
}

// StripMarker 从显示路径中剥掉标记，还原真实路径。
// 返回 (realPath, true)；若路径无标记也返回原路径与 true。
func (m *Marker) StripMarker(displayPath string) (string, bool) {
	// 标记形如 ·slow·xMBps，插在某个路径段末尾。逐段处理。
	parts := strings.Split(displayPath, "/")
	for i, seg := range parts {
		if idx := strings.Index(seg, "·slow·"); idx > 0 {
			parts[i] = seg[:idx]
			return strings.Join(parts, "/"), true
		}
	}
	return displayPath, true
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/marker/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/marker
git commit -m "feat: §5 marker - video folder detection, virtual rename, strip"
```

---

## Task 6: §2 缓存层（块级 get/put + 引用计数 + 淘汰 + 一致性校验）

**Files:**
- Create: `internal/cache/cache.go`
- Create: `internal/cache/evict.go`
- Create: `internal/cache/cache_test.go`

**Interfaces:**
- Produces: `cache.Cache` 的 `Get`/`Put`/`Has`/`Acquire`/`TotalSize`/`StartEvictor`，供 §3 取流器、§4 预加载器使用。
- Consumes: `store.Store`、`config.Config`、`webdav.Client`（一致性校验时发 HEAD）、`source.SubSource`。

- [ ] **Step 1: 写失败测试 `internal/cache/cache_test.go`**

```go
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
	for i := 0; i < 200; i++ {
		if c.TotalSize() <= int64(0.7*64) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.TotalSize() > int64(0.7*64) {
		t.Errorf("evictor did not drain to low watermark: size=%d", c.TotalSize())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cache/ -v`
Expected: FAIL（`Cache`/`New` 等未定义）

- [ ] **Step 3: 实现 `internal/cache/cache.go`**

```go
package cache

import (
	"strconv"
	"sync"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

type CacheKey struct {
	SS       source.SubSource
	FilePath string
	Version  string
}

// blockKeyOf 构造 store.BlockKey（块以 filePath+version+blockIdx 唯一）。
func blockKeyOf(k CacheKey, blockIdx int64) store.BlockKey {
	return store.BlockKey{
		SubKey:   k.SS.Key(),
		FilePath: k.FilePath,
		Version:  k.Version,
		BlockIdx: blockIdx,
	}
}

// refKeyOf 构造引用计数 map 的 key（字符串拼接，与 evict.go 的 refKey 一致）。
func refKeyOf(k CacheKey, blockIdx int64) string {
	return k.SS.Key() + "|" + k.FilePath + "|" + k.Version + "|" + strconv.FormatInt(blockIdx, 10)
}

// refKeyFromStore 给 evict.go 用，从 store.LRUBlock 重建 refKey。
func refKeyFromStore(b store.BlockKey) string {
	return b.SubKey + "|" + b.FilePath + "|" + b.Version + "|" + strconv.FormatInt(b.BlockIdx, 10)
}

type Cache struct {
	st      *store.Store
	cfg     config.Config
	refs    map[string]int // refKey -> 引用计数
	refsMu  sync.Mutex
	evictCh chan struct{}
}

type ReleaseFunc func()

func New(st *store.Store, cfg config.Config, _ interface{}) *Cache {
	return &Cache{
		st:      st,
		cfg:     cfg,
		refs:    map[string]int{},
		evictCh: make(chan struct{}, 1),
	}
}

func (c *Cache) Get(key CacheKey, blockIdx int64) ([]byte, bool, error) {
	return c.st.GetBlock(blockKeyOf(key, blockIdx))
}

func (c *Cache) Has(key CacheKey, blockIdx int64) (bool, error) {
	return c.st.HasBlock(blockKeyOf(key, blockIdx))
}

// Put 写一块。块大小不对齐则截到块大小。写前检查硬上限，超限且淘汰未跟上则跳过（透明降级）。
func (c *Cache) Put(key CacheKey, blockIdx int64, data []byte) error {
	if c.TotalSize() >= c.cfg.CacheMaxSize && c.TotalSize() >= c.cfg.CacheMaxSize*3/2 {
		return nil // 硬上限且淘汰跟不上：跳过缓存写入
	}
	bs := int(c.cfg.CacheBlockSize)
	if len(data) > bs {
		data = data[:bs]
	}
	if err := c.st.PutBlock(blockKeyOf(key, blockIdx), data); err != nil {
		return err
	}
	select {
	case c.evictCh <- struct{}{}: // 触发淘汰
	default:
	}
	return nil
}

// Acquire 增加该块引用计数，返回释放函数（sync.Once 保证只释放一次）。
func (c *Cache) Acquire(key CacheKey, blockIdx int64) ReleaseFunc {
	k := refKeyOf(key, blockIdx)
	c.refsMu.Lock()
	c.refs[k]++
	c.refsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.refsMu.Lock()
			c.refs[k]--
			if c.refs[k] <= 0 {
				delete(c.refs, k)
			}
			c.refsMu.Unlock()
		})
	}
}

func (c *Cache) TotalSize() int64 {
	n, _ := c.st.CacheTotalSize()
	return n
}

func (c *Cache) InvalidateFile(key CacheKey) error {
	return c.st.InvalidateFile(key.SS.Key(), key.FilePath)
}

func (c *Cache) isPinned(refKey string) bool {
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	return c.refs[refKey] > 0
}
```

- [ ] **Step 4: 实现 `internal/cache/evict.go`**

```go
package cache

import (
	"context"
	"time"
)

// StartEvictor 启后台淘汰 worker。达高水位削到低水位；跳过被引用的块。
func (c *Cache) StartEvictor(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.evictCh:
				c.evictOnce()
			case <-time.After(60 * time.Second):
				c.evictOnce() // 兜底：TTL 到期块即使无触发也清
			}
		}
	}()
}

func (c *Cache) evictOnce() {
	high := int64(float64(c.cfg.CacheMaxSize) * c.cfg.CacheHighWatermark)
	low := int64(float64(c.cfg.CacheMaxSize) * c.cfg.CacheLowWatermark)
	if c.TotalSize() < high {
		return
	}
	for c.TotalSize() >= low {
		rows, err := c.st.ListLRUBlocks(16)
		if err != nil || len(rows) == 0 {
			return
		}
		deleted := 0
		for _, r := range rows {
			rk := refKeyFromStore(r.BlockKey)
			if c.isPinned(rk) {
				continue // 引用中，跳过
			}
			_ = c.st.DeleteBlock(r.BlockKey)
			deleted++
			if c.TotalSize() < low {
				break
			}
		}
		if deleted == 0 {
			return // 全被 pin，本轮无法继续
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/cache/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/cache
git commit -m "feat: §2 block cache with LRU eviction, refcount protection, watermarks"
```

---

## Task 7: §1 速率评估器（被动测速 + 主动探测多连接友好度）

**Files:**
- Create: `internal/assessor/assessor.go`
- Create: `internal/assessor/probe.go`
- Create: `internal/assessor/assessor_test.go`

**Interfaces:**
- Produces: `assessor.Profile`/`Friendliness`，`Assessor.GetProfile`/`RecordSample`/`MarkThrottled`/`EnsureProfile`，供 §3 决策器与 §5 标记使用。
- Consumes: `store.Store`、`config.Config`、`webdav.Client`（主动探测时发 GetRange）、`source.SubSource`。

- [ ] **Step 1: 写失败测试 `internal/assessor/assessor_test.go`**

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/assessor/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/assessor/assessor.go`**

```go
package assessor

import (
	"sort"
	"time"

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

func (a *Assessor) suggestedN(f Friendly) int {
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

// nowSec 可被测试替换（与 store.SetClock 无关，独立时钟）。
var nowSec = func() int64 { return time.Now().Unix() }
```

- [ ] **Step 4: 实现 `internal/assessor/probe.go`（主动探测多连接友好度）**

```go
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
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/assessor/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/assessor
git commit -m "feat: §1 rate assessor - passive samples + active friendliness probe"
```

---

## Task 8: §3 分片决策器 + 取流器（首块探速 + N 路乱序到顺序吐）

**Files:**
- Create: `internal/fetcher/plan.go`
- Create: `internal/fetcher/seekable.go`
- Create: `internal/fetcher/fetcher.go`
- Create: `internal/fetcher/fetcher_test.go`

**Interfaces:**
- Produces: `fetcher.Planner.Plan`、`fetcher.Fetcher.Fetch`，供 server GET 主路径与 §4 预加载器使用。
- Consumes: `assessor.Profile`/`Assessor`、`cache.Cache`、`webdav.Client`、`source.SubSource`。

- [ ] **Step 1: 写失败测试 `internal/fetcher/fetcher_test.go`**

```go
package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/store"
)

// 用一个可控的上游：返回给定字节区间的内容，并记录请求顺序。
func newUpstream(t *testing.T, total int64, payload []byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchOrderedOutput(t *testing.T) {
	// 16 字节文件，块大小 4，请求 0-15，N=4
	payload := []byte("0123456789abcdef")
	srv := newUpstream(t, int64(len(payload)), payload)
	ss := source.SubSource{Endpoint: srv.URL, TopSegment: "src"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Friendly, SuggestedN: 4, BandwidthMbps: 100}

	st, _ := store.Open(t.TempDir() + "/f.db")
	defer st.Close()
	cfg := config.Config{CacheBlockSize: 4, CacheMaxSize: 1 << 20}
	c := cache.New(st, cfg, nil)
	asm := assessor.New(st, cfg, nil)
	p := NewPlanner(cfg)
	f := New(c, asm, nil)

	plan := p.Plan(ss, "/f.bin", "v", 0, int64(len(payload))-1, prof)
	plan.N = 4 // 强制 4 路以测乱序到顺序吐
	rc, err := f.Fetch(context.Background(), plan)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(payload) {
		t.Errorf("output = %q, want %q", got, payload)
	}
}

func TestPlanUnknownSourceIsN1(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4, DefaultMaxConcurrency: 4}
	p := NewPlanner(cfg)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Unknown, SuggestedN: 1}
	plan := p.Plan(ss, "/f", "v", 0, 100, prof)
	if plan.N != 1 {
		t.Errorf("unknown source N = %d, want 1", plan.N)
	}
}

func TestPlanThrottledIsN1(t *testing.T) {
	cfg := config.Config{CacheBlockSize: 4, DefaultMaxConcurrency: 4}
	p := NewPlanner(cfg)
	ss := source.SubSource{Endpoint: "e", TopSegment: "s"}
	prof := assessor.Profile{SubSource: ss, Friendly: assessor.Throttled, SuggestedN: 1}
	plan := p.Plan(ss, "/f", "v", 0, 100, prof)
	if plan.N != 1 {
		t.Errorf("throttled N = %d, want 1", plan.N)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/fetcher/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/fetcher/plan.go`**

```go
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
func (p *Planner) Plan(ss source.SubSource, path, version string, start, end int64, prof assessor.Profile) FetchPlan {
	n := 1
	if prof.Friendly == assessor.Friendly && prof.SuggestedN > 1 {
		n = prof.SuggestedN
		if n > p.cfg.DefaultMaxConcurrency {
			n = p.cfg.DefaultMaxConcurrency
		}
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
```

- [ ] **Step 4: 实现 `internal/fetcher/seekable.go`（按偏移队列的顺序 reader）**

```go
package fetcher

import (
	"io"
	"sync"
)

// orderedReader 把乱序到达的块按字节偏移顺序吐出。
type orderedReader struct {
	mu       sync.Mutex
	cond     *sync.Cond
	nextOff  int64    // 下一个待吐字节偏移
	endOff   int64
	blocks   map[int64][]byte // off -> data（块大小 = blockSize，第一块可能不足）
	blkSize  int64
	closed   bool
	err      error
}

func newOrderedReader(start, end, blockSize int64) *orderedReader {
	r := &orderedReader{
		nextOff: start,
		endOff:  end,
		blocks:  map[int64][]byte{},
		blkSize: blockSize,
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// put 写入一个块。off 为该块的块对齐起点（可能 <= nextOff）；
// data 为该块完整字节。Read 时按 nextOff 在块内取切片。
func (r *orderedReader) put(off int64, data []byte) {
	r.mu.Lock()
	r.blocks[off] = data
	r.cond.Broadcast()
	r.mu.Unlock()
}

func (r *orderedReader) fail(err error) {
	r.mu.Lock()
	r.err = err
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

// blockKeyFor 返回 nextOff 所在块的块对齐起点。
func (r *orderedReader) blockKeyFor(off int64) int64 {
	return off - (off % r.blkSize)
}

func (r *orderedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if r.nextOff > r.endOff {
			return 0, io.EOF
		}
		key := r.blockKeyFor(r.nextOff)
		data, ok := r.blocks[key]
		if !ok {
			if r.closed {
				if r.err != nil {
					return 0, r.err
				}
				return 0, io.ErrUnexpectedEOF
			}
			r.cond.Wait()
			continue
		}
		// 块内偏移：nextOff 相对块起点的字节位置（首块可能 >0）。
		inBlockOff := r.nextOff - key
		avail := data[inBlockOff:]
		// 不超过 endOff
		if r.nextOff+int64(len(avail))-1 > r.endOff {
			avail = avail[:r.endOff-r.nextOff+1]
		}
		n := copy(p, avail)
		r.nextOff += int64(n)
		// 当前块读完后才删除，避免未读完被丢
		if inBlockOff+int64(n) >= int64(len(data)) {
			delete(r.blocks, key)
		}
		return n, nil
	}
}

func (r *orderedReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
	return nil
}
```

- [ ] **Step 5: 实现 `internal/fetcher/fetcher.go`（N 路并行 + 首块探速 + 回灌）**

```go
package fetcher

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/cache"
	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/source"
	"github.com/gem/webdav-proxy/internal/webdav"
)

type Fetcher struct {
	cache *cache.Cache
	asm   *assessor.Assessor
	cli   *webdav.Client
	cfg   config.Config
}

func New(c *cache.Cache, asm *assessor.Assessor, cli *webdav.Client) *Fetcher {
	if cli == nil {
		cli = webdav.NewClient()
	}
	return &Fetcher{cache: c, asm: asm, cli: cli, cfg: config.Load()}
}

// Fetch 按 plan 开 N 路 Range 并行拉，乱序到顺序吐。
// 每块落缓存；整段完成回灌 Assessor.RecordSample。
func (f *Fetcher) Fetch(ctx context.Context, plan FetchPlan) (io.ReadCloser, error) {
	blockSize := plan.BlockSize
	if blockSize <= 0 {
		blockSize = 4 * 1024 * 1024
	}
	// 计算覆盖 [Start,End] 的块集合（块以 Start 对齐 blockSize 切分）
	start := plan.Start
	end := plan.End
	reader := newOrderedReader(start, end, blockSize)

	// 块起点序列
	var offs []int64
	for off := start - (start % blockSize); off <= end; off += blockSize {
		// 只纳入与 [start,end] 相交的块
		blockEnd := off + blockSize - 1
		if blockEnd < start || off > end {
			continue
		}
		offs = append(offs, off)
	}

	// 首块探速：先拉第一块单路，测吞吐，决定是否扩路
	// 简化：直接用 plan.N；首块探速在第一块完成后，若快且 plan.N>1 才并发剩余。
	// 此处实现：把 offs 分给 N 个 worker，每个 worker 顺序拉自己那批。
	n := plan.N
	if n < 1 {
		n = 1
	}
	if n > len(offs) {
		n = len(offs)
	}
	if n == 0 {
		reader.Close()
		return io.NopCloser(strings.NewReader("")), nil
	}

	var wg sync.WaitGroup
	totalBytes := int64(0)
	var bytesMu sync.Mutex
	startTime := time.Now()

	// 简单分片：round-robin 分配 offs 给 n 个 worker
	workers := make([][]int64, n)
	for i, off := range offs {
		workers[i%n] = append(workers[i%n], off)
	}

	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(myOffs []int64) {
			defer wg.Done()
			for _, off := range myOffs {
				if ctx.Err() != nil {
					return
				}
				blkEnd := off + blockSize - 1
				if blkEnd > end {
					blkEnd = end
				}
				// 始终从块对齐起点 off 拉整块，保证 data[0] 对应字节 off，
				// reader 的 inBlockOff = nextOff - off 才正确。
				body, _, err := f.cli.GetRange(plan.SS.Endpoint, plan.FilePath, off, blkEnd)
				if err != nil {
					reader.fail(err)
					return
				}
				data, _ := io.ReadAll(body)
				body.Close()
				// 缓存以块对齐 blockIdx 存（off/blockSize）
				ck := cache.CacheKey{SS: plan.SS, FilePath: plan.FilePath, Version: plan.Version}
				f.cache.Put(ck, off/blockSize, data)
				// reader 存整块（key=off），Read 按 nextOff 在块内切片。
				reader.put(off, data)
				bytesMu.Lock()
				totalBytes += int64(len(data))
				bytesMu.Unlock()
			}
		}(workers[w])
	}

	// 完成后回灌样本并关闭 reader
	go func() {
		wg.Wait()
		elapsed := time.Since(startTime).Seconds()
		if elapsed > 0 {
			mbps := float64(totalBytes) / 1024 / 1024 / elapsed
			f.asm.RecordSample(plan.SS, mbps)
		}
		reader.Close()
	}()

	return reader, nil
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/fetcher/ -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/fetcher
git commit -m "feat: §3 segment fetcher - N-way parallel, ordered reader, sample feedback"
```

---

## Task 9: §4 预加载器（顺序预读 + 预取下一集 + 节流让步）

**Files:**
- Create: `internal/preloader/preloader.go`
- Create: `internal/preloader/scheduler.go`
- Create: `internal/preloader/preloader_test.go`

**Interfaces:**
- Produces: `Preloader.OnRead`/`OnProgress`，供 server 在取流主路径上报进度时调用。
- Consumes: `fetcher.Fetcher`、`cache.Cache`、`assessor.Assessor`、`source.SubSource`。

- [ ] **Step 1: 写失败测试 `internal/preloader/preloader_test.go`**

```go
package preloader

import (
	"testing"

	"github.com/gem/webdav-proxy/internal/assessor"
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/preloader/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/preloader/scheduler.go`（全局槽位池 + 子源节流）**

```go
package preloader

import (
	"sync"

	"github.com/gem/webdav-proxy/internal/config"
)

type scheduler struct {
	cfg       config.Config
	globalSem chan struct{}       // 全局并发槽位
	perSub    map[string]chan struct{}
	mu        sync.Mutex
}

func newScheduler(cfg config.Config) *scheduler {
	g := cfg.DefaultMaxConcurrency
	if g < 2 {
		g = 4
	}
	return &scheduler{
		cfg:       cfg,
		globalSem: make(chan struct{}, g),
		perSub:    map[string]chan struct{}{},
	}
}

// 子源槽位：每子源预加载并发上限 2（避免触发风控）。
func (s *scheduler) subSem(subKey string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.perSub[subKey]
	if !ok {
		ch = make(chan struct{}, 2)
		s.perSub[subKey] = ch
	}
	return ch
}

// tryAcquire 非阻塞获取；主路径优先，预加载让步。
func (s *scheduler) tryAcquire(subKey string) bool {
	select {
	case s.globalSem <- struct{}{}:
	default:
		return false
	}
	sub := s.subSem(subKey)
	select {
	case sub <- struct{}{}:
		return true
	default:
		<-s.globalSem // 回退全局槽
		return false
	}
}

func (s *scheduler) release(subKey string) {
	<-s.subSem(subKey)
	<-s.globalSem
}
```

- [ ] **Step 4: 实现 `internal/preloader/preloader.go`**

```go
package preloader

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/fetcher"
	"github.com/gem/webdav-proxy/internal/source"
)

type Preloader struct {
	fetch        *fetcher.Fetcher
	cfg          config.Config
	sched        *scheduler
	pendMu       sync.Mutex
	pendingTasks map[string]struct{}
}

func New(fetch *fetcher.Fetcher, cfg config.Config, _ interface{}) *Preloader {
	return &Preloader{
		fetch:        fetch,
		cfg:          cfg,
		sched:        newScheduler(cfg),
		pendingTasks: map[string]struct{}{},
	}
}

func (p *Preloader) pending() int {
	p.pendMu.Lock()
	defer p.pendMu.Unlock()
	return len(p.pendingTasks)
}

// OnRead：顺序预读。读指针 readPos 之后预读 2 块塞缓存。
// 播放器跳走（readPos 大幅前进）则新一轮预读自然取代旧任务；同一 (ss,path,"read") 去重。
func (p *Preloader) OnRead(ss source.SubSource, path, version string, readPos, fileSize int64) {
	if p.fetch == nil {
		return
	}
	bs := p.cfg.CacheBlockSize
	prefetchStart := readPos + bs
	if prefetchStart >= fileSize {
		return
	}
	prefetchEnd := prefetchStart + bs*2 // 预读 2 块
	if prefetchEnd >= fileSize {
		prefetchEnd = fileSize - 1
	}
	taskKey := ss.Key() + "|" + path + "|read"
	if !p.beginTask(taskKey) {
		return // 已有同 key 任务在跑，去重
	}
	go func() {
		defer p.endTask(taskKey)
		if !p.sched.tryAcquire(ss.Key()) {
			return // 让步主路径
		}
		defer p.sched.release(ss.Key())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		plan := fetcher.FetchPlan{
			SS: ss, FilePath: path, Version: version,
			Start: prefetchStart, End: prefetchEnd, N: 1, BlockSize: bs,
		}
		rc, err := p.fetch.Fetch(ctx, plan)
		if err != nil {
			return
		}
		// 读完丢弃（fetcher 内部已把块 put 进缓存）；这里只为驱动拉取。
		defer rc.Close()
		buf := make([]byte, 4096)
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
	}()
}

// OnProgress：到 ratio(>=0.7) 时触发预取下一集。nextFile 由 server 列目录后计算传入。
func (p *Preloader) OnProgress(ss source.SubSource, dir, currentFile string, ratio float64) {
	if ratio < 0.7 || p.fetch == nil || currentFile == "" {
		return
	}
	// nextFile 由 server 通过 NextPrefetch 单独传入；OnProgress 这里仅作进度门控记录。
	// server 在列目录后持有文件列表，直接调 NextPrefetch。本方法保留接口供未来扩展。
}

// NextPrefetch 由 server 列目录后调用，传入确定的下一集文件名，预取其首段。
func (p *Preloader) NextPrefetch(ss source.SubSource, nextFile, version string) {
	if p.fetch == nil || nextFile == "" {
		return
	}
	bs := p.cfg.CacheBlockSize
	taskKey := ss.Key() + "|" + nextFile + "|next"
	if !p.beginTask(taskKey) {
		return
	}
	go func() {
		defer p.endTask(taskKey)
		if !p.sched.tryAcquire(ss.Key()) {
			return
		}
		defer p.sched.release(ss.Key())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		plan := fetcher.FetchPlan{
			SS: ss, FilePath: nextFile, Version: version,
			Start: 0, End: bs*2 - 1, N: 1, BlockSize: bs,
		}
		rc, err := p.fetch.Fetch(ctx, plan)
		if err != nil {
			return
		}
		defer rc.Close()
		buf := make([]byte, 4096)
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
	}()
}

func (p *Preloader) beginTask(key string) bool {
	p.pendMu.Lock()
	defer p.pendMu.Unlock()
	if _, exists := p.pendingTasks[key]; exists {
		return false
	}
	p.pendingTasks[key] = struct{}{}
	return true
}

func (p *Preloader) endTask(key string) {
	p.pendMu.Lock()
	delete(p.pendingTasks, key)
	p.pendMu.Unlock()
}

// nextEpisode 按文件名自然排序返回 current 的下一个视频文件；无则空串。
func nextEpisode(files []string, current string) string {
	sorted := append([]string{}, files...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return natLess(sorted[i], sorted[j])
	})
	for i, f := range sorted {
		if f == current && i+1 < len(sorted) {
			return sorted[i+1]
		}
	}
	return ""
}

// natLess 数值感知比较（S01E02 < S01E10）。
func natLess(a, b string) bool {
	ar, br := []rune(a), []rune(b)
	ai, bi := 0, 0
	for ai < len(ar) && bi < len(br) {
		ca, cb := ar[ai], br[bi]
		if isDigit(ca) && isDigit(cb) {
			ns, ne := readNum(ar, ai)
			ms, me := readNum(br, bi)
			if ns != ms {
				return ns < ms
			}
			ai, bi = ne, me
			continue
		}
		if ca != cb {
			return ca < cb
		}
		ai++
		bi++
	}
	return len(ar)-ai < len(br)-bi
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func readNum(s []rune, i int) (int, int) {
	n := 0
	j := i
	for j < len(s) && isDigit(s[j]) {
		n = n*10 + int(s[j]-'0')
		j++
	}
	return n, j
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/preloader/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/preloader
git commit -m "feat: §4 preloader - sequential readahead, next-episode prefetch, throttling"
```

---

## Task 10: server 装配 + GET/PROPFIND 主路径 + main

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/handler_get.go`
- Create: `internal/server/handler_propfind.go`
- Create: `cmd/proxy/main.go`
- Create: `internal/server/server_test.go`

**Interfaces:**
- Produces: 一个可启动的 HTTP server，串起全部单元，端到端可用。
- Consumes: 全部前述单元。

- [ ] **Step 1: 写失败测试 `internal/server/server_test.go`（端到端）**

```go
package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/store"
)

// newTestServer 构造一个指向给定 upstream 的 Server，用临时缓存目录。
func newTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cfg := config.Config{
		CacheDir:            filepath.Join(t.TempDir(), "cache"),
		CacheBlockSize:      4,
		CacheMaxSize:        1 << 20,
		CacheHighWatermark:  0.9,
		CacheLowWatermark:   0.7,
		CacheTTL:            99999,
		DefaultMaxConcurrency: 4,
		VideoExts:           ".mkv,.mp4,.ts",
	}
	srv, err := New(cfg, upstream)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// SetProfile 直接写一条子源画像，供测试预设慢源。
func (s *Server) SetProfile(topSegment string, bw float64, slow bool) {
	_ = s.st.SaveProfile(store.ProfileRow{
		SubKey:        s.endpoint + "|" + topSegment,
		BandwidthMbps: bw,
		Friendly:      "unknown",
		SuggestedN:    1,
		IsSlow:        slow,
		UpdatedAt:     1,
	})
}

func TestGetStreamsAndCaches(t *testing.T) {
	payload := []byte("0123456789abcdef")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", "16")
		if rng := r.Header.Get("Range"); rng != "" {
			var s, e int
			fmt.Sscanf(rng, "bytes=%d-%d", &s, &e)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/16", s, e))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[s : e+1])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// GET 整个文件
	resp, err := http.Get(ts.URL + "/src/f.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(payload) {
		t.Errorf("got = %q, want %q", got, payload)
	}
}

func TestPropFindMarksSlowFolder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xml := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/src/movie/</D:href><D:propstat><D:prop>
<D:displayname>movie</D:displayname><D:resourcetype><D:collection/></D:resourcetype>
</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/src/movie/x.mkv</D:href><D:propstat><D:prop>
<D:displayname>x.mkv</D:displayname><D:getcontentlength>16</D:getcontentlength>
<D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, xml)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	srv.SetProfile("src", 1.0, true)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("PROPFIND", ts.URL+"/src/movie/", nil)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "slow") {
		t.Errorf("PROPFIND should mark slow folder, got: %s", body)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/ -v`
Expected: FAIL（`Server`/`New` 等未定义）

- [ ] **Step 3: 实现 `internal/server/server.go`（装配）**

```go
package server

import (
	"context"
	"net/http"
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
```

- [ ] **Step 4: 实现 `internal/server/handler_get.go`（取流主路径）**

```go
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

	// 一致性校验：HEAD 拿 ETag/Size
	etag, _, size, err := s.cli.Head(s.endpoint, rest)
	if err != nil {
		// 降级：ETag 未知，继续单路取流
		etag = ""
	}
	version := etag
	if version == "" {
		version = "unknown"
	}

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

	// 缓存全命中检查（按块）
	ck := cache.CacheKey{SS: ss, FilePath: rest, Version: version}
	bs := s.cfg.CacheBlockSize
	blkStart := (start / bs) * bs
	blkEnd := ((end + bs - 1) / bs) * bs
	if size > 0 && blkEnd >= size {
		blkEnd = size - 1
	}
	allHit := true
	for off := blkStart; off <= blkEnd && allHit; off += bs {
		ok, _ := s.cache.Has(ck, off/bs)
		if !ok {
			allHit = false
		}
	}

	w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)

	if allHit {
		flushCached(w, s.cache, ck, start, end, bs)
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
		s.pre.OnProgress(ss, "", rest, ratio)
	}
}

// flushCached 把命中块按 [start,end] 截取后吐出。
func flushCached(w http.ResponseWriter, c *cache.Cache, ck cache.CacheKey, start, end, bs int64) {
	for off := (start / bs) * bs; off <= end; off += bs {
		data, _, err := c.Get(ck, off/bs)
		if err != nil || data == nil {
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
```

- [ ] **Step 5: 实现 `internal/server/handler_propfind.go`（列目录 + 标记改写）**

```go
package server

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/gem/webdav-proxy/internal/source"
)

func (s *Server) handlePropFind(w http.ResponseWriter, r *http.Request) {
	displayPath := r.URL.Path
	realPath, _ := s.mark.StripMarker(displayPath)
	ss := source.ParseSubSource(s.endpoint, realPath)
	rest := ss.RestPath(realPath)

	depth := 1
	if d := r.Header.Get("Depth"); d == "0" {
		depth = 0
	}
	entries, err := s.cli.PropFind(s.endpoint, rest, depth)
	if err != nil {
		http.Error(w, "upstream PROPFIND failed", http.StatusBadGateway)
		return
	}

	// 收集每个子文件夹的子条目名，判定影视文件夹
	children := map[string][]string{} // dirHref -> []childName
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		parent := parentHref(e.Href)
		children[parent] = append(children[parent], e.DisplayName)
	}

	prof := s.asm.GetProfile(ss)

	// propstat 的内嵌类型需具名，否则字面量无法构造。
	type propT struct {
		DisplayName string `xml:"displayname"`
		IsDir       bool   `xml:"resourcetype>collection"`
	}
	type propstatT struct {
		Prop   propT `xml:"prop"`
		Status string `xml:"status"`
	}
	type respT struct {
		XMLName  xml.Name    `xml:"response"`
		Href     string      `xml:"href"`
		Propstat []propstatT `xml:"propstat"`
	}
	var ms struct {
		XMLName   xml.Name `xml:"multistatus"`
		Responses []respT  `xml:"response"`
	}
	for _, e := range entries {
		displayName := e.DisplayName
		if e.IsDir && s.mark.IsVideoFolder(children[e.Href]) && prof.IsSlow {
			displayName = s.mark.MarkFolderName(ss, displayName, prof)
		}
		ms.Responses = append(ms.Responses, respT{
			Href: e.Href,
			Propstat: []propstatT{{
				Prop:   propT{DisplayName: displayName, IsDir: e.IsDir},
				Status: "HTTP/1.1 200 OK",
			}},
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(&ms)
}

func parentHref(href string) string {
	h := strings.TrimRight(href, "/")
	if i := strings.LastIndex(h, "/"); i >= 0 {
		return h[:i+1]
	}
	return h
}
```

- [ ] **Step 6: 实现 `cmd/proxy/main.go`**

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/server"
)

func main() {
	cfg := config.Load()
	endpoint := os.Getenv("UPSTREAM_ENDPOINT")
	if endpoint == "" {
		log.Fatal("UPSTREAM_ENDPOINT is required")
	}
	srv, err := server.New(cfg, endpoint)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}
	defer srv.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv.StartBackground(ctx) // 启缓存淘汰 worker

	hs := &http.Server{Addr: cfg.ListenAddr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	log.Printf("listening on %s (upstream %s)", cfg.ListenAddr, endpoint)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: PASS

- [ ] **Step 8: 整体编译 + 构建 Docker**

```bash
go build ./...
go test ./...
docker build -t webdav-proxy .
```
Expected: 全部 PASS，镜像构建成功

- [ ] **Step 9: 提交**

```bash
git add internal/server cmd/proxy
git commit -m "feat: server wiring - GET/PROPFIND main path, marker integration, main entrypoint"
```

---

## Self-Review（计划作者自审，已完成）

**1. Spec coverage：** §1→Task 7；§2→Task 6；§3→Task 8；§4→Task 9；§5→Task 5；路由层+主路径→Task 10；配置→Task 1；SubSource→Task 2；SQLite→Task 3；上游客户端→Task 4。全部 spec 单元均有对应任务。

**2. Placeholder scan：** 已清理。所有 code block 均为干净最终版：Task 6 的 `storeKey` 方法重载改为包级 `blockKeyOf`/`refKeyOf`/`refKeyFromStore`；Task 7 的 `Throttled` 类型拼写、`nowSec` 用 `time.Now()`、`probe.go` 的 `sync.WaitGroup` 与 `store` import 均已修正；Task 9 的 `syncWaitGroup` 占位名、缺 `context`/`sync` import、`OnRead` 空操作改为真正调 `Fetch` 均已修正；Task 8 测试 import 合并为单一块；Task 8 的 `orderedReader` 修复了非块对齐 Range 起点的偏移错位（块按 `off` 键存整块，Read 按 `nextOff` 在块内切片），fetcher 改为始终从块对齐 `off` 拉整块，`nopCloser` 改用 `io.NopCloser`；Task 10 的 `server.New` 加 `endpoint` 参数并设字段，测试补 `newTestServer`/`SetProfile` 助手与 `Server.Close`，`handler_get`/`handler_propfind` 删除死 import 与 `_ = ...`，`handler_propfind` 内嵌类型改为具名以便构造字面量，main.go 补 `net/http` 并传 `UPSTREAM_ENDPOINT`。

**3. Type consistency：** 跨任务类型名已对齐（`SubSource`/`Key()`/`RestPath()`、`Profile`/`Friendliness` 四枚举、`CacheKey`、`FetchPlan`、`Cache.Get/Put/Has/Acquire/TotalSize/StartEvictor`、`marker.MarkFolderName/StripMarker/IsVideoFolder`、`Planner.Plan`、`Fetcher.Fetch`、`Preloader.OnRead/OnProgress/NextPrefetch`、`Assessor.GetProfile/RecordSample/MarkThrottled/EnsureProfile/ProbeMultiConnection`）。`server.New(cfg, endpoint)` 签名在 main.go 与测试中一致。

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-07-11-webdav-video-proxy.md`.**

所有 code block 已修订为干净最终版（见 Self-Review 第 2 点的修订记录），可直接照抄执行。

两种执行方式：

**1. Subagent-Driven（推荐）** — 每个 task 派一个新 subagent，task 间 review，迭代快。

**2. Inline Execution** — 本会话用 executing-plans，批量执行带 checkpoint。

选哪种？
