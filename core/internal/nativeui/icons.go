package nativeui

// 图标层:Material Symbols Rounded 字体渲染(与 Vue 版同一套字形/码位)。
// MS_ICONS 与 webapp/src/lib/icons.js 保持同步(唯一事实源),字体子集由
// internal/assets/ms-icons.ttf 提供(fonttools 按 MS_ICONS 码位裁剪)。
//
// 组件:
//   - msText(name, color, size):单个图标字形(canvas.Text)
//   - iconButton:31×31 透明底图标按钮,悬停淡底(=task-actions .icon-btn)
//   - colorLabel:任意色的单行文本(fyne v2.8 的 Label 无 Color 字段)

import (
	"image/color"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/assets"
)

// MS_ICONS 码位表(逐行对齐 icons.js;新增图标需同步字体子集)
var MS_ICONS = map[string]rune{
	"folder": 0xe2c7, "folder_open": 0xe2c8, "folder_zip": 0xeb2c,
	"movie": 0xe404, "music_note": 0xe405, "image": 0xe3f4,
	"photo_library": 0xe413, "description": 0xe873, "article": 0xef42,
	"text_snippet": 0xf1c6, "archive": 0xe149, "picture_as_pdf": 0xe415,
	"subtitles": 0xe048, "data_object": 0xead3, "code": 0xe86f,
	"terminal": 0xeb8e, "table_chart": 0xe265, "note": 0xe66d,
	"album": 0xe019, "settings": 0xe8b8, "palette": 0xe40a,
	"add": 0xe145, "add_circle": 0xe990, "close": 0xe5cd,
	"check": 0xe668, "check_circle": 0xf0be, "delete": 0xe92e,
	"pause": 0xe034, "play_arrow": 0xe037, "fullscreen": 0xe5d0,
	"fullscreen_exit": 0xe5d1, "stop": 0xe047, "link": 0xe250,
	"info": 0xe88e, "download": 0xf090, "upload": 0xf09b,
	"more_vert": 0xe5d4, "content_copy": 0xe14d, "open_in_new": 0xe89e,
	"tune": 0xe429, "refresh": 0xe5d5, "search": 0xef7a,
	"speed": 0xe9e4, "graphic_eq": 0xe1b8, "movie_edit": 0xf840,
	"video_file": 0xeb87, "audio_file": 0xeb82, "notes": 0xe26c,
	"keyboard_arrow_down": 0xe313, "cleaning_services": 0xf0ff,
	"sensors": 0xe51e, "inbox": 0xe156, "public": 0xe80b, "radar": 0xf04e,
	"chevron_right": 0xe5cc,
}

var (
	iconFontOnce sync.Once
	iconFontRes  fyne.Resource
)

// iconFont 图标字体(子集 TTF;只含 MS_ICONS 覆盖的码位)
func iconFont() fyne.Resource {
	iconFontOnce.Do(func() {
		iconFontRes = fyne.NewStaticResource("ms-icons.ttf", assets.IconsFont)
	})
	return iconFontRes
}

// msText 单个图标字形
func msText(name string, c color.Color, size float32) *canvas.Text {
	t := canvas.NewText(string(MS_ICONS[name]), c)
	t.FontSource = iconFont()
	t.TextSize = size
	return t
}

// msIcon 图标 CanvasObject(便于替换/居中)
func msIcon(name string, c color.Color, size float32) fyne.CanvasObject {
	return msText(name, c, size)
}

// iconButton 透明底图标按钮(悬停淡底)。默认 31×31(=task-actions .icon-btn,
// 图标 15.5px);顶栏用 36×36/18px(=icon-btn)。默认 text-dim、悬停 text。
type iconButton struct {
	widget.BaseWidget
	icon       string
	boxSize    float32
	glyphSz    float32
	whiteGlyph bool
	danger     bool // 危险图标钮(=Vue dir-item-del/bans-del/nav-clear):悬停时字形与底变红
	onTap      func()
	bg         *canvas.Rectangle
	glyph      *canvas.Text
	hovered    bool
	pal        palette
	tip        string // 悬停提示(=Vue title 属性;fyne 无内建 tooltip)
	tipTimer   *time.Timer
	tipPop     *tipPopW
	hg         *hoverGroup // 挂靠的行组:悬停本钮时保持整行高亮
}

func (b *iconButton) SetHoverGroup(g *hoverGroup) { b.hg = g }

func newIconButton(pal palette, icon string, onTap func()) *iconButton {
	return newIconButtonSZ(pal, icon, 31, 15.5, onTap)
}

func newIconButtonSZ(pal palette, icon string, box, glyph float32, onTap func()) *iconButton {
	b := &iconButton{icon: icon, boxSize: box, glyphSz: glyph, onTap: onTap, pal: pal}
	b.ExtendBaseWidget(b)
	b.bg = canvas.NewRectangle(color.NRGBA{A: 0})
	b.bg.CornerRadius = float32(box / 3.6) // 31→8 / 36→10(=border-radius 8px/10px)
	b.glyph = msText(icon, pal.textDim, glyph)
	return b
}

// newCaretButton 白色图标按钮(=btn-caret:渐变分体钮右半,悬停淡白底;
// 普通 iconButton 悬停提字逻辑在渐变底上不可用)
func newCaretButton(icon string, box, glyph float32, onTap func()) *iconButton {
	b := newIconButtonSZ(palette{}, icon, box, glyph, onTap)
	b.whiteGlyph = true
	b.glyph.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	return b
}

// newDangerIconButtonSZ 危险图标钮:静置与普通图标钮一致(灰字),悬停转红
// (字形 #f87171 + 淡红底)。对应 Vue 的 dir-item-del/bans-del/nav-clear 三类
// 删除/清扫操作钮——用红色把"不可逆/敏感"与普通操作(暂停/继续等)区分开
func newDangerIconButtonSZ(pal palette, icon string, box, glyph float32, onTap func()) *iconButton {
	b := newIconButtonSZ(pal, icon, box, glyph, onTap)
	b.danger = true
	return b
}

