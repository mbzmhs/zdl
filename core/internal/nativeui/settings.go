package nativeui

// 设置对话框(SettingsDialog.vue 移植):六分节全字段 + 保存体逐字段对齐。
// 语义要点:
//   - PUT /api/config 部分合并;开机自启与文件关联各自落库,失败不阻断;
//   - 全局限速/BT 上传限速回显换算:优先整 MB,其次整 KB,否则两位小数 MB;
//   - 文件关联状态取本进程真实查询(Windows 注册表/macOS LaunchServices;
//     mac 的接管是面板内即时动作,不经保存流程);
//   - 伪装名继承 User-Agent(勾选只读跟随);封禁列表/客户端黑名单子弹窗;
//   - 系统日志 3s 自动刷新 + 签名比对(无新日志不重写);
//   - DHT 节点数/封禁计数实时点亮。
// 交互差异(打磨阶段):分节滑动手动指示条 → Fyne AppTabs;悬停提示(q-tip)
// → 控件 tooltip 缺位,文案并入标签后缀或打磨阶段自绘。

import (
	"fmt"
	"image/color"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/assoc"
	"zdownloader/internal/autostart"
	"zdownloader/internal/proxy"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
)

func canvasNewRect(r, g, b, a uint8) *canvas.Rectangle {
	return canvas.NewRectangle(color.NRGBA{R: r, G: g, B: b, A: a})
}

// boolPtr 粒度开关落库(值语义)
func boolPtr(b bool) *bool { return &b }

// boolDerefTrue *bool 读取:nil = 未配置 = 开启(=Vue `!== false`)
func boolDerefTrue(b *bool) bool { return b == nil || *b }

// settingsDraft 表单草稿(打开时整体从服务端快照填充;=SettingsDialog f)
type settingsDraft struct {
	downloadDir                   string
	maxActive, maxConn            int
	globalLimit                   float64
	globalLimitUnit               string
	userAgent                     string
	skipTLS, openBrowser          bool
	uiExitOnClose                 bool
	autoStart, showFloat          bool
	proxyMode, proxyUrl           string
	proxyHTTP, proxyTracker       bool
	proxyPeer, proxyWebSeed       bool
	dohDNS                        bool
	followView                    string
	autoOpenTorrent               bool
	assocMagnet, assocTorrent     bool
	assocPlatform                 string
	assocMagnetOk, assocTorrentOk bool
	btPort                        int
	btMaxPeers                    int
	btUpSpeed                     float64
	btUpSpeedUnit                 string
	btSeedRatio                   float64
	btSeedMinutes                 int
	btDHT, btPEX, btAutoSeed      bool
	btDelGenerated, btDelSource   bool
	btAutoBanPoison               bool
	btPoisonRate, btPoisonStrikes int
	btTrackers                    string
	btAutoTrackers                bool
	btSpoofUA                     bool
	btSpoofName                   string
	dhtNodes                      string
	rssAuto                       bool
	rssInterval, rssMaxItems      int
	rssOnStart                    bool
	rssWindow                     bool
	rssWindowStart, rssWindowEnd  string
	rssAutoDl                     string
	bansCount                     int
	clientBL                      []string
}

// openSettings 构建设置窗并拉取快照
func (app *App) openSettings() {
	// 单例:已有可见设置层 → 重复点击(托盘「设置」/工具栏按钮/uiShow 帧)
	// 直接返回,不再叠新层(每层都是整屏遮罩,叠层只能逐层关掉)。关闭后再
	// 点开走下方重建流程,重新拉最新配置快照
	if app.settingsPop != nil && app.settingsPop.Visible() {
		return
	}
	d := &settingsDraft{
		maxActive: 1, maxConn: 8, globalLimitUnit: "KB",
		proxyMode: "none", btMaxPeers: 60, btUpSpeedUnit: "KB",
		btSeedRatio: 1, btAutoBanPoison: true, btPoisonRate: 80, btPoisonStrikes: 3,
		btSpoofUA: true, btSpoofName: "BitComet 1.99",
		rssInterval: 60, rssMaxItems: 200, rssOnStart: true,
		rssWindowStart: "00:00", rssWindowEnd: "23:59", rssAutoDl: "off",
	}

	cfg, err := app.client.Config()
	if err != nil {
		app.toast(err.Error(), "err")
		return
	}
	app.store.SetCfg(&cfg)
	d.downloadDir = cfg.DownloadDir
	d.maxActive = cfg.MaxActiveTasks
	d.maxConn = cfg.MaxConnections
	gv, gu := unitInput(cfg.GlobalLimit)
	d.globalLimit, d.globalLimitUnit = gv, gu
	d.userAgent = cfg.UserAgent
	d.proxyMode = cfg.Proxy.Mode
	d.proxyUrl = cfg.Proxy.URL
	// 粒度开关 *bool:未配置(nil)视为开启(=Vue `!== false` 语义)
	d.proxyHTTP = boolDerefTrue(cfg.Proxy.ForHTTP)
	d.proxyTracker = boolDerefTrue(cfg.Proxy.ForTracker)
	d.proxyPeer = boolDerefTrue(cfg.Proxy.ForPeer)
	d.proxyWebSeed = boolDerefTrue(cfg.Proxy.ForWebSeed)
	d.dohDNS = boolDerefTrue(cfg.Proxy.DohDNS)
	if d.proxyMode == "" {
		d.proxyMode = "none"
	}
	d.skipTLS = cfg.SkipTLSVerify
	d.openBrowser = cfg.OpenBrowser
	d.uiExitOnClose = cfg.UIExitOnClose
	d.showFloat = cfg.ShowFloatWindow
	d.autoOpenTorrent = cfg.AutoOpenTorrent
	// BT
	{
		bt := cfg.BT
		d.btPort = bt.Port
		if bt.MaxPeers > 0 {
			d.btMaxPeers = bt.MaxPeers
		}
		uv, uu := unitInput(bt.UPSpeed)
		d.btUpSpeed, d.btUpSpeedUnit = uv, uu
		d.btSeedRatio = bt.SeedRatio
		d.btSeedMinutes = bt.SeedMinutes
		d.btDHT = bt.DHT
		d.btPEX = bt.PEX
		d.btAutoSeed = bt.AutoSeed
		d.btDelGenerated = bt.DeleteGeneratedTorrent
		d.btDelSource = bt.DeleteSourceTorrent
		d.btAutoBanPoison = bt.AutoBanPoison
		d.btPoisonRate = bt.PoisonRate
		d.btPoisonStrikes = bt.PoisonStrikes
		d.clientBL = append([]string{}, bt.ClientBlacklist...)
		d.btTrackers = strings.Join(bt.PublicTrackers, "\n")
		d.btAutoTrackers = bt.AutoAddTrackers
		d.btSpoofUA = bt.SpoofUA
		if bt.SpoofName != "" {
			d.btSpoofName = bt.SpoofName
		}
	}
	// RSS
	{
		r := cfg.RSS
		d.rssAuto = r.AutoUpdate
		d.rssInterval = r.IntervalMin
		d.rssMaxItems = r.MaxItems
		d.rssOnStart = r.UpdateOnStart
		d.rssWindow = r.WindowEnabled
		d.rssWindowStart = r.WindowStart
		d.rssWindowEnd = r.WindowEnd
		d.rssAutoDl = r.AutoDL
	}
	// 独立落库项:自启/关联(zdlui 进程内直调注册表,不经引擎;失败不阻塞弹窗)
	d.autoStart = autostart.Enabled()
	d.assocPlatform = runtime.GOOS
	d.assocMagnet, d.assocTorrent = assoc.MagnetEnabled(), assoc.TorrentEnabled()
	d.assocMagnetOk, d.assocTorrentOk = d.assocMagnet, d.assocTorrent
	if bans, err := app.client.Bans(); err == nil {
		d.bansCount = len(bans)
	}
	if st, err := app.client.Stats(); err == nil {
		if n := st.DHTNodes; n > 0 {
			d.dhtNodes = fmt.Sprintf("DHT 路由表 %d 个节点", n)
		} else {
			d.dhtNodes = "DHT 未入网(启动后约 1-2 分钟;或 DHT 未启用)"
		}
	}

	app.buildSettingsUI(d)
}

