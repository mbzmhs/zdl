// Package nativeui 是 v3 原生桌面外壳(Fyne):与 Wails 壳、浏览器扩展同构的
// API 客户端——引擎与回环 HTTP 服务完全复用,外壳只做展示与交互。
//
// 数据契约(对齐 Vue 版 store.js,移植基准见 docs/native-ui-porting.md):
// REST + SSE /api/events 双通道,SSE update 帧为主、REST 轮询兜底;
// X-Zdl-Version 快照门控(只接受不倒退的快照,防进度条闪回)。
package nativeui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
	"zdownloader/internal/engine"
	"zdownloader/internal/rss"
)

// Client 回环 API 客户端。零外部分页/重试策略:所有调用方按 Vue 版语义
// 自行决定(列表静默失败、动作弹 toast)。
type Client struct {
	Base string       // 形如 http://127.0.0.1:7890(仅回环)
	HTTP *http.Client // 常规请求
	Long *http.Client // SSE 长连接(无总超时)
}

// New 构造客户端
func New(base string) *Client {
	return &Client{
		Base: base,
		HTTP: &http.Client{Timeout: 15 * time.Second},
		Long: &http.Client{},
	}
}

// APIError 服务端业务错误(JSON {"error": "..."})
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return e.Msg }

// do 通用请求:JSON 编解码 + 错误归一。out 为 nil 时忽略响应体。
// 快照门控所需的 X-Zdl-Version 由 SSE 通道负责;REST 兜底轮询同样带上,
// 调用方通过 snapVer 出参取回(0=响应无此头)。
func (c *Client) do(method, path string, body any, out any) (snapVer int64, err error) {
	var rd io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return 0, e
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 桌面客户端自报家门:引擎按 UA 判定建任务持久槽的消费权(浏览器标签
	// 只读不消费,防桌面目标被恰开着的浏览器截胡;Wails webview 的请求由
	// 资产服务器统一附加 wails.io 标记,与此同义)
	req.Header.Set("User-Agent", "ZDownloader-Desktop")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	v, _ := strconv.ParseInt(resp.Header.Get("X-Zdl-Version"), 10, 64)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return v, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return v, &APIError{Code: resp.StatusCode, Msg: e.Error}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return v, err
		}
	}
	return v, nil
}

/* ---------------- 基础 ---------------- */

// Ping 探活;返回后端版本(顶栏徽标)
func (c *Client) Ping() (version string, err error) {
	var p struct {
		Version string `json:"version"`
	}
	_, err = c.do("GET", "/api/ping", nil, &p)
	return p.Version, err
}

// UIState UI 进程回写守护的界面状态(窗口位置/侧栏视图/主题)。
// 分离架构下 UI 只是 API 客户端,跨会话状态统一由守护 config 承载。
type UIState struct {
	WinX, WinY, WinW, WinH int
	WinPosSet              bool
	View, Theme            string
}

// UpdateUIState 界面状态落盘(守护侧 /api/uiState → config)。失败仅影响
// 下次恢复,调用方不重试(隐藏/退出/切换各时机一次性写入)。
func (c *Client) UpdateUIState(st UIState) error {
	_, err := c.do("POST", "/api/uiState", st, nil)
	return err
}

// PostJSON 通用 POST(引导槽等一次性调用;body 序列化为 JSON)
func (c *Client) PostJSON(path string, body any) error {
	_, err := c.do("POST", path, body, nil)
	return err
}

// SummonUI 请求守护唤起/拉起 UI 窗口(裸启动转交、托盘等价路径)。守护侧
// ShowUI 钩子决定广播 uiShow 帧还是拉起新 UI 子进程,调用方不关心。与既有
// ShowUI(本地唤窗钩子,网页版语义)区分命名。
func (c *Client) SummonUI(settings bool) (ok bool, err error) {
	q := ""
	if settings {
		q = "?settings=1"
	}
	var r struct {
		Ok bool `json:"ok"`
	}
	_, err = c.do("GET", "/api/ui/show"+q, nil, &r)
	return r.Ok, err
}

