// Package server 提供 REST API、SSE 实时推送与内嵌 Web UI。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/engine"
	"zdownloader/internal/hub"
	"zdownloader/internal/logbuf"
	"zdownloader/internal/proxy"
	"zdownloader/internal/rss"
)

const Version = "2.0.0"

type Server struct {
	mgr *engine.Manager
	hub *hub.Hub
	web fs.FS
	// OnExit 引擎进程收尾钩子(zdl main 注入 ctx cancel):/api/shutdown 走完
	// 存盘后结束进程。引擎已无头化,生命周期完全由常驻组件(shell)驱动。
	OnExit func()
	// ShowUI 可选唤起钩子:Wails 单体内嵌壳注入(扩展/转交直接弹它的窗口);
	// 无头引擎(zdl)不注入,走 uiShow 中继由订阅 SSE 的桌面端自行响应。
	ShowUI func(settings bool)
	// uiClients 经 SSE ?client=ui 自报家门的 UI 进程连接数:/api/ui/show 据此
	// 如实回 ok/desktop|web,浏览器扩展无桌面 UI 时回退为浏览器打开管理页。
	uiClients atomic.Int64

	// 待打开建任务窗的单槽:浏览器扩展(右键/快加/嗅探)请求弹「新建任务」窗时
	// 写入,前端在窗口获焦/重连/收到 openNewTask 帧时 GET 消费一次再弹窗。
	// 用持久槽而非带负载的瞬时 SSE 帧:窗口隐藏时 WebView2 挂起 JS,瞬时帧可能被
	// 丢弃;槽持久,唤窗到焦点后必能取到(见 handleOpenNewTask)。
	ntMu      sync.Mutex
	ntPending *newTaskPending
}

// newTaskPending 是「请前端打开新建任务窗 / 磁力种子窗 / 弹种子冲突确认框」的待办,
// 携带要预填或展示的内容。URLs 用于普通链接窗;Magnet/TorrentPath 用于 BT 窗(预填
// 磁力链接或 .torrent 文件);SeedConflict 用于"已存在同名种子"确认框(由用户决定
// 是否仍要创建)。BT 目标只弹窗预填、不自动建任务,待用户点击「创建任务」。
type newTaskPending struct {
	URLs        []string          `json:"urls"`
	Referer     string            `json:"referer,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Stay        bool              `json:"stay"`
	Magnet      string            `json:"magnet,omitempty"`
	TorrentPath string            `json:"torrentPath,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	// SeedConflict 非空表示这是一次"种子自动打开冲突"提醒:种子已下载完成,但列表
	// 里已存在同 InfoHash 的 BT 任务,未自动创建。前端据此弹确认框,由用户点「创建
	// 任务」(仍用 TorrentPath/Dir 建)或「取消」忽略。SeedName 用于展示种子名。
	SeedConflict bool   `json:"seedConflict,omitempty"`
	SeedName     string `json:"seedName,omitempty"`
	// Generated 标记该冲突种子是"本程序生成的"(HTTP 下载完成的 .torrent),前端
	// 强行创建时据此带 torrentGenerated,引擎才按 DeleteGeneratedTorrent 分流删除,
	// 而非误当作用户源文件。
	Generated bool `json:"generated,omitempty"`
}

func New(mgr *engine.Manager, h *hub.Hub, web fs.FS) *Server {
	return &Server{mgr: mgr, hub: h, web: web}
}

// StartAggregator 周期性检查版本号,有变化则向所有 SSE 客户端广播全量快照。
// 两个约束:
//   - 检查间隔(100ms)必须显著小于 manager 的 500ms 周期 bump:曾同为
//     500ms,两个时钟相位漂移,部分检查窗口恰好错过 bump,帧间隔在
//     500ms/1000ms 间跳变(实测约 1/3 窗口 miss)。
//   - 最小广播间隔(400ms):除周期 bump 外,peer/tracker 等状态事件也
//     会零散 bump,紧跟周期 bump 时两帧仅相距几十 ms(前端"两帧很近")。
//     节流后帧间隔下限 400ms;被节流的版本不推进 last,下一窗口补发。
func (s *Server) StartAggregator(ctx <-chan struct{}) {
	const minGap = 400 * time.Millisecond
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	last := uint64(0)
	var lastSent time.Time
	for {
		select {
		case <-ctx:
			return
		case <-tk.C:
		}
		v := s.mgr.Version()
		if v == last {
			continue
		}
		if !lastSent.IsZero() && time.Since(lastSent) < minGap {
			continue
		}
		last = v
		lastSent = time.Now()
		payload, err := s.snapshotFrame()
		if err != nil {
			continue
		}
		s.hub.Broadcast(payload)
	}
}

type snapshotMsg struct {
	Type    string            `json:"type"`
	Version uint64            `json:"version"` // 单调递增;前端据此丢弃乱序旧快照(修复进度条闪回)
	Stats   map[string]any    `json:"stats"`
	Tasks   []engine.TaskView `json:"tasks"`
}

