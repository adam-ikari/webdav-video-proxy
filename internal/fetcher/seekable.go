package fetcher

import (
	"io"
	"sync"
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