func (b *iconButton) hoverFX(on bool) {
	if b.whiteGlyph {
		if on {
			b.bg.FillColor = color.NRGBA{R: 255, G: 255, B: 255, A: 34}
		} else {
			b.bg.FillColor = color.NRGBA{A: 0}
		}
	} else if on {
		if b.danger {
			// 危险钮悬停转红(=nav-clear:hover rgba(248,113,113,.14)+红字):删除/清扫类敏感操作
			b.bg.FillColor = rgbaA(0xf87171, 0.14)
			b.glyph.Color = hexc(0xf87171)
		} else {
			b.bg.FillColor = rgbaA(0x7f7f7f, 0.08)
			b.glyph.Color = b.pal.text
		}
	} else {
		b.bg.FillColor = color.NRGBA{A: 0}
		b.glyph.Color = b.pal.textDim
	}
	b.bg.Refresh()
	b.glyph.Refresh()
}

func (b *iconButton) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: container.NewStack(b.bg, container.NewCenter(b.glyph)), min: fyne.NewSize(b.boxSize, b.boxSize)}
}

func (b *iconButton) Tapped(*fyne.PointEvent) {
	// 点击即取消悬停提示计时:本钮的 onTap(如切换主题)会触发整树重建,本钮被
	// 摘出画布后驱动不再发 MouseOut,hovered 卡在 true;若 500ms 计时器仍到点,
	// 会按重建前的旧色板 + 已失效的绝对坐标,在窗口左上角弹出一个旧色 tip
	// (=用户报的"点击主题钮后左上角出现旧色提示")。先 hideTip 再 onTap 兜住
	b.hideTip()
	if b.onTap != nil {
		b.onTap()
	}
}

// SetTip 挂悬停提示(=Vue title;悬停 500ms 弹出,移开即收)
func (b *iconButton) SetTip(text string) {
	b.tip = text
}

func (b *iconButton) MouseIn(*desktop.MouseEvent) {
	b.hg.enter() // 驱动整行:悬停行内图标钮时行高亮不丢
	b.hovered = true
	b.hoverFX(true)
	if b.tip == "" {
		return
	}
	drv := fyne.CurrentApp().Driver()
	cnv := drv.CanvasForObject(b)
	if cnv == nil {
		return
	}
	b.tipTimer = time.AfterFunc(500*time.Millisecond, func() {
		fyne.Do(func() {
			if !b.hovered {
				return
			}
			pal := b.pal
			lbl := newColorLabelS(b.tip, pal.text, false, 11.5)
			w := lbl.MinSize().Width + 22
			bg := canvas.NewRectangle(pal.panelSolid)
			bg.CornerRadius = 6
			bg.StrokeWidth = 1
			bg.StrokeColor = pal.borderStrong
			body := container.NewStack(bg,
				container.New(layout.NewCustomPaddedLayout(6, 6, 11, 11), lbl))
			// 内容树顶层浮层:widget.PopUp 非模态会吞点外那次点击,overlay
			// 会挡住整窗;floatLayer 的兄弟分支不参与命中拦截
			pop := newTipPop(body)
			abs := drv.AbsolutePositionForObject(b)
			x := abs.X - 8
			if x+w > cnv.Size().Width-8 {
				x = cnv.Size().Width - 8 - w
			}
			y := abs.Y + b.Size().Height + 4
			if y+body.MinSize().Height > cnv.Size().Height-8 {
				y = abs.Y - 8 - body.MinSize().Height
			}
			pop.show(fyne.NewPos(x, y), fyne.NewSize(w, body.MinSize().Height))
			b.tipPop = pop
		})
	})
}

func (b *iconButton) hideTip() {
	if b.tipTimer != nil {
		b.tipTimer.Stop()
		b.tipTimer = nil
	}
	if b.tipPop != nil {
		pop := b.tipPop
		b.tipPop = nil
		pop.remove()
	}
}

func (b *iconButton) MouseOut() {
	b.hg.leave()
	b.hovered = false
	b.hoverFX(false)
	b.hideTip()
}

func (b *iconButton) MouseMoved(*desktop.MouseEvent) {}

// 手形指针(=Vue 全局 button{cursor:pointer}):fyne 默认不替可点控件换指针,
// 经 Cursorable 接口让驱动在鼠标悬停本钮时切到手形
func (b *iconButton) Cursor() desktop.Cursor { return desktop.PointerCursor }

// SetIcon 换图标(播放/暂停切换用)
func (b *iconButton) SetIcon(name string) {
	b.icon = name
	b.glyph.Text = string(MS_ICONS[name])
	b.glyph.Refresh()
}

// flatButton 无底文本按钮(=btn-split 的两个透明子钮:渐变画在父容器,
// 悬停整体提亮由父容器管,这里只做文字+淡底)。padding 9px 18px、13.5px 粗体。
type flatButton struct {
	widget.BaseWidget
	lbl   *canvas.Text
	bg    *canvas.Rectangle
	onTap func()
}

