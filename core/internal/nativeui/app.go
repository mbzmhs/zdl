package nativeui

// App:Fyne 外壳组装。对照 App.vue 结构:TopBar / layout(SideBar + main(
// TaskList))/ 弹层群。任务卡逐字段对齐 TaskCard.vue(数值口径/动作组合/
// 标题点击路由);弹窗群按里程碑逐个接入,未接入项点击给"建设中"提示。

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"

	"zdownloader/internal/assets"
	"zdownloader/internal/engine"
	"zdownloader/internal/winfd"
)

// App 原生外壳
type App struct {
	store  *Store
	client *Client
	a      fyne.App
	win    fyne.Window

	// 顶栏可变部件(快照驱动刷新)
	speedDown *colorLabel
	speedUp   *colorLabel
	connBadge *colorLabel
	connBg    *canvas.Rectangle
	verBadge  *colorLabel
	chart     *speedChart

	// 侧栏计数(快照驱动)
	navRows   map[string]*navRow
	sideBox   *fyne.Container   // 侧栏行容器(测宽用)
	slideObj  fyne.CanvasObject // 滑动指示条(=nav-slide-ind)
	slideY    float32
	slideInit bool
	slideAnim *fyne.Animation

	// 任务列表:按 id 池化卡片(每快照就地更新,避免整列重建闪烁)
	listBox    *fyne.Container // VBox 容器
	cards      map[string]*taskCard
	emptyState *fyne.Container // 空状态(图标+提示,=.empty)
	emptyHint  *widget.Label
	emptyIcon  *canvas.Text

	// 主界面内容(播放页接管/恢复用)
	mainContent fyne.CanvasObject
	player      *playerPane // 挂载中的播放页(ESC 关闭路由;nil=未挂载)

	// 全局 toast(右下角堆叠,见 toast;面板挂在内容树顶层浮层)
	toastCards []fyne.CanvasObject

	// RSS 视图(M8;懒建,随视图切换显隐)
	rss *rssPaneState

	// 设置窗为全局单例(openSettings 幂等守卫用):当前在屏的设置遮罩层。
	// 托盘「设置」/工具栏按钮/uiShow 帧多次触发时不得叠新层——每层都是整屏
	// 遮罩,叠层后用户只能逐层点遮罩才能关完
	settingsPop *maskModal

	// 主题(切换即整窗重设)
	themeObj *zTheme

	// 主窗 HWND(原生对话框归属)。惰性获取:NewApp 时窗口尚未创建,
	// RunNative 回调拿到的 HWND 为 0(实测:对话框因此桌面居中、无归属);
	// 首次经 HWND() 在窗口创建后取回并缓存
	hwnd     uintptr
	hwndOnce sync.Once

	// onPreShow 入口层注入的"窗口显示前"回调:Show 前激活 Win32 位置"钉位"——
	// Fyne 的 Show 内部先 doCenterOnScreen(SetWindowPos 居中)再上屏,居中帧先于
	// 任何"显示后"回调,显示后才移必然闪一帧;唯有在 Show 前把目标坐标交给 Win32
	// 窗口过程,令 Fyne 的 SetWindowPos 在生效前就被改写到保存位,首帧即停保存位。
	onPreShow func()
	// onShown 入口层注入的"窗口显示后"回调:补 Fyne 缺的动作——关闭钉位、Win32
	// 提到最前(RequestFocus 只给输入焦点、不拉前台),并兜底再校一次位置/前置。
	onShown func()
	// themePersist 入口层注入的"主题切换落盘"回调:每次 SetTheme(顶栏调色板按钮)
	// 后调用。UI 层不依赖 engine/tray,持久化(conf.Theme)与托盘菜单明暗跟随由
	// 入口层经此反向注入(与 onPreShow/onShown 同模式)。
	themePersist func(t string)
}

// appIconImage 品牌 logo 位图(仅解码一次;canvas.Image 挂 Resource 会被
// 反复走 renderSVG 刷新路径,是常驻内存增长源之一)
var (
	appIconOnce sync.Once
	appIconImg  image.Image
)

func appIconImage() image.Image {
	appIconOnce.Do(func() {
		appIconImg, _, _ = image.Decode(bytes.NewReader(assets.AppIcon))
	})
	return appIconImg
}

// HWND 主窗原生句柄(0 = 不可用)。可在任意 goroutine 调用:RunNative
// 回调派发到主线程,主循环泵消息后即返回。
func (app *App) HWND() uintptr {
	app.hwndOnce.Do(func() { app.hwnd = NativeHWND(app.win) })
	return app.hwnd
}

// NewApp 组装外壳。run 后阻塞;onExit 供"关闭即退出"模式使用。
func NewApp(c *Client, s *Store) *App {
	a := fyneapp.New() // 显式创建(CurrentApp 在首次创建前为 nil)
	app := &App{store: s, client: c, a: a, cards: map[string]*taskCard{}}
	app.themeObj = newTheme(s.Theme() != "light")
	a.Settings().SetTheme(app.themeObj)

	app.win = a.NewWindow("ZDownloader")
	// 去掉 Fyne 默认的内容内边距(theme padding × DPI,实测约 7~8px):此前
	// 窗口四周露出一圈画布底色,模态遮罩画在内容里盖不到它(=用户报的
	// "窗体外层周边框遮罩无法覆盖");SetPadded(false) 后内容与遮罩铺满整个
	// 窗体,也与 Vue 版内容贴边一致。侧栏/顶栏自绘背景本就按全窗设计,无副作用。
	app.win.SetPadded(false)
	app.win.Resize(fyne.NewSize(1180, 760)) // 与 Wails 壳默认尺寸一致
	app.win.SetIcon(appIconResource())
	// ESC 关最上层弹窗(=Vue overlay 的 Esc 语义)。fyne 无修饰键不走 shortcut
	// 通道,且按键优先派发给焦点对象——输入框聚焦时需先点空白处再按 ESC。
	app.win.Canvas().SetOnTypedKey(func(e *fyne.KeyEvent) {
		if e.Name == fyne.KeyEscape {
			// 播放页接管窗口时不是弹层,hideTopModal 够不着——ESC 优先关它
			// (=Vue PlayerPane 的 Esc 语义)
			if app.player != nil {
				app.player.close()
				return
			}
			hideTopModal()
		}
	})

	// store 钩子注入(UI 回调必须经 fyne.Do——Fyne 2.8 线程模型)
	s.OnSnapshot = func() { fyne.Do(func() { app.applySnapshot() }) }
	s.OnConn = func(ok bool) { fyne.Do(func() { app.setConn(ok) }) }
	s.OnToast = func(msg, cls string) { fyne.Do(func() { app.toast(msg, cls) }) }
	s.OnConfirm = func(req ConfirmReq) { fyne.Do(func() { app.confirm(req) }) }
	s.palFn = func() palette { return app.themeObj.palv }
	s.OnThemeChange = func(t string) {
		fyne.Do(func() {
			app.themeObj.dark = t != "light"
			// 色板必须重建:palv 只在 newTheme 时算过一次,只翻 dark 标志
			// 等于什么都没换(=用户报的「点切换主题没反应」)
			app.themeObj.palv = newPalette(app.themeObj.dark)
			// 严禁在此再调 a.Settings().SetTheme(themeObj)!那会触发 Fyne 的
			// 全局重主题:ClearFontCache 清空字体缓存 + ApplySettingsWithCallback
			// 全树重渲染,于是每段文本测量都重新 parse 雅黑子集的 go-text 字体面,
			// 新解析的 outline 表缓冲(*SimpleGlyph.parsePoints / *Loader.findTableBuffer)
			// 挂在 fontCache 上永不回收——实测每切一次净增 ~7MB,40 次切换 +294MB。
			// zTheme.Color/Font 全读 in-place 的 palv/dark/font,且字体资源两主题同一份
			// (不随变体变),所以下面 buildRoot+SetContent 重建即让自研与内建组件都换色,
			// 无需 Fyne 级重主题;启动时那一次 SetTheme(见上方)已把 zTheme 设为 Fyne 主题。
			// 自绘组件的取色多为构造期快照,整体重建主界面树;开着的模态
			// 挂在旧树上,先关掉防陈旧遮罩
			clearModals()
			// RSS 面板是懒建缓存,rp.root 不随 buildRoot 重建——标题/按钮/
			// 正文行的色板快照须在此单独重刷(=亮主题下 RSS 页灰白看不清)
			if app.rss != nil {
				app.rss.retheme()
			}
			app.mainContent = app.buildRoot()
			app.win.SetContent(app.mainContent)
			// buildRoot 会把视图复位到默认任务页;重放快照恢复当前视图
			// (停留在 RSS 不被踢回)并立即重填/重着色任务列表
			app.applySnapshot()
			// 切换落盘(conf.Theme)+ 托盘菜单明暗跟随:engine/tray 由入口层
			// 经 themePersist 注入,UI 层不反向依赖(该回调自身做磁盘 IO 与
			// 跨线程标志写入,均在可接受的微秒级)
			if app.themePersist != nil {
				app.themePersist(t)
			}
		})
	}
	// 弹窗钩子:创建对话框本回合接入;BT/详情/RSS 后续里程碑
	s.OpenNewTask = func(p PendingNewTask) { fyne.Do(func() { app.openNewTaskDialog(p) }) }
	s.OpenBT = func(target, dir, fromTaskID string) {
		fyne.Do(func() { app.openBTDialog(target, dir, fromTaskID) })
	}
	s.OpenDetail = func(id string) {
		fyne.Do(func() { app.openDrawer(id) })
	}
	s.OpenPlayer = func(taskID, file string) {
		fyne.Do(func() { app.openPlayer(taskID, file) })
	}
	s.OnChoose = func(id, torrentFile, dir string) {
		// choosing 弹窗路由:有种子副本走 BT 对话框预填(创建后清挂起任务),
		// 否则改选窗
		if torrentFile != "" {
			fyne.Do(func() { app.openBTDialog(torrentFile, dir, id) })
		} else {
			fyne.Do(func() { app.openChooseDialog(id) })
		}
	}

	app.mainContent = app.buildRoot()
	app.win.SetContent(app.mainContent)
	s.Boot()
	return app
}

