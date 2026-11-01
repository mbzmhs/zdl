package nativeui

// 任务详情抽屉(Drawer.vue 移植):概览/速度/文件/连接/Tracker 五页签。
// 语义逐条对齐:
//   - 1s 轮询 /api/tasks/{id}/detail;客户端按秒采样累计量增量画速度曲线
//     (180 点 = 3 分钟;累计量回退视为 0 速,不产生负尖峰)
//   - 限速输入编辑中不回写(dwLimitDirty),否则秒级轮询覆写输入框
//   - 文件勾选未应用前轮询不重建勾选集(dwSelDirty)
//   - Tracker 表含虚拟来源行(DHT/PEX);连接表 IP 按点分段数值排序
//   - 任务被删:轮询 404 后自动关窗
// 交互差异(打磨阶段收敛):右键菜单 → v1 用行选中 + 按钮组等价实现,
// 表头排序/分块图列已补齐。

import (
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/winfd"
	"zdownloader/internal/engine"
)

// fmtDur 时长格式化(=peerfmt.js fmtDur:0:07 / 12:34 / 1:02:03)
func fmtDur(sec float64) string {
	if sec <= 0 {
		return "—"
	}
	s := int64(sec)
	if s < 60 {
		return fmt.Sprintf("0:%02d", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%d:%02d", s/60, s%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// srcText 来源中文(=peerfmt.js SRC_TEXT)
func srcText(s string) string {
	switch s {
	case "tracker":
		return "Tracker"
	case "dht":
		return "DHT"
	case "pex":
		return "PEX"
	case "manual":
		return "手动"
	case "incoming":
		return "入站"
	case "history":
		return "历史"
	}
	if s == "" {
		return "—"
	}
	return s
}

// cmpIp IPv4 按点分段数值比较(=peerfmt.js cmpIp,避免 2.x 排在 10.x 后)
func cmpIp(a, b string) int {
	pa := strings.SplitN(a, ".", 4)
	pb := strings.SplitN(b, ".", 4)
	if len(pa) == 4 && len(pb) == 4 {
		for i := 0; i < 4; i++ {
			x, _ := strconv.Atoi(pa[i])
			y, _ := strconv.Atoi(pb[i])
			if x != y {
				return x - y
			}
		}
	}
	return strings.Compare(a, b)
}

// taskSpeedChart 任务速度曲线(=Drawer.vue drawTaskSpeed:1s 采样 × 3 分钟,
// 上传绿在下、下载蓝在上;容器未显示时不画)
type taskSpeedChart struct {
	widget.BaseWidget
	rast    *canvas.Raster
	mu      sync.Mutex
	down    []float64
	up      []float64
	lastSig [2]float64 // 数据签名(暂停/零速免重绘)
}

func newTaskSpeedChart() *taskSpeedChart {
	c := &taskSpeedChart{}
	c.ExtendBaseWidget(c)
	c.rast = canvas.NewRaster(c.render)
	c.rast.SetMinSize(fyne.NewSize(600, 200))
	return c
}

func (c *taskSpeedChart) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: c.rast, min: fyne.NewSize(600, 200)}
}

func (c *taskSpeedChart) setSamples(down, up []float64) {
	// 签名守卫:暂停/零速时样本追加但数值全零,曲线不变,免每秒重绘
	var sig [2]float64
	for _, v := range down {
		sig[0] += v
	}
	for _, v := range up {
		sig[1] += v
	}
	c.mu.Lock()
	if sig == c.lastSig {
		c.mu.Unlock()
		return
	}
	c.lastSig = sig
	c.down, c.up = down, up
	c.mu.Unlock()
	c.rast.Refresh()
}

func (c *taskSpeedChart) render(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	c.mu.Lock()
	down, up := append([]float64(nil), c.down...), append([]float64(nil), c.up...)
	c.mu.Unlock()
	const ptsMax = 180 // 1s × 3 分钟,与 Drawer.vue SPEED_PTS_MAX 一致
	n := len(down)
	if n < 2 {
		return img
	}
	max := 1024.0
	for i := 0; i < n; i++ {
		if down[i] > max {
			max = down[i]
		}
		if up[i] > max {
			max = up[i]
		}
	}
	max *= 1.15
	plot := func(hist []float64, r, g, b uint8) {
		prevY := -1
		for x := 0; x < w; x++ {
			// 曲线右对齐最近样本(x 映射到 ptsMax 时间轴,左侧留空)
			i := n - ptsMax + x*(ptsMax-1)/(w-1)
			if i < 0 {
				continue
			}
			if i > n-1 {
				i = n - 1
			}
			y := h - 2 - int(hist[i]/max*float64(h-8))
			if y < 0 {
				y = 0
			}
			if y > h-1 {
				y = h - 1
			}
			lo, hi := y, y
			if prevY >= 0 {
				lo, hi = minInt(y, prevY), maxInt(y, prevY)
			}
			for yy := lo; yy <= hi; yy++ {
				img.SetNRGBA(x, yy, color.NRGBA{R: r, G: g, B: b, A: 255})
			}
			prevY = y
		}
	}
	plot(up, 0x34, 0xd3, 0x99)
	plot(down, 0x4f, 0x8c, 0xff)
	return img
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

/* ---------------- 抽屉主体 ---------------- */

type drawer struct {
	app    *App
	taskID string
	pop    *maskModal
	stop   chan struct{}
	once   sync.Once

	// 概览
	overview *fyne.Container
	title    *colorLabel
	sub      *colorLabel
	chipBg   *canvas.Rectangle
	chipLbl  *colorLabel

	// 页签(=dw-tabs:active accent 下划线)
	tabKeys  []string
	tabNames map[string]string
	tabUnder map[string]*canvas.Rectangle
	tabBtn   map[string]*textBtn
	panes    map[string]fyne.CanvasObject
	active   string

	// 表格排序(=peerSort/trSort:同列再点反转,异列重置升序;默认首列升序)
	peerSortCol, peerSortDir int
	trSortCol, trSortDir     int

	// 速度
	downCard, upCard *colorLabel
	chart            *taskSpeedChart
	dwLimit          *widget.Entry
	dwLimitUnit      *zSelect
	dwUpLimit        *widget.Entry
	dwUpLimitUnit    *zSelect
	dwLimField       fyne.CanvasObject
	dwUpLimField     fyne.CanvasObject
	saveBtn          *textBtn
	limitDirty       bool

	// 文件
	fileBody     *fyne.Container // 树或"元数据未就绪"占位
	fileTree     *ftTree
	fileToolbar  *fyne.Container
	selDirty     bool
	filesHaveMet bool
	filesStatus  engineStatus // 最近一拍的任务状态(工具栏即时刷新用)
	filesTBSig   string       // 工具栏形态签名(不变不重建,保住按钮悬停态)
	fileMsg      *widget.Label
	fileMode     int // fileBody 当前形态:0 未定 1 占位文案 2 树(避免每拍重建占位)

	// 概览缓存(=kv 行集;标签集不变时只原位改值,不重建行控件)
	ovSig  string
	ovVals []*widget.Label

	// 连接
	peerTable *menuTable
	peerRows  []peerRow
	peerSel   *rowSelSet

	// Tracker
	trTable *menuTable
	trRows  []trRow
	trSel   *rowSelSet

	// 最近一拍数据(页签切换时立即补一拍;隐藏页签轮询不刷新)
	lastDet *engine.TaskDetailView

	// 速度采样(=speedHistMap 单任务版)
	sampleMu       sync.Mutex
	lastDL, lastUL int64
	lastT          time.Time
	downPts        []float64
	upPts          []float64
	instD, instU   float64
}

// peerRow 连接表行(展示文本 + 排序键,=Drawer.vue buildPeerRows)
type peerRow struct {
	addr, ip, client, port, lport, prog, src, downRate, upRate, down, up,
	effTxt, effCls, inbound, proto, enc, connFor, flags, banned string
	srcKey string // 来源原始键(=p.source;.src-badge 配色依据)
	eff    float64
	bitf   string // 对端 piece 位图(base64;分块图列)
	// 排序原值(=v.*:数字列必须用原值,格式化串排序会 100>99 乱序)
	portN, lportN                    int64
	progN, downRateN, upRateN, downN float64
	upN, connN                       float64
	inbN, protoN, encN               int
}

// trRow Tracker 表行(展示文本 + 排序原值,=buildTrRows 的 v;虚拟 DHT/PEX
// 行的 url=展示名,排序原值全 0 同 Vue)
type trRow struct {
	virtual                                               bool
	url, stTxt, remain, seeds, leech, users, retries, msg string
	// 排序原值(=v.*:时间取 ms、状态 0=ok/1=未应答/2=err;数字列取原值)
	stN                              int
	remainMs                         float64
	seedsN, leechN, usersN, retriesN int64
}

// trSortVal Tracker 表排序取值(全部 8 列可排序,=Vue th 的 data-sk)
func (r *trRow) trSortVal(col int) any {
	switch col {
	case 0:
		return r.url
	case 1:
		return r.stN
	case 2:
		return r.remainMs
	case 3:
		return r.seedsN
	case 4:
		return r.leechN
	case 5:
		return r.usersN
	case 6:
		return r.retriesN
	case 7:
		return r.msg
	}
	return ""
}

// openDrawer 打开详情抽屉并启动 1s 轮询(=detail-dialog 780x720 居中卡片)
func (app *App) openDrawer(id string) {
	if _, ok := app.store.Task(id); !ok {
		return
	}
	d := &drawer{app: app, taskID: id, stop: make(chan struct{}),
		tabKeys:  []string{"overview", "speed", "files", "peers", "trackers"},
		tabNames: map[string]string{"overview": "概览", "speed": "速度", "files": "文件", "peers": "连接", "trackers": "Tracker"},
		tabUnder: map[string]*canvas.Rectangle{}, tabBtn: map[string]*textBtn{},
		panes: map[string]fyne.CanvasObject{}, active: "overview",
		peerSortCol: 1, peerSortDir: 1, trSortCol: 0, trSortDir: 1,
		peerSel: newRowSelSet(), trSel: newRowSelSet()}
	app.buildDrawerUI(d)

	// 任务被删自动关窗的兜底也在轮询里(=store.acceptTick 语义)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		d.pollOnce() // 首拍立即
		for {
			select {
			case <-d.stop:
				return
			case <-t.C:
				d.pollOnce()
			}
		}
	}()

	d.pop = modalCard(app.win, app.store.appPal(), d.root(), 780, 720)
	d.pop.Show()
}

func (d *drawer) close() {
	hideMenu() // 关抽屉收起右键菜单(弹层不随抽屉走)
	d.once.Do(func() { close(d.stop) })
}

// pollOnce 拉一拍详情并应用
func (d *drawer) pollOnce() {
	det, err := d.app.client.Detail(d.taskID)
	if err != nil {
		// 任务刚被删:关抽屉(=acceptTick 的 closeDrawer 联动)
		if apiErr, ok := err.(*APIError); ok && apiErr.Code == 404 {
			fyne.Do(func() {
				d.close()
				if d.pop != nil {
					d.pop.Hide()
				}
			})
		}
		return
	}
	d.sampleSpeed(&det)
	fyne.Do(func() { d.apply(&det) })
}

// sampleSpeed 按秒采样累计量增量(=Drawer.vue sampleSpeedHistory)
func (d *drawer) sampleSpeed(det *engine.TaskDetailView) {
	now := time.Now()
	cur := struct {
		dl, ul int64
		t      time.Time
	}{det.Downloaded, det.Uploaded, now}
	d.sampleMu.Lock()
	defer d.sampleMu.Unlock()
	if !d.lastT.IsZero() {
		dt := now.Sub(d.lastT).Seconds()
		if dt > 0.05 {
			dl := float64(cur.dl-d.lastDL) / dt
			ul := float64(cur.ul-d.lastUL) / dt
			if dl < 0 {
				dl = 0 // 累计量回退(会话重启)视为 0 速,不产生负尖峰
			}
			if ul < 0 {
				ul = 0
			}
			d.instD, d.instU = dl, ul
			d.downPts = append(d.downPts, dl)
			d.upPts = append(d.upPts, ul)
			if len(d.downPts) > 180 {
				d.downPts = d.downPts[len(d.downPts)-180:]
				d.upPts = d.upPts[len(d.upPts)-180:]
			}
		}
	}
	d.lastDL, d.lastUL, d.lastT = cur.dl, cur.ul, cur.t
}

// root 弹层内容(=detail-dialog:drawer-head + dw-tabs + drawer-body)
func (d *drawer) root() fyne.CanvasObject {
	pal := d.app.store.appPal()
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	barWrap := newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17))
	chip := container.NewStack(d.chipBg,
		container.New(layout.NewCustomPaddedLayout(3, 3, 10, 10), d.chipLbl))
	closeBtn := newIconButtonSZ(pal, "close", 36, 18, func() { d.pop.Hide(); d.close() })
	// 空副标题不进布局(=Vue 空 .sub 高度 0;否则标题区看起来是两行)
	titleCol := container.NewVBox(d.title)
	if d.sub.Visible() {
		titleCol.Add(d.sub)
	}
	head := container.NewBorder(nil, nil, barWrap,
		container.NewHBox(chip, closeBtn), titleCol)
	headPad := container.New(layout.NewCustomPaddedLayout(16, 12, 20, 20), head)

	// 页签行(=dw-tabs:底部 1px 边线 + active accent 下划线)
	items := make([]fyne.CanvasObject, 0, len(d.tabKeys))
	for _, k := range d.tabKeys {
		items = append(items, d.tabItem(k))
	}
	d.switchTab("overview") // 页签控件就绪后才能切(下划线/按钮 map 已填)
	line := canvas.NewRectangle(pal.border)
	line.SetMinSize(fyne.NewSize(0, 1))
	tabsRow := container.NewStack(
		container.NewBorder(nil, line, nil, nil, container.NewHBox(items...)))
	tabsPad := container.New(layout.NewCustomPaddedLayout(0, 0, 20, 20), tabsRow)

	paneObjs := make([]fyne.CanvasObject, 0, len(d.tabKeys))
	for _, k := range d.tabKeys {
		paneObjs = append(paneObjs, d.panes[k])
	}
	bodyPad := container.New(layout.NewCustomPaddedLayout(4, 14, 20, 20), container.NewStack(paneObjs...))
	return container.NewBorder(container.NewVBox(headPad, tabsPad), nil, nil, nil, bodyPad)
}

