package engine

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zdownloader/internal/util"
)

// 程序关闭时运行中的任务按"等待续传"落盘(而非暂停),重启后自动恢复下载。
func TestShutdownPersistsWaitingAndAutoResumes(t *testing.T) {
	payload := bigPayload(32 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeServerHandler(payload)(w, r)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	cfg := testConf(tmp)
	m := NewManager(cfg, filepath.Join(tmp, "config.json"), filepath.Join(tmp, "tasks"))
	if err := os.MkdirAll(filepath.Join(tmp, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.rootCtx = ctx
	m.loadTasks()
	go m.scheduler(ctx)
	go m.ticker(ctx)
	t.Cleanup(cancel) // Shutdown 之后兜底收尾

	views, _ := m.Create(CreateReq{URL: srv.URL + "/slow.bin", Connections: 2, SpeedLimit: 1 << 20}) // 1MB/s:确定性的暂停窗口
	id := views[0].ID
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := m.Get(id)
		if v.Downloaded > 2<<20 && v.Status == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("下载迟迟没有进度: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
	}

	m.Shutdown(context.Background()) // 模拟程序退出

	var rec Task
	if err := util.ReadJSON(m.taskJSONPath(m.get(id)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusWaiting {
		t.Fatalf("关闭后任务应落盘为 waiting(下次启动自动续传),实为 %s", rec.Status)
	}

	// 重启:新 Manager 同目录载入,任务应自动恢复为 running
	m2 := NewManager(cfg, filepath.Join(tmp, "config.json"), filepath.Join(tmp, "tasks"))
	ctx2, cancel2 := context.WithCancel(context.Background())
	m2.rootCtx = ctx2
	m2.loadTasks()
	go m2.scheduler(ctx2)
	m2.kick() // 与生产 Run() 一致:唤醒调度器接续排队的任务
	t.Cleanup(func() {
		cancel2()
		m2.wg.Wait() // 等运行协程退出,避免与 TempDir 清理竞争写文件
	})
	waitStatus(t, m2, id, StatusRunning, 5*time.Second)
}

// 旧版本任务记录没有 arrived 字段:loadTasks 应从位图估算真实落盘量,
// 避免重启后暂停态任务显示累计接收量(可能超过总量)。
func TestLoadTasksEstimatesArrivedFromBitfield(t *testing.T) {
	tmp := t.TempDir()
	tasksDir := filepath.Join(tmp, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const piecesTotal = 10
	const size = 10 << 20
	b := make([]byte, (piecesTotal+7)/8)
	b[0] = 0b11111100 // MSB-first:前 6 个 piece 已完成
	b[1] = 0b00000000
	rec := map[string]any{
		"id":          "bt_arrived1",
		"kind":        "bt",
		"url":         "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"infoHash":    "0123456789abcdef0123456789abcdef01234567",
		"status":      "paused",
		"filename":    "a.bin",
		"dir":         tmp,
		"size":        size,
		"downloaded":  99 << 20, // 累计接收量超过总量:旧版显示 bug 的来源
		"piecesTotal": piecesTotal,
		"bitfield":    base64.StdEncoding.EncodeToString(b),
	}
	if err := util.WriteJSONAtomic(filepath.Join(tasksDir, "bt_arrived1.json"), rec); err != nil {
		t.Fatal(err)
	}
	m := NewManager(testConf(tmp), filepath.Join(tmp, "config.json"), tasksDir)
	m.loadTasks()
	m.mu.Lock()
	tk := m.tasks["bt_arrived1"]
	m.mu.Unlock()
	if tk == nil {
		t.Fatal("任务未载入")
	}
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if want := int64(size) * 6 / piecesTotal; tk.arrived != want {
		t.Fatalf("arrived 应按位图估算为 %d,实为 %d", want, tk.arrived)
	}
}
