// Package limiter 实现基于令牌桶的速率限制,0 表示不限速。
package limiter

import (
	"context"
	"sync"
	"time"
)

type Limiter struct {
	mu     sync.Mutex
	rate   float64 // 字节/秒
	tokens float64
	last   time.Time
}

func New(rate int64) *Limiter {
	if rate < 0 {
		rate = 0
	}
	return &Limiter{rate: float64(rate), tokens: float64(rate) / 16, last: time.Now()}
}

// SetRate 运行时调整速率(字节/秒,0 为不限速)。
func (l *Limiter) SetRate(rate int64) {
	if rate < 0 {
		rate = 0
	}
	l.mu.Lock()
	l.rate = float64(rate)
	l.clampLocked()
	l.last = time.Now()
	l.mu.Unlock()
}

// clampLocked 限制突发容量为 1/16 秒配额:容量越大,空闲后积攒的信用越
// 多,恢复传输时对端短窗口实测速率超出限速值越多(BT 块粒度发送下,
// 1/4 秒配额足以让对端读出 +8% 的"虚高")。1/16 秒 ≈ BT 单块粒度。
// (调用方须已持有 mu)
func (l *Limiter) clampLocked() {
	if l.rate > 0 && l.tokens > l.rate/16 {
		l.tokens = l.rate / 16
	}
}

// Rate 返回当前限速值(字节/秒,0 不限)。
func (l *Limiter) Rate() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

// Refund 归还未消费的预购令牌(先付费后读取的短读补偿)。
func (l *Limiter) Refund(n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	l.tokens += float64(n)
	l.clampLocked()
	l.mu.Unlock()
}

// Wait 阻塞直到消费 n 个令牌。突发容量为 1/4 秒配额;
// 超过容量的请求自动拆成分期消费,平均速率不变、突发不再超标。
func (l *Limiter) Wait(ctx context.Context, n int64) error {
	if n <= 0 {
		return nil
	}
	remaining := float64(n)
	for {
		l.mu.Lock()
		now := time.Now()
		if l.rate > 0 {
			l.tokens += now.Sub(l.last).Seconds() * l.rate
			l.clampLocked()
		}
		l.last = now
		if l.rate <= 0 {
			l.mu.Unlock()
			return nil
		}
		take := remaining
		// 拆分上限必须与 clampLocked 的桶容量一致:容量之外的令牌在
		// deficit 等待期间会被 clamp 吃掉,取更大的 take 将永远无法满足。
		if cap := l.rate / 16; take > cap {
			take = cap
		}
		if l.tokens >= take {
			l.tokens -= take
			remaining -= take
			l.mu.Unlock()
			if remaining <= 0 {
				return nil
			}
			continue
		}
		deficit := (take - l.tokens) / l.rate
		l.mu.Unlock()

		d := time.Duration(deficit * float64(time.Second))
		if d > 100*time.Millisecond {
			d = 100 * time.Millisecond // 分片等待,及时响应 ctx 取消
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}