// tabItem 单个页签(文字钮 + 底部 accent 下划线,active 时显示)
func (d *drawer) tabItem(key string) fyne.CanvasObject {
	pal := d.app.store.appPal()
	under := canvas.NewRectangle(pal.accent)
	under.SetMinSize(fyne.NewSize(12, 2))
	under.Hide()
	d.tabUnder[key] = under
	btn := newTabButton(pal, d.tabNames[key], func() { d.switchTab(key) })
	d.tabBtn[key] = btn
	return container.NewVBox(
		container.New(layout.NewCustomPaddedLayout(8, 6, 14, 14), btn), under)
}

// switchTab 页签切换(显隐 + active 样式;表格页签加宽 780→1140 并重新居中,
// =drawerEl.classList.toggle("wide"))
func (d *drawer) switchTab(key string) {
	d.active = key
	hideMenu() // 页签切换收起右键菜单(菜单项指向的行即将消失)
	pal := d.app.store.appPal()
	for _, k := range d.tabKeys {
		if k == key {
			d.panes[k].Show()
			d.tabUnder[k].Show()
			d.tabBtn[k].lbl.Color = pal.accent
			d.tabBtn[k].lbl.TextStyle = fyne.TextStyle{Bold: true}
		} else {
			d.panes[k].Hide()
			d.tabUnder[k].Hide()
			d.tabBtn[k].lbl.Color = pal.textDim
			d.tabBtn[k].lbl.TextStyle = fyne.TextStyle{}
		}
		d.tabUnder[k].Refresh()
		d.tabBtn[k].Refresh()
	}
	if d.pop != nil {
		// 宽表(peers/trackers)卡片加宽到 1140,其余 780。只改卡片尺寸下限,
		// 全屏遮罩层保持不动,Center 自动重新居中卡片(=Vue classList.toggle
		// "wide")。切勿 Resize/Move d.pop——那是全屏遮罩层,动了遮罩就塌
		w := float32(780)
		if key == "peers" || key == "trackers" {
			w = 1140
		}
		d.pop.SetCardSize(w, 720)
	}
	// 隐藏页签轮询不刷新,切到新页签立即补一拍(不等下一轮询)
	if d.lastDet != nil {
		d.apply(d.lastDet)
	}
}

/* ---------------- 概览(=gridRows) ---------------- */

func (d *drawer) buildOverview() {
	// 行间距 6px(原 9px 过疏=用户报的"概览布局过于疏松");kv 行的 96px 键列
	// 自带 18px 垂直留白,收紧后整页更紧凑
	d.overview = container.New(&vgapLayout{gap: 6})
}