// Run 显示窗口并进入事件循环(阻塞)
func (app *App) Run() {
	app.win.ShowAndRun()
}

// RunLoop 只跑事件循环、不显示窗口(阻塞):静默 -tray 自启用。窗口初始隐藏、
// 常驻托盘,由 ShowUI(托盘左键/设置)经 win.Show 唤起。Fyne App.Run 启动事件
// 循环但不 Show,故进程随隐藏窗口常驻(事件循环不因窗口未上屏而退出),点托盘
// 才上屏——这正是"登录后静默常驻托盘"的语义。
func (app *App) RunLoop() {
	app.a.Run()
}

// Window 供入口层接窗口位置记忆/关闭拦截
func (app *App) Window() fyne.Window { return app.win }

// restoreMainContent 播放页关闭后恢复主界面
func (app *App) restoreMainContent() {
	app.win.SetContent(app.mainContent)
}

// ShowUI 唤起窗口置前(单实例转交/扩展「打开管理界面」)。Show 前调 onPreShow
// 激活 Win32 位置钉位(令 Fyne 居中/复位的 SetWindowPos 在生效前被改写到保存位,
// 首帧即停保存位),Show 后同帧调 onShown 关闭钉位并补"提到最前"(RequestFocus 只
// 给输入焦点、不拉前台)。托盘唤起、单实例转交、普通启动自动显示都经此,统一覆盖。
func (app *App) ShowUI(settings bool) {
	fyne.Do(func() {
		activateApp() // darwin:后台应用先激活再显窗(激活可能被系统缓办)
		if app.onPreShow != nil {
			app.onPreShow()
		}
		app.win.Show()
		app.win.RequestFocus()
		raiseWindowsToFront() // darwin:激活被缓办时的窗级置前兜底
		if app.onShown != nil {
			app.onShown()
		}
		if settings {
			app.openSettings() // 托盘「设置」入口
		}
	})
}

// SetOnPreShow 注入"窗口显示前"回调(入口层激活 Win32 位置钉位)。
func (app *App) SetOnPreShow(fn func()) {
	app.onPreShow = fn
}

// SetOnShown 注入"窗口显示后"回调(入口层关闭钉位、补 Win32 提到最前 / 兜底校位)。
func (app *App) SetOnShown(fn func()) {
	app.onShown = fn
}

// SetOnThemePersist 注入"主题切换落盘"回调(入口层写 conf.Theme 并同步托盘菜单明暗)。
func (app *App) SetOnThemePersist(fn func(t string)) {
	app.themePersist = fn
}

// PickDirNative 服务钩子(server.PickDir):HTTP goroutine 同步调用。
// 主循环持续泵消息,工作线程模态不会死锁(线程规则见下节)
func (app *App) PickDirNative(title string) (string, bool) {
	if p, ok, err := winfd.PickFolder(winfd.Options{Title: title, Owner: app.HWND()}); err == nil && ok {
		return p, true
	}
	r := make(chan string, 1)
	fyne.DoAndWait(func() {
		d := dialog.NewFolderOpen(func(uri fyne.ListableURI, ferr error) {
			if ferr != nil || uri == nil {
				r <- ""
				return
			}
			r <- uri.Path()
		}, app.win)
		d.Resize(fyne.NewSize(760, 520))
		d.Show()
	})
	p := <-r
	return p, p != ""
}

// SaveTorrentNative 服务钩子(server.SaveTorrent):原生另存
func (app *App) SaveTorrentNative(data []byte, suggested string) (string, error) {
	return app.saveFileNative(data, suggested)
}

/* ---------------- 原生文件/目录选择器 ----------------
 * 统一走 winfd(COM IFileDialog,资源管理器同款);COM 失败回退 Fyne 内置。
 *
 * 线程规则(实测踩坑):UI 线程内绝不能直接弹 winfd 对话框——对话框的模态
 * 消息泵会重入 Fyne 正在执行的事件回调,嵌套泵死锁(窗口直接卡死)。
 * 因此 UI 线程只发起、立即返回,对话框在工作线程弹出,结果经回调送回
 * (fyne.Do 回 UI);主循环持续泵消息,跨线程 WM_ENABLE 才能被处理。
 * HTTP 钩子路径(扩展转交)保持同步:主循环本来就在泵,无死锁面。 */

// pickFolderAsync 目录选择:cb 在后台 goroutine 回调(ok=false=取消/失败)
func (app *App) pickFolderAsync(title string, cb func(p string, ok bool)) {
	go func() {
		p, ok, err := winfd.PickFolder(winfd.Options{Title: title, Owner: app.HWND()})
		if err == nil && ok {
			cb(p, true)
			return
		}
		if err != nil && !errors.Is(err, winfd.ErrCancelled) {
			log.Printf("[nativeui] winfd 目录选择失败,回退 Fyne 兜底: %v", err)
			// COM 失败才走 Fyne 兜底:UI 线程未被阻塞,DoAndWait 安全
			fyne.DoAndWait(func() {
				d := dialog.NewFolderOpen(func(uri fyne.ListableURI, ferr error) {
					if ferr != nil || uri == nil {
						cb("", false)
						return
					}
					cb(uri.Path(), true)
				}, app.win)
				d.Resize(fyne.NewSize(760, 520))
				d.Show()
			})
			return
		}
		cb("", false)
	}()
}

// pickFileAsync 文件选择(filterName/filterSpec 形如 "种子文件"/"*.torrent");
// cb 在后台 goroutine 回调,拿到完整字节数据
func (app *App) pickFileAsync(title, filterName, filterSpec string, cb func(data []byte, path string, ok bool)) {
	go func() {
		p, ok, err := winfd.PickFile(winfd.Options{
			Title: title, Owner: app.HWND(),
			FilterName: filterName, FilterSpec: filterSpec,
		})
		if err == nil && ok {
			if data, rerr := os.ReadFile(p); rerr == nil {
				cb(data, p, true)
				return
			}
		}
		if err != nil && !errors.Is(err, winfd.ErrCancelled) {
			log.Printf("[nativeui] winfd 文件选择失败,回退 Fyne 兜底: %v", err)
			var ext string
			if strings.HasPrefix(filterSpec, "*.") {
				ext = filterSpec
			}
			var out []byte
			var path string
			fyne.DoAndWait(func() {
				d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, ferr error) {
					if ferr != nil || rc == nil {
						return
					}
					defer rc.Close()
					data, rerr := io.ReadAll(rc)
					if rerr == nil {
						out, path = data, rc.URI().Path()
					}
				}, app.win)
				if ext != "" {
					d.SetFilter(storage.NewExtensionFileFilter([]string{ext}))
				}
				d.Resize(fyne.NewSize(760, 520))
				d.Show()
			})
			if out != nil {
				cb(out, path, true)
				return
			}
		}
		cb(nil, "", false)
	}()
}

// saveFileNative 另存(仅 HTTP 钩子线程调用;取消返回 "已取消")
func (app *App) saveFileNative(data []byte, suggested string) (string, error) {
	ext := strings.ToLower(filepath.Ext(suggested))
	if p, ok, err := winfd.SaveFile(winfd.Options{
		Title: "另存为", Owner: app.HWND(),
		FileName: suggested, DefaultExtension: ext,
	}); err == nil && ok {
		if werr := os.WriteFile(p, data, 0o644); werr != nil {
			return "", werr
		}
		return p, nil
	}
	r := make(chan string, 1)
	fyne.DoAndWait(func() {
		d := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
			if err != nil || w == nil {
				r <- "已取消"
				return
			}
			p := w.URI().Path()
			_, werr := w.Write(data)
			w.Close()
			if werr != nil {
				r <- "已取消"
				return
			}
			r <- p
		}, app.win)
		if suggested != "" {
			d.SetFileName(suggested)
		}
		d.Resize(fyne.NewSize(760, 520))
		d.Show()
	})
	p := <-r
	if p == "已取消" {
		return p, nil
	}
	if p == "" {
		return "", fmt.Errorf("保存失败")
	}
	return p, nil
}

/* ---------------- 根布局 ---------------- */

