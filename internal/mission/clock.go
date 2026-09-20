package mission

import (
	"sync"
	"time"
)

// SimClock 是可被演练时间头驱动的时钟：一旦设置过模拟时间就以模拟时间为准，
// 未设置时回退真实墙钟。
type SimClock struct {
	mu  sync.RWMutex
	sim *time.Time
}

func NewSimClock() *SimClock { return &SimClock{} }

func (c *SimClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.sim != nil {
		return *c.sim
	}
	return time.Now()
}

// Set 设置演练时间；零值表示回到真实墙钟。
func (c *SimClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.IsZero() {
		c.sim = nil
		return
	}
	c.sim = &t
}

// SimTime 返回当前模拟时间及是否处于演练模式。
func (c *SimClock) SimTime() (time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.sim != nil {
		return *c.sim, true
	}
	return time.Time{}, false
}

// StartSweeper 启动后台巡检，周期性把超时执行权转入处置链。
// 返回停止函数。
func (s *Service) StartSweeper(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Second
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = s.Sweep()
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop); <-done }
}
