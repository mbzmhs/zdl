package nativeui

// 边下边播播放页(PlayerPane.vue 移植,Fyne + libvlc 承载)。
// 语义逐条对齐:
//   - 会话代次 epoch:每次切集/换任务单调 +1 拼进流 URL(&v=),引擎拒收旧
//     代次(409),根除"快速切集后被放弃的旧连接把会话拽回旧文件"的横跳;
//   - 打开先 Range bytes=0-0 探测,失败直接展示引擎错误不进播放器;
//   - 状态 500ms 轮询 + VLC 状态 300ms 轮询;过期响应按 taskId|file|epoch
//     门控丢弃(切集后旧请求迟到不闪回);
//   - piece 位图:优先窗口带/活动深度带/已完成块/头尾 pin(最小 6px)/
//     红线播放头/白线游标(下半段+描边,最后绘制);
//   - 播放头字节源链:表(seek 表)→ 锚(服务起点)→ 摊(全片均摊);
//     仅估算源做"连续完成边界"钳制(拖到尾部是合法动作,表源不钳——
//     与 Vue 版最终语义一致;浏览器特有的 buffered 内插源在 VLC 下不存在);
//   - 低速提示迟滞:低于 0.9× 码率进、高于 1.05× 出、缓冲 ≥99% 抑制;
//   - 剧集列表:三行视口、当前集居中、warmed 标记、自动连播、>90% 预热
//     下一集头尾元数据;
//   - 关闭:Stop → Release → DestroyWindow(spike 实测顺序)+ POST
//     stream/stop + 恢复主界面。
// 与浏览器版差异:缓冲/解码交给 libvlc(D3D11VA 硬解),serveStart 探测
// 由 Range 0-0 承担;MKV/H.265/AC3 编码覆盖优于 <video>。

import (
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/engine"
	"zdownloader/internal/vlc"
)

// playerEp 剧集条目
type playerEp struct {
	path string
	name string
	size int64
}

// playerPane 播放页会话(单例:同一时刻一个)
type playerPane struct {
	app    *App
	taskID string
	file   string // 当前集(空=单文件,由引擎解析)
	epoch  int64

	// libvlc 与视频子窗口
	vlcLoaded bool
	vlcPlayer *vlc.Player
	child     uintptr
	childWant bool
	childMu   sync.Mutex

	// 轮询与过期守卫
	stopCh      chan struct{}
	once        sync.Once
	pollersOnce sync.Once

	// 剧集
	eps        []playerEp
	curIdx     int
	warmed     map[string]bool
	lastTaskID string

	// 状态快照与播放头
	mu       sync.Mutex
	stat     *engine.BTStreamStatus
	vlcTime  float64 // 秒
	vlcDur   float64 // 秒
	vlcState vlc.State
	lowSpeed bool

	// UI
	titleLbl     *widget.Label
	stateLbl     *widget.Label
	bufLbl       *widget.Label
	playBtn      *textBtn // 自研幽灵钮(=原 widget.NewButton,统一全站按钮样式)
	seek         *widget.Slider
	timeLbl      *widget.Label
	video        *videoArea
	pieceCv      *pieceCanvas
	plScroll     *container.Scroll
	plBox        *fyne.Container
	plWrap       *fixedMin
	fullscreen   bool
	internalSeek bool
	lastSeek     time.Time   // 最近一次用户拖动(跳转窗口起点)
	seekTarget   float64     // 用户目标位置(秒)
	seekFlush    *time.Timer // 拖动停止 250ms 后补发最终位置(节流防丢尾)
	amStale      func(key string) bool
	myKey        func() string
}

// videoArea 视频区:空内容容器,布局时把 libvlc 子窗口对齐到自身矩形。
// Layout 在主线程执行,moveVideoChild 直接调用合法(见 winvideo 线程契约)。
type videoArea struct {
	widget.BaseWidget
	p *playerPane
}

func (v *videoArea) CreateRenderer() fyne.WidgetRenderer {
	return &videoAreaRenderer{v: v}
}

