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

// pending 返回当前在途任务集合的快照，供调用方/测试用 len() 观察节流上限。
func (p *Preloader) pending() map[string]struct{} {
	p.pendMu.Lock()
	defer p.pendMu.Unlock()
	cp := make(map[string]struct{}, len(p.pendingTasks))
	for k := range p.pendingTasks {
		cp[k] = struct{}{}
	}
	return cp
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
