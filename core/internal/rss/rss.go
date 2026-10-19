// Package rss 实现 RSS/Atom 订阅:抓取解析与本地存储(文件夹 / 订阅 / 条目 / 已读)。
// 仅依赖标准库,遵循项目零第三方依赖的约束。
package rss

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zdownloader/internal/util"
)

// ItemLink 是条目下可直接下载的资源链接(enclosure / 附件)。
type ItemLink struct {
	URL    string `json:"url"`
	Type   string `json:"type,omitempty"`   // MIME 类型(可空)
	Length int64  `json:"length,omitempty"` // 字节数,0 = 未知
	Title  string `json:"title,omitempty"`
}

// Item 是一条订阅条目。ID 由订阅 ID + 稳定标识(guid/link/标题+时间)哈希而来,
// 刷新时据此保留已读状态与首次发现时间。
type Item struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Link      string     `json:"link,omitempty"`
	Date      string     `json:"date,omitempty"` // RFC3339(不可解析时为原文)
	Summary   string     `json:"summary,omitempty"`
	Links     []ItemLink `json:"links,omitempty"`
	Read      bool       `json:"read"`
	FirstSeen time.Time  `json:"firstSeen"`
}

// Feed 是一个订阅源(持久化形态,含全部条目)。
type Feed struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`     // 用户自定义标题(优先展示)
	SiteTitle string `json:"siteTitle,omitempty"` // 抓取到的频道标题
	Link      string `json:"link,omitempty"`      // 站点主页
	FolderID  string `json:"folderId,omitempty"`  // 所属文件夹,空 = 未分组
	Order     int    `json:"order"`               // 同文件夹内排序
	// 自动下载设置:DownloadDir 空 = 跟随全局默认下载目录;AutoDL 为
	// ""/"inherit" = 继承全局 RSS 设置,"off" = 不自动下载,"notify" =
	// 自动下载完成后提醒,"silent" = 静默自动下载。自动下载只取条目的
	// 数据链接(enclosure),条目页面 Link 不参与。
	DownloadDir string    `json:"downloadDir,omitempty"`
	AutoDL      string    `json:"autoDl,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	LastFetch   time.Time `json:"lastFetch"`
	LastError   string    `json:"lastError,omitempty"`
	ETag        string    `json:"etag,omitempty"`
	LastMod     string    `json:"lastModified,omitempty"`
	Items       []Item    `json:"items,omitempty"`
}

// Folder 是订阅文件夹(单层,不嵌套)。
type Folder struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

// FeedView 是订阅源的展示快照(不含条目正文)。
type FeedView struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Title       string    `json:"title,omitempty"`
	SiteTitle   string    `json:"siteTitle,omitempty"`
	Link        string    `json:"link,omitempty"`
	FolderID    string    `json:"folderId,omitempty"`
	Order       int       `json:"order"`
	DownloadDir string    `json:"downloadDir,omitempty"` // 空 = 跟随全局默认下载目录
	AutoDL      string    `json:"autoDl,omitempty"`      // ""/inherit/off/notify/silent
	CreatedAt   time.Time `json:"createdAt"`
	LastFetch   time.Time `json:"lastFetch"`
	LastError   string    `json:"lastError,omitempty"`
	ItemCount   int       `json:"itemCount"`
	Unread      int       `json:"unread"`
}

// TreeView 是订阅树快照(文件夹 + 订阅 + 未读合计)。
type TreeView struct {
	Folders []Folder   `json:"folders"`
	Feeds   []FeedView `json:"feeds"`
	Unread  int        `json:"unread"`
}

// FeedRef 是抓取调度所需的订阅最小信息。
type FeedRef struct {
	ID        string
	URL       string
	ETag      string
	LastMod   string
	LastFetch time.Time
}

// ErrNotFound 目标文件夹/订阅/条目不存在。
var ErrNotFound = errors.New("订阅不存在")

// Store 是 rss.json 的内存态与持久化。所有导出方法并发安全。
type Store struct {
	mu     sync.Mutex
	path   string
	dir    []Folder
	feeds  []Feed
	unread atomic.Int64 // 未读合计(无锁读,供 SSE 快照高频取用)
}

