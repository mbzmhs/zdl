package nativeui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/conf"
	"zdownloader/internal/engine"
)

// Store 是 Vue 版 store.js 的移植:数据契约逐条对齐(REST + SSE 双通道、
// 快照门控、速度历史 95 样本、toast 4200/4600ms、RSS 完成提醒、choosing
// 每会话一弹)。原生壳无需动态 actions 注册表(Go 直接方法调用),改用
// 钩子字段承接跨层回调(弹窗打开/确认框/唤窗)。
type Store struct {
	mu     sync.RWMutex
	client *Client

	tasks       []engine.TaskView
	stats       Stats
	cfg         *conf.Config
	currentView string // all/active/completed/trash/seeding/rss
	conn        bool   // SSE 连接状态(顶栏徽章)
	version     string // 后端版本(/api/ping)
	snapVer     uint64 // 已接受的最大快照版本(单调门控)
	theme       string // dark / light

	// 速度历史(顶栏曲线):与上/下行同拍推入,上限 95——与 Vue speedHistory 一致
	speedHist []float64
	upHist    []float64

	// RSS 自动下载完成提醒(快照 diff):id -> 上次状态;页面会话级
	prevTaskStatus map[string]string
	// choosing 每任务每次会话只弹一次
	choosePrompted map[string]bool
	// 详情抽屉所指任务被删时关抽屉:由 UI 层经 OnSnapshot 比对(与 Vue 相同的
	// acceptTick 时机,这里提供钩子即可)

	// UI 钩子(App 注入;store 不 import fyne,保持数据层可单测)
	OnSnapshot    func()                                     // 每个被接受的快照后调用(重绘节拍,=snapTick)
	OnConn        func(ok bool)                              // SSE 连接状态变化
	OnToast       func(msg, cls string)                      // toast(cls: ""/ok/err/warn)
	OnConfirm     func(req ConfirmReq)                       // 确认框
	OpenNewTask   func(p PendingNewTask)                     // 待弹创建窗(magnet/torrentPath/urls 三态)
	OpenBT        func(magnetOrPath, dir, fromTaskID string) // BT 对话框(target:磁力/种子路径;fromTaskID=挂起任务)
	OnChoose      func(id, torrentFile, dir string)          // choosing 弹窗
	OpenDetail    func(id string)                            // 详情抽屉(M5 注入;nil 时 detail 动作静默)
	OpenPlayer    func(taskID, file string)                  // 播放面板(file 空=单文件种子由引擎解析)
	OnThemeChange func(theme string)
	// OnViewChange 侧栏视图切换(分离模式下入口层经此把当前视图回写守护 config,
	// 新 UI 进程启动时恢复);OnShowRequest 收到守护 uiShow 帧(托盘/转交/扩展唤起
	// 已驻留的隐藏 UI 进程)。均为 nil 安全。
	OnViewChange  func(v string)
	OnShowRequest func(settings bool)
	// OnExitRequest 收到守护 uiExit 帧(守护退出前广播):UI 进程应尽快自行
	// 收尾退出,避免成为指向死接口的孤儿窗口。nil 安全。
	OnExitRequest func()
	palFn         func() palette // UI 注入:当前色板
}

// ConfirmReq 全局确认弹窗语义(对齐 Vue ConfirmBox:主按钮 + 可选二级按钮)
type ConfirmReq struct {
	Title          string
	Message        string
	OKLabel        string
	SecondaryLabel string
	OnOk           func()
	OnSecondary    func()
}

// NewStore 构造。initialTheme 为持久化的明暗主题(conf.Theme):仅显式 "light"
// 为亮色,空串/历史缺省一律按暗色——与 newTheme(s.Theme() != "light") 同语义。
// initialView 为持久化的侧栏视图(conf.LastView):仅接受已知视图键,其余按
// "active"(Vue 默认)——UI 进程跨会话恢复到用户上次所在页。
func NewStore(c *Client, initialTheme, initialView string) *Store {
	if initialTheme != "light" {
		initialTheme = "dark"
	}
	switch initialView {
	case "all", "active", "seeding", "completed", "trash", "rss":
	default:
		initialView = "active"
	}
	return &Store{
		client:         c,
		currentView:    initialView,
		theme:          initialTheme,
		prevTaskStatus: map[string]string{},
		choosePrompted: map[string]bool{},
	}
}

