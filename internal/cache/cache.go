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
