package nativeui

// 详情页右键菜单与多选表格(=CtxMenu.vue + Drawer.vue 连接/Tracker 行操作)。
//   - menuLabel/menuSep/showMenu:菜单弹层逐项对齐 .ctx-menu(panel-solid 底 +
//     border-strong 边 + radius 10;item 12.5px、radius 7、accent 14% 悬停;1px 分隔线);
//     非模态 PopUp 的 OverlayContainer 自带"点菜单外收起"(=Vue onDocPointerDown)
//   - menuCell/menuTable:行级交互基座。fyne 命中测试取"最深实现鼠标接口的对象",
//     widget.Table 自身实现 Mouseable+Tappable 会吞掉一切事件(表现为单格选中、
//     表头拖宽),外壳包装层永远拿不到;单元格模板比 Table 更深,由它接管
//     左键/右键/hover,Table 退化为纯绘制+滚动
//   - 行选中只记录行序号(单行;Vue 表格无多选);排序只重排展示行,
//     所选行动作仍以行当前承载的地址为准

import (
	"image/color"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

/* ---------------- 菜单项 ---------------- */

// menuRowItem 菜单项接口:sep 为真渲染分隔线;disabled 为真不响应点按
type menuRowItem interface {
	fyne.CanvasObject
	sep() bool
	disabled() bool
	doTap()
}

// menuSep 分隔线(=-)
type menuSep struct {
	widget.BaseWidget
	ln *canvas.Rectangle
}

func newMenuSep() *menuSep {
	m := &menuSep{}
	m.ExtendBaseWidget(m)
	m.ln = canvas.NewRectangle(color.NRGBA{A: 0})
	m.ln.SetMinSize(fyne.NewSize(0, 1))
	return m
}

func (m *menuSep) sep() bool      { return true }
func (m *menuSep) disabled() bool { return true }
func (m *menuSep) doTap()         {}

func (m *menuSep) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: m.ln, min: fyne.NewSize(0, 9)}
}

// menuLabel 菜单项(=ctx-item:12.5px 文本、accent 14% 悬停、禁用淡显)
type menuLabel struct {
	widget.BaseWidget
	app   *App
	txt   string
	dis   bool
	onTap func()
	hov   *canvas.Rectangle
	lbl   *canvas.Text
}

func newMenuLabel(app *App, txt string, dis bool, onTap func()) *menuLabel {
	m := &menuLabel{app: app, txt: txt, dis: dis, onTap: onTap}
	m.ExtendBaseWidget(m)
	m.hov = canvas.NewRectangle(color.NRGBA{A: 0})
	m.hov.CornerRadius = 7
	m.lbl = canvas.NewText(txt, app.store.appPal().text)
	m.lbl.TextSize = 12.5
	if dis {
		m.lbl.Color = app.store.appPal().textFaint
	}
	return m
}

func (m *menuLabel) sep() bool      { return false }
func (m *menuLabel) disabled() bool { return m.dis }
func (m *menuLabel) doTap() {
	if m.dis || m.onTap == nil {
		return
	}
	hideMenu() // 先收起再执行(=CtxMenu pick)
	m.onTap()
}

func (m *menuLabel) CreateRenderer() fyne.WidgetRenderer {
	pal := m.app.store.appPal()
	if m.dis {
		m.lbl.Color = pal.textFaint
	} else {
		m.lbl.Color = pal.text
	}
	// =.ctx-item padding:6px 12px(CustomPaddedLayout 参数序是 top,bottom,left,right)
	content := container.New(layout.NewCustomPaddedLayout(6, 6, 12, 12), m.lbl)
	min := fyne.NewSize(m.lbl.MinSize().Width+24, m.lbl.MinSize().Height+12)
	stack := container.NewStack(m.hov, content)
	return &simpleRenderer{obj: stack, min: min}
}

// Tapped 点按执行
func (m *menuLabel) Tapped(*fyne.PointEvent) { m.doTap() }

// MouseIn/MouseOut 悬停高亮(=ctx-item:hover)
func (m *menuLabel) MouseIn(*desktop.MouseEvent) {
	if m.dis {
		return
	}
	pal := m.app.store.appPal()
	m.hov.FillColor = withAlpha(pal.accent, 0.14)
	m.hov.Refresh()
}

