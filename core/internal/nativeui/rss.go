package nativeui

// RSS 订阅视图(RssPane/RssFeedBlock/RssDialogs 移植):文件夹/订阅/条目/
// 资源链接四级树 + 批量勾选吸底条。语义对齐:
//   - 视图进入时加载 + 5s 轮询,离开即停(=startRssPoll/stopRssPoll);
//   - 条目展开加载资源链接(条目页面本身是"页面"候选,排附件后);
//   - 资源链接勾选集:磁力不参与批量(一次只能建一个 BT 任务,单独走
//     BT 对话框);条目折叠态复选框 = 全选该条目资源;
//   - 批量下载:普通链接 → 新建任务窗批量预填(stay),磁力 → BT 窗;
//   - 重排:文件夹上移/下移按钮 + 订阅经编辑对话框换文件夹(拖拽为
//     打磨项,v1 等价交互)。

import (
	"fmt"
	"image/color"
	"log"
	"net/url"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/rss"
)

// goSafe 带恢复的 goroutine:panic 写应用日志(windowsgui 下 panic 栈进不了
// stderr,进程直接消失且无迹可查;日志文件=系统日志页同源)
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[panic:%s] %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

var magnetRe = regexp.MustCompile(`(?i)^magnet:`)

// rssItemRes 条目可批量勾选的资源链接(=rssItemRes:排除磁力)
func rssItemRes(it *rss.Item) []rss.ItemLink {
	out := make([]rss.ItemLink, 0, len(it.Links))
	for _, l := range it.Links {
		if l.URL != "" && !magnetRe.MatchString(l.URL) {
			out = append(out, l)
		}
	}
	return out
}

// rssLinkName 资源链接显示名(=rssLinkName:title > URL 文件段 > 原地址)
func rssLinkName(l rss.ItemLink) string {
	if l.Title != "" {
		return l.Title
	}
	u := strings.SplitN(l.URL, "?", 2)[0]
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		u = u[i+1:]
	}
	if u != "" {
		return u
	}
	return l.URL
}

// fmtAgo 订阅更新时间(=rssfmt.js fmtAgo)
func fmtAgo(t time.Time) string {
	if t.IsZero() || t.Year() <= 1 {
		return "尚未更新"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "刚刚更新"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前更新", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前更新", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前更新", int(d.Hours()/24))
	}
}

// fmtItemDate 条目日期(yyyy-MM-dd HH:mm;不可解析原样)
func fmtItemDate(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return s
}

// rssPaneState RSS 视图状态(挂 app 上,懒建)
type rssPaneState struct {
	app     *App
	root    *fyne.Container
	box     *fyne.Container
	headLbl *colorLabel
	selBar  *fyne.Container
	selLbl  *fyne.Container // =rss-selbar .lbl:已选 <b>N</b> 个链接
	selNum  *colorLabel     // <b> 数字(加粗、text 色;前后缀 dim)
	selDim  [2]*colorLabel  // "已选 " / " 个链接"

	sel                  map[string]string // url → 显示名(勾选集)
	selFeed              map[string]string // url → 归属订阅 id(删订阅后按此剔除残留勾选)
	openFeeds, openItems map[string]bool
	collapsedFolders     map[string]bool
	itemsCache           map[string][]rss.Item
	busy                 bool

	lastTree      RSSTree // 上次渲染的树快照(未变化跳过重建)
	hasTree       bool    // lastTree 是否有效(RSSTree 是值类型,零值与空树无法区分)
	foldersSeeded bool    // 首载已按 Vue 语义折叠过全部文件夹(仅会话首次)

	// 拖拽排序状态(=makeRssDnd;仅 UI 线程读写)
	dndRows   []*dndRow // 当前渲染中可拖/可落行(每次重建刷新)
	dragKind  string
	dragID    string
	dragSrc   *dndRow // 拖源行(命中排除/落点比较按行身份,不能用 id:
	dropRow   *dndRow // 未分组行 id 为空串,与"无落点"初值撞车)
	dropPos   string  // 落点在目标行前/后("before"/"after")
	dropKind  string
	dropID    string
	dragGhost *tipPopW // 拖影(随光标移动的行名卡片)
	uiRev     int      // 本地渲染态版本:展开/折叠/条目缓存变化时 +1(仅 UI 线程读写)
	drawnRev  int      // 已渲染版本;落后 uiRev 说明本地态变了,树快照相同也必须重建

	pollStop chan struct{}
	pollOnce sync.Once
	visible  bool
	mu       sync.Mutex
}

// ensureRSS 懒建 RSS 视图
func (app *App) ensureRSS() *rssPaneState {
	if app.rss != nil {
		return app.rss
	}
	rp := &rssPaneState{
		app:              app,
		sel:              map[string]string{},
		selFeed:          map[string]string{},
		openFeeds:        map[string]bool{},
		openItems:        map[string]bool{},
		collapsedFolders: map[string]bool{},
		itemsCache:       map[string][]rss.Item{},
	}
	// 持久对象只建一次:headLbl/selLbl 承载动态文案(切主题只重着色不重建),
	// box 承载正文行(切主题只换色板、不重置滚动)。先建再入容器:typed-nil
	// 混入 Border 会在布局期崩溃
	pal := app.store.appPal()
	rp.headLbl = newColorLabelS("", pal.textFaint, false, 12)
	// 标签拆三段:数字加粗走 text 色(=Vue <b>),前后缀 text-dim
	rp.selDim[0] = newColorLabelS("已选 ", pal.textDim, false, 12.5)
	rp.selNum = newColorLabelS("0", pal.text, true, 12.5)
	rp.selDim[1] = newColorLabelS(" 个链接", pal.textDim, false, 12.5)
	rp.selLbl = container.NewHBox(rp.selDim[0], rp.selNum, rp.selDim[1])
	rp.box = container.New(&vgapLayout{gap: 5})
	rp.rebuildChrome()
	rp.refreshSel()
	app.rss = rp
	curRP = rp
	return rp
}

