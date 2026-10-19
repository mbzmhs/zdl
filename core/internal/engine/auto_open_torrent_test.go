package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zdownloader/internal/bt"
)

// writeSeedToDisk 把一个合法种子文件写进下载目录(模拟 HTTP 引擎下载完成的
// .torrent 产物),返回 (下载目录, 种子路径, 该种子 infohash 的小写十六进制)。
func writeSeedToDisk(t *testing.T) (dl, path, ih string) {
	t.Helper()
	dl = t.TempDir()
	_, torrent, _ := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatalf("解析种子: %v", err)
	}
	ih = strings.ToLower(fmt.Sprintf("%x", meta.InfoHash))
	path = filepath.Join(dl, "dl.torrent")
	if err := os.WriteFile(path, torrent, 0o644); err != nil {
		t.Fatal(err)
	}
	return dl, path, ih
}

// TestAutoOpenTorrentSameSeedConflict 同 InfoHash 已有任务(任意状态)时,HTTP
// 下载完成的 .torrent 不得静默跳过:应触发 OnSeedConflict 交前端弹确认框,由用户
// 决定是否仍要创建,且引擎自身不自动创建重复任务。
func TestAutoOpenTorrentSameSeedConflict(t *testing.T) {
	dl, path, ih := writeSeedToDisk(t)
	// 全局默认下载目录与"种子落盘目录 dl"刻意不同:旧实现取全局默认,若两者相同
	// 则下方断言无法暴露该 bug;取不同目录才能验证"沿用种子自身目录"。
	defaultDir := t.TempDir()
	m := NewManager(testConf(defaultDir), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))

	// 列表里已存在同种子的 BT 任务(完成态;任意状态都触发提醒)
	m.mu.Lock()
	m.tasks["existing"] = &Task{ID: "existing", Kind: KindBT, InfoHash: ih, Status: StatusCompleted, Filename: "x"}
	m.order = append(m.order, "existing")
	m.mu.Unlock()

	var cName, cPath, cDir string
	m.OnSeedConflict = func(name, p, dir string) { cName, cPath, cDir = name, p, dir }

	before := len(m.List())
	m.autoOpenTorrent(&Task{ID: "http", Kind: KindHTTP, Status: StatusCompleted, Dir: dl, Filename: filepath.Base(path)})

	if cPath == "" {
		t.Fatalf("同种子冲突应触发 OnSeedConflict 提醒")
	}
	if cPath != path {
		t.Fatalf("提醒携带的种子路径不符: got=%q want=%q", cPath, path)
	}
	if cDir != dl {
		t.Fatalf("提醒携带的目录不符: got=%q want=%q", cDir, dl)
	}
	if cName == "" {
		t.Fatalf("提醒应携带种子名")
	}
	if c := len(m.List()); c != before {
		t.Fatalf("冲突时引擎不应自动创建任务: before=%d after=%d", before, c)
	}
}

// TestAutoOpenTorrentNoConflictAutoCreates 列表无同种子任务时,下载完成的
// .torrent 照常自动创建 BT 任务,且不触发冲突提醒。
func TestAutoOpenTorrentNoConflictAutoCreates(t *testing.T) {
	dl, path, ih := writeSeedToDisk(t)
	base := t.TempDir()
	tasksDir := filepath.Join(base, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	} // AddBT→archiveTorrent 把种子归档到 tasksDir,须先建好
	// 全局默认目录与种子落盘目录 dl 刻意不同(同上),验证新任务沿用 dl 而非全局默认。
	defaultDir := t.TempDir()
	m := NewManager(testConf(defaultDir), filepath.Join(base, "cfg.json"), tasksDir)

	var fired int
	m.OnSeedConflict = func(_, _, _ string) { fired++ }

	before := len(m.List())
	m.autoOpenTorrent(&Task{ID: "http", Kind: KindHTTP, Status: StatusCompleted, Dir: dl, Filename: filepath.Base(path)})

	if fired != 0 {
		t.Fatalf("无冲突不应触发提醒: fired=%d", fired)
	}
	if c := len(m.List()); c != before+1 {
		t.Fatalf("无冲突应自动创建 1 个任务: before=%d after=%d", before, c)
	}
	found := false
	createdDir := ""
	for _, v := range m.List() {
		if v.Kind == KindBT && strings.EqualFold(v.InfoHash, ih) {
			found = true
			createdDir = v.Dir
		}
	}
	if !found {
		t.Fatalf("应存在新创建的同种子 BT 任务")
	}
	if createdDir != dl {
		t.Fatalf("新任务应沿用种子落盘目录(而非全局默认): got=%q want=%q", createdDir, dl)
	}
}
