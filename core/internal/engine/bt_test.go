package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
	"zdownloader/internal/util"
)

// fakeTracker 是最小 HTTP tracker:记录每个 info_hash 的 announce(ip+port),
// 应答 interval=1s 与全部已知 peer(compact)。
func fakeTracker(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	swarm := map[string]map[string]bool{} // infohash -> "ip:port"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ih := fmt.Sprintf("%x", []byte(q.Get("info_hash")))
		port := q.Get("port")
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		key := net.JoinHostPort(ip, port)
		mu.Lock()
		if swarm[ih] == nil {
			swarm[ih] = map[string]bool{}
		}
		swarm[ih][key] = true
		var peers []byte
		for k := range swarm[ih] {
			h, p, _ := net.SplitHostPort(k)
			ip4 := net.ParseIP(h).To4()
			if ip4 == nil {
				continue
			}
			var pn int
			fmt.Sscanf(p, "%d", &pn)
			peers = append(peers, ip4...)
			peers = append(peers, byte(pn>>8), byte(pn))
		}
		mu.Unlock()
		resp := bt.MustEncode(map[string]any{
			"interval":   int64(1),
			"peers":      string(peers),
			"complete":   int64(1),
			"incomplete": int64(0),
		})
		w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedSource 生成源目录与对应种子。
func seedSource(t *testing.T) (root string, torrent []byte, content map[string][]byte) {
	t.Helper()
	root = t.TempDir()
	files := map[string][]byte{
		"doc.txt": []byte("hello bt engine"),
		"sub/bin.dat": func() []byte {
			b := make([]byte, 50000)
			for i := range b {
				b[i] = byte(i * 3)
			}
			return b
		}(),
	}
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, meta, err := bt.BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	// announce 稍后由测试注入(重建 info 不变:直接改 announce 需重打包,这里返回后再生成)
	torrent, err = bt.BuildTorrentBytes(meta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return root, torrent, files
}

func waitForStatus(t *testing.T, m *Manager, id string, st Status, timeout time.Duration) TaskView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := m.Get(id); ok && v.Status == st {
			return v
		}
		time.Sleep(200 * time.Millisecond)
	}
	v, _ := m.Get(id)
	t.Fatalf("任务未在 %v 内到达状态 %s(当前 %s err=%q)", timeout, st, v.Status, v.Err)
	return TaskView{}
}

func TestBTTaskEndToEndWithSeeding(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, files := seedSource(t)

	// 给种子注入 announce
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	infoHash := meta.InfoHash
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err = bt.ParseTorrent(torrent)
	if err != nil || meta.InfoHash != infoHash {
		t.Fatalf("announce 注入失败: %v", err)
	}

	// 做种端:源数据即最终数据
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 下载端:完整 Manager
	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = true
	cfg.BT.SeedRatio = 0 // 永久做种,便于观察 seeding 状态
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond) // 等 Run 完成启动

	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	// 下载完成 → 自动做种
	v := waitForStatus(t, m, id, StatusSeeding, 30*time.Second)
	if v.PiecesTotal == 0 || v.Downloaded != v.Size {
		t.Fatalf("进度异常: %+v", v)
	}
	// 到货字节口径必须同步到 100%(完成落在 pump 两次 tick 之间也不能欠账)
	if v.Arrived != v.Size {
		t.Fatalf("arrived %d != size %d(完成时未补同步)", v.Arrived, v.Size)
	}
	// 内容比对(多文件种子落在 <name>/ 目录)
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dl, v.Filename, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("文件 %s 内容不符", rel)
		}
	}
	// 停止做种 → 已完成
	if err := m.StopSeeding(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	// 测试做种端应统计到为该任务服务的上传量
	if seed.Stats().Uploaded == 0 {
		t.Fatal("源做种端应统计到上传量")
	}

	cancel()
	m.Shutdown(context.Background())
}