// applyOverview 概览网格(=gridRows computed 逐行)
func (d *drawer) applyOverview(t *engine.TaskView, det *engine.TaskDetailView) {
	isBT := t.Kind == "bt"
	progBytes := t.Downloaded
	if t.Arrived > 0 {
		progBytes = t.Arrived
	} else if isBT && t.Size > 0 && t.Downloaded > t.Size {
		progBytes = t.Size
	}
	sizeTxt := fmtBytes(t.Downloaded)
	if t.Size > 0 {
		sizeTxt = fmtBytes(progBytes) + " / " + fmtBytes(t.Size)
	}
	speedTxt := "—"
	if t.Status == engineStatusRunning || t.Status == engineStatusSeeding {
		speedTxt = fmtSpeed(t.Speed)
	}
	proxy := "跟随全局设置"
	if t.ProxyOverride == "none" {
		proxy = "直连(不走代理)"
	} else if t.ProxyOverride != "" {
		proxy = t.ProxyOverride
	}
	rows := [][2]string{
		{"状态", StatusText(t.Status)},
		{"类型", map[string]string{"hls": "HLS 流媒体", "bt": "BitTorrent"}[t.Kind]},
	}
	if rows[1][1] == "" {
		rows[1][1] = "HTTP 直链"
	}
	rows = append(rows,
		[2]string{"保存目录", t.Dir},
		[2]string{"大小", sizeTxt},
		[2]string{"速度", speedTxt},
		[2]string{"线程/连接", strconv.Itoa(t.Connections)},
		[2]string{"代理", proxy},
		[2]string{"创建时间", fmtTime(t.CreatedAt)},
	)
	if isBT {
		ratio := "—"
		if t.Downloaded > 0 {
			ratio = fmt.Sprintf("%.2f", float64(t.Uploaded)/float64(t.Downloaded))
		}
		rows = append(rows,
			[2]string{"InfoHash", orDash(t.InfoHash)},
			[2]string{"piece", fmt.Sprintf("%d / %d 已完成", t.PiecesDone, t.PiecesTotal)},
			[2]string{"已上传", fmtBytes(t.Uploaded) + "(分享率 " + ratio + ")"},
			[2]string{"做种策略(本任务)", seedPolicyText(t.SeedRatio, t.SeedMinutes)},
		)
	}
	if t.Status == engineStatusCompleted && !t.CompletedAt.IsZero() {
		rows = append(rows, [2]string{"完成时间", fmtTime(t.CompletedAt)})
	}
	if t.Status == engineStatusError && t.Err != "" {
		rows = append(rows, [2]string{"错误", t.Err})
	}
	if len(det.Files) > 0 {
		rows = append(rows, [2]string{"文件", fmt.Sprintf("%d 个", len(det.Files))})
	}
	// 行集签名(键序+BT 收尾钮):不变则只原位改值——概览每秒重建全部
	// 行控件曾是全局卡顿源之一
	sig := fmt.Sprint(isBT, "|", t.Status == engineStatusCompleted && !t.CompletedAt.IsZero(),
		"|", t.Status == engineStatusError && t.Err != "", "|", len(det.Files) > 0)
	for _, r := range rows {
		sig += "\x00" + r[0]
	}
	if sig == d.ovSig && len(d.ovVals) == len(rows) {
		for i := range rows {
			if d.ovVals[i].Text != rows[i][1] {
				d.ovVals[i].SetText(rows[i][1])
			}
		}
		return
	}
	d.ovSig = sig
	kids := make([]fyne.CanvasObject, 0, len(rows)+1)
	pal := d.app.store.appPal()
	d.ovVals = make([]*widget.Label, 0, len(rows))
	for _, r := range rows {
		// kv-grid:左 96px 键列(dim) + 值列自动换行
		k := newColorLabelS(r[0], pal.textDim, false, 12.5)
		v := widget.NewLabel(r[1])
		v.Wrapping = fyne.TextWrapWord
		v.TextStyle = fyne.TextStyle{}
		d.ovVals = append(d.ovVals, v)
		row := container.NewBorder(nil, nil,
			newFixedMin(container.NewStack(k), fyne.NewSize(96, 18)), nil, v)
		kids = append(kids, row)
	}
	// 另存为种子(BT)——后端弹原生另存对话框
	if isBT {
		save := newGhostButton(pal, "另存为种子…", nil)
		save.SetMini()
		save.onTap = func() {
			go func() {
				// 种子字节取自引擎,另存对话框 zdlui 进程内直弹(引擎已无头)
				data, err := d.app.client.ExportTorrent(d.taskID)
				if err != nil {
					fyne.Do(func() { d.app.toast(err.Error(), "err") })
					return
				}
				name := d.taskID + ".torrent"
				if t, ok := d.app.store.Task(d.taskID); ok && t.Filename != "" {
					name = t.Filename + ".torrent"
				}
				p, ok, err := winfd.SaveFile(winfd.Options{
					Title: "另存为", FileName: name, DefaultExtension: ".torrent",
				})
				fyne.Do(func() {
					if err != nil || !ok {
						d.app.toast("已取消另存", "")
						return
					}
					if werr := os.WriteFile(p, data, 0o644); werr != nil {
						d.app.toast(werr.Error(), "err")
						return
					}
					d.app.toast("已另存种子到 "+p, "ok")
				})
			}()
		}
		kids = append(kids, container.NewHBox(layout.NewSpacer(), save))
	}
	d.overview.Objects = kids
	d.overview.Refresh()
}

func seedPolicyText(ratio float64, minutes int) string {
	if ratio <= 0 && minutes <= 0 {
		return "跟随全局设置"
	}
	var parts []string
	if ratio > 0 {
		parts = append(parts, "分享率 "+fmt.Sprintf("%g", ratio))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", minutes))
	}
	return strings.Join(parts, " 或 ")
}

/* ---------------- 速度页(=speed 页签) ---------------- */

// speedPane 速度页(=speed 页签:双卡片 + 300px 曲线图 + 图例 + 限速表单)
func (d *drawer) speedPane() fyne.CanvasObject {
	pal := d.app.store.appPal()
	d.downCard = newColorLabelS("—", pal.text, true, 22)
	d.upCard = newColorLabelS("—", pal.text, true, 22)
	d.chart = newTaskSpeedChart()
	// speed-card ×2:flex:1 等分(卡内 22px 粗值 + 11.5px 淡标签;=speed-cards)
	card := func(v *colorLabel, name string) fyne.CanvasObject {
		bg := canvas.NewRectangle(pal.softA(0.60))
		bg.CornerRadius = 9
		stroke := canvas.NewRectangle(color.NRGBA{A: 0})
		stroke.CornerRadius = 9
		stroke.StrokeWidth = 1
		stroke.StrokeColor = pal.border
		inner := container.NewVBox(v, container.New(
			layout.NewCustomPaddedLayout(2, 0, 0, 0),
			newColorLabelS(name, pal.textFaint, false, 11.5)))
		return container.NewStack(bg, stroke,
			container.New(layout.NewCustomPaddedLayout(14, 14, 18, 18), inner))
	}
	cards := hFlex(12, flex(card(d.downCard, "下载速度")), flex(card(d.upCard, "上传速度")))

	// 图表包裹层:border + radius 9 + bgSoft 45% + padding 8(=speed-chart-wrap)
	wrapBg := canvas.NewRectangle(pal.softA(0.45))
	wrapBg.CornerRadius = 9
	wrapStroke := canvas.NewRectangle(color.NRGBA{A: 0})
	wrapStroke.CornerRadius = 9
	wrapStroke.StrokeWidth = 1
	wrapStroke.StrokeColor = pal.border
	chartWrap := newFixedMin(container.NewStack(wrapBg, wrapStroke,
		container.New(layout.NewCustomPaddedLayout(8, 8, 8, 8), d.chart)), fyne.NewSize(740, 316))

	// 图例(色条 10x4;右侧说明 =.dim text-dim)
	lg := func(c color.Color, name string) fyne.CanvasObject {
		dot := canvas.NewRectangle(c)
		dot.SetMinSize(fyne.NewSize(10, 4))
		dot.CornerRadius = 2
		return hGap(6, dot, newColorLabelS(name, pal.textDim, false, 12))
	}
	legend := container.NewBorder(nil, nil,
		hGap(16, lg(pal.accent, "下载"), lg(pal.ok, "上传")),
		nil, newColorLabelS("最近 3 分钟(每秒采样)", pal.textDim, false, 12))

	// 限速表单(label 在上 + 数值/单位行;=field+unit-row)
	formRow := func(label string, e *widget.Entry, sel *zSelect) fyne.CanvasObject {
		selWrap := newFixedMin(container.NewStack(sel), fyne.NewSize(96, 36))
		return container.NewVBox(
			newColorLabelS(label, pal.textDim, true, 12.5),
			container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0),
				container.NewBorder(nil, nil, nil, selWrap, e)))
	}
	d.dwLimit = widget.NewEntry()
	singleLine(d.dwLimit) // 横向裁剪、无纵向滚动条(=input)
	d.dwLimitUnit = newZSelect(pal, []string{"KB/s", "MB/s"}, "KB/s")
	d.dwUpLimit = widget.NewEntry()
	singleLine(d.dwUpLimit)
	d.dwUpLimitUnit = newZSelect(pal, []string{"KB/s", "MB/s"}, "KB/s")
	onInput := func(string) { d.limitDirty = true }
	d.dwLimit.OnChanged = onInput
	d.dwUpLimit.OnChanged = onInput
	d.dwLimField = formRow("每任务下载限速(0 不限,保存后立即生效)", d.dwLimit, d.dwLimitUnit)
	d.dwUpLimField = formRow("每任务上传限速(0 不限,BT 任务)", d.dwUpLimit, d.dwUpLimitUnit)

	d.saveBtn = newPrimaryButton(pal, "保存限速", func() { d.saveLimits() })
	actions := container.NewBorder(nil, nil, nil,
		hGap(8, newGhostButton(pal, "刷新", func() { d.pollOnce() }), d.saveBtn))

	// 间距节奏逐段对齐 Vue(speed-cards margin 4/12、chart-legend margin-top 10、
	// pane-sep margin 16 + 1px 虚线、.drawer-body .field margin 10/14 兄弟折叠 14)
	// 右侧另加滚动条带宽:fyne 滚动条覆盖式(CSS 滚动条占布局空间),不留会压元素
	body := container.New(&vgapLayout{gap: 0},
		container.New(layout.NewCustomPaddedLayout(4, 12, 0, 8), cards),
		chartWrap,
		container.New(layout.NewCustomPaddedLayout(10, 16, 0, 8), legend),
		container.New(layout.NewCustomPaddedLayout(0, 30, 0, 8), newDashedLine(pal.border)),
		container.New(layout.NewCustomPaddedLayout(0, 14, 0, 8), d.dwLimField),
		container.New(layout.NewCustomPaddedLayout(0, 14, 0, 8), d.dwUpLimField),
		actions)
	// 表单局部主题:限速输入框/单位下拉获得 Vue 口径的文本内缩
	return vScrollNS(body)
}

// saveLimits 保存限速(=saveLimits:单位换算后 PUT,成功清 dirty)
func (d *drawer) saveLimits() {
	t, ok := d.app.store.Task(d.taskID)
	dl := int64(parseFloat(d.dwLimit.Text) * unitMult(d.dwLimitUnit.Selected))
	var ul int64
	if ok && t.Kind == "bt" {
		ul = int64(parseFloat(d.dwUpLimit.Text) * unitMult(d.dwUpLimitUnit.Selected))
	}
	go func() {
		err := d.app.client.UpdateTask(d.taskID, dl, ul)
		fyne.Do(func() {
			if err != nil {
				d.app.toast(err.Error(), "err")
				return
			}
			d.limitDirty = false // 已落库:下拍轮询按服务端值回填
			d.app.toast("任务属性已保存,限速立即生效", "ok")
			d.app.store.RefreshTasks()
		})
	}()
}

func unitMult(u string) float64 {
	if u == "MB/s" {
		return 1048576
	}
	return 1024
}