// rebuildChrome 用当前色板重建标题行与选中栏。标题文字与按钮的取色都是
// 构造期快照,切主题(只换 palv)不会重着色,须整体重建;headLbl/selLbl/box
// 是持久对象,只重着色不重建。root 每次重建,因 buildRoot 会重新 Stack 进主树。
func (rp *rssPaneState) rebuildChrome() {
	pal := rp.app.store.appPal()
	rp.headLbl.SetColor(pal.textFaint)
	rp.selDim[0].SetColor(pal.textDim)
	rp.selDim[1].SetColor(pal.textDim)
	rp.selNum.SetColor(pal.text)
	headRight := hGap(8,
		newGhostButton(pal, "新建文件夹", func() { rp.openFolderDialog("") }),
		newGhostButton(pal, "全部已读", func() {
			go func() {
				if err := rp.app.client.RSSReadAll(); err == nil {
					fyne.Do(rp.reload)
				}
			}()
		}),
		newGhostButton(pal, "全部更新", func() {
			go func() {
				if err := rp.app.client.RSSRefreshAll(); err == nil {
					fyne.Do(rp.reload)
				}
			}()
		}),
		newPrimaryButton(pal, "添加订阅", func() { rp.openFeedDialog("", nil) }))
	head := container.NewBorder(nil, nil,
		hGap(10, newColorLabelS("RSS 订阅", pal.text, true, 16), rp.headLbl),
		headRight)
	// "清空选择" 仅清勾选集不够:行复选框的勾选态是渲染期快照,refreshSel 只动
	// 吸底条不动行,清了仍显勾选(=用户报的"清空毫无作用")。须重建正文让行
	// 复选框/选中卡高亮复位;行数与行高不变,滚动位置得以保持
	clearSel := func() {
		rp.sel = map[string]string{}
		rp.selFeed = map[string]string{}
		rp.refreshSel()
		rp.rebuildFromState()
	}
	selInner := container.NewBorder(nil, nil, rp.selLbl,
		hGap(8,
			newGhostButton(pal, "清空选择", clearSel).SetMini(),
			newPrimaryButton(pal, "批量下载", func() { rp.batchDownload() }).SetMini()))
	// 样式对齐 Vue .rss-selbar:panel-solid 94% 底 + accent→cyan 半透明渐变
	// (90deg)+ 1px border-strong 描边 + radius 8 + padding 9/12;顶 10px 间距
	selBase := canvas.NewRectangle(withAlpha(pal.panelSolid, 0.94))
	selBase.CornerRadius = 0 // 直角矩形(=用户本轮要求;底/描边同半径,须一致)
	selGrad := canvas.NewLinearGradient(pal.accent, pal.accent2, 0)
	selGrad.StartColor = rgbaA(0x4f8cff, 0.16)
	selGrad.EndColor = rgbaA(0x22d3ee, 0.08)
	selBord := canvas.NewRectangle(color.NRGBA{A: 0})
	selBord.CornerRadius = 0 // 与 selBase 一致(直角)
	selBord.StrokeWidth = 1
	selBord.StrokeColor = pal.borderStrong
	styled := container.NewStack(selBase, selGrad, selBord,
		container.New(layout.NewCustomPaddedLayout(9, 9, 12, 12), selInner))
	rp.selBar = container.New(layout.NewCustomPaddedLayout(10, 0, 0, 0), styled)
	rp.root = container.NewBorder(container.NewVBox(head, widget.NewSeparator()), rp.selBar, nil, nil,
		container.NewVScroll(rp.box))
	// 重建出的 selBar 默认可见,须按当前勾选集复位显隐:切主题走 retheme→
	// rebuildChrome 却不另行 refreshSel,勾选为空时新 selBar 会带着"已选 0
	// 个链接"露出来(=用户报的"进 RSS 没选任何东西却已显示已选 0 个链接")。
	rp.refreshSel()
}

// retheme 切主题后刷新 RSS 面板:标题/选中栏按新色板重建;正文若正在显示就地
// 重排——纯切主题不触发 renderTree,正文行(文件夹名/订阅名/竖线)会停在旧色板
// 的灰白色,即亮主题下整页看不清的根因。
func (rp *rssPaneState) retheme() {
	rp.rebuildChrome()
	// 去掉 visible 守卫:在别的页切主题后再切回 RSS,正文缓存体若不重排会
	// 沿用旧主题色(=用户报的"切主题后打开 RSS 颜色仍旧")
	if rp.hasTree {
		rp.renderFromState()
	}
}

// onShow 视图可见:加载 + 启动轮询
func (rp *rssPaneState) onShow() {
	rp.visible = true
	rp.reload()
	rp.pollOnce.Do(func() {
		rp.pollStop = make(chan struct{})
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-rp.pollStop:
					return
				case <-t.C:
					if rp.visible {
						rp.reload()
					}
				}
			}
		}()
	})
}

// onHide 视图离开:停轮询
func (rp *rssPaneState) onHide() {
	rp.visible = false
	rp.pollOnce = sync.Once{}
}

// reload 拉树快照并重建整棵树。HTTP 放后台线程:reload 会被 UI 事件直接
// 调用(轮询/展开/勾选),阻塞请求在 UI 线程会卡帧(hover 闪烁元凶之一)。
func (rp *rssPaneState) reload() {
	go func() {
		tree, err := rp.app.client.RSS()
		if err != nil {
			return
		}
		fyne.Do(func() { rp.renderTree(tree) })
	}()
}

// renderTree 用快照重建整棵树(UI 线程)。树未变化且本地交互态也没变时跳过
// (整行重建会造成每 5 秒一帧 hover 闪动 + 容器 churn);条目缓存刷新仍在尾部照常执行。
func (rp *rssPaneState) renderTree(tree RSSTree) {
	// 展开中的订阅条目缓存刷新与树重建解耦:树未变也要刷新缓存
	// (静默更新,绝不在这里调 reload,否则与条目抓取互触发死循环)
	defer rp.refreshOpenItems()
	if rp.hasTree && reflect.DeepEqual(tree, rp.lastTree) && rp.drawnRev == rp.uiRev {
		return
	}
	rp.hasTree = true
	rp.lastTree = tree
	rp.pruneSelection(tree) // 删订阅后对账本地态:剔除残留勾选 + 清已删订阅的展开/缓存
	rp.renderFromState()
}

// pruneSelection 树快照刷新后对账本地态(UI 线程):删掉勾选集中已删订阅的
// 残留,并清掉已删订阅的展开态/条目缓存。否则删了订阅后,吸底条仍显示"已选
// n 个链接"、还能进批量下载去下已不存在的链接(=用户报的"删了订阅,勾选
// 集还在");且轮询会持续去抓已不存在的订阅。文件夹删除是把订阅移到未分组、
// 订阅仍在 tree.Feeds 里,不会被误清。
func (rp *rssPaneState) pruneSelection(tree RSSTree) {
	live := make(map[string]bool, len(tree.Feeds))
	for _, f := range tree.Feeds {
		live[f.ID] = true
	}
	changed := false
	for u, fid := range rp.selFeed {
		if !live[fid] {
			delete(rp.sel, u)
			delete(rp.selFeed, u)
			changed = true
		}
	}
	// itemsCache 会被 refreshOpenItems 的后台 goroutine 并发读写(持 rp.mu),
	// 清理须在同一把锁下进行;openFeeds 仅 UI 线程访问,一并放入临界区无副作用
	rp.mu.Lock()
	for fid, open := range rp.openFeeds {
		if open && !live[fid] {
			delete(rp.openFeeds, fid)
			delete(rp.itemsCache, fid)
			changed = true
		}
	}
	rp.mu.Unlock()
	if changed {
		rp.refreshSel()
	}
}

// rebuildFromState 本地交互态(展开/折叠/条目缓存)变化后的就地重建:
// 不拉树也不触发 refreshOpenItems——本函数也会被 refreshOpenItems 的完成
// 回调调用,再走 reload/renderTree 会互触发死循环(3GB 事故)。树未就绪退化为 reload。
func (rp *rssPaneState) rebuildFromState() {
	if !rp.hasTree {
		rp.reload()
		return
	}
	rp.uiRev++
	rp.renderFromState()
}