// New 载入(或初始化)订阅存储。
func New(path string) *Store {
	s := &Store{path: path}
	_ = util.ReadJSON(path, &struct {
		Folders *[]Folder `json:"folders"`
		Feeds   *[]Feed   `json:"feeds"`
	}{&s.dir, &s.feeds})
	s.normOrders()
	s.recount()
	return s
}

// save 持久化(调用方持锁);顺带刷新未读计数。
func (s *Store) save() {
	s.recount()
	_ = util.WriteJSONAtomic(s.path, struct {
		Folders []Folder `json:"folders"`
		Feeds   []Feed   `json:"feeds"`
	}{s.dir, s.feeds})
}

func (s *Store) recount() {
	n := 0
	for i := range s.feeds {
		for _, it := range s.feeds[i].Items {
			if !it.Read {
				n++
			}
		}
	}
	s.unread.Store(int64(n))
}

// normOrders 归一化排序号:同组内按现有 Order 稳定排序后重排 0..n-1。
func (s *Store) normOrders() {
	sort.SliceStable(s.dir, func(i, j int) bool { return s.dir[i].Order < s.dir[j].Order })
	for i := range s.dir {
		s.dir[i].Order = i
	}
	byFolder := map[string][]int{}
	for i := range s.feeds {
		byFolder[s.feeds[i].FolderID] = append(byFolder[s.feeds[i].FolderID], i)
	}
	for _, idxs := range byFolder {
		sort.SliceStable(idxs, func(a, b int) bool { return s.feeds[idxs[a]].Order < s.feeds[idxs[b]].Order })
		for n, i := range idxs {
			s.feeds[i].Order = n
		}
	}
}

func (s *Store) feed(id string) *Feed {
	for i := range s.feeds {
		if s.feeds[i].ID == id {
			return &s.feeds[i]
		}
	}
	return nil
}

// ---------- 查询 ----------

// Tree 返回订阅树快照与未读合计。
func (s *Store) Tree() TreeView {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 空树也输出 [] 而非 null:前端把 feeds/folders 当数组用,删光最后一个
	// 订阅后 null 会让 RSS 页渲染中断(实测整页空白且无法新增订阅)
	tv := TreeView{Folders: []Folder{}, Feeds: []FeedView{}}
	tv.Folders = append(tv.Folders, s.dir...)
	for i := range s.feeds {
		f := &s.feeds[i]
		v := FeedView{
			ID: f.ID, URL: f.URL, Title: f.Title, SiteTitle: f.SiteTitle,
			Link: f.Link, FolderID: f.FolderID, Order: f.Order,
			DownloadDir: f.DownloadDir, AutoDL: f.AutoDL,
			CreatedAt: f.CreatedAt, LastFetch: f.LastFetch, LastError: f.LastError,
			ItemCount: len(f.Items),
		}
		for _, it := range f.Items {
			if !it.Read {
				v.Unread++
			}
		}
		tv.Unread += v.Unread
		tv.Feeds = append(tv.Feeds, v)
	}
	sort.SliceStable(tv.Feeds, func(i, j int) bool {
		if tv.Feeds[i].FolderID != tv.Feeds[j].FolderID {
			return tv.Feeds[i].FolderID < tv.Feeds[j].FolderID
		}
		return tv.Feeds[i].Order < tv.Feeds[j].Order
	})
	return tv
}

// Items 返回某订阅的条目(新→旧)。
func (s *Store) Items(feedID string) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(feedID)
	if f == nil {
		return nil, ErrNotFound
	}
	return append([]Item(nil), f.Items...), nil
}

// Unread 返回全部订阅的未读条目数(无锁)。
func (s *Store) Unread() int { return int(s.unread.Load()) }

// DueFeeds 返回距上次抓取已超过 interval 的订阅(按添加顺序)。
func (s *Store) DueFeeds(interval time.Duration, now time.Time) []FeedRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FeedRef
	for i := range s.feeds {
		f := &s.feeds[i]
		if now.Sub(f.LastFetch) < interval {
			continue
		}
		out = append(out, FeedRef{ID: f.ID, URL: f.URL, ETag: f.ETag, LastMod: f.LastMod, LastFetch: f.LastFetch})
	}
	return out
}