func newFlatButton(pal palette, s string, onTap func()) *flatButton {
	f := &flatButton{onTap: onTap}
	f.ExtendBaseWidget(f)
	f.bg = canvas.NewRectangle(color.NRGBA{A: 0})
	f.bg.CornerRadius = 10
	f.lbl = canvas.NewText(s, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	f.lbl.TextSize = 13.5
	f.lbl.TextStyle = fyne.TextStyle{Bold: true}
	return f
}

func (f *flatButton) CreateRenderer() fyne.WidgetRenderer {
	m := fyne.MeasureText(f.lbl.Text, f.lbl.TextSize, f.lbl.TextStyle)
	return &simpleRenderer{
		obj: container.NewStack(f.bg, container.NewCenter(f.lbl)),
		min: fyne.NewSize(m.Width+36, m.Height+18),
	}
}

func (f *flatButton) Tapped(*fyne.PointEvent) {
	if f.onTap != nil {
		f.onTap()
	}
}

func (f *flatButton) MouseIn(*desktop.MouseEvent) {
	f.bg.FillColor = color.NRGBA{R: 255, G: 255, B: 255, A: 38}
	f.bg.Refresh()
}

func (f *flatButton) MouseOut() {
	f.bg.FillColor = color.NRGBA{A: 0}
	f.bg.Refresh()
}

func (f *flatButton) MouseMoved(*desktop.MouseEvent) {}

// 手形指针(=Vue button{cursor:pointer})
func (f *flatButton) Cursor() desktop.Cursor { return desktop.PointerCursor }

// textBtn 文本按钮基座(ghost/primary/bare 共用):图标+文字水平居中
type textBtn struct {
	widget.BaseWidget
	lbl         *canvas.Text
	icon        *canvas.Text
	bg          *canvas.Rectangle // ghost:描边底;primary:悬停提亮层
	grad        *roundedGrad      // primary 渐变底
	ghost       bool
	bare        bool // 无底无框(=dw-tab 页签文字钮)
	dangerGhost bool // 红色危险 ghost(=Vue .btn-danger:红字,悬停淡红底+红描边)
	mini        bool // 小尺寸(=btn-mini padding 5/12、12px 字号)
	hovered     bool
	pal         palette
	onTap       func()
	hg          *hoverGroup // 挂靠的行组:悬停行内文字钮时保持整行高亮
}

func (b *textBtn) SetHoverGroup(g *hoverGroup) { b.hg = g }

// newGhostButton 描边透明按钮(=btn-ghost:1px border + text-dim,悬停提亮)
func newGhostButton(pal palette, s string, onTap func()) *textBtn {
	return newTextBtn(pal, s, "", true, onTap)
}

// newPrimaryButton 渐变主按钮(=btn-primary:accent-grad 底 + 白字)
func newPrimaryButton(pal palette, s string, onTap func()) *textBtn {
	return newTextBtn(pal, s, "", false, onTap)
}

// newDangerButton 红色渐变危险确认键(=ConfirmBox.vue 确认键:固定红渐变
// #f87171→#ef4444,白字,两主题一致)。敏感操作(删除/取消/清空等)的确认
// 键用它,与蓝色主按钮("新建任务/开始下载")在视觉上区分开
func newDangerButton(pal palette, s string, onTap func()) *textBtn {
	b := newTextBtn(pal, s, "", false, onTap)
	b.grad = newRoundedGrad(hexc(0xf87171), hexc(0xef4444), 10)
	return b
}

// newDangerGhostButton 红色危险 ghost 钮(=ConfirmBox.vue 二级危险键 .btn-danger:
// 恒定红字,静置无边框,悬停淡红底 + 红描边)。用于"清空/删除全部"这类次级敏感键
func newDangerGhostButton(pal palette, s string, onTap func()) *textBtn {
	b := newTextBtn(pal, s, "", true, onTap)
	b.dangerGhost = true
	b.lbl.Color = hexc(0xf87171)
	b.bg.StrokeColor = color.NRGBA{A: 0} // =.btn-danger 的 border:1px solid transparent
	if b.icon != nil {
		b.icon.Color = b.lbl.Color
	}
	return b
}

// newTabButton 页签文字钮(=dw-tab:无底,悬停提字,active 态外部控制颜色)
func newTabButton(pal palette, s string, onTap func()) *textBtn {
	b := newTextBtn(pal, s, "", true, onTap)
	b.bare = true
	b.bg.StrokeWidth = 0
	return b
}

func newTextBtn(pal palette, s, icon string, ghost bool, onTap func()) *textBtn {
	b := &textBtn{pal: pal, ghost: ghost, onTap: onTap}
	b.ExtendBaseWidget(b)
	b.bg = canvas.NewRectangle(color.NRGBA{A: 0})
	b.bg.CornerRadius = 10
	if ghost {
		b.bg.StrokeWidth = 1
		b.bg.StrokeColor = pal.border
	} else {
		// 主按钮渐变走 gradA/gradB(两主题恒定高亮),避免亮色 accent 压暗
		b.grad = newRoundedGrad(pal.gradA, pal.gradB, 10)
	}
	b.lbl = canvas.NewText(s, pal.textDim)
	b.lbl.TextSize = 13.5
	b.lbl.TextStyle = fyne.TextStyle{Bold: true}
	if !ghost {
		b.lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	}
	if icon != "" {
		b.icon = msText(icon, b.lbl.Color, 15)
	}
	return b
}

// SetText 更新按钮文字(如封禁列表计数刷新复用同一按钮实例)
func (b *textBtn) SetText(s string) {
	b.lbl.Text = s
	b.lbl.Refresh()
}

func (b *textBtn) CreateRenderer() fyne.WidgetRenderer {
	inner := container.NewHBox()
	if b.icon != nil {
		inner.Add(b.icon)
		inner.Add(container.New(layout.NewCustomPaddedLayout(0, 0, 3, 0), b.lbl))
	} else {
		inner.Add(b.lbl)
	}
	var stack *fyne.Container
	if b.grad != nil {
		stack = container.NewStack(b.grad, b.bg, container.NewCenter(inner))
	} else {
		stack = container.NewStack(b.bg, container.NewCenter(inner))
	}
	return &simpleRenderer{obj: stack, min: b.lblMin()}
}

func (b *textBtn) lblMin() fyne.Size {
	m := fyne.MeasureText(b.lbl.Text, b.lbl.TextSize, b.lbl.TextStyle)
	px, py := float32(18), float32(9)
	if b.mini {
		px, py = 12, 5
	}
	w := m.Width + px*2
	if b.icon != nil {
		w += 18
	}
	return fyne.NewSize(w, m.Height+py*2)
}

// SetMini 小尺寸按钮(=btn-mini)
func (b *textBtn) SetMini() *textBtn {
	b.mini = true
	b.lbl.TextSize = 12
	return b
}

func (b *textBtn) Tapped(*fyne.PointEvent) {
	if b.onTap != nil {
		b.onTap()
	}
}

func (b *textBtn) MouseIn(*desktop.MouseEvent) {
	b.hg.enter()
	b.hovered = true
	b.applyHover()
}

func (b *textBtn) MouseOut() {
	b.hg.leave()
	b.hovered = false
	b.applyHover()
}

func (b *textBtn) applyHover() {
	p := b.pal
	if b.bare {
		if b.hovered {
			b.lbl.Color = p.text
		} else {
			b.lbl.Color = p.textDim
		}
		b.lbl.Refresh()
		return
	}
	if b.dangerGhost {
		// 红色危险 ghost:字色恒定红(Vue .btn-danger:hover 只变描边与底、不动字色),
		// 静置无边框,悬停加淡红底 + 红描边
		if b.hovered {
			b.bg.FillColor = rgbaA(0xf87171, 0.08)
			b.bg.StrokeColor = rgbaA(0xf87171, 0.4)
		} else {
			b.bg.FillColor = color.NRGBA{A: 0}
			b.bg.StrokeColor = color.NRGBA{A: 0}
		}
		b.bg.Refresh()
		if b.icon != nil {
			b.icon.Color = b.lbl.Color
			b.icon.Refresh()
		}
		b.lbl.Refresh()
		return
	}
	if b.ghost {
		if b.hovered {
			b.bg.FillColor = rgbaA(0x7f7f7f, 0.08)
			b.bg.StrokeColor = p.borderStrong
			b.lbl.Color = p.text
		} else {
			b.bg.FillColor = color.NRGBA{A: 0}
			b.bg.StrokeColor = p.border
			b.lbl.Color = p.textDim
		}
		b.bg.Refresh()
	} else {
		if b.hovered {
			b.bg.FillColor = color.NRGBA{R: 255, G: 255, B: 255, A: 26}
		} else {
			b.bg.FillColor = color.NRGBA{A: 0}
		}
		b.bg.Refresh()
	}
	if b.icon != nil {
		b.icon.Color = b.lbl.Color
		b.icon.Refresh()
	}
	b.lbl.Refresh()
}

func (b *textBtn) MouseMoved(*desktop.MouseEvent) {}

// 手形指针(=Vue button{cursor:pointer}):ghost/primary/danger/tab 文字钮都是可点按钮
func (b *textBtn) Cursor() desktop.Cursor { return desktop.PointerCursor }

// colorLabel 任意色单行文本(fyne v2.8 Label 无 Color 字段)。size>0 时按
// px 直设(CSS 字阶与主题档位对不上);maxW>0 时超宽截断加省略号。
type colorLabel struct {
	widget.BaseWidget
	t    *canvas.Text
	size float32
	maxW float32
	orig string
	// 删除线/下划线以画线实现(canvas.Line 叠加)而非 TextStyle 的
	// Strikethrough/Underline:字体 face 缓存按 TextStyle 全值键控,每个
	// 变体都要整链重解析一遍主题字体+Fyne 内置回退字体(实测 ~19MB/条目),
	// 画线让任务标题的"取消态/悬停态"复用 {Bold} 条目(内存分析结论)
	strike, underline bool
}

func newColorLabel(s string, c color.Color, bold bool) *colorLabel {
	return newColorLabelS(s, c, bold, 0)
}

func newColorLabelS(s string, c color.Color, bold bool, size float32) *colorLabel {
	l := &colorLabel{t: canvas.NewText(s, c), size: size, orig: s}
	if bold {
		l.t.TextStyle = fyne.TextStyle{Bold: true}
	}
	if size > 0 {
		l.t.TextSize = size
	}
	l.ExtendBaseWidget(l)
	return l
}

// SetBold 切换字面(雅黑真粗体=600/700;无 500 面,常规体更接近 500)
func (l *colorLabel) SetBold(b bool) {
	if l.t.TextStyle.Bold == b {
		return // 变更才重绘(同 SetColor:秒级刷新的卡顿源)
	}
	l.t.TextStyle = fyne.TextStyle{Bold: b}
	l.t.Refresh()
}

// SetAlign 水平对齐(=.num 右对齐 / 常规左对齐)
func (l *colorLabel) SetAlign(a fyne.TextAlign) {
	if l.t.Alignment == a {
		return // 变更才重绘(同 SetColor:秒级刷新的卡顿源)
	}
	l.t.Alignment = a
	l.t.Refresh()
}

func (l *colorLabel) CreateRenderer() fyne.WidgetRenderer {
	return &colorLabelRenderer{l: l, strike: canvas.NewLine(l.t.Color), underline: canvas.NewLine(l.t.Color)}
}

// colorLabelRenderer minSize 每帧现算(文本可变;固定 min 会导致 SetText 后
// 布局尺寸不更新),截断模式下横向 min 归零让布局能压缩它
type colorLabelRenderer struct {
	l                 *colorLabel
	strike, underline *canvas.Line // 装饰线:始终在 Objects 里,按开关显隐
}

func (r *colorLabelRenderer) Layout(size fyne.Size) {
	r.l.t.Resize(size)
	if r.l.maxW > 0 {
		r.l.applyTruncate(size.Width)
	}
	r.layoutDecor(size)
}

// layoutDecor 摆放装饰线:横跨文本实宽(截断时取容器宽),删除线过字面中线、
// 下划线贴底。位置跟 Layout/颜色跟文本色,显隐跟开关。
func (r *colorLabelRenderer) layoutDecor(size fyne.Size) {
	on := r.l.strike || r.l.underline
	// 下划线/删除线跨"文字实宽":截断模式下文字放不下才铺满容器宽(含省略号),
	// 放得下时只划文字本身——此前按 maxW>0 恒拉满容器,标题短时下划线一直拖到
	// 卡片右缘,与 Vue 的 .name.clickable:hover(仅划文字)观感不符
	w := r.l.minSize().Width
	if w > size.Width {
		w = size.Width
	}
	r.strike.Hidden = !r.l.strike
	r.underline.Hidden = !r.l.underline
	if !on {
		return
	}
	col := r.l.t.Color
	r.strike.StrokeColor = col
	r.underline.StrokeColor = col
	r.strike.Position1 = fyne.NewPos(0, size.Height*0.52)
	r.strike.Position2 = fyne.NewPos(w, size.Height*0.52)
	r.underline.Position1 = fyne.NewPos(0, size.Height-1.5)
	r.underline.Position2 = fyne.NewPos(w, size.Height-1.5)
	r.strike.Refresh()
	r.underline.Refresh()
}

func (r *colorLabelRenderer) MinSize() fyne.Size {
	if r.l.maxW > 0 {
		return fyne.NewSize(10, fyne.MeasureText("字", r.l.t.TextSize, r.l.t.TextStyle).Height)
	}
	return r.l.minSize()
}
func (r *colorLabelRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.l.t, r.strike, r.underline}
}
func (r *colorLabelRenderer) Destroy() {}
func (r *colorLabelRenderer) Refresh() { r.layoutDecor(r.l.Size()) }

