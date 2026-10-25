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

// streamSeedSource 生成单文件视频种子源(2MB mp4,16KB piece ≈ 128 片):
// 以文件路径制种 → 单文件 torrent(meta.Multi=false),顺带覆盖任务卡
// 快速入口的 streamable 标记;本地回环下仍有可观察的下载中窗口。
func streamSeedSource(t *testing.T) (srcFile string, torrent []byte, content []byte) {
	t.Helper()
	dir := streamTempDir(t)
	content = make([]byte, 2<<20)
	for i := range content {
		content[i] = byte(i*7 + i/251)
	}
	srcFile = filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(srcFile, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(srcFile, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return srcFile, torrent, content
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
		info, err = m.BTStreamOpen(id, "video.mp4", 0)
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
	if info.ContentType != "video/mp4" {
		t.Fatalf("Content-Type 应按扩展名,得到 %q", info.ContentType)
	}
	// 单文件视频种子:任务卡快速入口标记
	if v, ok := m.Get(id); !ok || !v.Streamable {
		t.Fatal("单文件视频种子应标记 streamable")
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
	if err != nil || !st.Streaming || st.File != "video.mp4" || st.Pieces == "" {
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

	if _, err := m.BTStreamOpen(id, "不存在.bin", 0); err == nil {
		t.Fatal("不存在的文件应报错")
	}
	var serr error
	for time.Now().Before(deadline) {
		if _, serr = m.BTStreamOpen(id, "video.mp4", 0); serr == nil {
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
		if _, err = m.BTStreamOpen(id, "video.mp4", 0); err == nil {
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

// TestBTStreamCompleted 完成态直读:任务完成(做种结束、会话已拆)后开流
// 不再被拒——边看边下途中完成转做种、做种达标停止时,播放中的流重连走
// done 模式直读最终文件无缝续播。位图全量、等待恒通、预热无操作、代次门
// 照常生效。
func TestBTStreamCompleted(t *testing.T) {
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
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	// 等下载走完;开自动做种时先停止做种落回完成态(会话随之拆除)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := m.Get(id)
		if !ok {
			t.Fatal("任务消失")
		}
		if v.Status == StatusSeeding {
			if err = m.StopSeeding(id); err != nil {
				t.Fatal(err)
			}
			waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
			break
		}
		if v.Status == StatusCompleted {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, _ := m.Get(id); v.Status != StatusCompleted {
		t.Fatalf("任务应已完成,状态 %s", v.Status)
	}

	// 完成态开流:不再拒绝(旧实现返回 ErrStreamCompleted)
	info, err := m.BTStreamOpen(id, "video.mp4", 5)
	if err != nil {
		t.Fatalf("完成态开流应放行: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("文件大小不符: %d", info.Size)
	}
	// 等待恒通 + 直读字节级比对(头/中段/尾)
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	for _, off := range []int64{0, int64(len(content)) / 2, int64(len(content)) - 16<<10} {
		if err = m.BTStreamWait(wctx, id, off, 16<<10); err != nil {
			t.Fatalf("完成态等待应恒通(off=%d): %v", off, err)
		}
		buf := make([]byte, 16<<10)
		if err = m.BTStreamRead(id, off, buf); err != nil {
			t.Fatalf("完成态直读(off=%d): %v", off, err)
		}
		if !bytes.Equal(buf, content[off:off+16<<10]) {
			t.Fatalf("完成态直读数据不符(off=%d)", off)
		}
	}
	if !m.BTStreamPieceDone(id, 0) {
		t.Fatal("完成态 PieceDone 应恒真")
	}
	// 状态:streaming + 全量位图 + 无窗口/pin
	st, err := m.BTStreamStatus(id)
	if err != nil || !st.Streaming || st.Pieces == "" {
		t.Fatalf("完成态状态异常: %+v err=%v", st, err)
	}
	if st.CursorPiece != -1 || st.Ahead != 0 {
		t.Fatalf("完成态不应有引擎窗口/游标: cursor=%d ahead=%d", st.CursorPiece, st.Ahead)
	}
	raw, err := base64.StdEncoding.DecodeString(st.Pieces)
	if err != nil {
		t.Fatal(err)
	}
	n := st.LastPiece - st.FirstPiece + 1
	if len(raw) != (n+7)/8 {
		t.Fatalf("位图长度不符: %d != %d", len(raw), (n+7)/8)
	}
	for i := 0; i < n; i++ {
		if raw[i/8]&(1<<(7-uint(i%8))) == 0 {
			t.Fatalf("完成态位图应全 1(piece %d)", i)
		}
	}
	// 预热:完成态无优先级,无操作成功
	if err = m.BTStreamWarm(id, "video.mp4"); err != nil {
		t.Fatalf("完成态预热应为无操作: %v", err)
	}
	// 代次门照常:旧代次拒绝,会话不动
	if _, err = m.BTStreamOpen(id, "video.mp4", 3); !errors.Is(err, ErrStreamStale) {
		t.Fatalf("旧代次应拒绝,得到 %v", err)
	}
	m.BTStreamStop(id)
	if st, _ = m.BTStreamStatus(id); st.Streaming {
		t.Fatal("停止后不应仍处于流播状态")
	}
}

// TestBTStreamReadaheadGuard 预读连接归属守卫:浏览器为向前预取会另开
// 远处起点的开放式 Range 连接——属主活跃吐数期间到达的这类连接按预读
// 处理(不抢游标/锚点);属主停滞后的远处开放式连接视为拖动后的新播放
// 连接,正常接管。
func TestBTStreamReadaheadGuard(t *testing.T) {
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
	seed.SetLimits(0, 128*1024)
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	m := NewManager(testConf(dl), filepath.Join(streamTempDir(t), "cfg.json"), filepath.Join(streamTempDir(t), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = m.BTStreamOpen(id, "video.mp4", 0); err == nil {
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
	pl := int64(16384)
	cur := func() int {
		st, err := m.BTStreamStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		return st.CursorPiece
	}

	// 主连接 A:Range 0,吐数活跃(游标推进到 2)
	seqA, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.BTStreamConnClose(id, seqA)
	m.BTStreamPos(id, seqA, 0, true)
	m.BTStreamServed(id, seqA, 2*pl)
	if got := cur(); got != 2 {
		t.Fatalf("主连接吐数应推进游标到 2,得到 %d", got)
	}
	// 预读连接 B:属主活跃(lastAt 刚更新)时,远处开放式 Range → 不抢游标
	seqB, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.BTStreamConnClose(id, seqB)
	far := 100 * pl // 100 片之外
	m.BTStreamPos(id, seqB, far, true)
	if got := cur(); got != 2 {
		t.Fatalf("活跃属主在场时预读连接不得抢游标,期望 2 得到 %d", got)
	}
	m.BTStreamServed(id, seqB, far+pl)
	if got := cur(); got != 2 {
		t.Fatalf("预读连接吐数不得推进游标,期望 2 得到 %d", got)
	}
	// 服务区间记账仍是属主 A 的(锚点不被预读带走)
	if st, _ := m.BTStreamStatus(id); st.ServeStart != 0 || st.ServeEnd != 2*pl {
		t.Fatalf("活动服务区间应保持属主 A 的 0..2 片,得到 %d..%d", st.ServeStart/pl, st.ServeEnd/pl)
	}
	// 属主停滞(>3s 无吐数)后,远处开放式连接 = 拖动后的新播放连接 → 接管
	time.Sleep(3500 * time.Millisecond)
	seqC, err := m.BTStreamConnOpen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.BTStreamConnClose(id, seqC)
	m.BTStreamPos(id, seqC, far, true)
	if got := cur(); got != int(far/pl) {
		t.Fatalf("属主停滞后远处开放式连接应接管游标,期望 %d 得到 %d", far/pl, got)
	}
	m.BTStreamStop(id)
	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}