// renderFromState 按 lastTree+本地交互态重建整棵树(UI 线程)
func (rp *rssPaneState) renderFromState() {
	tree := rp.lastTree
	rp.dndRows = rp.dndRows[:0]
	// 软件刚启动时文件夹(含未分组)一律折叠,首载之后完全跟随用户操作
	// (=rss.foldersSeeded 语义)
	if !rp.foldersSeeded {
		rp.foldersSeeded = true
		for _, fo := range tree.Folders {
			rp.collapsedFolders["F:"+fo.ID] = true
		}
		rp.collapsedFolders["F:"] = true
	}
	rp.mu.Lock()
	busy := tree.Busy
	updating := tree.Updating
	rp.mu.Unlock()
	rp.busy = busy

	feedsOf := func(fid string) []rss.FeedView {
		out := []rss.FeedView{}
		for _, f := range tree.Feeds {
			if (f.FolderID) == fid {
				out = append(out, f)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
		return out
	}
	folderUnread := func(fid string) int {
		n := 0
		for _, f := range feedsOf(fid) {
			n += f.Unread
		}
		return n
	}

	{
		rp.headLbl.SetText(fmt.Sprintf("%d 个文件夹 · %d 个订阅 · %d 条未读",
			len(tree.Folders), len(tree.Feeds), tree.Unread))
		rows := []fyne.CanvasObject{}
		if len(tree.Folders) == 0 && len(tree.Feeds) == 0 {
			rows = append(rows, widget.NewLabel("还没有 RSS 订阅"),
				widget.NewLabel("点右上角「添加订阅」粘贴 RSS / Atom 地址;下载链接可一键创建任务"))
		}
		updatingSet := map[string]bool{}
		for _, id := range updating {
			updatingSet[id] = true
		}
		for _, fo := range tree.Folders {
			fo := fo
			key := "F:" + fo.ID
			collapsed := rp.collapsedFolders[key]
			rows = append(rows, rp.folderRow(fo, collapsed, folderUnread(fo.ID), len(feedsOf(fo.ID))))
			if !collapsed {
				kids := []fyne.CanvasObject{}
				if len(feedsOf(fo.ID)) == 0 {
					kids = append(kids, indented("该文件夹暂无订阅"))
				}
				for _, f := range feedsOf(fo.ID) {
					kids = append(kids, rp.feedBlock(f, updatingSet[f.ID])...)
				}
				rows = append(rows, childrenWrap(kids...))
			}
		}
		// 未分组
		collapsed := rp.collapsedFolders["F:"]
		rows = append(rows, rp.folderRowSimple("未分组", collapsed, folderUnread(""), len(feedsOf(""))))
		if !collapsed {
			kids := []fyne.CanvasObject{}
			if len(feedsOf("")) == 0 {
				kids = append(kids, indented("暂无未分组订阅"))
			}
			for _, f := range feedsOf("") {
				kids = append(kids, rp.feedBlock(f, updatingSet[f.ID])...)
			}
			rows = append(rows, childrenWrap(kids...))
		}
		rp.box.Objects = rows
		rp.box.Refresh()
	}
	rp.drawnRev = rp.uiRev
}

// refreshOpenItems 拉取所有展开订阅的条目并静默更新缓存(不触发重建)。
// 注意:这里绝不能调 reload——renderTree 会触发条目抓取,抓取完成又
// reload,互触发死循环会把内存打爆(3GB 事故)。
func (rp *rssPaneState) refreshOpenItems() {
	for fid, open := range rp.openFeeds {
		if !open {
			continue
		}
		fid := fid
		go func() {
			items, err := rp.app.client.RSSItems(fid)
			fyne.Do(func() {
				rp.mu.Lock()
				old, had := rp.itemsCache[fid]
				if err != nil {
					if had {
						rp.mu.Unlock()
						return // 已有内容(哪怕旧数据)不被错误占位闪掉
					}
					rp.itemsCache[fid] = []rss.Item{{Title: "⚠ 加载失败: " + err.Error()}}
					rp.mu.Unlock()
					rp.rebuildFromState()
					return
				}
				if had && reflect.DeepEqual(old, items) {
					rp.mu.Unlock()
					return // 数据没变不重建,否则 5 秒轮询期整行重排引发 hover 闪动
				}
				rp.itemsCache[fid] = items
				rp.mu.Unlock()
				rp.rebuildFromState()
			})
		}()
	}
}

func indented(s string) fyne.CanvasObject {
	pal := currentRPal()
	return container.New(layout.NewCustomPaddedLayout(2, 2, 34, 0),
		newColorLabelS(s, pal.textFaint, false, 11.5))
}

// currentRPal RSS 行构建器的色板入口(行构建器无 app 上下文时兜底)
var curRP *rssPaneState

func currentRPal() palette {
	if curRP != nil {
		return curRP.app.store.appPal()
	}
	return newPalette(true)
}

// rssCaret 展开箭头(=rss-caret:13px chevron,折叠时向右)
func rssCaret(pal palette, open bool, onTap func()) *iconButton {
	name := "keyboard_arrow_down"
	if !open {
		name = "chevron_right"
	}
	return newIconButtonSZ(pal, name, 22, 15, onTap)
}

// rssAct 行内图标钮(=row-acts .icon-btn 24px)
func rssAct(pal palette, icon string, onTap func()) *iconButton {
	return newIconButtonSZ(pal, icon, 24, 15, onTap)
}

// rssBadge 未读徽章(=rss-badge:渐变药丸 10.5px 白字)
func rssBadge(pal palette, n int) fyne.CanvasObject {
	if n <= 0 {
		return layout.NewSpacer()
	}
	bg := newRoundedGrad(pal.accent, pal.accent2, 9)
	lbl := newColorLabelS(fmt.Sprint(n), color.NRGBA{R: 255, G: 255, B: 255, A: 255}, true, 10.5)
	return newFixedMin(container.NewStack(bg, container.NewCenter(lbl)), fyne.NewSize(20, 16))
}

// folderRow 文件夹行(=rss-folder:caret+folder 图标(warn)+名称+徽章+计数+行内动作)
func (rp *rssPaneState) folderRow(fo rss.Folder, collapsed bool, unread, feedN int) fyne.CanvasObject {
	pal := rp.app.store.appPal()
	key := "F:" + fo.ID
	toggle := func() { rp.collapsedFolders[key] = !collapsed; rp.rebuildFromState() }
	icName := "folder_open"
	if collapsed {
		icName = "folder"
	}
	ic := msText(icName, pal.warn, 16)
	name := newColorLabelS(fo.Name, pal.text, true, 13.5).SetMaxWidth()
	addBtn := rssAct(pal, "add", func() { rp.openFeedDialog(fo.ID, nil) })
	addBtn.SetTip("在此文件夹添加订阅")
	renBtn := rssAct(pal, "movie_edit", func() { rp.openFolderDialog(fo.ID) })
	renBtn.SetTip("重命名文件夹")
	delBtn := rssAct(pal, "delete", func() {
		rp.app.store.ConfirmBox(ConfirmReq{
			Message: "删除文件夹「" + fo.Name + "」?其中订阅会移到「未分组」,不会被删除。",
			OKLabel: "删除",
			OnOk: func() {
				go func() {
					if err := rp.app.client.RSSFolderDelete(fo.ID); err == nil {
						fyne.Do(rp.reload)
					}
				}()
			},
		})
	})
	delBtn.SetTip("删除文件夹(其中订阅移至未分组)")
	inner := container.New(layout.NewCustomPaddedLayout(8, 8, 12, 12),
		hFlex(8, rssCaret(pal, !collapsed, toggle), ic, flex(newTipLabel(name, func() string { return fo.Name })), rssBadge(pal, unread),
			newColorLabelS(fmt.Sprintf("%d 个订阅", feedN), pal.textFaint, false, 11),
			hGap(2, addBtn, renBtn, delBtn)))
	return wrapDND(rp, "folder", fo.ID, fo.Name, inner, toggle)
}

// folderRowSimple 未分组行(只折叠,无操作;=rss-root 弱化)
func (rp *rssPaneState) folderRowSimple(name string, collapsed bool, unread, feedN int) fyne.CanvasObject {
	pal := rp.app.store.appPal()
	key := "F:"
	toggle := func() { rp.collapsedFolders[key] = !collapsed; rp.rebuildFromState() }
	inner := container.New(layout.NewCustomPaddedLayout(8, 8, 12, 12),
		hFlex(8, rssCaret(pal, !collapsed, toggle),
			msText("folder", pal.textFaint, 16),
			newColorLabelS(name, pal.textDim, false, 13.5),
			rssBadge(pal, unread),
			flex(layout.NewSpacer()),
			newColorLabelS(fmt.Sprintf("%d 个订阅", feedN), pal.textFaint, false, 11)))
	return wrapDND(rp, "folder", "", "未分组", inner, toggle)
}

// moveFolder 文件夹上移/下移(重排 v1 交互)
func (rp *rssPaneState) moveFolder(id string, dir int) {
	tree, err := rp.app.client.RSS()
	if err != nil {
		return
	}
	ids := make([]string, 0, len(tree.Folders))
	at := -1
	for i, f := range tree.Folders {
		ids = append(ids, f.ID)
		if f.ID == id {
			at = i
		}
	}
	j := at + dir
	if at < 0 || j < 0 || j >= len(ids) {
		return
	}
	ids[at], ids[j] = ids[j], ids[at]
	go func() {
		if err := rp.app.client.RSSFolderOrder(ids); err == nil {
			fyne.Do(rp.reload)
		}
	}()
}

// feedBlock 订阅行 + 条目区(=RssFeedBlock:caret+article(accent)+名称+徽章
// +meta(右对齐)+行内动作)
func (rp *rssPaneState) feedBlock(f rss.FeedView, updating bool) []fyne.CanvasObject {
	pal := rp.app.store.appPal()
	out := []fyne.CanvasObject{}
	open := rp.openFeeds[f.ID]
	title := f.Title
	if title == "" {
		title = f.SiteTitle
	}
	if title == "" {
		title = f.URL
	}
	metaTxt := fmtAgo(f.LastFetch) + fmt.Sprintf(" · %d 条", f.ItemCount)
	metaClr := pal.textFaint
	if updating {
		metaTxt = "更新中…"
		metaClr = pal.accent2
	} else if f.LastError != "" {
		metaTxt = "更新失败:" + f.LastError
		metaClr = pal.warn
	}
	toggle := func() {
		rp.openFeeds[f.ID] = !open
		if rp.openFeeds[f.ID] && rp.itemsCache[f.ID] == nil {
			fid := f.ID
			go func() {
				items, err := rp.app.client.RSSItems(fid)
				fyne.Do(func() {
					rp.mu.Lock()
					if err != nil {
						if rp.itemsCache[fid] == nil {
							rp.itemsCache[fid] = []rss.Item{{Title: "⚠ 加载失败: " + err.Error()}}
							rp.mu.Unlock()
							rp.rebuildFromState()
						}
						return
					}
					rp.itemsCache[fid] = items
					rp.mu.Unlock()
					rp.rebuildFromState()
				})
			}()
		}
		rp.rebuildFromState()
	}
	refBtn := rssAct(pal, "refresh", func() {
		go func() {
			if err := rp.app.client.RSSFeedRefresh(f.ID); err == nil {
				fyne.Do(rp.reload)
			}
		}()
	})
	refBtn.SetTip("刷新该订阅")
	readBtn := rssAct(pal, "check", func() {
		go func() {
			if err := rp.app.client.RSSFeedRead(f.ID); err == nil {
				fyne.Do(rp.reload)
			}
		}()
	})
	readBtn.SetTip("全部标为已读")
	editBtn := rssAct(pal, "movie_edit", func() { rp.openFeedDialog("", &f) })
	editBtn.SetTip("编辑(名称/文件夹)")
	delBtn := rssAct(pal, "delete", func() {
		rp.app.store.ConfirmBox(ConfirmReq{
			Message: "删除订阅「" + title + "」?",
			OKLabel: "删除",
			OnOk: func() {
				go func() {
					if err := rp.app.client.RSSDeleteFeed(f.ID); err == nil {
						fyne.Do(rp.reload)
					}
				}()
			},
		})
	})
	delBtn.SetTip("删除订阅")
	row := container.New(layout.NewCustomPaddedLayout(7, 7, 10, 10),
		hFlex(8, rssCaret(pal, open, toggle),
			msText("article", pal.accent, 15),
			// 订阅名截断悬停显全文(=feed name :title)
			flex(newTipLabel(newColorLabelS(title, pal.text, false, 13).SetMaxWidth(), func() string { return title })),
			rssBadge(pal, f.Unread),
			newColorLabelS(metaTxt, metaClr, false, 11),
			hGap(2, refBtn, readBtn, editBtn, delBtn)))
	// 行主体点击=展开/收起(=RssPane onPaneClick 订阅行整行语义)
	out = append(out, wrapDND(rp, "feed", f.ID, title, row, toggle))
	if !open {
		return out
	}
	items, okCached := rp.itemsCache[f.ID]
	if !okCached {
		out = append(out, indented("加载中…"))
		return out
	}
	if len(items) == 0 {
		out = append(out, indented("暂无条目,可点该订阅的刷新按钮重试"))
		return out
	}
	// 条目区缩进(=.rss-items:margin-left 34,gap 2)
	itemsWrap := container.New(&vgapLayout{gap: 2})
	for i := range items {
		for _, o := range rp.itemRow(f.ID, &items[i]) {
			itemsWrap.Add(o)
		}
	}
	out = append(out, container.New(layout.NewCustomPaddedLayout(0, 0, 34, 0), itemsWrap))
	return out
}

// ---- 拖拽排序(=makeRssDnd) ----
// fyne 核心没有跨对象 Droppable:拖动事件由行包装层接收,悬停命中靠各行
// 绝对矩形匹配;落点语义对齐服务端 MoveFeed(空 beforeId=组内末尾)/
// RSSFolderOrder。行内子钮仍是 Tappable,点按不受影响。

type dndRow struct {
	widget.BaseWidget
	content  fyne.CanvasObject
	lineTop  *canvas.Rectangle // 落点指示线(=drop-before:inset 0 2px accent)
	lineBot  *canvas.Rectangle // 落点指示线(=drop-after)
	dim      *canvas.Rectangle // 拖源变暗(=.dragging opacity .4 的近似)
	hv       *canvas.Rectangle // 行悬停底(=.rss-folder/.rss-feed:hover 0.07)
	kind     string            // "feed" / "folder"
	id       string            // feed id / 目录 id(未分组为 "")
	name     string            // 行名(拖拽 ghost 文案)
	onTap    func()            // 行主体点击(caret/图标钮更深命中,不冒泡)
	rp       *rssPaneState
	dragging bool
	hg       *hoverGroup       // 行组:行内 caret/名称/操作钮悬停时保持整行高亮
}

func wrapDND(rp *rssPaneState, kind, id, name string, content fyne.CanvasObject, onTap func()) *dndRow {
	d := &dndRow{content: content, kind: kind, id: id, name: name, onTap: onTap, rp: rp}
	d.ExtendBaseWidget(d)
	accent := rp.app.store.appPal().accent
	d.lineTop = canvas.NewRectangle(accent)
	d.lineTop.CornerRadius = 1
	d.lineTop.SetMinSize(fyne.NewSize(10, 2))
	d.lineTop.Hide()
	d.lineBot = canvas.NewRectangle(accent)
	d.lineBot.CornerRadius = 1
	d.lineBot.SetMinSize(fyne.NewSize(10, 2))
	d.lineBot.Hide()
	if bg, ok := rp.app.store.appPal().bg.(color.NRGBA); ok {
		d.dim = canvas.NewRectangle(color.NRGBA{R: bg.R, G: bg.G, B: bg.B, A: 165})
	} else {
		d.dim = canvas.NewRectangle(color.NRGBA{A: 165})
	}
	d.dim.CornerRadius = 8
	d.dim.Hide()
	d.hv = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.07))
	d.hv.CornerRadius = 8
	d.hv.Hide()
	// 行组:指针移到行内 caret/名称/操作钮等子件时整行悬停不丢(=Z5 根治)
	d.hg = newHoverGroup(func(on bool) {
		if on {
			d.hv.Show()
		} else {
			d.hv.Hide()
		}
		d.hv.Refresh()
	})
	registerHoverGroup(d.content, d.hg)
	rp.dndRows = append(rp.dndRows, d)
	return d
}

