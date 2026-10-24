package engine

// 边下边播端到端:真实做种端 + 完整 Manager,下载进行中开启流播,
// 校验 BTStreamOpen/Wait/Read/Pos/Status/Stop 的行为与数据正确性。
// 优先级顺序由 bt 包 picker_stream_test 覆盖;本测试覆盖引擎编排与读取路径。

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zdownloader/internal/bt"
)

// streamTempDir 独立临时目录 + 带重试的清理:MB 级新写文件在 Windows 上
// 常被杀软/索引器短暂锁定,直接 RemoveAll 偶发 sharing violation(与测试
// 逻辑无关的收尾竞态)。重试 2.5s 后仍失败仅记录,不判失败。
func streamTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "btstream-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 25; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("临时目录清理失败(可能被杀软短暂占用): %v", err)
		}
	})
	return dir
}

// streamSeedSource 生成单个大文件源(2MB,16KB piece ≈ 128 片),
// 保证本地回环下仍有可观察的下载中窗口。
func streamSeedSource(t *testing.T) (root string, torrent []byte, content []byte) {
	t.Helper()
	root = streamTempDir(t)
	content = make([]byte, 2<<20)
	for i := range content {
		content[i] = byte(i*7 + i/251)
	}
	if err := os.WriteFile(filepath.Join(root, "video.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return root, torrent, content
}

func TestBTStreamWhileDownloading(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, content := streamSeedSource(t)

	// 注入 tracker announce(重建 info 不变;重 parse 让做种端也带 tracker,
	// 否则种子不通告、下载端在 swarm 里找不到它)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	infoHash := meta.InfoHash
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil || meta.InfoHash != infoHash {
		t.Fatalf("announce 注入失败: %v", err)
	}

	// 做种端上传限速 ~256KB/s:2MB 持续约 8 秒,留出"下载中开流"窗口
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 256*1024)
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	cfg := testConf(dl)
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	_ = infoHash

	// 等任务进入运行(状态先于会话挂载,开流对"会话未运行"短暂重试)
	deadline := time.Now().Add(15 * time.Second)
	running := false
	for time.Now().Before(deadline) {
		if v, ok := m.Get(id); ok && (v.Status == StatusRunning || v.Status == StatusSeeding) {
			running = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !running {
		v, _ := m.Get(id)
		t.Fatalf("任务未进入运行态: %+v", v)
	}

	// 开流:多文件种子按文件相对路径(会话挂载与状态置位有几毫秒差,重试)
	var info *BTStreamInfo
	for time.Now().Before(deadline) {
		info, err = m.BTStreamOpen(id, "video.bin")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrStreamNoSession) {
			t.Fatalf("BTStreamOpen: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BTStreamOpen 重试超时: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("文件大小不符: %d != %d", info.Size, len(content))
	}
	if info.ContentType != "application/octet-stream" {
		t.Fatalf("Content-Type 应按扩展名,得到 %q", info.ContentType)
	}
	// 连接登记 + Range 到达游标
	seq, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.BTStreamConnClose(id, seq)
	m.BTStreamPos(id, seq, 0, true)

	// 首块等待 + 读取比对(下载中或已完成均可:piece 落定即读)
	wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer wcancel()
	if err := m.BTStreamWait(wctx, id, 0, 32<<10); err != nil {
		t.Fatalf("BTStreamWait 头部: %v", err)
	}
	buf := make([]byte, 32<<10)
	if err := m.BTStreamRead(id, 0, buf); err != nil {
		t.Fatalf("BTStreamRead 头部: %v", err)
	}
	if !bytes.Equal(buf, content[:32<<10]) {
		t.Fatal("头部 32KB 数据不符")
	}

	// 拖动到文件尾部:后缀 Range(探测时长/索引的典型请求)
	tail := int64(len(content)) - 16<<10
	m.BTStreamPos(id, seq, tail, true)
	if err := m.BTStreamWait(wctx, id, tail, 16<<10); err != nil {
		t.Fatalf("BTStreamWait 尾部: %v", err)
	}
	if err := m.BTStreamRead(id, tail, buf[:16<<10]); err != nil {
		t.Fatalf("BTStreamRead 尾部: %v", err)
	}
	if !bytes.Equal(buf[:16<<10], content[tail:]) {
		t.Fatal("尾部 16KB 数据不符")
	}

	// 状态快照:流开着、位图可解码
	st, err := m.BTStreamStatus(id)
	if err != nil || !st.Streaming || st.File != "video.bin" || st.Pieces == "" {
		t.Fatalf("BTStreamStatus 异常: %+v err=%v", st, err)
	}
	// 停止:还原优先级,状态回落
	if err := m.BTStreamStop(id); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.BTStreamStatus(id); st.Streaming {
		t.Fatal("停止后不应仍处于流播状态")
	}
	// 显式停做种再收尾(同 EndToEnd 测试):避免做种句柄释放与 TempDir
	// 清理在 Windows 上竞态(unlinkat 占用)
	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}

// TestBTStreamOpenGuards 开流前置校验:文件不存在 / 未选文件 / 非 BT 任务。
func TestBTStreamOpenGuards(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, content := streamSeedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	if torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", ""); err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	cfg := testConf(dl)
	m := NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	_ = content

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := m.Get(id); ok && v.Status == StatusRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, err := m.BTStreamOpen(id, "不存在.bin"); err == nil {
		t.Fatal("不存在的文件应报错")
	}
	var serr error
	for time.Now().Before(deadline) {
		if _, serr = m.BTStreamOpen(id, "video.bin"); serr == nil {
			break
		}
		if !errors.Is(serr, ErrStreamNoSession) {
			t.Fatalf("正常开流不应报错: %v", serr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if serr != nil {
		t.Fatalf("开流重试超时: %v", serr)
	}
}

// TestBTStreamCursorGating 游标连接门控:仅最新连接的吐数推进游标;最新
// 连接离开后现存最新者接管。防"回拖后旧连接把游标拖到播放头前面"。
func TestBTStreamCursorGating(t *testing.T) {
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := streamSeedSource(t)
	meta, err := bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	if torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", ""); err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 256*1024) // 限速保持下载中:完成态不开 picker 流模式,游标恒 -1
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	cfg := testConf(dl)
	m := NewManager(cfg, filepath.Join(streamTempDir(t), "cfg.json"), filepath.Join(streamTempDir(t), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = m.BTStreamOpen(id, "video.bin"); err == nil {
			break
		}
		if !errors.Is(err, ErrStreamNoSession) {
			t.Fatalf("BTStreamOpen: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BTStreamOpen 重试超时: %v", err)
	}

	cursor := func() int {
		st, err := m.BTStreamStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		return st.CursorPiece
	}
	pl := int64(16384)
	offAt := func(piece int) int64 { return int64(piece) * pl }

	seqA, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.BTStreamConnClose(id, seqA)
	if got := cursor(); got != 0 {
		t.Fatalf("初始游标应为 0,得到 %d", got)
	}
	// 旧连接 A 吐数:此刻 A 是唯一连接(=最新),应推进
	m.BTStreamServed(id, seqA, offAt(3))
	if got := cursor(); got != 3 {
		t.Fatalf("唯一连接吐数应推进游标到 3,得到 %d", got)
	}
	// 新连接 B 打开(模拟拖动后的播放连接)并 Range 到 8:游标拨到 8
	seqB, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	m.BTStreamPos(id, seqB, offAt(8), true)
	if got := cursor(); got != 8 {
		t.Fatalf("最新连接 Range 应拨游标到 8,得到 %d", got)
	}
	// 服务区间记账随活跃连接:Pos 重置起点,Served 推进终点
	if st, _ := m.BTStreamStatus(id); st.ServeStart != offAt(8) || st.ServeEnd != offAt(8) {
		t.Fatalf("服务区间应自 8 重记账,得到 %d..%d", st.ServeStart/pl, st.ServeEnd/pl)
	}
	m.BTStreamServed(id, seqB, offAt(9))
	if st, _ := m.BTStreamStatus(id); st.ServeEnd != offAt(9) {
		t.Fatalf("活跃连接吐数应推进服务终点到 9,得到 %d", st.ServeEnd/pl)
	}
	// 旧连接 A 继续吐数(滞后数据):不得把游标拖回去,也不得改写活跃服务区间
	m.BTStreamServed(id, seqA, offAt(5))
	if got := cursor(); got != 9 {
		t.Fatalf("旧连接吐数不得移动游标,期望 9 得到 %d", got)
	}
	if st, _ := m.BTStreamStatus(id); st.ServeEnd != offAt(9) {
		t.Fatalf("旧连接吐数不得改写服务区间,期望 9 得到 %d", st.ServeEnd/pl)
	}
	// 新连接 B 关闭(探测连接即开即关):A 作为现存最新接管
	m.BTStreamConnClose(id, seqB)
	m.BTStreamServed(id, seqA, offAt(12))
	if got := cursor(); got != 12 {
		t.Fatalf("最新连接离开后应接管推进到 12,得到 %d", got)
	}

	// 探测连接(有界小 Range,如浏览器回读文件头)不抢游标、不进活动服务区间:
	// 否则主播放流被门控饿死在头部(实测红线跑到游标前面)
	seqP, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	m.BTStreamPos(id, seqP, 0, false) // bytes=0-0 式探测
	if got := cursor(); got != 12 {
		t.Fatalf("探测连接不得抢游标,期望 12 得到 %d", got)
	}
	m.BTStreamServed(id, seqP, offAt(4)) // 属主 A 在场:更"新"的探测吐数不得接管
	if got := cursor(); got != 12 {
		t.Fatalf("探测连接吐数不得接管游标,期望 12 得到 %d", got)
	}
	// 活动服务区间仍是属主 A 的(0..12),未被探测改写
	if st, _ := m.BTStreamStatus(id); st.ServeStart != 0 || st.ServeEnd != offAt(12) {
		t.Fatalf("探测连接不得改写活动服务区间,期望 0..12 得到 %d..%d", st.ServeStart/pl, st.ServeEnd/pl)
	}
	m.BTStreamConnClose(id, seqP)

	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}