func (app *App) buildRoot() fyne.CanvasObject {
	top := app.buildTopBar()
	side := app.buildSideBar()
	app.listBox = container.New(&vgapLayout{gap: 10, maxW: 1080})
	listScroll := container.NewVScroll(app.listBox)
	listScroll.SetMinSize(fyne.NewSize(640, 10))
	// 空状态(=.empty:图标 76px @0.4 + 提示 13px,整体垂直居中)
	pal0 := app.store.appPal()
	tx := pal0.text.(color.NRGBA)
	app.emptyHint = widget.NewLabel("")
	app.emptyHint.Alignment = fyne.TextAlignCenter
	app.emptyIcon = msText("download", color.NRGBA{R: tx.R, G: tx.G, B: tx.B, A: 102}, 76)
	emptyCol := container.NewVBox(
		container.NewCenter(app.emptyIcon),
		container.NewCenter(app.emptyHint))
	app.emptyState = container.NewCenter(emptyCol)
	rp := app.ensureRSS()
	main := container.NewStack(listScroll, app.emptyState, rp.root)
	rp.root.Hide() // 默认任务视图
	// .main{padding:20px 26px 40px}:RSS 面板同在 main 内,一并吃到
	padded := container.New(layout.NewCustomPaddedLayout(20, 40, 26, 26), main)
	body := container.NewBorder(nil, nil, side, nil, padded)
	// 内容树顶层浮层(toast/tooltip/拖影):绝不能做 overlay——fyne 命中
	// 分发只遍历 Overlays().Top() 一棵树,任何 overlay(哪怕全透明)都会
	// 让整窗内容不可达;内容树的全局命中搜索会穿过无命中的兄弟分支
	floatLayer = container.NewWithoutLayout()
	// 模态层(内容树,主 UI 之上/浮层之下):overlay 会画在 floatLayer
	// 上方,弹窗期间 toast 被遮罩盖没(=Vue toast 恒在 dialog 之上)
	modalLayer = container.New(&fullLayout{})
	// 根部一次性去滚动投影:主窗内所有滚动区(任务列表/RSS/侧栏)的边缘
	// 黑影都来自 Scroll 的 Shadow 色;遮罩色不受影响
	return container.NewStack(
		container.NewBorder(top, nil, nil, nil, body),
		modalLayer,
		floatLayer)
}

/* ---------------- 顶栏(对照 TopBar.vue) ---------------- */

func (app *App) buildTopBar() fyne.CanvasObject {
	pal := app.themeObj.palv
	app.verBadge = newColorLabelS("v"+app.store.Version(), pal.textFaint, false, 11)
	app.speedDown = newColorLabelS("—", pal.text, true, 14)
	app.speedUp = newColorLabelS("—", pal.text, true, 14)
	app.speedDown.t.Alignment = fyne.TextAlignTrailing
	app.speedUp.t.Alignment = fyne.TextAlignTrailing
	// 徽章初值跟当前连接态:主题重建主树时 OnConn 不会重发,写死「断开」
	// 会一直挂着红色假警报
	app.connBadge = newColorLabelS("连接断开,重连中…", pal.errc, true, 11)
	app.connBg = canvas.NewRectangle(rgbaA(0xf87171, 0.12))
	app.connBg.CornerRadius = 10
	if app.store.Conn() {
		app.connBadge.SetText("已连接")
		app.connBadge.SetColor(pal.ok)
		app.connBg.FillColor = rgbaA(0x34d399, 0.12)
	}
	app.chart = newSpeedChart(app.store)

	// 品牌(=brand:logo 34 + 名称 17 粗 + 版本 11 淡内联)
	logo := canvas.NewImageFromImage(appIconImage()) // 一次性解码,杜绝 SVG 重渲染路径
	logo.SetMinSize(fyne.NewSize(34, 34))
	logo.FillMode = canvas.ImageFillContain
	brand := hGap(10, container.NewStack(logo),
		hGap(6, newColorLabelS("ZDownloader", pal.text, true, 17), app.verBadge))

	// 上下行速率(=speed-number:色点 8px + 标签 11px + 数值 14px 粗、
	// 右缘齐列 min-width:76px;下行点为 accent 蓝)
	speedRow := func(dotC color.Color, lbl string, val *colorLabel) fyne.CanvasObject {
		d := canvas.NewRectangle(dotC)
		d.CornerRadius = 4
		d.SetMinSize(fyne.NewSize(8, 8))
		return hGap(6, d, newColorLabelS(lbl, pal.textFaint, false, 11),
			newFixedMin(container.NewStack(val), fyne.NewSize(76, 18)))
	}
	speeds := container.NewVBox(
		speedRow(pal.accent, "下载", app.speedDown),
		speedRow(pal.ok, "上传", app.speedUp))

	// 连接徽章(=conn-badge:半透明底药丸)
	conn := container.NewStack(app.connBg,
		container.New(layout.NewCustomPaddedLayout(3, 3, 10, 10), app.connBadge))

	themeBtn := newIconButtonSZ(pal, "palette", 36, 18, func() {
		if app.store.Theme() == "light" {
			app.store.SetTheme("dark")
		} else {
			app.store.SetTheme("light")
		}
	})
	themeBtn.SetTip("切换主题")
	settingsBtn := newIconButtonSZ(pal, "settings", 36, 18, func() { app.openSettings() })
	settingsBtn.SetTip("设置")

	// 分体主按钮(=btn-split:渐变画在整块上,主钮+分隔线+箭头子钮)
	primary := newFlatButton(pal, "＋ 新建任务", func() { app.openNewTaskDialog(PendingNewTask{}) })
	caret := newCaretButton("keyboard_arrow_down", 34, 15, nil)
	caret.SetTip("选择任务类型")
	div := canvas.NewRectangle(rgbaA(0xffffff, 0.25))
	div.SetMinSize(fyne.NewSize(1, 16))
	// 用 gradA/gradB(恒为亮色值):上一版误取 accent/accent2,亮色主题下
	// accent 偏暗,整块按钮仍是暗色(=用户报的"亮色下+新建任务仍是暗色")
	split := container.NewStack(newRoundedGrad(pal.gradA, pal.gradB, 10),
		hGap(2, primary, div, caret))
	// 箭头下拉(=newTaskMenu:链接任务 / 磁力种子)
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("链接任务(HTTP / HLS)", func() { app.openNewTaskDialog(PendingNewTask{}) }),
		fyne.NewMenuItem("磁力 / 种子任务(BT)", func() { app.openBTDialog("", "", "") }))
	caret.onTap = func() {
		m := widget.NewPopUpMenu(menu, app.win.Canvas())
		pos := app.a.Driver().AbsolutePositionForObject(caret)
		m.ShowAtPosition(fyne.NewPos(pos.X-170, pos.Y+caret.Size().Height+4))
	}

	right := hGap(16, app.chart, speeds, conn, themeBtn, settingsBtn, split)
	content := container.NewBorder(nil, nil, brand, right)
	padded := container.New(layout.NewCustomPaddedLayout(12, 12, 22, 22), content)
	panel := canvas.NewRectangle(pal.panel)
	bottom := canvas.NewRectangle(pal.border)
	bottom.SetMinSize(fyne.NewSize(0, 1))
	return container.NewStack(panel, container.NewBorder(padded, bottom, nil, nil))
}

// setConn 连接徽章(=conn-badge:绿底已连接 / 红底断开)
func (app *App) setConn(ok bool) {
	pal := app.themeObj.palv
	if ok {
		app.connBadge.SetText("已连接")
		app.connBadge.SetColor(pal.ok)
		app.connBg.FillColor = rgbaA(0x34d399, 0.10)
	} else {
		app.connBadge.SetText("连接断开,重连中…")
		app.connBadge.SetColor(pal.errc)
		app.connBg.FillColor = rgbaA(0xf87171, 0.12)
	}
	app.connBg.Refresh()
}

/* ---------------- 侧栏(对照 SideBar.vue:分类/计数/清扫) ---------------- */

type navItem struct {
	key, label string
	clearable  bool
	clearLabel string
}

var navItems = []navItem{
	{"all", "全部", false, ""},
	{"active", "下载中", false, ""},
	{"seeding", "做种中", false, ""},
	{"completed", "已完成", true, "已完成"},
	{"trash", "已取消", true, "已取消"},
	{"rss", "RSS订阅", false, ""},
}

// buildSideBar 侧栏(=sidebar:定宽 190、内边距 16/10、行距 4、右缘 1px 分隔;
// 清扫图标内嵌行尾且仅激活行显示;激活底用滑动指示条统一承载)
func (app *App) buildSideBar() fyne.CanvasObject {
	pal := app.themeObj.palv
	app.navRows = map[string]*navRow{}
	box := container.New(&vgapLayout{gap: 4})
	app.sideBox = box
	// 滑动指示条(=.nav-slide-ind:渐变底+左 3px 描边,切视图 0.25s 平移;
	// 放在行层下方,行自身 plain 化不再自绘激活底)
	g := canvas.NewLinearGradient(rgbaA(0x4f8cff, 0.16), rgbaA(0x22d3ee, 0.08), 0)
	bar := canvas.NewRectangle(pal.accent)
	bar.SetMinSize(fyne.NewSize(3, 0))
	app.slideObj = container.NewBorder(nil, nil, bar, nil, g)
	slideHolder := container.NewWithoutLayout(app.slideObj)
	app.slideObj.Move(fyne.NewPos(0, -100)) // 首拍 applySnapshot 会定位
	for _, it := range navItems {
		key, item := it.key, it
		row := newNavRow(pal, navIcon(key), item.label, func() { app.store.SwitchView(key) })
		row.plain = true
		app.navRows[key] = row
		if item.clearable {
			row.SetClear(func() { app.store.ClearView(key, item.clearLabel) })
			row.clear.Hide()
		}
		box.Add(row)
	}
	panel := canvas.NewRectangle(pal.softA(0.55))
	line := canvas.NewRectangle(pal.border)
	line.SetMinSize(fyne.NewSize(1, 0))
	inner := container.New(layout.NewCustomPaddedLayout(16, 16, 10, 10),
		container.NewVScroll(container.NewStack(slideHolder, box)))
	body := container.NewBorder(nil, nil, nil, line, inner)
	return newFixedMin(container.NewStack(panel, body), fyne.NewSize(190, 10))
}

