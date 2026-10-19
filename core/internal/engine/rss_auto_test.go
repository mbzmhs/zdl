package engine

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zdownloader/internal/conf"
	"zdownloader/internal/rss"
)

// TestRSSAutoDownload 自动下载全路径:生效模式继承全局、只取数据链接、
// 触发即置已读、无链接条目保持未读、任务 Source 标记提醒/静默。
func TestRSSAutoDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("payload-data"))
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), "rssdl")
	m := newTestManager(t, func(c *conf.Config) {
		c.RSS.AutoDL = "off" // 全局默认不自动下载
	})

	// 订阅 A:notify + 指定目录;条目先落库(模拟一轮抓取),再用 Fresh 触发
	feedA, err := m.rssStore.AddFeed("https://rss.example.com/a", "", "订阅A", dir, "notify")
	if err != nil {
		t.Fatal(err)
	}
	resA := m.rssStore.ApplyItems(feedA.ID, []rss.FetchedItem{
		{GUID: "ga1", Title: "第1话", Link: srv.URL + "/page1.html",
			Links: []rss.ItemLink{{URL: srv.URL + "/data.bin", Type: "application/octet-stream"}}},
		{GUID: "ga2", Title: "磁力条目",
			Links: []rss.ItemLink{{URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}}},
		{GUID: "ga3", Title: "纯页面条目", Link: srv.URL + "/page3.html"},
	}, 200)
	if len(resA.Fresh) != 3 {
		t.Fatalf("应落库 3 条,得到 %d", len(resA.Fresh))
	}
	m.rssAutoDownload(feedA.ID, resA.Fresh)

	// 数据链接与磁力各建一个任务,Source=rss-notify,目录为订阅指定目录
	views := m.List()
	if len(views) != 2 {
		t.Fatalf("应创建 2 个任务,得到 %d: %+v", len(views), views)
	}
	srcCount := map[string]int{}
	for _, v := range views {
		srcCount[v.Source]++
		if v.Dir != dir {
			t.Fatalf("任务目录应为订阅目录 %s,得到 %s", dir, v.Dir)
		}
	}
	if srcCount["rss-notify"] != 2 {
		t.Fatalf("Source 计数错误: %+v", srcCount)
	}
	// 数据链接条目已置已读;只有页面链接的条目保持未读
	its, _ := m.rssStore.Items(feedA.ID)
	byTitle := map[string]rss.Item{}
	for _, it := range its {
		byTitle[it.Title] = it
	}
	if !byTitle["第1话"].Read {
		t.Fatal("已触发下载的条目应置为已读")
	}
	if byTitle["纯页面条目"].Read {
		t.Fatal("无数据链接的条目不应被置已读")
	}
	if byTitle["磁力条目"].Read != true {
		t.Fatal("磁力条目已建任务,也应置为已读")
	}

	// 订阅 B:inherit + 全局 off → 不建任务
	feedB, _ := m.rssStore.AddFeed("https://rss.example.com/b", "", "订阅B", "", "")
	resB := m.rssStore.ApplyItems(feedB.ID, []rss.FetchedItem{
		{GUID: "gb1", Title: "b条目", Links: []rss.ItemLink{{URL: srv.URL + "/b.bin"}}},
	}, 200)
	m.rssAutoDownload(feedB.ID, resB.Fresh)
	if n := len(m.List()); n != 2 {
		t.Fatalf("继承全局 off 不应建任务,任务数 = %d", n)
	}

	// 订阅 C:silent → Source=rss-silent
	feedC, _ := m.rssStore.AddFeed("https://rss.example.com/c", "", "订阅C", "", "silent")
	resC := m.rssStore.ApplyItems(feedC.ID, []rss.FetchedItem{
		{GUID: "gc1", Title: "c条目", Links: []rss.ItemLink{{URL: srv.URL + "/c.bin"}}},
	}, 200)
	m.rssAutoDownload(feedC.ID, resC.Fresh)
	views = m.List()
	if len(views) != 3 {
		t.Fatalf("静默模式应建任务,任务数 = %d", len(views))
	}
	if views[len(views)-1].Source != "rss-silent" {
		t.Fatalf("静默任务 Source 错误: %q", views[len(views)-1].Source)
	}

	// 全局 notify + 订阅 inherit → 生效
	m2 := newTestManager(t, func(c *conf.Config) { c.RSS.AutoDL = "notify" })
	feedD, _ := m2.rssStore.AddFeed("https://rss.example.com/d", "", "订阅D", "", "inherit")
	resD := m2.rssStore.ApplyItems(feedD.ID, []rss.FetchedItem{
		{GUID: "gd1", Title: "d条目", Links: []rss.ItemLink{{URL: srv.URL + "/d.bin"}}},
	}, 200)
	m2.rssAutoDownload(feedD.ID, resD.Fresh)
	if n := len(m2.List()); n != 1 {
		t.Fatalf("继承全局 notify 应建任务,任务数 = %d", n)
	}

	// 自动创建的数据任务真实跑完(notify 完成提醒的数据基础)
	var done TaskView
	for _, v := range m.List() {
		if v.Kind == "http" && v.Source == "rss-notify" {
			done = waitStatus(t, m, v.ID, StatusCompleted, 15e9)
		}
	}
	if done.Status != StatusCompleted {
		t.Fatalf("自动下载任务应能完成: %+v", done)
	}
}