func (m *menuLabel) MouseOut() {
	m.hov.FillColor = color.NRGBA{A: 0}
	m.hov.Refresh()
}

func (m *menuLabel) MouseMoved(*desktop.MouseEvent) {}
func (m *menuLabel) MouseDown(*desktop.MouseEvent)  {}
func (m *menuLabel) MouseUp(*desktop.MouseEvent)    {}

// menuRowItemSatisfies 接口满足检查(编译期)
var _ menuRowItem = (*menuSep)(nil)
var _ menuRowItem = (*menuLabel)(nil)

// sepItem 便捷构造分隔线项(与 menuLabel 同列表使用)
func sepItem() *menuSep { return newMenuSep() }

/* ---------------- 弹层 ---------------- */

// menuPop 当前显示的菜单弹窗(仅 UI 线程读写)
var menuPop *widget.PopUp

// showMenu 显示右键菜单于 (x,y)(绝对坐标;视口钳位=CtxMenu watch 语义)
func (app *App) showMenu(x, y float32, items []menuRowItem) {
	hideMenu()
	pal := app.store.appPal()
	rows := make([]fyne.CanvasObject, 0, len(items))
	for _, it := range items {
		if it.sep() {
			s := canvas.NewRectangle(pal.border)
			s.SetMinSize(fyne.NewSize(0, 1))
			// =.ctx-sep:1px 线 + margin 4px 6px
			rows = append(rows, container.New(layout.NewCustomPaddedLayout(4, 4, 6, 6), s))
			continue
		}
		rows = append(rows, it)
	}
	pad := container.New(layout.NewCustomPaddedLayout(5, 5, 5, 5),
		container.New(&vgapLayout{gap: 0}, rows...))
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 10
	stroke := canvas.NewRectangle(color.NRGBA{A: 0})
	stroke.CornerRadius = 10
	stroke.StrokeWidth = 1
	stroke.StrokeColor = pal.borderStrong
	card := container.NewStack(bg, stroke, pad)
	size := card.MinSize()
	if size.Width < 170 { // =.ctx-menu min-width:170px
		size.Width = 170
	}
	cs := app.win.Canvas().Size()
	if x+size.Width+8 > cs.Width {
		x = cs.Width - size.Width - 8
	}
	if y+size.Height+8 > cs.Height {
		y = cs.Height - size.Height - 8
	}
	if x < 4 {
		x = 4
	}
	if y < 4 {
		y = 4
	}
	pop := widget.NewPopUp(card, app.win.Canvas())
	pop.Resize(size)
	pop.Move(fyne.NewPos(x, y))
	pop.Show()
	menuPop = pop
}

// hideMenu 收起菜单(幂等)
func hideMenu() {
	if menuPop == nil {
		return
	}
	if menuPop.Visible() {
		menuPop.Hide()
	}
	menuPop = nil
}

// menuOpen 菜单是否正显示。PopUp 被外部点掉时走 OverlayContainer 的 dismiss
// 直接 Hide,不经过 hideMenu,所以必须看 Visible 而不是只看指针
func menuOpen() bool {
	return menuPop != nil && menuPop.Visible()
}

/* ---------------- 多选表格基座 ---------------- */

// menuCell 单元格事件捕获层(包裹 CreateCell 模板)。
// 命中测试里它比 widget.Table 更深,鼠标事件(左键行选/右键菜单/hover)
// 都分发到这里,Table 的单格选中与表头拖宽不再触发(=Vue 无单元格选中)。
type menuCell struct {
	widget.BaseWidget
	mt      *menuTable
	id      widget.TableCellID
	content fyne.CanvasObject
	bg      *canvas.Rectangle // 行底色(表头/悬停/选中)
	border  *canvas.Rectangle // 行底 1px 分隔线(=td border-bottom)
}

var _ desktop.Mouseable = (*menuCell)(nil)
var _ desktop.Hoverable = (*menuCell)(nil)
var _ fyne.SecondaryTappable = (*menuCell)(nil)

func (c *menuCell) CreateRenderer() fyne.WidgetRenderer {
	return &menuCellRenderer{c: c}
}

