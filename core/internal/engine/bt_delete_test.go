package engine

import (
	"os"
	"path/filepath"
	"testing"

	"zdownloader/internal/conf"
)

// TestIsGeneratedTorrentProvenance 钉死"生成种子 vs 用户源文件"的分流口径:
// 磁链/种子字节/显式标记一律算生成;路径落在本程序 tasks 目录下也算生成;
// 其余(双击/手动选路径)算用户源文件。
func TestIsGeneratedTorrentProvenance(t *testing.T) {
	tasksDir := "/tmp/adt/tasks"
	cases := []struct {
		name string
		req  CreateReq
		want bool
	}{
		{"magnet", CreateReq{Magnet: "magnet:?xt=urn:btih:ab"}, true},
		{"torrentB64", CreateReq{TorrentB64: "Z3Vlcg=="}, true},
		{"explicitGenerated", CreateReq{TorrentPath: "/x/seed.torrent", TorrentGenerated: true}, true},
		{"archiveInTasksDir", CreateReq{TorrentPath: "/tmp/adt/tasks/001.torrent"}, true},
		{"sourceOutsideTasksDir", CreateReq{TorrentPath: "/home/user/seed.torrent"}, false},
		{"emptyNoFlag", CreateReq{}, false},
	}
	for _, c := range cases {
		if got := isGeneratedTorrent(c.req, tasksDir); got != c.want {
			t.Errorf("%s: isGeneratedTorrent = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// seedFixtures 在临时目录里造一个"可见种子副本"(TorrentFile)和一个"内部元数据
// 存档"(TorrentPath),并构造指向它们的最小 Task(deleteTorrentAfterCreate 直接
// 接受该 *Task,无需注册进 manager)。返回两份路径与该 Task 供断言。
func seedFixtures(t *testing.T, m *Manager, tasksDir string) (visible, archive string, tt *Task) {
	t.Helper()
	vis := filepath.Join(t.TempDir(), "visible.torrent")
	arc := filepath.Join(tasksDir, "001.torrent")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{vis, arc} {
		if err := os.WriteFile(p, []byte("d4:infon12:piece lengthi1048576ee"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tt = &Task{ID: "seedtest", Filename: "种子", TorrentFile: vis, TorrentPath: arc}
	return vis, arc, tt
}

// TestDeleteTorrentAfterCreateProvenance 钉死删除分流:可见副本只在对应开关打开且
// 来源归类正确时删除;内部存档永不删。四条路径覆盖生成/源 × 开/关。
func TestDeleteTorrentAfterCreateProvenance(t *testing.T) {
	m := newTestManager(t, func(c *conf.Config) { c.BT.DHT = false })
	m.cfg.BT.DeleteGeneratedTorrent = true
	m.cfg.BT.DeleteSourceTorrent = false
	vis, arc, tt := seedFixtures(t, m, m.tasksDir)
	// 生成种子(TorrentGenerated 显式标记):开关开 → 可见副本删,存档保留
	m.deleteTorrentAfterCreate(tt, CreateReq{TorrentPath: vis, TorrentGenerated: true})
	if _, err := os.Stat(vis); !os.IsNotExist(err) {
		t.Errorf("生成种子+开关开:可见副本应已删除,实际仍存在 %v", err)
	}
	if _, err := os.Stat(arc); err != nil {
		t.Errorf("内部存档永不删:实际缺失 %v", err)
	}

	// 重建副本,验证源文件(非 tasksDir 下)在未开 DeleteSourceTorrent 时保留
	vis2 := vis
	if err := os.WriteFile(vis2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tt.TorrentFile = vis2
	m.deleteTorrentAfterCreate(tt, CreateReq{TorrentPath: vis2, TorrentGenerated: false})
	if _, err := os.Stat(vis2); err != nil {
		t.Errorf("源文件+DeleteSourceTorrent 关:可见副本应保留,实际缺失 %v", err)
	}

	// 打开 DeleteSourceTorrent 后,同样路径的源文件应被删
	m.cfg.BT.DeleteSourceTorrent = true
	m.deleteTorrentAfterCreate(tt, CreateReq{TorrentPath: vis2, TorrentGenerated: false})
	if _, err := os.Stat(vis2); !os.IsNotExist(err) {
		t.Errorf("源文件+DeleteSourceTorrent 开:可见副本应已删除,实际仍存在 %v", err)
	}
}

// TestDeleteSourceTorrentDefaultOff 默认配置(两开关均关)不删任何种子,钉死
// "默认不删"的安全基线。
func TestDeleteSourceTorrentDefaultOff(t *testing.T) {
	m := newTestManager(t, func(c *conf.Config) { c.BT.DHT = false })
	vis, arc, tt := seedFixtures(t, m, m.tasksDir)
	m.deleteTorrentAfterCreate(tt, CreateReq{TorrentPath: vis, TorrentGenerated: true})
	for _, p := range []string{vis, arc} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("默认双开关关: %s 应保留,实际缺失 %v", filepath.Base(p), err)
		}
	}
}