func (d *dndRow) CreateRenderer() fyne.WidgetRenderer {
	stack := container.NewStack(d.dim, d.hv, d.content)
	return &dndRowRenderer{d: d, stack: stack}
}

// dndRowRenderer 指示线不放容器里:容器重排(含每次刷新的重跑)会把 2px
// 线拉满整行(=用户报的"整行蓝"),渲染器直接持有并在每次布局后钳位
type dndRowRenderer struct {
	d     *dndRow
	stack *fyne.Container
}

func (r *dndRowRenderer) Layout(size fyne.Size) {
	r.stack.Resize(size)
	r.clampLines(size)
}

func (r *dndRowRenderer) clampLines(size fyne.Size) {
	r.d.lineTop.Move(fyne.NewPos(2, 0))
	r.d.lineTop.Resize(fyne.NewSize(size.Width-4, 2))
	r.d.lineBot.Move(fyne.NewPos(2, size.Height-2))
	r.d.lineBot.Resize(fyne.NewSize(size.Width-4, 2))
}

func (r *dndRowRenderer) MinSize() fyne.Size { return r.stack.MinSize() }
func (r *dndRowRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.stack, r.d.lineTop, r.d.lineBot}
}
func (r *dndRowRenderer) Destroy() {}

func (r *dndRowRenderer) Refresh() {
	r.stack.Refresh()
	if s := r.d.Size(); s.Width > 0 {
		r.clampLines(s)
	}
}

