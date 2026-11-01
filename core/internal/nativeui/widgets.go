package nativeui

// 壳层助手:engine 类型别名(TaskCard.vue 移植代码统一短名)、顶栏速度曲线
// (SpeedChart.vue 移植)、应用图标、新建任务 CreateReq 构造。

import (
	"image"
	"image/color"
	"math"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/assets"
	"zdownloader/internal/engine"
)

// engine 类型别名
type (
	taskView      = engine.TaskView
	engineStatus  = engine.Status
	engineSegment = engine.Segment
)

const (
	engineStatusRunning   = engine.StatusRunning
	engineStatusWaiting   = engine.StatusWaiting
	engineStatusPaused    = engine.StatusPaused
	engineStatusCompleted = engine.StatusCompleted
	engineStatusError     = engine.StatusError
	engineStatusCanceled  = engine.StatusCanceled
	engineStatusSeeding   = engine.StatusSeeding
	engineStatusChoosing  = engine.StatusChoosing
)

// appIconResource 应用图标:internal/assets 已嵌入 app-icon.png(与 genicons
// 生成的 app.ico 同源于仓库根 icon.png)。此前恒返 nil(旧注:注入不可行——
// 当时 AppIcon 尚未嵌入),SetIcon(nil) 令标题栏/任务栏落回系统默认图标。
func appIconResource() fyne.Resource {
	return fyne.NewStaticResource("app-icon.png", assets.AppIcon)
}

// speedChart 顶栏总速度曲线(SpeedChart.vue drawChart 移植:95 样本,
// 上传绿在下、下载蓝在上)。Fyne 无折线对象,用 canvas.Raster 逐像素画
// (180x44 极小,每快照一次重绘无性能压力)。
type speedChart struct {
	widget.BaseWidget
	store   *Store
	rast    *canvas.Raster
	lastSig [2]float64 // 上次绘制的(下行峰值+总量, 上行峰值+总量)签名
}

func newSpeedChart(s *Store) *speedChart {
	sc := &speedChart{store: s}
	sc.ExtendBaseWidget(sc)
	sc.rast = canvas.NewRaster(sc.render)
	sc.rast.SetMinSize(fyne.NewSize(190, 40))
	return sc
}

func (sc *speedChart) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: sc.rast, min: fyne.NewSize(190, 40)}
}

// simpleRenderer 单对象渲染器(图表类小部件共用)
type simpleRenderer struct {
	obj fyne.CanvasObject
	min fyne.Size
}

func (r *simpleRenderer) Layout(size fyne.Size)        { r.obj.Resize(size) }
func (r *simpleRenderer) MinSize() fyne.Size           { return r.min }
func (r *simpleRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.obj} }
func (r *simpleRenderer) Destroy()                     {}
func (r *simpleRenderer) Refresh()                     {}

// refresh 重绘(数据签名未变则跳过:空闲时每秒的纹理重传是常驻内存增长源)
func (sc *speedChart) refresh() {
	down, up := sc.store.SpeedHist()
	var sig [2]float64
	for _, v := range down {
		sig[0] += v
		if v > sig[0] {
			sig[0] = v
		}
	}
	for _, v := range up {
		sig[1] += v
		if v > sig[1] {
			sig[1] = v
		}
	}
	if sig == sc.lastSig {
		return
	}
	sc.lastSig = sig
	sc.rast.Refresh()
}

// render 逐像素绘制两条曲线(上传绿在下层、下载蓝在上层,=SpeedChart.vue line())
func (sc *speedChart) render(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	down, up := sc.store.SpeedHist()
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
			i := x * (n - 1) / (w - 1)
			y := h - 2 - int(hist[i]/max*float64(h-6))
			if y < 0 {
				y = 0
			}
			if y > h-1 {
				y = h - 1
			}
			// 竖直补线抗断点(样本间距大于像素时)
			lo, hi := y, y
			if prevY >= 0 {
				if prevY < y {
					lo, hi = prevY, y
				} else {
					lo, hi = y, prevY
				}
			}
			for yy := lo; yy <= hi; yy++ {
				img.SetNRGBA(x, yy, color.NRGBA{R: r, G: g, B: b, A: 255})
			}
			prevY = y
		}
	}
	plot(up, 0x34, 0xd3, 0x99)   // 上传(下)
	plot(down, 0x4f, 0x8c, 0xff) // 下载(上)
	return img
}

// dashedLine 1px 虚线分隔(=.pane-sep/.sec-title 的 border-top:1px dashed;
// fyne 无虚线描边,光栅按 3px 实/3px 虚画在底边一行)
type dashedLine struct {
	widget.BaseWidget
	rast *canvas.Raster
	clr  color.Color
}

func newDashedLine(c color.Color) *dashedLine {
	d := &dashedLine{clr: c}
	d.ExtendBaseWidget(d)
	d.rast = canvas.NewRaster(d.render)
	d.rast.SetMinSize(fyne.NewSize(0, 1))
	return d
}

func (d *dashedLine) render(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	c := color.NRGBAModel.Convert(d.clr).(color.NRGBA)
	y := h - 1
	for x := 0; x < w; x += 6 {
		for dx := 0; dx < 3 && x+dx < w; dx++ {
			img.SetNRGBA(x+dx, y, c)
		}
	}
	return img
}

func (d *dashedLine) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: d.rast, min: fyne.NewSize(0, 1)}
}

// fixedMin 最小尺寸下限的单对象容器(进度条行高/侧栏宽度/弹层定宽等)。
// min 是下限而非定值:内容自身 min 更大时取内容(弹层 h=0 表示高度自适应)
type fixedMin struct {
	widget.BaseWidget
	min     fyne.Size
	content *fyne.Container
}

func newFixedMin(content *fyne.Container, min fyne.Size) *fixedMin {
	f := &fixedMin{min: min, content: content}
	f.ExtendBaseWidget(f)
	return f
}

// SetMin 运行时改尺寸下限(仅下限;内容更大仍按内容)。MinSize 每次现读
// f.min,故改完触发一次刷新即可让布局按新下限重排——宽表页签 780→1140 加宽用
func (f *fixedMin) SetMin(s fyne.Size) {
	f.min = s
	f.Refresh()
}

func (f *fixedMin) WrapObject() fyne.CanvasObject { return f.content }

type fixedMinRenderer struct{ f *fixedMin }

func (r *fixedMinRenderer) Layout(size fyne.Size) { r.f.content.Resize(size) }

func (r *fixedMinRenderer) MinSize() fyne.Size {
	m := r.f.content.MinSize()
	if r.f.min.Width > m.Width {
		m.Width = r.f.min.Width
	}
	if r.f.min.Height > m.Height {
		m.Height = r.f.min.Height
	}
	return m
}

func (r *fixedMinRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.f.content}
}
func (r *fixedMinRenderer) Destroy() {}
func (r *fixedMinRenderer) Refresh() {}

func (f *fixedMin) CreateRenderer() fyne.WidgetRenderer {
	return &fixedMinRenderer{f: f}
}

// Content 渲染器取内部容器
func (f *fixedMin) Content() *fyne.Container { return f.content }

// hgapLayout 水平排列 + 固定间距 + 垂直居中(Fyne HBox 间距吃主题 padding,
// 且不对齐竖直中心,CSS 的 gap 语义对不上)
type hgapLayout struct{ gap float32 }

func (g *hgapLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	x := float32(0)
	for _, o := range objs {
		if !o.Visible() {
			continue // 与 vgap 同理,隐藏不占位
		}
		m := o.MinSize()
		o.Resize(m)
		o.Move(fyne.NewPos(x, (s.Height-m.Height)/2))
		x += m.Width + g.gap
	}
}

