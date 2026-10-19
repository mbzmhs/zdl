package engine

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
	"zdownloader/internal/util"
)

// TestMain 全包封闭化:DHT 默认引导节点全是公网域名,单测一律置空——
// 每个 Manager/节点的启动引导不再发公网 DNS 与 UDP(门禁慢且不稳的主因)。
// 需要 DHT 拓扑的用例用本地节点互插路由表完成引导(TestBTMagnetViaDHT)。
func TestMain(m *testing.M) {
	bt.DHTBootstrapNodes = nil
	os.Exit(m.Run())
}

// testConf 测试配置底座:在默认值上掐掉公共 tracker 列表(磁力元数据
// 抓取与自动追加的来源),保证门禁不碰外网、不背 tracker 超时。
func testConf(dir string) conf.Config {
	c := conf.Default(dir)
	c.BT.PublicTrackers = nil
	return c
}

// stopManager 测试收尾:取消调度循环并等待任务协程全退+最终落盘。
// 只 cancel 不等待的话,后台持久化会与 TempDir 清理竞态
// (Windows unlinkat 报 not-empty,实测)。
func stopManager(cancel context.CancelFunc, m *Manager) {
	cancel()
	ctx, c2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer c2()
	m.Shutdown(ctx)
}

// 测试辅助:构建带临时目录的 Manager。
func newTestManager(t *testing.T, mutate func(*conf.Config)) *Manager {
	t.Helper()
	tmp := t.TempDir()
	cfg := testConf(tmp)
	if mutate != nil {
		mutate(&cfg)
	}
	m := NewManager(cfg, filepath.Join(tmp, "config.json"), filepath.Join(tmp, "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	m.rootCtx = ctx
	m.loadTasks()
	go m.scheduler(ctx)
	go m.ticker(ctx)
	t.Cleanup(func() { stopManager(cancel, m) })
	return m
}

func waitStatus(t *testing.T, m *Manager, id string, want Status, timeout time.Duration) TaskView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := m.Get(id); ok && v.Status == want {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := m.Get(id)
	t.Fatalf("任务未到达状态 %s: 当前 %+v", want, v)
	return TaskView{}
}

// ---- 分段池 ----

func TestChunkPoolCoversAllBytes(t *testing.T) {
	const size = 10 << 20
	p := newChunkPool(size, 8)
	var covered int64
	last := int64(-1)
	for {
		seg, ok := p.Acquire()
		if !ok {
			break
		}
		if seg.Start != last+1 {
			t.Fatalf("分块不连续: %d 之后是 %d", last, seg.Start)
		}
		last = seg.End
		covered += seg.Len()
		p.Complete(seg)
	}
	if covered != size || last != size-1 {
		t.Fatalf("分块覆盖错误: covered=%d last=%d", covered, last)
	}
}

func TestChunkPoolResumePartial(t *testing.T) {
	segs := []Segment{{Start: 0, End: 99, Done: 100}, {Start: 100, End: 199, Done: 40}, {Start: 200, End: 299}}
	p := newChunkPoolFrom(segs)
	if got := p.Remaining(); got != 160 {
		t.Fatalf("剩余应为 160,实际 %d", got)
	}
}

// ---- 多线程下载 ----

func bigPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i * 31)
	}
	return b
}

// rangeServer 提供 Range 支持(分段并发)的测试服务器。
func rangeServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", `"abc"`)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Write(payload)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start < 0 || end >= len(payload) || start > end {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMultiSegmentDownload(t *testing.T) {
	payload := bigPayload(5 << 20)
	srv := rangeServer(t, payload)
	m := newTestManager(t, nil)

	views, errs := m.Create(CreateReq{URL: srv.URL + "/big.bin", Connections: 4})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 15*time.Second)
	if v.Downloaded != int64(len(payload)) {
		t.Fatalf("下载字节数不符: %d != %d", v.Downloaded, len(payload))
	}
	got, err := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("下载内容与原始数据不一致")
	}
	if _, err := os.Stat(v.Filename[:0] + filepath.Join(v.Dir, v.Filename) + ".zdl.part"); !os.IsNotExist(err) {
		t.Fatal("临时文件应被清理")
	}
}

func TestSingleStreamFallbackNoRange(t *testing.T) {
	payload := bigPayload(2 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload) // 无 Range 支持,恒 200
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, nil)

	views, _ := m.Create(CreateReq{URL: srv.URL + "/file.bin"})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 15*time.Second)
	got, _ := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if !bytes.Equal(got, payload) {
		t.Fatal("单线程下载内容不一致")
	}
}

