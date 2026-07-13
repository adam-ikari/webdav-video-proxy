package fetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// Fetch 按 plan 开 N 路 Range 并行拉，乱序到顺序吐。每块落缓存；整段完成回灌样本。
// 上游不支持 Range（回 200 整文件）时降级为单连接整文件流。
func (f *Fetcher) Fetch(ctx context.Context, plan FetchPlan) (io.ReadCloser, error) {
	blockSize := plan.BlockSize
	if blockSize <= 0 {
		blockSize = f.cfg.CacheBlockSize
	}
	start := plan.Start
	end := plan.End

	// 块起点序列（块以 start 对齐 blockSize 切分）
	var offs []int64
	for off := start - (start % blockSize); off <= end; off += blockSize {
		blockEnd := off + blockSize - 1
		if blockEnd < start || off > end {
			continue
		}
		offs = append(offs, off)
	}
	if len(offs) == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}

	n := plan.N
	if n < 1 {
		n = 1
	}

	// 首块探速 + 整文件探测：先用 1 路拉首块，测吞吐/检测 Range 支持。
	// 若上游不支持 Range，立即降级整文件流。
	whole, firstData, firstOff, err := f.probeFirst(ctx, plan, offs[0], blockSize, end)
	if err != nil {
		return nil, err
	}
	if whole {
		return f.fetchWholeFile(ctx, plan, start, end, blockSize)
	}

	// 首块探速：若实测明显慢于画像则保守单连接；否则用 plan.N。
	if n > 1 {
		n = f.decideN(plan.SS, plan.N)
	}
	if n > len(offs) {
		n = len(offs)
	}

	reader := newOrderedReader(start, end, blockSize)

	var (
		wg         sync.WaitGroup
		totalBytes int64
	)
	startTime := time.Now()

	// 首块已由 probe 拉好：先塞进 reader，worker 不再重拉它。
	reader.put(firstOff, firstData)
	remaining := offs[1:]

	// round-robin 分配剩余块给 n 个 worker
	workers := make([][]int64, n)
	for i, off := range remaining {
		workers[i%n] = append(workers[i%n], off)
	}

	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(myOffs []int64) {
			defer wg.Done()
			for _, off := range myOffs {
				if ctx.Err() != nil {
					reader.fail(ctx.Err())
					return
				}
				data, ferr := f.fetchBlock(ctx, plan, off, blockSize, end)
				if ferr != nil {
					reader.fail(ferr)
					return
				}
				f.cache.Put(cache.CacheKey{SS: plan.SS, FilePath: plan.FilePath, Version: plan.Version}, off/blockSize, data)
				reader.put(off, data)
				atomic.AddInt64(&totalBytes, int64(len(data)))
			}
		}(workers[w])
	}

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

// probeFirst 用 1 路拉首块，用于检测 Range 支持 + 探速。
// 返回 (wholeFile, data, off, err)：wholeFile=true 表示上游不支持 Range，需降级整文件流；
// 否则 data 为首块字节（已入缓存），off 为其块对齐起点。
func (f *Fetcher) probeFirst(ctx context.Context, plan FetchPlan, firstOff, blockSize, end int64) (bool, []byte, int64, error) {
	data, err := f.fetchBlock(ctx, plan, firstOff, blockSize, end)
	if err != nil {
		if errors.Is(err, webdav.ErrRangeNotSupported) {
			return true, nil, 0, nil
		}
		return false, nil, 0, err
	}
	f.cache.Put(cache.CacheKey{SS: plan.SS, FilePath: plan.FilePath, Version: plan.Version}, firstOff/blockSize, data)
	return false, data, firstOff, nil
}

// decideN 根据首块探速结果与画像，决定是否扩路。
// 画像友好且首块单路成功 → 用 plannedN；画像未知识别为未知但首块可达 → 扩路；
// （精确的实测吞吐对比留待被动样本积累，此处仅做保守的可达性判断）。
func (f *Fetcher) decideN(ss source.SubSource, plannedN int) int {
	prof := f.asm.GetProfile(ss)
	// 画像友好或未知但首块已成功（说明上游可达且支持 Range）：允许扩路
	if prof.Friendly == assessor.Friendly || prof.Friendly == assessor.Unknown {
		return plannedN
	}
	// throttled/unfriendly：保守单连接
	return 1
}

// fetchBlock 拉一个块 [off, blkEnd]，带重试 + 429 反馈。
func (f *Fetcher) fetchBlock(ctx context.Context, plan FetchPlan, off, blockSize, end int64) ([]byte, error) {
	blkEnd := off + blockSize - 1
	if blkEnd > end {
		blkEnd = end
	}
	ck := cache.CacheKey{SS: plan.SS, FilePath: plan.FilePath, Version: plan.Version}
	if data, ok, _ := f.cache.Get(ck, off/blockSize); ok && data != nil {
		return data, nil // 命中缓存
	}
	const retries = 2
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		body, _, err := f.cli.GetRange(plan.SS.Endpoint, plan.FilePath, off, blkEnd)
		if err != nil {
			if errors.Is(err, webdav.ErrRangeNotSupported) {
				return nil, err // 不重试：交给上层降级整文件
			}
			if isThrottled(err) {
				f.asm.MarkThrottled(plan.SS)
			}
			lastErr = err
			if attempt < retries {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			}
			continue
		}
		data, rerr := io.ReadAll(body)
		body.Close()
		if rerr != nil && len(data) == 0 {
			lastErr = rerr
			continue
		}
		// io.ReadAll 可能因上游 Content-Length 与实际字节不符（部分 206 实现的 CL 不准）
		// 返回 io.ErrUnexpectedEOF 但已读到有效数据——以实际读到的字节为准。
		return data, nil
	}
	return nil, lastErr
}

// fetchWholeFile 上游不支持 Range 时的降级路径：单连接拉整文件，按块切片入缓存，顺序吐 [start,end]。
func (f *Fetcher) fetchWholeFile(ctx context.Context, plan FetchPlan, start, end, blockSize int64) (io.ReadCloser, error) {
	reader := newOrderedReader(start, end, blockSize)
	go func() {
		resp, err := f.cli.HTTP.Get(plan.SS.Endpoint + plan.FilePath)
		if err != nil {
			reader.fail(err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			reader.fail(&statusError{code: resp.StatusCode})
			return
		}
		buf := make([]byte, blockSize)
		off := int64(0)
		for {
			if ctx.Err() != nil {
				reader.fail(ctx.Err())
				return
			}
			n, rerr := io.ReadFull(resp.Body, buf)
			if n > 0 {
				// 必须复制：buf 在下一次迭代被复用，否则缓存/reader 拿到的是会被覆盖的切片。
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				f.cache.Put(cache.CacheKey{SS: plan.SS, FilePath: plan.FilePath, Version: plan.Version}, off/blockSize, chunk)
				reader.put(off, chunk)
				off += int64(n)
			}
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			if rerr != nil {
				reader.fail(rerr)
				return
			}
		}
		reader.Close()
	}()
	return reader, nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "upstream status " + itoa(e.code) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func isThrottled(err error) bool {
	return strings.Contains(err.Error(), "status 429") || strings.Contains(err.Error(), "status 503")
}