func (g *hgapLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	w, h := float32(0), float32(0)
	for i, o := range objs {
		if !o.Visible() {
			continue
		}
		m := o.MinSize()
		w += m.Width
		if m.Height > h {
			h = m.Height
		}
		if i < len(objs)-1 {
			w += g.gap
		}
	}
	return fyne.NewSize(w, h)
}

// hGap 横排容器(gap px 间距,子项垂直居中)
func hGap(gap float32, objs ...fyne.CanvasObject) *fyne.Container {
	return container.New(&hgapLayout{gap: gap}, objs...)
}

// flexWeighter 弹性子项标记(hflexLayout 里按剩余宽度伸展)
type flexWeighter interface{ FlexWeight() float32 }

// flexWrap 把任意对象标记为弹性子项(weight>0;内容 min 为下限)
type flexWrap struct {
	widget.BaseWidget
	content fyne.CanvasObject
	weight  float32
}

func (f *flexWrap) FlexWeight() float32 { return f.weight }

func (f *flexWrap) WrapObject() fyne.CanvasObject { return f.content }

func (f *flexWrap) CreateRenderer() fyne.WidgetRenderer {
	return &flexWrapRenderer{f: f}
}

type flexWrapRenderer struct{ f *flexWrap }

func (r *flexWrapRenderer) Layout(size fyne.Size) { r.f.content.Resize(size) }

func (r *flexWrapRenderer) MinSize() fyne.Size { return r.f.content.MinSize() }
func (r *flexWrapRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.f.content}
}
func (r *flexWrapRenderer) Destroy() {}
func (r *flexWrapRenderer) Refresh() {}

// flex 包裹弹性子项(weight=1 均分剩余宽度,=flex:1)
func flex(o fyne.CanvasObject) fyne.CanvasObject {
	f := &flexWrap{content: o, weight: 1}
	f.ExtendBaseWidget(f)
	return f
}

// hflexLayout 横排 + 固定间距:固定子项取 min,弹性子项均分剩余宽度
// (=CSS flex 行;截断标签必须配弹性宽度,min-only 布局会把它压成 10px)
type hflexLayout struct{ gap float32 }

func (g *hflexLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	fixed := float32(0)
	wsum := float32(0)
	for _, o := range objs {
		if !o.Visible() {
			continue // 与 vgap 同理,隐藏不占位
		}
		if fw, ok := o.(flexWeighter); ok && fw.FlexWeight() > 0 {
			wsum += fw.FlexWeight() // 按权重比例分(=CSS flex:N),等权时退化为均分
			continue
		}
		fixed += o.MinSize().Width
	}
	gapN := 0
	for _, o := range objs {
		if o.Visible() {
			gapN++
		}
	}
	gapTotal := g.gap * float32(gapN-1)
	avail := s.Width - fixed - gapTotal
	x := float32(0)
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		m := o.MinSize()
		w := m.Width
		if fw, ok := o.(flexWeighter); ok && fw.FlexWeight() > 0 && wsum > 0 {
			w = avail * fw.FlexWeight() / wsum
			if w < 0 {
				w = 0
			}
		}
		o.Resize(fyne.NewSize(w, m.Height))
		o.Move(fyne.NewPos(x, (s.Height-m.Height)/2))
		x += w + g.gap
	}
}

func (g *hflexLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	w, h := float32(0), float32(0)
	for i, o := range objs {
		m := o.MinSize()
		w += m.Width
		if m.Height > h {
			h = m.Height
		}
		if i < len(objs)-1 {
			w += g.gap
		}
	}
	return fyne.NewSize(w, h)
}

// hFlex 横排弹性容器(gap px 间距;flex() 包裹的子项均分剩余宽度)
func hFlex(gap float32, objs ...fyne.CanvasObject) *fyne.Container {
	return container.New(&hflexLayout{gap: gap}, objs...)
}

// vgapLayout 纵向排列 + 固定间距;maxW>0 时子项宽度封顶(=.task-list
// max-width:1080px,卡片不满窗宽)
type vgapLayout struct {
	gap  float32
	maxW float32
}

func (g *vgapLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	y := float32(0)
	w := s.Width
	if g.maxW > 0 && w > g.maxW {
		w = g.maxW
	}
	for _, o := range objs {
		if !o.Visible() {
			continue // 隐藏子项不占位(标准布局均如此;条件显示的行才能收紧)
		}
		m := o.MinSize()
		h := m.Height
		o.Resize(fyne.NewSize(w, h))
		o.Move(fyne.NewPos(0, y))
		y += h + g.gap
	}
}

func (g *vgapLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	w, h := float32(0), float32(0)
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		m := o.MinSize()
		if m.Width > w {
			w = m.Width
		}
		h += m.Height
	}
	if len(objs) > 1 {
		h += g.gap * float32(len(objs)-1)
	}
	if g.maxW > 0 && w > g.maxW {
		w = g.maxW
	}
	return fyne.NewSize(w, h)
}

// colsGapLayout 等宽列 + 固定间距(=.seg-strip:flex:1+gap:3px)
type colsGapLayout struct{ gap float32 }

func (g *colsGapLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	n := len(objs)
	if n == 0 {
		return
	}
	cw := (s.Width - g.gap*float32(n-1)) / float32(n)
	x := float32(0)
	for _, o := range objs {
		o.Resize(fyne.NewSize(cw, s.Height))
		o.Move(fyne.NewPos(x, 0))
		x += cw + g.gap
	}
}

func (g *colsGapLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	w, h := float32(0), float32(0)
	for _, o := range objs {
		m := o.MinSize()
		w += m.Width
		if m.Height > h {
			h = m.Height
		}
	}
	if len(objs) > 1 {
		w += g.gap * float32(len(objs)-1)
	}
	return fyne.NewSize(w, h)
}

// roundedGrad 圆角线性渐变(Fyne 的 LinearGradient 无圆角,CSS 的
// radius+gradient 到处都是:文件图标块/进度条/主按钮底)。canvas.Raster
// 逐像素:横向插值色 + 圆角覆盖率 alpha 抗锯齿。尺寸小(≤38px 高),
// 每帧重绘无压力。
type roundedGrad struct {
	widget.BaseWidget
	c1, c2 color.Color
	radius float32
	rast   *canvas.Raster
}

func newRoundedGrad(c1, c2 color.Color, radius float32) *roundedGrad {
	g := &roundedGrad{c1: c1, c2: c2, radius: radius}
	g.ExtendBaseWidget(g)
	g.rast = canvas.NewRaster(g.render)
	return g
}

// SetColors 换渐变两端(触发重绘)。同值早退:卡片 Layout 每帧都会调,
// 无守卫会让 Raster 每帧重新生成图片并重传 GPU 纹理(内存/带宽双杀)。
func (g *roundedGrad) SetColors(c1, c2 color.Color) {
	if sameColor(g.c1, c1) && sameColor(g.c2, c2) {
		return
	}
	g.c1, g.c2 = c1, c2
	g.rast.Refresh()
}

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func (g *roundedGrad) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: g.rast, min: fyne.NewSize(2, 2)}
}

