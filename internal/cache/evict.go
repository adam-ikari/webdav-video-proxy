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