// Tapped 行主体点击语义(=RssPane onPaneClick:feed 行=展开收起、
// 文件夹行=折叠切换;行内 caret/图标钮是更深命中,不冒泡到这)
func (d *dndRow) Tapped(*fyne.PointEvent) {
	if d.onTap != nil {
		d.onTap()
	}
}

func (d *dndRow) MouseIn(*desktop.MouseEvent) {
	d.hg.enter()
}

func (d *dndRow) MouseOut() {
	d.hg.leave()
}

func (d *dndRow) MouseMoved(*desktop.MouseEvent) {}

// hoverRow 行悬停底(=.rss-item:hover rgba(127,127,127,0.07);无拖放语义的行用)
type hoverRow struct {
	widget.BaseWidget
	content fyne.CanvasObject
	bg      *canvas.Rectangle
	onTap   func()
	hg      *hoverGroup // 行组:行内 caret/复选框/名称悬停时保持整行高亮
}

func wrapHover(pal palette, content fyne.CanvasObject) *hoverRow {
	h := &hoverRow{content: content}
	h.ExtendBaseWidget(h)
	h.bg = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.07))
	h.bg.CornerRadius = 8
	h.bg.Hide()
	h.hg = newHoverGroup(func(on bool) {
		if on {
			h.bg.Show()
		} else {
			h.bg.Hide()
		}
		h.bg.Refresh()
	})
	registerHoverGroup(h.content, h.hg)
	return h
}

// Tapped 条目行主体点击(=onPaneClick rss-item 分支:等价切换条目级
// 全选框;行内 caret/已读钮更深命中不冒泡)
func (h *hoverRow) Tapped(*fyne.PointEvent) {
	if h.onTap != nil {
		h.onTap()
	}
}

func (h *hoverRow) CreateRenderer() fyne.WidgetRenderer {
	return &simpleRenderer{obj: container.NewStack(h.bg, h.content), min: h.content.MinSize()}
}

func (h *hoverRow) MouseIn(*desktop.MouseEvent) {
	h.hg.enter()
}

func (h *hoverRow) MouseOut() {
	h.hg.leave()
}

func (h *hoverRow) MouseMoved(*desktop.MouseEvent) {}