// TestBTMagnetViaDHT 端到端:无 tracker、无 x.pe 的磁力任务,
// peer 发现完全经 DHT(做种端宣告 → Manager 侧节点查找)。
func TestBTMagnetViaDHT(t *testing.T) {
	srcRoot, torrent, files := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}

	// 做种端:独立 DHT 节点,启动即宣告
	seedDHT, err := bt.StartDHT(bt.DHTConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer seedDHT.Stop()

	// 下载端 Manager:自带共享 DHT 节点;PublicTrackers 清空确保无 tracker 可用
	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = true
	cfg.BT.SeedRatio = 0
	cfg.BT.PublicTrackers = nil
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	// 两个 DHT 节点互为引导(生产中引导自公共路由节点)。
	// 必须在做种端 Start 之前完成:其首轮宣告依赖路由表非空。
	mgrDHT := m.getDHT()
	if mgrDHT == nil {
		t.Fatal("Manager DHT 未启动")
	}
	var nid [20]byte
	nid[0] = 0x5A
	mgrAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: mgrDHT.Port()}
	seedAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seedDHT.Port()}
	seedDHT.Routing().Insert(nid, mgrAddr)
	mgrDHT.Routing().Insert(nid, seedAddr)

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8, DHT: seedDHT})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 磁力任务(仅 xt,无 tr)
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%x", meta.InfoHash)
	views, errs := m.AddBT(CreateReq{Magnet: magnet})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	// 元数据经 DHT peer 获取 → 下载 → 做种(两轮迭代收敛,负载下留足余量)
	v := waitForStatus(t, m, id, StatusSeeding, 120*time.Second)
	if v.Downloaded != v.Size || v.Size <= 0 {
		t.Fatalf("进度异常: %+v", v)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dl, v.Filename, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("文件 %s 内容不符", rel)
		}
	}
	if seed.Stats().Uploaded == 0 {
		t.Fatal("源做种端应统计到上传量")
	}
	if err := m.StopSeeding(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
}

func TestBTTaskResumeAfterRestart(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, files := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ = bt.ParseTorrent(torrent)

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := t.TempDir()
	tasksDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	cfg := testConf(dl)
	cfg.BT.AutoSeed = false

	// 第一轮:创建任务后立即关停(模拟中断)
	m := NewManager(cfg, cfgPath, tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	time.Sleep(2 * time.Second) // 让它跑一小段
	cancel()
	m.Shutdown(context.Background())

	// 第二轮:同一 tasksDir 重启,任务应自动续传至完成
	m2 := NewManager(cfg, cfgPath, tasksDir)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go m2.Run(ctx2)
	v := waitForStatus(t, m2, id, StatusCompleted, 40*time.Second)

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dl, v.Filename, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("文件 %s 内容不符", rel)
		}
	}
	cancel2()
	m2.Shutdown(context.Background())
}

func TestBTTaskDetailAndTrackers(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ = bt.ParseTorrent(torrent)

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = false
	cfg.BT.PublicTrackers = nil
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	waitForStatus(t, m, id, StatusCompleted, 30*time.Second)

	// 详情:文件进度应与种子文件一一对应且已完成
	d, err := m.TaskDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != len(meta.Files) {
		t.Fatalf("文件数不符: got %d want %d", len(d.Files), len(meta.Files))
	}
	for i, f := range d.Files {
		if f.Size != meta.Files[i].Length || f.Done != f.Size {
			t.Fatalf("文件 %v 进度异常: %+v", f.Path, f)
		}
	}

	// tracker 管理:添加 → 任务级列表更新;移除 → 列表恢复
	v, _ := m.Get(id)
	baseN := len(v.Trackers) // 种子内置 announce 已并入任务级列表
	if ok, err := m.AddTaskTracker(id, "udp://extra.example.com:1337/announce"); err != nil || !ok {
		t.Fatalf("添加失败: ok=%v err=%v", ok, err)
	}
	// 同 host:port 不同 scheme:重复,不再入库
	if ok, err := m.AddTaskTracker(id, "http://extra.example.com:1337/announce"); err != nil || ok {
		t.Fatalf("重复 tracker 应跳过: ok=%v err=%v", ok, err)
	}
	v, _ = m.Get(id)
	if len(v.Trackers) != baseN+1 || v.Trackers[baseN] != "udp://extra.example.com:1337/announce" {
		t.Fatalf("添加后任务 trackers: %v", v.Trackers)
	}
	if err := m.RemoveTaskTracker(id, "udp://extra.example.com:1337/announce"); err != nil {
		t.Fatal(err)
	}
	v, _ = m.Get(id)
	if len(v.Trackers) != baseN {
		t.Fatalf("移除后任务 trackers: %v", v.Trackers)
	}
	// 未运行会话重新通告 → 明确报错但不崩溃
	if err := m.ReannounceTask(id); err == nil {
		t.Fatal("无会话时重新通告应报错")
	}
}

