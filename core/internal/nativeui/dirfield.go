package nativeui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// dirField 可编辑目录选择框(=DirField.vue):输入框可自由输入/粘贴覆盖默认,
// 右缘 ▾ 弹出最近目录面板——首行全局默认下载目录(标"默认"灰显,选中即覆盖
// 输入),其后最近使用目录(× 删除)。新建任务/新建BT/RSS 新建订阅三处目录字段
// 统一用它;右侧再各留一个"浏览…"钮(=Vue input-row 的 DirField + 浏览)
type dirField struct {
	widget.BaseWidget
	pal       palette
	app       *App
	entry     *widget.Entry
	toggle    *iconButton
	pop       *dirRecentPop
	minH      float32 // 单行输入框规范高(=singleLineH),与原生输入框/组合框等齐
	OnChanged func(string) // 输入/覆盖时回调(=v-model 同步给对话框)
}

// NewDirField initial 为空则仅显示占位(默认目录由面板首行代表);否则预填该路径
func NewDirField(app *App, placeholder, initial string) *dirField {
	pal := app.store.appPal()
	d := &dirField{pal: pal, app: app}
	d.ExtendBaseWidget(d)
	d.entry = widget.NewEntry()
	d.entry.SetPlaceHolder(placeholder)
	singleLine(d.entry) // 横向裁剪、无纵向滚动条(=input)
	d.minH = singleLineH()
	d.entry.SetText(initial)
	d.entry.OnChanged = func(s string) {
		if d.OnChanged != nil {
			d.OnChanged(s)
		}
	}
	d.toggle = newIconButtonSZ(pal, "keyboard_arrow_down", 26, 14, d.showPop)
	return d
}

func (d *dirField) Text() string     { return d.entry.Text }
func (d *dirField) SetText(s string) { d.entry.SetText(s) }

func (d *dirField) CreateRenderer() fyne.WidgetRenderer {
	// 输入框吃掉剩余宽度,▾ 固定右缘(=Vue .dir-drop 贴输入框右内缘;native 无
	// 内嵌能力,放输入框边框右侧,视觉等价)
	inner := container.NewBorder(nil, nil, nil,
		container.New(layout.NewCustomPaddedLayout(0, 0, 2, 6), d.toggle), d.entry)
	return &simpleRenderer{obj: inner, min: fyne.NewSize(160, d.minH)}
}

// showPop 弹最近目录面板(=DirField popover):宽随输入框(下限 220),高
// min(内容,320) 超高滚动;贴输入框下缘,放不下翻到上方。无默认目录且无最近
// 目录时不弹(没东西可选);开着的再点 ▾ 收起(toggle 语义)
func (d *dirField) showPop() {
	if d.pop != nil && !d.pop.hid {
		d.pop.Hide()
		return
	}
	drv := fyne.CurrentApp().Driver()
	cnv := drv.CanvasForObject(d)
	if cnv == nil {
		return
	}
	rows := d.recentRows()
	if len(rows) == 0 {
		return
	}
	pop := newDirRecentPop(cnv, d.pal, rows,
		func(path string) { d.SetText(path) },
		func(path string) {
			d.app.store.DeleteRecentDir(path)
			// 删后重开面板拿最新列表:先摘旧面板,showPop 见 d.pop 为 nil 即重建
			d.pop = nil
			d.showPop()
		})
	pop.onHide = func() { d.pop = nil }
	d.pop = pop
	w := d.Size().Width
	if w < 220 {
		w = 220
	}
	h := pop.contentMin().Height + 8
	if h > 320 {
		h = 320
	}
	pop.size = fyne.NewSize(w, h)
	abs := drv.AbsolutePositionForObject(d)
	x := abs.X
	if x+w > cnv.Size().Width-8 {
		x = cnv.Size().Width - 8 - w
	}
	y := abs.Y + d.Size().Height + 2
	if y+h > cnv.Size().Height-8 {
		y = abs.Y - 2 - h
	}
	pop.ShowAtPosition(fyne.NewPos(x, y))
}

// recentRows 首行全局默认(标"默认"),其后最近目录(去重、最新靠前,=Vue 规则)
func (d *dirField) recentRows() []dirRecentRowData {
	var rows []dirRecentRowData
	cfg := d.app.store.Cfg()
	if cfg == nil {
		return rows
	}
	if cfg.DownloadDir != "" {
		rows = append(rows, dirRecentRowData{path: cfg.DownloadDir, isDef: true})
	}
	seen := map[string]bool{cfg.DownloadDir: true}
	for _, r := range cfg.RecentDirs {
		if seen[r] {
			continue
		}
		seen[r] = true
		rows = append(rows, dirRecentRowData{path: r})
	}
	return rows
}

// dirRecentPop 最近目录面板(=DirField popover 外壳):overlay 根被强制全画布,
// 面板按 pos/size 由渲染器摆放(=zMenuPop 同类规则),行 = dirRecentRow
type dirRecentPop struct {
	widget.BaseWidget
	cnv     fyne.Canvas
	panel   fyne.CanvasObject
	pos     fyne.Position
	size    fyne.Size
	onHide  func()
	hid     bool
	rowsBox *fyne.Container
}

func newDirRecentPop(cnv fyne.Canvas, pal palette, rows []dirRecentRowData, onPick func(string), onDel func(string)) *dirRecentPop {
	p := &dirRecentPop{cnv: cnv}
	p.ExtendBaseWidget(p)
	list := container.New(&vgapLayout{gap: 2})
	for _, rd := range rows {
		rd := rd
		list.Add(newDirRecentRow(pal, rd.path, rd.isDef,
			func() {
				p.hide()
				if onPick != nil {
					onPick(rd.path)
				}
			},
			func() {
				if onDel != nil {
					onDel(rd.path)
				}
			}))
	}
	p.rowsBox = list
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 9
	bg.StrokeWidth = 1
	bg.StrokeColor = pal.borderStrong
	p.panel = container.NewStack(bg,
		container.New(layout.NewCustomPaddedLayout(4, 4, 4, 4),
			container.NewVScroll(list)))
	return p
}