// siblingsOf 组内订阅(Order 序,剔除拖源自身;feed 落点 after 取后一兄弟用)
func (rp *rssPaneState) siblingsOf(folderID, exceptID string) []rss.FeedView {
	out := []rss.FeedView{}
	for _, f := range rp.lastTree.Feeds {
		if f.FolderID == folderID && f.ID != exceptID {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func (d *dndRow) Dragged(e *fyne.DragEvent) {
	if !d.dragging {
		d.dragging = true
		d.dim.Show()
		d.Refresh()
		d.rp.dragBegin(d.kind, d.id, d, e.AbsolutePosition)
	}
	d.rp.dragOver(e)
}

func (d *dndRow) DragEnd() {
	d.dragging = false
	d.dim.Hide()
	d.Refresh()
	if d.rp.dragGhost != nil {
		d.rp.dragGhost.remove()
		d.rp.dragGhost = nil
	}
	d.rp.dragDrop()
}

// dragBegin 拖动开始(首个 Dragged 事件);随光标挂一块 ghost 卡片
// (=HTML5 拖拽拖影,互动感)
func (rp *rssPaneState) dragBegin(kind, id string, src *dndRow, pos fyne.Position) {
	rp.dragKind, rp.dragID, rp.dragSrc = kind, id, src
	rp.dropRow, rp.dropPos = nil, ""
	if rp.dragGhost != nil {
		rp.dragGhost.remove()
	}
	pal := rp.app.store.appPal()
	icon := "article"
	if kind == "folder" {
		icon = "folder"
	}
	lbl := newColorLabelS(src.name, pal.text, false, 12.5)
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 8
	bg.StrokeWidth = 1
	bg.StrokeColor = pal.borderStrong
	body := container.NewStack(bg,
		container.New(layout.NewCustomPaddedLayout(6, 6, 12, 12),
			hGap(8, msText(icon, pal.accent, 14), lbl)))
	g := newTipPop(body)
	m := body.MinSize()
	g.show(pos.Add(fyne.NewPos(12, 14)), fyne.NewSize(m.Width, m.Height))
	rp.dragGhost = g
}

// dragOver 悬停命中:绝对坐标匹配目标行,规则对齐 makeRssDnd——
// 订阅可落到任意文件夹/订阅前后;文件夹可落到其他文件夹前后(重排)。
// 落点按行对象身份跟踪而非 id:未分组行 id 为空串,与"无落点"初值相同,
// 用 id 比较会导致永远拖不进未分组(实测)
func (rp *rssPaneState) dragOver(e *fyne.DragEvent) {
	if rp.dragKind == "" {
		return
	}
	if rp.dragGhost != nil {
		rp.dragGhost.move(e.AbsolutePosition.Add(fyne.NewPos(12, 14)))
	}
	drv := fyne.CurrentApp().Driver()
	abs := e.AbsolutePosition
	var hitObj *dndRow
	for _, t := range rp.dndRows {
		if t == rp.dragSrc {
			continue // 落在自己身上
		}
		if rp.dragKind == "feed" && t.kind != "feed" && t.kind != "folder" {
			continue
		}
		if rp.dragKind == "folder" && t.kind != "folder" {
			continue
		}
		pos := drv.AbsolutePositionForObject(t)
		sz := t.Size()
		if sz.Width <= 0 || sz.Height <= 0 {
			continue
		}
		if abs.X >= pos.X && abs.X < pos.X+sz.Width && abs.Y >= pos.Y && abs.Y < pos.Y+sz.Height {
			hitObj = t
			break
		}
	}
	pos := ""
	if hitObj != nil {
		if abs.Y < drv.AbsolutePositionForObject(hitObj).Y+hitObj.Size().Height/2 {
			pos = "before"
		} else {
			pos = "after"
		}
	}
	if hitObj != rp.dropRow || pos != rp.dropPos {
		for _, t := range rp.dndRows {
			t.lineTop.Hide()
			t.lineBot.Hide()
			t.Refresh()
		}
		rp.dropRow, rp.dropPos = hitObj, pos
		if hitObj != nil {
			if pos == "before" {
				hitObj.lineTop.Show()
			} else {
				hitObj.lineBot.Show()
			}
			hitObj.Refresh()
		}
	}
}

// dragDrop 落手:按落点执行移动;无落点(拖空)只清理状态
func (rp *rssPaneState) dragDrop() {
	kind, id := rp.dragKind, rp.dragID
	drop, dropPos := rp.dropRow, rp.dropPos
	for _, t := range rp.dndRows {
		t.lineTop.Hide()
		t.lineBot.Hide()
		t.Refresh()
	}
	rp.dragKind, rp.dragID = "", ""
	rp.dropRow, rp.dropPos = nil, ""
	if kind == "" || drop == nil {
		return
	}
	dropKind, dropID := drop.kind, drop.id
	// 落点目录名(成功提示用;未分组为空串)
	folderName := func(fid string) string {
		if fid == "" {
			return "未分组"
		}
		for _, f := range rp.lastTree.Folders {
			if f.ID == fid {
				return f.Name
			}
		}
		return fid
	}
	switch {
	case kind == "feed" && dropKind == "folder":
		// 换文件夹:追加到组内末尾(beforeId 空)
		name := folderName(dropID)
		goSafe("rss.move", func() {
			if err := rp.app.client.RSSFeedMove(id, dropID, ""); err != nil {
				fyne.Do(func() { rp.app.toast("移动失败: "+err.Error(), "err") })
				return
			}
			fyne.Do(func() { rp.app.toast("已移动到「"+name+"」", "ok"); rp.reload() })
		})
	case kind == "feed" && dropKind == "feed":
		// 插到目标订阅前/后(beforeId=目标或其后一个兄弟,=rssDropFeed);
		// 目录取目标所属组
		folder := ""
		for _, f := range rp.lastTree.Feeds {
			if f.ID == dropID {
				folder = f.FolderID
				break
			}
		}
		if dropPos == "after" {
			sibs := rp.siblingsOf(folder, id)
			for i, sf := range sibs {
				if sf.ID == dropID && i+1 < len(sibs) {
					dropID = sibs[i+1].ID // 无下一个=空串=组内末尾
				}
			}
		}
		fid := folder
		goSafe("rss.move", func() {
			if err := rp.app.client.RSSFeedMove(id, fid, dropID); err != nil {
				fyne.Do(func() { rp.app.toast("移动失败: "+err.Error(), "err") })
				return
			}
			fyne.Do(func() { rp.app.toast("已调整顺序", "ok"); rp.reload() })
		})
	case kind == "folder" && dropKind == "folder":
		// 文件夹重排:插到目标目录前/后(=rssDropFolder)
		ids := make([]string, 0, len(rp.lastTree.Folders))
		for _, f := range rp.lastTree.Folders {
			if f.ID != id {
				ids = append(ids, f.ID)
			}
		}
		at := len(ids)
		for i, fid := range ids {
			if fid == dropID {
				at = i
				if dropPos == "after" {
					at = i + 1
				}
				break
			}
		}
		ids = append(ids, "")
		copy(ids[at+1:], ids[at:])
		ids[at] = id
		goSafe("rss.folderOrder", func() {
			if err := rp.app.client.RSSFolderOrder(ids); err != nil {
				fyne.Do(func() { rp.app.toast("移动失败: "+err.Error(), "err") })
				return
			}
			fyne.Do(func() { rp.app.toast("已调整文件夹顺序", "ok"); rp.reload() })
		})
	}
}

// childrenWrap 文件夹子树导引线(=.rss-children:margin-left 20/padding-left 10
// /border-left 1px 分隔线)
func childrenWrap(rows ...fyne.CanvasObject) fyne.CanvasObject {
	pal := currentRPal()
	line := canvas.NewRectangle(pal.border)
	line.SetMinSize(fyne.NewSize(1, 0))
	inner := container.New(&vgapLayout{gap: 3}, rows...)
	return container.New(layout.NewCustomPaddedLayout(0, 0, 20, 0),
		container.NewBorder(nil, nil, line, nil,
			container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0), inner)))
}

// itemRow 条目行 + 展开的资源链接(=rss-item:caret+未读点+标题/副行+已读切换)
func (rp *rssPaneState) itemRow(feedID string, it *rss.Item) []fyne.CanvasObject {
	pal := rp.app.store.appPal()
	out := []fyne.CanvasObject{}
	open := rp.openItems[it.ID]
	date := fmtItemDate(it.Date)
	sub := date
	if it.Summary != "" {
		if sub != "" {
			sub += " · "
		}
		sum := it.Summary
		if len([]rune(sum)) > 60 {
			sum = string([]rune(sum)[:60]) + "…"
		}
		sub += sum
	}
	title := it.Title
	if title == "" {
		title = "(无标题)"
	}
	// 未读:标题加粗白字 + accent 圆点;已读:置灰
	titleClr, tBold := pal.textDim, false
	dotClr := rgbaA(0x7f7f7f, 0.35)
	if !it.Read {
		titleClr, tBold = pal.text, true
		dotClr = pal.accent
	}
	dot := canvas.NewRectangle(dotClr)
	dot.CornerRadius = 4
	dot.SetMinSize(fyne.NewSize(8, 8))
	head := []fyne.CanvasObject{
		rssCaret(pal, open, func() { rp.openItems[it.ID] = !open; rp.rebuildFromState() }),
		dot,
		// 条目标题/副行截断悬停显全文(=rss-item :title)
		flex(container.NewVBox(
			newTipLabel(newColorLabelS(title, titleClr, tBold, 13).SetMaxWidth(), func() string { return title }),
			container.New(layout.NewCustomPaddedLayout(1, 0, 0, 0),
				newTipLabel(newColorLabelS(sub, pal.textFaint, false, 11).SetMaxWidth(), func() string { return sub })))),
		func() *iconButton {
			b := rssAct(pal, map[bool]string{true: "refresh", false: "check"}[it.Read], func() {
				go func() {
					if err := rp.app.client.RSSItemRead(it.ID, !it.Read); err == nil {
						fyne.Do(rp.reload)
					}
				}()
			})
			b.SetTip(map[bool]string{true: "标为未读", false: "标为已读"}[it.Read])
			return b
		}(),
	}
	// 折叠态条目级复选框 = 全选该条目资源链接(不含页面/磁力)
	if !open && len(rssItemRes(it)) > 0 {
		allChecked := true
		for _, l := range rssItemRes(it) {
			if _, has := rp.sel[l.URL]; !has {
				allChecked = false
				break
			}
		}
		cb := checkedCheck(pal, allChecked, func(on bool) {
			for _, l := range rssItemRes(it) {
				if on {
					rp.sel[l.URL] = rssLinkName(l)
					rp.selFeed[l.URL] = feedID
				} else {
					delete(rp.sel, l.URL)
					delete(rp.selFeed, l.URL)
				}
			}
			fyne.Do(rp.refreshSel)
		})
		head = append(head[:1], append([]fyne.CanvasObject{cb}, head[1:]...)...)
	}
	// 折叠态点主体=切换整条资源链接勾选(与条目级复选框同效果;展开态
	// 主体点击无动作,Vue 亦然)
	itemRow := wrapHover(pal,
		container.New(layout.NewCustomPaddedLayout(6, 6, 9, 9), hFlex(8, head...)))
	if !open {
		var cbAll *styledCheck
		for _, c := range head {
			if sc, ok := c.(*styledCheck); ok {
				cbAll = sc
				break
			}
		}
		if cbAll != nil {
			itemRow.onTap = func() { cbAll.Tapped(nil) }
		}
	}
	out = append(out, itemRow)
	if !open {
		return out
	}
	// 资源链接行(附件 + 页面候选;=rss-link:边框卡 + 勾选 + 名称 + 信息 + 动作)
	links := append([]rss.ItemLink{}, it.Links...)
	if it.Link != "" {
		has := false
		for _, l := range it.Links {
			if l.URL == it.Link {
				has = true
				break
			}
		}
		if !has {
			links = append(links, rss.ItemLink{URL: it.Link, Title: it.Title})
		}
	}
	if len(links) == 0 {
		out = append(out, indented("该条目没有可直接下载的链接"))
		return out
	}
	for _, l := range links {
		l := l
		isPage := l.URL == it.Link
		isMagnet := magnetRe.MatchString(l.URL)
		name := rssLinkName(l)
		tag := ""
		if isPage {
			tag = " 页面"
		} else if isMagnet {
			tag = " 磁力"
		}
		info := []string{}
		if l.Length > 0 {
			info = append(info, fmtBytes(l.Length))
		}
		if l.Type != "" {
			info = append(info, l.Type)
		}
		row := []fyne.CanvasObject{}
		if isPage {
			row = append(row, layout.NewSpacer())
		} else {
			cb := checkedCheck(pal, rp.selHas(l.URL), func(on bool) {
				if on {
					rp.sel[l.URL] = name
					rp.selFeed[l.URL] = feedID
				} else {
					delete(rp.sel, l.URL)
					delete(rp.selFeed, l.URL)
				}
				fyne.Do(rp.refreshSel)
			})
			if isMagnet {
				cb.Disable()
			}
			row = append(row, cb)
		}
		var act *textBtn
		if isPage {
			act = newGhostButton(pal, "预览", func() {
				if u, perr := url.Parse(l.URL); perr == nil {
					rp.app.a.OpenURL(u)
				}
			})
		} else {
			act = newGhostButton(pal, "下载", func() { rp.downloadOne(l) })
		}
		act.SetMini()
		row = append(row,
			flex(newTipLabel(newColorLabelS(name+tag, pal.text, false, 12.5).SetMaxWidth(), func() string { return name+tag })),
			newColorLabelS(strings.Join(info, " · "), pal.textFaint, false, 11),
			act)
		cardBg := canvas.NewRectangle(color.NRGBA{A: 0})
		cardBg.CornerRadius = 8
		cardBg.StrokeWidth = 1
		cardBg.StrokeColor = pal.border
		if rp.selHas(l.URL) {
			cardBg.StrokeColor = rgbaA(0x4f8cff, 0.5)
			cardBg.FillColor = rgbaA(0x4f8cff, 0.07)
		}
		card := container.NewStack(cardBg,
			container.New(layout.NewCustomPaddedLayout(6, 6, 10, 10), hFlex(9, row...)))
		out = append(out, container.New(layout.NewCustomPaddedLayout(0, 0, 34, 0), card))
	}
	return out
}

// checkedCheck 带初始勾选态的复选框(自绘样式,=rss 勾选框)
func checkedCheck(pal palette, checked bool, on func(bool)) *styledCheck {
	return newStyledCheck(pal, "", checked, on)
}

func (rp *rssPaneState) selHas(url string) bool {
	_, has := rp.sel[url]
	return has
}

// refreshSel 吸底勾选条
func (rp *rssPaneState) refreshSel() {
	n := len(rp.sel)
	show := n > 0
	if show {
		rp.selNum.SetText(fmt.Sprintf("%d", n))
	}
	// 仅显隐真正翻转时才刷新根布局。Border 布局会跳过隐藏的底槽
	// (borderLayout.Layout 对 !Visible 的 bottom 不分配),隐藏→可见时若不同步
	// 刷新 rp.root,center 滚动区仍占满、selBar 停在旧 0 尺寸——表现就是"首次
	// 勾选吸底条不出现,须悬停一下才刷新"。已可见时再勾选只改数字,不重排 center
	if show != rp.selBar.Visible() {
		if show {
			rp.selBar.Show()
		} else {
			rp.selBar.Hide()
		}
		rp.root.Refresh()
	}
}

// downloadOne 单链接下载(=dl-one:磁力走 BT 窗,普通走新建任务窗)
func (rp *rssPaneState) downloadOne(l rss.ItemLink) {
	if magnetRe.MatchString(l.URL) {
		if rp.app.store.OpenBT != nil {
			rp.app.store.OpenBT(l.URL, "", "")
		}
		return
	}
	rp.batchDownloadLinks([]rss.ItemLink{l})
}

// batchDownload 批量下载勾选(=sel-download/rssDownload:磁力跳过提示,
// 普通链接批量预填新建任务窗)
func (rp *rssPaneState) batchDownload() {
	links := make([]rss.ItemLink, 0, len(rp.sel))
	for u, name := range rp.sel {
		links = append(links, rss.ItemLink{URL: u, Title: name})
	}
	if len(links) == 0 {
		return
	}
	rp.batchDownloadLinks(links)
	rp.sel = map[string]string{}
	rp.selFeed = map[string]string{}
	rp.refreshSel()
}

func (rp *rssPaneState) batchDownloadLinks(links []rss.ItemLink) {
	var mags, urls []rss.ItemLink
	for _, l := range links {
		if magnetRe.MatchString(l.URL) {
			mags = append(mags, l)
		} else {
			urls = append(urls, l)
		}
	}
	if len(urls) == 0 {
		if len(mags) > 0 && rp.app.store.OpenBT != nil {
			rp.app.store.OpenBT(mags[0].URL, "", "")
		}
		return
	}
	list := make([]string, 0, len(urls))
	for _, l := range urls {
		list = append(list, l.URL)
	}
	if rp.app.store.OpenNewTask != nil {
		rp.app.store.OpenNewTask(PendingNewTask{URLs: list, Stay: true})
	}
	if len(mags) > 0 {
		rp.app.toast(fmt.Sprintf("已跳过 %d 个磁力链接(请单独创建 BT 任务)", len(mags)), "")
	}
}

/* ---------------- 订阅/文件夹编辑对话框 ---------------- */

// openFeedDialog 添加/编辑订阅(标题/文件夹/下载目录/自动下载)
func (rp *rssPaneState) openFeedDialog(folderID string, edit *rss.FeedView) {
	pal := rp.app.store.appPal()
	urlE := widget.NewEntry()
	urlE.SetPlaceHolder("RSS / Atom 地址")
	singleLine(urlE) // 横向裁剪、无纵向滚动条(=input)
	titleE := widget.NewEntry()
	titleE.SetPlaceHolder("留空用频道标题")
	singleLine(titleE)
	folderSel := newZSelect(pal, nil, "") // 选项在拉取订阅树后经 SetOptions 填充
	var folderIDs []string
	tree, _ := rp.app.client.RSS()
	opts := []string{"未分组"}
	folderIDs = append(folderIDs, "")
	for _, fo := range tree.Folders {
		opts = append(opts, fo.Name)
		folderIDs = append(folderIDs, fo.ID)
	}
	folderSel.Options = opts
	pickFolder := func(name string) string {
		for i, o := range opts {
			if o == name {
				return folderIDs[i]
			}
		}
		return ""
	}
	// 目录用 dirField(=Vue DirField)+ 右侧"浏览…"(=rssFeedPickDir):可自由输入/粘贴 + ▾ 选最近目录
	dirE := NewDirField(rp.app, "留空跟随全局默认下载目录", "")
	dirBrowse := newGhostButton(pal, "浏览…", func() {
		rp.app.pickFolderAsync("选择订阅下载目录", func(p string, ok bool) {
			if !ok {
				return
			}
			fyne.Do(func() { dirE.SetText(p) })
		})
	}).SetMini()
	// 自动下载(=AUTODL_OPTS 值/标签分离):zSelect 的 Selected 存显示
	// 标签,提交前经 autoDlVal 映回接口值;编辑回显用 autoDlLbl 反查
	autoDlVals := []string{"inherit", "off", "notify", "silent"}
	autoDlLbls := []string{"继承全局", "不自动下载", "自动下载(完成后提醒)", "静默下载"}
	autoDlVal := func(lbl string) string {
		for i, l := range autoDlLbls {
			if l == lbl {
				return autoDlVals[i]
			}
		}
		return "inherit"
	}
	autoDlLbl := func(v string) string {
		for i, val := range autoDlVals {
			if val == v {
				return autoDlLbls[i]
			}
		}
		return autoDlLbls[0]
	}
	autoDl := rp.app.selectW(autoDlLbls, autoDlLbls[0])

	curFolder := folderID
	if edit != nil {
		curFolder = edit.FolderID
		urlE.SetText(edit.URL)
		urlE.Disable()
		titleE.SetText(edit.Title)
		dirE.SetText(edit.DownloadDir)
		autoDl.SetSelected(autoDlLbl(edit.AutoDL))
	}
	folderSel.OnChanged = func(s string) { curFolder = pickFolder(s) }
	// 回显当前文件夹名
	for i, id := range folderIDs {
		if id == curFolder {
			folderSel.SetSelected(opts[i])
			break
		}
	}

	title := "添加订阅"
	okLabel := "添加"
	if edit != nil {
		title, okLabel = "编辑订阅", "保存"
	}
	// =dialog 基类外壳:accent-bar 标题 + 纵向字段 + 右下动作(=RssDialogs)
	var pop *maskModal
	okBtn := newPrimaryButton(pal, okLabel, nil)
	submit := func() {
		go func() {
			if edit != nil {
				_, err := rp.app.client.RSSUpdateFeed(edit.ID, strPtr(titleE.Text), strPtr(curFolder), strPtr(dirE.Text()), strPtr(autoDlVal(autoDl.Selected)))
				fyne.Do(func() {
					if err != nil {
						rp.app.toast(err.Error(), "err")
						return
					}
					rp.app.toast("订阅已更新", "ok")
					fyne.Do(rp.reload)
				})
				return
			}
			if strings.TrimSpace(urlE.Text) == "" {
				fyne.Do(func() { rp.app.toast("请输入订阅地址", "err") })
				return
			}
			_, err := rp.app.client.RSSAddFeed(strings.TrimSpace(urlE.Text), curFolder, strings.TrimSpace(titleE.Text), dirE.Text(), autoDlVal(autoDl.Selected))
			fyne.Do(func() {
				if err != nil {
					rp.app.toast(err.Error(), "err")
					return
				}
				rp.app.toast("订阅已添加,正在抓取", "ok")
				fyne.Do(rp.reload)
			})
		}()
		pop.Hide()
	}
	okBtn.onTap = submit
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS(title, pal.text, true, 17))
	field := func(label string, w fyne.CanvasObject) fyne.CanvasObject {
		return container.NewVBox(newColorLabelS(label, pal.textDim, true, 12.5),
			container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0), w))
	}
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }), okBtn)
	body := container.New(&vgapLayout{gap: 12}, head,
		field("订阅地址", urlE), field("显示标题", titleE), field("文件夹", folderSel),
		field("下载目录", container.NewBorder(nil, nil, nil, dirBrowse, dirE)),
		field("自动下载", autoDl),
		container.NewBorder(nil, nil, nil, actions))
	// 贴高:fyne 滚动容器 min 高只有 32,须 SetMinSize 喂给 fixedMin 卡体
	// (同新建任务窗;超 560 转内部滚动)
	sc := container.NewVScroll(container.New(
		layout.NewCustomPaddedLayout(22, 22, 24, 24), body))
	h := sc.Content.MinSize().Height
	if h > 560 {
		h = 560
	}
	sc.SetMinSize(fyne.NewSize(0, h))
	pop = modalCard(rp.app.win, pal, sc, 560, 0)
	pop.Show()
}