// modalCard 弹层卡片(=dialog 基类:暗遮罩 + panel-solid 圆角 18 + 1px 边框)。
// w/h>0 时卡体定尺寸(内容自行内部滚动)。自绘遮罩层替代 ModalPopUp:
// 遮罩点击即关(与 ESC 一致,fyne modal 的遮罩回调是 internal 挂不上)
func modalCard(win fyne.Window, pal palette, inner fyne.CanvasObject, w, h float32) *maskModal {
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 18
	stroke := canvas.NewRectangle(color.NRGBA{A: 0})
	stroke.CornerRadius = 18
	stroke.StrokeWidth = 1
	stroke.StrokeColor = pal.borderStrong
	var sizer *fixedMin
	body := inner
	if w > 0 || h > 0 {
		sizer = newFixedMin(container.NewStack(inner), fyne.NewSize(w, h))
		body = sizer
	}
	card := container.NewStack(bg, stroke, body)
	pop := newMaskModal(win, card)
	pop.cardSizer = sizer // 宽表页签改卡片宽走它,别去 Resize/Move 全屏层
	trackPop(pop)
	return pop
}

// maskModal 自绘模态层:遮罩=显式 rgba(5,8,14,.62)(Shadow 色名已全局
// 透明),卡片按 min 尺寸居中(=ModalPopUp 视觉)。直接挂 Canvas.Overlays
// 顶,驱动会把它撑满画布。
type maskModal struct {
	widget.BaseWidget
	card  fyne.CanvasObject
	cnv   fyne.Canvas
	dim   *canvas.Rectangle
	catch *maskCatcher
	// cardSizer 卡片尺寸下限握柄(=modalCard 传入的 fixedMin):宽表页签改卡片
	// 宽度用它,绝不能 Resize/Move 本层——本层是全屏遮罩层,动了遮罩就塌
	cardSizer *fixedMin
}

// maskCatcher 全画布透明点击层:接住卡片外的点击。卡片上的事件被卡内更深的
// 控件消费,或沿父链找不到 Tappable 而丢弃,不会误触这层
type maskCatcher struct {
	widget.BaseWidget
	onTap func()
}

func (c *maskCatcher) Tapped(*fyne.PointEvent) {
	if c.onTap != nil {
		c.onTap()
	}
}

// 实现 Hoverable:全画布 catch 层成为弹窗打开期间最顶层的悬停目标,把光标从
// 底层内容(任务标题等)上"截走"。否则命中遍历会穿透这层纯 Tappable、落到
// 底层标题上触发其悬停提示——即用户报的"隔着模态弹窗还能触发底层 tooltip"。
// 空实现即可,目的只是让分发在 catch 停下、不再落到底层内容
func (c *maskCatcher) MouseIn(*desktop.MouseEvent)    {}
func (c *maskCatcher) MouseMoved(*desktop.MouseEvent) {}
func (c *maskCatcher) MouseOut()                       {}

// 实现 Draggable:fyne 拖拽命中走独立 matcher、只认 Draggable 对象,且命中
// 遍历不因上层遮挡而停(FindObjectAtPositionMatching 的 findFunc 恒返回
// false、后命中者覆盖)。本 catch 层在模态层、晚于底层 body 被遍历,若它不
// 是 Draggable,拖拽命中就会穿过它落到底层可拖对象(RSS 订阅/文件夹行),
// =用户报的"隔着模态弹窗还能把底层订阅拖到别的文件夹"。按下后过拖拽阈值,
// 本层成为被拖对象并吞掉全部 Dragged/DragEnd,底层 dndRow 收不到事件;
// 未过阈值的普通点击仍走 Tapped(遮罩点击即关),两路互不影响。
func (c *maskCatcher) Dragged(*fyne.DragEvent) {}
func (c *maskCatcher) DragEnd()                {}

var _ desktop.Hoverable = (*maskCatcher)(nil)
var _ fyne.Draggable    = (*maskCatcher)(nil)

func (c *maskCatcher) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: canvas.NewRectangle(color.NRGBA{A: 0}), min: fyne.NewSize(1, 1)}
}

// tapAbsorber 卡片同尺寸的 no-op 点击层:fyne 的命中遍历不因上层遮挡而
// 停止——卡片空白处的点击会继续落到全画布 maskCatcher 上(=用户报的
// "点窗体内部空白也关闭")。这层盖在卡片正下方把事件吃掉
type tapAbsorber struct {
	widget.BaseWidget
}

func (a *tapAbsorber) Tapped(*fyne.PointEvent) {}

func (a *tapAbsorber) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: canvas.NewRectangle(color.NRGBA{A: 0}), min: fyne.NewSize(1, 1)}
}

func newMaskModal(win fyne.Window, card fyne.CanvasObject) *maskModal {
	m := &maskModal{card: card, cnv: win.Canvas()}
	m.ExtendBaseWidget(m)
	// 遮罩显式取色(=overlay 的 rgba(5,8,14,.6)):Shadow 色名已全局透明
	// (根除 ThemeOverride scope 字体缓存泄漏),遮罩不能再读它
	m.dim = canvas.NewRectangle(rgbaA(0x05080e, 0.62))
	m.catch = &maskCatcher{onTap: func() { hideTopModal() }}
	m.catch.ExtendBaseWidget(m.catch)
	return m
}

func (m *maskModal) CreateRenderer() fyne.WidgetRenderer {
	absorber := &tapAbsorber{}
	absorber.ExtendBaseWidget(absorber)
	// 吸收层与卡片同进退(Center 一起按卡片 min 定位),遮罩外才轮得到 catcher
	return &maskModalRenderer{m: m, obj: container.NewStack(m.dim, m.catch,
		container.NewCenter(container.NewStack(absorber, m.card)))}
}

// maskModalRenderer Refresh 重放布局:自适应高度弹窗(h=0)内容显隐后
// 卡片 min 变化,靠 Refresh 重新居中/定高
type maskModalRenderer struct {
	m   *maskModal
	obj fyne.CanvasObject
}

func (r *maskModalRenderer) Layout(s fyne.Size) { r.obj.Resize(s) }
func (r *maskModalRenderer) MinSize() fyne.Size { return r.m.card.MinSize() }
func (r *maskModalRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.obj}
}
func (r *maskModalRenderer) Destroy() {}
func (r *maskModalRenderer) Refresh() {
	r.obj.Refresh()
	r.Layout(r.m.Size())
}

// fullLayout 模态层布局:可见子项整体拉伸到层尺寸(=overlay stack 行为,
// 窗口缩放时遮罩/卡片跟随重排),隐藏项不参与
type fullLayout struct{}

func (f fullLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		o.Move(fyne.NewPos(0, 0))
		o.Resize(s)
	}
}

func (f fullLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	m := fyne.NewSize(0, 0)
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		min := o.MinSize()
		if min.Width > m.Width {
			m.Width = min.Width
		}
		if min.Height > m.Height {
			m.Height = min.Height
		}
	}
	return m
}

// Show/Hide 挂内容树模态层(见 modalLayer 注释:overlay 会盖住 toast 层);
// Hide 同步置隐藏标志,调用方的 Visible() 轮询(关窗收尾/计数回读)才收得了
func (m *maskModal) Show() {
	m.Move(fyne.NewPos(0, 0))
	m.Resize(m.cnv.Size())
	if modalLayer != nil {
		modalLayer.Add(m)
	}
	m.BaseWidget.Show()
}

func (m *maskModal) Hide() {
	if modalLayer != nil {
		modalLayer.Remove(m)
		modalLayer.Refresh()
	}
	m.BaseWidget.Hide()
}