// applySpeed 速度页(=applyDrawer 的限速段 + drawTaskSpeed 节拍)
func (d *drawer) applySpeed(t *engine.TaskView) {
	d.downCard.SetText(fmtSpeed(int64(d.inst())))
	d.upCard.SetText(fmtSpeed(int64(d.instU2())))
	if h := d.app.store; h != nil {
	}
	d.chart.setSamples(d.pts())
	// 完成/已取消任务隐藏限速编辑与保存(=v-show)
	pal := d.app.store.appPal()
	if t.Status == engineStatusCompleted || t.Status == engineStatusCanceled {
		d.dwLimField.Hide()
		d.saveBtn.Hide()
	} else {
		d.dwLimField.Show()
		d.saveBtn.Show()
	}
	if t.Kind == "bt" {
		d.dwUpLimField.Show()
	} else {
		d.dwUpLimField.Hide()
	}
	_ = pal
	// 限速回显(编辑中不回写)
	if !d.limitDirty {
		v, u := unitInput(t.SpeedLimit)
		d.dwLimit.Text = fmtFloat(v)
		d.dwLimitUnit.SetSelected(u)
		if t.Kind == "bt" {
			uv, uu := unitInput(t.UPLimit)
			d.dwUpLimit.Text = fmtFloat(uv)
			d.dwUpLimitUnit.SetSelected(uu)
		}
		d.dwLimit.Refresh()
		d.dwUpLimit.Refresh()
	}
}

// inst/instU2/pts 采样读取助手
func (d *drawer) inst() float64 {
	d.sampleMu.Lock()
	defer d.sampleMu.Unlock()
	return d.instD
}
func (d *drawer) instU2() float64 {
	d.sampleMu.Lock()
	defer d.sampleMu.Unlock()
	return d.instU
}
func (d *drawer) pts() ([]float64, []float64) {
	d.sampleMu.Lock()
	defer d.sampleMu.Unlock()
	return append([]float64(nil), d.downPts...), append([]float64(nil), d.upPts...)
}

// unitInput 字节→{数值,单位}(=Drawer.vue unitInput:优先整 MB,其次整 KB)
func unitInput(bytes int64) (float64, string) {
	if bytes > 0 && bytes%1048576 == 0 {
		return float64(bytes / 1048576), "MB/s"
	}
	if bytes > 0 && bytes%1024 == 0 {
		return float64(bytes / 1024), "KB/s"
	}
	if bytes > 0 {
		return float64(bytes) / 1048576, "MB/s"
	}
	return 0, "KB/s"
}

func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func bigLabel(s string) *widget.Label {
	l := widget.NewLabel(s)
	l.TextStyle = fyne.TextStyle{Bold: true}
	return l
}

/* ---------------- 文件页(=files 页签 + FileTree view 模式) ---------------- */

func (d *drawer) filesPane() fyne.CanvasObject {
	d.fileBody = container.NewVBox(widget.NewLabel("元数据未就绪"))
	d.fileToolbar = container.NewHBox()
	// 不套外层滚动:ftTree 自带滚动区 + 固定表头(=ft-head position:sticky)
	return container.NewBorder(d.fileToolbar, nil, nil, nil, d.fileBody)
}

// applyFiles 文件页(=applyDrawer 文件段 + files 页签动作按钮)
func (d *drawer) applyFiles(t *engine.TaskView, det *engine.TaskDetailView) {
	isBT := t.Kind == "bt"
	if !isBT {
		return
	}
	d.filesStatus = t.Status
	all := det.Files
	d.filesHaveMet = len(all) > 0
	fts := make([]*ftFile, 0, len(all))
	for i := range all {
		f := &all[i]
		if FtIsPadding(&ftFile{Path: f.Path}) {
			continue
		}
		fts = append(fts, &ftFile{Path: f.Path, Size: f.Size, Done: f.Done, First: f.First, Last: f.Last, Wanted: f.Wanted})
	}

	if !d.filesHaveMet {
		msg := "元数据未就绪"
		if t.Status == engineStatusChoosing {
			msg = "正在获取元数据,就绪后弹出创建任务窗口"
		}
		// 占位态只建一次,之后原位改文案(每拍 new Label 是纯浪费)
		if d.fileMode != 1 || d.fileMsg == nil {
			d.fileMsg = widget.NewLabel(msg)
			d.fileBody.Objects = []fyne.CanvasObject{d.fileMsg}
			d.fileMode = 1
			d.fileBody.Refresh()
			d.fileToolbar.Objects = nil
			d.fileToolbar.Refresh()
			return
		}
		if d.fileMsg.Text != msg {
			d.fileMsg.SetText(msg)
		}
		return
	}
	if d.fileTree == nil {
		d.fileTree = newFtTreeView(d.app, fts, t, &d.selDirty)
		d.fileTree.SetPlayFn(func(path string) {
			// 文件树播放按钮 → 播放面板(M6 接入 libvlc)
			if d.app.store.OpenPlayer != nil {
				d.app.store.OpenPlayer(d.taskID, path)
			}
		})
		// 树只建一次;之后轮询走增量刷新(勾选/悬停态不丢)
		base := d.fileTree.onChange
		d.fileTree.onChange = func() {
			if base != nil {
				base()
			}
			d.refreshFilesToolbar() // 勾选变化立刻刷工具栏(=Vue 即时统计)
		}
		d.fileBody.Objects = []fyne.CanvasObject{d.fileTree}
		d.fileMode = 2
		d.fileBody.Refresh()
	} else {
		d.fileTree.updateData(fts, t)
		// 占位态又回来过(元数据消失重现):树还在但没挂回,补挂
		if d.fileMode != 2 {
			d.fileBody.Objects = []fyne.CanvasObject{d.fileTree}
			d.fileMode = 2
			d.fileBody.Refresh()
		}
	}
	d.refreshFilesToolbar()
}

// refreshFilesToolbar files 页签工具栏(勾选变化即时调用;签名不变不重建,
// 避免按钮每秒换新导致悬停态丢失)
func (d *drawer) refreshFilesToolbar() {
	if d.fileTree == nil {
		return
	}
	st := d.filesStatus
	editable := st != engineStatusCanceled
	showApply := editable && d.selDirty && st != engineStatusChoosing
	stN, stTotal, stBytes, _ := d.fileTree.SelStat()
	sig := fmt.Sprintf("%v|%v|%v|%d|%d|%s", editable, showApply,
		st == engineStatusChoosing, stN, stTotal, fmtBytes(stBytes))
	if sig == d.filesTBSig {
		return
	}
	d.filesTBSig = sig
	pal := d.app.store.appPal()
	tb := []fyne.CanvasObject{newColorLabelS("文件", pal.accent2, true, 12)}
	if editable {
		allBtn := newGhostButton(pal, "全选/全不选", nil)
		allBtn.SetMini()
		allBtn.onTap = func() {
			_, _, _, all := d.fileTree.SelStat()
			d.fileTree.SetAll(!all) // onChange 钩子会即时刷工具栏
		}
		tb = append(tb, layout.NewSpacer(), allBtn)
	}
	if showApply {
		applyBtn := newPrimaryButton(pal, fmt.Sprintf("应用更改(已选 %d/%d · %s)", stN, stTotal, fmtBytes(stBytes)), nil)
		applyBtn.SetMini()
		applyBtn.onTap = func() { d.applySelection() }
		tb = append(tb, applyBtn)
	}
	if st == engineStatusChoosing {
		txt := "创建下载任务…"
		if stTotal > 0 {
			txt = fmt.Sprintf("创建下载任务(已选 %d/%d · %s)", stN, stTotal, fmtBytes(stBytes))
		}
		cBtn := newPrimaryButton(pal, txt, nil)
		cBtn.SetMini()
		cBtn.onTap = func() { d.chooseCreate() }
		tb = append(tb, container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), cBtn))
	}
	d.fileToolbar.Objects = tb
	d.fileToolbar.Refresh()
}

// applySelection 应用勾选(=applySel)
func (d *drawer) applySelection() {
	if d.fileTree == nil {
		return
	}
	sel := d.fileTree.SelectedPaths()
	go func() {
		var err error
		if sel == nil {
			err = d.app.client.SetTaskFiles(d.taskID, true, nil)
		} else {
			err = d.app.client.SetTaskFiles(d.taskID, false, sel)
		}
		fyne.Do(func() {
			if err != nil {
				d.app.toast("应用失败: "+err.Error(), "err")
				return
			}
			d.selDirty = false
			d.app.toast("文件选择已更新", "ok")
			d.app.store.RefreshTasks()
		})
	}()
}

// chooseCreate choosing 任务按勾选直接创建(=chooseCreate)
func (d *drawer) chooseCreate() {
	if d.fileTree == nil {
		d.app.toast("元数据尚未就绪,稍后再试", "err")
		return
	}
	sel := d.fileTree.SelectedPaths()
	go func() {
		var err error
		if sel == nil {
			err = d.app.client.SetTaskFiles(d.taskID, true, nil)
		} else {
			err = d.app.client.SetTaskFiles(d.taskID, false, sel)
		}
		fyne.Do(func() {
			if err != nil {
				d.app.toast("创建失败: "+err.Error(), "err")
				return
			}
			d.selDirty = false
			d.app.toast("下载任务已创建", "ok")
			d.app.store.RefreshTasks()
		})
	}()
}

/* ---------------- 连接页(=peers 页签) ---------------- */

// 列宽给足表头+排序箭头(↑/↓)余量:Vue 是 auto 表格布局列随内容加宽,
// fyne 固定列宽,贴边列一点排序箭头就截断
var peerCols = []struct {
	title string
	width float32
}{
	{"", 25}, {"IP", 150}, {"客户端", 150}, {"远程端口", 74}, {"监听端口", 74},
	{"进度", 56}, {"分块图", 130}, {"来源", 60}, {"下载速度", 84}, {"上传速度", 84},
	{"下载大小", 84}, {"上传大小", 84}, {"piece有效率", 96},
	{"发起", 56}, {"协议", 50}, {"加密", 48}, {"连接时间", 74}, {"标志", 58},
}