// 任务分类过滤(viewFilter 逐分支对齐 store.js)
func (s *Store) ViewFilter(st engine.Status) bool {
	switch s.currentView {
	case "all":
		return true
	case "active":
		switch st {
		case engine.StatusRunning, engine.StatusWaiting, engine.StatusPaused,
			engine.StatusError, engine.StatusChoosing:
			return true
		}
		return false
	case "completed":
		return st == engine.StatusCompleted
	case "trash":
		return st == engine.StatusCanceled
	case "seeding":
		return st == engine.StatusSeeding
	case "rss":
		return false
	}
	return true
}

// SwitchView 切换分类
func (s *Store) SwitchView(v string) {
	s.mu.Lock()
	changed := s.currentView != v
	s.currentView = v
	s.mu.Unlock()
	if s.OnSnapshot != nil {
		s.OnSnapshot() // 重绘(视图过滤变化)
	}
	// 分离模式:入口层经此把当前视图回写守护 config(新 UI 进程启动时恢复)。
	// 同视图重复点击不回写(切 tab 本来就低频,省一次 HTTP)
	if changed && s.OnViewChange != nil {
		s.OnViewChange(v)
	}
}

// CurrentView 当前分类名
func (s *Store) CurrentView() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentView
}

// Tasks 当前任务快照(只读切片,勿改)
func (s *Store) Tasks() []engine.TaskView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks
}

// Task 按 id 查任务
func (s *Store) Task(id string) (engine.TaskView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return t, true
		}
	}
	return engine.TaskView{}, false
}

// Stats 顶栏统计
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// Cfg 配置(可能为 nil:启动早期)
func (s *Store) Cfg() *conf.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetCfg 回写本地配置缓存(recentDirs 变更等)
func (s *Store) SetCfg(c *conf.Config) {
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
}

// SpeedHist 顶栏速度曲线样本(下载,上传)
func (s *Store) SpeedHist() (down, up []float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.speedHist, s.upHist
}

// Version 后端版本串
func (s *Store) Version() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Conn SSE 连接状态
func (s *Store) Conn() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

// Theme 当前主题
func (s *Store) Theme() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.theme
}

// SetTheme 切主题并通知重绘(Vue 版落 localStorage;原生壳经入口层注入的
// themePersist 回调落 conf.Theme 并同步托盘菜单明暗,见 App.SetOnThemePersist)
func (s *Store) SetTheme(t string) {
	s.mu.Lock()
	s.theme = t
	s.mu.Unlock()
	if s.OnThemeChange != nil {
		s.OnThemeChange(t)
	}
}

/* ---------------- 快照门控与刷新 ---------------- */

// acceptSnapshot 单调门控:拒绝倒退快照(防进度条闪回,=Vue acceptSnapshot)
func (s *Store) acceptSnapshot(v uint64) bool {
	if v == 0 || v < s.snapVer {
		return false
	}
	s.snapVer = v
	return true
}

// applySnapshot 接受一帧 {tasks, stats}(SSE 与 REST 轮询共用)
func (s *Store) applySnapshot(tasks []engine.TaskView, stats Stats) {
	s.mu.Lock()
	s.tasks = tasks
	s.stats = stats
	// 速度历史:每接受一帧推一拍(与 Vue pushSpeedHistory 同拍同上限)
	s.speedHist = append(s.speedHist, float64(stats.TotalSpeed))
	s.upHist = append(s.upHist, float64(stats.TotalUpSpeed))
	if len(s.speedHist) > 95 {
		s.speedHist = s.speedHist[len(s.speedHist)-95:]
		s.upHist = s.upHist[len(s.upHist)-95:]
	}
	// 任务删除清理 choosing 去重表(防长会话无界增长)
	if len(s.choosePrompted) > 64 {
		alive := map[string]bool{}
		for _, t := range tasks {
			alive[t.ID] = true
		}
		for id := range s.choosePrompted {
			if !alive[id] {
				delete(s.choosePrompted, id)
			}
		}
	}
	s.mu.Unlock()

	s.notifyRssAutoDone()
	if s.OnSnapshot != nil {
		s.OnSnapshot()
	}
}

// notifyRssAutoDone RSS 自动下载完成提醒(prevTaskStatus diff,对齐 store.js)
func (s *Store) notifyRssAutoDone() {
	s.mu.RLock()
	tasks := s.tasks
	prev := s.prevTaskStatus
	s.mu.RUnlock()
	for _, t := range tasks {
		last := prev[t.ID]
		prev[t.ID] = string(t.Status)
		rssActive := last == "waiting" || last == "running" || last == "paused" ||
			last == "choosing" || last == "error"
		if t.Source == "rss-notify" && last != "" && rssActive &&
			(t.Status == engine.StatusCompleted || t.Status == engine.StatusSeeding) {
			s.Toast("RSS 自动下载完成:"+t.Filename, "ok")
		}
	}
}

