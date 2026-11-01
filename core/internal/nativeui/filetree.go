package nativeui

// BitComet 式文件树(lib/filetree.js 移植):目录聚合 + 填充文件识别 + 选择集。
// 纯函数与 widget 同文件——树结构只服务本包的对话框/详情页。

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// paddingFileRe BitComet 0.85+ 制种的 ____padding_file_N_ 填充文件
// (BEP-47 形态是 .pad/<字节数>;属种子正常成员但展示无意义——引擎对齐
// 段落读零写丢,盘上不会出现这些文件)
var paddingFileRe = regexp.MustCompile(`^_{2,}padding_file_\d+_`)

// ftFile 文件行统一形态(parse 端点的 length 与 detail 端点的 size 在此归一)
type ftFile struct {
	Path   []string
	Size   int64
	Done   int64
	First  int
	Last   int
	Wanted bool
}

// FtIsPadding 填充文件识别(=filetree.js isPaddingFile:末段 padding 命名,
// 或路径含 .pad 目录)
func FtIsPadding(f *ftFile) bool {
	n := len(f.Path)
	if n > 0 && paddingFileRe.MatchString(f.Path[n-1]) {
		return true
	}
	for _, seg := range f.Path {
		if seg == ".pad" {
			return true
		}
	}
	return false
}

// ftIsMedia 可边下边播的媒体扩展名(=filetree.js MEDIA_EXTS,与引擎口径一致并放宽)
func ftIsMedia(f *ftFile) bool {
	n := len(f.Path)
	if n == 0 {
		return false
	}
	parts := strings.Split(f.Path[n-1], ".")
	ext := strings.ToLower(parts[len(parts)-1])
	switch ext {
	case "mp4", "mkv", "avi", "mov", "wmv", "flv", "ts", "m2ts", "webm", "m4v",
		"rmvb", "mpg", "mpeg", "3gp", "vob", "mp3", "flac", "m4a", "aac", "wav", "ogg", "opus":
		return true
	}
	return false
}

// ftNode 目录树节点(=buildFileTree:按相对路径逐段聚合)
type ftNode struct {
	Name  string
	Key   string // 目录全相对路径(选择集/折叠集键)
	Dirs  []*ftNode
	Files []*ftFile
}

// ftBuild 构建目录树
func ftBuild(files []*ftFile) *ftNode {
	root := &ftNode{Name: "", Key: "", Dirs: []*ftNode{}}
	for _, f := range files {
		node := root
		acc := ""
		for i := 0; i < len(f.Path)-1; i++ {
			if acc == "" {
				acc = f.Path[i]
			} else {
				acc = acc + "/" + f.Path[i]
			}
			var ch *ftNode
			for _, d := range node.Dirs {
				if d.Key == acc {
					ch = d
					break
				}
			}
			if ch == nil {
				ch = &ftNode{Name: f.Path[i], Key: acc, Dirs: []*ftNode{}}
				node.Dirs = append(node.Dirs, ch)
			}
			node = ch
		}
		node.Files = append(node.Files, f)
	}
	// 目录序与文件序稳定(名字典序;Vue 端由对象键序隐式保证)
	var sortNode func(n *ftNode)
	sortNode = func(n *ftNode) {
		sort.Slice(n.Dirs, func(i, j int) bool { return n.Dirs[i].Name < n.Dirs[j].Name })
		sort.Slice(n.Files, func(i, j int) bool {
			return n.Files[i].Path[len(n.Files[i].Path)-1] < n.Files[j].Path[len(n.Files[j].Path)-1]
		})
		for _, d := range n.Dirs {
			sortNode(d)
		}
	}
	sortNode(root)
	return root
}

// ftAgg 目录聚合(=filetree.js ftAgg:大小/完成字节/文件数/piece 区间;
// wanted 推导:全未选=false / 混合=undefined / 全选=true)
type ftAggResult struct {
	Done   int64
	Size   int64
	Count  int
	First  int
	Last   int
	Wanted *bool // nil=混合
}