/* ---------------- 表单控件小助手 ---------------- */

// fieldRow 纵向字段(=field:label 在上 12.5px 600 dim,控件在下)
func (app *App) fieldRow(label string, w fyne.CanvasObject) fyne.CanvasObject {
	pal := app.store.appPal()
	return container.NewVBox(newColorLabelS(label, pal.textDim, true, 12.5),
		container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0), w))
}

// switchRow 开关行(=switch-row:开关+13px 标签,gap 10)
func (app *App) switchRow(label string, v *bool) fyne.CanvasObject {
	pal := app.store.appPal()
	sw := newSwitch(pal, *v, func(on bool) { *v = on })
	return container.NewHBox(sw, container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
		newColorLabelS(label, pal.text, false, 13)))
}

// switchRowTip 开关行 + ? 说明气泡
func (app *App) switchRowTip(label, tip string, v *bool) fyne.CanvasObject {
	pal := app.store.appPal()
	sw := newSwitch(pal, *v, func(on bool) { *v = on })
	return container.NewHBox(sw, container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
		newColorLabelS(label, pal.text, false, 13)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0), newQTip(pal, tip)))
}

// numOpts 1..n 数字下拉项(=NUM_OPTS)
func numOpts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i + 1)
	}
	return out
}

// fieldRowTip 带 ? 说明气泡的纵向字段(=label 内嵌 .q)
func (app *App) fieldRowTip(label, tip string, w fyne.CanvasObject) fyne.CanvasObject {
	pal := app.store.appPal()
	head := container.NewHBox(newColorLabelS(label, pal.textDim, true, 12.5),
		container.New(layout.NewCustomPaddedLayout(0, 0, 5, 0), newQTip(pal, tip)))
	return container.NewVBox(head, container.New(layout.NewCustomPaddedLayout(6, 0, 0, 0), w))
}

// checkRowTip 复选框 + ? 说明气泡(=lbl 包 .q 的开关行)
func (app *App) checkRowTip(label, tip string, v *bool) fyne.CanvasObject {
	pal := app.store.appPal()
	return container.NewHBox(checkRow(pal, label, v),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0), newQTip(pal, tip)))
}

func textEntry(v string) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(v)
	singleLine(e) // 横向裁剪、无纵向滚动条(=input)
	return e
}

func numEntry(v int) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(strconv.Itoa(v))
	singleLine(e)
	return e
}

func floatEntry(v float64) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(fmtFloat(v))
	singleLine(e)
	return e
}

func checkRow(pal palette, label string, v *bool) *styledCheck {
	return newStyledCheck(pal, label, *v, func(on bool) { *v = on })
}

// selectW 下拉(=ZSelect;值与 label 同串,含 value/label 映射的调用方自转换)
func (app *App) selectW(opts []string, cur string) *zSelect {
	return newZSelect(app.store.appPal(), opts, cur)
}

// paneSep 分区分隔线(=pane-sep:1px 虚线,上距 16 margin+14 padding=30、下距 16)
func paneSep(pal palette) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(30, 16, 0, 0),
		newDashedLine(pal.border))
}

/* ---------------- 六分节构建 ---------------- */

// buildSettingsUI 构建设置窗(=settings-dialog:头部/左导航 172px/内容区/动作行)
func (app *App) buildSettingsUI(d *settingsDraft) {
	pal := app.store.appPal()
	// setPane 设置内容页(=.settings-body padding 18px 22px):右侧在
	// 内边距上加滚动条带宽(fyne 滚动条覆盖式,CSS 滚动条占布局空间)
	setPane := func(o fyne.CanvasObject) fyne.CanvasObject {
		return vScrollNS(container.New(
			layout.NewCustomPaddedLayout(18, 18, 22, 30), o))
	}
	panes := map[string]fyne.CanvasObject{
		"general": setPane(app.settingsGeneral(d)),
		"proxy":   setPane(app.settingsProxy(d)),
		"bt":      setPane(app.settingsBT(d)),
		"rss":     setPane(app.settingsRSS(d)),
		"assoc":   setPane(app.settingsAssoc(d)),
	}
	logPane, logStart := app.settingsLog()
	panes["log"] = logPane

	type cat struct{ key, name, icon string }
	cats := []cat{
		{"general", "通用", "settings"},
		{"proxy", "代理", "public"},
		{"bt", "BT 下载", "radar"},
		{"rss", "RSS 订阅", "graphic_eq"},
		{"assoc", "文件关联", "link"},
		{"log", "系统日志", "article"},
	}
	stopLog := make(chan struct{})
	logStarted := false
	navBox := container.New(&vgapLayout{gap: 4})
	navRows := map[string]*navRow{}
	switchCat := func(key string) {
		for _, c := range cats {
			on := c.key == key
			navRows[c.key].SetActive(on)
			if on {
				panes[c.key].Show()
				if c.key == "log" && !logStarted {
					logStarted = true
					logStart(stopLog)
				}
			} else {
				panes[c.key].Hide()
			}
		}
	}
	for _, c := range cats {
		k := c.key
		row := newNavRow(pal, c.icon, c.name, func() { switchCat(k) })
		// Vue .settings-nav-item.active 只有文字提亮无选中底,不能沿用侧栏
		// 的渐变底+左描边(=用户报的整行变蓝)
		row.plain = true
		navRows[k] = row
		navBox.Add(row)
	}
	navPanel := canvas.NewRectangle(pal.softA(0.55))
	navLine := canvas.NewRectangle(pal.border)
	navLine.SetMinSize(fyne.NewSize(1, 0))
	nav := newFixedMin(container.NewStack(navPanel,
		container.NewBorder(nil, nil, nil, navLine,
			// 右内边距 +8 让位覆盖式滚动条;滚动区去边缘黑影
			container.New(layout.NewCustomPaddedLayout(14, 14, 10, 18),
				container.NewVScroll(navBox)))),
		fyne.NewSize(172, 10))

	paneStack := container.NewStack(
		panes["general"], panes["proxy"], panes["bt"], panes["rss"], panes["assoc"], panes["log"])
	body := container.NewBorder(nil, nil, nav, nil, paneStack)

	// 头部(=settings-head:h2 + 底部分隔线)
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("设置", pal.text, true, 17))
	headPad := container.New(layout.NewCustomPaddedLayout(18, 12, 22, 22), head)
	headLine := canvas.NewRectangle(pal.border)
	headLine.SetMinSize(fyne.NewSize(0, 1))
	headBlock := container.NewBorder(headPad, headLine, nil, nil)

	// 动作行(=settings-actions:顶部分隔线 + 取消/保存右对齐)
	var pop *maskModal
	actions := hGap(10,
		newGhostButton(pal, "取消", func() { pop.Hide() }),
		newPrimaryButton(pal, "保存", func() { app.saveSettings(d, func() { pop.Hide() }) }))
	actionsPad := container.New(layout.NewCustomPaddedLayout(14, 14, 22, 22),
		container.NewBorder(nil, nil, nil, actions))
	actLine := canvas.NewRectangle(pal.border)
	actLine.SetMinSize(fyne.NewSize(0, 1))
	actionsBlock := container.NewBorder(nil, nil, nil, nil, container.NewBorder(actLine, nil, nil, nil, actionsPad))

	inner := container.NewBorder(headBlock, actionsBlock, nil, nil, body)
	pop = modalCard(app.win, pal, inner, 900, 680)
	app.settingsPop = pop // 单例句柄:openSettings 据此判重,旧层被新层替换引用
	pop.Show()
	switchCat("general")
	// 日志页轮询随窗关闭收尾(PopUp 无关闭回调,轮询 Visible 兜底)
	go func() {
		for pop.Visible() {
			time.Sleep(500 * time.Millisecond)
		}
		close(stopLog)
	}()
}