// Toast 发 toast(cls: ""/ok/err/warn)
func (s *Store) Toast(msg, cls string) {
	if s.OnToast != nil {
		s.OnToast(msg, cls)
	}
}

// ConfirmBox 确认框
func (s *Store) ConfirmBox(req ConfirmReq) {
	if s.OnConfirm != nil {
		s.OnConfirm(req)
	} else if req.OnOk != nil {
		req.OnOk() // 无 UI 钩子(测试)时直接执行,保证语义完整
	}
}

// showErr 错误 toast(=Vue showErr)
func (s *Store) showErr(err error) {
	if err != nil {
		s.Toast(err.Error(), "err")
	}
}

/* ---------------- 启动与轮询 ---------------- */

// Boot 启动数据流:拉配置/ping → 连 SSE → 兜底轮询。对齐 Vue boot():
// REST 轮询 10s 兜底、待弹创建窗槽 2s;SSE 先于全量刷新建立。
func (s *Store) Boot() {
	if cfg, err := s.client.Config(); err == nil {
		s.mu.Lock()
		s.cfg = &cfg
		s.mu.Unlock()
	}
	if v, err := s.client.Ping(); err == nil {
		s.mu.Lock()
		s.version = v
		s.mu.Unlock()
		if s.OnSnapshot != nil {
			s.OnSnapshot() // 版本徽标更新
		}
	}
	s.RefreshTasks()
	go s.sseLoop()
	go func() { // REST 兜底轮询(SSE 断连期间也是数据源)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			s.RefreshTasks()
		}
	}()
	go func() { // 待弹创建窗槽轮询(2s;对话框未注册前钩子为 nil 即不消费)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			s.FetchPendingNewTask()
		}
	}()
}

// RefreshTasks 全量刷新(REST;接受门控后应用)
func (s *Store) RefreshTasks() {
	list, ver, err := s.client.Tasks()
	if err != nil {
		return // 离线静默(=Vue refreshTasks)
	}
	s.mu.Lock()
	ok := s.acceptSnapshot(uint64(ver))
	if ok {
		var st Stats
		if s2, err := s.client.Stats(); err == nil {
			st = s2
		}
		s.tasks = list
		s.stats = st
		s.speedHist = append(s.speedHist, float64(st.TotalSpeed))
		s.upHist = append(s.upHist, float64(st.TotalUpSpeed))
		if len(s.speedHist) > 95 {
			s.speedHist = s.speedHist[len(s.speedHist)-95:]
			s.upHist = s.upHist[len(s.upHist)-95:]
		}
	}
	s.mu.Unlock()
	if ok {
		s.notifyRssAutoDone()
		if s.OnSnapshot != nil {
			s.OnSnapshot()
		}
	}
}