func ftAgg(n *ftNode) ftAggResult {
	var a ftAggResult
	a.First = -1
	a.Last = -1
	wAny, wAll := false, true
	var walk func(x *ftNode)
	walk = func(x *ftNode) {
		for _, f := range x.Files {
			a.Done += f.Done
			a.Size += f.Size
			a.Count++
			if f.Last >= f.First {
				if a.First < 0 || f.First < a.First {
					a.First = f.First
				}
				if f.Last > a.Last {
					a.Last = f.Last
				}
			}
			if !f.Wanted {
				wAll = false
			} else {
				wAny = true
			}
		}
		for _, d := range x.Dirs {
			walk(d)
		}
	}
	walk(n)
	if a.First < 0 {
		a.First = 0
	}
	switch {
	case wAll && wAny:
		t := true
		a.Wanted = &t
	case wAny:
		a.Wanted = nil
	default:
		f := false
		a.Wanted = &f
	}
	return a
}

// ftPieceText 分块序号列文本(=filetree.js ftPieceText)
func ftPieceText(first, last int) string {
	if !(last >= first) {
		return "—" // 零长文件/空目录
	}
	if first == last {
		return fmt.Sprint(first)
	}
	return fmt.Sprintf("%d~%d", first, last)
}

/* ---------------- 选择模式文件树 widget ---------------- */

// ftTree 选择模式文件树(=FileTree.vue mode="sel"):复选框 + 目录折叠 +
// 目录勾选由后代推导。行容器 VBox;大种子默认折叠顶层目录(>60 文件,
// 同 Vue)。详情页的 view 模式(进度/优先级列/播放按钮)在 M5 扩展。
type ftTree struct {
	widget.BaseWidget
	app       *App
	files     []*ftFile
	sel       map[string]bool // key = 相对路径
	collapsed map[string]bool
	box       *fyne.Container
	headBox   *fyne.Container // 表头(固定在滚动区外=sticky)
	onChange  func()
	// view 模式(详情页):无勾选框(editable 时有),多进度/优先级列,
	// 媒体文件带播放按钮;sel 模式(创建对话框)见 newFtTree
	view       bool
	editable   bool
	taskStatus engineStatus
	playFn     func(path string)
	// minTreeH 树最小高度下限(默认 300);创建对话框按文件数适度调高,
	// 让多文件种子一次展示更多条目,而不是都挤进 300px 靠内部滚动
	minTreeH float32
	// 增量刷新状态:行控件按 key 缓存,轮询只原位更新文本/勾选——
	// 重建行会丢悬停高亮并重置勾选焦点,百行级种子每秒重建也是纯开销
	dirtyPtr *bool               // 用户勾选脏标记(drawer 持有,构造传入)
	ui       map[string]*ftRowUI // 行控件缓存(键=ftRowSpec.key)
	rowOrder []string            // 当前可见行序(变化才重排 box)
	uiWithCb bool                // 缓存构建时的勾选列形态(变化作废缓存)
	dirNodes map[string]*ftNode  // 目录 key→节点(回调只能持 key,节点每拍重建)
}

func newFtTree(app *App, files []*ftFile, onChange func()) *ftTree {
	t := &ftTree{app: app, files: files, sel: map[string]bool{}, collapsed: map[string]bool{}, onChange: onChange, minTreeH: 300}
	t.ExtendBaseWidget(t)
	// 初始:全部选中;大种子折叠顶层目录
	for _, f := range files {
		t.sel[strings.Join(f.Path, "/")] = true
	}
	if len(files) > 60 {
		seen := map[string]bool{}
		for _, f := range files {
			if !seen[f.Path[0]] {
				seen[f.Path[0]] = true
				t.collapsed[f.Path[0]] = true
			}
		}
	}
	t.box = container.NewVBox()
	t.headBox = container.NewVBox()
	return t
}

func (t *ftTree) CreateRenderer() fyne.WidgetRenderer {
	t.rebuild()
	// 表头固定在滚动区外(=ft-head position:sticky);滚动区去边缘黑影
	return &simpleRenderer{obj: container.NewBorder(t.headBox, nil, nil, nil,
		container.NewVScroll(t.box)),
		min: fyne.NewSize(560, t.minTreeH)}
}

// ftRowSpec 一行的展示数据(rebuild 从树派生;ftRowUI.update 消费)。
// key 加 "D:"/"F:" 前缀:目录 key 与顶层文件名可能撞车,缓存键必须唯一
type ftRowSpec struct {
	key       string
	ref       string // 原始键(目录 key / 文件路径,回调用)
	isDir     bool
	depth     int
	collapsed bool
	checked   bool
	mixed     bool
	name      string
	meta      string
	sizeTxt   string
	pct       float64
	prioTxt   string
	pieceTxt  string
	playable  bool
	onCheck   func(bool)
}