// slideNavTo 滑动指示条移动到激活行(=nav-slide-ind transition transform .25s)
func (app *App) slideNavTo(view string) {
	if app.slideObj == nil || app.sideBox == nil {
		return
	}
	idx := -1
	for i, it := range navItems {
		if it.key == view {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	const rowH, rowGap = float32(41), float32(4)
	w := app.sideBox.Size().Width
	if w <= 1 {
		w = 170
	}
	target := float32(idx) * (rowH + rowGap)
	obj := app.slideObj
	obj.Resize(fyne.NewSize(w, rowH))
	if !app.slideInit {
		app.slideInit = true
		app.slideY = target
		obj.Move(fyne.NewPos(0, target))
		obj.Refresh()
		return
	}
	if app.slideAnim != nil {
		fyne.CurrentApp().Driver().StopAnimation(app.slideAnim)
	}
	from := app.slideY
	if from == target {
		return
	}
	app.slideAnim = fyne.NewAnimation(250*time.Millisecond, func(f float32) {
		app.slideY = from + (target-from)*f
		obj.Move(fyne.NewPos(0, app.slideY))
		obj.Refresh()
	})
	app.slideAnim.Curve = fyne.AnimationEaseInOut
	fyne.CurrentApp().Driver().StartAnimation(app.slideAnim)
}

// navIcon 侧栏图标(对齐 SideBar.vue 的内联 SVG 语义)
func navIcon(view string) string {
	switch view {
	case "all":
		return "notes"
	case "active":
		return "download"
	case "seeding":
		return "upload"
	case "completed":
		return "check_circle"
	case "trash":
		return "delete"
	case "rss":
		return "sensors"
	}
	return "notes"
}

func (app *App) setNavCount(view string, n string) {
	if r, ok := app.navRows[view]; ok {
		r.SetCount(n)
	}
}

/* ---------------- 快照应用(重绘节拍) ---------------- */

func (app *App) applySnapshot() {
	tasks := app.store.Tasks()
	st := app.store.Stats()
	app.speedDown.SetText(fmtSpeed(st.TotalSpeed))
	app.speedUp.SetText(fmtSpeed(st.TotalUpSpeed))
	app.chart.refresh()
	if v := app.store.Version(); v != "" {
		app.verBadge.SetText("v" + v)
	}

	// 侧栏计数(=SideBar.vue 各 computed)
	count := func(pred func(engineStatus) bool) int {
		n := 0
		for i := range tasks {
			if pred(tasks[i].Status) {
				n++
			}
		}
		return n
	}
	isActive := func(s engineStatus) bool {
		return s == engineStatusRunning || s == engineStatusWaiting || s == engineStatusPaused ||
			s == engineStatusError || s == engineStatusChoosing
	}
	counts := map[string]int{
		"all":       len(tasks),
		"active":    count(isActive),
		"seeding":   count(func(s engineStatus) bool { return s == engineStatusSeeding }),
		"completed": count(func(s engineStatus) bool { return s == engineStatusCompleted }),
		"trash":     count(func(s engineStatus) bool { return s == engineStatusCanceled }),
	}
	for k := range app.navRows {
		if k == "rss" {
			if st.RSSUnread > 0 {
				app.setNavCount("rss", fmt.Sprint(st.RSSUnread))
			} else {
				app.setNavCount("rss", "")
			}
			continue
		}
		app.setNavCount(k, fmt.Sprint(counts[k]))
	}

	// 任务列表:过滤 + 创建时间倒序(=TaskList.vue computed visible)
	view := app.store.CurrentView()
	// 侧栏激活态(=nav-item.active)+滑动指示条平移
	for k, r := range app.navRows {
		r.SetActive(k == view)
	}
	app.slideNavTo(view)
	visible := make([]*taskView, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if app.store.ViewFilter(t.Status) {
			visible = append(visible, t)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].CreatedAt.After(visible[j].CreatedAt) })

	// 卡片池化更新
	alive := map[string]bool{}
	newChildren := make([]fyne.CanvasObject, 0, len(visible))
	for _, t := range visible {
		alive[t.ID] = true
		card := app.cards[t.ID]
		if card == nil {
			card = newTaskCard(app.store, t.ID)
			app.cards[t.ID] = card
		}
		card.update(t)
		newChildren = append(newChildren, card)
	}
	for id := range app.cards {
		if !alive[id] {
			delete(app.cards, id)
		}
	}
	// 仅当集合或顺序变化时重建容器(避免每拍全量刷新)
	if len(newChildren) != len(app.listBox.Objects) {
		app.listBox.Objects = newChildren
		app.listBox.Refresh()
	} else {
		changed := false
		for i := range newChildren {
			if app.listBox.Objects[i] != fyne.CanvasObject(newChildren[i]) {
				changed = true
				break
			}
		}
		if changed {
			app.listBox.Objects = newChildren
			app.listBox.Refresh()
		}
	}

	// RSS 视图切换(=RssPane watch currentView:进入加载+轮询,离开即停)
	if rp := app.rss; rp != nil {
		if view == "rss" {
			app.listBox.Hide()
			app.emptyState.Hide()
			rp.root.Show()
			rp.onShow()
		} else {
			rp.root.Hide()
			if rp.visible {
				rp.onHide()
			}
			if len(visible) == 0 {
				app.emptyHint.SetText(emptyTextFor(view))
				app.emptyState.Show()
			} else {
				app.emptyState.Hide()
			}
			app.listBox.Show()
		}
	} else if len(visible) == 0 && view != "rss" {
		app.emptyHint.SetText(emptyTextFor(view))
		app.emptyState.Show()
	} else {
		app.emptyState.Hide()
	}

	// choosing 自动弹窗检查(与 Vue acceptTick 同拍)
	app.store.MaybePromptChoose()
}

func emptyTextFor(view string) string {
	switch view {
	case "all":
		return "还没有任何任务,点击右上角「新建任务」开始"
	case "completed":
		return "还没有已完成的任务"
	case "trash":
		return "已取消的任务会显示在这里,可随时重新下载"
	case "seeding":
		return "没有正在做种的任务;完成任务后按 BT 设置中的策略自动做种"
	default:
		return "暂无下载任务,点击右上角「新建任务」开始"
	}
}

/* ---------------- 任务卡(对照 TaskCard.vue,视觉对齐 style.css) ---------------- */

// taskCard 单卡:头部(38px 渐变图标块 / 标题+URL 纵排 / 状态胶囊 / 图标动作)
// + 数值行 + 7px 渐变进度条 + 分段热度条。彩色文本走 colorLabel(fyne v2.8
// 的 Label 无 Color 字段),图标走 Material Symbols(iconButton)。
type taskCard struct {
	widget.BaseWidget
	store *Store
	id    string

	cardBg         *canvas.Rectangle
	cardBorder     *canvas.Rectangle
	iconBg         *roundedGrad
	iconLbl        *colorLabel
	nameLbl        *colorLabel // 标题:colorLabel 才支持悬停下划线与按态着色
	nameHover      bool        // 标题悬停(=.name.clickable:hover)
	nameClickable  bool        // 标题可点(=name.clickable:流媒体/文件列表/打开)
	nameStreamable bool        // 流媒体标题常驻 accent(=.name.streamable)
	urlLbl         *colorLabel
	chipBg         *canvas.Rectangle
	chipLbl        *colorLabel
	actions        *fyne.Container
	pctLbl         *colorLabel
	sizeLbl        *colorLabel
	speedLbl       *colorLabel
	etaLbl         *colorLabel
	errLbl         *colorLabel
	rightLbl       *colorLabel
	track          *canvas.Rectangle
	fill           *roundedGrad
	fillPct        float64         // -1 = indeterminate(running 无百分比)
	fillMode       int             // 0 普通 1 done 2 err 3 indeterminate
	lastFillMode   int             // 上次下发的填充模式(防每帧重传纹理)
	indAnim        *fyne.Animation // indeterminate 循环动画(离开该态即停)
	phase          float32         // indeterminate 相位(0→1,=margin-left -30%→100%)
	lastW          float32         // Layout 记录的轨道宽(动画 Tick 定位用)
	segStrip       *fyne.Container
	actBtns        map[string]*iconButton // 动作按钮缓存(每秒快照重建会抹掉 hover)
	lastT          *taskView
}

func newTaskCard(s *Store, id string) *taskCard {
	c := &taskCard{store: s, id: id}
	c.ExtendBaseWidget(c)
	p := s.appPal()
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	// 卡片容器(=task-card:panel 底 + 1px 边框 + 14px 圆角)
	c.cardBg = canvas.NewRectangle(p.panel)
	c.cardBg.CornerRadius = 14
	c.cardBorder = canvas.NewRectangle(color.NRGBA{A: 0})
	c.cardBorder.CornerRadius = 14
	c.cardBorder.StrokeWidth = 1
	c.cardBorder.StrokeColor = p.border
	// 图标块(=file-icon:38px、radius 10、渐变)
	c.iconBg = newRoundedGrad(white, white, 10)
	c.iconLbl = newColorLabelS("FILE", white, true, 10.5)
	// 标题/URL 纵排(标题 14.5px 粗 + 超宽省略号截断,=.task-title .name)
	c.nameLbl = newColorLabelS("", p.text, true, 14.5).SetMaxWidth()
	c.urlLbl = newColorLabelS("", p.textFaint, false, 11.5).SetMaxWidth()
	// 状态胶囊(=status-chip:11px 粗、padding 3px 10px、radius 20)
	c.chipBg = canvas.NewRectangle(p.accent2)
	c.chipBg.CornerRadius = 11
	c.chipLbl = newColorLabelS("", p.accent2, true, 11)
	// 数值行(=task-mid:12px;pct 13px 粗 min-width 52)
	c.pctLbl = newColorLabelS("", p.text, true, 13)
	c.sizeLbl = newColorLabelS("", p.textDim, false, 12)
	c.speedLbl = newColorLabelS("", p.accent2, true, 12)
	c.etaLbl = newColorLabelS("", p.textDim, false, 12)
	c.errLbl = newColorLabelS("", p.errc, false, 12)
	c.rightLbl = newColorLabelS("", p.textFaint, false, 12)
	c.actions = container.NewHBox()
	// 进度条(=progress-track:7px、radius 6、灰底;fill=accent-grad 圆角)
	c.track = canvas.NewRectangle(rgbaA(0x7f7f7f, 0.13))
	c.track.CornerRadius = 3
	c.fill = newRoundedGrad(p.accent, p.accent2, 3)
	c.segStrip = container.New(&colsGapLayout{gap: 3})
	return c
}

// CreateRenderer 布局:卡片容器(底+边框+padding 14/16/18)/头部行/数值行/
// 进度条/热度条,纵向 9px 间距(=task-mid margin-top 11 / track 8 的折中)
func (c *taskCard) CreateRenderer() fyne.WidgetRenderer {
	nameTap := newTitleTap(c.nameLbl, c.nameClick, func(h bool) {
		c.nameHover = h
		c.applyTitleState()
	})
	// 标题提示(=nameTitle:边下边播/文件列表/落盘路径/文件名)
	nameTap.SetTip(c.nameTitle)
	// 手形指针(=Vue .name.clickable):仅标题可点(流媒体/文件列表/打开)时显手形;
	// 判据读 c.nameClickable,随任务态在 refresh 中更新,故用闭包实时取
	nameTap.SetClickable(func() bool { return c.nameClickable })
	// 标题可点(=TaskCard.vue nameClick:流媒体>文件列表>打开)+ 悬停态
	// (=.name.clickable:hover:accent+下划线);URL 截断悬停显完整地址(=:title)
	// 标题/URL 栈与左侧文件图标之间留 12px 间距(=用户报的"离图标太近"),
	// 以左内边距形式加在中央扩展位,不影响右侧胶囊/动作区。
	// 注意 CustomPaddedLayout 参数序是 (上,下,左,右):上一版误写成上内边距,
	// 把整行撑高、图标被拉成竖长条且水平间距纹丝不动——就是本轮"图标变长条"的根因
	titleStack := container.New(layout.NewCustomPaddedLayout(0, 0, 12, 0),
		container.NewVBox(nameTap, newTipLabel(c.urlLbl, c.urlLbl.Text)))
	// 图标固定 38×38 正方形:Border 左槽会被拉伸到整行高,直接放 fixedMin 会把
	// 渐变底拉成竖条;再套一层 Center 让正方形在拉伸槽内垂直居中,行高变化也不变形
	iconBlock := container.NewCenter(newFixedMin(container.NewStack(c.iconBg, container.NewCenter(c.iconLbl)), fyne.NewSize(38, 38)))
	chip := container.NewStack(c.chipBg, container.New(layout.NewCustomPaddedLayout(3, 3, 10, 10), c.chipLbl))
	// Border 结构:标题栈占中央扩展位(TextTruncate 最小宽度趋近 0,放
	// HBox 会被压扁、胶囊挤到图标旁——实测事故),胶囊+动作靠右
	head := container.NewBorder(nil, nil, iconBlock,
		hGap(13, chip, c.actions), titleStack)
	// 数值行(=task-mid gap 14,right=margin-left auto 推到右缘)
	mid := container.NewBorder(nil, nil,
		hGap(14,
			newFixedMin(container.NewStack(c.pctLbl), fyne.NewSize(52, 16)),
			c.sizeLbl, c.speedLbl, c.etaLbl, c.errLbl),
		c.rightLbl)
	progress := newFixedMin(container.NewStack(c.track, c.fill), fyne.NewSize(10, 7))
	body := container.New(&vgapLayout{gap: 9}, head, mid, progress, c.segStrip)
	padded := container.New(layout.NewCustomPaddedLayout(14, 16, 18, 18), body)
	root := container.NewStack(c.cardBg, c.cardBorder, padded)
	return &cardRenderer{card: c, root: root, progress: progress, pal: c.store.appPal()}
}

type cardRenderer struct {
	card     *taskCard
	root     *fyne.Container
	progress *fixedMin
	pal      palette
}

func (r *cardRenderer) Layout(size fyne.Size) {
	r.root.Resize(size)
	inner := r.progress.Content()
	w, h := inner.Size().Width, inner.Size().Height
	pct := r.card.fillPct
	// indeterminate:30% 段滑动。CSS 原值是 margin-left -30%→100%,靠轨道
	// overflow:hidden 裁掉溢出段;Fyne 的 Stack 不裁剪子对象,若照搬会让 30%
	// 段滑出轨道右缘可见(=用户报的溢出),故改为在 [0, 0.7w] 内往返——
	// 填充(宽 0.3w)始终落在轨道 [0, w] 内。Tick 只做 Move,这里负责每拍宽度兜底
	if pct < 0 {
		pct = 0.3
		r.card.lastW = w
	}
	if pct > 1 {
		pct = 1
	}
	// 颜色只在模式变化时下发(SetColors 有同值守卫,但这里连比较都省了)
	if r.card.fillMode != r.card.lastFillMode {
		p := r.pal
		switch r.card.fillMode {
		case 1:
			r.card.fill.SetColors(p.ok, p.ok)
		case 2:
			r.card.fill.SetColors(p.errc, p.errc)
		default:
			r.card.fill.SetColors(p.accent, p.accent2)
		}
		r.card.lastFillMode = r.card.fillMode
	}
	r.card.fill.Resize(fyne.NewSize(w*float32(pct), h))
	if r.card.fillPct < 0 {
		// 相位 0→1 映射到 X 0→0.7w:填充宽 0.3w,右缘 0.7w+0.3w=w,不溢出轨道
		r.card.fill.Move(fyne.NewPos(r.card.phase*(w*0.7), 0))
	} else {
		r.card.fill.Move(fyne.NewPos(0, 0))
	}
	r.card.track.Resize(fyne.NewSize(w, h))
	r.card.track.Move(fyne.NewPos(0, 0))
}

func (r *cardRenderer) MinSize() fyne.Size { return r.root.MinSize() }
func (r *cardRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.root}
}
func (r *cardRenderer) Destroy() {}
func (r *cardRenderer) Refresh() {}