func (s *Server) snapshotMsg() snapshotMsg {
	// 版本号取自快照之前:bump 先于可见状态变化完成时,快照数据 ≥ 版本;
	// 丢弃判断只会漏判(多渲染一帧),绝不会丢弃更新的帧
	v := s.mgr.Version()
	return snapshotMsg{Type: "update", Version: v, Stats: s.mgr.Stats(), Tasks: s.mgr.List()}
}

func (s *Server) snapshotFrame() ([]byte, error) {
	b, err := json.Marshal(s.snapshotMsg())
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), append(b, '\n', '\n')...), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", s.handlePing)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/tasks", s.handleList)
	mux.HandleFunc("POST /api/tasks", s.handleCreate)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGet)
	mux.HandleFunc("GET /api/tasks/{id}/torrent", s.handleExportTorrent)
	mux.HandleFunc("PUT /api/tasks/{id}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.handleDelete)
	mux.HandleFunc("POST /api/tasks/{id}/{action}", s.handleAction)
	mux.HandleFunc("POST /api/tasks/clear", s.handleClear)
	mux.HandleFunc("POST /api/tasks/bt", s.handleCreateBT)
	mux.HandleFunc("POST /api/bt/parse", s.handleBTParse)
	mux.HandleFunc("POST /api/tasks/{id}/stopseed", s.handleStopSeed)
	mux.HandleFunc("GET /api/tasks/{id}/stream", s.handleBTStream)
	mux.HandleFunc("GET /api/tasks/{id}/stream/status", s.handleBTStreamStatus)
	mux.HandleFunc("POST /api/tasks/{id}/stream/stop", s.handleBTStreamStop)
	mux.HandleFunc("POST /api/tasks/{id}/stream/warm", s.handleBTStreamWarm)
	mux.HandleFunc("POST /api/tasks/{id}/files", s.handleTaskFiles)
	mux.HandleFunc("GET /api/tasks/{id}/detail", s.handleTaskDetail)
	mux.HandleFunc("POST /api/tasks/{id}/trackers", s.handleTaskTrackers)
	mux.HandleFunc("POST /api/tasks/{id}/peers", s.handleTaskPeers)
	mux.HandleFunc("GET /api/rss", s.handleRSS)
	mux.HandleFunc("GET /api/rss/items", s.handleRSSItems)
	mux.HandleFunc("POST /api/rss/refresh", s.handleRSSRefresh)
	mux.HandleFunc("POST /api/rss/read", s.handleRSSReadAll)
	mux.HandleFunc("POST /api/rss/feeds", s.handleRSSFeedAdd)
	mux.HandleFunc("PUT /api/rss/feeds/{id}", s.handleRSSFeedUpdate)
	mux.HandleFunc("DELETE /api/rss/feeds/{id}", s.handleRSSFeedDelete)
	mux.HandleFunc("POST /api/rss/feeds/{id}/refresh", s.handleRSSFeedRefresh)
	mux.HandleFunc("POST /api/rss/feeds/{id}/read", s.handleRSSFeedRead)
	mux.HandleFunc("POST /api/rss/feeds/{id}/move", s.handleRSSFeedMove)
	mux.HandleFunc("POST /api/rss/items/{id}/read", s.handleRSSItemRead)
	mux.HandleFunc("POST /api/rss/folders", s.handleRSSFolderAdd)
	mux.HandleFunc("PUT /api/rss/folders/{id}", s.handleRSSFolderUpdate)
	mux.HandleFunc("DELETE /api/rss/folders/{id}", s.handleRSSFolderDelete)
	mux.HandleFunc("POST /api/rss/folders/order", s.handleRSSFolderOrder)
	mux.HandleFunc("GET /api/bt/bans", s.handleBans)
	mux.HandleFunc("DELETE /api/bt/bans/{ip}", s.handleBanDelete)
	mux.HandleFunc("DELETE /api/bt/bans", s.handleBansClear)
	mux.HandleFunc("POST /api/bt/ban-client", s.handleBanClient)
	mux.HandleFunc("PUT /api/bt/client-blacklist", s.handleClientBlacklist)
	mux.HandleFunc("GET /api/log", s.handleLog)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("GET /api/sysproxy", s.handleSysProxy)
	mux.HandleFunc("POST /api/recentdir", s.handleAddRecentDir)
	mux.HandleFunc("POST /api/recentdir/delete", s.handleDeleteRecentDir)
	mux.HandleFunc("POST /api/probe", s.handleProbe)
	mux.HandleFunc("GET /api/goroutines", s.handleGoroutines)
	mux.HandleFunc("GET /api/ui/show", s.handleShowUI)
	mux.HandleFunc("POST /api/ui/show", s.handleShowUI)
	mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
	mux.HandleFunc("POST /api/uiState", s.handleUIState)
	mux.HandleFunc("POST /api/ui/newtask", s.handleOpenNewTask)
	mux.HandleFunc("GET /api/ui/newtask", s.handleTakeNewTask)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("/", s.handleStatic)
	return s.cors(mux)
}

