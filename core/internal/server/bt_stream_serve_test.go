package server

// 边下边播 HTTP 服务循环集成测试:真实做种端 + 完整 Manager,经 httptest
// 走 /api/tasks/{id}/stream 的 Range 语义(206/Content-Range/字节比对)。
// 覆盖快路径(整片盘读)与块级路径(piece 未落定时逐块内存直读)及其切换。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
	"zdownloader/internal/engine"
	"zdownloader/internal/hub"
)

func streamFakeTracker(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	swarm := map[string]map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ih := fmt.Sprintf("%x", []byte(q.Get("info_hash")))
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		key := net.JoinHostPort(ip, q.Get("port"))
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
		w.Write(bt.MustEncode(map[string]any{
			"interval": int64(1), "peers": string(peers),
			"complete": int64(1), "incomplete": int64(0),
		}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamRetryDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "btsrv-")
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
	})
	return dir
}

func TestHandleBTStreamServe(t *testing.T) {
	tracker := streamFakeTracker(t)
	srcRoot := streamRetryDir(t)
	content := make([]byte, 2<<20)
	for i := range content {
		content[i] = byte(i*11 + i/503)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "v.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(srcRoot, "", "", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err := bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 128*1024) // 2MB ≈ 16s:确保全部请求落在下载中窗口
	seed.Start()
	defer seed.Close()

	dl := streamRetryDir(t)
	cfg := conf.Default(dl)
	cfg.BT.PublicTrackers = nil
	m := engine.NewManager(cfg, filepath.Join(streamRetryDir(t), "cfg.json"), filepath.Join(streamRetryDir(t), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		sctx, sc := context.WithTimeout(context.Background(), 15*time.Second)
		defer sc()
		m.Shutdown(sctx)
	}()
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(engine.CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	svc := New(m, hub.New(), os.DirFS("testdata-noexist"))
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	// 等任务运行(带重试:状态先于会话挂载)
	fetch := func(url string, rng string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}
	streamURL := ts.URL + "/api/tasks/" + id + "/stream?file=v.bin"
	deadline := time.Now().Add(20 * time.Second)
	var head []byte
	for time.Now().Before(deadline) {
		resp, body := fetch(streamURL, "bytes=0-65535")
		if resp.StatusCode == http.StatusPartialContent {
			if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-65535/2097152" {
				t.Fatalf("Content-Range 异常: %q", cr)
			}
			if resp.Header.Get("Accept-Ranges") != "bytes" {
				t.Fatal("缺 Accept-Ranges")
			}
			head = body
			break
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("首个 Range 请求异常 %d: %s", resp.StatusCode, e.Error)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if head == nil {
		t.Fatal("流端点未在期限内就绪")
	}
	if !bytes.Equal(head, content[:65536]) {
		t.Fatal("头部 64KB 内容不符")
	}
	// 中段(紧跟就绪,大概率落在块级路径:piece 未落定,逐块内存直读)
	resp, mid := fetch(streamURL, fmt.Sprintf("bytes=%d-%d", 300<<10, 300<<10+(128<<10)-1))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(mid, content[300<<10:300<<10+128<<10]) {
		t.Fatalf("中段 Range 异常: status=%d body=%s", resp.StatusCode, mid)
	}
	// 后缀 Range(探测时长/索引):内容比对
	resp, tail := fetch(streamURL, "bytes=-16384")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(tail, content[len(content)-16384:]) {
		t.Fatalf("后缀 Range 异常: status=%d len=%d", resp.StatusCode, len(tail))
	}

	// 后缀 Range 不得抢走游标:下载中的小文件会在请求过程中完成、流模式被
	// 收尾清除(游标 -1),断言存在完成竞态——持等进入做种态(数据全在、会话
	// 稳定、无完成竞态)后再验证
	seedDL := time.Now().Add(25 * time.Second)
	for time.Now().Before(seedDL) {
		if v, ok := m.Get(id); ok && v.Status == engine.StatusSeeding {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, _ := m.Get(id); v.Status != engine.StatusSeeding {
		t.Skipf("未进入做种态,跳过游标断言(状态 %s)", v.Status)
	}
	cursor := func() int {
		c, err := http.Get(ts.URL + "/api/tasks/" + id + "/stream/status")
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			CursorPiece int `json:"cursorPiece"`
		}
		_ = json.NewDecoder(c.Body).Decode(&st)
		c.Body.Close()
		return st.CursorPiece
	}
	// 开放式 Range(播放语义)拨动游标→后缀 Range 不得抢走
	resp, open := fetch(streamURL, "bytes=0-")
	if resp.StatusCode != http.StatusPartialContent || len(open) == 0 {
		t.Fatalf("开放式 Range 异常: status=%d", resp.StatusCode)
	}
	before := cursor()
	resp, tail2 := fetch(streamURL, "bytes=-16384")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(tail2, content[len(content)-16384:]) {
		t.Fatalf("做种态后缀 Range 异常: status=%d", resp.StatusCode)
	}
	if after := cursor(); after != before {
		t.Fatalf("后缀 Range 不得抢走游标, %d → %d", before, after)
	}
}