// TestBTFileSelection:多文件种子只选部分文件,完成时所选文件内容完整、
// 未选文件为空,大小/进度按所选口径。
func TestBTFileSelection(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ = bt.ParseTorrent(torrent)

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 只选 doc.txt(小文件);sub/bin.dat(50000 字节)不选
	selected := []string{"doc.txt"}
	var wantSize int64
	for _, f := range meta.Files {
		if strings.Join(f.Path, "/") == selected[0] {
			wantSize = f.Length
		}
	}
	if wantSize == 0 {
		t.Fatal("所选文件未在种子中找到")
	}

	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = false
	cfg.BT.PublicTrackers = nil
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64, Files: selected})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	v := waitForStatus(t, m, id, StatusCompleted, 30*time.Second)
	if v.Size != wantSize || v.Downloaded != wantSize {
		t.Fatalf("大小口径不符: size=%d downloaded=%d want=%d", v.Size, v.Downloaded, wantSize)
	}
	// 到货字节口径:边界 piece(含 doc.txt 与 bin.dat 的首块)整块计入也
	// 不得超过所选大小,完成时必须恰好等于所选口径(实测全量会话残留
	// +边界整块计账会让 457MB 任务显示已下 684MB)
	if v.Arrived != wantSize {
		t.Fatalf("arrived %d != size %d(边界 piece 未按所选口径钳制)", v.Arrived, wantSize)
	}

	// 所选文件内容完整
	got, err := os.ReadFile(filepath.Join(dl, v.Filename, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello bt engine" {
		t.Fatalf("doc.txt 内容不符: %q", got)
	}
	// 详情里的文件进度:所选完成,未选为零
	d, err := m.TaskDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != len(meta.Files) {
		t.Fatalf("详情文件数: %d", len(d.Files))
	}
	for _, f := range d.Files {
		name := strings.Join(f.Path, "/")
		if name == "doc.txt" {
			if f.Done != f.Size || !f.Wanted {
				t.Fatalf("所选文件应完成且为需求: %+v", f)
			}
		}
		if name == "sub/bin.dat" {
			// 与 doc.txt 共享 piece0:按 piece 区间推算会把未选的它误标成
			// 需求(详情页显示"普通"),必须按文件路径精确判定
			if f.Done != 0 || f.Wanted {
				t.Fatalf("未选文件不应有进度/需求: %+v", f)
			}
		}
	}

	// 完成态改选(增选):按新选择重启会话补下未选文件;就地布局下目录
	// 与已有数据全部沿用,不得改名 "(1)" 或重下 doc.txt
	if _, err := m.ChooseBTFiles(id, []string{"doc.txt", "sub/bin.dat"}, false); err != nil {
		t.Fatalf("完成态增选失败: %v", err)
	}
	v2 := waitForStatus(t, m, id, StatusCompleted, 40*time.Second)
	full := int64(len("hello bt engine")) + 50000
	if v2.Size != full || v2.Arrived != full {
		t.Fatalf("增选后大小口径不符: size=%d arrived=%d want=%d", v2.Size, v2.Arrived, full)
	}
	if v2.Filename != v.Filename {
		t.Fatalf("增选后目录被改: %q -> %q", v.Filename, v2.Filename)
	}
	if got, err := os.ReadFile(filepath.Join(dl, v2.Filename, "doc.txt")); err != nil || string(got) != "hello bt engine" {
		t.Fatalf("已有文件被动过: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(dl, v2.Filename, "sub", "bin.dat")); err != nil || len(got) != 50000 {
		filepath.WalkDir(dl, func(path string, d os.DirEntry, err error) error {
			sz := int64(-1)
			if fi, e := d.Info(); e == nil {
				sz = fi.Size()
			}
			t.Logf("WALK %s size=%d err=%v", path, sz, err)
			return nil
		})
		t.Fatalf("新选文件未补下: %v len=%d", err, len(got))
	}
	if _, err := os.Stat(filepath.Join(dl, v2.Filename, "sub", "bin.dat"+bt.IncompleteSuffix)); !os.IsNotExist(err) {
		t.Fatal("完成的文件不应残留 .!zd 标记")
	}

	// 完成态改选(缩选):位图完全覆盖新选择 → 就地改账,不打断完成态
	if _, err := m.ChooseBTFiles(id, []string{"doc.txt"}, false); err != nil {
		t.Fatalf("完成态缩选失败: %v", err)
	}
	v3, ok := m.Get(id)
	if !ok || v3.Status != StatusCompleted {
		t.Fatalf("缩选应保持完成态: %+v", v3)
	}
	if v3.Size != int64(len("hello bt engine")) {
		t.Fatalf("缩选后大小未改账: %d", v3.Size)
	}
}

// TestBTCancelCleansInPlaceData 取消未完成的就地任务:数据目录随任务一起
// 删除,对齐旧临时形态"取消即清理"的语义(就地布局后数据写在最终路径,
// 旧清理删的是已不存在的 .zdlbt,取消会把整个数据目录遗留在下载目录)。
func TestBTCancelCleansInPlaceData(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err = bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = false
	cfg.BT.PublicTrackers = nil
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64, Files: []string{"sub/bin.dat"}})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	folder := filepath.Join(dl, views[0].Filename)
	// 1 B/s 限速:任务必然停在半成品,取消时不存在"已下完"的竞态
	if err := m.Update(id, 1, 0); err != nil {
		t.Fatal(err)
	}
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("大小口径异常: %+v", v)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(folder); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("会话未创建数据目录")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 注入非空续传状态并落盘:验证取消会把它们与数据一起作废
	// (真实场景里 btSaveBitfield 在运行期写入,取消时数据被删但状态残留)
	task := m.get(id)
	task.mu.Lock()
	task.Bitfield = "AAAA"
	task.BlockBits = "BBBB"
	task.mu.Unlock()
	m.persist(task)
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCanceled, 10*time.Second)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			break // 取消即清理 ✓
		}
		if time.Now().After(deadline) {
			t.Fatal("取消后数据目录未删除")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 续传状态必须与数据一起作废:块级位图描述的是已删除的数据,留着的话
	// 重新开始时 RestoreBlocks 会把它当"上个会话所写"标记持有——幻影块
	// 瞬间虚报进度(实测取消重启凭空多出 80MB),零块凑数还触发校验失败重下
	task = m.get(id)
	task.mu.Lock()
	bf, bb := task.Bitfield, task.BlockBits
	task.mu.Unlock()
	if bf != "" || bb != "" {
		t.Fatalf("取消后续传状态未作废: bitfield=%q blockBits=%q", bf, bb)
	}
	var saved struct {
		Bitfield  string `json:"bitfield"`
		BlockBits string `json:"blockBits"`
	}
	raw, err := os.ReadFile(filepath.Join(tasksDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Bitfield != "" || saved.BlockBits != "" {
		t.Fatalf("落盘 JSON 仍残留续传状态: bitfield=%q blockBits=%q", saved.Bitfield, saved.BlockBits)
	}
}

// TestBTTrackersForGating 验证公共 tracker 列表三路径门控:磁力元数据抓取
// 恒带公共列表;下载阶段仅当 autoAddTrackers 开启;私有种子任何路径都不带。
func TestBTTrackersForGating(t *testing.T) {
	const pub = "udp://pub.example:1337/announce"
	m := &Manager{cfg: conf.Config{BT: conf.BTConfig{
		PublicTrackers:  []string{pub},
		AutoAddTrackers: false,
	}}}
	task := &Task{Trackers: []string{"http://task.example/announce"}}

	// 元数据抓取:开关关闭也附带
	if got := m.btTrackersFor(task, nil, true); !slices.Contains(got, pub) {
		t.Fatalf("元数据抓取应附带公共 tracker,got=%v", got)
	}

	// 下载阶段开关关闭:公共列表不进任务,任务级/种子内置保留
	own := &bt.MetaInfo{Announce: "http://own.example/announce"}
	got := m.btTrackersFor(task, own, false)
	if slices.Contains(got, pub) {
		t.Fatalf("开关关闭时不应附带公共 tracker,got=%v", got)
	}
	if !slices.Contains(got, "http://task.example/announce") ||
		!slices.Contains(got, "http://own.example/announce") {
		t.Fatalf("任务级/种子内置 tracker 应保留,got=%v", got)
	}

	// 下载阶段开关开启:非私有任务附带(与种子内置重复时去重)
	m.cfg.BT.PublicTrackers = append(m.cfg.BT.PublicTrackers, "http://own.example/announce")
	got = m.btTrackersFor(task, own, true)
	if !slices.Contains(got, pub) {
		t.Fatalf("开关开启时非私有任务应附带公共 tracker,got=%v", got)
	}
	if n := slices.Index(got, "http://own.example/announce"); n < 0 {
		t.Fatalf("种子内置 tracker 缺失:%v", got)
	} else {
		for _, u := range got[n+1:] {
			if u == "http://own.example/announce" {
				t.Fatalf("重复 tracker 未去重:%v", got)
			}
		}
	}

	// 私有种子:任何路径都不附带
	priv := &bt.MetaInfo{Announce: "http://own.example/announce", Private: true}
	if got := m.btTrackersFor(task, priv, true); slices.Contains(got, pub) {
		t.Fatalf("私有种子不应附带公共 tracker,got=%v", got)
	}

	// 旧配置兼容:缺 autoAddTrackers 字段时 Load 保留默认 true
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"port":1234}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := conf.Load(path, t.TempDir()); !cfg.BT.AutoAddTrackers {
		t.Fatal("旧配置缺 autoAddTrackers 字段时应默认开启")
	}
}

// TestBTCompletedTaskRestartsAsSeeding 回归:已完成(位图满)并做种的任务重启后
// 必须按【原名】直接恢复做种,绝不能被冲突改名成 "xxx (1)" 再从零重下。
// 旧实现先 UniquePath:成品文件存在 → 改名 → 新名字下找不到文件 → 判未完成 →
// 从 0 重下,目录堆满 "(1) (1)…" 副本。
func TestBTCompletedTaskRestartsAsSeeding(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, files := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ = bt.ParseTorrent(torrent)

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := t.TempDir()
	tasksDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	cfg := testConf(dl)
	cfg.BT.AutoSeed = true
	cfg.BT.SeedRatio = 0
	cfg.BT.SeedMinutes = 0 // 永久做种,便于观察状态

	// 第一轮:下载至完成并进入做种
	m := NewManager(cfg, cfgPath, tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	v := waitForStatus(t, m, id, StatusSeeding, 40*time.Second)
	name := v.Filename
	if name == "" {
		t.Fatal("完成任务的文件名缺失")
	}
	cancel()
	m.Shutdown(context.Background())

	// 第二轮:同一 tasksDir 重启,应直接恢复做种而非重下
	m2 := NewManager(cfg, cfgPath, tasksDir)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go m2.Run(ctx2)
	v2 := waitForStatus(t, m2, id, StatusSeeding, 40*time.Second)
	if v2.Filename != name {
		t.Fatalf("重启后文件名被改: %q -> %q(应保持原名)", name, v2.Filename)
	}
	// 内容仍在且未被降级重下
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dl, v2.Filename, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("文件 %s 内容不符", rel)
		}
	}
	// 目录中不应出现 "原名 (1)" 之类的重下副本
	ext := filepath.Ext(name)
	dup := filepath.Join(dl, strings.TrimSuffix(name, ext)+" (1)"+ext)
	if _, err := os.Stat(dup); err == nil {
		t.Fatalf("出现了重下副本 %s", dup)
	}
	cancel2()
	m2.Shutdown(context.Background())
}

