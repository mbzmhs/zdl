package nativeui

// 主题:style.css 设计变量逐值移植(:root 暗色 + [data-theme="light"])。
// pal() 是全包取色口——状态胶囊/图标渐变/进度条等都从这走,与 CSS 保持
// 单一事实源;切主题=换 theme 对象并全局 SetTheme。

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"zdownloader/internal/assets"
)

// hexc 0xRRGGBB → color
func hexc(v uint32) color.Color {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// rgbaA 带透明度(RGB 常量 + alpha 0~1)
func rgbaA(rgb uint32, a float64) color.Color {
	return color.NRGBA{R: uint8(rgb >> 16), G: uint8(rgb >> 8), B: uint8(rgb), A: uint8(a * 255)}
}

// withAlpha 换 alpha(color.Color → NRGBA;右键菜单悬停等半透明叠色用)
func withAlpha(c color.Color, a float64) color.Color {
	r, g, b, _ := c.RGBA()
	return color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a * 255)}
}

// palette 语义色板(字段名即 CSS 变量名)
type palette struct {
	bg, bgSoft, panel, panelSolid color.Color
	border, borderStrong          color.Color
	text, textDim, textFaint      color.Color
	accent, accent2               color.Color
	gradA, gradB                  color.Color // 主按钮渐变(两主题恒定高亮,亮色不压暗)
	ok, warn, errc, purple        color.Color
	hover, selection              color.Color // 高频取色:预建避免每帧装箱分配
	dark                          bool
}

// newPalette 逐值对照 :root 与 [data-theme="light"]
func newPalette(dark bool) palette {
	if dark {
		return palette{
			bg: hexc(0x0d1117), bgSoft: hexc(0x131a23), panel: rgbaA(0x161d27, 0.9), panelSolid: hexc(0x161d27),
			border: rgbaA(0xffffff, 0.10), borderStrong: rgbaA(0xffffff, 0.18),
			text: hexc(0xe8edf4), textDim: hexc(0x98a4b3), textFaint: hexc(0x5d6875),
			accent: hexc(0x4f8cff), accent2: hexc(0x22d3ee),
			gradA: hexc(0x4f8cff), gradB: hexc(0x22d3ee),
			ok: hexc(0x34d399), warn: hexc(0xfbbf24), errc: hexc(0xf87171), purple: hexc(0xa78bfa),
			hover: rgbaA(0x7f7f7f, 0.10), selection: rgbaA(0x4f8cff, 0.25),
			dark: true,
		}
	}
	return palette{
		bg: hexc(0xf2f5f9), bgSoft: hexc(0xe9eef5), panel: rgbaA(0xffffff, 0.95), panelSolid: hexc(0xffffff),
		border: rgbaA(0x0f172a, 0.11), borderStrong: rgbaA(0x0f172a, 0.20),
		text: hexc(0x17202b), textDim: hexc(0x5a6675), textFaint: hexc(0x94a0ae),
		accent: hexc(0x3b6ee8), accent2: hexc(0x0891b2),
		// 主按钮渐变刻意沿用暗色高亮值:亮色下 0x3b6ee8/0x0891b2 观感偏暗,
		// 用户要求主按钮两主题观感一致
		gradA: hexc(0x4f8cff), gradB: hexc(0x22d3ee),
		ok: hexc(0x0f9d6c), warn: hexc(0xd9970c), errc: hexc(0xdc4b4b), purple: hexc(0x8b6ff0),
		hover: rgbaA(0x7f7f7f, 0.10), selection: rgbaA(0x3b6ee8, 0.25),
		dark: false,
	}
}

// softA bg-soft 的半透明版(=CSS color-mix(in srgb, var(--bg-soft) N%,
// transparent);侧栏/详情表格底色随主题走,不能硬编码深夜色)
func (p palette) softA(a float32) color.Color {
	if n, ok := p.bgSoft.(color.NRGBA); ok {
		return color.NRGBA{R: n.R, G: n.G, B: n.B, A: uint8(float32(n.A) * a)}
	}
	return p.bgSoft
}

// statusChip 状态胶囊配色(=status-chip.*)
func (p palette) statusChip(s engineStatus) (fg, bg color.Color) {
	switch s {
	case engineStatusRunning:
		return p.accent2, rgbaA(0x22d3ee, 0.12)
	case engineStatusWaiting:
		return p.textDim, rgbaA(0x7f7f7f, 0.12)
	case engineStatusPaused:
		return p.warn, rgbaA(0xfbbf24, 0.12)
	case engineStatusCompleted:
		return p.ok, rgbaA(0x34d399, 0.12)
	case engineStatusSeeding:
		return p.purple, rgbaA(0xa78bfa, 0.14)
	case engineStatusChoosing:
		return p.accent2, rgbaA(0x22d3ee, 0.12)
	case engineStatusError:
		return p.errc, rgbaA(0xf87171, 0.13)
	default: // canceled
		return p.textFaint, rgbaA(0x7f7f7f, 0.12)
	}
}