// cors 为 /api 前缀补齐跨域头(扩展 host_permissions 已放行,此处兜底)。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- SSE ----------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if frame, err := s.snapshotFrame(); err == nil {
		if _, err := w.Write(frame); err != nil {
			return
		}
		fl.Flush()
	}

	// UI 进程自报家门(?client=ui):供 /api/ui/show 判断是否有桌面 UI 可唤
	if r.URL.Query().Get("client") == "ui" {
		s.uiClients.Add(1)
		defer s.uiClients.Add(-1)
	}

	ch, cancel := s.hub.Subscribe()
	defer cancel()
	// 保活心跳:SSE 空闲期(无任务变化=零帧)会被部分网络环境当作死连接回收
	// (实测:回环连接静默数分钟即被掐,引擎误判 UI 离线,托盘中继退化成
	// "拉新进程"慢路径,唤窗延迟数秒)。": ping" 是 SSE 规范的注释帧,全部
	// 消费方(浏览器 EventSource / Go 侧 Scanner)读入后按行忽略。
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case msg, ok := <-ch:
			if !ok {
				return // hub CloseAll(退出流程):立即收尾连接,让 Shutdown 不用干等
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// CloseSSE 掐断全部 SSE 长连接(退出流程用):http.Server.Shutdown 会等
// 活跃连接空闲,SSE 永不空闲,不先关闭它 Shutdown 就要干等满超时。
func (s *Server) CloseSSE() { s.hub.CloseAll() }

// ---------- REST ----------

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": "ZDownloader", "version": Version,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	// 版本号经响应头返回(保持响应体为数组,扩展兼容)
	w.Header().Set("X-Zdl-Version", fmt.Sprint(s.mgr.Version()))
	writeJSON(w, http.StatusOK, s.mgr.List())
}

// handleExportTorrent 导出任务种子文件(.torrent 二进制),供前端「另存为种子」
// 下载。默认文件名=任务名(服务端置 Content-Disposition,前端再兜底命名)。
func (s *Server) handleExportTorrent(w http.ResponseWriter, r *http.Request) {
	filename, data, err := s.mgr.ExportTorrent(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(data)
}

// handleTaskDetail 任务详情侧边栏快照。
func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	d, err := s.mgr.TaskDetail(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleTaskTrackers 任务级 tracker 管理:{add} / {adds:[]} / {remove} / {reannounce:true}。
func (s *Server) handleTaskTrackers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Add        string   `json:"add"`
		Adds       []string `json:"adds"`
		Remove     string   `json:"remove"`
		Reannounce bool     `json:"reannounce"`
		Dedupe     bool     `json:"dedupe"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	var err error
	switch {
	case len(req.Adds) > 0:
		// 批量添加:逐条入库,部分失败不算整体失败,返回失败明细供前端提示;
		// 已存在(同 host:port,含不同协议)的计入 skipped,不算失败
		added, skipped := 0, 0
		var failed []string
		for _, u := range req.Adds {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") &&
				!strings.HasPrefix(u, "udp://") {
				failed = append(failed, u)
				continue
			}
			ok, e := s.mgr.AddTaskTracker(id, u)
			if e != nil {
				failed = append(failed, u)
				if err == nil {
					err = e
				}
			} else if ok {
				added++
			} else {
				skipped++
			}
		}
		if added == 0 && skipped == 0 && err != nil {
			writeErr(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "skipped": skipped, "failed": failed})
		return
	case req.Add != "":
		_, err = s.mgr.AddTaskTracker(id, req.Add)
	case req.Remove != "":
		err = s.mgr.RemoveTaskTracker(id, req.Remove)
	case req.Dedupe:
		// 去重:同 host:port(不同协议 scheme)每组保留一个
		var removed []string
		if removed, err = s.mgr.DedupeTaskTrackers(id); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
			return
		}
	case req.Reannounce:
		err = s.mgr.ReannounceTask(id)
	default:
		err = fmt.Errorf("缺少 add/remove/reannounce/dedupe")
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTaskPeers 任务级 peer 操作:{ban:"ip:port"} 封锁 IP 并断开,
// {remove:"ip:port"} 仅断开当前连接,{add:["ip:port",…]} 手动添加 peer
// (立即拨号,来源标记"手动")。
func (s *Server) handleTaskPeers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ban    string   `json:"ban"`
		Remove string   `json:"remove"`
		Add    []string `json:"add"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	var err error
	switch {
	case len(req.Add) > 0:
		var n int
		if n, err = s.mgr.AddTaskPeers(id, req.Add); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": n})
			return
		}
	case req.Ban != "":
		err = s.mgr.BanTaskPeer(id, req.Ban)
	case req.Remove != "":
		err = s.mgr.KickTaskPeer(id, req.Remove)
	default:
		err = fmt.Errorf("缺少 add/ban/remove")
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- RSS 订阅 ----------

// handleRSS 订阅树快照(文件夹 + 订阅 + 未读 + 进行中抓取)。
func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.RSSSnapshot())
}