// settingsGeneral 通用分节
func (app *App) settingsGeneral(d *settingsDraft) fyne.CanvasObject {
	pal := app.store.appPal()
	dirE := textEntry(d.downloadDir)
	browse := newGhostButton(pal, "浏览…", nil)
	browse.SetMini()
	browse.onTap = func() {
		app.pickFolderAsync("选择默认下载目录", func(p string, ok bool) {
			if ok {
				fyne.Do(func() { dirE.SetText(p) })
			}
		})
	}
	dirE.OnChanged = func(s string) { d.downloadDir = s }
	// 数值项用下拉(=ZSelect NUM_OPTS:任务数 1..10、线程数 1..64)
	maxActive := app.selectW(numOpts(10), strconv.Itoa(d.maxActive))
	maxActive.OnChanged = func(s string) { d.maxActive = atoiDef(s, 1) }
	maxConn := app.selectW(numOpts(64), strconv.Itoa(d.maxConn))
	maxConn.OnChanged = func(s string) { d.maxConn = atoiDef(s, 8) }
	gl := floatEntry(d.globalLimit)
	gl.OnChanged = func(s string) { d.globalLimit = parseFloat(s) }
	glUnit := app.selectW([]string{"KB/s", "MB/s"}, d.globalLimitUnit)
	glUnit.OnChanged = func(s string) { d.globalLimitUnit = s }
	ua := textEntry(d.userAgent)
	ua.OnChanged = func(s string) { d.userAgent = s }
	// 悬浮窗由常驻组件 zdl_shell 承载,桌面端开关真实生效(与 Vue 版同位置同字段)
	floatRow := app.switchRow("悬浮下载进度窗", &d.showFloat)
	// 行距 14(=field/switch-row margin-bottom:14)
	return container.New(&vgapLayout{gap: 14},
		app.fieldRow("默认下载目录", container.NewBorder(nil, nil, nil, browse, dirE)),
		container.New(layout.NewGridLayoutWithColumns(2), app.fieldRow("同时下载任务数", maxActive), app.fieldRow("默认线程数", maxConn)),
		container.New(layout.NewGridLayoutWithColumns(2), app.fieldRow("全局限速(0 为不限速)", container.NewBorder(nil, nil, nil, glUnit, gl)), app.fieldRow("User-Agent", ua)),
		paneSep(pal),
		app.switchRow("跳过 TLS 证书验证(自签名场景)", &d.skipTLS),
		app.switchRow("启动时自动打开管理界面", &d.openBrowser),
		app.switchRow("开机自动启动(登录后静默常驻托盘)", &d.autoStart),
		// 分离架构:关窗默认隐藏驻留(托盘唤起秒开、状态保留);打开后关窗即
		// 退出 UI 进程释放内存(常驻只剩守护),代价是下次唤起重新加载
		app.switchRow("关闭窗口时退出 UI 进程(释放内存)", &d.uiExitOnClose),
		floatRow,
	)
}

// settingsProxy 代理分节。与 Vue 一致按模式条件显示:env→只读回显、
// manual→地址框、非 none→五个粒度开关(行距 14)
func (app *App) settingsProxy(d *settingsDraft) fyne.CanvasObject {
	pal := app.store.appPal()
	// 下拉选项直接用中文标签(=PROXY_MODE_OPTS),value 与 label 双向映射
	modeLabel := map[string]string{"none": "不使用代理", "manual": "手动配置", "env": "跟随系统环境变量"}
	labelMode := map[string]string{}
	for k, v := range modeLabel {
		labelMode[v] = k
	}
	mode := app.selectW([]string{modeLabel["none"], modeLabel["manual"], modeLabel["env"]}, modeLabel[d.proxyMode])
	urlE := textEntry(d.proxyUrl)
	urlE.OnChanged = func(s string) { d.proxyUrl = s }
	urlE.PlaceHolder = "http://127.0.0.1:7890 或 socks5://user:pass@127.0.0.1:1080"
	follow := textEntry(d.followView)
	follow.Disable()
	refreshFollow := func() {
		s := proxy.FollowSystemStatus()
		{
			switch s.Source {
			case "env":
				d.followView = "环境变量 " + s.EnvVar + " → " + orDash2(s.URL, s.Addr)
			case "windows":
				d.followView = "Windows 系统代理 → " + orDash2(s.URL, s.Addr)
			case "macos":
				d.followView = "macOS 系统代理 → " + orDash2(s.URL, s.Addr)
			default:
				d.followView = "未检测到代理(将直连)"
			}
			follow.SetText(d.followView)
		}
	}
	followHint := hintText(pal, "检测到的代理(只读):")
	urlField := app.fieldRowTip("代理地址", "支持 HTTP(S) 与 SOCKS5 代理;内网地址始终直连。", urlE)
	// 粒度开关组整组显隐,组内行距 14 与其他行一致
	uses := container.New(&vgapLayout{gap: 14},
		app.switchRow("使用代理进行 HTTP 下载", &d.proxyHTTP),
		app.switchRowTip("使用代理连接 Tracker", "udp tracker 直连失败时经 SOCKS5 中继重试(HTTP 代理无 UDP 通道);DHT 始终直连。", &d.proxyTracker),
		app.switchRow("使用代理连接 Peer", &d.proxyPeer),
		app.switchRow("使用代理连接长效种子(WebSeed)", &d.proxyWebSeed),
		app.switchRowTip("DNS 解析走代理(DoH)", "经 DoH 加密解析域名,防本地 DNS 污染;tracker 解析与 DHT 引导均使用。", &d.dohDNS),
	)
	// 条件显示与 Vue computed 一致:env→只读回显+开关组、manual→地址框+
	// 开关组、none→全隐;显隐前先全收,避免分支漏网
	applyMode := func(m string) {
		followHint.Hide()
		follow.Hide()
		urlField.Hide()
		uses.Hide()
		switch m {
		case "env":
			refreshFollow()
			followHint.Show()
			follow.Show()
			uses.Show()
		case "manual":
			urlField.Show()
			uses.Show()
		}
	}
	mode.OnChanged = func(s string) {
		d.proxyMode = labelMode[s]
		applyMode(d.proxyMode)
	}
	applyMode(d.proxyMode)
	return container.New(&vgapLayout{gap: 14},
		app.fieldRowTip("代理模式", "优先读取环境变量 HTTPS_PROXY / HTTP_PROXY / ALL_PROXY,同时识别 Windows 系统代理(Internet 选项);NO_PROXY / 例外列表生效,下方粒度开关同样约束各用途;DHT 始终直连,udp tracker 仅 SOCKS5 代理支持中继(HTTP 代理无 UDP 通道)。", mode),
		followHint,
		follow,
		urlField,
		paneSep(pal),
		uses,
	)
}