// SetCardSize 运行时改卡片宽度(=宽表页签 780→1140)。只动卡片尺寸下限,
// 全屏遮罩层保持不动;刷新本层让 Center 按新 min 重新居中卡片。
// 曾误用 Resize/Move 直接改本层,把全屏遮罩缩成卡片大小,切 tab 后遮罩
// 消失/露出底层内容
func (m *maskModal) SetCardSize(w, h float32) {
	if m.cardSizer == nil {
		return
	}
	m.cardSizer.SetMin(fyne.NewSize(w, h))
	m.Refresh()
}

// modalStack 弹窗注册表:ESC 关最上层可见弹窗(=Vue overlay 统一的 Esc 语义;
// fyne PopUp 无 OnDismiss/Escape 处理,由主窗 SetOnTypedKey 驱动,见 NewApp)。
// 仅 UI 线程读写。PopUpMenu 聚焦时自带 Escape 关闭,不入栈。
type modalLike interface {
	fyne.CanvasObject
	Hide()
}

var modalStack []modalLike

func trackPop(pop modalLike) {
	modalStack = append(modalStack, pop)
}

// hideTopModal 从栈顶关第一个仍可见的弹窗,顺带清理已自隐藏的陈旧项
// (部分弹窗会直接 Hide 兜底,如抽屉 404 自动关窗)
func hideTopModal() {
	top := -1
	kept := modalStack[:0]
	for _, pop := range modalStack {
		if pop.Visible() {
			kept = append(kept, pop)
			top = len(kept) - 1
		}
	}
	modalStack = kept
	if top >= 0 {
		kept[top].Hide()
	}
}

// clearModals 关闭全部模态并清空注册表(主题切换重建主界面树前调用:
// 模态挂在旧树上,留着就是悬空的陈旧遮罩)
func clearModals() {
	for _, pop := range modalStack {
		pop.Hide()
	}
	modalStack = nil
}

// switchW 开关(=switch:40×22 轨道 radius 20,16px 白色圆钮,选中 accent 底、
// 圆钮右移 18px;0.18s 过渡只做视觉,状态与回调即时生效)
type switchW struct {
	widget.BaseWidget
	on    bool
	pal   palette
	track *canvas.Rectangle
	knob  *canvas.Circle
	knobX float32 // 圆钮当前 x(动画与布局的共同事实源;Stack 布局会重置子项
	// 位置,靠 Move 存状态会在任意重排后漂回左端——用户报的"圆点位置错误")
	onChg func(bool)
	anim  *fyne.Animation
}

func newSwitch(pal palette, on bool, onChg func(bool)) *switchW {
	s := &switchW{on: on, pal: pal, onChg: onChg}
	s.ExtendBaseWidget(s)
	s.track = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.3))
	s.track.CornerRadius = 11
	s.knob = canvas.NewCircle(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	s.apply(false)
	return s
}

func (s *switchW) CreateRenderer() fyne.WidgetRenderer { return &switchRenderer{s: s} }

// switchRenderer 圆钮位置由 knobX 显式摆放,重排不再吞掉动画结果
type switchRenderer struct{ s *switchW }

func (r *switchRenderer) Layout(size fyne.Size) {
	// 轨道固定 40×22 垂直居中:行内 HBox 会把子项拉到行高(与更高按钮同
	// 行时胶囊跟着变高=用户报的图2)
	th := fyne.NewSize(40, 22)
	y := (size.Height - th.Height) / 2
	if y < 0 {
		y = 0
	}
	r.s.track.Move(fyne.NewPos(0, y))
	r.s.track.Resize(th)
	r.s.knob.Move(fyne.NewPos(r.s.knobX, y+(th.Height-16)/2))
	r.s.knob.Resize(fyne.NewSize(16, 16))
}

func (r *switchRenderer) MinSize() fyne.Size { return fyne.NewSize(40, 22) }
func (r *switchRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.s.track, r.s.knob}
}
func (r *switchRenderer) Refresh() { r.Layout(r.s.Size()) } // 位置按 knobX 重放
func (r *switchRenderer) Destroy() {}

func (s *switchW) Tapped(*fyne.PointEvent) { s.SetOn(!s.on) }

// SetOn 置位并触发回调(同值跳过;动画期间再点直接重定目标)
func (s *switchW) SetOn(on bool) {
	if s.on == on {
		return
	}
	s.on = on
	s.apply(true)
	if s.onChg != nil {
		s.onChg(on)
	}
}

func (s *switchW) apply(animate bool) {
	if s.on {
		s.track.FillColor = s.pal.accent
	} else {
		s.track.FillColor = rgbaA(0x7f7f7f, 0.3)
	}
	s.track.Refresh()
	target := float32(3)
	if s.on {
		target = 21 // 40-16-3(=translateX(18px))
	}
	if !animate || s.knobX == target {
		s.knobX = target
		s.Refresh() // 位置由 renderer 按 knobX 重放(圆点纵向也由此居中)
		return
	}
	from := s.knobX
	if s.anim != nil {
		fyne.CurrentApp().Driver().StopAnimation(s.anim)
	}
	s.anim = fyne.NewAnimation(180*time.Millisecond, func(f float32) {
		s.knobX = from + (target-from)*f
		s.Refresh()
	})
	fyne.CurrentApp().Driver().StartAnimation(s.anim)
}

// qTip 设置项问号气泡(=.q/.q-tip:14px 圆圈 ?,悬停提亮、点击弹出说明;
// fyne 无 CSS 悬停层,气泡用 PopUp 承载,点外部即收)
type qTip struct {
	widget.BaseWidget
	text    string
	pal     palette
	ring    *canvas.Rectangle
	dot     *canvas.Text
	hovered bool
}

func newQTip(pal palette, text string) *qTip {
	q := &qTip{text: text, pal: pal}
	q.ExtendBaseWidget(q)
	q.ring = canvas.NewRectangle(color.NRGBA{A: 0})
	q.ring.CornerRadius = 7
	q.ring.StrokeWidth = 1
	q.ring.StrokeColor = pal.borderStrong
	q.dot = canvas.NewText("?", pal.textFaint)
	q.dot.TextSize = 10
	q.dot.TextStyle = fyne.TextStyle{Bold: true}
	q.dot.Alignment = fyne.TextAlignCenter
	return q
}

func (q *qTip) CreateRenderer() fyne.WidgetRenderer {
	return &qTipRenderer{q: q, obj: container.NewStack(q.ring, container.NewCenter(q.dot))}
}

// qTipRenderer 圆圈固定 14×14 行内居中:行布局(HBox)会把子项拉到行高,
// 直接 Resize 会把圆圈拉成竖椭圆(用户报的"圆圈变椭圆")
type qTipRenderer struct {
	q   *qTip
	obj fyne.CanvasObject
}

func (r *qTipRenderer) Layout(size fyne.Size) {
	r.obj.Move(fyne.NewPos((size.Width-14)/2, (size.Height-14)/2))
	r.obj.Resize(fyne.NewSize(14, 14))
}
func (r *qTipRenderer) MinSize() fyne.Size           { return fyne.NewSize(14, 14) }
func (r *qTipRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.obj} }
func (r *qTipRenderer) Destroy()                     {}
func (r *qTipRenderer) Refresh()                     {}

func (q *qTip) Tapped(*fyne.PointEvent) { q.showTip() }
func (q *qTip) MouseIn(*desktop.MouseEvent) {
	q.hovered = true
	q.tint()
}
func (q *qTip) MouseOut() {
	q.hovered = false
	q.tint()
}
func (q *qTip) MouseMoved(*desktop.MouseEvent) {}

func (q *qTip) tint() {
	if q.hovered {
		q.ring.StrokeColor = q.pal.accent
		q.dot.Color = q.pal.accent
	} else {
		q.ring.StrokeColor = q.pal.borderStrong
		q.dot.Color = q.pal.textFaint
	}
	q.ring.Refresh()
	q.dot.Refresh()
}