func (l *colorLabel) minSize() fyne.Size {
	return fyne.MeasureText(l.t.Text, l.t.TextSize, l.t.TextStyle)
}

// Layout 截断窗口:父布局每帧给多少宽,超宽即省略号截断(=CSS ellipsis)
func (l *colorLabel) Resize(s fyne.Size) {
	l.BaseWidget.Resize(s)
	if l.maxW > 0 {
		l.applyTruncate(s.Width)
	}
}

func (l *colorLabel) applyTruncate(w float32) {
	// 先判全文:装得下就整串显示。此前每轮候选都强拼省略号比较,等价于
	// 只要在截断模式就恒丢末字加省略号——"剩余时间"(48px)在 56px 列里
	// 被截成"剩余时…",表头/数据一片假截断
	if fyne.MeasureText(l.orig, l.t.TextSize, l.t.TextStyle).Width <= w {
		l.t.Text = l.orig
		return
	}
	// 按 rune 收缩:按字节截会留下半截 UTF-8 序列,渲染成 notdef 方块
	runes := []rune(l.orig)
	for len(runes) > 0 && fyne.MeasureText(string(runes)+"…", l.t.TextSize, l.t.TextStyle).Width > w {
		runes = runes[:len(runes)-1]
	}
	l.t.Text = string(runes) + "…"
}