func (p *dirRecentPop) contentMin() fyne.Size { return p.rowsBox.MinSize() }

// Tapped 根捕获层:点面板外=收起(点外即关);行/× 更深处命中不冒到这
func (p *dirRecentPop) Tapped(*fyne.PointEvent) { p.Hide() }

func (p *dirRecentPop) CreateRenderer() fyne.WidgetRenderer {
	return &dirRecentPopRenderer{p: p}
}

// dirRecentPopRenderer 面板不进中间容器:overlay 根被强制全画布,任何包裹容器
// 的重排都会把面板拉回满幅,渲染器按 p.pos/p.size 直接摆放(=zMenuPopRenderer)
type dirRecentPopRenderer struct{ p *dirRecentPop }

func (r *dirRecentPopRenderer) Layout(fyne.Size) {
	r.p.panel.Move(r.p.pos)
	r.p.panel.Resize(r.p.size)
}
func (r *dirRecentPopRenderer) MinSize() fyne.Size           { return fyne.NewSize(1, 1) }
func (r *dirRecentPopRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.p.panel} }
func (r *dirRecentPopRenderer) Destroy()                     {}
func (r *dirRecentPopRenderer) Refresh()                     { r.Layout(fyne.Size{}) }

// ShowAtPosition 挂 overlay 栈(会被强制全画布)后把面板摆到目标位置定尺寸
func (p *dirRecentPop) ShowAtPosition(pos fyne.Position) {
	p.pos = pos
	if p.cnv == nil {
		return
	}
	p.cnv.Overlays().Add(p)
	p.BaseWidget.Show()
	p.panel.Move(pos)
	p.panel.Resize(p.size)
	trackPop(p) // 入 ESC 栈(hideTopModal 的 Visible 兜底会清陈旧项)
	p.Refresh()
}

func (p *dirRecentPop) Hide() {
	if p.hid {
		return
	}
	p.hid = true
	if p.cnv != nil {
		p.cnv.Overlays().Remove(p)
	}
	p.BaseWidget.Hide()
	if p.onHide != nil {
		p.onHide()
	}
}

func (p *dirRecentPop) hide() {
	p.hid = true
	if p.cnv != nil {
		p.cnv.Overlays().Remove(p)
	}
	p.BaseWidget.Hide()
	if p.onHide != nil {
		p.onHide()
	}
}

type dirRecentRowData struct {
	path  string
	isDef bool // 全局默认目录
}

// dirRecentRow 面板单行:默认行左侧"默认"标签、路径灰显;最近行右侧"×"删除钮。
// 整行可点(=选中覆盖输入),× 更深处命中只触删除不触选中
type dirRecentRow struct {
	widget.BaseWidget
	pal    palette
	isDef  bool
	path   string
	onPick func()
	onDel  func()
	hl     *canvas.Rectangle
	tag    *colorLabel // "默认"(仅默认行)
	lbl    *colorLabel // 路径(SetMaxWidth 截断)
	del    *iconButton // ×(仅最近行)
}

func newDirRecentRow(pal palette, path string, isDef bool, onPick, onDel func()) *dirRecentRow {
	r := &dirRecentRow{pal: pal, isDef: isDef, path: path, onPick: onPick, onDel: onDel}
	r.ExtendBaseWidget(r)
	// hl 常驻可见、默认透明填充:Show/Hide 切换会让隐藏态对象重绘区域失效(=
	// zMenuRow 同类坑),只切 FillColor 保持重绘区域有效
	r.hl = canvas.NewRectangle(color.NRGBA{A: 0})
	r.hl.CornerRadius = 6
	c := pal.text
	if isDef {
		c = pal.textDim
	}
	r.lbl = newColorLabelS(path, c, false, 13).SetMaxWidth()
	if isDef {
		r.tag = newColorLabelS("默认", pal.accent, true, 12)
	} else {
		// 移除最近目录=删除记录,敏感操作:红悬停图标钮(=.dir-item-del:hover 变红)
		r.del = newDangerIconButtonSZ(pal, "close", 20, 11, onDel)
		r.del.SetTip("移除该目录")
	}
	return r
}

func (r *dirRecentRow) CreateRenderer() fyne.WidgetRenderer {
	var left, right fyne.CanvasObject
	if r.isDef {
		left = container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), r.tag)
	} else {
		right = r.del
	}
	inner := container.New(layout.NewCustomPaddedLayout(7, 7, 10, 10),
		container.NewBorder(nil, nil, left, right, r.lbl))
	return &simpleRenderer{obj: container.NewStack(r.hl, inner), min: fyne.NewSize(60, 33)}
}

func (r *dirRecentRow) Tapped(*fyne.PointEvent) {
	if r.onPick != nil {
		r.onPick()
	}
}

// hover=accent 12% 底(=zMenuRow hover),只改填充色不 Show/Hide
func (r *dirRecentRow) MouseIn(*desktop.MouseEvent) {
	r.hl.FillColor = withAlpha(r.pal.accent, 0.12)
	r.hl.Refresh()
}

func (r *dirRecentRow) MouseOut() {
	r.hl.FillColor = color.NRGBA{A: 0}
	r.hl.Refresh()
}

func (r *dirRecentRow) MouseMoved(*desktop.MouseEvent) {}
