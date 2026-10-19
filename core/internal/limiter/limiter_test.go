package limiter

import (
	"context"
	"testing"
	"time"
)

func TestLimiterRate(t *testing.T) {
	l := New(1 << 20) // 1MB/s
	start := time.Now()
	buf := make([]byte, 1<<20)
	for i := 0; i < 3; i++ { // 3MB / 1MBps ≈ 2s
		if err := l.Wait(context.Background(), int64(len(buf))); err != nil {
			t.Fatal(err)
		}
	}
	el := time.Since(start)
	if el < 1500*time.Millisecond {
		t.Fatalf("限速未生效: 3MB 只用了 %v", el)
	}
	if el > 3500*time.Millisecond {
		t.Fatalf("限速过慢: %v", el)
	}
}

func TestLimiterUnlimited(t *testing.T) {
	l := New(0)
	start := time.Now()
	for i := 0; i < 100; i++ {
		_ = l.Wait(context.Background(), 1<<20)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("不限速模式不应等待")
	}
}

func TestLimiterCancel(t *testing.T) {
	l := New(1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := l.Wait(ctx, 1<<30); err == nil {
		t.Fatal("取消后应返回错误")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("取消响应过慢")
	}
}

// 突发容量被限制在 1/16 秒配额:超额消费必须阻塞而不是立即通过。
func TestBurstCapBounded(t *testing.T) {
	l := New(8000) // 8KB/s → 突发上限 500B
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, 4000); err == nil {
		t.Fatal("4KB 消费不应立即通过(突发容量应为 1/16 秒=500B)")
	}
}
