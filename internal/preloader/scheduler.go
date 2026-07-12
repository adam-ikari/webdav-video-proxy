package preloader

import (
	"sync"

	"github.com/gem/webdav-proxy/internal/config"
)

type scheduler struct {
	cfg       config.Config
	globalSem chan struct{} // 全局并发槽位
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

// subSem 子源槽位：每子源预加载并发上限 2（避免触发风控）。
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