type videoAreaRenderer struct {
	v *videoArea
}

func (r *videoAreaRenderer) Layout(size fyne.Size) {
	p := r.v.p
	if p.childWant {
		p.ensureChild() // 主线程创建(Layout 必在主线程)
	}
	// 子窗口坐标必须是画布绝对坐标(=客户区):Position() 只相对父容器
	// (Border 中缝恒为 0,0),直接用会把视频钉在窗口左上角盖住顶栏
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(r.v)
	p.childMu.Lock()
	child := p.child
	p.childMu.Unlock()
	if child != 0 {
		moveVideoChild(child, int(pos.X), int(pos.Y), int(size.Width), int(size.Height))
	}
}

func (r *videoAreaRenderer) MinSize() fyne.Size           { return fyne.NewSize(320, 180) }
func (r *videoAreaRenderer) Objects() []fyne.CanvasObject { return nil }
func (r *videoAreaRenderer) Destroy()                     {}
func (r *videoAreaRenderer) Refresh()                     {}

// ensureChild 主线程确保子窗口存在(首次 Layout 时创建)
func (p *playerPane) ensureChild() {
	if !p.childWant {
		return
	}
	p.childMu.Lock()
	need := p.child == 0
	p.childMu.Unlock()
	if !need {
		return
	}
	h := makeVideoChild(p.app.HWND())
	p.childMu.Lock()
	p.child = h
	p.childMu.Unlock()
	if h != 0 {
		// 创建后立即对齐当前矩形(Layout 稍后也会再校);绝对坐标同理
		if p.video != nil {
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.video)
			size := p.video.Size()
			moveVideoChild(h, int(pos.X), int(pos.Y), int(size.Width), int(size.Height))
		}
	}
}

/* ---------------- pieceCanvas:分块位图 ---------------- */

// pieceCanvas 单行分块位图(=PlayerPane drawBuf)
type pieceCanvas struct {
	widget.BaseWidget
	p    *playerPane
	rast *canvas.Raster
}

func newPieceCanvas(p *playerPane) *pieceCanvas {
	c := &pieceCanvas{p: p}
	c.ExtendBaseWidget(c)
	c.rast = canvas.NewRaster(c.render)
	c.rast.SetMinSize(fyne.NewSize(200, 26))
	return c
}

func (c *pieceCanvas) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: c.rast, min: fyne.NewSize(200, 26)}
}

func (c *pieceCanvas) refresh() { c.rast.Refresh() }

var (
	blueFull  = color4(0x3b, 0x82, 0xf6, 0xff)
	bluePart  = color4(0x3b, 0x82, 0xf6, 0x73)
	blueBand  = color4(0x3b, 0x82, 0xf6, 0x1a)
	blueDepth = color4(0x3b, 0x82, 0xf6, 0x33)
	amberPin  = color4(0xf5, 0x9e, 0x0b, 0xff)
	redHead   = color4(0xef, 0x44, 0x44, 0xff)
	whiteCur  = color4(0xff, 0xff, 0xff, 0xf2)
	whiteEdge = color4(0, 0, 0, 0x8c)
)

func color4(r, g, b, a uint8) color.NRGBA { return color.NRGBA{r, g, b, a} }