// AllFeeds 返回全部订阅的抓取信息(手动"全部更新"用)。
func (s *Store) AllFeeds() []FeedRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FeedRef, 0, len(s.feeds))
	for i := range s.feeds {
		f := &s.feeds[i]
		out = append(out, FeedRef{ID: f.ID, URL: f.URL, ETag: f.ETag, LastMod: f.LastMod, LastFetch: f.LastFetch})
	}
	return out
}

// FeedURL 返回订阅地址(抓取前校验存在)。
func (s *Store) FeedURL(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil {
		return "", false
	}
	return f.URL, true
}

// FeedDownloadOpts 返回订阅的自动下载设置(下载目录/模式/标题)。
func (s *Store) FeedDownloadOpts(id string) (downloadDir, autoDL, title string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil {
		return "", "", "", false
	}
	return f.DownloadDir, f.AutoDL, f.Title, true
}

// ---------- 订阅增删改 ----------

func validFeedURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("仅支持 http/https 订阅地址")
	}
	return nil
}

// normAutoDL 归一自动下载模式;非法值回退继承全局。
func normAutoDL(m string) string {
	switch m {
	case "off", "notify", "silent":
		return m
	}
	return "inherit"
}

// AddFeed 新增订阅;同地址已存在时报错。downloadDir 空 = 跟随全局默认
// 下载目录;autoDL 见 Feed.AutoDL。
func (s *Store) AddFeed(rawURL, folderID, title, downloadDir, autoDL string) (FeedView, error) {
	rawURL = strings.TrimSpace(rawURL)
	if err := validFeedURL(rawURL); err != nil {
		return FeedView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.feeds {
		if s.feeds[i].URL == rawURL {
			return FeedView{}, errors.New("该订阅已存在")
		}
	}
	if folderID != "" && !s.folderExists(folderID) {
		folderID = ""
	}
	f := Feed{
		ID: util.RandomID(), URL: rawURL, Title: strings.TrimSpace(title),
		FolderID: folderID, DownloadDir: strings.TrimSpace(downloadDir),
		AutoDL: normAutoDL(autoDL), CreatedAt: time.Now(),
	}
	f.Order = s.nextOrder(folderID)
	s.feeds = append(s.feeds, f)
	s.save()
	return s.feeds[len(s.feeds)-1].view(), nil
}

func (s *Store) folderExists(id string) bool {
	for i := range s.dir {
		if s.dir[i].ID == id {
			return true
		}
	}
	return false
}

func (s *Store) nextOrder(folderID string) int {
	n := 0
	for i := range s.feeds {
		if s.feeds[i].FolderID == folderID {
			n++
		}
	}
	return n
}

func (f *Feed) view() FeedView {
	v := FeedView{
		ID: f.ID, URL: f.URL, Title: f.Title, SiteTitle: f.SiteTitle, Link: f.Link,
		FolderID: f.FolderID, Order: f.Order,
		DownloadDir: f.DownloadDir, AutoDL: f.AutoDL,
		CreatedAt: f.CreatedAt,
		LastFetch: f.LastFetch, LastError: f.LastError, ItemCount: len(f.Items),
	}
	for _, it := range f.Items {
		if !it.Read {
			v.Unread++
		}
	}
	return v
}

// UpdateFeed 修改订阅标题/所属文件夹/自动下载设置(nil 指针 = 不改动)。
func (s *Store) UpdateFeed(id string, title, folderID, downloadDir, autoDL *string) (FeedView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil {
		return FeedView{}, ErrNotFound
	}
	if title != nil {
		f.Title = strings.TrimSpace(*title)
	}
	if folderID != nil && *folderID != f.FolderID {
		if *folderID == "" || s.folderExists(*folderID) {
			f.FolderID = *folderID
			f.Order = s.nextOrder(*folderID)
			s.normOrders()
		}
	}
	if downloadDir != nil {
		f.DownloadDir = strings.TrimSpace(*downloadDir)
	}
	if autoDL != nil {
		f.AutoDL = normAutoDL(*autoDL)
	}
	s.save()
	return f.view(), nil
}

// RemoveFeed 删除订阅(连同其条目与已读状态)。
func (s *Store) RemoveFeed(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.feeds {
		if s.feeds[i].ID == id {
			s.feeds = append(s.feeds[:i], s.feeds[i+1:]...)
			s.normOrders()
			s.save()
			return nil
		}
	}
	return ErrNotFound
}

// MoveFeed 把订阅移动到 folderID 下的 beforeFeedID 之前(beforeFeedID 为空 = 末尾)。
func (s *Store) MoveFeed(id, folderID, beforeFeedID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil {
		return ErrNotFound
	}
	if folderID != "" && !s.folderExists(folderID) {
		return ErrNotFound
	}
	if beforeFeedID == id {
		return nil
	}
	// 目标分组当前顺序(剔除被移动者)
	order := []string{}
	for i := range s.feeds {
		if s.feeds[i].ID == id || s.feeds[i].FolderID != folderID {
			continue
		}
		order = append(order, s.feeds[i].ID)
	}
	at := len(order)
	for i, fid := range order {
		if fid == beforeFeedID {
			at = i
			break
		}
	}
	f.FolderID = folderID
	order = append(order[:at], append([]string{id}, order[at:]...)...)
	for i, fid := range order {
		if g := s.feed(fid); g != nil {
			g.Order = i
		}
	}
	s.normOrders()
	s.save()
	return nil
}

// ---------- 文件夹增删改 ----------

// AddFolder 新建文件夹。
func (s *Store) AddFolder(name string) (Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Folder{}, errors.New("文件夹名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := Folder{ID: util.RandomID(), Name: name, Order: len(s.dir)}
	s.dir = append(s.dir, f)
	s.save()
	return f, nil
}

// RenameFolder 重命名文件夹。
func (s *Store) RenameFolder(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("文件夹名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.dir {
		if s.dir[i].ID == id {
			s.dir[i].Name = name
			s.save()
			return nil
		}
	}
	return ErrNotFound
}

// RemoveFolder 删除文件夹,其中订阅回到未分组(不删订阅)。
func (s *Store) RemoveFolder(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.dir {
		if s.dir[i].ID != id {
			continue
		}
		s.dir = append(s.dir[:i], s.dir[i+1:]...)
		for j := range s.feeds {
			if s.feeds[j].FolderID == id {
				s.feeds[j].FolderID = ""
			}
		}
		s.normOrders()
		s.save()
		return nil
	}
	return ErrNotFound
}

// ReorderFolders 按给定 ID 顺序重排文件夹(需含全部文件夹 ID)。
func (s *Store) ReorderFolders(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) != len(s.dir) {
		return errors.New("文件夹顺序与现有文件夹不匹配")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if s.folderExists(id) == false || seen[id] {
			return errors.New("文件夹顺序与现有文件夹不匹配")
		}
		seen[id] = true
	}
	for i, id := range ids {
		for j := range s.dir {
			if s.dir[j].ID == id {
				s.dir[j].Order = i
			}
		}
	}
	s.normOrders()
	s.save()
	return nil
}

// ---------- 已读状态 ----------

// MarkFeedRead 标记某订阅全部条目已读/未读,返回受影响条数。
func (s *Store) MarkFeedRead(feedID string, read bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(feedID)
	if f == nil {
		return 0, ErrNotFound
	}
	n := 0
	for i := range f.Items {
		if f.Items[i].Read != read {
			f.Items[i].Read = read
			n++
		}
	}
	if n > 0 {
		s.save()
	}
	return n, nil
}

// MarkItemRead 标记单条已读/未读。
func (s *Store) MarkItemRead(itemID string, read bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.feeds {
		for j := range s.feeds[i].Items {
			if s.feeds[i].Items[j].ID != itemID {
				continue
			}
			if s.feeds[i].Items[j].Read == read {
				return false, nil
			}
			s.feeds[i].Items[j].Read = read
			s.save()
			return true, nil
		}
	}
	return false, ErrNotFound
}

// MarkAllRead 全部标记已读,返回受影响条数。
func (s *Store) MarkAllRead() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.feeds {
		for j := range s.feeds[i].Items {
			if !s.feeds[i].Items[j].Read {
				s.feeds[i].Items[j].Read = true
				n++
			}
		}
	}
	if n > 0 {
		s.save()
	}
	return n
}