// FlexWeight 截断标签在 hflex 布局中作弹性子项(撑满剩余宽度)
func (l *colorLabel) FlexWeight() float32 {
	if l.maxW > 0 {
		return 1
	}
	return 0
}

// SetMaxWidth 打开截断(无参=按布局宽度自动)
func (l *colorLabel) SetMaxWidth() *colorLabel { l.maxW = 1; return l }

// IsTruncated 截断模式下当前显示是否被省略号截短(全文放不下的才需要悬停提示)
func (l *colorLabel) IsTruncated() bool {
	return l.maxW > 0 && l.t.Text != l.orig
}

func (l *colorLabel) SetText(s string) {
	if l.orig == s {
		return // 每秒快照同值刷新会整片重绘(hover 闪烁源之一)
	}
	l.orig = s
	l.t.Text = s
	if l.maxW > 0 {
		l.applyTruncate(l.Size().Width)
	}
	// 内层 canvas.Text 必须单独 Refresh:本渲染器的 Refresh() 是空操作(秒级
	// 刷新省重绘),只刷 widget 区不会把新文本画出来——RSS 吸底条"已选 N"
	// 改数后须移鼠标才变即此。与 SetBold/SetAlign/SetStyle 直刷 l.t 一致。
	l.t.Refresh()
	l.Refresh() // 失效 min 尺寸:数字位数变化(如 9→10)时让父布局按新宽重排
}

func (l *colorLabel) Text() string { return l.orig }

func (l *colorLabel) SetColor(c color.Color) {
	// 变更才重绘:秒级刷新逐格无条件 SetColor 会把整表文本纹理全部重生成(卡顿源)
	if sameColor(l.t.Color, c) {
		return
	}
	l.t.Color = c
	l.Refresh()
}

// SetStyle 设字面(粗体/下划线/删除线)。任务标题悬停加下划线、取消态加删除线
// 都走这里;同值守卫避免 hover 进出抖动重绘
func (l *colorLabel) SetStyle(st fyne.TextStyle) {
	if l.t.TextStyle == st {
		return
	}
	l.t.TextStyle = st
	l.t.Refresh()
}

// SetDecor 以 base 样式为底、用画线实现删除线/下划线(见 colorLabel 字段
// 注释:TextStyle 变体每条 ~19MB 字体缓存,画线零新增)。样式或装饰有变才刷新。
func (l *colorLabel) SetDecor(st fyne.TextStyle, strike, underline bool) {
	changedDecor := l.strike != strike || l.underline != underline
	if l.t.TextStyle == st && !changedDecor {
		return
	}
	l.t.TextStyle = st
	l.strike, l.underline = strike, underline
	if changedDecor {
		l.Refresh() // 线的显隐/位置/颜色在 renderer 的 Layout/Refresh 里更新
	} else {
		l.t.Refresh()
	}
}

// stDot 状态圆点(=st-dot:9px 圆点,左 8px=td padding、垂直居中;
// 默认 text-faint,ok/warn/err 三色)
type stDot struct {
	widget.BaseWidget
	circle *canvas.Circle
}

func newStDot() *stDot {
	d := &stDot{}
	d.ExtendBaseWidget(d)
	d.circle = canvas.NewCircle(color.NRGBA{A: 0})
	return d
}

func (d *stDot) setColor(c color.Color) {
	if sameColor(d.circle.FillColor, c) {
		return // 变更才重绘(秒级刷新的卡顿源)
	}
	d.circle.FillColor = c
	d.circle.Refresh()
}

func (d *stDot) CreateRenderer() fyne.WidgetRenderer { return &stDotRenderer{d: d} }

type stDotRenderer struct{ d *stDot }

