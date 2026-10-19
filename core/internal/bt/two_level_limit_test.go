package bt

import (
	"context"
	"testing"
	"time"

	"zdownloader/internal/limiter"
)

// TestTwoLevelLimitsSharedGlobal 两个会话共享同一全局限速器时,总消耗受
// 全局预算约束(而非各会话各自一份):512KB/s 全局,两会话各取 1MB,串行
// 耗时必须接近 4s(1MB×2 ÷ 512KB/s,扣除初始桶信用)。旧实现的退化形态
// (全局值算进每会话自身限速器)会让两次 Wait 瞬间完成。
func TestTwoLevelLimitsSharedGlobal(t *testing.T) {
	gDown := limiter.New(512 * 1024)
	gUp := limiter.New(512 * 1024)
	mk := func() *Client {
		meta := buildHubMeta(t, "limit.bin")
		c, err := NewClient(meta, t.TempDir(), ClientConfig{GlobalDown: gDown, GlobalUp: gUp}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close) // 释放存储句柄,防 Windows TempDir 清理撞文件占用
		return c
	}
	c1, c2 := mk(), mk()

	ctx := context.Background()
	start := time.Now()
	if err := c1.waitDL(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := c2.waitDL(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 3*time.Second {
		t.Fatalf("下载全局预算未共享:两会话各取 1MB 仅耗时 %v(应 ≈4s)", el)
	}

	start = time.Now()
	if err := c1.waitUL(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 1900*time.Millisecond {
		t.Fatalf("上传全局限速未生效:1MB 耗时 %v(应 ≈2s)", el)
	}
}

// TestSetLimitsTaskLevelOnly SetLimits 只动任务级限速器:0 = 不设任务级
// 上限,全局约束不受影响(旧实现 0 直接等于不限速,全局被静默抹掉)。
func TestSetLimitsTaskLevelOnly(t *testing.T) {
	gDown := limiter.New(0) // 全局不限
	meta := buildHubMeta(t, "limit2.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{DownLimit: 64 << 10, UpLimit: 32 << 10, GlobalDown: gDown}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close) // 释放存储句柄,防 Windows TempDir 清理撞文件占用
	if c.dlLim.Rate() != 64<<10 || c.ulLim.Rate() != 32<<10 {
		t.Fatalf("任务级限速初值不符: %d/%d", c.dlLim.Rate(), c.ulLim.Rate())
	}
	c.SetLimits(0, 0)
	if c.dlLim.Rate() != 0 || c.ulLim.Rate() != 0 {
		t.Fatalf("SetLimits 后任务级应为 0: %d/%d", c.dlLim.Rate(), c.ulLim.Rate())
	}
	if gDown.Rate() != 0 {
		t.Fatal("SetLimits 不得改动全局限速器")
	}
	// 任务级 0 但全局受限:waitDL 仍被全局约束
	gDown.SetRate(256 * 1024)
	start := time.Now()
	if err := c.waitDL(context.Background(), 1<<20); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 3500*time.Millisecond {
		t.Fatalf("任务级 0 时全局约束丢失:1MB 耗时 %v(应 ≈4s)", el)
	}
}