// render 位图逐像素(行高 26:0~22 内容,底部 4px 留给 pin 下划线)
func (c *pieceCanvas) render(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	p := c.p
	p.mu.Lock()
	s := p.stat
	if s == nil {
		p.mu.Unlock()
		return img
	}
	sCopy := *s
	p.mu.Unlock()
	if sCopy.Pieces == "" || sCopy.LastPiece < sCopy.FirstPiece {
		return img
	}
	raw, err := base64.StdEncoding.DecodeString(sCopy.Pieces)
	if err != nil {
		return img
	}
	n := sCopy.LastPiece - sCopy.FirstPiece + 1
	bit := func(i int) bool {
		if i < 0 || i >= len(raw)*8 {
			return false
		}
		return raw[i>>3]>>(7-(i&7))&1 == 1
	}
	barH := h - 3
	// 优先窗口带 [cursor, cursor+ahead) 与活动深度带
	if sCopy.CursorPiece >= 0 && sCopy.Ahead > 0 {
		c0 := maxInt(0, sCopy.CursorPiece-sCopy.FirstPiece)
		c1 := minInt(n, c0+sCopy.Ahead)
		fillRect(img, xOf(c0, n, w), 0, xOf(c1, n, w)-xOf(c0, n, w), barH, blueBand)
		if sCopy.Depth > 0 {
			d1 := minInt(n, c0+sCopy.Depth)
			fillRect(img, xOf(c0, n, w), 0, xOf(d1, n, w)-xOf(c0, n, w), barH, blueDepth)
		}
	}
	// 已完成分块(列合并:全有=实蓝 / 部分=半蓝)
	for x := 0; x < w; x++ {
		st := x * n / w
		en := maxInt(st+1, (x+1)*n/w)
		any, full := false, true
		for i := st; i < en && i < n; i++ {
			if bit(i) {
				any = true
			} else {
				full = false
			}
		}
		if any {
			col := bluePart
			if full {
				col = blueFull
			}
			fillRect(img, x, 1, 1, barH-4, col)
		}
	}
	// 头/尾元数据 pin:底部橙色下划线,最小 6px 可视宽度
	pin := func(a, b int) {
		if a < 0 || b < a {
			return
		}
		i0 := maxInt(0, a-sCopy.FirstPiece)
		i1 := minInt(n-1, b-sCopy.FirstPiece)
		if i1 < i0 {
			return
		}
		wpx := maxInt(6, xOf(i1+1, n, w)-xOf(i0, n, w))
		fillRect(img, xOf(i0, n, w), h-4, wpx, 3, amberPin)
	}
	pin(sCopy.PinHeadFirst, sCopy.PinHeadLast)
	pin(sCopy.PinTailFirst, sCopy.PinTailLast)
	// 红线播放头(全高)
	if idx, src := p.playheadPiece(&sCopy); idx >= 0 && idx < n {
		// 估算源钳制到自锚点起连续完成边界(硬不变式:播放头只进已下载数据;
		// 表源不钳——拖到尾部是合法动作)
		if src != "表" {
			idx = p.clampToDone(&sCopy, raw, idx)
		}
		fillRect(img, minInt(w-2, xOf(idx, n, w)), 0, 2, h-3, redHead)
	}
	// 白线下载游标:下半段 + 深色描边,最后绘制(重合时两线均可见)
	if sCopy.CursorPiece >= 0 {
		cx := sCopy.CursorPiece - sCopy.FirstPiece
		if cx >= 0 && cx < n {
			x := minInt(w-1, xOf(cx, n, w))
			fillRect(img, maxInt(0, x-1), 0, 3, h-3, whiteEdge)
			fillRect(img, x, h/2, 2, h/2-3, whiteCur)
		}
	}
	return img
}

func xOf(i, n, w int) int { return i * w / n }

func fillRect(img *image.NRGBA, x, y, w, h int, c color.NRGBA) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if xx >= 0 && yy >= 0 && xx < img.Bounds().Dx() && yy < img.Bounds().Dy() {
				img.SetNRGBA(xx, yy, c)
			}
		}
	}
}

// playheadPiece 播放头时间→piece(源链:表→锚→摊)
func (p *playerPane) playheadPiece(s *engine.BTStreamStatus) (int, string) {
	n := s.LastPiece - s.FirstPiece + 1
	if n <= 0 {
		return -1, ""
	}
	p.mu.Lock()
	t := p.vlcTime
	p.mu.Unlock()
	if t < 0 {
		return -1, ""
	}
	var fileOff float64
	src := ""
	// 表:seek 表(秒→字节)
	if len(s.Seek) >= 2 {
		fileOff = seekTableOff(s.Seek, t)
		src = "表"
	}
	// 锚:活跃服务起点(引擎正为播放头下载的字节位置)
	if src == "" && s.ServeEnd >= maxI64(s.ServeStart, s.MetaEnd) &&
		(s.ServeStart > 0 || s.ServeEnd > 0 || s.MetaEnd > 0) {
		fileOff = float64(maxI64(s.ServeStart, s.MetaEnd)) - float64(s.FileOffset)
		src = "锚"
	}
	// 摊:全片均摊(VBR 会有偏差,最后兜底)
	if src == "" {
		dur := p.vlcDurLoad()
		if dur > 0 {
			fileOff = (t / dur) * float64(s.Size)
			src = "摊"
		}
	}
	if src == "" {
		return -1, ""
	}
	idx := int((float64(s.FileOffset)+fileOff)/float64(s.PieceLen)) - s.FirstPiece
	return idx, src
}