// showTip 说明浮层(=q-tip:max 320 宽,8/11 内距,panel-solid+边框,
// 图标下方 -8px 左偏;贴底翻转到上方,越界回夹)
func (q *qTip) showTip() {
	drv := fyne.CurrentApp().Driver()
	cnv := drv.CanvasForObject(q)
	if cnv == nil {
		return
	}
	// 自然宽度超 320 才换行(=width:max-content;max-width:320px)
	natural := newColorLabelS(q.text, q.pal.textDim, false, 11.5).MinSize().Width + 22
	wrap := natural > 320
	lbl := widget.NewLabel(q.text)
	if wrap {
		lbl.Wrapping = fyne.TextWrapWord
	}
	bg := canvas.NewRectangle(q.pal.panelSolid)
	bg.CornerRadius = 6
	bg.StrokeWidth = 1
	bg.StrokeColor = q.pal.borderStrong
	w := natural
	if w > 320 {
		w = 320
	}
	body := newFixedMin(container.NewStack(bg,
		container.New(layout.NewCustomPaddedLayout(8, 8, 11, 11), lbl)), fyne.NewSize(w, 0))
	pop := widget.NewPopUp(body, cnv)
	abs := drv.AbsolutePositionForObject(q)
	x := abs.X - 8
	if x+w > cnv.Size().Width-8 {
		x = cnv.Size().Width - 8 - w
	}
	y := abs.Y + 22
	if y+body.MinSize().Height > cnv.Size().Height-8 {
		y = abs.Y - 8 - body.MinSize().Height
	}
	pop.ShowAtPosition(fyne.NewPos(x, y))
}

func (g *roundedGrad) render(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r1, g1, b1, a1 := g.c1.RGBA()
	r2, g2, b2, a2 := g.c2.RGBA()
	rad := float64(g.radius)
	fw, fh := float64(w), float64(h)
	for y := 0; y < h; y++ {
		fy := float64(y) + 0.5
		dy := math.Min(fy, fh-fy)
		for x := 0; x < w; x++ {
			fx := float64(x) + 0.5
			dx := math.Min(fx, fw-fx)
			cov := 1.0
			// 圆角区:到圆心的距离不足半径则按覆盖率渐隐(±0.5 抗锯齿)
			if dx < rad && dy < rad {
				dd := math.Hypot(rad-dx, rad-dy)
				cov = math.Min(1, math.Max(0, rad-dd+0.5))
			}
			if cov <= 0 {
				continue
			}
			t := 0.0
			if w > 1 {
				t = float64(x) / float64(w-1)
			}
			fr := uint8(math.Round((float64(r1) + (float64(r2)-float64(r1))*t) / 257))
			fgc := uint8(math.Round((float64(g1) + (float64(g2)-float64(g1))*t) / 257))
			fbc := uint8(math.Round((float64(b1) + (float64(b2)-float64(b1))*t) / 257))
			fa := uint8(math.Round((float64(a1) + (float64(a2)-float64(a1))*t) / 257 * cov))
			img.SetNRGBA(x, y, color.NRGBA{R: fr, G: fgc, B: fbc, A: fa})
		}
	}
	return img
}

// styledCheck 自绘勾选框(=input[type=checkbox] appearance:none 样式):
// 16px 圆角方(半径 5)、1.5px 描边、灰 12% 底;勾选=accent→accent-2 渐变底
// + 白勾;半选=渐变底 + 白横杠(目录部分选中);悬停=accent 描边 + accent
// 12% 底;禁用=整体 45% 透明。不用 widget.Check:它可聚焦,点击后画焦点
// 圆圈(用户报的"圆圈"),且观感与主题不符。可选文字标签(整体可点)。
type styledCheck struct {
	widget.BaseWidget
	pal      palette
	label    string
	checked  bool
	mixed    bool
	disabled bool
	onChange func(bool)

	hovered bool
	bg      *canvas.Rectangle // 底 + 描边(勾选时让位渐变层)
	grad    *roundedGrad      // 勾选/半选渐变底
	tick    *canvas.Text      // 白勾
	dash    *canvas.Rectangle // 半选白杠
	lbl     *colorLabel
	hg      *hoverGroup       // 挂靠的行组:悬停本复选框时保持整行高亮
}

var _ fyne.Tappable = (*styledCheck)(nil)
var _ desktop.Hoverable = (*styledCheck)(nil)

func (c *styledCheck) SetHoverGroup(g *hoverGroup) { c.hg = g }

func newStyledCheck(pal palette, label string, checked bool, onChange func(bool)) *styledCheck {
	c := &styledCheck{pal: pal, label: label, checked: checked, onChange: onChange}
	c.ExtendBaseWidget(c)
	c.bg = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.12))
	c.bg.CornerRadius = 5
	c.bg.StrokeWidth = 1.5
	c.grad = newRoundedGrad(pal.accent, pal.accent2, 5)
	c.tick = msText("check", color.NRGBA{R: 255, G: 255, B: 255, A: 255}, 11)
	c.dash = canvas.NewRectangle(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	c.dash.CornerRadius = 1.5
	if label != "" {
		c.lbl = newColorLabelS(label, pal.text, false, 13)
	}
	c.applyStyle()
	return c
}

// SetChecked 程序化置位(不触发 onChange:轮询同步服务端状态用)
func (c *styledCheck) SetChecked(v bool) {
	if c.checked == v {
		return
	}
	c.checked = v
	if v {
		c.mixed = false
	}
	c.applyStyle()
}

// SetMixed 半选(目录部分选中);置位同时清勾选
func (c *styledCheck) SetMixed(v bool) {
	if c.mixed == v {
		return
	}
	c.mixed = v
	if v {
		c.checked = false
	}
	c.applyStyle()
}

// IsChecked 当前勾选态(=btChoose.Checked 读点)
func (c *styledCheck) IsChecked() bool { return c.checked }

// Disable/Enable 禁用(=input:disabled opacity .45,磁力链接等)
func (c *styledCheck) Disable() {
	if !c.disabled {
		c.disabled = true
		c.applyStyle()
	}
}

func (c *styledCheck) Enable() {
	if c.disabled {
		c.disabled = false
		c.applyStyle()
	}
}

// Tapped 点击切换(半选点击=全选,=原生 indeterminate 语义)
func (c *styledCheck) Tapped(*fyne.PointEvent) {
	if c.disabled {
		return
	}
	if c.mixed {
		c.mixed = false
		c.checked = true
	} else {
		c.checked = !c.checked
	}
	c.applyStyle()
	if c.onChange != nil {
		c.onChange(c.checked)
	}
}

func (c *styledCheck) MouseIn(*desktop.MouseEvent)    { c.hg.enter(); c.hovered = true; c.applyStyle() }
func (c *styledCheck) MouseOut()                      { c.hg.leave(); c.hovered = false; c.applyStyle() }
func (c *styledCheck) MouseMoved(*desktop.MouseEvent) {}