// TestBTSubsetCompletedRestartsAsSeeding 回归:文件选择(子集)任务完成后
// 重启,必须与全量任务一样按【原名】直接恢复做种。旧实现 complete 只认
// wanted==nil,子集任务永远不走捷径 → 目录被冲突改名 "(1)" → 已完成的
// 数据被无视,整份重下(实测 BeanSub 子集任务重启后重下 457MB)。
func TestBTSubsetCompletedRestartsAsSeeding(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := seedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	// 注入 announce 后必须重parse:seeder 也要向 tracker 通告,否则下载端无源
	meta, err = bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := t.TempDir()
	tasksDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	cfg := testConf(dl)
	cfg.BT.AutoSeed = true
	cfg.BT.SeedRatio = 0
	cfg.BT.SeedMinutes = 0

	// 第一轮:只选 doc.txt,下载至完成并进入做种
	m := NewManager(cfg, cfgPath, tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64, Files: []string{"doc.txt"}})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	v := waitForStatus(t, m, id, StatusSeeding, 40*time.Second)
	name := v.Filename
	wantSize := v.Size
	if name == "" || wantSize == 0 {
		t.Fatalf("完成任务信息缺失: %+v", v)
	}
	cancel()
	m.Shutdown(context.Background())

	// 第二轮:同一 tasksDir 重启,应按原名直接恢复做种,不重下不改名
	m2 := NewManager(cfg, cfgPath, tasksDir)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go m2.Run(ctx2)
	v2 := waitForStatus(t, m2, id, StatusSeeding, 40*time.Second)
	if v2.Filename != name {
		t.Fatalf("重启后文件名被改: %q -> %q(应保持原名)", name, v2.Filename)
	}
	if v2.Size != wantSize || v2.Arrived != wantSize {
		t.Fatalf("重启后口径异常: size=%d arrived=%d want=%d", v2.Size, v2.Arrived, wantSize)
	}
	dup := filepath.Join(dl, name+" (1)")
	if _, err := os.Stat(dup); err == nil {
		t.Fatalf("出现了重下副本 %s", dup)
	}
	// 所选文件内容仍在
	got, err := os.ReadFile(filepath.Join(dl, name, "doc.txt"))
	if err != nil || string(got) != "hello bt engine" {
		t.Fatalf("所选文件丢失/不符: %v %q", err, got)
	}
	cancel2()
	m2.Shutdown(context.Background())
}