// settingsBT BT 分节(行距 14;DHT 路由表计数内嵌开关行=Vue 485)
func (app *App) settingsBT(d *settingsDraft) fyne.CanvasObject {
	pal := app.store.appPal()
	port := numEntry(d.btPort)
	port.OnChanged = func(s string) { d.btPort = atoiDef(s, 0) }
	maxPeers := numEntry(d.btMaxPeers)
	maxPeers.OnChanged = func(s string) { d.btMaxPeers = atoiDef(s, 60) }
	up := floatEntry(d.btUpSpeed)
	up.OnChanged = func(s string) { d.btUpSpeed = parseFloat(s) }
	upUnit := app.selectW([]string{"KB/s", "MB/s"}, d.btUpSpeedUnit)
	upUnit.OnChanged = func(s string) { d.btUpSpeedUnit = s }
	ratio := floatEntry(d.btSeedRatio)
	ratio.OnChanged = func(s string) { d.btSeedRatio = parseFloat(s) }
	minutes := numEntry(d.btSeedMinutes)
	minutes.OnChanged = func(s string) { d.btSeedMinutes = atoiDef(s, 0) }
	poisonRate := numEntry(d.btPoisonRate)
	poisonRate.OnChanged = func(s string) { d.btPoisonRate = atoiDef(s, 80) }
	poisonStrikes := numEntry(d.btPoisonStrikes)
	poisonStrikes.OnChanged = func(s string) { d.btPoisonStrikes = atoiDef(s, 3) }
	trackers := widget.NewMultiLineEntry()
	trackers.Wrapping = fyne.TextWrapWord // 多行回绕,不出横向滚动条(=textarea)
	trackers.SetText(d.btTrackers)
	trackers.SetPlaceHolder("udp://tracker.opentrackr.org:1337/announce")
	trackers.OnChanged = func(s string) { d.btTrackers = s }
	spoofName := textEntry(d.btSpoofName)
	spoofName.SetPlaceHolder("BitComet 1.99")
	spoofName.OnChanged = func(s string) { d.btSpoofName = s }
	if d.btSpoofUA {
		spoofName.Disable() // 继承 UA 时只读(=Vue :readonly)
	}
	dhtHint := newColorLabelS(d.dhtNodes, pal.textFaint, false, 11.5).SetMaxWidth()
	if d.dhtNodes == "" {
		dhtHint.SetText("DHT 路由表: –")
	}
	var bansBtn *textBtn
	bansBtn = newGhostButton(pal, fmt.Sprintf("封禁列表 (%d)", d.bansCount), func() { app.openBans(d, bansBtn) })
	var blBtn *textBtn
	blBtn = newGhostButton(pal, fmt.Sprintf("管理(%d 条)…", len(d.clientBL)), func() { app.openClientBL(d, blBtn) })
	// 伪装名行:开关直接控制输入框只读(Vue v-model+readonly 同一行内联动)
	spoofSw := newSwitch(pal, d.btSpoofUA, func(on bool) {
		d.btSpoofUA = on
		if on {
			spoofName.Disable()
		} else {
			spoofName.Enable()
		}
	})
	spoofRow := container.NewHBox(spoofSw,
		container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
			newColorLabelS("伪装名继承 User-Agent", pal.text, false, 13)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0),
			newQTip(pal, "勾选后 BT 客户端名称跟随上方 User-Agent 设置;取消勾选可单独填写。")))
	return container.New(&vgapLayout{gap: 14},
		container.New(layout.NewGridLayoutWithColumns(2), app.fieldRowTip("BT 监听端口", "0 = 自动选择;端口被占用时自动顺延。所有 BT 任务共享同一监听端口,入站连接按种子自动路由;NAT 穿透映射此端口。", port), app.fieldRow("单任务最大 peer 连接数", maxPeers)),
		app.fieldRow("全局上传限速(0 为不限速)", container.NewBorder(nil, nil, nil, upUnit, up)),
		container.New(layout.NewGridLayoutWithColumns(2), app.fieldRow("做种目标分享率(0 = 不按比率停)", ratio), app.fieldRow("最短做种时长(分钟,0 = 不按时长停)", minutes)),
		paneSep(pal),
		app.dhtRow(pal, d, dhtHint),
		app.switchRowTip("使用 PEX", "peer 交换:与已连接的 peer 互换更多 peer 地址;私有种子自动禁用。", &d.btPEX),
		app.switchRowTip("任务完成后自动做种", "完成后继续上传,分享率或最短时长任一达标即停止;两项均为 0 则一直做种。", &d.btAutoSeed),
		app.switchRowTip("创建 BT 任务后删除生成的种子文件", "本程序生成的种子(磁链抓取产物、HTTP 下载完成的种子自动转建)在任务创建后删除其 .torrent 副本。引擎保留内部元数据存档,下载/续传/做种不受影响,详情页可随时「另存为种子」取回。", &d.btDelGenerated),
		app.switchRowTip("删除双击/手动打开的种子源文件", "双击 .torrent 或手动选路径创建任务后,删除你自有的那个源种子文件。默认关闭——多数用户想保留自己的种子库,需要清理时再开。", &d.btDelSource),
		paneSep(pal),
		app.banRow(pal, d, bansBtn),
		app.fieldRowTip("白名单 piece 有效率阈值(%)", "该 IP 参与过的 piece 中通过校验的比例达到此值才豁免自动封禁(参与片数就是「连接」表里的 piece有效率列);低于即踢出白名单,之后累计坏块达宽容次数即封。默认 80,可设 50 - 100。阈值越高越严格:100 表示掺一片坏块就失去豁免。无样本(从未参与过 piece 校验)的 peer 不在白名单内。", poisonRate),
		app.fieldRowTip("坏块宽容次数(次)", "同一 IP 累计供坏块达到该次数才自动封禁(白名单 peer 有效率达标期间不计打击,跌破阈值后从零开始累计)。默认 3,可设 1 - 10:1 = 掺一片坏块就封,最严但对我方 uTP 接收瑕疵误伤的容忍度也最低;调大更宽容。只改封禁节奏,不改判定口径;只想观察不封禁请用上方总开关。", poisonStrikes),
		app.fieldRowTip("客户端黑名单", "按对端客户端名称自动封禁,快速拉黑迅雷等吸血端与 PCDN 常用客户端。支持通配符:星号匹配任意长度、问号匹配单个字符,大小写不敏感(如 *Xunlei*、*Thunder*)。命中即写入全局封禁名单并断开连接,保存立即生效——新连接在握手期直接拒绝,已连上的热封断开。注意:有 PCDN 伪装成 qBittorrent 4.6.7,拉黑 qB 系列会误伤大量正常 peer,请精确到版本。", blBtn),
		app.fieldRowTip("公共 tracker 列表", "每行一个。自动添加到所有 BT 任务;磁力链接抓取元数据时始终使用。", trackers),
		app.switchRowTip("自动补充公共 tracker", "把上方公共 tracker 列表自动添加到 BT 任务(下载阶段生效;私有种子始终不添加)。", &d.btAutoTrackers),
		paneSep(pal),
		spoofRow,
		// 输入框弹性撑满,qtip 固定在尾(hflex;HBox 会把它压成 min 宽)
		hFlex(4, flex(spoofName),
			newQTip(pal, "BT 对外呈现的客户端名称(扩展握手 / tracker 通告 UA / 制种信息),peer_id 前缀固定为 BitComet 格式以通过兼容性白名单。")),
	)
}

