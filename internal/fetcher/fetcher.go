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