// ftRowUI 行控件缓存:轮询增量刷新只改文本/勾选态,不重建行——重建会丢
// 行悬停高亮并重置勾选焦点,百行级种子每秒重建也是纯 CPU 开销
type ftRowUI struct {
	wrap  *ftRowWrap
	cb    *styledCheck
	caret *canvas.Text // 目录折叠箭头(换字形)
	name  *colorLabel
	meta  *colorLabel // 目录 "N 项"
	size  *colorLabel
	prog  *miniProg
	pctL  *colorLabel
	prio  *colorLabel
	piece *colorLabel
	play  *iconButton
}

// rebuild 重建/刷新行。行网格逐列对齐 .ftable.view.editable
// (20|1fr|90|120|64|82、gap 10、padding 4px 10px、min 高 29);表头/进度条/
// 优先级配色/图标/播放钮同 Vue。结构(行集/勾选列)变化才重排容器,数据
// 变化(进度/勾选态)一律原位更新
func (t *ftTree) rebuild() {
	pal := t.app.store.appPal()
	withCb := !t.view || t.editable
	if t.ui == nil || t.uiWithCb != withCb {
		// 勾选列出现/消失会改变行内部结构,行缓存整体作废
		t.ui = map[string]*ftRowUI{}
		t.rowOrder = nil
		t.headBox.Objects = nil
		t.uiWithCb = withCb
	}
	if len(t.headBox.Objects) == 0 {
		t.buildHead(pal)
	}
	tree := ftBuild(t.files)
	t.dirNodes = map[string]*ftNode{}
	order := make([]string, 0, 32)
	objs := make([]fyne.CanvasObject, 0, 32)
	relayout := false
	add := func(s ftRowSpec) {
		u := t.ui[s.key]
		if u == nil {
			u = t.newRowUI(s, pal)
			t.ui[s.key] = u
			relayout = true
		}
		if u.update(s, pal) {
			relayout = true
		}
		order = append(order, s.key)
		objs = append(objs, u.wrap)
	}
	var walk func(n *ftNode, depth int)
	walk = func(n *ftNode, depth int) {
		for _, dir := range n.Dirs {
			d := dir
			t.dirNodes[d.Key] = d
			agg := ftAgg(d)
			files := t.allFilesUnder(d)
			nFiles, nOn := 0, 0
			for _, f := range files {
				nFiles++
				if t.sel[strings.Join(f.Path, "/")] {
					nOn++
				}
			}
			collapsed := t.collapsed[d.Key]
			pct := 100.0
			if agg.Size > 0 {
				pct = minFloat(100, float64(agg.Done)/float64(agg.Size)*100)
			}
			dKey := d.Key
			add(ftRowSpec{
				key: "D:" + dKey, ref: dKey, isDir: true, depth: depth,
				collapsed: collapsed,
				checked:   nFiles > 0 && nOn == nFiles,
				mixed:     nOn > 0 && nOn < nFiles,
				name:      d.Name,
				meta:      fmt.Sprintf("%d 项", agg.Count),
				sizeTxt:   fmtBytes(agg.Size),
				pct:       pct,
				prioTxt:   ftPrioTxt(agg.Done, agg.Size, agg.Wanted, t.taskStatus == engineStatusRunning),
				pieceTxt:  ftPieceText(agg.First, agg.Last),
				onCheck:   func(on bool) { t.checkDirKey(dKey, on) },
			})
			if !collapsed {
				walk(d, depth+1)
			}
		}
		for _, f := range n.Files {
			fkey := strings.Join(f.Path, "/")
			fname := f.Path[len(f.Path)-1]
			pct := 100.0
			if f.Size > 0 {
				pct = minFloat(100, float64(f.Done)/float64(f.Size)*100)
			}
			wanted := f.Wanted
			add(ftRowSpec{
				key: "F:" + fkey, ref: fkey, depth: depth,
				checked:  t.sel[fkey],
				name:     fname,
				sizeTxt:  fmtBytes(f.Size),
				pct:      pct,
				prioTxt:  ftPrioTxt(f.Done, f.Size, &wanted, t.taskStatus == engineStatusRunning),
				pieceTxt: ftPieceText(f.First, f.Last),
				playable: t.view && ftIsMedia(f) && f.Wanted && t.taskStatus != engineStatusCanceled,
				onCheck: func(on bool) {
					t.sel[fkey] = on
					if t.onChange != nil {
						t.onChange()
					}
					t.rebuild()
				},
			})
		}
	}
	walk(tree, 0)

	if relayout || !equalStrings(order, t.rowOrder) {
		t.box.Objects = objs
		t.box.Refresh()
		t.rowOrder = order
	}
}