// applyStyle 状态→视觉(全量同值早退:增量刷新反复 SetChecked 不重绘)
func (c *styledCheck) applyStyle() {
	on := c.checked || c.mixed
	var fill, stroke color.Color
	if on {
		fill = color.NRGBA{A: 0} // 渐变层盖住
		stroke = color.NRGBA{A: 0}
	} else {
		fill = rgbaA(0x7f7f7f, 0.12)
		stroke = c.pal.borderStrong
		if c.hovered && !c.disabled {
			fill = withAlpha(c.pal.accent, 0.12)
			stroke = c.pal.accent
		}
	}
	dim := func(x color.Color) color.Color {
		if c.disabled {
			return withAlpha(x, 0.45)
		}
		return x
	}
	if !sameColor(c.bg.FillColor, dim(fill)) {
		c.bg.FillColor = dim(fill)
		c.bg.Refresh()
	}
	if !sameColor(c.bg.StrokeColor, dim(stroke)) {
		c.bg.StrokeColor = dim(stroke)
		c.bg.Refresh()
	}
	if c.grad.Visible() != on {
		if on {
			c.grad.Show()
		} else {
			c.grad.Hide()
		}
	}
	glyphColor := dim(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	if !sameColor(c.tick.Color, glyphColor) {
		c.tick.Color = glyphColor
		c.tick.Refresh()
	}
	if c.tick.Visible() != c.checked {
		if c.checked {
			c.tick.Show()
		} else {
			c.tick.Hide()
		}
	}
	if c.dash.Visible() != c.mixed {
		if c.mixed {
			c.dash.Show()
		} else {
			c.dash.Hide()
		}
	}
}

func (c *styledCheck) CreateRenderer() fyne.WidgetRenderer {
	return &styledCheckRenderer{c: c}
}

type styledCheckRenderer struct{ c *styledCheck }

func (r *styledCheckRenderer) Layout(size fyne.Size) {
	y := (size.Height - 16) / 2
	if y < 0 {
		y = 0
	}
	r.c.bg.Move(fyne.NewPos(0, y))
	r.c.bg.Resize(fyne.NewSize(16, 16))
	r.c.grad.Move(fyne.NewPos(0, y))
	r.c.grad.Resize(fyne.NewSize(16, 16))
	ts := r.c.tick.MinSize()
	r.c.tick.Move(fyne.NewPos((16-ts.Width)/2, y+(16-ts.Height)/2))
	r.c.dash.Move(fyne.NewPos(3.5, y+6.75))
	r.c.dash.Resize(fyne.NewSize(9, 2.5))
	if r.c.lbl != nil {
		ls := r.c.lbl.MinSize()
		ly := (size.Height - ls.Height) / 2
		if ly < 0 {
			ly = 0
		}
		r.c.lbl.Move(fyne.NewPos(24, ly))
		r.c.lbl.Resize(fyne.NewSize(size.Width-24, ls.Height))
	}
}

func (r *styledCheckRenderer) MinSize() fyne.Size {
	if r.c.lbl == nil {
		return fyne.NewSize(16, 16)
	}
	ls := r.c.lbl.MinSize()
	h := ls.Height
	if h < 16 {
		h = 16
	}
	return fyne.NewSize(24+ls.Width, h)
}

func (r *styledCheckRenderer) Objects() []fyne.CanvasObject {
	if r.c.lbl == nil {
		return []fyne.CanvasObject{r.c.bg, r.c.grad, r.c.tick, r.c.dash}
	}
	return []fyne.CanvasObject{r.c.bg, r.c.grad, r.c.tick, r.c.dash, r.c.lbl}
}

func (r *styledCheckRenderer) Refresh() { r.c.applyStyle() }
func (r *styledCheckRenderer) Destroy() {}

// vScrollNS 纵向滚动区。曾用 ThemeOverride 局部去滚动黑影/调表单内距,
// 但字体缓存按 scope 键控,每个 ThemeOverride 都复制整套字体 Face+表缓冲
// (实测每开一个弹窗 +3~4MB 永不回收);黑影/内距已全局化,退化为普通滚动
func vScrollNS(content fyne.CanvasObject) fyne.CanvasObject {
	return container.NewVScroll(content)
}

// flexW 指定权重的弹性子项(=CSS flex:N;网格列宽比例用,如封禁表
// 1fr/.9fr/148px/1.2fr/28px)
func flexW(o fyne.CanvasObject, w float32) fyne.CanvasObject {
	f := &flexWrap{content: o, weight: w}
	f.ExtendBaseWidget(f)
	return f
}

// hintText 说明小字(=hint-text:11.5px text-faint,margin-top 5)
func hintText(pal palette, s string) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(5, 0, 0, 0),
		newColorLabelS(s, pal.textFaint, false, 11.5).SetMaxWidth())
}

// tapWrap 无视觉包装,点击子对象时回调(=Vue 任务卡标题 nameClick 这类
// 「文本本身是入口」的语义;直接给 widget.Label 挂 Tappable 需自扩展,
// 包一层避免侵入标签本身)
type tapWrap struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onTap   func()
}

func newTapWrap(content fyne.CanvasObject, onTap func()) *tapWrap {
	t := &tapWrap{content: content, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tapWrap) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.content)
}

func (t *tapWrap) WrapObject() fyne.CanvasObject { return t.content }

func (t *tapWrap) Tapped(*fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

// titleTap 可点标题:整块点击回调 + 悬停进出回调(=Vue .name.clickable:hover),
// 可选挂悬停提示(=Vue :title nameTitle)。fyne 的 Label/文本对象不自带 hover,
// 包一层才能既接点击又接悬停态。
type titleTap struct {
	widget.BaseWidget
	content   fyne.CanvasObject
	onTap     func()
	onHover   func(bool)
	tipText   func() string // 悬停提示;nil=不提示(=Vue :title nameTitle)
	clickable func() bool   // 是否可点(=Vue .name.clickable):仅可点时手形指针;nil=不可点
	tipTimer  *time.Timer
	tipPop    *tipPopW
	hg        *hoverGroup // 挂靠的行组(nil=独立,如任务卡标题)
}

func newTitleTap(content fyne.CanvasObject, onTap func(), onHover func(bool)) *titleTap {
	w := &titleTap{content: content, onTap: onTap, onHover: onHover}
	w.ExtendBaseWidget(w)
	return w
}

func (w *titleTap) SetHoverGroup(g *hoverGroup) { w.hg = g }

// SetTip 挂悬停提示(=Vue :title;悬停 500ms 弹出,移开即收)
func (w *titleTap) SetTip(fn func() string) { w.tipText = fn }

// SetClickable 设可点判据(=Vue .name.clickable):仅可点时鼠标悬停显手形指针
func (w *titleTap) SetClickable(fn func() bool) { w.clickable = fn }

// 手形指针:仅标题可点(边下边播/文件列表/打开)时切手形,不可点保持默认箭头
func (w *titleTap) Cursor() desktop.Cursor {
	if w.clickable != nil && w.clickable() {
		return desktop.PointerCursor
	}
	return desktop.DefaultCursor
}

func (w *titleTap) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(w.content)
}

func (w *titleTap) Tapped(*fyne.PointEvent) {
	if w.onTap != nil {
		w.onTap()
	}
}

func (w *titleTap) MouseIn(*desktop.MouseEvent) {
	w.hg.enter()
	if w.onHover != nil {
		w.onHover(true)
	}
	if w.tipText == nil {
		return
	}
	w.tipTimer = time.AfterFunc(500*time.Millisecond, func() {
		fyne.Do(func() {
			if w.tipPop != nil {
				return
			}
			if txt := w.tipText(); txt != "" {
				w.tipPop = showLabelTip(w, txt)
			}
		})
	})
}

func (w *titleTap) MouseOut() {
	w.hg.leave()
	if w.onHover != nil {
		w.onHover(false)
	}
	if w.tipTimer != nil {
		w.tipTimer.Stop()
		w.tipTimer = nil
	}
	if w.tipPop != nil {
		pop := w.tipPop
		w.tipPop = nil
		pop.remove()
	}
}

