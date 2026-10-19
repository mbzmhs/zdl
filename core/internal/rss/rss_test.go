package rss

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rssSample = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>示例站点</title>
    <link>https://example.com/</link>
    <item>
      <title><![CDATA[第一个 & 条目]]></title>
      <link>https://example.com/1</link>
      <guid isPermaLink="false">tag:1</guid>
      <pubDate>Mon, 02 Jan 2006 15:04:05 +0800</pubDate>
      <description><![CDATA[<p>介绍&nbsp;文本</p>]]></description>
      <enclosure url="https://example.com/a.torrent" length="12345" type="application/x-bittorrent"/>
      <enclosure url="https://example.com/a.torrent" length="12345"/>
    </item>
    <item>
      <title>第二个</title>
      <link>https://example.com/2</link>
      <description>纯文本 &amp; 实体</description>
    </item>
  </channel>
</rss>`

const atomSample = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom 示例</title>
  <link href="https://atom.example.org/" rel="alternate"/>
  <entry>
    <title>条目一</title>
    <id>urn:uuid:1</id>
    <updated>2024-05-01T10:00:00Z</updated>
    <link href="https://atom.example.org/1"/>
    <link rel="enclosure" href="https://atom.example.org/1.mp4" type="video/mp4" length="1024"/>
    <summary>摘要文本</summary>
  </entry>
</feed>`

const rdfSample = `<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <channel rdf:about="https://rdf.example.com/">
    <title>RDF 示例</title>
    <link>https://rdf.example.com/</link>
  </channel>
  <item rdf:about="https://rdf.example.com/1">
    <title>RDF 条目</title>
    <link>https://rdf.example.com/1</link>
    <dc:date>2024-01-01</dc:date>
  </item>
</rdf:RDF>`

func TestParseRSS2(t *testing.T) {
	p, err := Parse([]byte(rssSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Title != "示例站点" || p.Link != "https://example.com/" {
		t.Fatalf("频道信息错误: %+v", p)
	}
	if len(p.Items) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(p.Items))
	}
	it := p.Items[0]
	if it.Title != "第一个 & 条目" {
		t.Errorf("标题 = %q", it.Title)
	}
	if it.GUID != "tag:1" || it.Link != "https://example.com/1" {
		t.Errorf("标识/链接错误: %+v", it)
	}
	if it.Date != "2006-01-02T15:04:05+08:00" {
		t.Errorf("时间归一化 = %q", it.Date)
	}
	if it.Summary != "介绍 文本" {
		t.Errorf("摘要 = %q", it.Summary)
	}
	if len(it.Links) != 1 || it.Links[0].URL != "https://example.com/a.torrent" ||
		it.Links[0].Length != 12345 || it.Links[0].Type != "application/x-bittorrent" {
		t.Errorf("附件 = %+v", it.Links)
	}
	if p.Items[1].Title != "第二个" || p.Items[1].Date != "" {
		t.Errorf("第二条错误: %+v", p.Items[1])
	}
	if p.Items[1].Summary != "纯文本 & 实体" {
		t.Errorf("第二条摘要 = %q", p.Items[1].Summary)
	}
}

