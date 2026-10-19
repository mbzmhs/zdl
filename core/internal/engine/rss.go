package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/proxy"
	"zdownloader/internal/rss"
)

// RSS 订阅:抓取遵循全局代理/UA/TLS 设置;自动更新按设置里的间隔与时段执行,
// 手动刷新不受开关与时段限制。条目/文件夹/已读状态持久化于数据目录 rss.json。
const (
	rssFetchTimeout = 25 * time.Second
	rssMaxBody      = 8 << 20 // 订阅文件上限(远超常规 feed,防超大响应)
	rssTick         = 30 * time.Second
)

// rssState 是抓取的运行态:全局忙标记 + 进行中的订阅集合(UI 展示"更新中")。
type rssState struct {
	mu       sync.Mutex
	inflight map[string]bool
	busy     bool
}

func (st *rssState) begin(id string) {
	st.mu.Lock()
	if st.inflight == nil {
		st.inflight = map[string]bool{}
	}
	st.inflight[id] = true
	st.mu.Unlock()
}

func (st *rssState) end(id string) {
	st.mu.Lock()
	delete(st.inflight, id)
	st.mu.Unlock()
}

func (st *rssState) snapshot() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.inflight))
	for id := range st.inflight {
		out = append(out, id)
	}
	return out
}

// rssInit 创建订阅存储(与 bans.json 同目录,重启后已读/条目继续生效)。
func (m *Manager) rssInit(cfgPath string) {
	m.rssStore = rss.New(filepath.Join(filepath.Dir(cfgPath), "rss.json"))
}

// rssLoop 自动更新循环:每 30s 检查一次到期订阅("启动时立即更新"关闭时,
// 启动后先静默一个完整间隔再开始,避免开机瞬间集中抓取)。
func (m *Manager) rssLoop(ctx context.Context) {
	if !m.cfg.RSS.UpdateOnStart {
		m.rssGrace = time.Now().Add(m.cfg.RSS.Interval())
	}
	tk := time.NewTicker(rssTick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			m.rssAutoTick(ctx)
		}
	}
}

func (m *Manager) rssAutoTick(ctx context.Context) {
	cfg := m.cfg.RSS
	if !cfg.AutoUpdate || !cfg.InWindow(time.Now()) {
		return
	}
	if !m.rssGrace.IsZero() && time.Now().Before(m.rssGrace) {
		return
	}
	if !m.rssGate.CompareAndSwap(false, true) {
		return
	}
	defer m.rssGate.Store(false)
	m.rssSt.mu.Lock()
	m.rssSt.busy = true
	m.rssSt.mu.Unlock()
	defer func() {
		m.rssSt.mu.Lock()
		m.rssSt.busy = false
		m.rssSt.mu.Unlock()
	}()
	for _, ref := range m.rssStore.DueFeeds(cfg.Interval(), time.Now()) {
		if ctx.Err() != nil {
			return
		}
		m.rssRefreshOne(ctx, ref)
	}
}

// rssRefreshOne 抓取单个订阅并落盘(304 只记录抓取时间)。
func (m *Manager) rssRefreshOne(ctx context.Context, ref rss.FeedRef) {
	m.rssSt.begin(ref.ID)
	defer m.rssSt.end(ref.ID)
	fctx, cancel := context.WithTimeout(ctx, rssFetchTimeout)
	defer cancel()
	res, err := m.rssFetch(fctx, ref)
	if err != nil {
		if m.rssStore.Touch(ref.ID, nil, "", "", err) {
			m.bump()
		}
		log.Printf("[rss] 更新失败 %s — %v", ref.URL, err)
		return
	}
	if res.NotModified {
		m.rssStore.Touch(ref.ID, nil, res.ETag, res.LastModified, nil)
		return
	}
	p, perr := rss.Parse(res.Body)
	if perr != nil {
		if m.rssStore.Touch(ref.ID, nil, "", "", perr) {
			m.bump()
		}
		log.Printf("[rss] 解析失败 %s — %v", ref.URL, perr)
		return
	}
	metaChanged := m.rssStore.Touch(ref.ID, p, res.ETag, res.LastModified, nil)
	apply := m.rssStore.ApplyItems(ref.ID, p.Items, m.cfg.RSS.MaxItems)
	if apply.Added > 0 {
		log.Printf("[rss] %s 新增 %d 条", ref.URL, apply.Added)
	}
	// 自动下载:每轮更新对新增未读条目触发(该订阅首次抓到条目时跳过,
	// 防止新订阅把整个存档队列灌满)。
	if apply.Added > 0 && !apply.Seeded {
		m.rssAutoDownload(ref.ID, apply.Fresh)
	}
	if metaChanged || apply.Changed {
		m.bump()
	}
}