// seekTableOff seek 表二分(=前端 seekTableOff:最后一个 t<=time 的点的 off)
func seekTableOff(seek [][]float64, t float64) float64 {
	lo, hi := 0, len(seek)-1
	if t <= seek[0][0] {
		return seek[0][1]
	}
	if t >= seek[hi][0] {
		return seek[hi][1]
	}
	for lo < hi-1 {
		mid := (lo + hi) / 2
		if seek[mid][0] <= t {
			lo = mid
		} else {
			hi = mid
		}
	}
	// 组内线性插值
	a, b := seek[lo], seek[hi]
	if b[0] <= a[0] {
		return a[1]
	}
	f := (t - a[0]) / (b[0] - a[0])
	return a[1] + f*(b[1]-a[1])
}

// clampToDone 钳到自锚点起连续已完成的最远 piece 边界(=Vue lastDone 循环)
func (p *playerPane) clampToDone(s *engine.BTStreamStatus, raw []byte, idx int) int {
	n := s.LastPiece - s.FirstPiece + 1
	bit := func(i int) bool {
		if i < 0 || i >= len(raw)*8 {
			return false
		}
		return raw[i>>3]>>(7-(i&7))&1 == 1
	}
	anchor, _ := p.playheadPiece(s)
	_ = anchor
	aIdx := maxInt(0, s.CursorPiece-s.FirstPiece)
	if aIdx >= n {
		return idx
	}
	lastDone := -1
	for i := aIdx; i < n; i++ {
		if bit(i) {
			lastDone = i
		} else {
			break
		}
	}
	if lastDone >= 0 && idx > lastDone {
		return lastDone
	}
	return idx
}

func (p *playerPane) vlcDurLoad() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vlcDur
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

/* ---------------- 打开与挂载 ---------------- */

// openPlayer 打开播放页(主窗口内容整体切换;关闭恢复)
func (app *App) openPlayer(taskID, file string) {
	if _, ok := app.store.Task(taskID); !ok {
		return
	}
	p := &playerPane{
		app: app, taskID: taskID, file: file,
		epoch: 0, curIdx: -1,
		stopCh: make(chan struct{}),
		warmed: map[string]bool{},
	}
	p.amStale = func(key string) bool { return key != p.myKey() }
	p.myKey = func() string { return fmt.Sprintf("%s|%s|%d", p.taskID, p.file, p.epoch) }

	// libvlc 可用性:缺失给出引导文案(其余 UI 仍可显示状态)
	if err := vlc.Load(); err != nil {
		app.toast("播放内核不可用: "+err.Error(), "err")
		return
	}
	p.vlcLoaded = true

	app.mountPlayer(p)
}