// startIndeterminate 无进度循环滑动(=progress-fill.indeterminate:
// 1.4s ease-in-out infinite;30% 段在轨道内 [0,100%-宽] 往返,见 Layout 注释)
func (c *taskCard) startIndeterminate() {
	if c.indAnim != nil {
		return
	}
	anim := fyne.NewAnimation(1400*time.Millisecond, func(f float32) {
		c.phase = f
		if c.lastW > 0 {
			// 与 Layout 同口径:相位 0→1 落在 [0, 0.7w],填充不溢出轨道
			c.fill.Move(fyne.NewPos(f*(c.lastW*0.7), 0))
			c.fill.Refresh()
		}
	})
	anim.Curve = fyne.AnimationEaseInOut
	anim.RepeatCount = fyne.AnimationRepeatForever
	c.indAnim = anim
	fyne.CurrentApp().Driver().StartAnimation(anim)
}

// stopIndeterminate 离开 indeterminate 态停动画并复位填充
func (c *taskCard) stopIndeterminate() {
	if c.indAnim == nil {
		return
	}
	fyne.CurrentApp().Driver().StopAnimation(c.indAnim)
	c.indAnim = nil
	c.phase = 0
	c.fill.Move(fyne.NewPos(0, 0))
	c.fill.Refresh()
}

// nameTitle 标题悬停提示(=TaskCard.vue nameTitle:边下边播/文件列表/落盘路径/
// 文件名)
func (c *taskCard) nameTitle() string {
	t := c.lastT
	if t == nil {
		return ""
	}
	canStream := t.Streamable && (t.Status == engineStatusRunning || t.Status == engineStatusSeeding)
	canFiles := t.Kind == "bt" && t.FileCount > 1
	canOpen := t.Status == engineStatusCompleted || t.Status == engineStatusSeeding
	switch {
	case canStream:
		return "点击边下边播"
	case canFiles:
		return "点击查看文件列表"
	case canOpen:
		return t.Dir + "\\" + t.Filename
	default:
		return t.Filename
	}
}

// nameClick 标题点击路由(=TaskCard.vue nameClick:流媒体 > 文件列表 > 打开)
func (c *taskCard) nameClick() {
	t, ok := c.store.Task(c.id)
	if !ok {
		return
	}
	canStream := t.Streamable && (t.Status == engineStatusRunning || t.Status == engineStatusSeeding)
	canFiles := t.Kind == "bt" && t.FileCount > 1
	canOpen := t.Status == engineStatusCompleted || t.Status == engineStatusSeeding
	switch {
	case canStream:
		if c.store.OpenPlayer != nil {
			c.store.OpenPlayer(t.ID, "")
		}
	case canFiles:
		if c.store.OpenDetail != nil {
			c.store.OpenDetail(t.ID)
		}
	case canOpen:
		c.store.OpenTaskFile(t.ID)
	}
}