// rssAutoDownload 对本轮更新新增的条目执行自动下载。只取条目的数据链接
// (enclosure),条目页面 Link 不参与——"只下载数据,不下载页面文件";
// 无数据链接的条目保持未读,留给用户手动处理。生效模式按订阅设置继承
// 全局:off = 跳过,notify/silent = 创建任务并置条目已读;任务的 Source
// (rss-notify / rss-silent)供前端在完成时区分是否提醒。
func (m *Manager) rssAutoDownload(feedID string, items []rss.Item) {
	dlDir, autoDL, feedTitle, ok := m.rssStore.FeedDownloadOpts(feedID)
	if !ok {
		return
	}
	mode := autoDL
	if mode == "" || mode == "inherit" {
		mode = m.cfg.RSS.AutoDL
	}
	if mode != "notify" && mode != "silent" {
		return
	}
	source := "rss-" + mode
	created := 0
	for _, it := range items {
		if len(it.Links) == 0 {
			continue
		}
		n := 0
		for _, l := range it.Links {
			var views []TaskView
			var errs []string
			if strings.HasPrefix(strings.ToLower(l.URL), "magnet:") {
				views, errs = m.AddBT(CreateReq{Magnet: l.URL, Dir: dlDir, Source: source})
			} else {
				views, errs = m.Create(CreateReq{URL: l.URL, Dir: dlDir, Source: source})
			}
			n += len(views)
			for _, e := range errs {
				log.Printf("[rss] 自动下载失败 %s — %s", l.URL, e)
			}
		}
		if n > 0 {
			if _, err := m.rssStore.MarkItemRead(it.ID, true); err == nil {
				created += n
				log.Printf("[rss] 自动下载(%s,%s):%s — %d 个资源", feedTitle, mode, it.Title, n)
			}
		}
	}
	if created > 0 {
		m.bump() // 已读状态变化,推进未读徽章
	}
}

// rssRefreshAsync 异步刷新单个订阅(手动"刷新"按钮);结束后 bump 让徽章更新。
func (m *Manager) rssRefreshAsync(id string) {
	var ref rss.FeedRef
	found := false
	for _, r := range m.rssStore.AllFeeds() {
		if r.ID == id {
			ref, found = r, true
			break
		}
	}
	if !found {
		return
	}
	go func() {
		m.rssRefreshOne(m.rootCtx, ref)
		m.bump()
	}()
}

// rssFetch 拉取订阅内容(条件请求:ETag / Last-Modified)。
type rssFetchResult struct {
	NotModified  bool
	Body         []byte
	ETag         string
	LastModified string
}

func (m *Manager) rssFetch(ctx context.Context, ref rss.FeedRef) (*rssFetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.cfg.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if ref.ETag != "" {
		req.Header.Set("If-None-Match", ref.ETag)
	}
	if ref.LastMod != "" {
		req.Header.Set("If-Modified-Since", ref.LastMod)
	}
	cl := &http.Client{Transport: m.rssTransport()}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return &rssFetchResult{NotModified: true,
			ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, rssMaxBody))
	if err != nil {
		return nil, err
	}
	return &rssFetchResult{Body: body,
		ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}, nil
}

// rssTransport 订阅抓取专用传输层:遵循全局代理设置(HTTP 用途开关),
// 按配置缓存复用连接;UpdateConfig 清空传输层缓存时一并重建。
func (m *Manager) rssTransport() *http.Transport {
	httpOn := m.cfg.Proxy.HTTPOn()
	opts := proxy.Options{Mode: m.cfg.Proxy.Mode, URL: m.cfg.Proxy.URL, SkipTLSVerify: m.cfg.SkipTLSVerify}
	if !httpOn {
		opts.Manual = "none"
	}
	key := "rss|" + opts.Mode + ":" + opts.URL + ":" +
		strconv.FormatBool(opts.SkipTLSVerify) + ":" + strconv.FormatBool(httpOn)
	m.transportsMu.Lock()
	defer m.transportsMu.Unlock()
	if tr := m.transports[key]; tr != nil {
		return tr
	}
	tr := proxy.Transport(opts)
	m.transports[key] = tr
	return tr
}