func (r *stDotRenderer) Layout(size fyne.Size) {
	r.d.circle.Resize(fyne.NewSize(9, 9))
	r.d.circle.Move(fyne.NewPos(8, (size.Height-9)/2))
}

func (r *stDotRenderer) MinSize() fyne.Size           { return fyne.NewSize(25, 9) }
func (r *stDotRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.d.circle} }
func (r *stDotRenderer) Refresh()                     { r.d.circle.Refresh() }
func (r *stDotRenderer) Destroy()                     {}

// badge 语义徽章(=src-badge:11px 文字、padding 1px 6px、radius 6;
// 底=语义色 14~16% 透明、前景=语义色;左 8px、垂直居中、宽度贴文字)
type badge struct {
	widget.BaseWidget
	bg  *canvas.Rectangle
	lbl *colorLabel
}

func newBadge() *badge {
	b := &badge{}
	b.ExtendBaseWidget(b)
	b.bg = canvas.NewRectangle(color.NRGBA{A: 0})
	b.bg.CornerRadius = 6
	b.lbl = newColorLabelS("", color.NRGBA{A: 0}, false, 11)
	return b
}

// set 应用文字与语义色(池化复用每次全量重设;颜色变更才重绘)
func (b *badge) set(text string, fg, bg color.Color) {
	b.lbl.SetText(text)
	b.lbl.SetColor(fg)
	if sameColor(b.bg.FillColor, bg) {
		return
	}
	b.bg.FillColor = bg
	b.bg.Refresh()
}

func (b *badge) CreateRenderer() fyne.WidgetRenderer { return &badgeRenderer{b: b} }

type badgeRenderer struct{ b *badge }

func (r *badgeRenderer) Layout(size fyne.Size) {
	m := r.MinSize()
	y := (size.Height - m.Height) / 2
	r.b.bg.Move(fyne.NewPos(8, y))
	r.b.bg.Resize(m)
	r.b.lbl.Move(fyne.NewPos(8+6, y+1))
	r.b.lbl.Resize(fyne.NewSize(m.Width-12, m.Height-2))
}

func (r *badgeRenderer) MinSize() fyne.Size {
	m := r.b.lbl.MinSize()
	return fyne.NewSize(m.Width+12, m.Height+2)
}

func (r *badgeRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.b.bg, r.b.lbl} }
func (r *badgeRenderer) Refresh()                     {}
func (r *badgeRenderer) Destroy()                     {}

// navRow 侧栏导航行(=nav-item + nav-item-wrap):图标+文本+计数药丸;
// 悬停淡底;激活=横向渐变底(rgba(79,140,255,.16)→rgba(34,211,238,.08))
// + 左侧 3px accent 描边(=nav-slide-ind 承载的视觉)。可清扫行带清扫
// 图标,仅激活时显示(=.nav-item-wrap:has(.active) .nav-clear)。
type navRow struct {
	widget.BaseWidget
	pal      palette
	iconName string
	bg       *canvas.LinearGradient
	hover    *canvas.Rectangle
	leftBar  *canvas.Rectangle
	icon     *canvas.Text
	lbl      *canvas.Text
	cnt      *colorLabel
	cntBg    *canvas.Rectangle
	clear    *iconButton
	active   bool
	hovered  bool
	plain    bool // 激活底交给外层滑动指示条画(侧栏用;设置导航 Vue 只有文字提亮,同样置 plain)
	onTap    func()
	hg       *hoverGroup // 行组:悬停清扫钮等子件时保持整行高亮
}

func newNavRow(pal palette, iconName, label string, onTap func()) *navRow {
	n := &navRow{pal: pal, iconName: iconName, onTap: onTap}
	n.ExtendBaseWidget(n)
	n.bg = canvas.NewLinearGradient(pal.accent, pal.accent2, 0)
	n.bg.StartColor = rgbaA(0x4f8cff, 0.16)
	n.bg.EndColor = rgbaA(0x22d3ee, 0.08)
	n.bg.Hide() // 仅激活显示
	n.hover = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.07))
	n.hover.CornerRadius = 9
	n.hover.Hide()
	n.leftBar = canvas.NewRectangle(pal.accent)
	n.leftBar.SetMinSize(fyne.NewSize(3, 40))
	n.leftBar.Hide()
	n.icon = msText(iconName, pal.textDim, 17)
	n.lbl = canvas.NewText(label, pal.textDim)
	n.lbl.TextSize = 13.5
	n.cnt = newColorLabelS("", pal.textFaint, true, 11)
	n.cntBg = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.12))
	n.cntBg.CornerRadius = 9
	n.cntBg.SetMinSize(fyne.NewSize(24, 17))
	n.cntBg.Hide()
	// 行组:高亮绘制走既有 applyState;清扫钮经 SetClear 挂入本组
	n.hg = newHoverGroup(func(on bool) {
		n.hovered = on
		n.applyState()
	})
	return n
}

// SetClear 挂清扫动作(行内图标,仅激活时显示)
func (n *navRow) SetClear(on func()) {
	// 清扫=删空整个分类,危险操作:用红悬停图标钮(=.nav-clear:hover 变红)
	n.clear = newDangerIconButtonSZ(n.pal, "cleaning_services", 26, 14, on)
	n.clear.SetHoverGroup(n.hg)
}

func (n *navRow) CreateRenderer() fyne.WidgetRenderer {
	// 图标走 Border 槽:右留 5px 与文字间距、底补 5px 透明空间把字形上抬。
	// Material Symbols 字形基线偏低于同尺寸文字,等距 Padded 会让图标视觉偏下;
	// 底部补空间使 HBox 居中后字形上移,与 13.5px 文字基线对齐
	gapR := canvas.NewRectangle(color.NRGBA{A: 0})
	gapR.SetMinSize(fyne.NewSize(5, 1))
	nudge := canvas.NewRectangle(color.NRGBA{A: 0})
	nudge.SetMinSize(fyne.NewSize(1, 5))
	iconSlot := container.NewBorder(nil, nudge, nil, gapR, n.icon)
	inner := container.NewHBox(iconSlot, n.lbl)
	inner.Add(layout.NewSpacer())
	if n.clear != nil {
		inner.Add(n.clear)
	}
	inner.Add(container.NewStack(n.cntBg, container.NewCenter(n.cnt)))
	stack := container.NewStack(n.bg, n.hover, n.leftBar, container.New(layout.NewCustomPaddedLayout(10, 10, 14, 14), inner))
	return &navRowRenderer{n: n, stack: stack}
}