func (d *drawer) peersPane() fyne.CanvasObject {
	// =.bt-table:12px、无可视分割线(行底 1px 线由 menuCell 画=td border-bottom);
	// 格内分四层(文本/状态圆点/分块图/来源徽章),按列显隐
	inner := widget.NewTable(
		func() (int, int) { return len(d.peerRows) + 1, len(peerCols) }, // row0=表头
		func() fyne.CanvasObject {
			pal := d.app.store.appPal()
			lbl := newColorLabelS("", pal.text, false, 12).SetMaxWidth()
			// 行盒对齐 Vue 行距 27px(td 4+line-height 1.5×12=18+4、+1px 线;
			// fyne 文本盒 16.34px,余量摊上下;列间隙 4px 由 menuCell 底色外扩盖掉)
			padV := (23 - lbl.MinSize().Height) / 2
			if padV < 0 {
				padV = 0
			}
			return container.NewStack(
				container.New(layout.NewCustomPaddedLayout(padV, padV, 8, 8), lbl),
				newStDot(),
				container.NewCenter(newBFCell()),
				newBadge(),
			)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			pal := d.app.store.appPal()
			st := o.(*fyne.Container)
			lblWrap := st.Objects[0].(*fyne.Container)
			l := lblWrap.Objects[0].(*colorLabel)
			dot := st.Objects[1].(*stDot)
			bcWrap := st.Objects[2]
			bc := bcWrap.(*fyne.Container).Objects[0].(*bfCell)
			bdg := st.Objects[3].(*badge)
			// 池化复用:只调整有变化的层(Show/Hide 每次都标脏,
			// 秒级刷新全表调用是卡顿源)
			only := func(want fyne.CanvasObject) {
				for _, o := range []fyne.CanvasObject{lblWrap, dot, bcWrap, bdg} {
					if o == want {
						if !o.Visible() {
							o.Show()
						}
					} else if o.Visible() {
						o.Hide()
					}
				}
			}
			l.SetBold(false)
			l.SetColor(pal.text)
			l.SetAlign(fyne.TextAlignLeading)
			txt := func(s string) {
				only(lblWrap)
				l.SetText(s)
			}
			dim := func(s string) { // =dim-cell
				only(lblWrap)
				l.SetColor(pal.textDim)
				l.SetText(s)
			}
			num := func(s string) { // =.num 右对齐
				only(lblWrap)
				l.SetAlign(fyne.TextAlignTrailing)
				l.SetText(s)
			}
			if id.Row == 0 {
				// =th:color var(--text-dim)、font-weight 500(雅黑无 500 面,常规体最接近)
				dim(d.peerHeadTitle(id.Col))
				return
			}
			if id.Row > len(d.peerRows) {
				return
			}
			r := &d.peerRows[id.Row-1]
			switch id.Col {
			case 0: // =st-dot:下载中 ok、上传中 warn、其余灰
				only(dot)
				switch {
				case r.downRateN > 1024:
					dot.setColor(pal.ok)
				case r.upRateN > 1024:
					dot.setColor(pal.warn)
				default:
					dot.setColor(pal.textFaint)
				}
			case 1:
				txt(r.ip + r.banned)
			case 2:
				dim(r.client)
			case 3:
				num(r.port)
			case 4:
				num(r.lport)
			case 5:
				num(r.prog)
			case 6: // 分块图
				only(bcWrap)
				bc.SetBF(r.bitf)
			case 7: // =src-badge 来源徽章
				only(bdg)
				fg, bg := srcBadgeColor(pal, r.srcKey)
				bdg.set(r.src, fg, bg)
			case 8:
				num(r.downRate)
			case 9:
				num(r.upRate)
			case 10:
				num(r.down)
			case 11:
				num(r.up)
			case 12:
				// =.eff-warn/.eff-bad:掺坏块警示,bad 加粗
				num(r.effTxt)
				if r.effCls == "bad" {
					l.SetColor(pal.errc)
					l.SetBold(true)
				} else if r.effCls == "warn" {
					l.SetColor(pal.warn)
				}
			case 13:
				dim(r.inbound)
			case 14:
				txt(r.proto)
			case 15:
				txt(r.enc)
			case 16:
				num(r.connFor)
			case 17:
				dim(r.flags)
			}
		},
	)
	inner.HideSeparators = true
	for i, c := range peerCols {
		inner.SetColumnWidth(i, c.width)
	}
	colW := make([]float32, len(peerCols))
	for i, c := range peerCols {
		colW[i] = c.width
	}
	mt := newMenuTable(d.app, inner, func(row int) bool { return d.peerSel.has(row) })
	mt.colW = colW
	// 表头点击=排序(=toggleSort;状态点/分块图列不可排,=Vue 该 th 无 data-sk)
	mt.OnHeadTap = func(col int) {
		if col == 0 || col == 6 {
			return
		}
		if col == d.peerSortCol {
			d.peerSortDir = -d.peerSortDir
		} else {
			d.peerSortCol, d.peerSortDir = col, 1
		}
		d.sortPeers(d.peerRows)
		mt.refreshData() // 行序变了但行数没变:可见格原位套新数据
	}
	mt.OnRowTap = func(id widget.TableCellID) {
		old := d.peerSel.sel
		d.peerSel.set(id.Row)
		mt.restyleRows(old, id.Row) // 选中高亮只重画两行
	}
	mt.OnBlankTap = func() {
		old := d.peerSel.sel
		d.peerSel.clear()
		mt.restyleRows(old)
	}
	mt.OnRowCtx = func(id widget.TableCellID, abs fyne.Position) {
		d.peerCtxMenu(id, abs)
	}
	// 表头/空白处右键=手动添加连接(=onPeerCtx 无 tr[data-addr] 分支)
	mt.OnBlankCtx = func(abs fyne.Position) {
		d.app.showMenu(abs.X, abs.Y, []menuRowItem{
			newMenuLabel(d.app, "手动添加连接…", false, func() { d.openPeerAdd() }),
		})
	}
	d.peerTable = mt

	// Vue peers 页无工具栏,右键是唯一操作入口(=onPeerCtx)
	return tableWrap(d.app.store.appPal(), mt)
}

// peerCtxMenu 连接表数据行右键菜单(=onPeerCtx 的 tr[data-addr] 分支)
func (d *drawer) peerCtxMenu(id widget.TableCellID, abs fyne.Position) {
	items := []menuRowItem{}
	rowIdx := id.Row - 1
	if rowIdx < 0 || rowIdx >= len(d.peerRows) {
		return
	}
	r := &d.peerRows[rowIdx]
	items = append(items,
		newMenuLabel(d.app, "断开连接", false, func() {
			go d.peerOp(PeerOps{Remove: r.addr}, "连接已断开")
		}),
		newMenuLabel(d.app, "封锁此 IP…", false, func() {
			d.app.store.ConfirmBox(ConfirmReq{
				Message: "将封锁 " + r.ip + " 并断开其所有连接,本任务内不再与其建立连接,确定?",
				OKLabel: "封锁",
				OnOk:    func() { go d.peerOp(PeerOps{Ban: r.ip}, "已封锁 "+r.ip) },
			})
		}),
	)
	if r.client != "" && r.client != "未知" {
		client := r.client
		items = append(items, sepItem(),
			newMenuLabel(d.app, "封禁所有「"+client+"」…", false, func() {
				d.app.store.ConfirmBox(ConfirmReq{
					Message: "将封禁全部任务中客户端名为「" + client + "」的 peer 并断开连接,并把「" + client + "」加入客户端黑名单,确定?",
					OKLabel: "封禁",
					OnOk: func() {
						go func() {
							n, err := d.app.client.BanClient(client)
							fyne.Do(func() {
								if err != nil {
									d.app.toast(err.Error(), "err")
									return
								}
								d.app.toast(fmt.Sprintf("已封禁 %d 个 IP,「%s」已列入客户端黑名单", n, client), "ok")
							})
						}()
					},
				})
			}))
	}
	items = append(items, sepItem(),
		newMenuLabel(d.app, "复制地址", false, func() {
			fyne.CurrentApp().Clipboard().SetContent(r.addr)
		}),
		sepItem(),
		newMenuLabel(d.app, "手动添加连接…", false, func() { d.openPeerAdd() }),
	)
	d.app.showMenu(abs.X, abs.Y, items)
}

// tableWrap 表格卡片壳(=table-scroll:1px 边框 + radius 9)。
// 套 noShadow 主题:去掉 fyne 滚动区四边的渐变黑影(=表格四周那圈黑阴影)
func tableWrap(pal palette, t fyne.CanvasObject) fyne.CanvasObject {
	stroke := canvas.NewRectangle(color.NRGBA{A: 0})
	stroke.CornerRadius = 9
	stroke.StrokeWidth = 1
	stroke.StrokeColor = pal.border
	return container.NewStack(stroke, t)
}

// peerAction 连接操作 + 提示(=peerAction)
func (d *drawer) peerOp(ops PeerOps, okMsg string) {
	_, _, err := d.app.client.TaskPeers(d.taskID, ops)
	fyne.Do(func() {
		if err != nil {
			d.app.toast(err.Error(), "err")
			return
		}
		d.app.toast(okMsg, "ok")
	})
}