// dhtRow DHT 开关行,尾部内嵌路由表计数(=Vue 485 的 hint-text margin 0 0 0 8px)
func (app *App) dhtRow(pal palette, d *settingsDraft, hint *colorLabel) fyne.CanvasObject {
	sw := newSwitch(pal, d.btDHT, func(on bool) { d.btDHT = on })
	// 路由表提示走 Border 中缝吃满剩余宽(=Vue flex:1 截断):放 HBox 时
	// SetMaxWidth 的 MinSize 宽只有 10px,整段提示会缩成一个「…」
	left := container.NewHBox(sw,
		container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
			newColorLabelS("使用 DHT", pal.text, false, 13)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0),
			newQTip(pal, "无 tracker 磁力链接的 peer 来源:经公共路由节点(DHT 网络)发现 peer。")))
	return container.NewBorder(nil, nil, left, nil,
		container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), hint))
}

// banRow 自动封禁开关行 + 封禁列表按钮(=Vue 503-507;按钮不随行高拉伸)
func (app *App) banRow(pal palette, d *settingsDraft, bansBtn *textBtn) fyne.CanvasObject {
	sw := newSwitch(pal, d.btAutoBanPoison, func(on bool) { d.btAutoBanPoison = on })
	return container.NewHBox(sw,
		container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
			newColorLabelS("自动封禁PCDN客户端", pal.text, false, 13)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0),
			newQTip(pal, "两类特征自动拉黑:① piece 校验失败按块来源追责,同一 IP 累计供坏块达下方「坏块宽容次数」(默认 3 次)封禁(整块唯一来源同样按次数累计);对端要维持在「白名单」里才豁免,白名单按下方 piece 有效率阈值动态判定——校验通过才算好块,混着掺坏块把有效率拖到阈值以下就当场踢出、按一般 peer 记打击。② 只取不予:下载期间长期索取我们上 MB 数据、回传不足其 1/16 且不到 512KB(对端自报的下载进度可以伪造,不作依据;做种期改按「回报零且进度静止」判定),观察满 10 分钟即封。封禁名单全局共享、重启仍生效;点「封禁列表」可查看与解除。")),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0), bansBtn))
}

// settingsRSS RSS 分节(行距 14;时段行开关控制时间行显隐=Vue v-show)
func (app *App) settingsRSS(d *settingsDraft) fyne.CanvasObject {
	pal := app.store.appPal()
	interval := numEntry(d.rssInterval)
	interval.OnChanged = func(s string) { d.rssInterval = atoiDef(s, 60) }
	maxItems := numEntry(d.rssMaxItems)
	maxItems.OnChanged = func(s string) { d.rssMaxItems = atoiDef(s, 200) }
	startT := textEntry(d.rssWindowStart)
	startT.OnChanged = func(s string) { d.rssWindowStart = s }
	endT := textEntry(d.rssWindowEnd)
	endT.OnChanged = func(s string) { d.rssWindowEnd = s }
	// 自动下载下拉用中文标签(=RSS_AUTODL_OPTS),value/label 双向映射
	adLabel := map[string]string{"off": "不自动下载", "notify": "自动下载(完成后提醒)", "silent": "静默下载"}
	adMode := map[string]string{}
	for k, v := range adLabel {
		adMode[v] = k
	}
	autoDl := app.selectW([]string{adLabel["off"], adLabel["notify"], adLabel["silent"]}, adLabel[d.rssAutoDl])
	autoDl.OnChanged = func(s string) { d.rssAutoDl = adMode[s] }
	winGrid := container.New(layout.NewGridLayoutWithColumns(2),
		app.fieldRow("开始时间", startT), app.fieldRow("结束时间", endT))
	if !d.rssWindow {
		winGrid.Hide()
	}
	winSw := newSwitch(pal, d.rssWindow, func(on bool) {
		d.rssWindow = on
		if on {
			winGrid.Show()
		} else {
			winGrid.Hide()
		}
	})
	winRow := container.NewHBox(winSw,
		container.New(layout.NewCustomPaddedLayout(0, 0, 10, 0),
			newColorLabelS("仅在指定时段内自动更新", pal.text, false, 13)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 4, 0),
			newQTip(pal, "时段外不自动抓取(手动刷新不受限制);支持跨天时段,如 23:00 - 06:00。")))
	refreshBtn := newGhostButton(pal, "立即更新", func() {
		go func() {
			if err := app.client.RSSRefreshAll(); err != nil {
				fyne.Do(func() { app.toast(err.Error(), "err") })
				return
			}
			fyne.Do(func() { app.toast("已开始更新全部订阅", "ok") })
		}()
	})
	return container.New(&vgapLayout{gap: 14},
		app.switchRowTip("定时自动更新订阅", "按下方间隔抓取全部订阅的新条目;关闭后仅在 RSS 页面手动点「全部更新」。", &d.rssAuto),
		container.New(layout.NewGridLayoutWithColumns(2),
			app.fieldRowTip("更新间隔(分钟)", "5 - 1440 分钟;每轮只抓取已到期的订阅,带 ETag/Last-Modified 条件请求,内容没变不重复下载。", interval),
			app.fieldRowTip("每个订阅保留条目数", "20 - 5000;超出后从最旧条目开始丢弃(已读状态随之清理)。", maxItems)),
		app.switchRowTip("启动后立即更新一次", "关闭时启动后先静默一个完整间隔再开始自动更新,避免开机瞬间集中抓取。", &d.rssOnStart),
		winRow,
		winGrid,
		app.fieldRowTip("自动下载", "每次更新订阅时自动下载新条目的资源链接(附件/磁力),条目页面不会被下载;触发后条目置为已读,完成后可提醒。全局默认值,各订阅可在添加/编辑订阅时单独设为「继承全局」或指定其他模式。", autoDl),
		paneSep(pal),
		// 立即更新行:说明左、按钮右(=margin-left:auto)
		container.NewHBox(newColorLabelS("立即更新全部订阅", pal.text, false, 13),
			layout.NewSpacer(), refreshBtn),
		hintText(pal, "订阅抓取遵循「代理」页的设置(HTTP 用途开关);磁力与种子附件可直接创建 BT 任务。"),
	)
}