// ---------- 抓取结果落盘 ----------

// Touch 记录一次抓取尝试的结果;成功时更新站点标题/主页与校验器。
// 返回是否产生了可见变化(供 UI 版本号推进)。
func (s *Store) Touch(id string, p *Parsed, etag, lastMod string, fetchErr error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil {
		return false
	}
	f.LastFetch = time.Now()
	changed := false
	if fetchErr != nil {
		if f.LastError != fetchErr.Error() {
			f.LastError = fetchErr.Error()
			changed = true
		}
		s.save()
		return changed
	}
	if f.LastError != "" {
		f.LastError = ""
		changed = true
	}
	if etag != "" {
		f.ETag = etag
	}
	if lastMod != "" {
		f.LastMod = lastMod
	}
	if p != nil {
		if t := strings.TrimSpace(p.Title); t != "" && t != f.SiteTitle {
			f.SiteTitle = t
			changed = true
		}
		if p.Link != "" && p.Link != f.Link {
			f.Link = p.Link
			changed = true
		}
	}
	s.save()
	return changed
}

// ApplyResult 是一次条目合并的汇总。
type ApplyResult struct {
	Added     int    // 新增条数
	Changed   bool   // 是否产生可见变化(供 UI 版本号推进)
	Truncated bool   // 本次触发了保留上限截断
	Seeded    bool   // 本次是该订阅的首次抓到条目(此前为空),自动下载应跳过
	Fresh     []Item // 本次新增的条目(供自动下载调度)
}

