package cache

import (
	"context"
	"time"

	"github.com/gem/webdav-proxy/internal/store"
)

// StartEvictor 启后台淘汰 worker。达高水位削到低水位；跳过被引用的块；
// 每 60s 也扫一次 TTL 过期块（即使未达水位）。
func (c *Cache) StartEvictor(ctx context.Context) {
	go func() {
		ttlTicker := time.NewTicker(60 * time.Second)
		defer ttlTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.evictCh:
				c.evictOnce()
				c.evictExpired()
			case <-ttlTicker.C:
				c.evictExpired()
			}
		}
	}()
}

// evictOnce 按高/低水位做 LRU 淘汰。引用中的块跳过（check+delete 在同一锁内，无 TOCTOU）。
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
			if c.tryEvict(r.BlockKey) {
				deleted++
				if c.TotalSize() < low {
					break
				}
			}
		}
		if deleted == 0 {
			return // 全被 pin，本轮无法继续
		}
	}
}

// evictExpired 按 CacheTTL 淘汰过期块。TTL<=0 视为不过期。
func (c *Cache) evictExpired() {
	if c.cfg.CacheTTL <= 0 {
		return
	}
	cutoff := store.Now() - c.cfg.CacheTTL
	for {
		rows, err := c.st.ListExpiredBlocks(cutoff, 32)
		if err != nil || len(rows) == 0 {
			return
		}
		deleted := 0
		for _, r := range rows {
			if c.tryEvict(r.BlockKey) {
				deleted++
			}
		}
		if deleted == 0 {
			return
		}
	}
}

// tryEvict 原子地检查引用计数并删除：在 refsMu 内检查 pin，仍为 0 则立即删除，
// 删除期间持锁以防 Acquire 在 check 与 delete 之间插入（关闭 TOCTOU）。
func (c *Cache) tryEvict(bk store.BlockKey) bool {
	c.refsMu.Lock()
	defer c.refsMu.Unlock()
	if c.refs[refKeyFromStore(bk)] > 0 {
		return false // 引用中
	}
	if err := c.st.DeleteBlock(bk); err != nil {
		return false
	}
	return true
}