// settingsAssoc 文件关联分节:Windows 走注册表双开关(保存时生效);macOS 走
// LaunchServices 接管流——两种关联随 .app 一并生效(不能单独开关),开启后
// 系统弹确认框异步生效,取消关联系统不允许应用代劳(只能去系统设置改默认
// 应用),故 mac 侧是「即时动作按钮」而非「保存延迟的双开关」。裸跑(非
// .app)无 bundle 标识,接管按钮会给出明确报错。
func (app *App) settingsAssoc(d *settingsDraft) fyne.CanvasObject {
	pal := app.store.appPal()
	if d.assocPlatform == "darwin" {
		status := func(ok bool) *colorLabel {
			c := pal.textFaint
			if ok {
				c = pal.ok
			}
			return newColorLabelS(boolText(ok), c, true, 12)
		}
		// 状态行就地刷新:LS 接管的系统确认框是异步的,点「接管」后延迟复查
		// 一次,用户确认完回来即见实况;下次进设置页也会重查(草稿每次打开
		// 都重新加载)。
		magnetLbl, torrentLbl := status(d.assocMagnetOk), status(d.assocTorrentOk)
		refresh := func() {
			time.AfterFunc(3*time.Second, func() {
				fyne.Do(func() {
					magnetLbl.SetText(boolText(assoc.MagnetEnabled()))
					torrentLbl.SetText(boolText(assoc.TorrentEnabled()))
				})
			})
		}
		takeOver := newGhostButton(pal, "接管为系统默认", func() {
			go func() {
				err := assoc.Set(true, true)
				fyne.Do(func() {
					if err != nil {
						app.toast(err.Error(), "err")
						return
					}
					app.toast("已请求接管,请在系统确认框中点「使用 ZDownloader」", "ok")
				})
				refresh()
			}()
		})
		openPane := newGhostButton(pal, "打开系统设置", func() {
			if err := assoc.OpenSystemSettings(); err != nil {
				fyne.Do(func() { app.toast(err.Error(), "err") })
			}
		})
		rows := []fyne.CanvasObject{
			container.NewHBox(newColorLabelS("magnet: 链接", pal.text, false, 13),
				container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), magnetLbl)),
			container.NewHBox(newColorLabelS("种子文件(.torrent)", pal.text, false, 13),
				container.New(layout.NewCustomPaddedLayout(0, 0, 8, 0), torrentLbl)),
		}
		if d.assocMagnetOk && d.assocTorrentOk {
			rows = append(rows,
				container.NewHBox(newColorLabelS("已是系统默认", pal.ok, false, 13),
					layout.NewSpacer(), openPane))
		} else {
			rows = append(rows,
				container.NewHBox(newColorLabelS("接管系统默认关联", pal.text, false, 13),
					layout.NewSpacer(), takeOver),
				container.NewHBox(newColorLabelS("取消关联", pal.text, false, 13),
					layout.NewSpacer(), openPane))
		}
		rows = append(rows,
			app.switchRowTip("自动打开下载完成的种子文件", "本程序 HTTP 下载的 .torrent 完成后,自动按其内容创建 BT 任务;同 InfoHash 已有任务时不重复创建。仅对本程序下载的种子生效,随时可在此关闭。", &d.autoOpenTorrent),
			hintText(pal, "关联随 ZDownloader.app 一并生效:接管后系统弹确认框,点确认即生效(已为默认时静默);取消关联请在系统设置中更改默认应用。以 .app 方式运行时才能管理关联。"),
		)
		return container.New(&vgapLayout{gap: 14}, rows...)
	}
	return container.New(&vgapLayout{gap: 14},
		app.switchRowTip("磁力链接关联", "注册 magnet: 协议:点击磁力链接时由本程序接手——程序在运行则转交并在界面弹出创建窗口(可选保存目录与下载文件),未运行则自动启动后同样弹出。写入当前用户注册表(HKCU),无需管理员权限;若此前已为磁力链接指定其他程序,需在系统「默认应用」设置中切换一次。", &d.assocMagnet),
		app.switchRowTip("种子文件关联(.torrent)", "注册 .torrent 打开方式:双击种子文件用本程序打开,界面弹出创建窗口(可选保存目录与下载文件)后开始下载。写入当前用户注册表(HKCU),无需管理员权限;若系统此前已为 .torrent 指定其他默认程序,需在资源管理器「打开方式」中确认一次。", &d.assocTorrent),
		app.switchRowTip("自动打开下载完成的种子文件", "本程序 HTTP 下载的 .torrent 完成后,自动按其内容创建 BT 任务;同 InfoHash 已有任务时不重复创建。仅对本程序下载的种子生效,随时可在此关闭。", &d.autoOpenTorrent),
		hintText(pal, "关联写入当前用户注册表,保存设置时生效;命令已指向其他程序的关联不会被改动。"),
	)
}

// settingsLog 系统日志:自动滚动、可选中复制的文本框(对照 Vue
// <pre class="log-view">)+ 3s 自动刷新 + 签名比对(无新日志不重写)。
// 用 Entry 承载文本:天生可选中、可 Ctrl+C 复制(=pre 的可复制性);多行+
// 换行+纵向内部滚动。v2.8 的 TextStyle 无等宽开关,故等宽观感退化为应用
// 雅黑——原生应用通体雅黑,统一优先于逐控件复刻 pre 的等宽。
func (app *App) settingsLog() (fyne.CanvasObject, func(stop chan struct{})) {
	pal := app.store.appPal()
	entry := widget.NewMultiLineEntry()
	entry.Wrapping = fyne.TextWrapWord
	entry.Scroll = fyne.ScrollVerticalOnly
	// 只读观感:拦截输入,任何改动立即还原为最新日志。选择/复制不触发
	// OnChanged,故不受影响——保住「可复制」的同时挡住误输入
	var mu sync.Mutex
	var locked string
	suppress := false
	entry.OnChanged = func(s string) {
		if suppress {
			return
		}
		mu.Lock()
		want := locked
		mu.Unlock()
		if s != want {
			suppress = true
			entry.SetText(want)
			suppress = false
		}
	}
	// 钉底:Entry 的内部滚动条不导出,只能从 renderer 的 Objects() 里把
	// *container.Scroll(=internal/widget.Scroll 别名)捞出来滚动;放在下一
	// 帧执行,此时本帧布局已完成、滚动区尺寸已知
	scrollToBottom := func() {
		if r := entry.CreateRenderer(); r != nil {
			for _, o := range r.Objects() {
				if sc, ok := o.(*container.Scroll); ok {
					sc.ScrollToBottom()
					return
				}
			}
		}
	}
	refresh := func() {
		lines, err := app.client.LogLines(200)
		fyne.Do(func() {
			if err != nil {
				entry.SetText("加载失败:" + err.Error())
				return
			}
			s := strings.Join(lines, "\n")
			if s == "" {
				// 空日志给占位文案:否则与初始 locked 同为 "" 走「未变」早退,
				// 页面永远停在初始空态
				s = "(暂无日志)"
			}
			mu.Lock()
			if s == locked {
				mu.Unlock()
				return // 内容未变不动:省一次重绘,也不打断用户的选中/滚动
			}
			locked = s
			mu.Unlock()
			entry.SetText(s)
			entry.Refresh()
			fyne.Do(scrollToBottom) // 新日志钉在底部(=Vue 日志尾部跟随)
		})
	}
	refresh()
	// 标题/提示固定在外(不随日志滚动);日志框吃满其下全部高度、内部滚动,
	// 左右内边距对齐其余分节(setPane 的 22/30)
	title := container.NewVBox(
		newColorLabelS("系统日志", pal.text, true, 14),
		hintText(pal, "全局尾部日志(最近 200 行);设置窗口打开期间每 3 秒自动刷新。"))
	topBlock := container.New(layout.NewCustomPaddedLayout(18, 0, 22, 30), title)
	// Entry 是 BaseWidget、无 SetMinSize:用 fixedMin 兜底最小高(小窗不至于
	// 塌成一条),fullLayout 让条目撑满 min 框再吃满标题以下的剩余高度
	entryMin := newFixedMin(container.New(&fullLayout{}, entry), fyne.NewSize(0, 120))
	entryBox := container.New(layout.NewCustomPaddedLayout(12, 18, 22, 30), entryMin)
	return container.NewBorder(topBlock, nil, nil, nil, entryBox), func(stop chan struct{}) {
		t := time.NewTicker(3 * time.Second)
		go func() {
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					refresh()
				}
			}
		}()
	}
}