// openFolderDialog 新建/重命名文件夹
func (rp *rssPaneState) openFolderDialog(editID string) {
	e := widget.NewEntry()
	singleLine(e) // 横向裁剪、无纵向滚动条(=input)
	title, okLabel := "新建文件夹", "创建"
	if editID != "" {
		title, okLabel = "重命名文件夹", "保存"
		tree, _ := rp.app.client.RSS()
		for _, fo := range tree.Folders {
			if fo.ID == editID {
				e.SetText(fo.Name)
			}
		}
	}
	pal := rp.app.store.appPal()
	var pop *maskModal
	okBtn := newPrimaryButton(pal, okLabel, nil)
	okBtn.onTap = func() {
		name := strings.TrimSpace(e.Text)
		if name == "" {
			return
		}
		go func() {
			var err error
			if editID != "" {
				err = rp.app.client.RSSFolderRename(editID, name)
			} else {
				err = rp.app.client.RSSFolderAdd(name)
			}
			fyne.Do(func() {
				if err != nil {
					rp.app.toast(err.Error(), "err")
					return
				}
				fyne.Do(rp.reload)
			})
		}()
		pop.Hide()
	}
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS(title, pal.text, true, 17))
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }), okBtn)
	body := container.New(&vgapLayout{gap: 12}, head,
		container.NewVBox(newColorLabelS("名称", pal.textDim, true, 12.5),
			container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0), e)),
		container.NewBorder(nil, nil, nil, actions))
	inner := container.New(layout.NewCustomPaddedLayout(22, 22, 24, 24), body)
	// 高=0 让卡体贴内容(=Vue width-only 对话框);固定 240 会留一大块空白(=用户报"太高")
	pop = modalCard(rp.app.win, pal, inner, 420, 0)
	pop.Show()
}

func strPtr(s string) *string { return &s }