// handleRSSItems 某订阅的条目列表(?feed=ID)。
func (s *Server) handleRSSItems(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("feed")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 feed 参数")
		return
	}
	items, err := s.mgr.RSSItems(id)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleRSSFeedAdd 新增订阅 {url, folderId?, title?, downloadDir?, autoDl?};随后自动抓取一次。
func (s *Server) handleRSSFeedAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL         string `json:"url"`
		FolderID    string `json:"folderId"`
		Title       string `json:"title"`
		DownloadDir string `json:"downloadDir"`
		AutoDL      string `json:"autoDl"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	v, err := s.mgr.RSSAddFeed(req.URL, req.FolderID, req.Title, req.DownloadDir, req.AutoDL)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleRSSFeedUpdate 修改订阅 {title?, folderId?, downloadDir?, autoDl?}。
func (s *Server) handleRSSFeedUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       *string `json:"title"`
		FolderID    *string `json:"folderId"`
		DownloadDir *string `json:"downloadDir"`
		AutoDL      *string `json:"autoDl"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	v, err := s.mgr.RSSUpdateFeed(r.PathValue("id"), req.Title, req.FolderID, req.DownloadDir, req.AutoDL)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleRSSFeedDelete 删除订阅(仅删订阅记录,不影响已创建任务)。
func (s *Server) handleRSSFeedDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RSSRemoveFeed(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRSSFeedMove 移动订阅 {folderId, beforeId?}:拖拽排序/换文件夹。
func (s *Server) handleRSSFeedMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID string `json:"folderId"`
		BeforeID string `json:"beforeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.mgr.RSSMoveFeed(r.PathValue("id"), req.FolderID, req.BeforeID); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRSSRefresh 刷新订阅:id 为空 = 全部;POST /api/rss/feeds/{id}/refresh 走单个。
func (s *Server) handleRSSRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RSSRefresh(""); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRSSFeedRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RSSRefresh(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRSSFeedRead 标记整个订阅已读/未读 {read}。
func (s *Server) handleRSSFeedRead(w http.ResponseWriter, r *http.Request) {
	read, ok := decodeReadFlag(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	n, err := s.mgr.RSSMarkFeedRead(r.PathValue("id"), read)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

// handleRSSItemRead 标记单条已读/未读 {read}。
func (s *Server) handleRSSItemRead(w http.ResponseWriter, r *http.Request) {
	read, ok := decodeReadFlag(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ok2, err := s.mgr.RSSMarkItemRead(r.PathValue("id"), read)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": ok2})
}

// handleRSSReadAll 全部标记已读。
func (s *Server) handleRSSReadAll(w http.ResponseWriter, r *http.Request) {
	n := s.mgr.RSSMarkAllRead()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

func decodeReadFlag(r *http.Request) (bool, bool) {
	var req struct {
		Read *bool `json:"read"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || req.Read == nil {
		return false, false
	}
	return *req.Read, true
}

// handleRSSFolderAdd 新建文件夹 {name}。
func (s *Server) handleRSSFolderAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	f, err := s.mgr.RSSAddFolder(req.Name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// handleRSSFolderUpdate 重命名文件夹 {name}。
func (s *Server) handleRSSFolderUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.mgr.RSSRenameFolder(r.PathValue("id"), req.Name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRSSFolderDelete 删除文件夹(其中订阅回到未分组)。
func (s *Server) handleRSSFolderDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RSSRemoveFolder(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRSSFolderOrder 文件夹排序 {ids:[...]}(拖拽后提交完整顺序)。
func (s *Server) handleRSSFolderOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.mgr.RSSReorderFolders(req.IDs); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBans 全局封禁名单(GET /api/bt/bans)。
func (s *Server) handleBans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"bans": s.mgr.Bans()})
}

// handleBanDelete 解除单个 IP 的封禁(DELETE /api/bt/bans/{ip})。
func (s *Server) handleBanDelete(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if ip == "" {
		writeErr(w, http.StatusBadRequest, "缺少 IP")
		return
	}
	if !s.mgr.BanRemove(ip) {
		writeErr(w, http.StatusNotFound, "封禁记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBansClear 清空全局封禁名单(DELETE /api/bt/bans)。
func (s *Server) handleBansClear(w http.ResponseWriter, r *http.Request) {
	n := s.mgr.BansClear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// handleBanClient 按客户端名封禁其全部现存 peer(POST /api/bt/ban-client,
// 连接详情右键「封禁所有该客户端」)。
func (s *Server) handleBanClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Client)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少客户端名")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"banned":  s.mgr.BanClientName(name),
		"pattern": bt.ClientBlacklistPattern(name),
	})
}