// applyTitleState 标题配色与字面(=.name 各态):取消=删除线+置灰(取消任务本
// 不可点,无 hover,故取消态优先)> 可点悬停=accent+下划线 > 流媒体=常驻 accent
// > 常规=text。SetColor/SetStyle 同值早退,悬停进出只在跨态时重绘。
func (c *taskCard) applyTitleState() {
	pal := c.store.appPal()
	t := c.lastT
	canceled := t != nil && t.Status == engineStatusCanceled
	col := pal.text
	// 删除线/下划线用画线实现(SetDecor)而非 TextStyle 变体:字体 face 缓存
	// 按 TextStyle 全值键控,每变体 ~19MB(整链重解析主题字体+内置回退),
	// 这两个一次性装饰不值得各占一条(内存分析结论);基础样式恒为 {Bold}。
	st := fyne.TextStyle{Bold: true}
	hover := c.nameHover && c.nameClickable
	switch {
	case canceled:
		col = pal.textFaint
	case hover:
		col = pal.accent
	case c.nameStreamable:
		col = pal.accent
	}
	c.nameLbl.SetColor(col)
	c.nameLbl.SetDecor(st, canceled, hover && !canceled)
}

// update 一帧刷新(数值口径对齐 TaskCard.vue computed;视觉对齐 style.css)
func (c *taskCard) update(t *taskView) {
	c.lastT = t
	pal := c.store.appPal()
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	display := t.Filename
	if display == "" {
		display = MagnetFallbackName(*t)
	}
	// 卡底/描边跟随主题(构造期快照在换肤后会陈旧,每帧同值早退)
	if !sameColor(c.cardBg.FillColor, pal.panel) {
		c.cardBg.FillColor = pal.panel
		c.cardBg.Refresh()
	}
	if !sameColor(c.cardBorder.StrokeColor, pal.border) {
		c.cardBorder.StrokeColor = pal.border
		c.cardBorder.Refresh()
	}
	// 图标块(颜色随类型变化;SetColors 同值早退)
	cls := iconClass(*t)
	g1, g2 := pal.iconGrad(cls)
	c.iconBg.SetColors(g1, g2)
	lbl := iconLabel(*t)
	if len(lbl) > 5 {
		lbl = lbl[:5]
	}
	c.iconLbl.SetText(lbl)
	c.iconLbl.SetColor(white)
	// 标题态(=.name):可点=流媒体/文件列表/打开,悬停 accent+下划线,流媒体常驻 accent
	canStream := t.Streamable && (t.Status == engineStatusRunning || t.Status == engineStatusSeeding)
	canFiles := t.Kind == "bt" && t.FileCount > 1
	canOpen := t.Status == engineStatusCompleted || t.Status == engineStatusSeeding
	c.nameClickable = canStream || canFiles || canOpen
	c.nameStreamable = canStream
	c.nameLbl.SetText(display)
	c.applyTitleState()
	c.urlLbl.SetText(t.URL)
	// 状态胶囊(fg 语义色 + 半透明底;底色同值不重绘)
	fg, bg := pal.statusChip(t.Status)
	if !sameColor(c.chipBg.FillColor, bg) {
		c.chipBg.FillColor = bg
		c.chipBg.Refresh()
	}
	c.chipLbl.SetText(StatusText(t.Status))
	c.chipLbl.SetColor(fg)

	// 数值口径(=progBytes/bytePct/segPct)
	isHLS := t.Kind == "hls"
	var pct float64
	progBytes := t.Downloaded
	if t.Arrived > 0 {
		progBytes = t.Arrived
	} else if t.Kind == "bt" && t.Size > 0 && t.Downloaded > t.Size {
		progBytes = t.Size
	}
	hasPct := false
	if isHLS && t.SegTotal > 0 {
		pct = float64(t.SegDone) / float64(t.SegTotal) * 100
		hasPct = true
		txt := fmt.Sprintf("%d/%d 分片", t.SegDone, t.SegTotal)
		if t.Downloaded > 0 {
			txt += " · " + fmtBytes(t.Downloaded)
		}
		if t.Encrypted {
			txt += " · 已加密"
		}
		c.sizeLbl.SetText(txt)
	} else {
		if t.Size > 0 {
			pct = float64(progBytes) / float64(t.Size) * 100
			hasPct = true
			c.sizeLbl.SetText(fmtBytes(progBytes) + " / " + fmtBytes(t.Size))
		} else {
			c.sizeLbl.SetText(fmtBytes(progBytes))
		}
	}
	if hasPct {
		c.pctLbl.SetText(fmt.Sprintf("%.1f%%", pct))
	} else {
		c.pctLbl.SetText("—")
	}

	// 速度/ETA/分享率/完成时间
	if t.Status == engineStatusSeeding {
		c.speedLbl.SetText("↑ " + fmtSpeed(t.Speed))
		ratio := "0.00"
		if t.Downloaded > 0 {
			ratio = fmt.Sprintf("%.2f", float64(t.Uploaded)/float64(t.Downloaded))
		}
		c.etaLbl.SetText("分享率 " + ratio + " · 已上传 " + fmtBytes(t.Uploaded))
	} else if t.Status == engineStatusRunning {
		c.speedLbl.SetText(fmtSpeed(t.Speed))
		if isHLS || t.Size <= 0 {
			c.etaLbl.SetText("")
		} else {
			c.etaLbl.SetText(fmtETA(t.Speed, t.Size-progBytes))
		}
	} else {
		c.speedLbl.SetText("")
		c.etaLbl.SetText("")
	}
	if (t.Status == engineStatusCompleted || t.Status == engineStatusSeeding) && !t.CompletedAt.IsZero() {
		c.rightLbl.SetText("完成于 " + fmtTime(t.CompletedAt))
	} else if !t.CreatedAt.IsZero() {
		// 未完成任务同位显示创建时间(=Vue rightText 口径的增量:完成/做种显"完成于",
		// 其余显"创建于"),让每行进度条右上方始终有可读的时间信息
		c.rightLbl.SetText("创建于 " + fmtTime(t.CreatedAt))
	} else {
		c.rightLbl.SetText("")
	}
	if t.Status == engineStatusError {
		c.errLbl.SetText(t.Err)
		c.errLbl.Show()
	} else {
		c.errLbl.SetText("")
		c.errLbl.Hide()
	}

	// 已取消任务:数值行全列置灰(=is-canceled .task-mid)
	if t.Status == engineStatusCanceled {
		c.pctLbl.SetColor(pal.textFaint)
		c.sizeLbl.SetColor(pal.textFaint)
		c.speedLbl.SetColor(pal.textFaint)
		c.etaLbl.SetColor(pal.textFaint)
		c.rightLbl.SetColor(pal.textFaint)
	} else {
		c.pctLbl.SetColor(pal.text)
		c.sizeLbl.SetColor(pal.textDim)
		c.speedLbl.SetColor(pal.accent2)
		c.etaLbl.SetColor(pal.textDim)
		c.rightLbl.SetColor(pal.textFaint)
	}

	// 进度条三态(0 普通 1 done 2 err 3 indeterminate)
	switch {
	case t.Status == engineStatusCompleted || t.Status == engineStatusSeeding:
		c.fillMode, c.fillPct = 1, 1.0
	case hasPct && t.Status != engineStatusError:
		c.fillMode, c.fillPct = 0, pct/100
	case hasPct:
		c.fillMode, c.fillPct = 2, pct/100
	case t.Status == engineStatusRunning:
		c.fillMode, c.fillPct = 3, -1
	default:
		c.fillMode, c.fillPct = 0, 0
	}
	// indeterminate 动画随态启停(=CSS animation 只在 .indeterminate 生效)
	if c.fillMode == 3 {
		c.startIndeterminate()
	} else {
		c.stopIndeterminate()
	}

	// 分段热度条
	if t.Status == engineStatusRunning && len(t.Segments) > 1 {
		c.renderSegs(t.Segments)
		c.segStrip.Show()
	} else {
		c.segStrip.Hide()
	}

	c.renderActions(t)
	c.Refresh()
}

// renderActions 图标动作按钮(=actionsFor:detail 固定首位;MS 图标对齐
// icons.js 的 SVGS/ms 用法)
func (c *taskCard) renderActions(t *taskView) {
	pal := c.store.appPal()
	btn := func(icon, act string) fyne.CanvasObject {
		if c.actBtns == nil {
			c.actBtns = map[string]*iconButton{}
		}
		if b, ok := c.actBtns[act]; ok {
			b.SetIcon(icon) // resume/pause 等同位换图标
			return b
		}
		b := newIconButton(pal, icon, func() { c.store.HandleAction(act, c.id) })
		c.actBtns[act] = b
		return b
	}
	var items []fyne.CanvasObject
	items = append(items, btn("info", "detail"))
	switch t.Status {
	case engineStatusRunning, engineStatusPaused, engineStatusWaiting:
		items = append(items, btn("folder_open", "open"))
		if t.Status == engineStatusPaused {
			items = append(items, btn("play_arrow", "resume"))
		} else {
			items = append(items, btn("pause", "pause"))
		}
		items = append(items, btn("close", "cancel"))
	case engineStatusChoosing:
		items = append(items, btn("check_circle", "choosefiles"))
		items = append(items, btn("play_arrow", "resume"))
		items = append(items, btn("close", "cancel"))
	case engineStatusError:
		items = append(items, btn("folder_open", "open"), btn("refresh", "retry"), btn("close", "cancel"))
	case engineStatusSeeding:
		items = append(items, btn("folder_open", "open"), btn("stop", "stopseed"), btn("delete", "trash"))
	case engineStatusCompleted:
		items = append(items, btn("folder_open", "open"))
		if t.Kind == "bt" {
			items = append(items, btn("upload", "reseed"))
		}
		items = append(items, btn("delete", "trash"))
	case engineStatusCanceled:
		items = append(items, btn("refresh", "resume"), btn("delete", "trash"))
	}
	c.actions.Objects = items
	c.actions.Refresh()
}