// buildHead 表头(=ft-head:11px 粗体 text-faint、灰底、下边线;文件名列左对齐)
func (t *ftTree) buildHead(pal palette) {
	head := []fyne.CanvasObject{}
	if t.uiWithCb {
		head = append(head, newFixedMin(container.NewHBox(), fyne.NewSize(20, 1)))
	}
	hl := func(s string, w float32) fyne.CanvasObject {
		l := newColorLabelS(s, pal.textFaint, true, 11).SetMaxWidth()
		l.SetAlign(fyne.TextAlignTrailing)
		return newFixedMin(container.NewStack(l), fyne.NewSize(w, 1))
	}
	nameHead := newColorLabelS("文件名", pal.textFaint, true, 11).SetMaxWidth()
	head = append(head, nameHead, hl("大小", 90))
	if t.view {
		head = append(head, hl("进度", 120), hl("优先级", 64))
	}
	head = append(head, hl("分块序号", 82))
	headBg := canvas.NewRectangle(rgbaA(0x7f7f7f, 0.07))
	headLine := canvas.NewRectangle(pal.border)
	headLine.SetMinSize(fyne.NewSize(0, 1))
	t.headBox.Objects = []fyne.CanvasObject{container.NewVBox(
		container.NewStack(headBg, ftGridRow(0, head...)), headLine)}
	t.headBox.Refresh()
}

// newRowUI 构建一行控件(每 key 一次;此后增量走 update)
func (t *ftTree) newRowUI(s ftRowSpec, pal palette) *ftRowUI {
	u := &ftRowUI{}
	cells := []fyne.CanvasObject{}
	if t.uiWithCb {
		u.cb = newStyledCheck(pal, "", s.checked, s.onCheck)
		cells = append(cells, newFixedMin(container.NewCenter(u.cb), fyne.NewSize(20, 1)))
	}
	if s.isDir {
		u.caret = msText("keyboard_arrow_down", pal.textFaint, 12)
		u.name = newColorLabelS(s.name, pal.text, true, 12.5).SetMaxWidth()
		u.meta = newColorLabelS(s.meta, pal.textFaint, false, 10.5)
		// 名称截断悬停显全文(=文件树 :title;长目录名/文件名看不全)
		cells = append(cells, flex(hFlex(6,
			u.caret, msIcon("folder", pal.warn, 14), newTipLabel(u.name, u.name.Text), u.meta)))
	} else {
		u.name = newColorLabelS(s.name, pal.text, false, 12.5).SetMaxWidth()
		nameKids := []fyne.CanvasObject{msIcon(ftFileIcon(s.name), pal.text, 17), newTipLabel(u.name, u.name.Text)}
		if t.view {
			// 播放钮常驻(媒体文件),显隐由 playable 控制(=勾选联动)
			u.play = newIconButtonSZ(pal, "play_arrow", 22, 15, func() {
				if t.playFn != nil {
					t.playFn(s.ref)
				}
			})
			nameKids = append(nameKids, u.play)
		}
		cells = append(cells, flex(hFlex(6, nameKids...)))
	}
	// 大小列(右对齐;固定 90px 对齐 .ftable 网格)
	u.size = newColorLabelS(s.sizeTxt, pal.textDim, false, 11.5).SetMaxWidth()
	u.size.SetAlign(fyne.TextAlignTrailing)
	cells = append(cells, newFixedMin(container.NewStack(u.size), fyne.NewSize(90, 1)))
	if t.view {
		u.pctL = newColorLabelS(fmt.Sprintf("%.0f%%", s.pct), pal.textDim, false, 11)
		u.prog = newMiniProg(pal, 60, float32(s.pct))
		cells = append(cells, newFixedMin(container.NewHBox(layout.NewSpacer(),
			hGap(7, u.prog, u.pctL)), fyne.NewSize(120, 1)))
		u.prio = newColorLabelS(s.prioTxt, pal.textDim, false, 11.5).SetMaxWidth()
		u.prio.SetAlign(fyne.TextAlignTrailing)
		cells = append(cells, newFixedMin(container.NewStack(u.prio), fyne.NewSize(64, 1)))
	}
	u.piece = newColorLabelS(s.pieceTxt, pal.textDim, false, 11.5).SetMaxWidth()
	u.piece.SetAlign(fyne.TextAlignTrailing)
	cells = append(cells, newFixedMin(container.NewStack(u.piece), fyne.NewSize(82, 1)))
	indent := float32(s.depth) * 14
	if !s.isDir {
		indent += 20
	}
	grid := ftGridRow(indent, cells...)
	if s.isDir {
		u.wrap = wrapFtRow(grid, func() { t.toggleDir(s.ref) })
	} else {
		u.wrap = wrapFtRow(grid, nil)
	}
	return u
}