type navRowRenderer struct {
	n     *navRow
	stack *fyne.Container
}

func (r *navRowRenderer) Layout(size fyne.Size) {
	r.stack.Resize(size)
	// 左指示条是贴行左缘的 3px 通高竖条;Stack 布局会把子项拉到满宽,
	// 必须在布局后收回 3px,否则激活时图标后面出现整块 accent 色方块
	r.n.leftBar.Move(fyne.NewPos(0, 0))
	r.n.leftBar.Resize(fyne.NewSize(3, size.Height))
}

func (r *navRowRenderer) MinSize() fyne.Size { return fyne.NewSize(166, 41) }
func (r *navRowRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.stack}
}
func (r *navRowRenderer) Destroy() {}
func (r *navRowRenderer) Refresh() {}

func (n *navRow) Tapped(*fyne.PointEvent) {
	if n.onTap != nil {
		n.onTap()
	}
}

func (n *navRow) MouseIn(*desktop.MouseEvent) {
	n.hg.enter()
}

func (n *navRow) MouseOut() {
	n.hg.leave()
}

// 手形指针:侧栏分类行可点切换视图(=Vue .nav-item 为可点按钮)
func (n *navRow) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (n *navRow) MouseMoved(*desktop.MouseEvent) {}

// SetActive 激活态(渐变底 + 左描边 + 文字提亮;同值早退防每拍重绘)
func (n *navRow) SetActive(on bool) {
	if n.active == on {
		return
	}
	n.active = on
	n.applyState()
}

// SetCount 计数(空串隐藏徽章,RSS 未读 0 时整枚隐藏=SideBar.vue rssBadge)
func (n *navRow) SetCount(s string) {
	n.cnt.SetText(s)
	if s == "" {
		n.cntBg.Hide()
	} else {
		n.cntBg.Show()
	}
	n.cntBg.Refresh()
	n.cnt.Refresh()
}

func (n *navRow) applyState() {
	p := n.pal
	switch {
	case n.active:
		if n.plain {
			// 侧栏:底由滑动指示条统一画;设置导航(Vue 无选中底)按用户
			// 要求复用 hover 底色给选中态一个反馈
			n.bg.Hide()
			n.leftBar.Hide()
			n.hover.Show()
		} else {
			n.bg.Show()
			n.leftBar.Show()
			n.hover.Hide()
		}
		n.icon.Color = p.text
		n.lbl.Color = p.text
	case n.hovered:
		n.bg.Hide()
		n.hover.Show()
		n.leftBar.Hide()
		n.icon.Color = p.text
		n.lbl.Color = p.text
	default:
		n.bg.Hide()
		n.hover.Hide()
		n.leftBar.Hide()
		n.icon.Color = p.textDim
		n.lbl.Color = p.textDim
	}
	// 清扫图标仅激活行显示(=nav-clear 的 :has 选择器规则)
	if n.clear != nil {
		if n.active {
			n.clear.Show()
		} else {
			n.clear.Hide()
		}
	}
	n.bg.Refresh()
	n.hover.Refresh()
	n.leftBar.Refresh()
	n.icon.Refresh()
	n.lbl.Refresh()
}

// floatLayer 内容树顶层浮层(toast/tooltip/拖影)。绝不能用 overlay:
// fyne 命中分发只遍历 Overlays().Top() 一棵树,任何 overlay(哪怕透明)
// 都会让整窗内容不可达;内容树的全局命中搜索会穿过无命中的兄弟分支
var floatLayer *fyne.Container

// modalLayer 内容树模态层(主 UI 之上、floatLayer 之下)。模态放 overlay
// 会画在整个内容树上方,弹窗期间 toast(浮层)被遮罩盖没——Vue 里 toast
// 恒在 dialog 之上。层内布局用 fullLayout 拉伸可见模态到全画布
var modalLayer *fyne.Container

// tipPopW 纯展示浮层面板句柄(=tooltip):面板放进 floatLayer,点击与
// 按键全部穿透到下层控件
type tipPopW struct {
	panel fyne.CanvasObject
}

func newTipPop(panel fyne.CanvasObject) *tipPopW {
	return &tipPopW{panel: panel}
}

func (t *tipPopW) CreateRenderer() fyne.WidgetRenderer {
	box := container.NewWithoutLayout()
	box.Add(t.panel)
	return &simpleRenderer{obj: box, min: fyne.NewSize(1, 1)}
}

// show 挂内容树顶层浮层并摆到目标位置定尺寸
func (t *tipPopW) show(pos fyne.Position, size fyne.Size) {
	if floatLayer == nil {
		return
	}
	floatLayer.Add(t.panel)
	t.place(pos, size)
}

func (t *tipPopW) place(pos fyne.Position, size fyne.Size) {
	t.panel.Move(pos)
	t.panel.Resize(size)
	floatLayer.Refresh()
}

// move 拖拽 ghost 跟随光标
func (t *tipPopW) move(pos fyne.Position) {
	t.panel.Move(pos)
	floatLayer.Refresh()
}

// remove 从浮层摘除
func (t *tipPopW) remove() {
	if floatLayer != nil {
		floatLayer.Remove(t.panel)
		floatLayer.Refresh()
	}
}

// curPal 当前色板(经主题对象取,独立于 app 实例;供顶层浮层/独立控件取色)
func curPal() palette {
	if zt, ok := fyne.CurrentApp().Settings().Theme().(*zTheme); ok {
		return zt.palv
	}
	return newPalette(true)
}