// mountPlayer 构建播放页 UI 并接管主窗口
func (app *App) mountPlayer(p *playerPane) {
	pal := app.store.appPal()
	p.titleLbl = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.stateLbl = widget.NewLabel("连接中…")
	p.bufLbl = widget.NewLabel("")
	p.timeLbl = widget.NewLabel("--:-- / --:--")
	p.pieceCv = newPieceCanvas(p)
	p.video = &videoArea{p: p}
	p.video.ExtendBaseWidget(p.video)

	// 控制钮统一走自研幽灵钮(=原 widget.NewButton):播放/暂停、返回、全屏
	p.playBtn = newGhostButton(pal, "⏸", func() { p.togglePause() })
	p.seek = widget.NewSlider(0, 1000)
	p.seek.Step = 0.25 // 250ms 步进,拖动平滑
	p.seek.OnChanged = func(v float64) {
		if p.internalSeek {
			return
		}
		// 用户拖动:记录目标、进入跳转窗口(轮询暂停回写滑杆),实时
		// seek 走 250ms 节流;尾值经 flush 定时器补发(Fyne 无松手回调,
		// 节流会丢最后一次)
		p.seekTarget = v
		p.lastSeek = time.Now()
		if p.seekFlush != nil {
			p.seekFlush.Stop()
		}
		p.seekFlush = time.AfterFunc(250*time.Millisecond, func() { p.seekTo(p.seekTarget) })
		p.seekTo(v)
	}

	backBtn := newGhostButton(pal, "← 返回", func() { p.close() })
	fsBtn := newGhostButton(pal, "全屏", func() { p.toggleFullscreen() })

	top := container.NewHBox(backBtn, p.titleLbl, p.stateLbl, layout.NewSpacer(), fsBtn)
	// 滑杆占 Border 中央扩展位(HBox 会把它压到最小宽度,只剩一个圆点)
	controls := container.NewBorder(nil, nil, p.playBtn, p.timeLbl, p.seek)
	statusLine := container.NewHBox(p.bufLbl, layout.NewSpacer())

	playlistWrap := p.buildPlaylist()
	mid := container.NewBorder(nil, container.NewVBox(controls, statusLine, p.pieceCv), nil, nil, p.video)
	root := container.NewBorder(top, playlistWrap, nil, nil, mid)
	app.win.SetContent(container.NewPadded(root))
	app.player = p // ESC 关闭路由(buildRoot 的按键钩子读此字段)

	p.childWant = true // 首次 Layout 时创建子窗口(主线程)
	p.loadPlaylist()
	p.playCurrent()
}

// buildPlaylist 剧集列表(三行视口;单文件隐藏)
func (p *playerPane) buildPlaylist() fyne.CanvasObject {
	p.plBox = container.NewVBox()
	p.plScroll = container.NewVScroll(p.plBox)
	// 滚动区去边缘黑影;引用保留原 Scroll 供滚动定位,包裹在放置层
	p.plWrap = newFixedMin(container.NewStack(
		p.plScroll), fyne.NewSize(400, 3*36+8))
	p.plWrap.Hide()
	return p.plWrap
}

/* ---------------- 剧集(=loadPlaylist/playEp/warmNext) ---------------- */

// loadPlaylist 可播媒体清单(过滤填充/非媒体/未选;=loadPlaylist)
func (p *playerPane) loadPlaylist() {
	p.eps = nil
	p.curIdx = -1
	d, err := p.app.client.Detail(p.taskID)
	if err != nil {
		return
	}
	for i := range d.Files {
		f := &d.Files[i]
		if FtIsPadding(&ftFile{Path: f.Path}) || !ftIsMedia(&ftFile{Path: f.Path}) || !f.Wanted {
			continue
		}
		p.eps = append(p.eps, playerEp{
			path: strings.Join(f.Path, "/"),
			name: f.Path[len(f.Path)-1],
			size: f.Size,
		})
	}
	if len(p.eps) < 2 {
		p.plWrap.Hide()
		return
	}
	p.plWrap.Show()
	p.renderPlaylist()
}