// handleClientBlacklist 更新客户端黑名单模式表(PUT /api/bt/client-blacklist,
// 设置模态窗增删改后保存):落盘配置并对运行中会话热生效。
func (s *Server) handleClientBlacklist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Patterns []string `json:"patterns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	patterns := s.mgr.UpdateClientBlacklist(req.Patterns)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "patterns": patterns})
}

// handleLog 系统日志尾部(?n=200)。
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		fmt.Sscanf(v, "%d", &n)
		n = max(10, min(n, 800))
	}
	buf := logbuf.Default()
	writeJSON(w, http.StatusOK, map[string]any{
		"lines":    buf.Tail(n),
		"dropped":  buf.Dropped(),
		"overflow": buf.Dropped() > 0,
	})
}

type createResp struct {
	Tasks  []engine.TaskView `json:"tasks"`
	Errors []string          `json:"errors,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req engine.CreateReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	views, errs := s.mgr.Create(req)
	// 需用户选保存目录的创建(关联启动转发/磁链建任务):由服务端主动唤起并
	// 置前原生窗口。窗口被隐藏时 WebView2 会挂起窗口内 JS,SSE/轮询不被唤醒,
	// 若靠前端发现新任务再唤窗,隐藏实例会一直卡在隐藏态(双击种子文件唤不
	// 出主界面即此)。故服务端直接唤窗,再交由已唤醒的前端弹创建对话框。
	if req.Choose {
		// 无头引擎不认识窗口:广播 uiShow 帧,由订阅 SSE 的 zdlui 显示自己
		// (未驻留时由常驻组件负责拉起)
		s.hub.Broadcast(UIShowFrame(false))
	}
	writeJSON(w, http.StatusOK, createResp{Tasks: views, Errors: errs})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	v, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, engine.ErrNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleUpdateTask 修改未完成任务的可变属性(当前:每任务限速,运行中即时生效)。
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpeedLimit *int64 `json:"speedLimit"`
		UPLimit    *int64 `json:"upLimit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil || req.SpeedLimit == nil {
		writeErr(w, http.StatusBadRequest, "缺少 speedLimit")
		return
	}
	id := r.PathValue("id")
	upLimit := int64(-1)
	if req.UPLimit != nil {
		upLimit = *req.UPLimit
	}
	if err := s.mgr.Update(id, *req.SpeedLimit, upLimit); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	v, ok := s.mgr.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, engine.ErrNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleClear 批量清扫一个分类的任务(仅允许已完成/已取消)。
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		View string `json:"view"` // completed | trash
		File bool   `json:"file"` // 连同已下载文件一起删除
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	status := engine.StatusCompleted
	switch req.View {
	case "completed":
	case "trash":
		status = engine.StatusCanceled
	default:
		writeErr(w, http.StatusBadRequest, "仅允许清扫已完成或已取消分类")
		return
	}
	n, err := s.mgr.Clear(status, req.File)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// handleCreateBT 创建磁力/种子任务。
func (s *Server) handleCreateBT(w http.ResponseWriter, r *http.Request) {
	var req engine.CreateReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 34<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	views, errs := s.mgr.AddBT(req)
	// 关联启动转发/磁链建任务(Choose=需选保存目录):服务端直接唤窗取焦。
	// 隐藏实例的窗口 JS 被挂起,不会自己发现新任务再唤窗,须由此处兜底唤起。
	if req.Choose {
		// 无头引擎不认识窗口:广播 uiShow 帧,由订阅 SSE 的 zdlui 显示自己
		// (未驻留时由常驻组件负责拉起)
		s.hub.Broadcast(UIShowFrame(false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "tasks": views, "errors": errs})
}

// handleBTParse 预解析磁链/种子(创建对话框展示名称/大小/文件树,不入队)。
func (s *Server) handleBTParse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Magnet      string `json:"magnet"`
		TorrentB64  string `json:"torrentB64"`
		TorrentPath string `json:"torrentPath"` // 本机 .torrent 路径(磁链产物预填)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 34<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	type btFileInfo struct {
		Path   []string `json:"path"`
		Length int64    `json:"length"`
		First  int      `json:"first"` // 分块序号:起始 piece
		Last   int      `json:"last"`  // 结束 piece(零长文件 last<first)
	}
	var infoHash, name, announce string
	var total int64
	var files []btFileInfo
	var private bool
	var trackers []string
	readTorrent := func(raw []byte) error {
		meta, err := bt.ParseTorrent(raw)
		if err != nil {
			return err
		}
		infoHash = fmt.Sprintf("%x", meta.InfoHash)
		name, total, private = meta.Name, meta.TotalLength, meta.Private
		for i, f := range meta.Files {
			first, last := meta.FilePieceRange(i)
			files = append(files, btFileInfo{Path: f.Path, Length: f.Length, First: first, Last: last})
		}
		announce = meta.Announce
		for _, tier := range meta.AnnounceList {
			trackers = append(trackers, tier...)
		}
		return nil
	}
	switch {
	case strings.TrimSpace(req.TorrentPath) != "":
		raw, err := os.ReadFile(strings.TrimSpace(req.TorrentPath))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "种子文件读取失败: "+err.Error())
			return
		}
		if err := readTorrent(raw); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case strings.TrimSpace(req.TorrentB64) != "":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.TorrentB64))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "种子文件解码失败: "+err.Error())
			return
		}
		if err := readTorrent(raw); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case strings.TrimSpace(req.Magnet) != "":
		mg, err := bt.ParseMagnet(req.Magnet)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		infoHash = fmt.Sprintf("%x", mg.InfoHash)
		name = mg.Name
		trackers = mg.Trackers
		total = -1 // 磁链大小未知,取到元数据后可见
	default:
		writeErr(w, http.StatusBadRequest, "缺少 magnet 或 torrentB64")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"infoHash": infoHash, "name": name, "size": total,
		"files": files, "private": private, "announce": announce,
		"trackers": trackers,
	})
}

// handleTaskFiles 落定 BT 文件选择(choosing 确认 / paused 改选)。
func (s *Server) handleTaskFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []string `json:"files"`
		All   bool     `json:"all"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	v, err := s.mgr.ChooseBTFiles(r.PathValue("id"), req.Files, req.All)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleStopSeed 停止做种。