// openPeerAdd 手动添加连接(=peerAddOverlay)。自绘 modalCard + 自研按钮
// (原 dialog.NewForm 读 Shadow 画框已随 Shadow 全局透明失效 → 无框 + 默认钮)
func (d *drawer) openPeerAdd() {
	pal := d.app.store.appPal()
	e := widget.NewMultiLineEntry()
	e.Wrapping = fyne.TextWrapWord // 多行回绕,不出横向滚动条(=textarea)
	e.SetPlaceHolder("每行一个 peer 地址(ip:port)\n192.168.1.100:24521\n[2001:db8::1]:24521")
	var pop *maskModal
	okBtn := newPrimaryButton(pal, "连接", nil)
	submit := func() {
		var lines []string
		for _, ln := range strings.Split(e.Text, "\n") {
			if s := strings.TrimSpace(ln); s != "" {
				lines = append(lines, s)
			}
		}
		if len(lines) == 0 {
			d.app.toast("请输入至少一个 peer 地址(ip:port)", "err")
			return
		}
		go func() {
			added, _, err := d.app.client.TaskPeers(d.taskID, PeerOps{Add: lines})
			fyne.Do(func() {
				if err != nil {
					d.app.toast(err.Error(), "err")
					return
				}
				d.app.toast(fmt.Sprintf("已提交 %d 个连接请求,握手成功即出现在列表", added), "ok")
			})
		}()
		pop.Hide()
	}
	okBtn.onTap = submit
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("手动添加连接", pal.text, true, 17))
	field := container.NewVBox(newColorLabelS("地址", pal.textDim, true, 12.5),
		container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0),
			newFixedMin(container.NewStack(e), fyne.NewSize(0, 120))))
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }), okBtn)
	body := container.New(&vgapLayout{gap: 12}, head, field,
		container.NewBorder(nil, nil, nil, actions))
	pop = modalCard(d.app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 22, 24, 24), body), 520, 0)
	pop.Show()
}

// applyPeers 连接表(=buildPeerRows;默认 IP 点分段升序)
func (d *drawer) applyPeers(det *engine.TaskDetailView) {
	banned := map[string]bool{}
	for _, ip := range det.Banned {
		banned[ip] = true
	}
	rows := make([]peerRow, 0, len(det.Peers))
	for i := range det.Peers {
		p := &det.Peers[i]
		ip, port := splitAddr(p.Addr)
		portN, _ := strconv.ParseInt(port, 10, 64)
		effN := p.PiecesOK + p.PiecesBad
		eff := -1.0
		effTxt := "—"
		effCls := ""
		if effN > 0 {
			eff = float64(p.PiecesOK) / float64(effN)
			effTxt = fmt.Sprintf("%.0f%%", eff*100)
			if eff < 1 {
				effTxt = fmt.Sprintf("%.1f%%", eff*100)
			}
			if p.PiecesBad >= 3 {
				effCls = "bad"
			} else if p.PiecesBad > 0 {
				effCls = "warn"
			}
		}
		rows = append(rows, peerRow{
			addr: p.Addr, ip: ip, client: orDash(p.Client), port: port,
			lport:    strconv.FormatInt(p.ListenPort, 10),
			prog:     fmt.Sprintf("%.0f%%", p.Progress*100),
			src:      srcText(p.Source),
			srcKey:   p.Source,
			downRate: speedOrEmpty(int64(p.DownRate)),
			upRate:   speedOrEmpty(int64(p.UpRate)),
			down:     fmtBytes(p.Down), up: fmtBytes(p.Up),
			effTxt: effTxt, effCls: effCls, eff: eff, bitf: p.Bitfield,
			inbound: map[bool]string{true: "远程", false: "本地"}[p.Inbound],
			proto:   map[bool]string{true: "uTP", false: "TCP"}[p.UTP],
			enc:     map[bool]string{true: "E", false: ""}[p.Encrypted],
			connFor: fmtDur(p.ConnectedFor),
			flags:   p.Flags,
			banned:  map[bool]string{true: " 已封锁", false: ""}[banned[ip]],
			portN:   portN, lportN: p.ListenPort,
			progN: p.Progress, downRateN: p.DownRate, upRateN: p.UpRate,
			downN: float64(p.Down), upN: float64(p.Up), connN: p.ConnectedFor,
			inbN:   map[bool]int{true: 1, false: 0}[p.Inbound],
			protoN: map[bool]int{true: 1, false: 0}[p.UTP],
			encN:   map[bool]int{true: 1, false: 0}[p.Encrypted],
		})
	}
	d.sortPeers(rows)
	old := d.peerRows
	switch {
	case len(rows) != len(old):
		d.peerRows = rows
		d.peerTable.Refresh() // 行数变化:滚动条/补白格需整表重排
	case peerRowsEqual(old, rows):
		// 快照全等:零重绘
	default:
		d.peerRows = rows
		d.peerTable.refreshData() // 速率/进度小变化:可见格原位更新
	}
}

// peerRowsEqual 行快照全等(peerRow 全可比字段;排序在比较前完成,序也参与)
func peerRowsEqual(a, b []peerRow) bool {
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

// peerSortVal 连接表排序取值(=v.* 口径:数字列取原值,文本列取展示串;
// 列 0 状态点、列 6 分块图,不可排序)
func (r *peerRow) peerSortVal(col int) any {
	switch col {
	case 1:
		return r.ip
	case 2:
		return r.client
	case 3:
		return r.portN
	case 4:
		return r.lportN
	case 5:
		return r.progN
	case 7:
		return r.src
	case 8:
		return r.downRateN
	case 9:
		return r.upRateN
	case 10:
		return r.downN
	case 11:
		return r.upN
	case 12:
		return r.eff
	case 13:
		return r.inbN
	case 14:
		return r.protoN
	case 15:
		return r.encN
	case 16:
		return r.connN
	case 17:
		return r.flags
	}
	return ""
}

// srcBadgeColor 来源徽章语义色(=.src-badge 变体:dht 紫、pex 青、incoming 绿,
// 其余 accent;底=前景 14~16% 透明)
func srcBadgeColor(pal palette, key string) (fg, bg color.Color) {
	switch key {
	case "dht":
		return pal.purple, withAlpha(pal.purple, 0.16)
	case "pex":
		return pal.accent2, withAlpha(pal.accent2, 0.15)
	case "incoming":
		return pal.ok, withAlpha(pal.ok, 0.15)
	}
	return pal.accent, withAlpha(pal.accent, 0.14)
}

// cmpCell 排序值比较:数字比大小,字符串按字典序;返回 -1/0/1
func cmpCell(a, b any) int {
	switch av := a.(type) {
	case int64:
		if bv, ok := b.(int64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case float64:
		if bv, ok := b.(float64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	}
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	return strings.Compare(as, bs)
}

// bfCell peer 分块图(=BitfieldCanvas:110×13,每竖列对应一段 piece,
// 全有实蓝 #3b82f6、部分半蓝 45%;位图不变不重绘,避免每秒纹理上传)
type bfCell struct {
	widget.BaseWidget
	bf   string // 上次绘制的位图
	rast *canvas.Raster
}

func newBFCell() *bfCell {
	b := &bfCell{}
	b.ExtendBaseWidget(b)
	b.rast = canvas.NewRaster(b.draw)
	b.rast.SetMinSize(fyne.NewSize(110, 13))
	return b
}

// SetBF 换位图(同值早退)
func (b *bfCell) SetBF(bf string) {
	if b.bf == bf {
		return
	}
	b.bf = bf
	b.rast.Refresh()
}

func (b *bfCell) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: b.rast, min: fyne.NewSize(110, 13)}
}

func (b *bfCell) draw(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	if b.bf == "" {
		return img
	}
	raw, err := base64.StdEncoding.DecodeString(b.bf)
	if err != nil {
		return img
	}
	n := len(raw) * 8
	full := color.NRGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 255}
	part := color.NRGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 115} // 0.45
	for x := 0; x < w; x++ {
		s := x * n / w
		e := (x + 1) * n / w
		if e <= s {
			e = s + 1
		}
		any, all := false, true
		for i := s; i < e && i < n; i++ {
			if (raw[i>>3]>>(7-(i&7)))&1 == 1 {
				any = true
			} else {
				all = false
			}
		}
		if !any {
			continue
		}
		c := part
		if all {
			c = full
		}
		for y := 1; y < h-1; y++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// sortPeers 连接表排序(=sortRows(rows, peerSort, ["ip","rport"]):
// 主键按 dir,同值按 IP 点分段、再按远程端口升序兜底)
func (d *drawer) sortPeers(rows []peerRow) {
	col, dir := d.peerSortCol, d.peerSortDir
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if c := cmpCell(a.peerSortVal(col), b.peerSortVal(col)); c != 0 {
			if dir >= 0 {
				return c < 0
			}
			return c > 0
		}
		if c := cmpIp(a.ip, b.ip); c != 0 {
			return c < 0
		}
		return a.portN < b.portN
	})
}

func speedOrEmpty(n int64) string {
	if n <= 0 {
		return ""
	}
	return fmtSpeed(n)
}

func splitAddr(addr string) (ip, port string) {
	i := strings.LastIndexByte(addr, ':')
	if i > 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, ""
}

/* ---------------- Tracker 页(=trackers 页签) ---------------- */

var trCols = []struct {
	title string
	width float32
}{
	{"Tracker / 来源", 300}, {"状态", 58}, {"剩余时间", 82}, {"种子", 58},
	{"下载中", 64}, {"全部用户", 72}, {"重试", 54}, {"最近消息", 220},
}