// renderPlaylist 列表渲染(当前集加粗前缀;行高 36)
func (p *playerPane) renderPlaylist() {
	pal := p.app.store.appPal()
	rows := make([]fyne.CanvasObject, 0, len(p.eps))
	for i, ep := range p.eps {
		ep := ep
		mark := "  "
		if p.warmed[ep.path] {
			mark = "● "
		}
		cur := "  "
		if i == p.curIdx {
			cur = "▶ "
		}
		// 剧集行走自研无框文字钮(=原 widget.NewButton);当前集用 accent 字 +
		// selection 底色高亮(=原 HighImportance 填充钮的等价)
		row := newTabButton(pal, cur+mark+ep.name, func() { p.playEpisode(ep.path) })
		if i == p.curIdx {
			row.lbl.Color = pal.accent
			row.bg.FillColor = pal.selection
			row.bg.CornerRadius = 6
		}
		// 行高固定 36:下方 ScrollToOffset 按 36px/行计算,不能随按钮 min 漂
		rows = append(rows, newFixedMin(container.NewStack(row), fyne.NewSize(0, 36)))
	}
	p.plBox.Objects = rows
	p.plBox.Refresh()
	// 当前集滚到视口中部(行高 36)
	if p.curIdx > 1 {
		off := (p.curIdx - 1) * 36
		p.plScroll.ScrollToOffset(fyne.NewPos(0, float32(off)))
	} else {
		p.plScroll.ScrollToTop()
	}
}

// playEpisode 切集(=playEp:新代次 + 重新探测)
func (p *playerPane) playEpisode(path string) {
	if path == p.file {
		return
	}
	p.file = path
	for i, ep := range p.eps {
		if ep.path == path {
			p.curIdx = i
			break
		}
	}
	p.renderPlaylist()
	p.playCurrent()
}

// nextEp 下一集(=nextEp)
func (p *playerPane) nextEp() (playerEp, bool) {
	if p.curIdx >= 0 && p.curIdx+1 < len(p.eps) {
		return p.eps[p.curIdx+1], true
	}
	return playerEp{}, false
}

// warmNext 预热下一集头尾元数据(=warmNext)
func (p *playerPane) warmNext() {
	nx, ok := p.nextEp()
	if !ok || p.warmed[nx.path] {
		return
	}
	p.warmed[nx.path] = true
	go func() {
		_ = p.app.client.StreamWarm(p.taskID, nx.path)
		fyne.Do(func() { p.renderPlaylist() })
	}()
}

/* ---------------- 播放控制 ---------------- */

// playCurrent 探测 → 起 VLC(切集/首次共用;=probe + 换源)
func (p *playerPane) playCurrent() {
	p.epoch++
	key := p.myKey()
	fyne.Do(func() {
		p.stateLbl.SetText("连接中…")
		p.titleLbl.SetText(p.currentTitle())
		p.internalSeek = true
		p.seek.SetValue(0)
		p.internalSeek = false
		p.mu.Lock()
		p.vlcTime, p.vlcDur = 0, 0
		p.stat = nil
		p.mu.Unlock()
	})
	go func() {
		if err := p.app.client.StreamProbe(p.taskID, p.file, p.epoch); err != nil {
			if p.amStale(key) {
				return // 期间已切集:结果作废
			}
			fyne.Do(func() { p.stateLbl.SetText("✗ " + err.Error()) })
			return
		}
		// 换集:同一 vlc 播放器 Stop → PlayURL(重新 set_media)
		if p.vlcPlayer == nil {
			vp, err := vlc.NewPlayer()
			if err != nil {
				fyne.Do(func() { p.stateLbl.SetText("✗ " + err.Error()) })
				return
			}
			p.vlcPlayer = vp
		} else {
			p.vlcPlayer.Stop()
		}
		// 渲染面=视频区子窗口;绝不把顶层窗口交给 VLC——它会自建子窗口
		// 盖满整窗、遮死全部 Fyne 控件(实测事故)。此处主线程同步确保
		// 子窗口就位(首拍 Layout 可能尚未跑到)
		fyne.DoAndWait(func() { p.ensureChild() })
		p.childMu.Lock()
		child := p.child
		p.childMu.Unlock()
		if child == 0 {
			fyne.Do(func() { p.stateLbl.SetText("✗ 视频子窗口创建失败(仅音频)") })
			return
		}
		p.vlcPlayer.SetRenderWindow(child)
		url := p.app.client.StreamURL(p.taskID, p.file, p.epoch)
		if err := p.vlcPlayer.PlayURL(url); err != nil {
			fyne.Do(func() { p.stateLbl.SetText("✗ " + err.Error()) })
			return
		}
		fyne.Do(func() { p.stateLbl.SetText("播放中") })
		p.startPollersOnce()
	}()
}