// TestNotifyScanRSSComplete 完成提醒扫描(确定性:最小 Manager,无后台
// 协程):RSS 自动下载(完成后提醒)任务完成即触发一次 OnNotify;静默模式
// 不触发;启动基线里已完成的任务不补报;基线后的快速完成任务(生命周期
// 短于扫描间隔)也能命中。
func TestNotifyScanRSSComplete(t *testing.T) {
	var fired []string
	collect := func(title, text string) { fired = append(fired, title+"|"+text) }
	mkTask := func(m *Manager, id, src string, st Status) {
		tk := &Task{ID: id, Source: src, Status: st, Filename: id + ".bin"}
		m.mu.Lock()
		m.tasks[id] = tk
		m.order = append(m.order, id)
		m.mu.Unlock()
	}
	scan := func(m *Manager) { m.notifyScan() }
	fireCount := func() int { return len(fired) }

	// 场景一:运行中的提醒任务正常完成 → 恰好提醒一次
	m := &Manager{tasks: map[string]*Task{}, OnNotify: collect}
	mkTask(m, "t1", "rss-notify", StatusWaiting)
	scan(m)
	if fireCount() != 0 {
		t.Fatalf("基线扫描不应提醒: %v", fired)
	}
	m.get("t1").mu.Lock()
	m.get("t1").Status = StatusRunning
	m.get("t1").mu.Unlock()
	scan(m)
	if fireCount() != 0 {
		t.Fatalf("运行态不应提醒: %v", fired)
	}
	m.get("t1").mu.Lock()
	m.get("t1").Status = StatusCompleted
	m.get("t1").mu.Unlock()
	scan(m)
	if fireCount() != 1 {
		t.Fatalf("完成态应提醒一次: %v", fired)
	}
	scan(m)
	if fireCount() != 1 {
		t.Fatalf("重复扫描不应重复提醒: %v", fired)
	}

	// 场景二:静默任务完成 → 不提醒
	mkTask(m, "t2", "rss-silent", StatusRunning)
	scan(m)
	m.get("t2").mu.Lock()
	m.get("t2").Status = StatusCompleted
	m.get("t2").mu.Unlock()
	scan(m)
	if fireCount() != 1 {
		t.Fatalf("静默任务不应提醒: %v", fired)
	}

	// 场景三:进程重启基线——启动时已完成的提醒任务不补报
	m2 := &Manager{tasks: map[string]*Task{}, OnNotify: collect}
	mkTask(m2, "t3", "rss-notify", StatusCompleted)
	scan(m2) // 基线:已完成 → 标已提醒
	scan(m2)
	if fireCount() != 1 {
		t.Fatalf("启动时已完成的任务不应补报: %v", fired)
	}

	// 场景四:基线之后创建并立刻完成(生命周期短于扫描间隔)→ 命中
	mkTask(m2, "t4", "rss-notify", StatusCompleted)
	scan(m2)
	if fireCount() != 2 {
		t.Fatalf("基线后的快速完成任务应提醒: %v", fired)
	}
}