func TestPauseResumeMultiSegment(t *testing.T) {
	payload := bigPayload(32 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond) // 人为限速,保证有暂停窗口
		rangeServerHandler(payload)(w, r)
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, nil)

	views, _ := m.Create(CreateReq{URL: srv.URL + "/slow.bin", Connections: 2})
	id := views[0].ID

	// 等待出现进度且远未完成,再暂停(确定性窗口)
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := m.Get(id)
		if v.Downloaded > 2<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("下载迟迟没有进度: %+v", v)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	v := waitStatus(t, m, id, StatusPaused, 5*time.Second)
	if v.Downloaded >= int64(len(payload)) {
		t.Fatalf("暂停时应未完成: %d", v.Downloaded)
	}
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, m, id, StatusCompleted, 60*time.Second)
	got, _ := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if !bytes.Equal(got, payload) {
		t.Fatal("续传后内容不一致")
	}
}

// rangeServerHandler 提取出来复用。
func rangeServerHandler(payload []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			w.Write(payload)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start < 0 || end >= len(payload) || start > end {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start : end+1])
	}
}

func TestChecksumVerify(t *testing.T) {
	payload := []byte("hello zdownloader")
	sum := md5.Sum(payload)
	srv := rangeServer(t, payload)
	m := newTestManager(t, nil)
	views, _ := m.Create(CreateReq{
		URL:      srv.URL + "/h.txt",
		Checksum: "md5:" + hex.EncodeToString(sum[:]),
	})
	waitStatus(t, m, views[0].ID, StatusCompleted, 10*time.Second)

	// 错误校验和 → error 状态
	srv2 := rangeServer(t, payload)
	views2, _ := m.Create(CreateReq{URL: srv2.URL + "/h.txt", Checksum: "md5:deadbeef"})
	waitStatus(t, m, views2[0].ID, StatusError, 10*time.Second)
}

func TestFilenameFromContentDisposition(t *testing.T) {
	payload := bigPayload(1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report final.pdf"`)
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, nil)
	views, _ := m.Create(CreateReq{URL: srv.URL + "/whatever?id=1"})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 15*time.Second)
	if !strings.HasSuffix(v.Filename, "report final.pdf") {
		t.Fatalf("文件名应为 Content-Disposition 决定,实际 %q", v.Filename)
	}
}

func TestUniquePathOnConflict(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := util.UniquePath(dir, "a.txt")
	if filepath.Base(got) != "a (1).txt" {
		t.Fatalf("应自动重命名为 a (1).txt,实际 %q", got)
	}
}

// 完成时绝不能覆盖已存在的同名文件(扩展创建的任务带原始文件名,
// 目标路径可能已有用户的旧文件)。
func TestCompletionNeverOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "same.bin")
	original := []byte("USER'S PRECIOUS ORIGINAL DATA")
	if err := os.WriteFile(existing, original, 0o644); err != nil {
		t.Fatal(err)
	}

	payload := bigPayload(2 << 20)
	srv := rangeServer(t, payload)
	// 管理器的下载目录必须与测试目录一致
	m := newTestManager(t, func(c *conf.Config) { c.DownloadDir = dir })

	// 显式指定与已有文件相同的名字(模拟扩展捕获场景)
	views, _ := m.Create(CreateReq{URL: srv.URL + "/same.bin", Filename: "same.bin"})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 15*time.Second)

	// 原文件必须原封不动
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("已存在的同名文件被覆盖了!")
	}
	// 新下载落到 (1) 副本
	fresh, err := os.ReadFile(filepath.Join(dir, v.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, payload) {
		t.Fatal("新下载内容不正确")
	}
	if v.Filename != "same (1).bin" {
		t.Fatalf("应改名为 same (1).bin,实际 %q", v.Filename)
	}
}