// ---------- 供 REST API 使用的订阅操作 ----------

// RSSSnapshot 订阅树快照(附带抓取进行中的订阅,供 UI 显示"更新中")。
func (m *Manager) RSSSnapshot() map[string]any {
	tv := m.rssStore.Tree()
	m.rssSt.mu.Lock()
	busy := m.rssSt.busy
	m.rssSt.mu.Unlock()
	return map[string]any{
		"folders":  tv.Folders,
		"feeds":    tv.Feeds,
		"unread":   tv.Unread,
		"updating": m.rssSt.snapshot(),
		"busy":     busy,
	}
}

func (m *Manager) RSSItems(feedID string) ([]rss.Item, error) { return m.rssStore.Items(feedID) }

func (m *Manager) RSSAddFeed(url, folderID, title, downloadDir, autoDL string) (rss.FeedView, error) {
	v, err := m.rssStore.AddFeed(url, folderID, title, downloadDir, autoDL)
	if err != nil {
		return v, err
	}
	m.rssRefreshAsync(v.ID) // 新增后立即抓取一次,UI 轮询取回标题与条目
	return v, nil
}

func (m *Manager) RSSUpdateFeed(id string, title, folderID, downloadDir, autoDL *string) (rss.FeedView, error) {
	return m.rssStore.UpdateFeed(id, title, folderID, downloadDir, autoDL)
}

func (m *Manager) RSSRemoveFeed(id string) error { return m.rssStore.RemoveFeed(id) }

func (m *Manager) RSSMoveFeed(id, folderID, beforeFeedID string) error {
	return m.rssStore.MoveFeed(id, folderID, beforeFeedID)
}

// RSSRefresh 手动刷新:id 为空 = 全部异步;单个订阅也异步(前端轮询取结果)。
func (m *Manager) RSSRefresh(id string) error {
	if id != "" {
		if _, ok := m.rssStore.FeedURL(id); !ok {
			return rss.ErrNotFound
		}
		m.rssRefreshAsync(id)
		return nil
	}
	if !m.rssGate.CompareAndSwap(false, true) {
		return nil // 已有全量刷新在跑
	}
	go func() {
		defer m.rssGate.Store(false)
		m.rssSt.mu.Lock()
		m.rssSt.busy = true
		m.rssSt.mu.Unlock()
		defer func() {
			m.rssSt.mu.Lock()
			m.rssSt.busy = false
			m.rssSt.mu.Unlock()
		}()
		for _, ref := range m.rssStore.AllFeeds() {
			if m.rootCtx.Err() != nil {
				return
			}
			m.rssRefreshOne(m.rootCtx, ref)
		}
		m.bump()
	}()
	return nil
}

func (m *Manager) RSSMarkFeedRead(id string, read bool) (int, error) {
	n, err := m.rssStore.MarkFeedRead(id, read)
	if n > 0 {
		m.bump()
	}
	return n, err
}

func (m *Manager) RSSMarkItemRead(id string, read bool) (bool, error) {
	ok, err := m.rssStore.MarkItemRead(id, read)
	if ok {
		m.bump()
	}
	return ok, err
}

func (m *Manager) RSSMarkAllRead() int {
	n := m.rssStore.MarkAllRead()
	if n > 0 {
		m.bump()
	}
	return n
}

func (m *Manager) RSSAddFolder(name string) (rss.Folder, error) { return m.rssStore.AddFolder(name) }
func (m *Manager) RSSRenameFolder(id, name string) error        { return m.rssStore.RenameFolder(id, name) }
func (m *Manager) RSSRemoveFolder(id string) error              { return m.rssStore.RemoveFolder(id) }
func (m *Manager) RSSReorderFolders(ids []string) error         { return m.rssStore.ReorderFolders(ids) }

// rssUnread 未读合计(进入 /api/stats 与 SSE 快照,供侧栏徽章实时显示)。
func (m *Manager) rssUnread() int32 {
	if m.rssStore == nil {
		return 0
	}
	return int32(m.rssStore.Unread())
}