func (s *Server) handleStopSeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.StopSeeding(id); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	delFile := r.URL.Query().Get("file") == "1"
	if err := s.mgr.Remove(r.PathValue("id"), delFile); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "pause":
		err = s.mgr.Pause(id)
	case "resume":
		err = s.mgr.Resume(id)
	case "cancel":
		err = s.mgr.Cancel(id)
	case "open":
		err = s.mgr.OpenLocation(id)
	case "openfile":
		err = s.mgr.OpenFile(id)
	case "reseed":
		err = s.mgr.ReseedTask(id)
	default:
		err = fmt.Errorf("未知操作 %q", r.PathValue("action"))
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	v, ok := s.mgr.Get(id)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Config())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var cfg json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	// 保留端口等仅服务端可改字段:前端不传 port 时沿用现值
	merged := s.mgr.Config()
	if err := json.Unmarshal(cfg, &merged); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mgr.UpdateConfig(merged); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Config())
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Stats())
}

// handleSysProxy 回显"跟随系统"实际检测到的代理,供前端(设置页只读回显)展示。
// Wails/浏览器走 Vue 前端,经此接口实时读系统代理;Fyne 原生设置页则进程内直调
// proxy.FollowSystemStatus()。字段(source/envVar/addr/url)与前端 refreshFollowProxy
// 读取的键一一对应,未检测到时 source 为空串,前端据此回显"未检测到代理(将直连)"。
func (s *Server) handleSysProxy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proxy.FollowSystemStatus())
}

// handleAddRecentDir 记录最近使用的保存目录(前端在创建任务成功后调用,只记
// 用户手动选择/输入的路径):后端去重、最新靠前、落盘,返回完整列表供下拉展示。
func (s *Server) handleAddRecentDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	cfg := s.mgr.AddRecentDir(req.Dir)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recentDirs": cfg.RecentDirs})
}

// handleDeleteRecentDir 删除一条最近使用的保存目录(下拉内逐条清除按钮),返回最新列表。
func (s *Server) handleDeleteRecentDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	cfg := s.mgr.RemoveRecentDir(req.Dir)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recentDirs": cfg.RecentDirs})
}