// update 原位刷新一行(全部变更守卫;返回是否需重排版——计数宽度/播放钮
// 显隐会改变行内布局)
func (u *ftRowUI) update(s ftRowSpec, pal palette) bool {
	relayout := false
	if u.cb != nil {
		u.cb.onChange = s.onCheck
		u.cb.SetChecked(s.checked)
		u.cb.SetMixed(s.mixed)
	}
	if u.caret != nil {
		want := MS_ICONS["keyboard_arrow_down"]
		if s.collapsed {
			want = MS_ICONS["chevron_right"]
		}
		if u.caret.Text != string(want) {
			u.caret.Text = string(want)
			u.caret.Refresh()
		}
	}
	u.name.SetText(s.name)
	if u.meta != nil {
		if u.meta.Text() != s.meta {
			relayout = true // 计数变宽需重排版
		}
		u.meta.SetText(s.meta)
	}
	u.size.SetText(s.sizeTxt)
	if u.prog != nil {
		u.prog.setPct(float32(s.pct))
		u.pctL.SetText(fmt.Sprintf("%.0f%%", s.pct))
	}
	if u.prio != nil {
		c := pal.textDim
		switch s.prioTxt {
		case "[完成]":
			c = pal.ok
		case "不下载":
			c = pal.textFaint
		}
		u.prio.SetColor(c)
		u.prio.SetText(s.prioTxt)
	}
	u.piece.SetText(s.pieceTxt)
	if u.play != nil {
		if u.play.Visible() != s.playable {
			if s.playable {
				u.play.Show()
			} else {
				u.play.Hide()
			}
			relayout = true
		}
	}
	return relayout
}

// equalStrings 串切片全等(行序变化判定)
func equalStrings(a, b []string) bool {
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

// ftGridRow 组一行网格(=.ftable .ft-row:列 gap 10、padding 4px 10px、min 高 29)
func ftGridRow(indent float32, cells ...fyne.CanvasObject) fyne.CanvasObject {
	return newFixedMin(container.New(
		layout.NewCustomPaddedLayout(4, 4, 10+indent, 10),
		hFlex(10, cells...)), fyne.NewSize(0, 29))
}

// ftFileIcon 按扩展名挑文件图标(=lib/filetree.js FILE_ICON_RULES)
func ftFileIcon(name string) string {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	}
	switch ext {
	case "mp4", "mkv", "avi", "mov", "wmv", "flv", "ts", "m2ts", "webm", "m4v", "rmvb", "mpg", "mpeg", "3gp", "vob":
		return "movie"
	case "mp3", "flac", "ape", "m4a", "aac", "wav", "ogg", "opus", "wma", "mid", "tak", "dsf":
		return "music_note"
	case "jpg", "jpeg", "png", "gif", "bmp", "webp", "svg", "heic", "avif", "tiff", "ico", "psd":
		return "image"
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst", "iso", "cab":
		return "folder_zip"
	case "pdf":
		return "picture_as_pdf"
	case "srt", "ass", "ssa", "sub", "idx", "sup":
		return "subtitles"
	case "json", "yml", "yaml", "xml", "html", "htm", "css", "js", "mjs", "jsx", "tsx", "py", "go", "c", "h", "cpp", "hpp", "java", "rs", "sh", "bat", "cmd", "ps1", "sql":
		return "code"
	case "xls", "xlsx", "csv", "ods":
		return "table_chart"
	case "txt", "doc", "docx", "epub", "mobi", "md":
		return "article"
	}
	return "note"
}

