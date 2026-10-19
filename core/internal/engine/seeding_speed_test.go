package engine

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zdownloader/internal/bt"
)

// TestSeedingTransitionSpeed 完成态转入做种态的速度口径:
// 转换瞬间(goSeeding bump 后、首个测速 tick 前)任务不得带着上一会话
// 冻结的旧速度;做种期上报速度必须与真实上传增量一致(不虚报尖峰)。
func TestSeedingTransitionSpeed(t *testing.T) {
	root := t.TempDir()
	data := make([]byte, 3_000_000)
	for i := range data {
		data[i] = byte(i * 11)
	}
	if err := os.WriteFile(filepath.Join(root, "s.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err := bt.BuildTorrentBytes(meta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, root, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	cfg := testConf(t.TempDir())
	cfg.BT.SeedRatio = 0 // 永久做种:停做种由测试显式驱动,不被策略抢先
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond) // 等 Run 完成启动
	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	// 等会话客户端就绪后直接注入 seed 地址(无 tracker,省去发现环节)
	deadline := time.Now().Add(15 * time.Second)
	for {
		tk := m.get(id)
		tk.mu.Lock()
		cl := tk.btcl
		tk.mu.Unlock()
		if cl != nil {
			if err := cl.AddPeerAddr(seed.ListenAddr()); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("会话客户端未就绪")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 下载完成 → 自动做种
	waitForStatus(t, m, id, StatusSeeding, 60*time.Second)

	// 转换刚发生:此时任务速度必须已归零,不得残留下载期的 EWMA 旧值
	if v := m.get(id).View(); v.Speed != 0 {
		t.Fatalf("转入做种瞬间速度应归零, got %d (残留旧值会闪现突变)", v.Speed)
	}

	// 让一个 leech 从我们的做种会话拉数据(localhost,高速率)
	tk := m.get(id)
	tk.mu.Lock()
	cl := tk.btcl
	tk.mu.Unlock()
	if cl == nil {
		t.Fatal("做种会话不存在")
	}
	leech, err := bt.NewClient(meta, t.TempDir(), bt.ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	if err := leech.AddPeerAddr(cl.ListenAddr()); err != nil {
		t.Fatal(err)
	}

	// 100ms 采样 12s:上报速度不得超过同期真实上传增量的合理上限
	// (500ms 测速 tick + EWMA ≤ 4×;虚报尖峰倍数远超此值)
	var lastUp int64
	var lastAt time.Time
	maxExcess := float64(0)
	for i := 0; i < 120; i++ {
		v := m.get(id).View()
		now := time.Now()
		if !lastAt.IsZero() {
			if dt := now.Sub(lastAt).Seconds(); dt > 0.05 {
				if realRate := float64(v.Uploaded-lastUp) / dt; realRate > 0 {
					if e := float64(v.Speed) / realRate; e > maxExcess {
						maxExcess = e
					}
				}
				lastUp, lastAt = v.Uploaded, now
			}
		} else {
			lastUp, lastAt = v.Uploaded, now
		}
		time.Sleep(100 * time.Millisecond)
	}
	if maxExcess > 4.0 {
		t.Fatalf("做种期速度虚报: 上报速度峰值达真实增量的 %.1f 倍", maxExcess)
	}
	if leech.Stats().Downloaded == 0 {
		t.Fatal("leech 未拉到数据,测试前提不成立")
	}

	// 停止做种 → 完成:速度必须归零。旧实现冻结在最后一拍的 EWMA 值
	// (实测 completed 任务卡着 1.36MB/s 幽灵速度),下次「重新做种」时
	// goSeeding 先 bump 刷新 UI,冻结旧值闪现一拍,即用户看到的突变
	if err := m.StopSeeding(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	zeroDeadline := time.Now().Add(3 * time.Second)
	for {
		if v := m.get(id).View(); v.Speed == 0 {
			break
		}
		if time.Now().After(zeroDeadline) {
			t.Fatalf("停止做种后速度应归零, got %d", m.get(id).View().Speed)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 重新做种:转换瞬间速度同样必须为 0,不得闪现旧值
	if err := m.ReseedTask(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusSeeding, 30*time.Second)
	if v := m.get(id).View(); v.Speed != 0 {
		t.Fatalf("重新做种瞬间速度应归零, got %d", v.Speed)
	}
}