/* ---------------- 保存(=save:PUT /api/config 部分合并) ---------------- */

func (app *App) saveSettings(d *settingsDraft, onDone func()) {
	cfg := conf.Config{
		DownloadDir:    strings.TrimSpace(d.downloadDir),
		MaxActiveTasks: d.maxActive,
		MaxConnections: d.maxConn,
		GlobalLimit:    int64(d.globalLimit * unitMult(d.globalLimitUnit)),
		UserAgent:      strings.TrimSpace(d.userAgent),
		Proxy: conf.Proxy{
			Mode:       d.proxyMode,
			URL:        strings.TrimSpace(d.proxyUrl),
			ForHTTP:    boolPtr(d.proxyHTTP),
			ForTracker: boolPtr(d.proxyTracker),
			ForPeer:    boolPtr(d.proxyPeer),
			ForWebSeed: boolPtr(d.proxyWebSeed),
			DohDNS:     boolPtr(d.dohDNS),
		},
		SkipTLSVerify:   d.skipTLS,
		OpenBrowser:     d.openBrowser,
		ShowFloatWindow: d.showFloat,
		AutoOpenTorrent: d.autoOpenTorrent,
		UIExitOnClose:   d.uiExitOnClose,
		BT: conf.BTConfig{
			Port:                   d.btPort,
			MaxPeers:               d.btMaxPeers,
			UPSpeed:                int64(d.btUpSpeed * unitMult(d.btUpSpeedUnit)),
			SeedRatio:              d.btSeedRatio,
			SeedMinutes:            d.btSeedMinutes,
			DHT:                    d.btDHT,
			PEX:                    d.btPEX,
			AutoSeed:               d.btAutoSeed,
			DeleteGeneratedTorrent: d.btDelGenerated,
			DeleteSourceTorrent:    d.btDelSource,
			AutoBanPoison:          d.btAutoBanPoison,
			PoisonRate:             clampInt(d.btPoisonRate, 50, 100),
			PoisonStrikes:          clampInt(d.btPoisonStrikes, 1, 10),
			PublicTrackers:         splitLines(d.btTrackers),
			AutoAddTrackers:        d.btAutoTrackers,
			SpoofUA:                d.btSpoofUA,
			SpoofName:              spoofNameOut(d.btSpoofUA, d.btSpoofName),
			ClientBlacklist:        d.clientBL,
		},
		RSS: conf.RSSConfig{
			AutoUpdate:    d.rssAuto,
			IntervalMin:   d.rssInterval,
			UpdateOnStart: d.rssOnStart,
			WindowEnabled: d.rssWindow,
			WindowStart:   orDefault(d.rssWindowStart, "00:00"),
			WindowEnd:     orDefault(d.rssWindowEnd, "23:59"),
			MaxItems:      d.rssMaxItems,
			AutoDL:        d.rssAutoDl,
		},
	}
	go func() {
		saved, err := app.client.SaveConfig(cfg)
		fyne.Do(func() {
			if err != nil {
				app.toast(err.Error(), "err")
				return
			}
			// 用服务端规整后的返回值刷新本地快照:分离模式下关窗拦截直接读
			// store 快照里的 UIExitOnClose 决定"隐藏驻留/退进程",不刷新会
			// 按旧值行事(旧单进程模式读 mgr 实时配置,无此问题)
			app.store.SetCfg(&saved)
		})
		// 自启/关联:zdlui 进程内直调(注册表/LS),失败不影响其余设置
		var autoErr, assocErr error
		if d.autoStart {
			autoErr = autostart.Enable()
		} else {
			autoErr = autostart.Disable()
		}
		if autoErr != nil {
			fyne.Do(func() { app.toast(autoErr.Error(), "err") })
		}
		// darwin 的关联不走保存流程:面板内「接管」按钮即时调 LS(见
		// settingsAssoc),此处仅 Windows 注册表开关
		if d.assocPlatform != "darwin" {
			assocErr = assoc.Set(d.assocMagnet, d.assocTorrent)
			fyne.Do(func() {
				if assocErr != nil {
					app.toast(assocErr.Error(), "err")
					return
				}
				d.assocMagnet, d.assocTorrent = assoc.MagnetEnabled(), assoc.TorrentEnabled()
			})
		}
		fyne.Do(func() {
			app.toast("设置已保存", "ok")
			if onDone != nil {
				onDone()
			}
		})
	}()
}

func spoofNameOut(inherit bool, name string) string {
	if inherit {
		return "" // 继承 UA:服务端取 User-Agent
	}
	return strings.TrimSpace(name)
}

func splitLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func atoiDef(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

func boolText(b bool) string {
	if b {
		return "已接管 ✓"
	}
	return "未接管"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

/* ---------------- 封禁列表 / 客户端黑名单子弹窗 ---------------- */

// openBans 封禁列表(=BansDialog.vue:表格 1fr/.9fr/148px/1.2fr/28px,逐行
// 解除 + 清空全部,计数同步设置页按钮)。此前是纯 IP 文本列表且误按
// []string 解码 BanEntry 数组,列表空白还报 unmarshal 错
func (app *App) openBans(d *settingsDraft, bansBtn *textBtn) {
	pal := app.store.appPal()
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	cntLbl := newColorLabelS("", pal.textFaint, false, 13)
	head := container.NewHBox(
		newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 9, 0),
			newColorLabelS("封禁列表", pal.text, true, 17)),
		container.New(layout.NewCustomPaddedLayout(0, 0, 6, 0), cntLbl))
	desc := newColorLabelS("全部 BT 任务共享;封禁中的 IP 无法向本程序发起任何连接。解除后该 IP 立即恢复可连。",
		pal.textDim, false, 12.5).SetMaxWidth()
	tbl := container.New(&vgapLayout{gap: 5})
	syncCount := func(n int) {
		d.bansCount = n
		if n > 0 {
			cntLbl.SetText(fmt.Sprintf("(%d)", n))
			bansBtn.SetText(fmt.Sprintf("封禁列表 (%d)", n))
		} else {
			cntLbl.SetText("")
			bansBtn.SetText("封禁列表")
		}
	}
	var refresh func()
	refresh = func() {
		go func() {
			bans, err := app.client.Bans()
			fyne.Do(func() {
				tbl.Objects = tbl.Objects[:0]
				if err != nil {
					syncCount(0)
					tbl.Objects = append(tbl.Objects,
						newColorLabelS("加载失败:"+err.Error(), pal.errc, false, 12.5))
					tbl.Refresh()
					return
				}
				syncCount(len(bans))
				if len(bans) == 0 {
					tbl.Objects = append(tbl.Objects, container.New(
						layout.NewCustomPaddedLayout(34, 34, 0, 0),
						container.NewCenter(newColorLabelS("暂无封禁记录", pal.textFaint, false, 12.5))))
					tbl.Refresh()
					return
				}
				tbl.Objects = append(tbl.Objects, bansHead(pal))
				for _, e := range bans {
					tbl.Objects = append(tbl.Objects, app.bansRowUI(pal, e, refresh))
				}
				tbl.Refresh()
			})
		}()
	}
	clearBtn := newGhostButton(pal, "清空全部", func() {
		app.store.ConfirmBox(ConfirmReq{Message: "将解除全部封禁记录,被封禁的 IP 立即恢复可连。确认清空?",
			OKLabel: "清空", OnOk: func() {
				go func() {
					if err := app.client.BansClear(); err == nil {
						fyne.Do(refresh)
					}
				}()
			}})
	})
	// 清空钮红字(=btn-ghost style color:var(--err))
	clearBtn.lbl.Color = pal.errc
	clearBtn.lbl.Refresh()
	closeBtn := newGhostButton(pal, "关闭", func() { hideTopModal() })
	actions := container.NewHBox(clearBtn, layout.NewSpacer(), closeBtn)
	body := container.NewBorder(
		container.NewVBox(head,
			container.New(layout.NewCustomPaddedLayout(8, 14, 0, 0), desc)),
		actions, nil, nil,
		vScrollNS(container.New(layout.NewCustomPaddedLayout(0, 8, 0, 4), tbl)))
	dg := modalCard(app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 18, 24, 24), body), 640, 460)
	dg.Show()
	refresh()
	// 关弹窗后回读一次计数(期间可能有后台封禁落表)
	go func() {
		for dg.Visible() {
			time.Sleep(500 * time.Millisecond)
		}
		refresh()
	}()
}