// 取消任务只清理 .zdl.part 临时文件,绝不动用户的原始文件。
func TestCancelOnlyRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	original := []byte("precious")
	if err := os.WriteFile(filepath.Join(dir, "target.bin"), original, 0o644); err != nil {
		t.Fatal(err)
	}

	// 64MB 且不支持 Range:单连接流式;先吐 2MB 再挂起,保证取消时任务必然在运行
	payload := bigPayload(64 << 20)
	release := make(chan struct{})
	var once sync.Once
	// 注册顺序即清理顺序:LIFO,必须先关 release 再 Close 服务器,
	// 否则 handler 仍阻塞在 <-release 时 srv.Close 永久等待。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if r.Method == http.MethodHead {
			return // 探测请求立即返回,否则 HEAD 也阻塞在 release 上,下载无法启动
		}
		w.Write(payload[:2<<20])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		w.Write(payload[2<<20:])
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	m := newTestManager(t, func(c *conf.Config) { c.DownloadDir = dir })

	views, _ := m.Create(CreateReq{URL: srv.URL + "/target.bin", Filename: "target.bin"})
	id := views[0].ID
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := m.Get(id)
		if v.Downloaded >= 1<<20 {
			break // 已有进度且因 handler 挂起必然不会推进
		}
		if time.Now().After(deadline) {
			t.Fatal("下载迟迟没有进度")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, id, StatusCanceled, 5*time.Second)

	// 原始文件完好
	got, _ := os.ReadFile(filepath.Join(dir, "target.bin"))
	if !bytes.Equal(got, original) {
		t.Fatal("取消任务影响了用户的原始文件!")
	}
	// 清理由 runTask 收尾协程完成,轮询等待临时文件消失
	partDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "target.bin.zdl.part")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(partDeadline) {
			t.Fatal("取消后临时 .zdl.part 未被清理")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 捕获链路:浏览器「另存为」选中的目录与文件名须原样生效(核心不覆盖、不重定向)。
func TestCreateHonorsDirAndFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "16")
		_, _ = w.Write([]byte("0123456789abcdef"))
	}))
	t.Cleanup(srv.Close)

	m := newTestManager(t, nil) // 默认下载目录 = 临时根目录
	absDir := filepath.Join(t.TempDir(), "影片")

	views, errs := m.Create(CreateReq{
		URL:      srv.URL + "/whatever-name.mp4",
		Dir:      absDir, // 绝对路径:原样采用
		Filename: "自定义 名.bin",
	})
	if len(errs) != 0 || len(views) != 1 {
		t.Fatalf("创建失败: errs=%v", errs)
	}
	if views[0].Dir != absDir {
		t.Fatalf("绝对目录未生效: %s", views[0].Dir)
	}
	if views[0].Filename != "自定义 名.bin" {
		t.Fatalf("指定文件名未生效: %s", views[0].Filename)
	}

	views2, _ := m.Create(CreateReq{URL: srv.URL + "/b.bin", Dir: "相对目录"})
	if len(views2) != 1 {
		t.Fatal("相对目录任务创建失败")
	}
	wantRel := filepath.Join(m.Config().DownloadDir, "相对目录")
	if views2[0].Dir != wantRel {
		t.Fatalf("相对目录应拼接默认下载目录: got %s want %s", views2[0].Dir, wantRel)
	}

	v := waitStatus(t, m, views[0].ID, StatusCompleted, 10*time.Second)
	got, err := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if err != nil {
		t.Fatalf("文件未落在指定位置: %v", err)
	}
	if string(got) != "0123456789abcdef" {
		t.Fatalf("内容不符: %q", got)
	}
	waitStatus(t, m, views2[0].ID, StatusCompleted, 10*time.Second)
}

