package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveMultiFileDir 已完成的多文件 BT 任务:删除记录和文件时必须递归
// 删除最终目录(旧实现对目录 os.Remove 静默失败,数据删不干净)。
func TestRemoveMultiFileDir(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(testConf(dir), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	if err := os.MkdirAll(m.tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	finalDir := filepath.Join(dir, "Show")
	if err := os.MkdirAll(filepath.Join(finalDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, "sub", "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: "mf1", Kind: KindBT, Status: StatusCompleted, Dir: dir, Filename: "Show"}
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.order = append(m.order, task.ID)
	m.mu.Unlock()
	m.persist(task)

	if err := m.Remove("mf1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(finalDir); !os.IsNotExist(err) {
		t.Fatalf("最终目录未删除: err=%v", err)
	}
	if _, err := os.Stat(m.taskJSONPath(task)); !os.IsNotExist(err) {
		t.Fatalf("任务记录未删除: err=%v", err)
	}
}

// TestRemoveRunningImmediate 运行中删除:任务应从列表与磁盘立即消失,
// 且收尾协程的 persist 不得把记录写回(否则重启后复活)。
func TestRemoveRunningImmediate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(testConf(dir), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	if err := os.MkdirAll(m.tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canceled := false
	task := &Task{ID: "run1", Kind: KindBT, Status: StatusRunning, Dir: dir, Filename: "x.mp4",
		cancel: func() { canceled = true }}
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.order = append(m.order, task.ID)
	m.mu.Unlock()
	m.persist(task)

	if err := m.Remove("run1", false); err != nil {
		t.Fatal(err)
	}
	if !canceled {
		t.Fatal("运行中删除应取消任务上下文")
	}
	if _, ok := m.Get("run1"); ok {
		t.Fatal("任务应立即从列表移除")
	}
	if _, err := os.Stat(m.taskJSONPath(task)); !os.IsNotExist(err) {
		t.Fatalf("任务记录应立即从磁盘删除: err=%v", err)
	}
	// 收尾协程再次 persist 不应复活记录
	m.persist(task)
	if _, err := os.Stat(m.taskJSONPath(task)); !os.IsNotExist(err) {
		t.Fatalf("已删除任务的 persist 复活了记录: err=%v", err)
	}
}

// TestRemoveSeedingKeepsDataByDefault 做种任务仅删除记录时保留最终数据。
func TestRemoveSeedingKeepsDataByDefault(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(testConf(dir), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	if err := os.MkdirAll(m.tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "seed.mp4")
	if err := os.WriteFile(final, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: "sd1", Kind: KindBT, Status: StatusSeeding, Dir: dir, Filename: "seed.mp4"}
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.order = append(m.order, task.ID)
	m.mu.Unlock()
	m.persist(task)

	if err := m.Remove("sd1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("仅删记录时数据应保留: %v", err)
	}
}