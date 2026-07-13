package fetcher

import (
	"io"
	"sync"
	"time"
)

// orderedReader 把乱序到达的块按字节偏移顺序吐出。
type orderedReader struct {
	mu      sync.Mutex
	cond    *sync.Cond
	nextOff int64 // 下一个待吐字节偏移
	endOff  int64
	blocks  map[int64][]byte // off -> data（块大小 = blockSize，第一块可能不足）
	blkSize int64
	closed  bool
	err     error
	waitFor time.Duration // 单块等待上限，超时则 fail（防 worker 静默丢失导致永久阻塞）
}

func newOrderedReader(start, end, blockSize int64) *orderedReader {
	r := &orderedReader{
		nextOff: start,
		endOff:  end,
		blocks:  map[int64][]byte{},
		blkSize: blockSize,
		waitFor: 30 * time.Second,
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

// waitForBlock 等待下一块到达，带超时。返回是否继续（true=已通知重试，false=超时需 fail）。
// 基于 cond 的超时等待：Go 的 sync.Cond 无原生超时，用后台定时器广播唤醒。
func (r *orderedReader) waitForBlock() bool {
	if r.waitFor <= 0 {
		r.cond.Wait()
		return true
	}
	done := make(chan struct{})
	timer := time.AfterFunc(r.waitFor, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
		close(done)
	})
	defer timer.Stop()
	r.cond.Wait()
	select {
	case <-done:
		return false // 超时
	default:
		return true
	}
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
			if !r.waitForBlock() {
				// 超时：该块迟迟未到，判定为 worker 丢失，降级报错让上层重试。
				if r.err == nil {
					r.err = io.ErrUnexpectedEOF
				}
				return 0, r.err
			}
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