// showLabelTip 在目标对象下方弹出一段文本的悬停提示(=Vue :title;fyne 无内建)。
// 越界翻到上方、收进左右边缘。返回句柄,调用方 MouseOut 时 remove
func showLabelTip(target fyne.CanvasObject, text string) *tipPopW {
	if text == "" {
		return nil
	}
	drv := fyne.CurrentApp().Driver()
	cnv := drv.CanvasForObject(target)
	if cnv == nil {
		return nil
	}
	pal := curPal()
	lbl := newColorLabelS(text, pal.text, false, 11.5)
	w := lbl.MinSize().Width + 22
	if w > cnv.Size().Width-16 {
		w = cnv.Size().Width - 16 // 超长限宽,防提示溢出窗口
	}
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 6
	bg.StrokeWidth = 1
	bg.StrokeColor = pal.borderStrong
	body := container.NewStack(bg, container.New(layout.NewCustomPaddedLayout(6, 6, 11, 11), lbl))
	pop := newTipPop(body)
	abs := drv.AbsolutePositionForObject(target)
	x := abs.X
	if x+w > cnv.Size().Width-8 {
		x = cnv.Size().Width - 8 - w
	}
	if x < 8 {
		x = 8
	}
	y := abs.Y + target.Size().Height + 4
	if y+body.MinSize().Height > cnv.Size().Height-8 {
		y = abs.Y - 8 - body.MinSize().Height
	}
	if y < 8 {
		y = 8
	}
	pop.show(fyne.NewPos(x, y), fyne.NewSize(w, body.MinSize().Height))
	return pop
}

// hoverGroup 把"整行悬停"表达为"指针落在组内任一成员",根治 Fyne 命中取"最深
// Hoverable"导致的行高亮闪烁:驱动只维护单一 mouseOver,指针从行本体移到行内
// label/按钮子件时会先发行的 MouseOut、再发子件的 MouseIn,各行/子件各自维护
// hover 态就会闪烁丢失。行(容器)与行内所有 hoverable 子件共享同一计数器:
// 驱动在同一帧内成对地发 leave(旧)→enter(新),计数 0↔1 抵消,最终态正确。
type hoverGroup struct {
	depth int
	paint func(bool) // 整组统一高亮绘制(行背景)
}

func newHoverGroup(paint func(bool)) *hoverGroup {
	return &hoverGroup{paint: paint}
}

// enter 组内任一成员收到 MouseIn(指针进入该行范围,含行本体或任一子件)
func (g *hoverGroup) enter() {
	if g == nil {
		return
	}
	g.depth++
	if g.depth == 1 {
		g.paint(true)
	}
}

// leave 组内任一成员收到 MouseOut(移出整行 / 移到另一成员,与 enter 成对抵消)
func (g *hoverGroup) leave() {
	if g == nil {
		return
	}
	if g.depth > 0 {
		g.depth--
	}
	if g.depth == 0 {
		g.paint(false)
	}
}

// groupAware 可挂靠 hoverGroup 的子件:被行登记后,自身 MouseIn/Out 顺带驱动整行
type groupAware interface {
	SetHoverGroup(*hoverGroup)
}

// wrappedObj 单对象包装器(flexWrap/fixedMin/tapWrap):它们把子件包在
// BaseWidget 里,不是 *fyne.Container,仅下钻容器会漏掉其内部的 group-aware 件
// (如 flex(newTipLabel(...)))。据此接口下钻到被包裹内容
type wrappedObj interface {
	WrapObject() fyne.CanvasObject
}

// registerHoverGroup 递归登记内容树里所有 groupAware 子件到组:下钻容器与
// 单对象包装器;group-aware 子件本身是叶子,不再向其内部渲染对象递归
func registerHoverGroup(root fyne.CanvasObject, g *hoverGroup) {
	if g == nil || root == nil {
		return
	}
	if ga, ok := root.(groupAware); ok {
		ga.SetHoverGroup(g)
	}
	if c, ok := root.(*fyne.Container); ok {
		for _, ch := range c.Objects {
			registerHoverGroup(ch, g)
		}
	}
	if w, ok := root.(wrappedObj); ok {
		registerHoverGroup(w.WrapObject(), g)
	}
}

// tipLabel 带悬停提示的截断文本(=Vue :title):仅当文本被省略号截断时,悬停
// 500ms 弹出全文;未截断不弹,避免短名刷屏。FlexWeight 透传,包进 hFlex 时不
// 破坏原弹性截断布局
type tipLabel struct {
	widget.BaseWidget
	lbl      *colorLabel
	full     func() string
	tipTimer *time.Timer
	tipPop   *tipPopW
	hg       *hoverGroup // 挂靠的行组:悬停本 label 时保持整行高亮
}

func newTipLabel(lbl *colorLabel, full func() string) *tipLabel {
	w := &tipLabel{lbl: lbl, full: full}
	w.ExtendBaseWidget(w)
	return w
}

func (w *tipLabel) SetHoverGroup(g *hoverGroup) { w.hg = g }

// FlexWeight 透传内部 colorLabel 的弹性权重(截断标签=1),hFlex 里仍是弹性子项
func (w *tipLabel) FlexWeight() float32 { return w.lbl.FlexWeight() }

func (w *tipLabel) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(w.lbl)
}

func (w *tipLabel) MouseIn(*desktop.MouseEvent) {
	w.hg.enter() // 先驱动整行(未截断同样要保行高亮,不能因不提示而漏)
	if w.full == nil || !w.lbl.IsTruncated() {
		return // 未截断不提示
	}
	w.tipTimer = time.AfterFunc(500*time.Millisecond, func() {
		fyne.Do(func() {
			if w.tipPop != nil {
				return
			}
			if txt := w.full(); txt != "" {
				w.tipPop = showLabelTip(w, txt)
			}
		})
	})
}

func (w *tipLabel) MouseOut() {
	w.hg.leave()
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

func (w *tipLabel) MouseMoved(*desktop.MouseEvent) {}