// Shutdown 请求引擎优雅退出(常驻组件托盘「退出」调用):引擎广播 uiExit、
// 存任务后结束进程。
func (c *Client) Shutdown() error {
	_, err := c.do("POST", "/api/shutdown", nil, nil)
	return err
}

// Stats 总览速度与 RSS 未读(顶栏)
func (c *Client) Stats() (s Stats, err error) {
	_, err = c.do("GET", "/api/stats", nil, &s)
	return s, err
}

// Stats 顶栏统计(Vue 版 stats 口径)
type Stats struct {
	TotalSpeed   int64 `json:"totalSpeed"`
	TotalUpSpeed int64 `json:"totalUpSpeed"`
	RSSUnread    int   `json:"rssUnread"`
	DHTNodes     int   `json:"dhtNodes"`
}

/* ---------------- 任务 ---------------- */

// Tasks 任务全量快照
func (c *Client) Tasks() (list []engine.TaskView, ver int64, err error) {
	ver, err = c.do("GET", "/api/tasks", nil, &list)
	return list, ver, err
}

// CreateURL 新建 HTTP/HLS 任务(NewTaskDialog 提交体逐字段对齐)
func (c *Client) CreateURL(req engine.CreateReq) (tasks []engine.TaskView, errors []string, err error) {
	var r struct {
		Tasks  []engine.TaskView `json:"tasks"`
		Errors []string          `json:"errors"`
	}
	_, err = c.do("POST", "/api/tasks", req, &r)
	return r.Tasks, r.Errors, err
}

// CreateBT 新建 BT 任务(BTDialog 提交体:磁力/种子 b64/种子路径三选一)
func (c *Client) CreateBT(req engine.CreateReq) (tasks []engine.TaskView, errors []string, err error) {
	var r struct {
		Tasks  []engine.TaskView `json:"tasks"`
		Errors []string          `json:"errors"`
	}
	_, err = c.do("POST", "/api/tasks/bt", req, &r)
	return r.Tasks, r.Errors, err
}

// Action 任务动作(pause/resume/reseed/cancel/open/openfile/stopseed)
func (c *Client) Action(id, action string) error {
	_, err := c.do("POST", "/api/tasks/"+url.PathEscape(id)+"/"+action, nil, nil)
	return err
}

// Delete 删除任务记录;withFiles 同时删除已下载文件(确认框语义在 UI 层)
func (c *Client) Delete(id string, withFiles bool) error {
	p := "/api/tasks/" + url.PathEscape(id)
	if withFiles {
		p += "?file=1"
	}
	_, err := c.do("DELETE", p, nil, nil)
	return err
}

// UpdateTask 每任务限速(详情抽屉保存;BT 任务含上传限速)
func (c *Client) UpdateTask(id string, speedLimit, upLimit int64) error {
	body := map[string]int64{"speedLimit": speedLimit}
	if upLimit > 0 || upLimit < 0 {
		body["upLimit"] = upLimit
	}
	_, err := c.do("PUT", "/api/tasks/"+url.PathEscape(id), body, nil)
	return err
}

// Clear 清扫分类(completed/canceled)
func (c *Client) Clear(view string, withFiles bool) (removed int, err error) {
	var r struct {
		Removed int `json:"removed"`
	}
	_, err = c.do("POST", "/api/tasks/clear", map[string]any{"view": view, "file": withFiles}, &r)
	return r.Removed, err
}

// SetTaskFiles 文件选择(choosing 创建参数 / 详情改选)
func (c *Client) SetTaskFiles(id string, all bool, files []string) error {
	var body any
	if all {
		body = map[string]any{"all": true}
	} else {
		body = map[string]any{"files": files}
	}
	_, err := c.do("POST", "/api/tasks/"+url.PathEscape(id)+"/files", body, nil)
	return err
}

// Detail 任务详情快照(抽屉 1s 轮询)
func (c *Client) Detail(id string) (d engine.TaskDetailView, err error) {
	_, err = c.do("GET", "/api/tasks/"+url.PathEscape(id)+"/detail", nil, &d)
	return d, err
}