func (d *drawer) trackersPane() fyne.CanvasObject {
	// =.bt-table:12px、无可视分割线(行底 1px 线由 menuCell 画=td border-bottom);
	// 状态列是圆点不是文字(=st-dot)
	inner := widget.NewTable(
		func() (int, int) { return len(d.trRows) + 1, len(trCols) },
		func() fyne.CanvasObject {
			pal := d.app.store.appPal()
			lbl := newColorLabelS("", pal.text, false, 12).SetMaxWidth()
			padV := (23 - lbl.MinSize().Height) / 2 // 行盒对齐 Vue 行距 27px(见 peersPane 注释)
			if padV < 0 {
				padV = 0
			}
			return container.NewStack(
				container.New(layout.NewCustomPaddedLayout(padV, padV, 8, 8), lbl),
				newStDot(),
			)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			pal := d.app.store.appPal()
			st := o.(*fyne.Container)
			lblWrap := st.Objects[0].(*fyne.Container)
			l := lblWrap.Objects[0].(*colorLabel)
			dot := st.Objects[1].(*stDot)
			// 池化复用:只调整有变化的层(Show/Hide 每次都标脏,卡顿源)
			only := func(want fyne.CanvasObject) {
				for _, o := range []fyne.CanvasObject{lblWrap, dot} {
					if o == want {
						if !o.Visible() {
							o.Show()
						}
					} else if o.Visible() {
						o.Hide()
					}
				}
			}
			l.SetBold(false)
			l.SetColor(pal.text)
			l.SetAlign(fyne.TextAlignLeading)
			txt := func(s string) {
				only(lblWrap)
				l.SetText(s)
			}
			dim := func(s string) { // =dim-cell
				only(lblWrap)
				l.SetColor(pal.textDim)
				l.SetText(s)
			}
			num := func(s string) { // =.num 右对齐
				only(lblWrap)
				l.SetAlign(fyne.TextAlignTrailing)
				l.SetText(s)
			}
			if id.Row == 0 {
				// =th:color var(--text-dim)、font-weight 500(雅黑无 500 面,常规体最接近)
				dim(d.trHeadTitle(id.Col))
				return
			}
			if id.Row > len(d.trRows) {
				return
			}
			r := &d.trRows[id.Row-1]
			switch id.Col {
			case 0:
				txt(orDash(r.url))
			case 1: // =st-dot:✓ ok、✗ err、未应答 warn
				only(dot)
				switch r.stTxt {
				case "✓":
					dot.setColor(pal.ok)
				case "✗":
					dot.setColor(pal.errc)
				default:
					dot.setColor(pal.warn)
				}
			case 2:
				num(r.remain)
			case 3:
				num(r.seeds)
			case 4:
				num(r.leech)
			case 5:
				num(r.users)
			case 6:
				num(r.retries)
			case 7:
				dim(r.msg)
			}
		},
	)
	inner.HideSeparators = true
	for i, c := range trCols {
		inner.SetColumnWidth(i, c.width)
	}
	colW := make([]float32, len(trCols))
	for i, c := range trCols {
		colW[i] = c.width
	}
	mt := newMenuTable(d.app, inner, func(row int) bool { return d.trSel.has(row) })
	mt.colW = colW
	// 全部 8 列可排序(=Vue th data-sk:url/st/remain/seeds/leech/users/retries/msg)
	mt.OnHeadTap = func(col int) {
		if d.trSortCol == col {
			d.trSortDir = -d.trSortDir
		} else {
			d.trSortCol, d.trSortDir = col, 1
		}
		d.sortTrackers(d.trRows)
		mt.refreshData() // 行序变了但行数没变:可见格原位套新数据
	}
	mt.OnRowTap = func(id widget.TableCellID) {
		old := d.trSel.sel
		d.trSel.set(id.Row)
		mt.restyleRows(old, id.Row) // 选中高亮只重画两行
	}
	mt.OnBlankTap = func() {
		old := d.trSel.sel
		d.trSel.clear()
		mt.restyleRows(old)
	}
	mt.OnRowCtx = func(id widget.TableCellID, abs fyne.Position) {
		d.trackerCtxMenu(id, abs)
	}
	// 表头/空白处右键不弹(=onTrCtx 仅 tr[data-url] 有菜单)
	d.trTable = mt

	// 工具栏(=dw-pane > div:添加 Tracker…/重新通告)
	pal := d.app.store.appPal()
	tb := container.New(layout.NewCustomPaddedLayout(0, 0, 8, 8),
		hGap(8,
			newGhostButton(pal, "添加 Tracker…", func() { d.openTrackerAdd("") }),
			newGhostButton(pal, "重新通告", func() {
				go d.trackerOp(TrackerOps{Reannounce: true}, "已请求立即重新通告")
			})))
	return container.NewBorder(tb, nil, nil, nil, tableWrap(pal, mt))
}

// trackerCtxMenu Tracker 表数据行右键菜单(=onTrCtx 的 tr[data-url] 分支;
// DHT/PEX 虚拟行无 data-url,同 Vue 不弹菜单)
func (d *drawer) trackerCtxMenu(id widget.TableCellID, abs fyne.Position) {
	rowIdx := id.Row - 1
	if rowIdx < 0 || rowIdx >= len(d.trRows) {
		return
	}
	r := &d.trRows[rowIdx]
	if r.virtual {
		return
	}
	items := []menuRowItem{
		newMenuLabel(d.app, "编辑 Tracker…", false, func() {
			d.openTrackerAdd(r.url)
		}),
		newMenuLabel(d.app, "更新 Tracker", false, func() {
			go d.trackerOp(TrackerOps{Reannounce: true}, "已请求立即重新通告")
		}),
		sepItem(),
		newMenuLabel(d.app, "删除 Tracker", false, func() {
			d.app.store.ConfirmBox(ConfirmReq{
				Message: "移除 tracker?(确认后从任务中移除)\n" + r.url,
				OKLabel: "移除",
				OnOk:    func() { go d.trackerOp(TrackerOps{Remove: r.url}, "tracker 已删除") },
			})
		}),
		newMenuLabel(d.app, "查看日志", false, func() { d.showTrackerLog(r) }),
		sepItem(),
		newMenuLabel(d.app, "删除所有无效 Tracker…", false, func() { d.removeInvalidTrackers() }),
		newMenuLabel(d.app, "去除重复 Tracker", false, func() {
			go d.trackerOp(TrackerOps{Dedupe: true}, "已去除重复 tracker")
		}),
	}
	d.app.showMenu(abs.X, abs.Y, items)
}

// showTrackerLog 查看 tracker 日志弹窗(=showTrackerLog)。自绘 modalCard + 自研
// 按钮:只读 RichText 展示(正常字色、可回绕),单一「关闭」钮
func (d *drawer) showTrackerLog(r *trRow) {
	pal := d.app.store.appPal()
	lines := []string{"最近结果: " + orDash(r.msg)}
	if r.url != "" {
		lines = append(lines, "URL: "+r.url)
	}
	if r.retries != "" && r.retries != "0" {
		lines = append(lines, "连续失败: "+r.retries+" 次")
	}
	rt := widget.NewRichTextWithText(strings.Join(lines, "\n"))
	rt.Wrapping = fyne.TextWrapWord
	// 内容框:1px 边框圆角 9(=tableWrap 同画法),内衬 10,定高 160
	stroke := canvas.NewRectangle(color.NRGBA{A: 0})
	stroke.CornerRadius = 9
	stroke.StrokeWidth = 1
	stroke.StrokeColor = pal.border
	box := container.NewStack(stroke,
		container.New(layout.NewCustomPaddedLayout(10, 10, 10, 10),
			newFixedMin(container.NewStack(rt), fyne.NewSize(0, 160))))
	var pop *maskModal
	okBtn := newPrimaryButton(pal, "关闭", func() { pop.Hide() })
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("Tracker 日志", pal.text, true, 17))
	body := container.New(&vgapLayout{gap: 12}, head, box,
		container.NewBorder(nil, nil, nil, okBtn))
	pop = modalCard(d.app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 22, 24, 24), body), 520, 0)
	pop.Show()
}

// removeInvalidTrackers 删除所有通告失败的 tracker(=removeInvalidTrackers)
func (d *drawer) removeInvalidTrackers() {
	bad := []string{}
	for _, r := range d.trRows {
		if !r.virtual && r.stTxt == "✗" {
			bad = append(bad, r.url)
		}
	}
	if len(bad) == 0 {
		d.app.toast("没有通告失败的 tracker", "ok")
		return
	}
	d.app.store.ConfirmBox(ConfirmReq{
		Message: fmt.Sprintf("将删除 %d 个通告失败的 tracker,确定?", len(bad)),
		OKLabel: "删除",
		OnOk: func() {
			go func() {
				n := 0
				for _, url := range bad {
					if _, _, _, _, err := d.app.client.TaskTrackers(d.taskID, TrackerOps{Remove: url}); err == nil {
						n++
					}
				}
				fyne.Do(func() {
					d.app.toast(fmt.Sprintf("已删除 %d 个无效 tracker", n), "ok")
				})
			}()
		},
	})
}

// trackerOp tracker 操作 + 提示(=trackerAction)
func (d *drawer) trackerOp(ops TrackerOps, okMsg string) {
	_, _, _, _, err := d.app.client.TaskTrackers(d.taskID, ops)
	fyne.Do(func() {
		if err != nil {
			d.app.toast(err.Error(), "err")
			return
		}
		d.app.toast(okMsg, "ok")
	})
}