// TestBTChooseFiles 文件选择落定:choosing 确认后立即排队、大小按所选口径;
// paused 态可改选(保持暂停);all=true 恢复全量。
func TestBTChooseFiles(t *testing.T) {
	_, torrent, files := seedSource(t)
	var total int64
	for _, d := range files {
		total += int64(len(d))
	}
	dl := t.TempDir()
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil { // 不跑 Run 循环,目录需自备
		t.Fatal(err)
	}
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), tasksDir)

	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	// 模拟磁力元数据就绪后的挂起态(不经 swarm 抓取)
	tt := m.get(id)
	tt.mu.Lock()
	hasArchive := tt.TorrentPath != ""
	tt.Status = StatusChoosing
	tt.mu.Unlock()
	if !hasArchive {
		t.Fatal("种子任务应有归档")
	}

	// 空选择 / 未知路径必须被拒
	if _, err := m.ChooseBTFiles(id, nil, false); err == nil {
		t.Fatal("空选择应报错")
	}
	if _, err := m.ChooseBTFiles(id, []string{"nope.bin"}, false); err == nil {
		t.Fatal("未知路径应报错")
	}
	// 选择 sub/bin.dat → 立即排队,大小按所选口径
	v, err := m.ChooseBTFiles(id, []string{"sub/bin.dat"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusWaiting {
		t.Fatalf("确认后应排队下载, got %s", v.Status)
	}
	if v.Size != int64(len(files["sub/bin.dat"])) {
		t.Fatalf("所选大小 %d != %d", v.Size, len(files["sub/bin.dat"]))
	}
	// 暂停后改选:保持暂停,口径重算
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	v2, err := m.ChooseBTFiles(id, []string{"doc.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Status != StatusPaused {
		t.Fatalf("暂停态改选应保持暂停, got %s", v2.Status)
	}
	if v2.Size != int64(len(files["doc.txt"])) {
		t.Fatalf("改选大小 %d != %d", v2.Size, len(files["doc.txt"]))
	}
	// all=true 清空选择,恢复全量口径
	v3, err := m.ChooseBTFiles(id, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Status != StatusPaused || v3.Size != total {
		t.Fatalf("全量选择: status=%s size=%d 应为 %d", v3.Status, v3.Size, total)
	}

	// 磁链 choosing 产物的提交路径:按本机种子路径创建任务(带文件选择)
	tt.mu.Lock()
	tpath := tt.TorrentPath
	tt.mu.Unlock()
	views2, errs2 := m.AddBT(CreateReq{TorrentPath: tpath, Files: []string{"doc.txt"}})
	if len(errs2) > 0 {
		t.Fatalf("torrentPath 创建失败: %v", errs2)
	}
	if views2[0].Size != int64(len(files["doc.txt"])) {
		t.Fatalf("torrentPath 任务大小 %d != %d", views2[0].Size, len(files["doc.txt"]))
	}
}

// TestBTHoldForChooseTorrentFile 磁力挂起为 choosing 时,"可见种子副本"直接引用
// 隐藏任务目录里的归档种子(TorrentPath)——不在下载目录落盘,默认下载文件夹
// 保持干净;用户确认目录后由 AddBT 把种子副本复制进所选目录。
func TestBTHoldForChooseTorrentFile(t *testing.T) {
	_, torrent, _ := seedSource(t)
	dl := t.TempDir()
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	tt := &Task{ID: "hold-torrent-1", Kind: KindBT, Dir: dl, Status: StatusRunning, ChoosePending: true}
	// 模拟磁力任务:元数据就绪后归档种子已写入隐藏任务目录(btEnsureMeta→archiveTorrent)
	tp := filepath.Join(tasksDir, tt.ID+".torrent")
	if err := os.WriteFile(tp, torrent, 0o644); err != nil {
		t.Fatal(err)
	}
	tt.TorrentPath = tp
	m.mu.Lock()
	m.tasks[tt.ID] = tt
	m.mu.Unlock()

	if !m.btHoldForChoose(tt, meta, torrent) {
		t.Fatal("多文件磁力应挂起等待创建任务")
	}
	tt.mu.Lock()
	vpath, st := tt.TorrentFile, tt.Status
	tt.mu.Unlock()
	if st != StatusChoosing || vpath != tp {
		t.Fatalf("应挂起并引用隐藏归档作可见副本: status=%s torrentFile=%q want=%q", st, vpath, tp)
	}
	got, err := os.ReadFile(vpath)
	if err != nil {
		t.Fatalf("可见副本未落盘: %v", err)
	}
	if string(got) != string(torrent) {
		t.Fatal("副本内容应与种子原文一致")
	}
	// 下载目录保持干净:choosing 阶段不在 dl 里写任何种子副本
	entries, err := os.ReadDir(dl)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("choosing 阶段下载目录应保持空, got %d 个条目", len(entries))
	}
}

// TestBTHoldForChooseSingleFile 单文件种子的挂起口径:普通"下载前选择"
// (仅 Choose)单文件直接放行;关联启动(Choose+ChooseAny)单文件也挂起
// ——用户至少要能过创建对话框选保存目录。
func TestBTHoldForChooseSingleFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "only.bin"), slices.Repeat([]byte{7}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Files) != 1 {
		t.Fatalf("预置单文件种子, got %d 个文件", len(meta.Files))
	}
	torrent, err := bt.BuildTorrentBytes(meta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dl := t.TempDir()
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))

	plain := &Task{ID: "hold-single-1", Kind: KindBT, Dir: dl, Status: StatusRunning, ChoosePending: true}
	if m.btHoldForChoose(plain, meta, torrent) {
		t.Fatal("单文件 + 仅 Choose 应直接放行(既有语义)")
	}
	assoc := &Task{ID: "hold-single-2", Kind: KindBT, Dir: dl, Status: StatusRunning, ChoosePending: true, HoldAny: true}
	if !m.btHoldForChoose(assoc, meta, torrent) {
		t.Fatal("单文件 + ChooseAny(关联启动)应挂起选目录")
	}
	assoc.mu.Lock()
	st, copyPath := assoc.Status, assoc.TorrentFile
	pendingLeft, holdLeft := assoc.ChoosePending, assoc.HoldAny
	assoc.mu.Unlock()
	if st != StatusChoosing || copyPath == "" {
		t.Fatalf("应挂起为 choosing 并落盘副本: status=%s torrentFile=%q", st, copyPath)
	}
	if pendingLeft || holdLeft {
		t.Fatalf("挂起后标志应清除: choosePending=%v holdAny=%v", pendingLeft, holdLeft)
	}
}

