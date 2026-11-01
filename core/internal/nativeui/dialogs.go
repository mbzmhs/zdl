package nativeui

// BT 创建对话框(BTDialog.vue 移植)与 choosing 文件选择窗(ChooseDialog.vue
// 移植)。核心交互逐条对齐:
//   - 磁链 600ms 防抖解析(格式/哈希校验,元数据未取前不含文件清单)
//   - .torrent 走原生文件选择对话框(读字节转 base64)——浏览器形态做不到
//     的本地文件直读,原生壳天然具备
//   - choosing 产物种子路径预填(torrentPath),创建成功后删除挂起任务
//   - 同 InfoHash 重复任务不静默建,确认后再建
//   - 填充文件隐藏但始终选中(边界块校验需要其字节)

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"zdownloader/internal/engine"
)

// openBTDialog BT 创建窗。target 三态:磁力链接 / .torrent 本机路径 / 空;
// dir 预填保存目录(choosing 产物);fromTaskID 非空 = 来源挂起任务,创建
// 成功后删除(=Vue prefillTaskId)。
func (app *App) openBTDialog(target, dirPrefill, fromTaskID string) {
	// ---- 状态(对话框作用域内,=BTDialog.vue 各 ref)----
	pal := app.store.appPal()
	var torrentB64, torrentPathRef string
	var prefillTaskID = fromTaskID
	var parsedInfoHash string
	var parseSeq int // 解析序号:过期响应作废(防抖竞态)
	var tree *ftTree
	var statLbl *widget.Label
	// 前置声明:submit 闭包(定义在后)要在创建成功后关窗、renderFileSel 要在
	// 文件树就绪后重算高度,均须能按引用捕获 pop / fit
	var pop *maskModal
	var fit func()
	// fileSec 必须以真实容器初始化:typed-nil *fyne.Container 混入 VBox
	// 会在布局期 Visible() 空指针崩溃(实测);解析未就绪时它就是空盒子。
	// 用 VBox 纵向堆叠(标题行/文件树/统计行):HBox 会把三者当三列横排,
	// 并把按钮拉伸成竖条、MinSize 宽度累加撑爆对话框(实测多文件场景)。
	fileSec := container.NewVBox()

	probeLbl := widget.NewLabel("")
	probeLbl.Wrapping = fyne.TextWrapWord
	probeLbl.Hide()
	setProbe := func(ok bool, text string) {
		probeLbl.TextStyle = fyne.TextStyle{}
		probeLbl.Importance = widget.MediumImportance
		if ok {
			probeLbl.Importance = widget.SuccessImportance
		} else {
			probeLbl.Importance = widget.DangerImportance
		}
		probeLbl.SetText(text)
		probeLbl.Show()
	}
	hideProbe := func() { probeLbl.Hide() }

	magnet := widget.NewMultiLineEntry()
	magnet.Wrapping = fyne.TextWrapWord // 多行回绕,不出横向滚动条(=textarea)
	magnet.SetPlaceHolder("magnet:?xt=urn:btih:…")
	torrentName := widget.NewEntry()
	torrentName.SetPlaceHolder("未选择文件")
	singleLine(torrentName) // 横向裁剪、无纵向滚动条(=input)
	upLimit := widget.NewEntry()
	upLimit.SetText("0")
	singleLine(upLimit)
	upLimitUnit := newZSelect(pal, []string{"KB/s", "MB/s"}, "KB/s")
	seedPolicy := newZSelect(pal, []string{"跟随全局", "按分享率", "按时长", "永久做种"}, "跟随全局")
	seedRatio := widget.NewEntry()
	seedRatio.SetText("1")
	singleLine(seedRatio)
	seedMinutes := widget.NewEntry()
	seedMinutes.SetText("60")
	singleLine(seedMinutes)
	seedExtra := container.NewHBox()
	btChoose := newStyledCheck(app.store.appPal(), "磁力任务获取到元数据后弹出文件选择窗口(仅多文件种子)", true, nil)

	// 目录用 dirField(=Vue BTDialog 的 DirField):可自由输入/粘贴 + ▾ 选最近目录
	dirEntry := NewDirField(app, "默认下载目录", dirPrefill)
	// 统一走自定义风格钮(=Vue btn-ghost);Fyne 默认 widget.NewButton 观感
	// 突兀(=用户报的"部分按钮用了默认样式")
	browse := newGhostButton(pal, "浏览…", func() {
		app.pickFolderAsync("选择下载文件夹", func(p string, ok bool) {
			if !ok {
				return
			}
			fyne.Do(func() { dirEntry.SetText(p) })
		})
	})

	renderFileSel := func(files []BTParseFile) {
		if len(files) < 2 {
			fileSec.Objects = nil
			fileSec.Refresh()
			tree = nil
			if fit != nil {
				fit() // 单文件/未解析:收回单文件高度
			}
			return
		}
		fts := make([]*ftFile, 0, len(files))
		for i := range files {
			f := &files[i]
			fts = append(fts, &ftFile{Path: f.Path, Size: f.Length, First: f.First, Last: f.Last, Wanted: true})
		}
		// 填充文件不显示但始终选中(=parsedFull/selectedFiles 语义)
		visible := make([]*ftFile, 0, len(fts))
		for _, f := range fts {
			if !FtIsPadding(f) {
				visible = append(visible, f)
			}
		}
		tree = newFtTree(app, visible, func() {
			n, total, bytes, all := tree.SelStat()
			statLbl.SetText(fmt.Sprintf("已选 %d/%d 个文件 · %s", n, total, fmtBytes(bytes)) + map[bool]string{true: "(全部)", false: ""}[all])
		})
		// 多文件:按文件数适度抬高树的最小高度(每多一个 +30px,上限 520),
		// 让文件列表一次展示更多条目,而不是全挤进 300px 靠内部滚动
		if n := len(visible); n > 8 {
			tree.minTreeH = 300 + float32(n-8)*30
			if tree.minTreeH > 520 {
				tree.minTreeH = 520
			}
		}
		_ = fts
		// 文件区小钮走迷你幽灵钮(=btn-ghost btn-sm 观感)
		allBtn := newGhostButton(pal, "全选", func() { tree.SetAll(true) }).SetMini()
		noneBtn := newGhostButton(pal, "全不选", func() { tree.SetAll(false) }).SetMini()
		statLbl = widget.NewLabel("")
		fileSec.Objects = []fyne.CanvasObject{
			container.NewHBox(widget.NewLabelWithStyle("选择下载文件(不选 = 单文件种子)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				layout.NewSpacer(), allBtn, noneBtn),
			tree, statLbl,
		}
		fileSec.Refresh()
		tree.onChange()
		if fit != nil {
			fit() // 文件树就绪后重算高度:多文件时加高、单文件时收回
		}
	}

	// 防抖解析磁链(=onMagnetInput:600ms;输入即清种子预填)
	var debounce *time.Timer
	magnet.OnChanged = func(v string) {
		// 手输磁力:清除预填种子与挂起来源(=onMagnetInput)
		torrentPathRef = ""
		prefillTaskID = ""
		if debounce != nil {
			debounce.Stop()
		}
		parseSeq++
		seq := parseSeq
		if strings.TrimSpace(v) == "" {
			hideProbe()
			return
		}
		debounce = time.AfterFunc(600*time.Millisecond, func() {
			r, err := app.client.BTParse(strings.TrimSpace(v), "", "")
			fyne.Do(func() {
				if seq != parseSeq {
					return // 期间已改道
				}
				if err != nil {
					setProbe(false, "✗ "+err.Error())
					return
				}
				parsedInfoHash = r.InfoHash
				renderFileSel(nil)
				torrentName.Text = "" // 手输磁力:清种子名
				torrentName.Refresh()
				parts := []string{"✓ InfoHash " + shortHash(r.InfoHash)}
				if r.Name != "" {
					parts = append(parts, r.Name)
				}
				if r.Size > 0 {
					parts = append(parts, fmtBytes(r.Size))
				}
				parts = append(parts, fmt.Sprintf("%d 个 tracker", len(r.Trackers)))
				setProbe(true, strings.Join(parts, " · "))
			})
		})
	}

	pickTorrent := newGhostButton(pal, "选择文件…", func() {
		// 原生文件选择在工作线程弹(线程规则见 app.go);回调里解析磁链产物,
		// 状态回写经 fyne.Do(=BTDialog onTorrentFile 语义)
		app.pickFileAsync("选择种子文件", "种子文件", "*.torrent", func(data []byte, path string, ok bool) {
			if !ok {
				return
			}
			b64 := base64.StdEncoding.EncodeToString(data)
			r, perr := app.client.BTParse("", b64, "")
			fyne.Do(func() {
				if perr != nil {
					setProbe(false, "✗ "+perr.Error())
					return
				}
				torrentB64 = b64
				torrentPathRef = ""
				prefillTaskID = ""
				parsedInfoHash = r.InfoHash
				renderFileSel(r.Files)
				magnet.Text = ""
				magnet.Refresh()
				torrentName.SetText(path)
				hideProbe()
				setProbe(true, fmt.Sprintf("✓ %s · %s · %d 个文件 · %d 个 tracker", r.Name,
					map[bool]string{true: fmtBytes(r.Size), false: "大小未知"}[r.Size > 0], len(r.Files), len(r.Trackers)))
			})
		})
	})

	// torrentPath 预填(=openBTPrefilled:choosing 产物/关联启动种子)
	if target != "" && !strings.HasPrefix(strings.ToLower(target), "magnet:") {
		path := target
		torrentPathRef = path
		prefillTaskID = fromTaskID
		torrentName.SetText(path)
		go func() {
			r, err := app.client.BTParse("", "", path)
			fyne.Do(func() {
				if torrentPathRef != path {
					return // 期间用户已改道
				}
				if err != nil {
					setProbe(false, "✗ "+err.Error())
					return
				}
				parsedInfoHash = r.InfoHash
				renderFileSel(r.Files)
				setProbe(true, fmt.Sprintf("✓ %s · %s · %d 个文件 · %d 个 tracker", r.Name,
					map[bool]string{true: fmtBytes(r.Size), false: "大小未知"}[r.Size > 0], len(r.Files), len(r.Trackers)))
			})
		}()
	} else if target != "" {
		magnet.SetText(target) // 触发 OnChanged 走防抖解析
	}

	submit := func() {
		dir := strings.TrimSpace(dirEntry.Text())
		body := engine.CreateReq{Dir: dir}
		// 上传限速换算(界面 KB/s|MB/s → 字节/秒)
		uv := parseFloat(upLimit.Text)
		if upLimitUnit.Selected == "MB/s" {
			uv *= 1048576
		} else {
			uv *= 1024
		}
		body.UPLimit = int64(uv)
		switch seedPolicy.Selected {
		case "按分享率":
			body.SeedRatio = parseFloat(seedRatio.Text)
		case "按时长":
			body.SeedMinutes = int(parseFloat(seedMinutes.Text))
		case "永久做种":
			body.SeedRatio = 0
			body.SeedMinutes = 0
		}
		var sel []string
		if tree != nil {
			sel = tree.SelectedPaths()
		}
		switch {
		case torrentPathRef != "":
			body.TorrentPath = torrentPathRef
			body.TorrentGenerated = false
			if sel != nil {
				body.Files = sel
			}
		case torrentB64 != "":
			body.TorrentB64 = torrentB64
			if sel != nil {
				body.Files = sel
			}
		default:
			m := strings.TrimSpace(magnet.Text)
			if m == "" {
				app.toast("请输入磁力链接或选择种子文件", "err")
				return
			}
			body.Magnet = m
			body.Choose = btChoose.IsChecked()
		}
		// 同 InfoHash 已有任务:确认后再建(=submit 的 dup 检查)
		if parsedInfoHash != "" {
			for _, t := range app.store.Tasks() {
				if t.Kind == "bt" && t.InfoHash != "" && strings.EqualFold(t.InfoHash, parsedInfoHash) {
					name := t.Filename
					if name == "" {
						name = shortHash(parsedInfoHash)
					}
					app.store.ConfirmBox(ConfirmReq{
						Title:   "同名任务已存在",
						Message: fmt.Sprintf("「%s」已存在任务(%s),再建将产生重复。是否仍要创建?", name, StatusText(t.Status)),
						OKLabel: "仍要创建",
						// 确认才关 BT 窗再创建;取消则留窗供用户继续调整
						OnOk: func() {
							pop.Hide()
							app.doBTCreate(body, dir, prefillTaskID)
						},
					})
					return
				}
			}
		}
		// 无重复:提交即关窗(异步创建继续,成功/错误走 toast,=新建任务窗语义)
		app.doBTCreate(body, dir, prefillTaskID)
		pop.Hide()
	}

	// 布局放松:组间用 vgap 14(原整块 VBox 仅 4px,过于紧凑);磁力链接并入
	// fieldRow 使标签样式(淡色/粗体/12.5)与其他字段一致,解析状态行紧跟其后
	magnetGroup := container.NewVBox(app.fieldRow("磁力链接", magnet), probeLbl)
	content := container.New(&vgapLayout{gap: 14},
		magnetGroup,
		container.NewBorder(nil, nil, nil, pickTorrent, torrentName),
		container.NewBorder(nil, nil, nil, browse, dirEntry),
		container.New(layout.NewGridLayoutWithColumns(2),
			app.fieldRow("上传限速(0 不限)",
				container.NewBorder(nil, nil, nil, upLimitUnit, upLimit)),
			app.fieldRow("做种策略",
				container.NewHBox(seedPolicy, seedExtra))),
		btChoose,
		fileSec,
	)
	// 分享率/时长字段按策略显隐(v1:勾选策略时即时追加输入框)
	seedPolicy.OnChanged = func(v string) {
		seedExtra.Objects = nil
		switch v {
		case "按分享率":
			seedExtra.Objects = []fyne.CanvasObject{widget.NewLabel("目标分享率"), seedRatio}
		case "按时长":
			seedExtra.Objects = []fyne.CanvasObject{widget.NewLabel("做种时长(分钟)"), seedMinutes}
		}
		seedExtra.Refresh()
		if fit != nil {
			fit() // 字段增删改变内容高,重算卡体贴高
		}
	}

	// =dialog 基类外壳:accent-bar 标题 + 内容滚动区 + 右下动作行
	bar := newRoundedGrad(pal.accent, pal.accent2, 3)
	head := hGap(9, newFixedMin(container.NewStack(bar), fyne.NewSize(4, 17)),
		newColorLabelS("新建磁力 / 种子任务", pal.text, true, 17))
	actions := hGap(10, newGhostButton(pal, "取消", func() { pop.Hide() }),
		newPrimaryButton(pal, "创建任务", submit))
	// 标题固定在外、不进滚动区(=用户报的"标题被卷入滚动范围"):卡体 = 标题
	// (顶部,含 18 下边距) + 下方滚动(内容+动作)。标题下 18px 放固定标题块
	// 而非滚动内容顶内边距,否则滚动时随内容滚走、下边缘顶到标题(=本轮反馈)
	scrollBody := container.New(&vgapLayout{gap: 14},
		content, container.NewBorder(nil, nil, nil, actions))
	headWrap := container.New(layout.NewCustomPaddedLayout(26, 18, 30, 28), head)
	// 高度自适应+上限(同新建任务窗):贴内容,超高转内部滚动
	// (fyne 滚动容器 min 高只有 32,须 SetMinSize 喂给 fixedMin 卡体)
	sc := container.NewVScroll(container.New(
		layout.NewCustomPaddedLayout(0, 24, 30, 28), scrollBody))
	inner := container.NewVBox(headWrap, sc)
	pop = modalCard(app.win, pal, inner, 680, 0)
	fit = func() {
		h := sc.Content.MinSize().Height
		// 多文件时上限放宽(820),让文件列表更完整;单文件维持 620;
		// 且绝不超出可视高度(留 40 余量)。标题固定在外,扣掉后留给滚动内容
		maxH := float32(620)
		if tree != nil {
			maxH = 820
		}
		if avail := app.win.Canvas().Size().Height - 40; avail < maxH {
			maxH = avail
		}
		if head := headWrap.MinSize().Height; maxH > head {
			maxH -= head
		}
		if h > maxH {
			h = maxH
		}
		sc.SetMinSize(fyne.NewSize(0, h))
		pop.Refresh()
	}
	fit()
	pop.Show()
}

// doBTCreate 提交创建(=doCreate:错误 toast、recentDir 回写、挂起任务清理)
func (app *App) doBTCreate(body engine.CreateReq, dir, prefillTaskID string) {
	go func() {
		_, errs, err := app.client.CreateBT(body)
		fyne.Do(func() {
			if err != nil {
				app.toast(err.Error(), "err")
				return
			}
			if len(errs) > 0 {
				app.toast(errs[0], "err")
				return
			}
			if strings.TrimSpace(dir) != "" {
				app.store.RecordRecentDir(strings.TrimSpace(dir))
			}
			if prefillTaskID != "" {
				// 来源挂起任务使命完成:种子已固化到新任务,移除记录
				go func() {
					if derr := app.client.Delete(prefillTaskID, false); derr == nil {
						fyne.Do(func() { app.toast("已从种子创建下载任务,原挂起任务已移除", "ok") })
					} else {
						fyne.Do(func() { app.toast("BT 任务已创建(原挂起任务移除失败,可手动删除)", "ok") })
					}
					fyne.Do(func() { app.store.RefreshTasks() })
				}()
			} else {
				app.toast("BT 任务已创建", "ok")
			}
			app.store.RefreshTasks()
			app.store.SwitchView("active")
		})
	}()
}

/* ---------------- Choose 对话框(=ChooseDialog.vue) ---------------- */

// openChooseDialog choosing 任务文件选择窗
func (app *App) openChooseDialog(id string) {
	go func() {
		d, err := app.client.Detail(id)
		fyne.Do(func() {
			if err != nil {
				app.toast(err.Error(), "err")
				return
			}
			files := make([]*ftFile, 0, len(d.Files))
			total := int64(0)
			for i := range d.Files {
				f := &d.Files[i]
				if FtIsPadding(&ftFile{Path: f.Path}) {
					continue
				}
				files = append(files, &ftFile{Path: f.Path, Size: f.Size, Done: f.Done, First: f.First, Last: f.Last, Wanted: f.Wanted})
				total += f.Size
			}
			if len(files) == 0 {
				app.toast("元数据尚未就绪,稍后再试", "err")
				return
			}
			info := fmt.Sprintf("%s · %d 个文件 · 共 %s", orDash(d.Filename), len(files), fmtBytes(total))
			tree := newFtTree(app, files, nil)
			stat := widget.NewLabel("")
			updateStat := func() {
				n, tot, bytes, all := tree.SelStat()
				stat.SetText(fmt.Sprintf("已选 %d/%d 个文件 · %s", n, tot, fmtBytes(bytes)) + map[bool]string{true: "(全部)", false: ""}[all])
			}
			tree.onChange = updateStat
			updateStat()

			submit := func(all bool, sel []string) {
				go func() {
					var serr error
					if all {
						serr = app.client.SetTaskFiles(id, true, nil)
					} else {
						serr = app.client.SetTaskFiles(id, false, sel)
					}
					fyne.Do(func() {
						if serr != nil {
							app.toast(serr.Error(), "err")
							return
						}
						t, _ := app.store.Task(id)
						if t.Status == engineStatusPaused {
							app.toast("文件选择已保存,点「继续」开始下载", "ok")
						} else {
							app.toast("已开始下载所选文件", "ok")
						}
						app.store.RefreshTasks()
					})
				}()
			}
			selNow := func() (bool, []string) { return tree.SelectedPaths() == nil, tree.SelectedPaths() }

			// 三键语义对齐 ChooseDialog:稍后选择 / 全部下载 / 开始下载
			var pop *maskModal
			pal := app.store.appPal()
			body := container.NewVBox(
				widget.NewLabel(info),
				tree,
				stat,
				container.NewHBox(layout.NewSpacer(),
					newGhostButton(pal, "稍后选择", func() { pop.Hide() }),
					newGhostButton(pal, "全部下载", func() { pop.Hide(); submit(true, nil) }),
					newPrimaryButton(pal, "开始下载", func() {
						all, sel := selNow()
						pop.Hide()
						submit(all, sel)
					})))
			inner := container.New(layout.NewCustomPaddedLayout(22, 22, 24, 24),
				vScrollNS(body))
			pop = modalCard(app.win, pal, inner, 680, 560)
			pop.Show()
		})
	}()
}

/* ---------------- 小助手 ---------------- */

func dlgHide(d interface{ Hide() }) { d.Hide() }

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}

func orDash(s string) string {
	if s == "" {
		return "(未命名)"
	}
	return s
}