// openTrackerAdd 添加/编辑 Tracker(=trAddOverlay;编辑=先删后加,同 Vue)。
// 走自绘 modalCard + 自研按钮:原 dialog.NewForm 靠 Shadow 色画框,该色已全局
// 透明(根除字体缓存泄漏)→ 弹窗失去边框、OK/取消是 Fyne 默认钮,与全站不一致
func (d *drawer) openTrackerAdd(editURL string) {
	pal := d.app.store.appPal()
	e := widget.NewMultiLineEntry()
	e.Wrapping = fyne.TextWrapWord
	title, okLabel := "添加 Tracker", "添加"
	if editURL != "" {
		title, okLabel = "编辑 Tracker", "保存"
		e.SetText(editURL)
	} else {
		e.SetPlaceHolder("每行一个地址,支持 http(s):// 与 udp://")
	}
	var pop *maskModal
	okBtn := newPrimaryButton(pal, okLabel, nil)
	submit := func() {
		if editURL != "" {
			nu := firstLine(e.Text)
			if !isTrackerURL(nu) {
				d.app.toast("地址需以 http(s):// 或 udp:// 开头", "warn")
				return
			}
			if nu == editURL {
				pop.Hide() // 未改动直接关(=Vue 点保存后收起)
				return
			}
			go func() {
				d.app.client.TaskTrackers(d.taskID, TrackerOps{Remove: editURL})
				_, _, _, _, err := d.app.client.TaskTrackers(d.taskID, TrackerOps{Add: nu})
				fyne.Do(func() {
					if err != nil {
						d.app.toast(err.Error(), "err")
						return
					}
					d.app.toast("tracker 已更新", "ok")
				})
			}()
			pop.Hide()
			return
		}
		var lines []string
		for _, ln := range strings.Split(e.Text, "\n") {
			if s := strings.TrimSpace(ln); s != "" && isTrackerURL(s) {
				lines = append(lines, s)
			}
		}
		if len(lines) == 0 {
			d.app.toast("没有可添加的地址,每行需以 http(s):// 或 udp:// 开头", "warn")
			return
		}
		go func() {
			added, skipped, failed, _, err := d.app.client.TaskTrackers(d.taskID, TrackerOps{Adds: lines})
			fyne.Do(func() {
				if err != nil {
					d.app.toast(err.Error(), "err")
					return
				}
				parts := []string{fmt.Sprintf("已添加 %d 个 tracker 并开始通告", added)}
				if skipped > 0 {
					parts = append(parts, fmt.Sprintf("%d 个已存在(同服务器)跳过", skipped))
				}
				if len(failed) > 0 {
					parts = append(parts, fmt.Sprintf("%d 个添加失败", len(failed)))
				}
				cls := "ok"
				if len(failed) > 0 {
					cls = "warn"
				}
				d.app.toast(strings.Join(parts, ","), cls)
			})
		}()
		pop.Hide()
	}
	okBtn.onTap = submit
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS(title, pal.text, true, 17))
	field := container.NewVBox(newColorLabelS("地址", pal.textDim, true, 12.5),
		container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0),
			newFixedMin(container.NewStack(e), fyne.NewSize(0, 120))))
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }), okBtn)
	body := container.New(&vgapLayout{gap: 12}, head, field,
		container.NewBorder(nil, nil, nil, actions))
	pop = modalCard(d.app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 22, 24, 24), body), 560, 0)
	pop.Show()
}

// applyTrackers Tracker 表(=buildTrRows;虚拟 DHT/PEX 行置顶)
func (d *drawer) applyTrackers(det *engine.TaskDetailView) {
	rows := make([]trRow, 0, len(det.Trackers)+2)
	if det.DHTNodes > 0 {
		rows = append(rows, trRow{virtual: true, url: "DHT Network", stTxt: "✓",
			users: strconv.Itoa(det.DHTNodes), msg: fmt.Sprintf("路由表 %d 个节点", det.DHTNodes)})
	}
	if det.PEXReceived > 0 {
		rows = append(rows, trRow{virtual: true, url: "Peer Exchange", stTxt: "✓",
			users: strconv.Itoa(int(det.PEXReceived)), msg: fmt.Sprintf("PEX 累计发现 %d 个 peer", det.PEXReceived)})
	}
	for i := range det.Trackers {
		tr := &det.Trackers[i]
		st := "…"
		stN := 1
		if tr.LastErr != "" {
			st = "✗"
			stN = 2
		} else if tr.LastOK != "" {
			st = "✓"
			stN = 0
		}
		remain := "—"
		remainMs := math.MaxFloat64 // =remainMs() 无下次通告时间的 Infinity
		if tr.NextAnnounce != "" {
			if t, e := time.Parse(time.RFC3339, tr.NextAnnounce); e == nil {
				ms := time.Until(t).Milliseconds()
				remainMs = float64(ms)
				if ms <= 0 {
					remain = "即将通告"
				} else {
					remain = fmtDur(float64(ms) / 1000)
				}
			}
		}
		msg := tr.Message
		if msg == "" {
			msg = "—"
		}
		rows = append(rows, trRow{
			url: tr.URL, stTxt: st, remain: remain,
			seeds:   strconv.FormatInt(tr.Seeds, 10),
			leech:   strconv.FormatInt(tr.Leechers, 10),
			users:   strconv.FormatInt(tr.Seeds+tr.Leechers, 10),
			retries: strconv.Itoa(tr.Retries),
			msg:     msg,
			stN:     stN, remainMs: remainMs,
			seedsN: tr.Seeds, leechN: tr.Leechers,
			usersN: tr.Seeds + tr.Leechers, retriesN: int64(tr.Retries),
		})
	}
	d.sortTrackers(rows)
	old := d.trRows
	switch {
	case len(rows) != len(old):
		d.trRows = rows
		d.trTable.Refresh() // 行数变化:滚动条/补白格需整表重排
	case trRowsEqual(old, rows):
		// 快照全等:零重绘
	default:
		d.trRows = rows
		d.trTable.refreshData() // 倒计时/计数小变化:可见格原位更新
	}
}

// trRowsEqual 行快照全等(trRow 全可比字段)
func trRowsEqual(a, b []trRow) bool {
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

// peerHeadTitle 表头标题(排序列缀 ↑/↓,=th.sort-asc/desc 的 after 标记)
func (d *drawer) peerHeadTitle(col int) string {
	t := peerCols[col].title
	if col == d.peerSortCol {
		if d.peerSortDir >= 0 {
			t += " ↑"
		} else {
			t += " ↓"
		}
	}
	return t
}

// trHeadTitle Tracker 表头(仅 URL 列可排序)
func (d *drawer) trHeadTitle(col int) string {
	t := trCols[col].title
	if col == d.trSortCol {
		if d.trSortDir >= 0 {
			t += " ↑"
		} else {
			t += " ↓"
		}
	}
	return t
}

// sortTrackers Tracker 表排序(=sortRows(rows, trSort, ["url"]):全部 8 列
// 可排序,虚拟行以展示名/0 值参与;并列按 url 升序收尾,与方向无关)
func (d *drawer) sortTrackers(rows []trRow) {
	col, dir := d.trSortCol, d.trSortDir
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if c := cmpCell(a.trSortVal(col), b.trSortVal(col)); c != 0 {
			if dir >= 0 {
				return c < 0
			}
			return c > 0
		}
		return a.url < b.url
	})
}

func isTrackerURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "udp://")
}

func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

/* ---------------- 构建与一拍应用 ---------------- */

// buildDrawerUI 构建全部页签控件(打开时一次)
func (app *App) buildDrawerUI(d *drawer) {
	pal := app.store.appPal()
	d.buildOverview()
	// 右侧 8px 让位覆盖式滚动条(=dw-body padding 20px 外,滚动条压不到 kv 行)
	d.panes["overview"] = vScrollNS(container.New(
		layout.NewCustomPaddedLayout(0, 0, 0, 8), d.overview))
	d.panes["speed"] = d.speedPane()
	d.panes["files"] = d.filesPane()
	d.panes["peers"] = d.peersPane()
	d.panes["trackers"] = d.trackersPane()
	if t, ok := app.store.Task(d.taskID); ok {
		d.title = newColorLabelS(orDash2(t.Filename, MagnetFallbackName(t)), pal.text, true, 15.5).SetMaxWidth()
		d.sub = newColorLabelS(t.URL, pal.textDim, false, 11.5).SetMaxWidth()
		fg, bg := pal.statusChip(t.Status)
		d.chipLbl = newColorLabelS(StatusText(t.Status), fg, true, 11)
		d.chipBg = canvas.NewRectangle(bg)
		d.chipBg.CornerRadius = 11
	} else {
		d.title = newColorLabelS("任务详情", pal.text, true, 15.5)
		d.sub = newColorLabelS("", pal.textDim, false, 11.5)
		d.chipLbl = newColorLabelS("", pal.textDim, true, 11)
		d.chipBg = canvas.NewRectangle(color.NRGBA{A: 0})
		d.chipBg.CornerRadius = 11
	}
	// 空副标题不占行高(=Vue 空 .sub 高度为 0;否则标题区看起来是两行)
	if strings.TrimSpace(d.sub.Text()) == "" {
		d.sub.Hide()
	}
}

func orDash2(a, b string) string {
	if a != "" {
		return a
	}
	if b != "" {
		return b
	}
	return "(探测后确定)"
}

// apply 一拍应用全部页签(=applyDrawer)。标题/状态章恒刷;主体只刷当前
// 页签——fyne 里隐藏页签的表格 Refresh/文件树重建照样跑 UI 线程(整表
// SetObjects+moveIndicators、整树换行控件),是"Tracker 页卡"的主因;
// 切页时由 switchTab 用 lastDet 立即补一拍
func (d *drawer) apply(det *engine.TaskDetailView) {
	d.lastDet = det
	t, ok := d.app.store.Task(d.taskID)
	if !ok {
		// 快照未含(轮询兜底间隙):直接用 detail 字段渲染标题
		t = engine.TaskView{Kind: "bt", Status: engineStatusRunning}
		if det.Filename != "" {
			t.Filename = det.Filename
		}
	}
	if d.chipLbl != nil {
		fg, bg := d.app.store.appPal().statusChip(t.Status)
		d.chipLbl.SetText(StatusText(t.Status))
		d.chipLbl.SetColor(fg)
		d.chipBg.FillColor = bg
		d.chipBg.Refresh()
	}
	switch d.active {
	case "overview":
		d.applyOverview(&t, det)
	case "speed":
		d.applySpeed(&t)
	case "files":
		d.applyFiles(&t, det)
	case "peers":
		// 右键菜单显示中暂停刷新(=Vue 防错位:菜单绑定的是打开时刻的行,
		// 轮询重排会让菜单指向别的行)
		if !menuOpen() {
			d.applyPeers(det)
		}
	case "trackers":
		if !menuOpen() {
			d.applyTrackers(det)
		}
	}
}