func (w *titleTap) MouseMoved(*desktop.MouseEvent) {}

// zSelect 下拉(=ZSelect.vue)。fyne 自带 Select 无边框且弹层按内容高撑满
// 画布(64 项数字下拉直通天际=用户报的图4),自建:描边触发钮 + 定宽限高
// 弹出菜单(PopUpMenu 自带滚动与外部点击收起)。
// singleLine 把单行输入框配成"横向裁剪、无纵向滚动条"。Fyne v2.8 里
// fyne.TextTruncate 实为默认值,会给文本套一个 ScrollBoth 滚动器——内容一长
// 就同时冒出横/纵滚动条,超长 URL 甚至越出文本框右缘(=用户报的图1)。
// 改 TextWrapOff + 横向滚动:短内容无任何滚动条,超长仅出现细横条且文本被
// 正确裁剪不越框(近似原生 <input> 行为)
func singleLine(e *widget.Entry) *widget.Entry {
	e.Wrapping = fyne.TextWrapOff
	e.Scroll = fyne.ScrollHorizontalOnly
	return e
}

// singleLineH 单行输入框规范高:以一个与界面所有单行框同配置的原生 Entry 实测
// MinSize 为准,供 dirField/zSelect 等自绘控件对齐,杜绝"硬编码 36 比原生框矮
// 一截"(=用户报的组合框/目录框高度不齐)。须与 singleLine 的 Scroll 模式一致
func singleLineH() float32 {
	e := widget.NewEntry()
	e.SetText("M") // 喂一个字符强制按单行文本量高(空占位量高不稳)
	e.Wrapping = fyne.TextWrapOff
	e.Scroll = fyne.ScrollHorizontalOnly
	return e.MinSize().Height
}

type zSelect struct {
	widget.BaseWidget
	pal       palette
	Options   []string     // 选项(label 即值,与 Vue options {value,label} 的调用方映射一致)
	Selected  string       // 当前值(调用方直接读)
	OnChanged func(string) // 选中回调(=ZSelect @change)
	open      bool
	hovered   bool
	pop       *zMenuPop // 打开中的弹层(再点触发钮收起用)
	box       *canvas.Rectangle // 底(=input 背景色,内缩整个 border 宽);与 Entry 分体画法对齐
	ring      *canvas.Rectangle // 描边(透明底 + border 色细描边,内缩 half);=Entry 的 border
	lbl       *colorLabel
	chev      *canvas.Text
	minH      float32 // 单行输入框规范高(=singleLineH),与原生输入框等齐
}

func newZSelect(pal palette, opts []string, cur string) *zSelect {
	s := &zSelect{pal: pal, Options: opts, Selected: cur}
	s.ExtendBaseWidget(s)
	s.minH = singleLineH()
	// 底/描边分体,逐像素对齐 widget.Entry 的 box+border 画法(entry.go:181-186):
	// box=input 背景色、无描边;ring=透明底 + 描边(粗=SizeNameInputBorder 未覆盖
	// 时=2,色=ColorNameInputBorder=border)。此前用一个"填充+描边"合一的 ring 且
	// 填充只内缩 1px,会画成一块灰底卡片(=用户报的"组合框外一圈灰粗边、像 tooltip";
	// 非阴影——主题已把 ColorNameShadow 全局置透明)
	s.box = canvas.NewRectangle(pal.bgSoft)
	s.box.CornerRadius = 9
	s.ring = canvas.NewRectangle(color.Transparent)
	s.ring.CornerRadius = 9
	s.ring.StrokeWidth = 2
	s.ring.StrokeColor = pal.border
	s.lbl = newColorLabelS(cur, pal.text, false, 13.5).SetMaxWidth()
	s.chev = msText("keyboard_arrow_down", pal.textDim, 14)
	return s
}

// SetSelected 静默置值(不触发 OnChanged;=ZSelect 外部 v-model 回填)
func (s *zSelect) SetSelected(v string) {
	s.Selected = v
	s.lbl.SetText(v)
	s.Refresh()
}

func (s *zSelect) setBorder(c color.Color) {
	if sameColor(s.ring.StrokeColor, c) {
		return
	}
	s.ring.StrokeColor = c
	s.ring.Refresh()
}

func (s *zSelect) Tapped(*fyne.PointEvent) { s.showPop() }
func (s *zSelect) MouseIn(*desktop.MouseEvent) {
	s.hovered = true
	s.syncBorder()
}
func (s *zSelect) MouseOut() {
	s.hovered = false
	s.syncBorder()
}
func (s *zSelect) MouseMoved(*desktop.MouseEvent) {}

// 手形指针(=Vue .zselect-trigger{cursor:pointer}):下拉触发钮可点
func (s *zSelect) Cursor() desktop.Cursor { return desktop.PointerCursor }

// syncBorder 边框三态:open=accent(zselect-trigger.open)、hover=border-strong、
// 常态=border
func (s *zSelect) syncBorder() {
	if s.open {
		s.setBorder(s.pal.accent)
	} else if s.hovered {
		s.setBorder(s.pal.borderStrong)
	} else {
		s.setBorder(s.pal.border)
	}
}

func (s *zSelect) CreateRenderer() fyne.WidgetRenderer {
	// 文字走 Border 中列拉伸:HBox 只给子项 min 宽(10px),maxW 截断
	// 模式下永远显示省略号
	inner := container.New(layout.NewCustomPaddedLayout(8, 8, 12, 12),
		container.NewBorder(nil, nil, nil,
			container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), s.chev), s.lbl))
	// 描边不进容器:容器重排会把 1px 描边拉满整块(dnd 指示线同类坑),
	// 渲染器每次布局后单独摆
	return &zSelectRenderer{s: s, inner: inner}
}

// zSelectRenderer 底/描边几何逐像素对齐 fyne Entry(entry.go:1553-1557):
// box 内缩 border 宽(2)、尺寸 -4;border 内缩 half(1)、尺寸 -2.5。填充停在
// 描边内侧,只有描边是可见框线——与文本框同色同宽同圆角,不再呈"灰底卡片"
type zSelectRenderer struct {
	s     *zSelect
	inner fyne.CanvasObject
}

func (r *zSelectRenderer) Layout(size fyne.Size) {
	r.s.box.Move(fyne.NewPos(2, 2))
	r.s.box.Resize(fyne.NewSize(size.Width-4, size.Height-4))
	r.s.ring.Move(fyne.NewPos(1, 1))
	r.s.ring.Resize(fyne.NewSize(size.Width-2.5, size.Height-2.5))
	r.inner.Resize(size)
}

func (r *zSelectRenderer) MinSize() fyne.Size { return fyne.NewSize(110, r.s.minH) }
func (r *zSelectRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.s.box, r.s.ring, r.inner}
}
func (r *zSelectRenderer) Destroy() {}

func (r *zSelectRenderer) Refresh() {
	r.inner.Refresh()
	if s := r.s.Size(); s.Width > 0 {
		r.Layout(s)
	}
}