// TestBTHoldForChooseSourceIsLocalFile 种子来源是本机文件(如双击 .torrent
// 打开)时,挂起选文件不再往下载目录写副本:原文件即"可见副本"。此前
// 无条件写副本,种子与下载目录同目录时会产生"同名 (1)"副本(用户实测)。
// 原文件丢失则退回写副本的旧路。
func TestBTHoldForChooseSourceIsLocalFile(t *testing.T) {
	_, torrent, _ := seedSource(t)
	dl := t.TempDir()
	// 来源种子就放在下载目录里(用户场景:双击下载目录里的种子)
	src := filepath.Join(dl, "mine.torrent")
	if err := os.WriteFile(src, torrent, 0o644); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dl)
	if err != nil {
		t.Fatal(err)
	}
	tt := &Task{ID: "hold-src-1", Kind: KindBT, Dir: dl, Status: StatusRunning,
		ChoosePending: true, TorrentFile: src}
	if !m.btHoldForChoose(tt, meta, torrent) {
		t.Fatal("多文件种子任务应挂起等待创建")
	}
	tt.mu.Lock()
	got, st := tt.TorrentFile, tt.Status
	tt.mu.Unlock()
	if st != StatusChoosing || got != src {
		t.Fatalf("TorrentFile 应保持为原文件路径: got=%q want=%q status=%s", got, src, st)
	}
	after, err := os.ReadDir(dl)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("下载目录不应新增文件: before=%d after=%d", len(before), len(after))
	}

	// 原文件丢失:退回写副本的旧路(磁力同径)
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	tt2 := &Task{ID: "hold-src-2", Kind: KindBT, Dir: dl, Status: StatusRunning,
		ChoosePending: true, TorrentFile: src}
	if !m.btHoldForChoose(tt2, meta, torrent) {
		t.Fatal("多文件种子任务应挂起等待创建")
	}
	tt2.mu.Lock()
	got2 := tt2.TorrentFile
	tt2.mu.Unlock()
	if got2 == src || got2 == "" {
		t.Fatalf("原文件丢失应退回写副本: torrentFile=%q", got2)
	}
	if _, err := os.Stat(got2); err != nil {
		t.Fatalf("回退副本未落盘: %v", err)
	}
}