func TestParseAtom(t *testing.T) {
	p, err := Parse([]byte(atomSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Title != "Atom 示例" || p.Link != "https://atom.example.org/" {
		t.Fatalf("频道信息错误: %+v", p)
	}
	if len(p.Items) != 1 {
		t.Fatalf("条目数 = %d, want 1", len(p.Items))
	}
	it := p.Items[0]
	if it.GUID != "urn:uuid:1" || it.Title != "条目一" ||
		it.Link != "https://atom.example.org/1" || it.Date != "2024-05-01T10:00:00Z" {
		t.Errorf("条目错误: %+v", it)
	}
	if len(it.Links) != 1 || it.Links[0].URL != "https://atom.example.org/1.mp4" ||
		it.Links[0].Length != 1024 {
		t.Errorf("附件 = %+v", it.Links)
	}
}

func TestParseRDF(t *testing.T) {
	p, err := Parse([]byte(rdfSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Title != "RDF 示例" {
		t.Fatalf("频道标题 = %q", p.Title)
	}
	if len(p.Items) != 1 || p.Items[0].Title != "RDF 条目" ||
		p.Items[0].Date != "2024-01-01T00:00:00Z" {
		t.Fatalf("RDF 条目错误: %+v", p.Items)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse([]byte("not xml at all")); err == nil {
		t.Fatal("非订阅内容应报错")
	}
	if _, err := Parse([]byte(`<html><body>hi</body></html>`)); err == nil {
		t.Fatal("非订阅 XML 应报错")
	}
}

// fakeFeed 构造用于存储测试的抓取条目。
func fakeFeed(titles ...string) []FetchedItem {
	out := make([]FetchedItem, 0, len(titles))
	for _, s := range titles {
		out = append(out, FetchedItem{
			GUID:  "g-" + s,
			Title: s,
			Link:  "https://example.com/" + s,
			Date:  "2024-01-01T00:00:00Z",
			Links: []ItemLink{{URL: "https://example.com/" + s + ".torrent"}},
		})
	}
	return out
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "rss.json"))
}

func TestStoreFeedLifecycle(t *testing.T) {
	s := newTestStore(t)
	f1, err := s.AddFeed("https://a.example.com/feed", "", "", "", "")
	if err != nil {
		t.Fatalf("添加订阅失败: %v", err)
	}
	if _, err := s.AddFeed("https://a.example.com/feed", "", "", "", ""); err == nil {
		t.Fatal("重复订阅应报错")
	}
	if _, err := s.AddFeed("ftp://x", "", "", "", ""); err == nil {
		t.Fatal("非 http(s) 订阅应报错")
	}
	dir, err := s.AddFolder("影视")
	if err != nil {
		t.Fatalf("新建文件夹失败: %v", err)
	}
	if _, err := s.AddFolder("  "); err == nil {
		t.Fatal("空文件夹名应报错")
	}
	if _, err := s.UpdateFeed(f1.ID, nil, &dir.ID, nil, nil); err != nil {
		t.Fatalf("移动订阅失败: %v", err)
	}
	if tv := s.Tree(); len(tv.Feeds) != 1 || tv.Feeds[0].FolderID != dir.ID {
		t.Fatalf("移动后归属错误: %+v", tv.Feeds)
	}
	// 删除文件夹:订阅回到未分组而非被删
	if err := s.RemoveFolder(dir.ID); err != nil {
		t.Fatalf("删除文件夹失败: %v", err)
	}
	if tv := s.Tree(); len(tv.Feeds) != 1 || tv.Feeds[0].FolderID != "" {
		t.Fatalf("删除文件夹后订阅应回到未分组: %+v", tv.Feeds)
	}
	// 标题:用户标题优先于站点标题(展示层由前端回退)
	title := "我的订阅"
	if _, err := s.UpdateFeed(f1.ID, &title, nil, nil, nil); err != nil {
		t.Fatalf("改标题失败: %v", err)
	}
	if tv := s.Tree(); tv.Feeds[0].Title != title {
		t.Fatalf("标题未更新: %+v", tv.Feeds[0])
	}
	if err := s.RemoveFeed(f1.ID); err != nil {
		t.Fatalf("删除订阅失败: %v", err)
	}
	if tv := s.Tree(); len(tv.Feeds) != 0 {
		t.Fatal("删除后仍有订阅")
	}
}

func TestStoreMoveAndOrder(t *testing.T) {
	s := newTestStore(t)
	d1, _ := s.AddFolder("一")
	d2, _ := s.AddFolder("二")
	f1, _ := s.AddFeed("https://1.example.com/f", "", "", "", "")
	f2, _ := s.AddFeed("https://2.example.com/f", "", "", "", "")
	f3, _ := s.AddFeed("https://3.example.com/f", "", "", "", "")

	// 三条都放进 d1,f3 拖到 f1 之前 → 顺序 f3, f1, f2
	for _, id := range []string{f1.ID, f2.ID, f3.ID} {
		if err := s.MoveFeed(id, d1.ID, ""); err != nil {
			t.Fatalf("移入文件夹失败: %v", err)
		}
	}
	if err := s.MoveFeed(f3.ID, d1.ID, f1.ID); err != nil {
		t.Fatalf("拖拽排序失败: %v", err)
	}
	orders := map[string]int{}
	for _, f := range s.Tree().Feeds {
		orders[f.ID] = f.Order
	}
	if !(orders[f3.ID] < orders[f1.ID] && orders[f1.ID] < orders[f2.ID]) {
		t.Fatalf("同文件夹排序错误: %+v", orders)
	}
	// f2 移到 d2
	if err := s.MoveFeed(f2.ID, d2.ID, ""); err != nil {
		t.Fatalf("跨文件夹移动失败: %v", err)
	}
	for _, f := range s.Tree().Feeds {
		if f.ID == f2.ID && f.FolderID != d2.ID {
			t.Fatalf("跨文件夹移动未生效: %+v", f)
		}
	}
	// 文件夹重排:二在前
	if err := s.ReorderFolders([]string{d2.ID, d1.ID}); err != nil {
		t.Fatalf("文件夹排序失败: %v", err)
	}
	tv := s.Tree()
	if tv.Folders[0].ID != d2.ID || tv.Folders[1].ID != d1.ID {
		t.Fatalf("文件夹顺序错误: %+v", tv.Folders)
	}
	if err := s.ReorderFolders([]string{d2.ID}); err == nil {
		t.Fatal("缺少文件夹的顺序提交应报错")
	}
}

func TestApplyItemsAndRead(t *testing.T) {
	s := newTestStore(t)
	f, _ := s.AddFeed("https://a.example.com/feed", "", "", "", "")

	res := s.ApplyItems(f.ID, fakeFeed("A", "B"), 200)
	if res.Added != 2 || !res.Changed {
		t.Fatalf("首次合并 added=%d changed=%v", res.Added, res.Changed)
	}
	if s.Unread() != 2 {
		t.Fatalf("未读 = %d, want 2", s.Unread())
	}
	items, _ := s.Items(f.ID)
	firstSeen := items[0].FirstSeen
	if items[0].Title != "A" || items[1].Title != "B" {
		t.Fatalf("条目顺序错误: %+v", items)
	}
	// 标记 A 已读,再刷新(源里 A 内容变化 + 新增 C)
	marked, err := s.MarkItemRead(items[0].ID, true)
	if !marked || err != nil {
		t.Fatalf("标记已读失败: %v", err)
	}
	if n, _ := s.MarkFeedRead(f.ID, false); n != 1 {
		t.Fatalf("全部未读应只影响 1 条, got %d", n)
	}
	// 刷新:新增 C;已知条目 A(guid 相同)标题更新;B 原样
	newItems := []FetchedItem{
		{GUID: "g-C", Title: "C", Link: "https://example.com/C", Date: "2024-01-02T00:00:00Z"},
		{GUID: "g-A", Title: "A改", Link: "https://example.com/A", Date: "2024-01-01T00:00:00Z",
			Links: []ItemLink{{URL: "https://example.com/A.torrent"}}},
		{GUID: "g-B", Title: "B", Link: "https://example.com/B", Date: "2024-01-01T00:00:00Z"},
	}
	res = s.ApplyItems(f.ID, newItems, 200)
	if res.Added != 1 {
		t.Fatalf("新增数 = %d, want 1", res.Added)
	}
	items, _ = s.Items(f.ID)
	if len(items) != 3 || items[0].Title != "C" {
		t.Fatalf("合并结果错误: %+v", items)
	}
	byTitle := map[string]Item{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	a, ok := byTitle["A改"]
	if !ok {
		t.Fatalf("已知条目标题未跟随刷新: %+v", items)
	}
	if _, stale := byTitle["A"]; stale {
		t.Fatalf("旧标题条目未被合并: %+v", items)
	}
	if !a.FirstSeen.Equal(firstSeen) {
		t.Error("已知条目首见时间被重置")
	}
	if len(a.Links) != 1 {
		t.Error("已知条目链接未保留")
	}
	if a.Read {
		t.Error("已知条目已读状态未保留(应为未读)")
	}
	if s.Unread() != 3 {
		t.Fatalf("未读 = %d, want 3", s.Unread())
	}
	// 源中消失的条目保留在尾部
	if res := s.ApplyItems(f.ID, fakeFeed("C"), 200); res.Added != 0 {
		t.Fatalf("无新条目时 added = %d", res.Added)
	}
	if items, _ = s.Items(f.ID); len(items) != 3 {
		t.Fatalf("消失条目应保留: %+v", items)
	}
	// 保留上限截断
	s.ApplyItems(f.ID, fakeFeed("D"), 2)
	if items, _ = s.Items(f.ID); len(items) != 2 {
		t.Fatalf("截断后条数 = %d, want 2", len(items))
	}
	// Touch:错误可见,恢复后清空
	if !s.Touch(f.ID, nil, "", "", errTest) {
		t.Fatal("错误状态变化应报 changed")
	}
	if tv := s.Tree(); tv.Feeds[0].LastError != errTest.Error() {
		t.Fatalf("错误未记录: %+v", tv.Feeds[0])
	}
	s.Touch(f.ID, &Parsed{Title: "站点名", Link: "https://a.example.com/"}, "etag1", "mod1", nil)
	tv := s.Tree()
	if tv.Feeds[0].LastError != "" || tv.Feeds[0].SiteTitle != "站点名" {
		t.Fatalf("抓取成功后状态错误: %+v", tv.Feeds[0])
	}
	if refs := s.DueFeeds(time.Hour, time.Now()); len(refs) != 0 {
		t.Fatalf("刚抓取过的订阅不应到期: %+v", refs)
	}
	if refs := s.DueFeeds(time.Hour, time.Now().Add(2*time.Hour)); len(refs) != 1 {
		t.Fatalf("2 小时后应到期: %+v", refs)
	}
	// 全部已读
	if n := s.MarkAllRead(); n != 2 {
		t.Fatalf("全部已读影响 = %d, want 2", n)
	}
	if s.Unread() != 0 {
		t.Fatalf("全部已读后未读 = %d", s.Unread())
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rss.json")
	s := New(path)
	f, _ := s.AddFeed("https://a.example.com/feed", "", "标题", "", "")
	s.ApplyItems(f.ID, fakeFeed("A", "B"), 200)
	items, _ := s.Items(f.ID)
	s.MarkItemRead(items[0].ID, true)
	dir, _ := s.AddFolder("分组")
	s.UpdateFeed(f.ID, nil, &dir.ID, nil, nil)

	s2 := New(path)
	tv := s2.Tree()
	if len(tv.Feeds) != 1 || tv.Feeds[0].FolderID != dir.ID || tv.Feeds[0].Title != "标题" {
		t.Fatalf("重载后订阅错误: %+v", tv.Feeds)
	}
	if tv.Unread != 1 {
		t.Fatalf("重载后未读 = %d, want 1", tv.Unread)
	}
	its, _ := s2.Items(f.ID)
	if len(its) != 2 || !its[0].Read || its[1].Read {
		t.Fatalf("重载后已读状态错误: %+v", its)
	}
}

type staticErr struct{}

func (staticErr) Error() string { return "抓取失败:超时" }

var errTest error = staticErr{}

func TestItemIDSanitize(t *testing.T) {
	id := itemID("feed1", FetchedItem{Title: "t", Date: "d"})
	if !strings.HasPrefix(id, "i") || len(id) != 17 {
		t.Fatalf("ID 形态异常: %q", id)
	}
	if id == itemID("feed2", FetchedItem{Title: "t", Date: "d"}) {
		t.Fatal("不同订阅的同名条目不应共用 ID")
	}
}

// TestApplyItemsFreshAndSeeded ApplyItems 新语义:返回本轮新增条目与
// "首次抓到条目"标记(自动下载跳过新订阅的整个存档)。
func TestApplyItemsFreshAndSeeded(t *testing.T) {
	s := newTestStore(t)
	f, _ := s.AddFeed("https://a.example.com/feed", "", "", "", "")

	res := s.ApplyItems(f.ID, fakeFeed("A", "B"), 200)
	if !res.Seeded || res.Added != 2 || len(res.Fresh) != 2 {
		t.Fatalf("首抓应 Seeded 且 Fresh=2: %+v", res)
	}
	// 已有条目的订阅再刷新:不再 Seeded,Fresh 只含本轮新出现的
	if res := s.ApplyItems(f.ID, fakeFeed("A", "C"), 200); res.Seeded || res.Added != 1 || len(res.Fresh) != 1 || res.Fresh[0].Title != "C" {
		t.Fatalf("二轮应 Fresh=[C]: %+v", res)
	}
}