// startPollersOnce VLC 状态轮询(300ms)与引擎状态轮询(500ms),会话级单次
func (p *playerPane) startPollersOnce() {
	onceBody := func() {
		// VLC:状态/时间/时长 → 控件;Ended → 自动连播;>90% → 预热
		go func() {
			t := time.NewTicker(300 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-p.stopCh:
					return
				case <-t.C:
					if p.vlcPlayer == nil {
						continue
					}
					st := p.vlcPlayer.State()
					tm := float64(p.vlcPlayer.Time()) / 1000
					dur := float64(p.vlcPlayer.Length()) / 1000
					p.mu.Lock()
					p.vlcTime, p.vlcDur, p.vlcState = tm, dur, st
					p.mu.Unlock()
					fyne.Do(func() { p.applyVLC() })
				}
			}
		}()
		// 引擎:流会话状态(位图/缓冲/速度/游标)
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-p.stopCh:
					return
				case <-t.C:
					key := p.myKey()
					st, err := p.app.client.StreamStatus(p.taskID)
					if err != nil {
						continue
					}
					if p.amStale(key) {
						continue // 轮询期间切集:丢弃旧会话数据
					}
					p.mu.Lock()
					p.stat = &st
					p.mu.Unlock()
					fyne.Do(func() {
						p.pieceCv.refresh()
						p.applyStatus()
					})
				}
			}
		}()
	}
	p.pollersOnce.Do(onceBody)
}

// applyVLC 控件回写(时间/滑杆/播放键/连播/预热)
func (p *playerPane) applyVLC() {
	p.mu.Lock()
	t, dur, st := p.vlcTime, p.vlcDur, p.vlcState
	p.mu.Unlock()
	p.timeLbl.SetText(fmtTime2(t) + " / " + fmtTime2(dur))
	// 跳转窗口:VLC seek 异步(引擎重排 piece 优先级 + 新建 Range),期间
	// get_time 仍是旧位置——不回写滑杆(否则用户拖一下就被拉回,实测事故),
	// 时间显示目标位置;到位或 8s 超时后恢复跟随
	arrived := p.seekTarget > 0 && absF(t-p.seekTarget) <= 2.0
	freeze := p.seekTarget > 0 && time.Since(p.lastSeek) < 8*time.Second && !arrived
	if freeze {
		p.timeLbl.SetText(fmtTime2(p.seekTarget) + " / " + fmtTime2(dur))
		if p.stateLbl.Text != "跳转中…" {
			p.stateLbl.SetText("跳转中…")
		}
	} else {
		if p.seekTarget > 0 && arrived && p.stateLbl.Text == "跳转中…" {
			p.stateLbl.SetText("播放中")
		}
		p.internalSeek = true
		if dur > 0 {
			p.seek.Max = dur
			p.seek.SetValue(minFloat(t, dur))
		}
		p.internalSeek = false
		p.timeLbl.SetText(fmtTime2(t) + " / " + fmtTime2(dur))
	}
	switch st {
	case vlc.StatePlaying:
		p.playBtn.SetText("⏸")
		if p.stateLbl.Text != "播放中" {
			p.stateLbl.SetText("播放中")
		}
	case vlc.StatePaused:
		p.playBtn.SetText("▶")
		p.stateLbl.SetText("已暂停")
	case vlc.StateEnded:
		p.playBtn.SetText("▶")
		// 自动连播下一集(=onEnded)
		if nx, ok := p.nextEp(); ok {
			p.playEpisode(nx.path)
			return
		}
		p.stateLbl.SetText("已播完")
	case vlc.StateError:
		p.stateLbl.SetText("✗ 解码/源错误")
	}
	// 预热:>90%(=onTimeUpdate warmNext)
	if dur > 0 && t/dur > 0.9 {
		p.warmNext()
	}
}