// renderSegs 分段热度条(压 48 根;灰=p0,accent=部分,cyan=满)
func (c *taskCard) renderSegs(segs []engineSegment) {
	const maxBars = 48
	step := (len(segs) + maxBars - 1) / maxBars
	pal := c.store.appPal()
	children := make([]fyne.CanvasObject, 0, maxBars)
	for i := 0; i < len(segs); i += step {
		done, total := int64(0), int64(0)
		for j := i; j < i+step && j < len(segs); j++ {
			done += segs[j].Done
			total += segs[j].End - segs[j].Start + 1
		}
		var col color.Color
		switch {
		case total <= 0 || done >= total:
			col = pal.accent2
		case done > 0:
			col = pal.accent
		default:
			col = rgbaA(0x7f7f7f, 0.16)
		}
		bar := canvas.NewRectangle(col)
		bar.SetMinSize(fyne.NewSize(6, 5))
		children = append(children, bar)
	}
	c.segStrip.Objects = children
	c.segStrip.Refresh()
}

/* ---------------- Toast / 确认框 ---------------- */

// toast 底部浮动提示(=Toasts.vue;淡出动画打磨阶段接,先做静态 4.2s 消失)
// toast 全局提示(=Toasts.vue:右下 20px 堆叠 gap 8,panel-solid 圆角 10
// + border-strong + 左侧 3px 语义色条,13px 文本 max 380 宽;4400ms 移除)。
// 放内容树顶层浮层(绝不能做 overlay:fyne 命中分发只遍历 Overlays().Top(),
// 任何 overlay 都会让整窗内容不可达=实测点不动)
func (app *App) toast(msg, cls string) {
	if floatLayer == nil {
		return
	}
	pal := app.store.appPal()
	// 单行截断不用 widget.Label(带换行的 Label min 高按整段文本测,
	// 卡片高度会虚胖/文字溢出不可控);长提示截断在 380 宽内
	lbl := newColorLabelS(msg, pal.text, false, 13).SetMaxWidth()
	bar := canvas.NewRectangle(pal.accent) // .toast 默认左条 accent
	switch cls {
	case "err":
		bar.FillColor = pal.errc
	case "ok":
		bar.FillColor = pal.ok
	case "warn":
		bar.FillColor = pal.warn
	}
	// 左条做成垂直圆角胶囊(宽 4、高 20、半径 2=半宽→两端半圆):卡片 10px 圆角
	// 下通高方条会在顶/底左角溢出(方条直角伸到圆角外)。胶囊上下居中留白,落在
	// 圆角外侧的直边区,fyne 无曲线边框,用内缩胶囊近似 CSS border-left+radius
	bar.CornerRadius = 2
	bar.SetMinSize(fyne.NewSize(4, 20))
	// 上下 spacer 把胶囊垂直居中(留白),不贴卡片顶/底边
	leftCol := container.NewVBox(layout.NewSpacer(), bar, layout.NewSpacer())
	bg := canvas.NewRectangle(pal.panelSolid)
	bg.CornerRadius = 10
	bg.StrokeWidth = 1
	bg.StrokeColor = pal.borderStrong
	// maxW 标签的 min 宽是 10px,卡宽必须按文本实测(=MeasureText);
	// +36 = 32(padding)+ 3(左条)+ 1 冗余;MeasureText 对 CJK 逐字少算
	// 1px(实测 14 字截 1 字),按字符数再补
	tw := fyne.MeasureText(msg, 13, fyne.TextStyle{}).Width
	w := tw + 36 + float32(utf8.RuneCountInString(msg))
	wrapW := float32(380)
	if w > wrapW {
		w = wrapW
	}
	panel := newFixedMin(container.NewStack(bg,
		container.NewBorder(nil, nil, leftCol, nil,
			container.New(layout.NewCustomPaddedLayout(11, 11, 16, 16), lbl))),
		fyne.NewSize(w, 0))
	// 面板必须真挂上顶层浮层:I 轮重构后只记了 toastCards 没挂 floatLayer,
	// toast 从此只存在于数组里(点击报错/RSS 立即更新全都无提示)
	floatLayer.Add(panel)
	app.toastCards = append(app.toastCards, panel)
	// 新增即整体重排:只摆新卡会让它落在最底、与已有 toast 重叠(=用户报的
	// "新 toast 和已有 toast 位置重叠");整体重排让旧卡随新卡依次上移
	app.relayoutToasts()
	time.AfterFunc(4400*time.Millisecond, func() {
		fyne.Do(func() {
			for i, o := range app.toastCards {
				if o == panel {
					app.toastCards = append(app.toastCards[:i], app.toastCards[i+1:]...)
					break
				}
			}
			floatLayer.Remove(panel)
			app.relayoutToasts()
		})
	})
}

// layoutToast 单卡定宽摆放(右下第 idx 层)
func (app *App) layoutToast(panel fyne.CanvasObject, idx int) {
	if floatLayer == nil {
		return
	}
	cnv := app.win.Canvas()
	m := panel.MinSize()
	bottom := float32(0)
	for i := len(app.toastCards) - 1; i > idx; i-- {
		bottom += app.toastCards[i].MinSize().Height + 8
	}
	panel.Resize(fyne.NewSize(m.Width, m.Height))
	panel.Move(fyne.NewPos(cnv.Size().Width-20-m.Width, cnv.Size().Height-20-bottom-m.Height))
	floatLayer.Refresh()
}

// relayoutToasts 右下角自下而上堆叠(=.toasts fixed right/bottom 20)
func (app *App) relayoutToasts() {
	for i := len(app.toastCards) - 1; i >= 0; i-- {
		app.layoutToast(app.toastCards[i], i)
	}
}

// confirm 确认框(=ConfirmBox.vue:主按钮 + 可选二级按钮)
// confirm 自绘确认框(=Vue confirmBox:消息 + secondary/cancel/ok 三键)。
// 曾用 dialog.NewConfirm——其遮罩读 ColorNameShadow,该色名已全局透明
// (根除 ThemeOverride scope 字体缓存泄漏),遮罩会消失,故换 maskModal
func (app *App) confirm(req ConfirmReq) {
	pal := app.store.appPal()
	title := req.Title
	if title == "" {
		title = "确认操作"
	}
	okLabel := orDefault(req.OKLabel, "确认")
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS(title, pal.text, true, 17))
	msg := newColorLabelS(req.Message, pal.textDim, false, 13).SetMaxWidth()
	var pop *maskModal
	close := func() { pop.Hide() }
	// 确认框专用于敏感操作(删除任务/文件、取消、清空、封禁等),按键配色对齐
	// ConfirmBox.vue:确认键=红渐变(#f87171→#ef4444),二级危险键=红 ghost,
	// 取消键保持中性 ghost。红色把"点下去不可逆"与普通对话框的蓝色主键区分开
	buttons := []fyne.CanvasObject{}
	if req.SecondaryLabel != "" {
		buttons = append(buttons, newDangerGhostButton(pal, req.SecondaryLabel, func() {
			close()
			if req.OnSecondary != nil {
				req.OnSecondary()
			}
		}))
	}
	buttons = append(buttons, newGhostButton(pal, "取消", close))
	buttons = append(buttons, newDangerButton(pal, okLabel, func() {
		close()
		if req.OnOk != nil {
			req.OnOk()
		}
	}))
	actions := hGap(10, buttons...)
	body := container.New(&vgapLayout{gap: 14},
		head,
		container.New(layout.NewCustomPaddedLayout(2, 2, 0, 0), msg),
		container.NewBorder(nil, nil, nil, actions))
	pop = modalCard(app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 20, 24, 24), body), 420, 0)
	pop.Show()
}

/* ---------------- 新建任务对话框(NewTaskDialog.vue) ---------------- */

// parseHeadersText 解析「Key: Value」每行一条的请求头文本(=parseHeaders)
func parseHeadersText(s string) map[string]string {
	h := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			if k != "" {
				h[k] = v
			}
		}
	}
	return h
}