// miniProg 细进度条(=.mini-prog:5px 高、radius 3、灰底 + accent→accent-2 渐变)
type miniProg struct {
	widget.BaseWidget
	track *canvas.Rectangle
	fill  *roundedGrad
	pct   float32
}

func newMiniProg(pal palette, w, pct float32) *miniProg {
	p := &miniProg{pct: pct}
	p.ExtendBaseWidget(p)
	p.track = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.18))
	p.track.CornerRadius = 3
	p.fill = newRoundedGrad(pal.accent, pal.accent2, 3)
	p.track.SetMinSize(fyne.NewSize(w, 5))
	return p
}

func (p *miniProg) CreateRenderer() fyne.WidgetRenderer { return &miniProgRenderer{p: p} }

// setPct 原位更新进度(增量刷新;变更才重排填充条)
func (p *miniProg) setPct(v float32) {
	if p.pct == v {
		return
	}
	p.pct = v
	p.Refresh()
}

type miniProgRenderer struct{ p *miniProg }

func (r *miniProgRenderer) Layout(size fyne.Size) {
	r.p.track.Resize(size)
	w := size.Width * r.p.pct / 100
	if w < 2 && r.p.pct > 0 {
		w = 2
	}
	r.p.fill.Move(fyne.NewPos(0, 0))
	r.p.fill.Resize(fyne.NewSize(w, size.Height))
}

func (r *miniProgRenderer) Refresh() { r.Layout(r.p.Size()) }

func (r *miniProgRenderer) MinSize() fyne.Size { return fyne.NewSize(60, 5) }
func (r *miniProgRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.p.track, r.p.fill}
}
func (r *miniProgRenderer) Destroy() {}

// ftRowWrap 文件树行:悬停淡底(=ft-row:hover)+ 整行点按(目录折叠)。
// 行内复选框/播放钮更深且自身可点,命中优先于行
type ftRowWrap struct {
	widget.BaseWidget
	content fyne.CanvasObject
	bg      *canvas.Rectangle
	onTap   func()
	hg      *hoverGroup // 行组:行内复选框/文件名悬停时保持整行高亮
}

func wrapFtRow(content fyne.CanvasObject, onTap func()) *ftRowWrap {
	r := &ftRowWrap{content: content, onTap: onTap}
	r.ExtendBaseWidget(r)
	r.bg = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.07))
	r.bg.Hide()
	r.hg = newHoverGroup(func(on bool) {
		if on {
			r.bg.Show()
		} else {
			r.bg.Hide()
		}
		r.bg.Refresh()
	})
	registerHoverGroup(r.content, r.hg)
	return r
}

func (r *ftRowWrap) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: container.NewStack(r.bg, r.content), min: r.content.MinSize()}
}

func (r *ftRowWrap) Tapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap()
	}
}

func (r *ftRowWrap) MouseIn(*desktop.MouseEvent) {
	r.hg.enter()
}

func (r *ftRowWrap) MouseOut() {
	r.hg.leave()
}

func (r *ftRowWrap) MouseMoved(*desktop.MouseEvent) {}

func (t *ftTree) allFilesUnder(n *ftNode) []*ftFile {
	var out []*ftFile
	var walk func(x *ftNode)
	walk = func(x *ftNode) {
		out = append(out, x.Files...)
		for _, d := range x.Dirs {
			walk(d)
		}
	}
	walk(n)
	return out
}

// checkDirKey 目录勾选=覆盖其下所有文件勾选。节点指针每次重建都会换,
// 回调只能持 key,运行时经 dirNodes 取当前节点
func (t *ftTree) checkDirKey(key string, on bool) {
	n := t.dirNodes[key]
	if n == nil {
		return
	}
	for _, f := range t.allFilesUnder(n) {
		t.sel[strings.Join(f.Path, "/")] = on
	}
	if t.onChange != nil {
		t.onChange()
	}
	t.rebuild()
}