// applyStatus 状态行(已缓冲/低速迟滞;=tick + stepLowSpeed + bufStat)
func (p *playerPane) applyStatus() {
	p.mu.Lock()
	s := p.stat
	p.mu.Unlock()
	if s == nil || s.Pieces == "" || s.LastPiece < s.FirstPiece {
		return
	}
	n := s.LastPiece - s.FirstPiece + 1
	raw, err := base64.StdEncoding.DecodeString(s.Pieces)
	if err != nil {
		return
	}
	done := 0
	for i := 0; i < n && i < len(raw)*8; i++ {
		if raw[i>>3]>>(7-(i&7))&1 == 1 {
			done++
		}
	}
	pct := 0.0
	if n > 0 {
		pct = float64(done) / float64(n) * 100
	}
	txt := fmt.Sprintf("已缓冲 %.0f%%(分块 %d/%d) · %s · ↓ %s",
		pct, done, n, fmtBytes(s.Size), fmtSpeed(int64(s.Speed)))
	// 低速提示迟滞(=stepLowSpeed)
	if s.Bitrate > 0 && pct < 99 {
		if s.Speed < s.Bitrate*0.9 {
			p.lowSpeed = true
		} else if s.Speed > s.Bitrate*1.05 {
			p.lowSpeed = false
		}
	} else {
		p.lowSpeed = false
	}
	if p.lowSpeed {
		txt += fmt.Sprintf(" · ⚠ 速度低于码率需求(需 %s/s)", fmtBytes(int64(s.Bitrate)))
	}
	p.bufLbl.Text = txt
	p.bufLbl.Refresh()
	p.renderPlaylistMaybe()
}

// renderPlaylistMaybe 引擎侧 file 就绪(单文件种子的名称回填)后刷新当前集
func (p *playerPane) renderPlaylistMaybe() {
	if p.file == "" && p.stat != nil && p.stat.File != "" {
		p.file = p.stat.File
		for i, ep := range p.eps {
			if ep.path == p.file {
				p.curIdx = i
				break
			}
		}
		if p.titleLbl != nil {
			p.titleLbl.SetText(p.currentTitle())
		}
	}
}

func (p *playerPane) currentTitle() string {
	name := p.file
	if name == "" && p.stat != nil {
		name = p.stat.File
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "(解析中)"
	}
	return name
}

func (p *playerPane) togglePause() {
	if p.vlcPlayer != nil {
		p.vlcPlayer.TogglePause()
	}
}

func (p *playerPane) seekTo(v float64) {
	if p.vlcPlayer == nil {
		return
	}
	// 滑杆值是秒,libvlc SetTime 收毫秒——单位错会表现为"拖动回到开头"
	p.vlcPlayer.SetTime(int64(v * 1000))
}

func (p *playerPane) toggleFullscreen() {
	p.fullscreen = !p.fullscreen
	p.app.win.SetFullScreen(p.fullscreen)
}

// fmtTime2 秒 → m:ss / h:mm:ss
func fmtTime2(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	s := int64(sec)
	if s < 3600 {
		return fmt.Sprintf("%d:%02d", s/60, s%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

/* ---------------- 关闭(顺序:Stop → Release → DestroyWindow) ---------------- */

func (p *playerPane) close() {
	p.once.Do(func() { close(p.stopCh) })
	go func() {
		if p.vlcPlayer != nil {
			// Stop 在主线程(子窗口消息队列活着);Release 任意线程;销毁最后
			fyne.DoAndWait(func() { p.vlcPlayer.Stop() })
			p.vlcPlayer.Release()
			p.childMu.Lock()
			child := p.child
			p.childMu.Unlock()
			fyne.DoAndWait(func() { destroyVideoChild(child) })
		}
		_ = p.app.client.StreamStop(p.taskID) // 还原纯下载优先级(尽力而为)
		fyne.Do(func() {
			if p.fullscreen {
				p.app.win.SetFullScreen(false)
			}
			p.app.player = nil // 先摘播放页,ESC 路由随即回到弹层语义
			p.app.restoreMainContent()
		})
	}()
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