// sseLoop SSE 主循环:断线自动重连(EventSource 语义),逐帧解析。
// 帧 = "data: {json}\n\n";type=update 走门控,type=openNewTask 取槽。
// 重连退避 100ms 起步、2s 封顶:断线窗口越短,托盘中继误判「无附着」的
// 概率越低(窗口期点托盘会走双开慢路径,唤窗延迟数秒)。
func (s *Store) sseLoop() {
	backoff := 100 * time.Millisecond
	for {
		resp, err := s.client.Long.Get(s.client.Base + "/api/events?client=ui")
		if err == nil {
			s.setConn(true)
			s.scanSSE(resp)
			resp.Body.Close()
			backoff = 100 * time.Millisecond // 连上过即复位,下次断线从快重试
		}
		s.setConn(false)
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// scanSSE 逐帧读取解析(行协议:空行分隔,data: 前缀)
func (s *Store) scanSSE(resp *http.Response) {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20) // 全量任务快照帧可能很大
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg struct {
			Type     string            `json:"type"`
			Version  uint64            `json:"version"`
			Tasks    []engine.TaskView `json:"tasks"`
			Stats    Stats             `json:"stats"`
			Settings bool              `json:"settings"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &msg); err != nil {
			continue // 坏帧忽略(=Vue)
		}
		switch msg.Type {
		case "update":
			s.mu.Lock()
			ok := s.acceptSnapshot(msg.Version)
			s.mu.Unlock()
			if !ok {
				continue
			}
			s.setConn(true)
			s.applySnapshot(msg.Tasks, msg.Stats)
		case "openNewTask":
			s.FetchPendingNewTask()
		case "uiShow":
			// 分离架构:守护唤起已驻留的隐藏 UI 进程(托盘左键/转交/扩展)
			if s.OnShowRequest != nil {
				s.OnShowRequest(msg.Settings)
			}
		case "uiExit":
			// 分离架构:守护退出前广播,UI 进程自行收尾(兜底是守护 kill 子进程)
			if s.OnExitRequest != nil {
				s.OnExitRequest()
			}
		}
	}
}

func (s *Store) setConn(ok bool) {
	s.mu.Lock()
	changed := s.conn != ok
	s.conn = ok
	s.mu.Unlock()
	if changed && s.OnConn != nil {
		s.OnConn(ok)
	}
}

// FetchPendingNewTask 取走待弹创建窗槽(=store.fetchPendingNewTask:对话框
// 未注册前严禁消费——钩子为 nil 直接返回,槽留给下一次轮询)
func (s *Store) FetchPendingNewTask() {
	if s.OpenNewTask == nil && s.OpenBT == nil {
		return
	}
	p, empty, err := s.client.PendingNewTask()
	if err != nil || empty {
		return
	}
	switch {
	case p.SeedConflict:
		s.showSeedConflict(p)
	case p.Magnet != "" && s.OpenBT != nil:
		s.OpenBT(p.Magnet, p.Dir, "")
	case p.TorrentPath != "" && s.OpenBT != nil:
		s.OpenBT(p.TorrentPath, p.Dir, "") // 路径预填,创建后清槽来源
	case len(p.URLs) > 0 && s.OpenNewTask != nil:
		s.OpenNewTask(p)
	}
}

// showSeedConflict 种子自动打开冲突确认(=store.showSeedConflict;「创建任务」
// 直接按种子落盘目录建 BT 任务,等价旧版不再弹 BT 窗)
func (s *Store) showSeedConflict(p PendingNewTask) {
	s.ConfirmBox(ConfirmReq{
		Message: "种子「" + p.SeedName + "」已下载完成,但任务列表中已存在同名任务,未自动创建。是否仍要创建该任务?",
		OKLabel: "创建任务",
		OnOk: func() {
			tasks, errs, err := s.client.CreateBT(engine.CreateReq{
				TorrentPath:      p.TorrentPath,
				Dir:              p.Dir,
				TorrentGenerated: p.Generated,
			})
			if err != nil {
				s.showErr(err)
				return
			}
			if len(errs) > 0 {
				s.Toast(errs[0], "err")
				return
			}
			_ = tasks
			s.Toast("已创建任务", "ok")
			s.RefreshTasks()
		},
	})
}

// MaybePromptChoose choosing 自动弹窗(每任务每次会话一次;对话框开着不触发,
// 由 UI 层在 OnChoose 钩子里保证)
func (s *Store) MaybePromptChoose() {
	s.mu.RLock()
	tasks := s.tasks
	prompted := s.choosePrompted
	var hit *engine.TaskView
	for i := range tasks {
		t := &tasks[i]
		if t.Kind == "bt" && t.Status == engine.StatusChoosing && !prompted[t.ID] {
			hit = t
			break
		}
	}
	s.mu.RUnlock()
	if hit == nil {
		return
	}
	s.mu.Lock()
	s.choosePrompted[hit.ID] = true
	s.mu.Unlock()
	_ = s.client.ShowUI() // 唤起原生窗口并置前取焦(=Vue fetch /api/ui/show)
	s.OnChoose(hit.ID, hit.TorrentFile, hit.Dir)
}

/* ---------------- 任务操作(=store.handleAction 逐分支) ---------------- */

// HandleAction 任务卡动作分发
func (s *Store) HandleAction(act, id string) {
	switch act {
	case "pause":
		s.showErr(s.client.Action(id, "pause"))
	case "resume", "retry":
		s.showErr(s.client.Action(id, "resume"))
	case "reseed":
		if err := s.client.Action(id, "reseed"); err != nil {
			s.showErr(err)
			return
		}
		s.Toast("已请求重新做种", "ok")
		s.RefreshTasks()
	case "cancel":
		s.ConfirmBox(ConfirmReq{
			Message: "取消后已下载数据将被丢弃,确定取消该任务?",
			OKLabel: "取消任务",
			OnOk:    func() { s.showErr(s.client.Action(id, "cancel")) },
		})
	case "open":
		s.showErr(s.client.Action(id, "open"))
	case "stopseed":
		s.ConfirmBox(ConfirmReq{
			Message: "停止做种后任务将归入已完成,确定停止?",
			OKLabel: "停止做种",
			OnOk:    func() { s.showErr(s.client.Action(id, "stopseed")) },
		})
	case "detail":
		if s.OpenDetail != nil {
			s.OpenDetail(id)
		}
	case "choosefiles":
		if t, ok := s.Task(id); ok && t.TorrentFile != "" && s.OpenBT != nil {
			s.OpenBT(t.TorrentFile, t.Dir, id)
		} else if s.OnChoose != nil {
			s.OnChoose(id, "", "")
		}
	case "trash":
		t, ok := s.Task(id)
		if !ok {
			return
		}
		name := t.Filename
		if name == "" {
			name = MagnetFallbackName(t)
		}
		if name == "" {
			name = "未命名任务"
		}
		req := ConfirmReq{
			Message: "删除任务「" + name + "」的记录?",
			OKLabel: "仅删除记录",
			OnOk:    func() { s.showErr(s.client.Delete(id, false)) },
		}
		if t.Status == engine.StatusCompleted || t.Status == engine.StatusSeeding {
			req.SecondaryLabel = "删除记录和文件"
			req.OnSecondary = func() { s.showErr(s.client.Delete(id, true)) }
		}
		s.ConfirmBox(req)
	}
}

// OpenTaskFile 文件名点击打开(=store.openTaskFile:可执行文件先确认)
func (s *Store) OpenTaskFile(id string) {
	t, ok := s.Task(id)
	if !ok || (t.Status != engine.StatusCompleted && t.Status != engine.StatusSeeding) {
		return
	}
	go_ := func() {
		if err := s.client.Action(id, "openfile"); err != nil {
			s.showErr(err)
			return
		}
		s.Toast("已调用系统程序打开", "ok")
	}
	if isExeName(t.Filename) {
		s.ConfirmBox(ConfirmReq{
			Message: "「" + t.Filename + "」是可执行文件,直接运行可能有风险。确定要打开吗?",
			OKLabel: "仍然打开",
			OnOk:    go_,
		})
		return
	}
	go_()
}

// isExeName 可执行扩展名(对齐 store.js EXE_RE)
func isExeName(name string) bool {
	switch strings.ToLower(extOf(name)) {
	case "exe", "msi", "bat", "cmd", "com", "scr", "pif", "ps1", "vbs":
		return true
	}
	return false
}

// ClearView 分类清扫(=store.clearView)
func (s *Store) ClearView(view, label string) {
	status := "canceled"
	if view == "completed" {
		status = "completed"
	}
	n := 0
	for _, t := range s.Tasks() {
		if string(t.Status) == status {
			n++
		}
	}
	if n == 0 {
		s.Toast("该分类没有可清扫的任务", "")
		return
	}
	req := ConfirmReq{
		Message: "清扫「" + label + "」分类的全部 " + itoa(n) + " 条任务记录?",
		OKLabel: "仅删除记录",
	}
	req.OnOk = func() {
		removed, err := s.client.Clear(view, false)
		if err != nil {
			s.showErr(err)
			return
		}
		s.Toast("已清扫 "+itoa(removed)+" 条任务记录", "ok")
	}
	if view == "completed" {
		req.SecondaryLabel = "删除记录和文件"
		req.OnSecondary = func() {
			removed, err := s.client.Clear(view, true)
			if err != nil {
				s.showErr(err)
				return
			}
			s.Toast("已清扫 "+itoa(removed)+" 条任务记录及已下载文件", "ok")
		}
	}
	s.ConfirmBox(req)
}

// RecordRecentDir 创建成功后回写最近目录(响应刷新本地 cfg)
func (s *Store) RecordRecentDir(dir string) {
	if dir == "" {
		return
	}
	recent, err := s.client.AddRecentDir(dir)
	if err != nil {
		return // 失败不影响创建流程(=Vue)
	}
	if cfg := s.Cfg(); cfg != nil {
		cfg.RecentDirs = recent
		s.SetCfg(cfg)
	}
}

// DeleteRecentDir 从最近目录移除(响应刷新本地 cfg;=Vue del 后 re-render)
func (s *Store) DeleteRecentDir(dir string) {
	if dir == "" {
		return
	}
	recent, err := s.client.DeleteRecentDir(dir)
	if err != nil {
		return
	}
	if cfg := s.Cfg(); cfg != nil {
		cfg.RecentDirs = recent
		s.SetCfg(cfg)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// appPal 当前色板(任务卡等组件经 store 取;App 注入)
func (s *Store) appPal() palette { return s.palFn() }