// toggleDir 折叠/展开(整行点按;重建为增量,行控件复用)
func (t *ftTree) toggleDir(key string) {
	t.collapsed[key] = !t.collapsed[key]
	t.rebuild()
}

// SetAll 全选/全不选(=FileTree.vue setAll)
func (t *ftTree) SetAll(on bool) {
	for k := range t.sel {
		t.sel[k] = on
	}
	if t.onChange != nil {
		t.onChange()
	}
	t.rebuild()
}

// SelectedPaths 部分选择返回选中清单;全选返回 nil(=selectedPaths 语义:
// 创建请求只在部分选择时携带 files)
func (t *ftTree) SelectedPaths() []string {
	keys := make([]string, 0, len(t.sel))
	all := true
	for k, on := range t.sel {
		keys = append(keys, k)
		if !on {
			all = false
		}
	}
	if all {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if t.sel[k] {
			out = append(out, k)
		}
	}
	return out
}

// SelStat 已选统计(=selStat:n/total/bytes/all)
func (t *ftTree) SelStat() (n, total int, bytes int64, all bool) {
	total = len(t.files)
	sel := t.SelectedPaths()
	if sel == nil {
		for _, f := range t.files {
			bytes += f.Size
		}
		return total, total, bytes, true
	}
	set := map[string]bool{}
	for _, k := range sel {
		set[k] = true
	}
	for _, f := range t.files {
		if set[strings.Join(f.Path, "/")] {
			n++
			bytes += f.Size
		}
	}
	return n, total, bytes, false
}

// ftPrioTxt 优先级列(=filetree.js ftPrioTxt:对照 BitComet 下载优先级)
func ftPrioTxt(done, size int64, wanted *bool, running bool) string {
	if wanted != nil && !*wanted {
		return "不下载"
	}
	if size <= 0 || done >= size {
		return "[完成]"
	}
	if running && done > 0 {
		return "下载中"
	}
	return "普通"
}

// newFtTreeView 详情页 view 模式文件树:进度/优先级列 + 可编辑勾选 +
// 媒体文件播放按钮。dirtyPtr 非空时勾选变化置位(轮询不重建勾选集)。
func newFtTreeView(app *App, files []*ftFile, t *taskView, dirtyPtr *bool) *ftTree {
	tr := &ftTree{
		app: app, files: files, sel: map[string]bool{}, collapsed: map[string]bool{},
		view: true, editable: t.Status != engineStatusCanceled,
		taskStatus: t.Status, dirtyPtr: dirtyPtr, minTreeH: 300,
	}
	tr.onChange = func() {
		if dirtyPtr != nil {
			*dirtyPtr = true
		}
	}
	tr.playFn = func(path string) {} // 由调用方覆盖(store.OpenPlayer)
	// 初始勾选:按服务端 wanted(=selReset 语义);大种子默认折叠顶层
	for _, f := range files {
		tr.sel[strings.Join(f.Path, "/")] = f.Wanted
	}
	if len(files) > 60 {
		seen := map[string]bool{}
		for _, f := range files {
			if !seen[f.Path[0]] {
				seen[f.Path[0]] = true
				tr.collapsed[f.Path[0]] = true
			}
		}
	}
	tr.ExtendBaseWidget(tr)
	tr.box = container.NewVBox()
	tr.headBox = container.NewVBox()
	return tr
}

// updateData 数据刷新(view 模式):行控件增量刷新(悬停/勾选态不丢);
// 用户勾选未应用前轮询不得用服务端 wanted 覆盖(=dwSelDirty 语义)
func (t *ftTree) updateData(files []*ftFile, task *taskView) {
	t.files = files
	t.taskStatus = task.Status
	t.editable = task.Status != engineStatusCanceled
	if !t.selDirtyCheck() {
		for _, f := range files {
			t.sel[strings.Join(f.Path, "/")] = f.Wanted
		}
	}
	t.rebuild()
}

// selDirtyCheck 用户改过勾选且未应用(脏标记由 drawer 持有,构造传入)
func (t *ftTree) selDirtyCheck() bool {
	return t.dirtyPtr != nil && *t.dirtyPtr
}

// SetPlayFn 设置播放按钮回调(=FileTree emit("play"))
func (t *ftTree) SetPlayFn(fn func(path string)) { t.playFn = fn }

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