// bansHead 表头行(=bans-row.bans-head:透明无边框,text-faint 11px)
func bansHead(pal palette) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(0, 2, 10, 10),
		hFlex(10,
			flexW(newColorLabelS("IP", pal.textFaint, false, 11), 1),
			flexW(newColorLabelS("客户端", pal.textFaint, false, 11), 0.9),
			newColorLabelS("封禁时间", pal.textFaint, false, 11),
			flexW(newColorLabelS("原因", pal.textFaint, false, 11), 1.2),
			newColorLabelS("", pal.textFaint, false, 11)))
}

// bansRowUI 单条封禁行(=bans-row:panel 底+边框+radius-sm,padding 7px 10px)
func (app *App) bansRowUI(pal palette, e bt.BanEntry, afterDel func()) fyne.CanvasObject {
	ip := newColorLabelS(e.IP, pal.text, false, 12.5).SetMaxWidth()
	// 累计次数缀在 IP 后(=×N,dim;1 次不显示)
	ipCell := fyne.CanvasObject(ip)
	if e.Count > 1 {
		ipCell = hFlex(4, ip, newColorLabelS(fmt.Sprintf("×%d", e.Count), pal.textFaint, false, 12))
	}
	client := newColorLabelS(orDefault(e.Client, "—"), pal.textDim, false, 12.5).SetMaxWidth()
	timeL := newColorLabelS(fmtTime(e.Time), pal.textDim, false, 11.5)
	reason := newColorLabelS(orDefault(e.Reason, "—"), pal.textDim, false, 12.5).SetMaxWidth()
	// 解除封禁=改黑名单状态的敏感操作,红悬停图标钮(=.bans-del:hover 变红)
	del := newDangerIconButtonSZ(pal, "close", 24, 13, func() {
		go func() {
			if err := app.client.BanDelete(e.IP); err == nil {
				fyne.Do(func() { app.toast("已解除 "+e.IP, "ok") })
			}
			fyne.Do(afterDel)
		}()
	})
	bg := canvas.NewRectangle(pal.panel)
	bg.CornerRadius = 9
	stroke := canvas.NewRectangle(color.NRGBA{A: 0})
	stroke.CornerRadius = 9
	stroke.StrokeWidth = 1
	stroke.StrokeColor = pal.border
	return container.NewStack(bg, stroke,
		container.New(layout.NewCustomPaddedLayout(7, 7, 10, 10),
			hFlex(10,
				flexW(ipCell, 1),
				flexW(client, 0.9),
				newFixedMin(container.NewStack(timeL), fyne.NewSize(148, 0)),
				flexW(reason, 1.2),
				del)))
}

// openClientBL 客户端黑名单(=ClientBLDialog.vue:每行一条模式的增删列表,
// 打开聚焦首行,保存去重落库;列表区弹性撑满、动作行固定底部,计数回传按钮)
func (app *App) openClientBL(d *settingsDraft, blBtn *textBtn) {
	pal := app.store.appPal()
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("客户端黑名单", pal.text, true, 17))
	hint := hintText(pal, "每行一条模式:星号 * 匹配任意长度、问号 ? 匹配单个字符,大小写不敏感(如 *Xunlei*、*Thunder*)。命中即写入全局封禁名单并断开连接,保存后立即生效")
	rowsBox := container.New(&vgapLayout{gap: 8})
	type rowRef struct {
		row   fyne.CanvasObject
		entry *widget.Entry
	}
	var rows []*rowRef
	addRow := func(v string, focus bool) {
		e := widget.NewEntry()
		e.SetText(v)
		e.SetPlaceHolder("*Xunlei*")
		singleLine(e)
		e.TextStyle = fyne.TextStyle{Monospace: true} // 模式是匹配串,等宽(=bl-row .mono)
		row := container.New(&hflexLayout{gap: 8})
		del := newGhostButton(pal, "删除", func() {
			for i, r := range rows {
				if r.row == row {
					rows = append(rows[:i], rows[i+1:]...)
					break
				}
			}
			for i, o := range rowsBox.Objects {
				if o == row {
					rowsBox.Objects = append(rowsBox.Objects[:i], rowsBox.Objects[i+1:]...)
					break
				}
			}
			rowsBox.Refresh()
		})
		del.SetMini()
		row.Add(flex(e))
		row.Add(del)
		rows = append(rows, &rowRef{row, e})
		rowsBox.Add(row)
		rowsBox.Refresh()
		if focus {
			app.win.Canvas().Focus(e) // =Vue 打开/新增后 focus 输入框
		}
	}
	if len(d.clientBL) == 0 {
		addRow("", false)
	} else {
		for _, p := range d.clientBL {
			addRow(p, false)
		}
	}
	var pop *maskModal
	save := newPrimaryButton(pal, "保存", func() {
		seen := map[string]bool{}
		patterns := make([]string, 0, len(rows))
		for _, r := range rows {
			t := strings.TrimSpace(r.entry.Text)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			patterns = append(patterns, t)
		}
		go func() {
			err := app.client.ClientBlacklist(patterns)
			fyne.Do(func() {
				if err != nil {
					app.toast(err.Error(), "err")
					return
				}
				d.clientBL = patterns
				blBtn.SetText(fmt.Sprintf("管理(%d 条)…", len(patterns)))
				if n := len(patterns); n > 0 {
					app.toast(fmt.Sprintf("客户端黑名单已保存(%d 条)", n), "ok")
				} else {
					app.toast("客户端黑名单已清空", "ok")
				}
				pop.Hide()
			})
		}()
	})
	addBtn := newGhostButton(pal, "+ 添加模式", func() { addRow("", true) })
	addBtn.SetMini()
	cancel := newGhostButton(pal, "取消", func() { pop.Hide() })
	actions := hGap(10, cancel, save)
	body := container.NewBorder(
		container.NewVBox(head,
			container.New(layout.NewCustomPaddedLayout(8, 14, 0, 0), hint)),
		container.NewVBox(
			container.New(layout.NewCustomPaddedLayout(10, 0, 0, 0), addBtn),
			container.New(layout.NewCustomPaddedLayout(14, 0, 0, 0),
				container.NewBorder(nil, nil, nil, actions))),
		nil, nil,
		vScrollNS(container.New(layout.NewCustomPaddedLayout(0, 4, 0, 4), rowsBox)))
	dg := modalCard(app.win, pal,
		container.New(layout.NewCustomPaddedLayout(22, 18, 24, 24), body), 560, 460)
	pop = dg
	dg.Show()
}