// 点击文件名打开文件:仅完成任务可用,文件丢失时报错而非拉起程序。
func TestOpenFileAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, nil)

	views, errs := m.Create(CreateReq{URL: srv.URL + "/a.txt", Filename: "a.txt"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	id := views[0].ID
	waitStatus(t, m, id, StatusCompleted, 10*time.Second)

	var opened string
	openFileHook = func(p string) error { opened = p; return nil }
	t.Cleanup(func() { openFileHook = util.OpenFile })

	if err := m.OpenFile(id); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(opened) != "a.txt" {
		t.Fatalf("打开了错误路径: %q", opened)
	}

	// 文件被移动/删除:应返回错误,而不是静默成功
	os.Remove(filepath.Join(m.Config().DownloadDir, "a.txt"))
	if err := m.OpenFile(id); err == nil {
		t.Fatal("文件已删除仍返回成功")
	}

	if err := m.OpenFile("nonexistent"); err != ErrNotFound {
		t.Fatalf("不存在的任务应返回 ErrNotFound,实际 %v", err)
	}
}

// 重启后持久化的 waiting 任务必须自动续传(回归:loadTasks 后无人 kick,调度器不醒)。
func TestRestartResumesQueuedTasks(t *testing.T) {
	payload := []byte("resume-me")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	tasksDir := filepath.Join(tmp, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 预置一份"运行中"的历史任务(模拟异常退出的残留)
	stale := map[string]any{
		"id": "stale01", "kind": "http", "url": srv.URL + "/r.bin",
		"dir": tmp, "filename": "r.bin", "status": "running",
		"size": -1, "connections": 3, "createdAt": time.Now().Format(time.RFC3339Nano),
	}
	if err := util.WriteJSONAtomic(filepath.Join(tasksDir, "stale01.json"), stale); err != nil {
		t.Fatal(err)
	}

	m := NewManager(testConf(tmp), filepath.Join(tmp, "config.json"), tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	m.rootCtx = ctx
	t.Cleanup(cancel)
	m.Run(ctx) // 关键:Run 自身需要唤醒调度器

	v := waitStatus(t, m, "stale01", StatusCompleted, 10*time.Second)
	got, err := os.ReadFile(filepath.Join(tmp, "r.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("续传结果异常: %v %q", err, got)
	}
	_ = v
}

func TestIfRangeOnResumeSpec(t *testing.T) {
	tmp := t.TempDir()
	m := NewManager(testConf(tmp), filepath.Join(tmp, "config.json"), filepath.Join(tmp, "tasks"))
	m.rootCtx = context.Background()

	tk := &Task{
		ID: "t1", Kind: KindHTTP, URL: "http://x/y.bin", Dir: tmp,
		ETag:         `"v2"`,
		LastModified: "Wed, 01 Oct 2025 00:00:00 GMT",
	}
	req, err := m.newRequest(context.Background(), "GET", tk.URL, m.specOf(tk))
	if err != nil {
		t.Fatal(err)
	}
	// ETag 存在时优先用 ETag
	if got := req.Header.Get("If-Range"); got != `"v2"` {
		t.Fatalf("If-Range = %q, want ETag", got)
	}
	// 无 ETag 时退回 Last-Modified
	tk.ETag = ""
	req, err = m.newRequest(context.Background(), "GET", tk.URL, m.specOf(tk))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("If-Range"); got != "Wed, 01 Oct 2025 00:00:00 GMT" {
		t.Fatalf("If-Range = %q, want Last-Modified", got)
	}
	// 两者都无:不发送
	tk.LastModified = ""
	req, err = m.newRequest(context.Background(), "GET", tk.URL, m.specOf(tk))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("If-Range"); got != "" {
		t.Fatalf("If-Range = %q, want empty", got)
	}
}

func TestCleanupOrphanTemps(t *testing.T) {
	tmp := t.TempDir()
	tasksDir := filepath.Join(tmp, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(id, status, filename string) {
		rec := map[string]any{
			"id": id, "kind": "http", "url": "http://x/" + filename,
			"dir": tmp, "filename": filename, "status": status,
			"size": -1, "createdAt": time.Now().Format(time.RFC3339Nano),
		}
		if err := util.WriteJSONAtomic(filepath.Join(tasksDir, id+".json"), rec); err != nil {
			t.Fatal(err)
		}
	}
	mk("keep01", "paused", "keep.bin") // 待续传:part 保留
	mk("cxl01", "canceled", "cxl.bin") // 已取消:遗留 part 删除
	os.WriteFile(filepath.Join(tmp, "keep.bin.zdl.part"), []byte("k"), 0o644)
	os.WriteFile(filepath.Join(tmp, "cxl.bin.zdl.part"), []byte("c"), 0o644)
	os.WriteFile(filepath.Join(tmp, "orphan.bin.zdl.part"), []byte("o"), 0o644)
	os.MkdirAll(filepath.Join(tmp, "orphan.bin.zdlwork"), 0o755)

	m := NewManager(testConf(tmp), filepath.Join(tmp, "config.json"), tasksDir)
	m.rootCtx = context.Background()
	m.loadTasks()
	m.cleanupOrphanTemps()

	for _, p := range []string{"keep.bin.zdl.part"} {
		if _, err := os.Stat(filepath.Join(tmp, p)); err != nil {
			t.Fatalf("%s 应保留: %v", p, err)
		}
	}
	for _, p := range []string{"cxl.bin.zdl.part", "orphan.bin.zdl.part", "orphan.bin.zdlwork"} {
		if _, err := os.Stat(filepath.Join(tmp, p)); !os.IsNotExist(err) {
			t.Fatalf("%s 应被清扫: %v", p, err)
		}
	}
}
