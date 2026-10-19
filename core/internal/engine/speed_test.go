package engine

import (
	"path/filepath"
	"testing"

)

// TestSpeedStep 重启恢复后首样本只立基线:cur 里重启前的累计量
// 不得虚报为本次速度(用户可见的"16MB/s 误报")。
func TestSpeedStep(t *testing.T) {
	// 恢复现场:last=0(会话新起)而 cur 含重启前 134MB → 首样本不出速度
	sp, last, primed := speedStep(false, 0, 0, 134<<20)
	if sp != 0 {
		t.Fatalf("首样本应为 0,got %d", sp)
	}
	if last != 134<<20 {
		t.Fatalf("基线应立在 134MB,got %d", last)
	}
	if !primed {
		t.Fatal("首样本后应已立基线")
	}

	// 第二个样本:500ms 内真到货 512KB → 秒速 1MB
	sp, last, _ = speedStep(primed, sp, last, 134<<20+512<<10)
	if sp != 1<<20 {
		t.Fatalf("真实增量秒速应为 1MB,got %d", sp)
	}

	// EWMA 平滑(α=1/4):旧速 8MB/s、增量秒速 2MB/s → 8·¾+2·¼ = 6.5MB/s
	sp, _, _ = speedStep(true, 8<<20, 100<<20, 101<<20)
	if sp != 8<<20-2<<20+512<<10 {
		t.Fatalf("EWMA 应为 6.5MB/s,got %d", sp)
	}

	// 小负增量走 EWMA 衰减:8MB→6MB + (-100B·2)/4 = 6MB-25
	sp, _, _ = speedStep(true, 8<<20, 100, 50)
	if sp != 6<<20-25 {
		t.Fatalf("小负增量应 EWMA 衰减,got %d", sp)
	}
	// 大负增量(计数器回退)→ 速度钳 0
	sp, _, _ = speedStep(true, 1<<20, 1<<20, 0)
	if sp != 0 {
		t.Fatalf("计数器回退应钳 0,got %d", sp)
	}
}

// TestStepTaskSpeedUpload 上传速度采样与状态切换口径:
// 运行态按累计上传量增量独立采样;做种态上传速度即 speed(基线重置后
// 不虚报历史累计);非运行态清零;Stats 按状态分桶汇总。
func TestStepTaskSpeedUpload(t *testing.T) {
	tmp := t.TempDir()
	m := NewManager(testConf(tmp), filepath.Join(tmp, "cfg.json"), filepath.Join(tmp, "tasks"))

	tk := &Task{ID: "t1", Kind: KindBT, Status: StatusRunning}
	m.mu.Lock()
	m.tasks = map[string]*Task{tk.ID: tk}
	m.order = []string{tk.ID}
	m.mu.Unlock()

	// 首样本只立基线:pump 已回写 1MB 累计上传,速度仍为 0
	tk.mu.Lock()
	tk.Uploaded = 1 << 20
	tk.mu.Unlock()
	if !m.stepTaskSpeed(tk) {
		t.Fatal("运行态应视为 active")
	}
	tk.mu.Lock()
	if tk.upSpeed != 0 || !tk.upPrimed || tk.lastUp != 1<<20 {
		t.Fatalf("首样本应只立基线: upSpeed=%d primed=%v lastUp=%d", tk.upSpeed, tk.upPrimed, tk.lastUp)
	}
	tk.mu.Unlock()

	// 500ms 内新增 512KB 上传 → 秒速 1MB
	tk.mu.Lock()
	tk.Uploaded = 1<<20 + 512<<10
	tk.mu.Unlock()
	m.stepTaskSpeed(tk)
	tk.mu.Lock()
	if tk.upSpeed != 1<<20 {
		t.Fatalf("上传秒速应为 1MB, got %d", tk.upSpeed)
	}
	tk.mu.Unlock()

	// 计数器回退(会话重启,Uploaded 缩回 uploadBase)→ 立即钳 0
	tk.mu.Lock()
	tk.Uploaded = 1 << 20
	tk.mu.Unlock()
	m.stepTaskSpeed(tk)
	tk.mu.Lock()
	if tk.upSpeed != 0 {
		t.Fatalf("计数器回退应钳 0, got %d", tk.upSpeed)
	}
	tk.mu.Unlock()

	// 切做种:速度数据源换成 Uploaded,基线重置(goSeeding)后首样本为 0
	tk.mu.Lock()
	tk.Status = StatusSeeding
	tk.speed, tk.speedPrimed, tk.lastBytes = 0, false, 0
	tk.Uploaded = 3 << 20
	tk.mu.Unlock()
	m.stepTaskSpeed(tk)
	tk.mu.Lock()
	if tk.upSpeed != 0 {
		t.Fatalf("做种首样本上传速度应为 0, got %d", tk.upSpeed)
	}
	tk.mu.Unlock()
	tk.mu.Lock()
	tk.Uploaded = 3<<20 + 512<<10
	tk.mu.Unlock()
	m.stepTaskSpeed(tk)
	tk.mu.Lock()
	if tk.upSpeed != 1<<20 || tk.speed != 1<<20 {
		t.Fatalf("做种态 upSpeed 应等于 speed=1MB, got %d/%d", tk.upSpeed, tk.speed)
	}
	tk.mu.Unlock()

	// Stats 汇总分桶:做种只进 totalUpSpeed,不进 totalSpeed
	st := m.Stats()
	if st["totalSpeed"] != int64(0) || st["totalUpSpeed"] != int64(1<<20) {
		t.Fatalf("Stats 分桶错误: totalSpeed=%v totalUpSpeed=%v", st["totalSpeed"], st["totalUpSpeed"])
	}

	// 离开运行态:上传速度清零并待重新立基线
	tk.mu.Lock()
	tk.Status = StatusPaused
	tk.mu.Unlock()
	if m.stepTaskSpeed(tk) {
		t.Fatal("暂停态不应视为 active")
	}
	tk.mu.Lock()
	if tk.upSpeed != 0 || tk.upPrimed {
		t.Fatalf("暂停后应清零: upSpeed=%d primed=%v", tk.upSpeed, tk.upPrimed)
	}
	tk.mu.Unlock()
}