// MouseDown 左键=行选中
func (c *menuCell) MouseDown(e *desktop.MouseEvent) {
	if e.Button == desktop.MouseButtonPrimary {
		c.mt.cellTap(c.id)
	}
}

func (c *menuCell) MouseUp(*desktop.MouseEvent)    {}
func (c *menuCell) MouseMoved(*desktop.MouseEvent) {}

// TappedSecondary 右键=行菜单/空白菜单(=onPeerCtx/onTrCtx)
func (c *menuCell) TappedSecondary(e *fyne.PointEvent) {
	c.mt.cellCtx(c.id, e.AbsolutePosition)
}

func (c *menuCell) MouseIn(*desktop.MouseEvent) { c.mt.cellHover(c) }
func (c *menuCell) MouseOut()                   { c.mt.cellUnhover(c) }

// style 行级视觉(=bt-table th/td):表头 bg-soft 底、悬停行 accent 7%、
// 选中行 accent 12%、行底 1px 线。补白格(视口空白占位)只藏内容,行底色与
// 行线照常画——行视觉跨满视口宽(=Vue 表格 width:100% 行线拉通,否则右侧
// 空白区看起来不属于表)。底色同时盖掉 Table 内部的单格选中/悬停标记
func (c *menuCell) style() {
	m := c.mt
	pad := m.isPad(c.id)
	if pad {
		c.content.Hide()
	} else {
		c.content.Show()
	}
	pal := m.app.store.appPal()
	var bg color.Color = color.NRGBA{A: 0}
	switch {
	case c.id.Row == 0:
		bg = pal.bgSoft // =th background:var(--bg-soft)
	case !pad && m.rowSel(c.id.Row):
		bg = withAlpha(pal.accent, 0.12) // 选中行整行高亮
	case !pad && m.hoverRow == c.id.Row:
		bg = withAlpha(pal.accent, 0.07) // =tbody tr:hover accent 7%
	}
	// 颜色变更才重绘:秒级刷新逐格无条件 Refresh 是整表卡顿源
	if !sameColor(c.bg.FillColor, bg) {
		c.bg.FillColor = bg
		c.bg.Refresh()
	}
	if !c.border.Visible() {
		c.border.Show()
	}
	bc := pal.border // =td border-bottom
	if c.id.Row == 0 {
		bc = pal.borderStrong // =th border-bottom
	}
	if !sameColor(c.border.FillColor, bc) {
		c.border.FillColor = bc
		c.border.Refresh()
	}
}

// menuCellRenderer bg + 行底线 + 内容三层;行底线贴单元格底边
type menuCellRenderer struct {
	c *menuCell
}

func (r *menuCellRenderer) Layout(size fyne.Size) {
	// 底色/行线双向外扩半格间隙:fyne 表格格间有 SizeNamePadding 间隙,透底会
	// 形成竖缝(=用户报的"竖线";表头/选中/悬停行尤其明显),外扩后行底贯通、
	// td 行线连续(=Vue);间隙 4px,相邻格各扩 2px 正好接壤互不遮盖
	g := r.c.mt.Theme().Size(theme.SizeNamePadding) / 2
	r.c.bg.Move(fyne.NewPos(-g, -g))
	r.c.bg.Resize(fyne.NewSize(size.Width+2*g, size.Height+2*g))
	r.c.content.Resize(size)
	r.c.border.Move(fyne.NewPos(-g, size.Height+g-1))
	r.c.border.Resize(fyne.NewSize(size.Width+2*g, 1))
}

func (r *menuCellRenderer) MinSize() fyne.Size { return r.c.content.MinSize() }
func (r *menuCellRenderer) Refresh()           { r.c.content.Refresh() }
func (r *menuCellRenderer) Destroy()           {}

func (r *menuCellRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.c.bg, r.c.border, r.c.content}
}