// TestAddBTTorrentPropagatesChooseFlags 关联启动经 /api/tasks/bt 创建
// .torrent 任务时,Choose/ChooseAny 必须落到任务上(否则种子任务永不挂起)。
func TestAddBTTorrentPropagatesChooseFlags(t *testing.T) {
	_, torrent, _ := seedSource(t)
	dl := t.TempDir()
	tasksDir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil { // 不跑 Run 循环,目录需自备
		t.Fatal(err)
	}
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	b64 := base64.StdEncoding.EncodeToString(torrent)
	views, errs := m.AddBT(CreateReq{TorrentB64: b64, Choose: true, ChooseAny: true})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	tt := m.get(views[0].ID)
	tt.mu.Lock()
	pending, holdAny := tt.ChoosePending, tt.HoldAny
	tt.mu.Unlock()
	if !pending || !holdAny {
		t.Fatalf("种子任务应携带挂起标志: choosePending=%v holdAny=%v", pending, holdAny)
	}
}

// TestAddBTVisibleTorrentPlacement 磁链 choosing 产物按本机(隐藏归档)种子路径
// 创建新任务时,种子副本要复制进用户选定的下载目录(种子随数据走);来源是外部
// 本机文件(双击 / 路径创建)时不复制,直接引用原文件。
func TestAddBTVisibleTorrentPlacement(t *testing.T) {
	_, torrent, _ := seedSource(t)
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(root, "dl")
	m := NewManager(testConf(dl), filepath.Join(root, "cfg.json"), tasksDir)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}

	// 场景1:来源是隐藏任务目录里的归档(磁链 choosing 产物)→ 副本进所选目录
	tp := filepath.Join(tasksDir, "cache.torrent")
	if err := os.WriteFile(tp, torrent, 0o644); err != nil {
		t.Fatal(err)
	}
	views, errs := m.AddBT(CreateReq{TorrentPath: tp, Dir: dl})
	if len(errs) > 0 {
		t.Fatalf("归档来源创建失败: %v", errs)
	}
	tt := m.get(views[0].ID)
	tt.mu.Lock()
	vpath := tt.TorrentFile
	tt.mu.Unlock()
	dlClean := filepath.Clean(dl) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(vpath), dlClean) {
		t.Fatalf("归档来源应把种子副本写进所选目录: torrentFile=%q", vpath)
	}
	if filepath.Base(vpath) != util.SanitizeFilename(meta.Name)+".torrent" {
		t.Fatalf("副本命名应为 <种子名>.torrent: %q", vpath)
	}
	if _, err := os.Stat(vpath); err != nil {
		t.Fatalf("副本未落盘: %v", err)
	}

	// 场景2:来源是外部本机文件(如双击 .torrent)→ 不复制,直接引用原路径
	ext := filepath.Join(root, "mine.torrent")
	if err := os.WriteFile(ext, torrent, 0o644); err != nil {
		t.Fatal(err)
	}
	views2, errs2 := m.AddBT(CreateReq{TorrentPath: ext, Dir: dl})
	if len(errs2) > 0 {
		t.Fatalf("外部文件创建失败: %v", errs2)
	}
	tt2 := m.get(views2[0].ID)
	tt2.mu.Lock()
	vpath2 := tt2.TorrentFile
	tt2.mu.Unlock()
	if filepath.Clean(vpath2) != filepath.Clean(ext) {
		t.Fatalf("外部文件来源应保持原路径引用: got=%q want=%q", vpath2, ext)
	}
}