// ExportTorrent 导出种子字节(「导出种子文件」备用通道)
func (c *Client) ExportTorrent(id string) ([]byte, error) {
	resp, err := c.HTTP.Get(c.Base + "/api/tasks/" + url.PathEscape(id) + "/torrent")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &APIError{Code: resp.StatusCode, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

/* ---------------- Tracker / Peer ---------------- */

// TrackerOps tracker 操作(添加/删除/重新通告/去重),字段语义对齐 Drawer
type TrackerOps struct {
	Add        string   `json:"add,omitempty"`
	Adds       []string `json:"adds,omitempty"`
	Remove     string   `json:"remove,omitempty"`
	Reannounce bool     `json:"reannounce,omitempty"`
	Dedupe     bool     `json:"dedupe,omitempty"`
}

// TaskTrackers tracker 变更;返回统计(toast 文案用)
func (c *Client) TaskTrackers(id string, ops TrackerOps) (added, skipped int, failed []string, removed []string, err error) {
	var r struct {
		Added   int      `json:"added"`
		Skipped int      `json:"skipped"`
		Failed  []string `json:"failed"`
		Removed []string `json:"removed"`
	}
	_, err = c.do("POST", "/api/tasks/"+url.PathEscape(id)+"/trackers", ops, &r)
	return r.Added, r.Skipped, r.Failed, r.Removed, err
}

// PeerOps peer 操作
type PeerOps struct {
	Add    []string `json:"add,omitempty"`
	Remove string   `json:"remove,omitempty"` // addr(ip:port)
	Ban    string   `json:"ban,omitempty"`    // ip
}

// TaskPeers peer 变更(手动添加/断开/封锁)
func (c *Client) TaskPeers(id string, ops PeerOps) (added, banned int, err error) {
	var r struct {
		Added  int `json:"added"`
		Banned int `json:"banned"`
	}
	_, err = c.do("POST", "/api/tasks/"+url.PathEscape(id)+"/peers", ops, &r)
	return r.Added, r.Banned, err
}

/* ---------------- BT 全局 ---------------- */

// Bans 封禁记录(=GET /api/bt/bans)。服务端返回 BanEntry 数组(含时间/
// 原因/客户端/次数),此前误按 []string 解码导致 unmarshal 报错
func (c *Client) Bans() (list []bt.BanEntry, err error) {
	var r struct {
		Bans []bt.BanEntry `json:"bans"`
	}
	_, err = c.do("GET", "/api/bt/bans", nil, &r)
	return r.Bans, err
}

// BanDelete 删除单条封禁
func (c *Client) BanDelete(ip string) error {
	_, err := c.do("DELETE", "/api/bt/bans/"+url.PathEscape(ip), nil, nil)
	return err
}

// BansClear 清空封禁名单
func (c *Client) BansClear() error {
	_, err := c.do("DELETE", "/api/bt/bans", nil, nil)
	return err
}

// BanClient 按客户端名封禁(全部任务中该客户端的 peer 连同 IP 入名单)
func (c *Client) BanClient(client string) (banned int, err error) {
	var r struct {
		Banned int `json:"banned"`
	}
	_, err = c.do("POST", "/api/bt/ban-client", map[string]string{"client": client}, &r)
	return r.Banned, err
}

// ClientBlacklist 客户端黑名单读写(通配符模式列表)
func (c *Client) ClientBlacklist(patterns []string) error {
	_, err := c.do("PUT", "/api/bt/client-blacklist", patterns, nil)
	return err
}

// BTParse 解析磁力/种子(BTDialog 600ms 防抖预解析)
func (c *Client) BTParse(magnet, torrentB64, torrentPath string) (r BTParseResult, err error) {
	body := map[string]string{}
	if magnet != "" {
		body["magnet"] = magnet
	}
	if torrentB64 != "" {
		body["torrentB64"] = torrentB64
	}
	if torrentPath != "" {
		body["torrentPath"] = torrentPath
	}
	_, err = c.do("POST", "/api/bt/parse", body, &r)
	return r, err
}

// BTParseResult /api/bt/parse 应答(字段对齐 BTDialog 探针行)
type BTParseResult struct {
	InfoHash string        `json:"infoHash"`
	Name     string        `json:"name"`
	Size     int64         `json:"size"`
	Files    []BTParseFile `json:"files"`
	Trackers []string      `json:"trackers"`
	Announce string        `json:"announce"`
}

// BTParseFile 解析出的种子文件(path/length/first/last,=server handleBTParse)
type BTParseFile struct {
	Path   []string `json:"path"`
	Length int64    `json:"length"`
	First  int      `json:"first"` // 分块序号:起始 piece
	Last   int      `json:"last"`  // 结束 piece(零长文件 last<first)
}

/* ---------------- 流端点(边下边播) ---------------- */

// StreamURL 构造流地址(epoch 为会话代次,见 Vue 版 playerEpoch)
func (c *Client) StreamURL(taskID, file string, epoch int64) string {
	u := c.Base + "/api/tasks/" + url.PathEscape(taskID) + "/stream?file=" + url.QueryEscape(file) + "&v=" + strconv.FormatInt(epoch, 10)
	return u
}

// StreamStatus 流会话状态(播放页 500ms 轮询)
func (c *Client) StreamStatus(taskID string) (st engine.BTStreamStatus, err error) {
	_, err = c.do("GET", "/api/tasks/"+url.PathEscape(taskID)+"/stream/status", nil, &st)
	return st, err
}

// StreamProbe 流会话探测(Range bytes=0-0):206/200 = 就绪;其余按引擎
// 错误文案返回(未选文件/会话未运行/整校验等)。=PlayerPane probe()
func (c *Client) StreamProbe(taskID, file string, epoch int64) error {
	req, err := http.NewRequest("GET", c.StreamURL(taskID, file, epoch), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	if e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// StreamStop 关闭流会话(还原纯下载优先级)
func (c *Client) StreamStop(taskID string) error {
	_, err := c.do("POST", "/api/tasks/"+url.PathEscape(taskID)+"/stream/stop", nil, nil)
	return err
}

// StreamWarm 预热下一集头/尾元数据 piece(自动连播体验)
func (c *Client) StreamWarm(taskID, file string) error {
	_, err := c.do("POST", "/api/tasks/"+url.PathEscape(taskID)+"/stream/warm", map[string]string{"file": file}, nil)
	return err
}

/* ---------------- 配置 ---------------- */

// Config 读取配置
func (c *Client) Config() (cfg conf.Config, err error) {
	_, err = c.do("GET", "/api/config", nil, &cfg)
	return cfg, err
}

// SaveConfig 保存配置(设置页整体提交)
func (c *Client) SaveConfig(cfg conf.Config) (conf.Config, error) {
	var out conf.Config
	_, err := c.do("PUT", "/api/config", cfg, &out)
	return out, err
}

// RecentDirs 追加最近使用目录(创建成功后回写;响应为去重后的全表)
func (c *Client) AddRecentDir(dir string) (recent []string, err error) {
	var r struct {
		RecentDirs []string `json:"recentDirs"`
	}
	_, err = c.do("POST", "/api/recentdir", map[string]string{"dir": dir}, &r)
	return r.RecentDirs, err
}

// DeleteRecentDir 删除一条最近使用目录
func (c *Client) DeleteRecentDir(dir string) (recent []string, err error) {
	var r struct {
		RecentDirs []string `json:"recentDirs"`
	}
	_, err = c.do("POST", "/api/recentdir/delete", map[string]string{"dir": dir}, &r)
	return r.RecentDirs, err
}

// Probe 链接预解析(NewTaskDialog 防抖探针:文件名/大小/续传/HLS)
func (c *Client) Probe(url_, referer string, headers map[string]string, proxy string) (r ProbeResult, err error) {
	_, err = c.do("POST", "/api/probe", map[string]any{
		"url": url_, "referer": referer, "headers": headers, "proxy": proxy,
	}, &r)
	return r, err
}

// ProbeResult 探针应答
type ProbeResult struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Size         int64  `json:"size"`
	AcceptRanges bool   `json:"acceptRanges"`
	HLS          bool   `json:"hls,omitempty"`
}

/* ---------------- 系统集成 ---------------- */

// SysProxy 系统代理探测结果
type SysProxy struct {
	Source string `json:"source,omitempty"` // "env" / "windows" / "macos"
	EnvVar string `json:"envVar,omitempty"`
	URL    string `json:"url,omitempty"`
	Addr   string `json:"addr,omitempty"`
}

// LogLines 系统日志尾部(设置→系统日志;签名比对防抖动在 UI 层)
func (c *Client) LogLines(n int) (lines []string, err error) {
	var r struct {
		Lines []string `json:"lines"`
	}
	_, err = c.do("GET", fmt.Sprintf("/api/log?n=%d", n), nil, &r)
	return r.Lines, err
}

// ShowUI 唤起主窗口置前(单实例转交时由新进程调用)
func (c *Client) ShowUI() error {
	_, err := c.do("GET", "/api/ui/show", nil, nil)
	return err
}

// PendingNewTask 取走待弹创建窗槽(取走即清;语义对齐 store.fetchPendingNewTask:
// 对话框未注册前严禁消费)。seedConflict=true 时为种子冲突确认负载。
// 空槽以 204 表达(核心约定),这里归一为 empty=true。
func (c *Client) PendingNewTask() (p PendingNewTask, empty bool, err error) {
	code, err := c.doRaw("GET", "/api/ui/newtask", nil, &p)
	if err != nil {
		return p, false, err
	}
	if code == http.StatusNoContent {
		return p, true, nil
	}
	return p, false, nil
}

// PendingNewTask 待弹创建窗负载(关联启动/扩展转交/种子冲突)
type PendingNewTask struct {
	Magnet       string            `json:"magnet,omitempty"`
	TorrentPath  string            `json:"torrentPath,omitempty"`
	Dir          string            `json:"dir,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
	Stay         bool              `json:"stay,omitempty"`
	Referer      string            `json:"referer,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	SeedConflict bool              `json:"seedConflict,omitempty"`
	SeedName     string            `json:"seedName,omitempty"`
	Generated    bool              `json:"generated,omitempty"`
}

// ForwardNewTask 把启动目标转交给运行中实例(单实例/关联启动)
func (c *Client) ForwardNewTask(payload map[string]string) error {
	_, err := c.do("POST", "/api/ui/newtask", payload, nil)
	return err
}

/* ---------------- RSS ---------------- */

// RSSTree 订阅树快照(=GET /api/rss;类型复用 internal/rss)
type RSSTree struct {
	Folders  []rss.Folder   `json:"folders"`
	Feeds    []rss.FeedView `json:"feeds"`
	Unread   int            `json:"unread"`
	Updating []string       `json:"updating"`
	Busy     bool           `json:"busy"`
}

// RSS 订阅树
func (c *Client) RSS() (t RSSTree, err error) {
	_, err = c.do("GET", "/api/rss", nil, &t)
	return t, err
}

// RSSItems 某订阅的条目
func (c *Client) RSSItems(feedID string) (items []rss.Item, err error) {
	// 服务端包了一层 {items:[...]}(=handleRSSItems),直接解数组会失败
	var out struct {
		Items []rss.Item `json:"items"`
	}
	_, err = c.do("GET", "/api/rss/items?feed="+url.PathEscape(feedID), nil, &out)
	return out.Items, err
}

// RSSRefreshAll 全部更新
func (c *Client) RSSRefreshAll() error {
	_, err := c.do("POST", "/api/rss/refresh", map[string]any{}, nil)
	return err
}

// RSSReadAll 全部已读
func (c *Client) RSSReadAll() error {
	_, err := c.do("POST", "/api/rss/read", map[string]any{}, nil)
	return err
}

// RSSFeedRefresh 单订阅刷新
func (c *Client) RSSFeedRefresh(id string) error {
	_, err := c.do("POST", "/api/rss/feeds/"+url.PathEscape(id)+"/refresh", map[string]any{}, nil)
	return err
}

// RSSFeedRead 单订阅全部已读
func (c *Client) RSSFeedRead(id string) error {
	_, err := c.do("POST", "/api/rss/feeds/"+url.PathEscape(id)+"/read", map[string]bool{"read": true}, nil)
	return err
}

// RSSItemRead 条目已读/未读
func (c *Client) RSSItemRead(id string, read bool) error {
	_, err := c.do("POST", "/api/rss/items/"+url.PathEscape(id)+"/read", map[string]bool{"read": read}, nil)
	return err
}

// RSSAddFeed 添加订阅(新增后核心立即抓取一次)
func (c *Client) RSSAddFeed(url_, folderID, title, downloadDir, autoDL string) (v rss.FeedView, err error) {
	body := map[string]string{"url": url_, "folderId": folderID, "title": title,
		"downloadDir": downloadDir, "autoDl": autoDL}
	_, err = c.do("POST", "/api/rss/feeds", body, &v)
	return v, err
}

// RSSUpdateFeed 编辑订阅(指针字段 nil = 不改)
func (c *Client) RSSUpdateFeed(id string, title, folderID, downloadDir, autoDL *string) (v rss.FeedView, err error) {
	body := map[string]any{}
	if title != nil {
		body["title"] = *title
	}
	if folderID != nil {
		body["folderId"] = *folderID
	}
	if downloadDir != nil {
		body["downloadDir"] = *downloadDir
	}
	if autoDL != nil {
		body["autoDl"] = *autoDL
	}
	_, err = c.do("PUT", "/api/rss/feeds/"+url.PathEscape(id), body, &v)
	return v, err
}

// RSSDeleteFeed 删除订阅
func (c *Client) RSSDeleteFeed(id string) error {
	_, err := c.do("DELETE", "/api/rss/feeds/"+url.PathEscape(id), nil, nil)
	return err
}

// RSSFeedMove 订阅换文件夹/排序(=move:{folderId, beforeId})
func (c *Client) RSSFeedMove(id, folderID, beforeID string) error {
	_, err := c.do("POST", "/api/rss/feeds/"+url.PathEscape(id)+"/move",
		map[string]string{"folderId": folderID, "beforeId": beforeID}, nil)
	return err
}

// RSSFolderAdd 新建文件夹
func (c *Client) RSSFolderAdd(name string) error {
	_, err := c.do("POST", "/api/rss/folders", map[string]string{"name": name}, nil)
	return err
}

// RSSFolderRename 重命名文件夹
func (c *Client) RSSFolderRename(id, name string) error {
	_, err := c.do("PUT", "/api/rss/folders/"+url.PathEscape(id), map[string]string{"name": name}, nil)
	return err
}

// RSSFolderDelete 删除文件夹(订阅移至未分组)
func (c *Client) RSSFolderDelete(id string) error {
	_, err := c.do("DELETE", "/api/rss/folders/"+url.PathEscape(id), nil, nil)
	return err
}

// RSSFolderOrder 文件夹排序
func (c *Client) RSSFolderOrder(ids []string) error {
	_, err := c.do("POST", "/api/rss/folders/order", map[string]any{"ids": ids}, nil)
	return err
}

// doRaw 与 do 相同但不把 >=400 归一为错误(槽接口用 204 表达"空")
func (c *Client) doRaw(method, path string, body any, out any) (int, error) {
	req, err := http.NewRequest(method, c.Base+path, nil)
	if err != nil {
		return 0, err
	}
	// 与 do 一致:取槽(GET /api/ui/newtask)走本路径,引擎按 UA 判定桌面端消费权,
	// 缺此头会被当成浏览器标签而拒发(建任务槽永远探到空,弹窗不弹)——曾因此导致
	// 双击种子后主窗弹出却无创建窗(=用户实测"空窗口")
	req.Header.Set("User-Agent", "ZDownloader-Desktop")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// ErrOffline 核心 HTTP 服务不可达(SSE 断开/轮询失败的统一判定入口)
func (c *Client) ErrOffline(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return false
	}
	return err != nil
}