// menuTable widget.Table 的交互与样式层。Length 包一层补白行/列:视口空白区
// (数据下方/右侧)本没有单元格,事件会落到 Table 手里无响应;按视口尺寸补
// 空白占位格,空白区也能右键/左键(=Vue 空白区 onPeerCtx 手动添加连接)。
type menuTable struct {
	widget.BaseWidget
	app    *App
	inner  *widget.Table
	rawLen func() (int, int)                           // 未补白的 Length(行数含表头行)
	rawUpd func(widget.TableCellID, fyne.CanvasObject) // 原始 UpdateCell(轻刷新用)
	rowSel func(row int) bool

	OnRowTap   func(id widget.TableCellID)                    // 数据行左键
	OnHeadTap  func(col int)                                  // 表头左键(排序)
	OnBlankTap func()                                         // 空白处左键(清选)
	OnRowCtx   func(id widget.TableCellID, abs fyne.Position) // 数据行右键
	OnBlankCtx func(abs fyne.Position)                        // 表头/空白处右键(nil=不弹)

	hoverRow  int
	hoverCell *menuCell
	rowH      float32 // 行高(模板最小高;所有行等高,从未 SetRowHeight)
	colW      []float32
	padW      float32     // 补白列宽(=视口剩余宽度)
	cells     []*menuCell // 在册单元格(池化复用,上限≈可见格数;定向重绘用)
}

func newMenuTable(app *App, inner *widget.Table, rowSel func(int) bool) *menuTable {
	m := &menuTable{app: app, inner: inner, rawLen: inner.Length, rowSel: rowSel, hoverRow: -1}
	m.ExtendBaseWidget(m)

	rawCreate, rawUpdate := inner.CreateCell, inner.UpdateCell
	m.rawUpd = rawUpdate
	inner.CreateCell = func() fyne.CanvasObject {
		c := &menuCell{mt: m, id: widget.TableCellID{Row: -1, Col: -1}, content: rawCreate()}
		c.ExtendBaseWidget(c)
		c.bg = canvas.NewRectangle(color.NRGBA{A: 0})
		c.border = canvas.NewRectangle(color.NRGBA{A: 0})
		m.cells = append(m.cells, c) // 在册:悬停/选中只重画受影响行
		return c
	}
	inner.UpdateCell = func(id widget.TableCellID, o fyne.CanvasObject) {
		c := o.(*menuCell)
		c.id = id
		c.style()
		if !m.isPad(id) {
			rawUpdate(id, c.content)
		}
	}
	m.rowH = inner.CreateCell().MinSize().Height
	inner.Length = func() (int, int) {
		rows, cols := m.rawLen()
		// 仅在有数据行(rows>1)时才补白行填满视口;空表(只剩表头行 rows==1)
		// 若也补白,内容会被撑到超过视口高,空列表照样恒有竖向滚动条(=用户报的
		// "Tracker 空列表仍有滚动条",peers 空表同理)。空表就只显示一行表头
		if rows > 1 && m.rowH > 0 && m.Size().Height > 0 {
			pitch := m.rowH + m.Theme().Size(theme.SizeNamePadding)
			need := int((m.Size().Height + pitch - 1) / pitch)
			if rows < need {
				rows = need // 补白行:填满视口高
			}
		}
		if m.padW > 0 {
			cols++ // 补白列:填满视口宽
		}
		return rows, cols
	}
	return m
}

func (m *menuTable) CreateRenderer() fyne.WidgetRenderer {
	return &menuTableRenderer{m: m}
}

// Resize 视口变化时同步补白列宽(=Vue .bt-table width:100% 余量;余量不摊进
// 各列而由占位列吸收,好让空白区始终有格子接事件)
func (m *menuTable) Resize(size fyne.Size) {
	if size == m.Size() {
		return
	}
	m.BaseWidget.Resize(size)
	if len(m.colW) == 0 {
		return
	}
	sep := m.Theme().Size(theme.SizeNamePadding)
	total := sep * float32(len(m.colW)-1)
	for _, w := range m.colW {
		total += w
	}
	// 补白列自己前面还有一个分隔符宽,少扣恰好一个 sep → 内容总宽永远
	// 比视口宽 4px → 空间充足也出现横向滚动条
	rem := m.Size().Width - total - sep
	if rem < 0 {
		rem = 0
	}
	m.padW = rem
	if rem > 0 {
		m.inner.SetColumnWidth(len(m.colW), rem)
	}
	m.inner.Refresh()
}

// isPad 补白行/列(视口空白区占位格)
func (m *menuTable) isPad(id widget.TableCellID) bool {
	rows, cols := m.rawLen()
	return id.Row >= rows || id.Col >= cols
}