// showPop 弹层:宽=触发钮宽(下限 120),高 min(内容,320) 超高滚动;
// 贴触发钮下缘,放不下翻到上方,右缘回夹(=zselect-pop + Vue 定位规则)。
// 焦点在手:点别处(FocusLost)与 ESC 都能收起;再次点触发钮也能收
func (s *zSelect) showPop() {
	drv := fyne.CurrentApp().Driver()
	cnv := drv.CanvasForObject(s)
	if cnv == nil || len(s.Options) == 0 {
		return
	}
	if s.pop != nil && !s.pop.hid {
		s.pop.Hide() // 开着再点=收起(Vue toggle 语义;焦点丢失时也兜底)
		return
	}
	pop := newZMenuPop(cnv, s.pal, s.Options, s.Selected, s.popPick)
	pop.onHide = func() {
		s.pop = nil
		s.open = false
		s.syncBorder()
	}
	s.pop = pop
	s.open = true
	s.syncBorder()
	w := s.Size().Width
	if w < 120 {
		w = 120
	}
	h := pop.contentMin().Height + 8 // +上下 padding
	if h > 320 {
		h = 320 // =zselect-pop max-height 语义,超出交给内部滚动
	}
	pop.size = fyne.NewSize(w, h)
	abs := drv.AbsolutePositionForObject(s)
	x := abs.X
	if x+w > cnv.Size().Width-8 {
		x = cnv.Size().Width - 8 - w
	}
	y := abs.Y + s.Size().Height + 2
	if y+h > cnv.Size().Height-8 {
		y = abs.Y - 2 - h
	}
	pop.ShowAtPosition(fyne.NewPos(x, y))
}

func (s *zSelect) popPick(v string) {
	changed := v != s.Selected
	s.Selected = v
	s.lbl.SetText(v)
	if changed && s.OnChanged != nil {
		s.OnChanged(v)
	}
}

// zMenuPop 自建下拉弹层(=zselect-pop:panel-solid+border-strong 圆角,
// padding 4;行=hover 提亮/选中 accent 加粗+右勾)。fyne Menu 的 MinSize
// 恒为整份内容高,overlay 布局的 MaxSizes 规则会把外部 Resize 顶回去,限高
// 必须自建。fyne 命中分发只遍历 Overlays().Top() 一棵树——弹层开着时根
// 节点必须是 Tappable 捕获层:点外面=收起(与 Vue 点外即关一致),否则
// 一次未命中的点击会被整个吞掉、下层 UI 全部锁死。ESC 走 modalStack。
type zMenuPop struct {
	widget.BaseWidget
	cnv     fyne.Canvas // 所属画布(根未挂树时 CanvasForObject 拿不到,须显式传)
	panel   fyne.CanvasObject
	pos     fyne.Position
	size    fyne.Size
	onPick  func(string)
	onHide  func()
	rowsBox *fyne.Container
	hid     bool
}

func newZMenuPop(cnv fyne.Canvas, pal palette, opts []string, cur string, onPick func(string)) *zMenuPop {
	p := &zMenuPop{cnv: cnv, onPick: onPick}
	p.ExtendBaseWidget(p)
	rows := container.New(&vgapLayout{gap: 2})
	for _, o := range opts {
		opt := o
		rows.Add(newZMenuRow(pal, o, o == cur, func() {
			p.hide()
			if p.onPick != nil {
				p.onPick(opt)
			}
		}))
	}
	p.rowsBox = rows
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 9
	bg.StrokeWidth = 1
	bg.StrokeColor = pal.borderStrong
	p.panel = container.NewStack(bg,
		container.New(layout.NewCustomPaddedLayout(4, 4, 4, 4),
			container.NewVScroll(rows)))
	return p
}

func (p *zMenuPop) contentMin() fyne.Size { return p.rowsBox.MinSize() }

// Tapped 根捕获层:点在面板外(面板行更深处命中,不会冒到这)=收起,
// 与 Vue 点外即关一致;这次点击按桌面惯例只用于关闭
func (p *zMenuPop) Tapped(*fyne.PointEvent) { p.Hide() }

func (p *zMenuPop) CreateRenderer() fyne.WidgetRenderer {
	return &zMenuPopRenderer{p: p}
}

// zMenuPopRenderer 面板不进中间容器:overlay 根会被强制全画布,任何包裹
// 容器的重排都会把面板拉回满幅,渲染器按 p.pos/p.size 直接摆放
type zMenuPopRenderer struct{ p *zMenuPop }

func (r *zMenuPopRenderer) Layout(fyne.Size) {
	r.p.panel.Move(r.p.pos)
	r.p.panel.Resize(r.p.size)
}

func (r *zMenuPopRenderer) MinSize() fyne.Size           { return fyne.NewSize(1, 1) }
func (r *zMenuPopRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.p.panel} }
func (r *zMenuPopRenderer) Destroy()                     {}

func (r *zMenuPopRenderer) Refresh() { r.Layout(fyne.Size{}) }

// ShowAtPosition 挂 overlay 栈(会被强制全画布)后把面板摆到目标位置定尺寸
func (p *zMenuPop) ShowAtPosition(pos fyne.Position) {
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

// Hide 收起(hid 防重入);选中项走 hide 先摘 overlay 避免重入
func (p *zMenuPop) Hide() {
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

func (p *zMenuPop) hide() {
	p.hid = true
	if p.cnv != nil {
		p.cnv.Overlays().Remove(p)
	}
	p.BaseWidget.Hide()
	if p.onHide != nil {
		p.onHide()
	}
}

// zMenuRow 单条选项(=zselect-item:7/10 内距、hover accent 12%、
// 选中 accent 加粗+右勾)
type zMenuRow struct {
	widget.BaseWidget
	pal      palette
	label    string
	selected bool
	onTap    func()
	hl       *canvas.Rectangle
	lbl      *colorLabel
	check    *canvas.Text
}

func newZMenuRow(pal palette, label string, selected bool, onTap func()) *zMenuRow {
	r := &zMenuRow{pal: pal, label: label, selected: selected, onTap: onTap}
	r.ExtendBaseWidget(r)
	// hl 常驻可见、默认透明填充:若用 Hide()/Show() 切换,隐藏态的 canvas 对象
	// 在驱动里没登记真实区域,MouseIn 里 Show() 要等滚动重建区域/裁剪后才真正
	// 重绘——正是"必须滚一下才有 hover"的根因。改为始终可见只切 FillColor,
	// 重绘区域始终有效
	r.hl = canvas.NewRectangle(color.NRGBA{A: 0})
	r.hl.CornerRadius = 6
	c := pal.text
	if selected {
		c = pal.accent
	}
	r.lbl = newColorLabelS(label, c, selected, 13).SetMaxWidth()
	r.check = msText("check", pal.accent, 14)
	r.check.Hide()
	return r
}

func (r *zMenuRow) CreateRenderer() fyne.WidgetRenderer {
	inner := container.New(layout.NewCustomPaddedLayout(7, 7, 10, 10),
		container.NewBorder(nil, nil, nil,
			container.New(layout.NewCustomPaddedLayout(0, 0, 12, 0), r.check), r.lbl))
	return &simpleRenderer{obj: container.NewStack(r.hl, inner), min: fyne.NewSize(60, 33)}
}

func (r *zMenuRow) Tapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap()
	}
}

// hover=accent 12% 底(=zselect-item hover),非 hover=全透明;只改填充色,
// 不 Show/Hide,重绘区域保持有效
func (r *zMenuRow) MouseIn(*desktop.MouseEvent) {
	r.hl.FillColor = withAlpha(r.pal.accent, 0.12)
	r.hl.Refresh()
}

func (r *zMenuRow) MouseOut() {
	r.hl.FillColor = color.NRGBA{A: 0}
	r.hl.Refresh()
}

func (r *zMenuRow) MouseMoved(*desktop.MouseEvent) {}

// 手形指针(=Vue .zselect-item{cursor:pointer}):下拉选项可点
func (r *zMenuRow) Cursor() desktop.Cursor { return desktop.PointerCursor }