// iconGrad 文件图标块渐变(=file-icon.* 两端色)
func (p palette) iconGrad(cls string) (c1, c2 color.Color) {
	switch cls {
	case "hls":
		return hexc(0xa78bfa), hexc(0xf472b6)
	case "video":
		return hexc(0xf59e0b), hexc(0xf43f5e)
	case "audio":
		return hexc(0x34d399), hexc(0x22d3ee)
	case "img":
		return hexc(0xa78bfa), hexc(0x818cf8)
	case "doc":
		return hexc(0x60a5fa), hexc(0x6366f1)
	case "archive":
		return hexc(0xfbbf24), hexc(0xf59e0b)
	default:
		return hexc(0x64748b), hexc(0x475569)
	}
}

// zTheme fyne 主题实现
type zTheme struct {
	fyne.Theme
	dark           bool
	font, fontBold fyne.Resource
	palv           palette
}

func newTheme(dark bool) *zTheme {
	t := &zTheme{Theme: theme.DefaultTheme(), dark: dark, palv: newPalette(dark)}
	t.font, t.fontBold = loadUIFont()
	return t
}

// Pal 组件取色口(App 持有当前色板)
func (t *zTheme) Pal() palette { return t.palv }

func (t *zTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	p := t.palv
	switch name {
	case theme.ColorNameBackground:
		return p.bg
	case theme.ColorNameForeground:
		return p.text
	case theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return p.panelSolid
	case theme.ColorNameInputBackground:
		return p.bgSoft
	case theme.ColorNameInputBorder:
		// 输入框描边 =.field input border var(--border);聚焦态走
		// ColorNamePrimary(=accent),与 Vue :focus 一致
		return p.border
	case theme.ColorNamePrimary, theme.ColorNameFocus:
		return p.accent
	case theme.ColorNameHover:
		return p.hover
	case theme.ColorNameSelection:
		return p.selection
	case theme.ColorNameSeparator:
		// 原版明细表只有行底线(自绘),widget.Table 的网格线全部隐藏
		return color.NRGBA{A: 0}
	case theme.ColorNameScrollBar:
		// 滚动条拇指 =::-webkit-scrollbar-thumb var(--border-strong)
		return p.borderStrong
	case theme.ColorNameScrollBarBackground:
		return color.NRGBA{A: 0} // 轨道透明(=scrollbar-track transparent)
	case theme.ColorNameButton:
		return p.borderStrong
	case theme.ColorNamePlaceHolder:
		return p.textFaint
	case theme.ColorNameDisabled, theme.ColorNameDisabledButton:
		return p.textFaint
	case theme.ColorNameShadow:
		// 投影全局透明:Scroll 四边黑影(fyne SubmergedContentLevel)随之消失;
		// 模态遮罩改由 maskModal 显式取色。字体缓存按 ThemeOverride scope
		// 键控,任何 ThemeOverride 都会复制整套字体 Face+表缓冲(实测每开
		// 一个弹窗 +3~4MB 永不回收)——这是去掉所有局部主题的根因
		return color.NRGBA{A: 0}
	case theme.ColorNameSuccess:
		return p.ok
	case theme.ColorNameWarning:
		return p.warn
	case theme.ColorNameError:
		return p.errc
	}
	v := theme.VariantDark
	if !t.dark {
		v = theme.VariantLight
	}
	return t.Theme.Color(name, v)
}

func (t *zTheme) Font(style fyne.TextStyle) fyne.Resource {
	if style.Bold && t.fontBold != nil {
		return t.fontBold // 雅黑真粗体(=font-weight:600/700 的字重)
	}
	if t.font != nil {
		return t.font
	}
	return t.Theme.Font(style)
}

// Size 紧凑密度(对齐原版 12~14.5px 字阶)
func (t *zTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 13.5
	case theme.SizeNameHeadingText:
		return 17
	case theme.SizeNameSubHeadingText:
		return 14
	case theme.SizeNameCaptionText:
		return 11
	case theme.SizeNamePadding:
		return 4
	case theme.SizeNameInnerPadding:
		return 10 // =Vue .field input 9px 12px 取中;全局提升免 per-pane 主题
	case theme.SizeNameLineSpacing:
		return 3
	case theme.SizeNameScrollBar:
		return 8
	case theme.SizeNameScrollBarRadius:
		return 4 // 8px 条取半径 4 = Vue 9px 条 radius 8 的胶囊形
	case theme.SizeNameInputRadius:
		return 9 // 输入类圆角对齐 radius-sm(Vue 输入框与下拉同为 9px)
	case theme.SizeNameSeparatorThickness:
		return 1
	}
	return t.Theme.Size(name)
}

// loadUIFont 内嵌雅黑子集(GB2312 常用字+假名;生成脚本 tools/genfonts.py)。
// 不再读系统字体:子集跨平台一致,且 simhei 无粗体面导致所有加粗文本失效。
func loadUIFont() (reg, bold fyne.Resource) {
	if len(assets.UISans) > 0 {
		reg = fyne.NewStaticResource("msyh-regular.ttf", assets.UISans)
	}
	if len(assets.UIBold) > 0 {
		bold = fyne.NewStaticResource("msyh-bold.ttf", assets.UIBold)
	}
	return reg, bold
}