// cellTap 左键分发:表头=排序;空白=清选;数据行=行选中
func (m *menuTable) cellTap(id widget.TableCellID) {
	switch {
	case id.Row == 0:
		if m.OnHeadTap != nil {
			m.OnHeadTap(id.Col)
		}
	case m.isPad(id):
		if m.OnBlankTap != nil {
			m.OnBlankTap()
		}
	default:
		if m.OnRowTap != nil {
			m.OnRowTap(id)
		}
	}
}

// cellCtx 右键分发:数据行=行菜单(未选中先选中,=浏览器语义);表头/空白=OnBlankCtx
func (m *menuTable) cellCtx(id widget.TableCellID, abs fyne.Position) {
	if id.Row == 0 || m.isPad(id) {
		if m.OnBlankCtx != nil {
			m.OnBlankCtx(abs)
		}
		return
	}
	if !m.rowSel(id.Row) && m.OnRowTap != nil {
		m.OnRowTap(id)
	}
	if m.OnRowCtx != nil {
		m.OnRowCtx(id, abs)
	}
}

// restyleRows 只重画指定行的行级视觉(悬停/选中变化)。全表 Refresh 会整表
// SetObjects 重排+移动分隔线,鼠标划过表格逐行触发,是"Tracker 页卡"的
// 直接来源;style() 自带同色守卫,重复调用零重绘
func (m *menuTable) restyleRows(rows ...int) {
	want := make(map[int]bool, len(rows))
	for _, r := range rows {
		want[r] = true
	}
	for _, c := range m.cells {
		if want[c.id.Row] {
			c.style()
		}
	}
}

// refreshData 数据轻刷新:行数不变时对在册格子重跑原始 UpdateCell(内容
// setter 全带变更守卫,未变的格子零重绘),不走 Table 全量 Refresh——后者
// 每次重建可见映射+SetObjects 整表重排+挪分隔线,倒计时类每秒小变化
// 也会触发整表级联
func (m *menuTable) refreshData() {
	if m.rawUpd == nil {
		m.inner.Refresh()
		return
	}
	for _, c := range m.cells {
		if c.id.Row < 0 {
			continue
		}
		if m.isPad(c.id) {
			c.style()
			continue
		}
		m.rawUpd(c.id, c.content)
		c.style()
	}
}

// cellHover/cellUnhover 行悬停(=tbody tr:hover;同格/同行不重绘)
func (m *menuTable) cellHover(c *menuCell) {
	if m.hoverCell == c {
		return
	}
	m.hoverCell = c
	if c.id.Row == m.hoverRow {
		return
	}
	old := m.hoverRow
	m.hoverRow = c.id.Row
	m.restyleRows(old, m.hoverRow)
}

func (m *menuTable) cellUnhover(c *menuCell) {
	if m.hoverCell != c {
		return
	}
	m.hoverCell = nil
	old := m.hoverRow
	m.hoverRow = -1
	m.restyleRows(old)
}

// menuTableRenderer 转发内层 Table 的渲染(绘制/滚动/单元格复用全不变);
// Refresh 必须转发,否则数据刷新(applyPeers/applyTrackers)不生效
type menuTableRenderer struct {
	m *menuTable
}

func (r *menuTableRenderer) Layout(size fyne.Size) {
	r.m.inner.Move(fyne.NewPos(0, 0))
	r.m.inner.Resize(size)
}

func (r *menuTableRenderer) MinSize() fyne.Size           { return r.m.inner.MinSize() }
func (r *menuTableRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.m.inner} }
func (r *menuTableRenderer) Destroy()                     {}
func (r *menuTableRenderer) Refresh()                     { r.m.inner.Refresh() }

/* ---------------- 行选中集合 ---------------- */

// rowSelSet 当前选中行(单行选中,=浏览器/资源管理器语义;Vue 表格无多选,
// 行动作均为单行)。行序号从 1 起,0=未选
type rowSelSet struct {
	mu  sync.Mutex
	sel int
}

func newRowSelSet() *rowSelSet {
	return &rowSelSet{sel: 0}
}

func (s *rowSelSet) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sel = 0
}

func (s *rowSelSet) has(row int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel == row
}

// set 选中单行(点哪行选哪行)
func (s *rowSelSet) set(row int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sel = row
}