// handleProbe 预解析下载链接(合法性/文件名/大小/断点续传),供新建任务对话框实时提示。
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Referer string            `json:"referer"`
		Headers map[string]string `json:"headers"`
		Proxy   string            `json:"proxy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.URL == "" {
		writeErr(w, http.StatusBadRequest, "缺少 url")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.mgr.Probe(ctx, req.URL, req.Referer, req.Headers, req.Proxy))
}

// handleGoroutines 全量 goroutine 转储(诊断拨号/会话卡死用)。
func (s *Server) handleGoroutines(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(buf[:n])
}

// handleShowUI 唤起桌面 UI。引擎已无头化、不认识任何窗口:把唤起请求以
// uiShow 帧广播给订阅 SSE 的 zdlui(常驻隐藏时秒回;未驻留时由常驻组件
// shell 拉起新 zdlui)。有无附着 UI 经 SSE ?client=ui 计数判断,无 UI 时
// 如实回 ui:web,浏览器扩展据此回退为在浏览器打开管理页。
func (s *Server) handleShowUI(w http.ResponseWriter, r *http.Request) {
	settings := r.URL.Query().Get("settings") == "1"
	if s.ShowUI != nil { // 内嵌壳:窗口同进程,直接弹
		s.ShowUI(settings)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ui": "desktop"})
		return
	}
	// 广播按附着状态分流:有 UI → uiShow(窗口进程置前自己);无 UI → uiSpawn
	// (常驻组件拉起新窗口进程)。两种帧消费者不同,绝不互答——否则 shell 响应
	// uiShow 调本接口再广播 uiShow,回环疯狂唤窗(实测)
	attached := s.uiClients.Load() > 0
	s.hub.Broadcast(s.uiSummonFrame(settings, attached))
	if attached {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ui": "desktop"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "ui": "web"})
}

// uiSummonFrame 按附着状态选帧:attached=uiShow(窗口置前)/ 否则 uiSpawn(拉起)
func (s *Server) uiSummonFrame(settings, attached bool) []byte {
	if attached {
		return UIShowFrame(settings)
	}
	return UISpawnFrame(settings)
}

// handleShutdown 引擎优雅退出(常驻组件托盘「退出」调用):先广播 uiExit 让
// UI 进程自行收尾,再停 SSE/服务、存任务,最后经 OnExit 结束进程。zdl 无头化
// 后没有托盘,这是引擎唯一的退出入口。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	s.hub.Broadcast(UIExitFrame)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() {
		s.CloseSSE()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.mgr.Shutdown(shutdownCtx)
		if s.OnExit != nil {
			s.OnExit()
		}
	}()
}

// UIShowFrame 构造「唤起 UI 窗口」的 SSE 帧(settings=直达设置页)。分离架构下
// UI 是独立进程,守护侧 ShowUI 钩子对已驻留的隐藏 UI 只能经 SSE 送达唤起请求
// (入口层 daemon.go 直接广播,故导出)。
func UIShowFrame(settings bool) []byte {
	return []byte("data: " + fmt.Sprintf(`{"type":"uiShow","settings":%v}`, settings) + "\n\n")
}

// handleUIState UI 进程回写界面状态(窗口位置/侧栏视图/主题),守护侧落盘到
// config:分离架构下 UI 只是 API 客户端,窗口位置、当前视图、主题这些跨会话
// 状态统一经此汇入守护配置,新 UI 进程启动时经 GET /api/config 取回恢复。
// 字段按需更新:零值字段跳过,避免部分回写互相覆盖。
func (s *Server) handleUIState(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		WinX, WinY, WinW, WinH int
		WinPosSet              bool
		View, Theme            string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	if req.WinPosSet && req.WinW > 0 && req.WinH > 0 {
		s.mgr.UpdateWindowPos(req.WinX, req.WinY, req.WinW, req.WinH)
	}
	if req.View != "" {
		s.mgr.UpdateLastView(req.View)
	}
	if req.Theme != "" {
		s.mgr.UpdateTheme(req.Theme)
		// 主题变化随 SSE 广播,常驻组件(托盘)据此同步菜单明暗
		s.hub.Broadcast(themeFrame(req.Theme))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// UIExitFrame 通知 UI 进程「守护即将退出,请关闭自己」的轻量 SSE 帧:分离架构
// 下守护退出时 UI 进程若继续存活,会变成指向死接口的孤儿窗口,故退出前广播
// 一帧让 UI 进程自行收尾(存位置、卸钩子、退进程)。窗口隐藏时本帧可能丢,
// 兜底是守护最后 kill 子进程。(入口层 daemon.go 直接广播,故导出)
var UIExitFrame = []byte("data: " + `{"type":"uiExit"}` + "\n\n")

// UISpawnFrame 唤起帧的无 UI 变体:消费者是常驻组件(拉起新窗口进程),
// 与 uiShow(消费者是已附着的窗口进程,置前自己)严格分离,防止 shell 响应
// uiShow 调 /api/ui/show 再广播 uiShow 的回环
func UISpawnFrame(settings bool) []byte {
	return []byte("data: " + fmt.Sprintf(`{"type":"uiSpawn","settings":%v}`+"\n\n", settings))
}

// themeFrame 主题变化帧:常驻组件(托盘菜单明暗跟随)与任意感兴趣方消费
func themeFrame(theme string) []byte {
	return []byte("data: " + fmt.Sprintf(`{"type":"theme","theme":%q}`+"\n\n", theme))
}

// openNewTaskFrame 是提示前端「去取槽弹窗」的轻量 SSE 帧:不带负载,仅唤醒前端
// 走 GET /api/ui/newtask 消费持久槽。窗口隐藏/挂起时本帧可能丢,但槽仍在,前端
// 唤窗获焦或重连时会再取,故不依赖本帧是否送达。
var openNewTaskFrame = []byte("data: " + `{"type":"openNewTask"}` + "\n\n")

// handleOpenNewTask 供浏览器扩展「请弹新建任务窗」:把待办写入持久槽、唤起并置
// 前桌面窗口,再广播一帧提示。前端在 收到 openNewTask 帧 / EventSource 重连 /
// 窗口获焦 三种时机之一 GET /api/ui/newtask 取走并弹窗(自动填 URL,其余自填)。
func (s *Server) handleOpenNewTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req struct {
		URLs        []string          `json:"urls"`
		URL         string            `json:"url"`
		Referer     string            `json:"referer"`
		Headers     map[string]string `json:"headers"`
		Stay        bool              `json:"stay"`
		Magnet      string            `json:"magnet"`      // BT:预填「磁力链接」
		TorrentPath string            `json:"torrentPath"` // BT:预填本机 .torrent 路径
		Dir         string            `json:"dir"`         // BT:预填保存目录(空则用核心默认)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	urls := req.URLs
	if u := strings.TrimSpace(req.URL); u != "" {
		urls = append(urls, u)
	}
	var clean []string
	seen := map[string]bool{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			clean = append(clean, u)
		}
	}
	magnet := strings.TrimSpace(req.Magnet)
	tpath := strings.TrimSpace(req.TorrentPath)
	// 普通链接窗需有 urls;BT 窗则只需磁链或 .torrent 路径其一。
	if len(clean) == 0 && magnet == "" && tpath == "" {
		writeErr(w, http.StatusBadRequest, "缺少 urls/magnet/torrentPath")
		return
	}
	s.ntMu.Lock()
	s.ntPending = &newTaskPending{
		URLs: clean, Referer: strings.TrimSpace(req.Referer), Headers: req.Headers, Stay: req.Stay,
		Magnet: magnet, TorrentPath: tpath, Dir: strings.TrimSpace(req.Dir),
	}
	s.ntMu.Unlock()
	s.hub.Broadcast(s.uiSummonFrame(false, s.uiClients.Load() > 0))
	s.hub.Broadcast(openNewTaskFrame)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTakeNewTask 消费一次待办:取走并清空槽。前端据 urls 是否为空决定弹窗;
// 消费一次保证「关窗后重连不会再把同一请求重新弹出来」。
func (s *Server) handleTakeNewTask(w http.ResponseWriter, r *http.Request) {
	// 消费权归桌面端:Wails webview 的请求 UA 带 wails.io 标记(资产服务器统
	// 一附加),Fyne 客户端自报 ZDownloader-Desktop;浏览器标签(同为 Vue+SSE)
	// 一律探询到空且不消费——否则双击种子的目标会被恰好开着的浏览器截胡,
	// 桌面窗反而不弹
	if ua := r.UserAgent(); !strings.Contains(ua, "wails.io") && !strings.Contains(ua, "ZDownloader-Desktop") {
		writeJSON(w, http.StatusOK, map[string]any{"urls": []string{}})
		return
	}
	s.ntMu.Lock()
	p := s.ntPending
	s.ntPending = nil
	s.ntMu.Unlock()
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"urls": []string{}})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// QueueBootstrapTarget 供本进程内关联启动(无运行中实例、本进程即宿主)写入
// 「请弹创建对话框」待办:磁链 → BT 窗预填磁力链接;.torrent → BT 窗预填文件。
// 只写持久槽 + 唤窗广播(不自动建任务);窗口尚未就绪时 ShowUI 为空操作,前端
// 载入后 onopen/获焦会取走槽并弹窗(showNativeWindow 置前取焦)。
func (s *Server) QueueBootstrapTarget(target string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	p := &newTaskPending{}
	if strings.HasPrefix(strings.ToLower(target), "magnet:") {
		p.Magnet = target
	} else {
		p.TorrentPath = target
	}
	s.ntMu.Lock()
	s.ntPending = p
	s.ntMu.Unlock()
	s.hub.Broadcast(s.uiSummonFrame(false, s.uiClients.Load() > 0))
	s.hub.Broadcast(openNewTaskFrame)
}

// QueueSeedConflict 供引擎"种子自动打开冲突"(同 InfoHash 已有任务,任意状态)写入
// 「请弹种子冲突确认框」待办:写持久槽 + 唤窗 + 广播提示帧,前端取走后弹确认框,
// 由用户点「创建任务」(仍用种子文件建)或「取消」忽略。复用 openNewTask 帧与持久槽,
// 故同样抗 WebView2 挂起(窗口隐藏时帧可能丢,但唤窗获焦/重连/轮询会再取槽)。
func (s *Server) QueueSeedConflict(name, torrentPath, dir string) {
	s.ntMu.Lock()
	s.ntPending = &newTaskPending{SeedConflict: true, SeedName: name, TorrentPath: torrentPath, Dir: dir, Generated: true}
	s.ntMu.Unlock()
	s.hub.Broadcast(UIShowFrame(false))
	s.hub.Broadcast(openNewTaskFrame)
}

func statusFor(err error) int {
	if errors.Is(err, engine.ErrNotFound) || errors.Is(err, rss.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, engine.ErrStreamNotWanted) || errors.Is(err, engine.ErrStreamNoSession) ||
		errors.Is(err, engine.ErrStreamV2Deferred) || errors.Is(err, engine.ErrStreamStale) {
		return http.StatusConflict // 前置条件不满足:需扩选/会话未运行/文件不支持/旧代次
	}
	return http.StatusBadRequest
}

// ---------- 静态资源(SPA 回退) ----------

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, http.StatusNotFound, "接口不存在")
		return
	}
	// 原生壳不嵌入 Web UI:web 为 nil 时静态区整体 404,仅 API 可用
	if s.web == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	f, err := s.web.Open(p)
	if err != nil {
		s.serveFile(w, r, "index.html")
		return
	}
	defer f.Close()
	if d, err := f.Stat(); err != nil || d.IsDir() {
		s.serveFile(w, r, "index.html")
		return
	}
	s.serveFile(w, r, p)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if s.web == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := s.web.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, rs)
}

// ---------- JSON 辅助 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