// openNewTaskDialog 链接任务创建:批量链接/文件名/线程数/保存目录 +
// 高级选项折叠(Referer/请求头/限速/校验和/代理)+ 链接预解析行,逻辑对齐
// NewTaskDialog.vue(探针带高级参数,保证解析条件与实际下载一致)
func (app *App) openNewTaskDialog(p PendingNewTask) {
	pal := app.store.appPal()
	field := func(label string, w fyne.CanvasObject) fyne.CanvasObject {
		return container.NewVBox(newColorLabelS(label, pal.textDim, true, 12.5),
			container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0), w))
	}
	urls := widget.NewMultiLineEntry()
	urls.Wrapping = fyne.TextWrapWord // 多行回绕,不出横向滚动条(=textarea)
	urls.SetPlaceHolder("https://example.com/file.zip(每行一个,支持批量)")
	if len(p.URLs) > 0 {
		urls.SetText(strings.Join(p.URLs, "\n"))
	}
	filename := widget.NewEntry()
	filename.SetPlaceHolder("自动识别")
	singleLine(filename) // 横向裁剪、无纵向滚动条(=input)
	// 线程数默认取全局配置(=store.cfg.maxConnections)
	connDef := 8
	if cfg, err := app.client.Config(); err == nil && cfg.MaxConnections > 0 {
		connDef = cfg.MaxConnections
	}
	connections := app.selectW(numOpts(64), strconv.Itoa(connDef))
	// 目录用 dirField(=Vue DirField):可自由输入/粘贴 + ▾ 选最近目录,右侧再留"浏览…"
	dir := NewDirField(app, "默认下载目录", "")
	browse := newGhostButton(pal, "浏览…", nil)
	browse.SetMini()
	browse.onTap = func() {
		app.pickFolderAsync("选择下载文件夹", func(p string, ok bool) {
			if !ok {
				return
			}
			fyne.Do(func() { dir.SetText(p) })
		})
	}

	// ---- 高级选项(默认收起;=adv-panel) ----
	referer := textEntry("")
	referer.SetPlaceHolder("留空则不发送")
	headersE := widget.NewMultiLineEntry()
	headersE.Wrapping = fyne.TextWrapWord
	headersE.SetPlaceHolder("Cookie: session=...(每行一条,格式 Key: Value)")
	speedLimit := numEntry(0)
	slUnit := app.selectW([]string{"KB/s", "MB/s"}, "KB/s")
	checksum := textEntry("")
	checksum.SetPlaceHolder("md5: / sha1: / sha256: / crc32:")
	// 代理:跟随全局/直连/自定义(=PROXY_OPTS;选自定义才显地址框)
	proxyLabel := map[string]string{
		"": "跟随全局设置", "none": "直连(不走代理)", "custom": "自定义…"}
	proxyVal := map[string]string{}
	for k, v := range proxyLabel {
		proxyVal[v] = k
	}
	proxySel := app.selectW([]string{proxyLabel[""], proxyLabel["none"], proxyLabel["custom"]}, proxyLabel[""])
	proxyCustom := textEntry("")
	proxyCustom.SetPlaceHolder("socks5://127.0.0.1:1080")
	proxyCustom.Hide()
	proxySel.OnChanged = func(s string) {
		if proxyVal[s] == "custom" {
			proxyCustom.Show()
		} else {
			proxyCustom.Hide()
		}
	}
	advPanel := container.New(&vgapLayout{gap: 14},
		field("Referer", referer),
		field("自定义请求头(每行一个,格式 Key: Value)", headersE),
		container.New(layout.NewGridLayoutWithColumns(2),
			field("限速(0 不限)", container.NewBorder(nil, nil, nil,
				// 单位下拉定宽 96(=.unit-row select width:96px;跟随内容
				// 会被压到 min 宽,"KB/s" 只剩省略号)
				newFixedMin(container.NewStack(slUnit), fyne.NewSize(96, 36)), speedLimit)),
			field("校验和(如 md5:xxxx)", checksum)),
		field("代理", proxySel),
		proxyCustom)
	advPanel.Hide()
	var pop *maskModal
	var fitHeight func()
	relayout := func() {
		if fitHeight != nil {
			fitHeight() // 内容显隐后重算卡高(自适应+620 上限)并重排
		}
	}
	advHead := wrapHover(pal, hGap(5,
		newColorLabelS("高级选项", pal.accent, true, 12.5),
		msText("keyboard_arrow_down", pal.accent, 13)))
	advHead.onTap = func() {
		if advPanel.Visible() {
			advPanel.Hide()
		} else {
			advPanel.Show()
		}
		relayout()
	}

	// ---- 链接预解析(防抖;=runProbe,带高级参数) ----
	probe := widget.NewLabel("")
	probe.Wrapping = fyne.TextWrapWord
	probe.Hide()
	var probeTimer *time.Timer
	probeSeq := 0
	proxyValue := func() string {
		v := proxyVal[proxySel.Selected]
		if v == "custom" {
			return strings.TrimSpace(proxyCustom.Text)
		}
		return v
	}
	runProbe := func() {
		lines := []string{}
		for _, line := range strings.Split(urls.Text, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				lines = append(lines, s)
			}
		}
		probeSeq++
		if len(lines) == 0 {
			probe.Hide()
			relayout()
			return
		}
		seq := probeSeq
		probe.SetText("解析中…")
		probe.Show()
		relayout()
		go func() {
			r, err := app.client.Probe(lines[0], strings.TrimSpace(referer.Text),
				parseHeadersText(headersE.Text), proxyValue())
			fyne.Do(func() {
				if seq != probeSeq {
					return
				}
				switch {
				case err != nil:
					probe.SetText("✗ " + err.Error())
				case !r.OK:
					probe.SetText("✗ " + orDefault(r.Error, "无法解析该链接"))
				default:
					bits := []string{}
					if r.Filename != "" {
						bits = append(bits, r.Filename)
					} else {
						bits = append(bits, "未知文件名")
					}
					if r.HLS {
						bits = append(bits, "HLS 流媒体(分段下载)")
					} else {
						if r.Size > 0 {
							bits = append(bits, fmtBytes(r.Size))
						} else {
							bits = append(bits, "大小未知")
						}
						bits = append(bits, map[bool]string{true: "支持断点续传", false: "不支持断点续传"}[r.AcceptRanges])
					}
					if len(lines) > 1 {
						bits = append(bits, fmt.Sprintf("共 %d 个链接,取首个解析", len(lines)))
					}
					probe.SetText("✓ " + strings.Join(bits, " · "))
				}
				probe.Show()
				relayout()
			})
		}()
	}
	urls.OnChanged = func(string) {
		if probeTimer != nil {
			probeTimer.Stop()
		}
		probeTimer = time.AfterFunc(300*time.Millisecond, runProbe)
	}
	// 预填链接立即解析(=openDialog 尾部 runProbe)
	if len(p.URLs) > 0 {
		runProbe()
	}

	submit := func() {
		list := []string{}
		for _, line := range strings.Split(urls.Text, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				list = append(list, s)
			}
		}
		if len(list) == 0 {
			app.toast("请输入至少一个下载链接", "err")
			return
		}
		// 限速换算字节/秒(=submit speedLimit:KB 1024 / MB 1048576)
		mult := int64(1024)
		if slUnit.Selected == "MB/s" {
			mult = 1048576
		}
		req := engine.CreateReq{
			URLs:        list,
			Filename:    strings.TrimSpace(filename.Text),
			Dir:         dir.Text(),
			Connections: atoiDef(connections.Selected, connDef),
			Referer:     strings.TrimSpace(referer.Text),
			Headers:     parseHeadersText(headersE.Text),
			SpeedLimit:  int64(parseFloat(speedLimit.Text) * float64(mult)),
			Checksum:    strings.TrimSpace(checksum.Text),
			Proxy:       proxyValue(),
		}
		// 后台提交:阻塞网络调用不得卡 UI 线程
		go func() {
			tasks, errs, err := app.client.CreateURL(req)
			if err != nil {
				fyne.Do(func() { app.toast(err.Error(), "err") })
				return
			}
			fyne.Do(func() {
				app.toast(fmt.Sprintf("已创建 %d 个任务", len(tasks)), "ok")
				for _, e := range errs {
					app.toast(e, "err")
				}
				if len(tasks) > 0 && strings.TrimSpace(dir.Text()) != "" {
					app.store.RecordRecentDir(strings.TrimSpace(dir.Text()))
				}
				if len(tasks) > 0 {
					app.store.SwitchView("active")
				}
				app.store.RefreshTasks()
			})
		}()
		pop.Hide()
	}
	// =dialog 基类:accent-bar 标题 + 纵向字段(label 在上)+ 右下动作行
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	title := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("新建下载任务", pal.text, true, 17))
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }),
		newPrimaryButton(pal, "开始下载", submit))
	actionsWrap := container.NewBorder(nil, nil, nil, actions)
	content := container.New(&vgapLayout{gap: 14},
		field("下载链接(每行一个,支持批量)", urls),
		probe,
		container.New(layout.NewGridLayoutWithColumns(2),
			field("保存文件名(留空自动识别)", filename),
			field("线程数", connections)),
		field("保存目录", container.NewBorder(nil, nil, nil, browse, dir)),
		advHead,
		advPanel,
		actionsWrap)
	// 标题固定在外、不进滚动区(=用户报的"标题被卷入滚动范围"):卡体 = 标题
	// (顶部,含上内边距 + 18 下边距) + 下方滚动表单。标题下 18px(=Vue h2
	// margin-bottom)放在固定标题块里而非滚动内容顶内边距——放滚动区里会随
	// 内容一起滚走,滚动时下边缘直接顶到标题、观感差(=用户本轮反馈)。左右
	// 24 与标题对齐;高级选项展开后内容超上限转内部滚动(fyne 滚动容器 min 高
	// 只有 32,须 SetMinSize 喂给 fixedMin 卡体)
	titleWrap := container.New(layout.NewCustomPaddedLayout(18, 18, 24, 24), title)
	sc := container.NewVScroll(container.New(
		layout.NewCustomPaddedLayout(0, 18, 24, 24), content))
	inner := container.NewVBox(titleWrap, sc)
	fitHeight = func() {
		h := sc.Content.MinSize().Height
		// 卡高上限 620 按"标题+滚动"整体算:标题固定在外,扣掉后留给滚动内容
		if capH := 620 - titleWrap.MinSize().Height; h > capH {
			h = capH
		}
		sc.SetMinSize(fyne.NewSize(0, h))
		if pop != nil {
			pop.Refresh()
		}
	}
	pop = modalCard(app.win, pal, inner, 560, 0)
	fitHeight()
	pop.Show()
}