// ApplyItems 合并一次抓取到的条目:新条目置顶(未读),已知条目更新可变字段并
// 保留已读/首次发现时间;从源里消失的旧条目保留在尾部;超出 maxItems 截断。
func (s *Store) ApplyItems(id string, items []FetchedItem, maxItems int) ApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.feed(id)
	if f == nil || len(items) == 0 {
		return ApplyResult{}
	}
	seeded := len(f.Items) == 0
	old := make(map[string]Item, len(f.Items))
	for _, it := range f.Items {
		old[it.ID] = it
	}
	out := make([]Item, 0, len(items)+len(f.Items))
	seen := make(map[string]bool, len(items))
	now := time.Now()
	updated := false
	var fresh []Item
	for _, fi := range items {
		key := itemID(f.ID, fi)
		if seen[key] {
			continue
		}
		seen[key] = true
		if prev, ok := old[key]; ok {
			if prev.Title != fi.Title || prev.Link != fi.Link || prev.Date != fi.Date ||
				prev.Summary != fi.Summary || !linksEqual(prev.Links, fi.Links) {
				prev.Title, prev.Link, prev.Date, prev.Summary, prev.Links = fi.Title, fi.Link, fi.Date, fi.Summary, fi.Links
				updated = true
			}
			out = append(out, prev)
			continue
		}
		it := Item{
			ID: key, Title: fi.Title, Link: fi.Link, Date: fi.Date,
			Summary: fi.Summary, Links: fi.Links, FirstSeen: now,
		}
		out = append(out, it)
		fresh = append(fresh, it)
	}
	// 源中已消失的旧条目:保留在尾部(历史可追溯),直到超出保留上限
	for _, it := range f.Items {
		if !seen[it.ID] {
			out = append(out, it)
		}
	}
	res := ApplyResult{Added: len(fresh), Seeded: seeded, Fresh: fresh}
	if maxItems > 0 && len(out) > maxItems {
		out = out[:maxItems]
		res.Truncated = true
	}
	if res.Added > 0 || updated || res.Truncated || len(out) != len(f.Items) {
		res.Changed = true
	}
	f.Items = out
	s.save()
	return res
}

func linksEqual(a, b []ItemLink) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itemID 由订阅 ID 与条目稳定标识生成;GUID 优先,其次链接,最后标题+时间。
func itemID(feedID string, fi FetchedItem) string {
	key := fi.GUID
	if key == "" {
		key = fi.Link
	}
	if key == "" {
		key = fi.Title + "\x00" + fi.Date
	}
	sum := sha1.Sum([]byte(feedID + "\x00" + key))
	return "i" + hex.EncodeToString(sum[:8])
}
