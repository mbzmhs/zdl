package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/proxy"
	"zdownloader/internal/util"
)

// AddBT 创建 BT 任务(磁力链接或 .torrent)。种子内容立即解析并归档;
// 磁力任务先入队,元数据在运行时经 tracker peer 获取(BEP-9)。
func (m *Manager) AddBT(req CreateReq) ([]TaskView, []string) {
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = m.cfg.DownloadDir
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(m.cfg.DownloadDir, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, []string{"无法创建下载目录: " + err.Error()}
	}

	var raw []byte
	if p := strings.TrimSpace(req.TorrentPath); p != "" {
		// 本机 .torrent 路径(磁链 choosing 产物预填创建对话框后提交)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, []string{"种子文件读取失败: " + err.Error()}
		}
		raw = b
	} else if b64 := strings.TrimSpace(req.TorrentB64); b64 != "" {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, []string{"种子文件解码失败: " + err.Error()}
		}
		raw = b
	}
	if raw != nil {
		meta, err := bt.ParseTorrent(raw)
		if err != nil {
			return nil, []string{"种子解析失败: " + err.Error()}
		}
		t := m.newBTTask(req, dir)
		// 关联启动会带 Choose/ChooseAny:种内容已就绪,挂起点在 runBT
		// 的 btHoldForChoose(UI 对话框创建带 Files,不会挂起)
		t.ChoosePending = req.Choose
		t.HoldAny = req.ChooseAny
		// 可见种子副本落点:来源是隐藏任务目录里我们自己生成的归档(磁链
		// choosing 产物)时,把它复制进用户选定的下载目录,种子随数据一起走;
		// 来源是外部本机文件(双击 / 路径创建)时直接引用原文件,不再复制
		// (下载目录与种子同目录时会得到"同名(1)"副本,实测)。
		t.TorrentFile = m.placeVisibleTorrent(req, dir, meta, raw)
		if err := m.archiveTorrent(t, raw); err != nil {
			return nil, []string{"种子归档失败: " + err.Error()}
		}
		m.fillBTTaskFromMeta(t, meta)
		if len(req.Files) > 0 {
			// 所选文件逐一对照种子清单再落账:路径对不上的选择在
			// WantedPiecesFor 里会无声息回退成全量下载(实测表现为"没选的
			// 文件也在下、任务大小比所选大"),必须在创建时就拒绝。
			if _, err := meta.FileWantedFor(req.Files); err != nil {
				return nil, []string{err.Error()}
			}
			t.mu.Lock()
			t.WantedFiles = req.Files
			t.mu.Unlock()
			// 按选择重算大小/进度口径
			wanted := meta.WantedPiecesFor(req.Files)
			t.mu.Lock()
			t.Size = selectedBytes(meta, req.Files)
			t.PiecesTotal = countTrue(wanted)
			t.mu.Unlock()
		}
		m.deleteTorrentAfterCreate(t, req)
		m.registerBTTask(t)
		return []TaskView{t.View()}, nil
	}

	magnet := strings.TrimSpace(req.Magnet)
	if magnet == "" {
		return nil, []string{"缺少磁力链接或种子文件"}
	}
	mg, err := bt.ParseMagnet(magnet)
	if err != nil {
		return nil, []string{err.Error()}
	}
	t := m.newBTTask(req, dir)
	t.URL = magnet
	t.InfoHash = fmt.Sprintf("%x", mg.InfoHash)
	t.Trackers = append(t.Trackers, mg.Trackers...)
	t.ChoosePending = req.Choose
	t.HoldAny = req.ChooseAny
	if specName := strings.TrimSpace(req.Filename); specName != "" {
		// 用户指定名以 .zdlmeta 占位,元数据到手后去掉后缀
		t.Filename = util.SanitizeFilename(specName) + ".zdlmeta"
	}
	m.registerBTTask(t)
	return []TaskView{t.View()}, nil
}

func (m *Manager) newBTTask(req CreateReq, dir string) *Task {
	return &Task{
		ID:          util.RandomID(),
		Kind:        KindBT,
		Dir:         dir,
		Size:        -1,
		Connections: 0, // BT 连接数 = 当前 peer 数,运行中由 btPump 实时刷新
		Status:      StatusWaiting,
		CreatedAt:   time.Now(),
		SpeedLimit:  req.SpeedLimit,
		Source:      req.Source,
		UPLimit:     req.UPLimit,
		SeedRatio:   req.SeedRatio,
		SeedMinutes: req.SeedMinutes,
		Trackers:    append([]string(nil), req.Trackers...),
	}
}

func (m *Manager) registerBTTask(t *Task) {
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.mu.Unlock()
	m.persist(t)
	m.bump()
	m.kick()
	log.Printf("创建 BT 任务 %s -> %s", shortBTSource(t), t.Dir)
}

// archiveTorrent 把 .torrent 原文存到任务目录,重启后据此恢复(含做种)。
func (m *Manager) archiveTorrent(t *Task, raw []byte) error {
	path := filepath.Join(m.tasksDir, t.ID+".torrent")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	t.mu.Lock()
	t.TorrentPath = path
	t.mu.Unlock()
	return nil
}

// isGeneratedTorrent 判定创建来源是否为"本程序生成的种子":磁链抓取、b64 解析、
// HTTP 下载转建(显式标记)、或归档路径落在本程序任务目录下的磁链 choosing 产物。
// 其余(双击/手动选路径的源文件)视为用户种子,走单独的删除开关。
func isGeneratedTorrent(req CreateReq, tasksDir string) bool {
	if req.Magnet != "" || req.TorrentB64 != "" || req.TorrentGenerated {
		return true
	}
	if p := strings.TrimSpace(req.TorrentPath); p != "" {
		base := filepath.Clean(tasksDir) + string(filepath.Separator)
		if strings.HasPrefix(filepath.Clean(p), base) {
			return true
		}
	}
	return false
}

// deleteTorrentAfterCreate 依"创建后删除种子"开关删除任务的可见种子副本(源
// 文件 / 磁链 choosing 的下载目录副本),保留内部元数据存档(TorrentPath)——
// 存档是运行/续传/做种的元数据来源,详情页可随时「另存为种子」取回。开关选择
// 由 isGeneratedTorrent 分流:生成种子走 DeleteGeneratedTorrent,用户源文件走
// DeleteSourceTorrent。删后清空 TorrentFile,重启不再重复尝试。
func (m *Manager) deleteTorrentAfterCreate(t *Task, req CreateReq) {
	t.mu.Lock()
	tf, tp := t.TorrentFile, t.TorrentPath
	t.mu.Unlock()
	if tf == "" || tf == tp {
		return // 无可见种子,或可见副本即内部存档(纯磁链)
	}
	var on bool
	if isGeneratedTorrent(req, m.tasksDir) {
		on = m.cfg.BT.DeleteGeneratedTorrent
	} else {
		on = m.cfg.BT.DeleteSourceTorrent
	}
	if !on {
		return
	}
	if _, err := os.Stat(tf); err != nil {
		return // 源文件已不在,无需删
	}
	if err := os.Remove(tf); err != nil {
		log.Printf("创建 BT 任务后删除种子文件 %s 失败: %v", tf, err)
		return
	}
	t.mu.Lock()
	t.TorrentFile = ""
	t.mu.Unlock()
	log.Printf("创建 BT 任务后删除种子文件 %s", tf)
}

// ExportTorrent 导出任务的种子文件(内部元数据存档)。默认文件名=任务名。
// 任务或存档缺失时返回错误,由服务端转为 400 提示。
func (m *Manager) ExportTorrent(id string) (filename string, data []byte, err error) {
	m.mu.Lock()
	t := m.tasks[id]
	var tp, name string
	if t != nil {
		tp, name = t.TorrentPath, t.Filename
	}
	m.mu.Unlock()
	if t == nil {
		return "", nil, ErrNotFound
	}
	if tp == "" {
		return "", nil, fmt.Errorf("种子尚未就绪(元数据未获取)")
	}
	data, err = os.ReadFile(tp)
	if err != nil {
		return "", nil, fmt.Errorf("读取种子失败: %w", err)
	}
	return util.SanitizeFilename(name + ".torrent"), data, nil
}

// placeVisibleTorrent 决定新任务"可见种子副本"落在哪里,返回该路径:
//   - 来源是隐藏任务目录里我们自己生成的归档(磁链 choosing 产物):把它复制
//     进用户选定的下载目录,与数据放一起(用户在创建对话框里确认了目录,种子
//     随数据走)。隐藏归档本身保留(作重启/做种的持久种子)。
//   - 来源是本机任意其它 .torrent(双击 / 路径创建):原文件本身就是可见副本,
//     直接引用,不再复制(下载目录与种子同目录时会得到"同名(1)"副本)。
//
// 写失败时回退为引用原路径,不阻断任务创建。
func (m *Manager) placeVisibleTorrent(req CreateReq, dir string, meta *bt.MetaInfo, raw []byte) string {
	src := strings.TrimSpace(req.TorrentPath)
	if src == "" {
		return "" // 磁力/种子字节来源:无本机路径可作可见副本
	}
	tdClean := filepath.Clean(m.tasksDir)
	srcClean := filepath.Clean(src)
	if !strings.HasPrefix(srcClean, tdClean+string(filepath.Separator)) {
		return src // 非本应用生成的隐藏归档:原文件即副本,直接引用
	}
	if meta == nil || meta.Name == "" {
		return src // 无名兜底:直接引用隐藏归档
	}
	name := util.SanitizeFilename(meta.Name) + ".torrent"
	cp := util.UniquePath(dir, name)
	if werr := os.WriteFile(cp, raw, 0o644); werr != nil {
		log.Printf("种子副本写入 %s 失败(%v),回退引用隐藏归档", cp, werr)
		return src
	}
	return cp
}

// fillBTTaskFromMeta 用解析出的元数据回填任务的展示字段。
func (m *Manager) fillBTTaskFromMeta(t *Task, meta *bt.MetaInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.InfoHash = fmt.Sprintf("%x", meta.InfoHash)
	t.Filename = strings.TrimSuffix(t.Filename, ".zdlmeta")
	if t.Filename == "" {
		t.Filename = meta.Name
	}
	t.Size = meta.TotalLength
	t.PiecesTotal = meta.PieceCount()
	if len(t.WantedFiles) > 0 { // 文件选择:大小与进度按所选口径
		if wanted := meta.WantedPiecesFor(t.WantedFiles); wanted != nil {
			t.Size = selectedBytes(meta, t.WantedFiles)
			t.PiecesTotal = countTrue(wanted)
		}
	}
	t.AcceptRanges = true // BT 恒可续传
	t.FileCount = len(meta.Files)
	// 单文件视频种子:任务列表文件名可作边下边播快速入口(前端据 streamable
	// 渲染点击;多文件种子标题点击进详情文件页)。文件被改选排除时不开入口。
	t.Streamable = !meta.Multi && len(meta.Files) == 1 &&
		btMediaExts[strings.ToLower(filepath.Ext(meta.Name))] &&
		(len(t.WantedFiles) == 0 || btNormPath(t.WantedFiles[0]) == meta.Name)
	// 种子内置 tracker 并入任务列表(详情展示与重启用)
	has := map[string]bool{}
	for _, u := range t.Trackers {
		has[u] = true
	}
	add := func(u string) {
		if u != "" && !has[u] {
			has[u] = true
			t.Trackers = append(t.Trackers, u)
		}
	}
	add(meta.Announce)
	for _, tier := range meta.AnnounceList {
		for _, u := range tier {
			add(u)
		}
	}
}

func shortBTSource(t *Task) string {
	if strings.HasPrefix(t.URL, "magnet:") {
		if len(t.URL) > 72 {
			return t.URL[:72] + "…"
		}
		return t.URL
	}
	return "torrent:" + t.InfoHash
}

// ---------- 运行 ----------

// runBT 执行一个 BT 任务:取得元数据 → 下载 → 校验落盘 → 按策略进入做种。
// 做种不占用调度槽位,由独立的做种协程接管。
func (m *Manager) runBT(ctx context.Context, t *Task) error {
	seeding := false // 移交做种后由做种协程收尾
	meta, raw, err := m.btEnsureMeta(ctx, t)
	if err != nil {
		return err
	}
	if m.btHoldForChoose(t, meta, raw) {
		return nil // 挂起等用户创建任务;调度槽已释放,确认后重新排队
	}
	trackers := m.btTrackersFor(t, meta, m.autoAddPublicTrackers())
	t.mu.Lock()
	wantedFiles := t.WantedFiles
	origName := t.Filename
	t.mu.Unlock()
	if origName == "" {
		origName = meta.Name
	}
	wanted := meta.WantedPiecesFor(wantedFiles)
	// 展示口径的文件级需求(按路径精确匹配;见 bt.ClientConfig.FileWanted)。
	// 匹配失败(老任务/种子被替换)仅告警并回退 piece 推算,不阻断会话。
	fileWanted, fwErr := meta.FileWantedFor(wantedFiles)
	if fwErr != nil {
		log.Printf("BT 任务 %s 文件选择与种子不符(%v),展示口径回退按 piece 推算", t.ID, fwErr)
	}
	initialHave := btDecodeBitfield(t.Bitfield, meta.PieceCount())
	// allHave:位图全真 = 每个 piece 都曾校验通过(含文件选择排除项)。
	// complete:位图覆盖全部需求 piece,即可走"最终文件存在 → 直接做种恢复"
	// 的捷径。文件选择任务只对外声明需求内的 piece,排除文件不在盘上也
	// 照样做种——但需求位图必须全部已持有,否则老老实实走下载路径。
	// (实测:子集任务被 complete=require(nil) 排除在捷径外,重启后目录被
	// 冲突改名 "(1)",已完成的 457MB 无视,整份重下。)
	allHave := len(initialHave) == meta.PieceCount() && allTrue(initialHave)
	complete := wanted == nil && allHave
	if wanted != nil && len(initialHave) == meta.PieceCount() {
		complete = true
		for i, w := range wanted {
			if w && !initialHave[i] {
				complete = false
				break
			}
		}
	}

	var cl *bt.Client
	// 已完成任务的做种恢复:必须先用【原名】找最终文件,绝不能先做冲突改名。
	// 旧实现先 UniquePath:原文件存在 → 名字被改成 "xxx (1)" → 再到新名字下
	// 找文件必然缺失 → 判定未完成 → 用全新临时文件从 0 重下(实测:100% 的
	// 做种任务重启后文件变 "(1)" 并从零重下,下载目录堆满 "(1) (1)…" 副本)。
	if complete {
		cfg := m.btClientConfig(t)
		cfg.NameOverride = origName
		cfg.WantedPieces = wanted
		cfg.FileWanted = fileWanted
		final := filepath.Join(t.Dir, origName)
		if _, statErr := os.Stat(final); statErr == nil {
			cl, err = bt.NewSeedClient(meta, final, cfg)
			if err != nil {
				m.btPortRelease(cfg.ListenPort)
				return err
			}
			// 做种会话必须完整启动:挂 tracker(下载者要能 announce 找到我们)、
			// 回灌 peer 历史、Start(监听/announce/补拨循环)。旧实现直接进
			// goSeeding,客户端从未 Start——tracker 页只剩全局 DHT,入站连接
			// 永远为 0,做种形同虚设。
			cl.AddTrackers(trackers)
			t.mu.Lock()
			hist := t.PeerHistory
			t.mu.Unlock()
			if n := cl.LoadPeerHistory(hist); n > 0 {
				log.Printf("做种恢复 peer 历史回灌 %d 个候选", n)
			}
			t.mu.Lock()
			t.Downloaded = t.Size
			// 数据已全量在盘:同步落盘进度口径(历史版本完成瞬间未补同步,
			// 持久化的 arrived 停在最后一次 pump tick,重启后仍显示 <100%)
			t.arrived = t.Size
			t.Arrived = t.Size
			t.mu.Unlock()
			cl.Start()
			m.lsdAnnounceTask(t)
			seeding = true
			m.goSeeding(t, cl)
			return nil
		}
		// 最终文件缺失:数据可能仍在临时文件(未 finalize)。沿用原名继续,
		// 交由 NewClient 逐 piece 校验;全部通过时立即收尾(见 FinishIfComplete)。
	}

	// 名字:就地布局下数据自始至终写在最终路径上,续传绝不能改名(改名
	// 只会制造找不到数据的新目录并从零重下)。仅当目标名被【其他任务或
	// 外来数据】占用时才冲突改名一次,此后 Filename 固定。
	finalName := origName
	if !allHave && m.btNameOccupied(t, origName) {
		m.mu.Lock()
		finalName = filepath.Base(util.UniquePath(t.Dir, origName))
		m.mu.Unlock()
	}

	cfg := m.btClientConfig(t)
	cfg.NameOverride = finalName
	cfg.WantedPieces = wanted
	cfg.FileWanted = fileWanted
	if cl, err = bt.NewClient(meta, t.Dir, cfg, initialHave); err != nil {
		m.btPortRelease(cfg.ListenPort)
		return err
	}
	// 还原暂停/崩溃前保存的块级位图:在途 piece 的已收块不重下,
	// 消除"暂停再继续进度倒退一大截"(位图缺失/损坏时退回整 piece 恢复)。
	// 前置条件是数据还在盘上(RestoreBlocks 信任位图、逐块标记持有,不做
	// 磁盘校验):数据目录已不在(取消清理/手动删除)时,位图描述的是
	// 幻影数据——照信会瞬间虚报进度(实测取消重启凭空多出 80MB),又让
	// piece 以零块凑数、校验失败整片重下。此时连同已校验位图一起作废。
	t.mu.Lock()
	blockBits := t.BlockBits
	t.mu.Unlock()
	if raw, derr := base64.StdEncoding.DecodeString(blockBits); derr == nil && len(raw) > 0 {
		if btDataOnDisk(t) {
			cl.RestoreBlocks(raw)
		} else {
			log.Printf("[bt] 数据目录已不在,作废续传位图重新下载 %s", finalName)
			t.mu.Lock()
			t.Bitfield, t.BlockBits = "", ""
			t.mu.Unlock()
		}
	}
	cl.AddTrackers(trackers)

	// 灌入上次会话的 peer 历史(对照 BitComet"历史"来源):回补循环按最久
	// 未试顺序重拨,tracker/DHT 冷启动失败时也能靠老邻居入网
	t.mu.Lock()
	hist := t.PeerHistory
	t.mu.Unlock()
	if n := cl.LoadPeerHistory(hist); n > 0 {
		log.Printf("BT peer 历史回灌 %d 个候选", n)
	}

	t.mu.Lock()
	t.Filename = finalName
	// piece 口径随文件选择:全选为全量,选择下载为需求内 piece 数。
	// 无条件写全量会把选择任务的"已完成 x/需求 y"冲成 x/全量(实测
	// 4/24 文件任务显示 x/2844 而非 x/517)
	if wanted == nil {
		t.PiecesTotal = meta.PieceCount()
	} else {
		t.PiecesTotal = countTrue(wanted)
	}
	t.uploadBase = t.Uploaded
	t.downloadBase = t.Downloaded
	t.btcl = cl
	t.mu.Unlock()

	cl.Start()
	m.lsdAnnounceTask(t) // 局域网广播本会话(LSD 未启用时为空操作)
	// 若起始位图(或磁盘扫描恢复)已使全部 piece 校验通过,立即收尾落定→做种。
	// 不主动触发的话,会话会停在 AllDone 却无人触发完成(任务"运行中"、速度恒 0)。
	// FinishIfComplete 在未全完成时是空操作,故无需前置判断。
	if cl.FinishIfComplete() {
		log.Printf("BT 任务启动即校验通过,直接落定 %s", finalName)
	}

	defer func() {
		if seeding {
			return
		}
		// 收尾守卫:ctx 取消到本 defer 执行之间,用户可能已完成"取消"
		// (状态=canceled、位图作废、JSON 落盘)。bt.82 后 Stop 不再同步
		// 等通告,收尾几毫秒内跑完,与紧随暂停的取消并发成为常态——旧
		// 快照若照常 btSaveBitfield+persist,会把已作废的位图复活进 JSON
		// (回归测试实测:"AA==" 复活)。取消语义由 Cancel 的落盘为准,
		// 此处让路;其余路径(暂停/错误/退出)照常保存续传状态。
		t.mu.Lock()
		canceled := t.Status == StatusCanceled
		t.mu.Unlock()
		if !canceled {
			m.btSaveBitfield(t, cl, meta)
		}
		st := cl.Stats()
		cl.Stop()
		// 会话结束,NAT 按实际监听端口撤销映射(做种交接路径不经过此处)
		m.btPortRelease(cl.ListenPort())
		t.mu.Lock()
		t.Uploaded = t.uploadBase + st.Uploaded
		// 到货字节补同步:pump 每秒一 tick,暂停/取消若落在两次 tick 之间,
		// 最后一段已落盘数据不在进度里(仍归本会话所有时才写,防止回归新会话)
		if t.btcl == cl {
			t.arrived = btClampArrived(t.Size, cl.ArrivedBytes())
			t.Arrived = t.arrived
			t.Connections = 0 // 暂停/取消后会话已结束
			t.PiecesDone = cl.PiecesDone()
		}
		// 归属判定:暂停后快速恢复时,新会话可能已把自己的 client 挂上
		// t.btcl。旧会话迟到收尾若无条件置 nil,新会话的引用就被抹掉——
		// 任务仍在下载,但详情页 tracker/peer 从此为空,封禁/手动加 peer
		// 等会话操作全部报"会话未运行"。
		t.mu.Unlock()
		// 关闭(溢写攒块缓冲)与落盘必须先于摘除 btcl:改选重启靠
		// "btcl 已清"判断旧会话句柄释放完毕,提前清会让新会话与旧会话的
		// 溢写并发写同一批文件(零占位块盖掉已落定数据)。
		cl.Close()
		if !canceled {
			m.persist(t)
		}
		t.mu.Lock()
		if t.btcl == cl {
			t.btcl = nil
			t.stream = nil // 会话结束,边下边播状态一并作废(读取方将报会话未运行)
		}
		t.mu.Unlock()
	}()

	go m.btPump(ctx, t, cl, meta) // 后台同步进度;完成或取消即退出

	completeCh := make(chan error, 1)
	go func() { completeCh <- cl.WaitComplete(ctx) }()
	select {
	case <-ctx.Done():
	case err := <-completeCh:
		if err != nil && !cl.Completed() {
			return err
		}
	}
	if ctx.Err() != nil && !cl.Completed() {
		return ctx.Err()
	}

	// 下载完成:落定状态,按策略转入做种。完成与取消竞态时一律以完成为准
	// (Completed()=数据已 Finalize 落定到最终路径):此时把任务交给取消
	// 清理会删掉成品;取消被"吞"是有意为之——成品比取消语义更值钱。
	final := cl.FinalPath()
	m.btSaveBitfield(t, cl, meta) // 快速完成时 pump 的收尾 tick 可能被 ctx 取消抢先,这里同步落盘
	t.mu.Lock()
	// 有意口径:完成瞬间 Downloaded 收口为大小(分享率分母=任务量),
	// 不保留重传等会话内累计流量;做种期由 uploadBase 继续累计上传量
	t.Downloaded = t.Size
	// 到货字节补同步到 100%:完成可能落在 pump 两次 tick 之间,最后一段
	// 数据(实测可见 ~30KB~1MB)若不在此补记,任务将永远停在 <100%
	t.arrived = btClampArrived(t.Size, cl.ArrivedBytes())
	t.Arrived = t.arrived
	t.Connections = 0 // 不转做种时会话已结束;转做种则由做种循环接管
	t.PiecesDone = countTrue(wanted)
	t.BlockBits = "" // 下载完成:块级位图不再需要
	t.Status = StatusCompleted
	t.CompletedAt = time.Now()
	t.Err = ""
	t.mu.Unlock()
	m.persist(t)
	m.bump()
	log.Printf("BT 任务完成 %s -> %s", finalName, final)

	if m.shouldSeed() {
		seeding = true
		m.goSeeding(t, cl)
	}
	return nil
}

// btEnsureMeta 获取元数据:种子任务读归档;磁力任务经 peer 抓取后归档。
// 返回解析结果与种子原文(choosing 挂起时据此写用户可见副本)。
func (m *Manager) btEnsureMeta(ctx context.Context, t *Task) (*bt.MetaInfo, []byte, error) {
	t.mu.Lock()
	tpath, magnet := t.TorrentPath, strings.HasPrefix(t.URL, "magnet:")
	t.mu.Unlock()

	if tpath != "" {
		if raw, err := os.ReadFile(tpath); err == nil {
			if meta, perr := bt.ParseTorrent(raw); perr == nil {
				m.fillBTTaskFromMeta(t, meta)
				return meta, raw, nil
			}
		}
	}
	if !magnet {
		return nil, nil, errors.New("种子归档丢失且任务无磁力源")
	}
	mg, err := bt.ParseMagnet(t.URL)
	if err != nil {
		return nil, nil, err
	}
	trackers := append(append([]string(nil), mg.Trackers...), m.btTrackersFor(t, nil, true)...)
	port := m.btAllocPort()
	// tracker/peer 分别按粒度开关解析代理出口(udp tracker 与 DHT 无法走 http 代理)
	trackerTr := proxy.Transport(m.proxyOptsFor(t, "tracker"))
	m.dohWrapTransport(trackerTr)
	nc := bt.NetConfig{
		HTTPClient: &http.Client{
			Timeout:   20 * time.Second,
			Transport: trackerTr,
		},
		Dialer:    m.btPeerDialer(t),
		UDPRelay:  m.getUDPRelay(),
		Resolver:  m.dohResolver(),
		SpoofName: m.cfg.BT.EffectiveSpoofName(m.cfg.UserAgent),
	}
	log.Printf("磁力任务 %x 开始获取元数据(trackers=%d, x.pe=%d, dht=%v)", mg.InfoHash, len(trackers), len(mg.Peers), m.dhtSrv != nil)
	direct := bt.ParseDirectPeers(mg.Peers)
	var meta *bt.MetaInfo
	if mg.IsV2() {
		meta, err = bt.FetchMetadataV2(ctx, mg.InfoHash, mg.V2Hash, bt.PeerID(), trackers, direct, port, m.getDHT(), nc)
	} else {
		meta, err = bt.FetchMetadata(ctx, mg.InfoHash, bt.PeerID(), trackers, direct, port, m.getDHT(), nc)
	}
	if err != nil {
		return nil, nil, err
	}
	announce := ""
	if len(trackers) > 0 {
		announce = trackers[0]
	}
	raw, err := bt.BuildTorrentBytes(meta, announce, m.cfg.BT.EffectiveSpoofName(m.cfg.UserAgent))
	if err != nil {
		return nil, nil, err
	}
	if err := m.archiveTorrent(t, raw); err != nil {
		return nil, nil, err
	}
	m.fillBTTaskFromMeta(t, meta)
	m.bump()
	log.Printf("磁力任务元数据就绪:%s(%s,%d 个文件)", meta.Name, fmt.Sprintf("%d 字节", meta.TotalLength), len(meta.Files))
	return meta, raw, nil
}

// btHoldForChoose 磁力"先选文件再下载":创建时要求选择(ChoosePending)、
// 尚未选过时挂起任务为 choosing 状态——把 .torrent 副本写到下载目录
// (<名>.torrent,任务删除后仍可再创建),UI 据此预填创建对话框。等用户
// 创建任务或点继续(= 全部下载)。单文件无可选,直接放行——关联启动
// (HoldAny)除外:单文件也要先过创建对话框选保存目录。挂起即返回:
// 调度槽释放,不建 BT 会话。
func (m *Manager) btHoldForChoose(t *Task, meta *bt.MetaInfo, raw []byte) bool {
	t.mu.Lock()
	pending, chosen := t.ChoosePending, len(t.WantedFiles) > 0
	holdAny := t.HoldAny
	tf := t.TorrentFile // 来源是本机种子文件时,创建时已记下原文件路径
	t.mu.Unlock()
	if !pending || chosen {
		return false
	}
	if len(meta.Files) <= 1 && !holdAny {
		return false
	}
	t.mu.Lock()
	t.ChoosePending = false
	t.HoldAny = false
	t.Status = StatusChoosing
	// 可见副本(供创建对话框预填与用户留存):
	//   - 种子文件来源(双击 .torrent / 路径创建):原文件即副本,直接引用,
	//     不复制(下载目录与来源同目录时会得到"同名(1)"副本)。
	//   - 磁力任务:种子字节已归档进隐藏任务目录(btEnsureMeta→archiveTorrent
	//     写的 m.tasksDir/<id>.torrent),直接引用那份归档作可见副本——choosing
	//     阶段不落下载目录,默认下载文件夹不再被占用;用户确认目录后 AddBT 把
	//     种子副本复制进所选目录(见 placeVisibleTorrent),种子随数据一起走。
	vpath := ""
	if tf != "" {
		if _, err := os.Stat(tf); err == nil {
			vpath = tf
		}
	}
	if vpath == "" {
		vpath = t.TorrentPath // 隐藏任务目录里的归档种子
	}
	if vpath == "" {
		vpath = filepath.Join(m.tasksDir, t.ID+".torrent")
	}
	needWrite := false
	if len(raw) > 0 && vpath != tf {
		if _, serr := os.Stat(vpath); serr != nil {
			needWrite = true // 归档缺失时的兜底:写进隐藏任务目录(不落下载目录)
		}
	}
	t.TorrentFile = vpath
	t.mu.Unlock()
	if needWrite {
		_ = os.MkdirAll(m.tasksDir, 0o755) // 兜底:确保隐藏任务目录存在(生产环境由 Run 先行创建)
		if werr := os.WriteFile(vpath, raw, 0o644); werr != nil {
			// 可见副本写失败不阻断:任务仍挂起,UI 回退到内置改选窗口
			t.mu.Lock()
			t.TorrentFile = ""
			t.mu.Unlock()
			vpath = ""
			log.Printf("磁力种子副本写入失败(%v),任务转为内置选择窗口", werr)
		}
	}
	m.persist(t)
	m.bump()
	log.Printf("磁力任务种子已获取:%s(%d 个文件),等待创建下载任务(%s)", meta.Name, len(meta.Files), vpath)
	// 异步迁到 choosing:主动唤窗+强制唤醒自托盘隐藏的 WebView2,让前端弹出文件
	// 选择/创建窗(聚合器只广播快照不唤窗,挂起的前端无法自行弹出)。
	if m.OnChoosePrompt != nil {
		m.OnChoosePrompt(t.ID)
	}
	return true
}

// ChooseBTFiles 落定 BT 文件选择:files 为选中的相对路径,all=true 表示全部。
// choosing 状态选完立即排队下载;paused 状态允许改选(重算大小/piece 口径,
// 仍保持暂停)。返回更新后的任务快照。
func (m *Manager) ChooseBTFiles(id string, files []string, all bool) (TaskView, error) {
	t := m.get(id)
	if t == nil {
		return TaskView{}, ErrNotFound
	}
	t.mu.Lock()
	kind, status, tpath := t.Kind, t.Status, t.TorrentPath
	t.mu.Unlock()
	if kind != KindBT {
		return TaskView{}, errors.New("仅 BT 任务支持文件选择")
	}
	switch status {
	case StatusChoosing, StatusWaiting, StatusPaused, StatusError,
		StatusRunning, StatusSeeding, StatusCompleted:
		// 详情页随时可改选:未运行状态直接改账;运行/做种中的会话停掉后
		// 按新选择重启;已完成的缩选只改账不打断
	default:
		return TaskView{}, fmt.Errorf("状态 %s 不可选择文件", status)
	}
	if tpath == "" {
		return TaskView{}, errors.New("元数据尚未就绪")
	}
	raw, err := os.ReadFile(tpath)
	if err != nil {
		return TaskView{}, errors.New("种子归档读取失败: " + err.Error())
	}
	meta, err := bt.ParseTorrent(raw)
	if err != nil {
		return TaskView{}, errors.New("种子归档解析失败: " + err.Error())
	}
	if !all {
		// 路径统一成正斜杠(与 WantedPiecesFor 的口径一致),再对照种子清单过滤
		known := map[string]bool{}
		for _, f := range meta.Files {
			known[strings.Join(f.Path, "/")] = true
		}
		sel := make([]string, 0, len(files))
		seen := map[string]bool{}
		for _, p := range files {
			p = strings.Join(strings.Split(p, "\\"), "/")
			if known[p] && !seen[p] {
				seen[p] = true
				sel = append(sel, p)
			}
		}
		if len(sel) == 0 {
			return TaskView{}, errors.New("至少选择一个文件")
		}
		files = sel
	}
	// 已完成任务"只删不增"(新选择仍被位图完全覆盖):数据早已齐,就地
	// 改账即可,不打断做种/完成态。覆盖判定只读位图,放在大锁外
	// (btSelectionCovered 内部要取 t.mu,不可在持锁区调用)
	selForCover := files
	if all {
		selForCover = nil
	}
	covered := btSelectionCovered(t, meta, selForCover)
	// 活动会话的停止句柄须在锁内取出:running 靠取消运行 ctx(会话经
	// select 退出并收尾),seeding 靠关闭做种信号(协程负责停会话)。
	// 旧会话不退,新会话就会与它并发写同一批数据(实测双会话:旧会话
	// 按旧选择把整个种子下完,再用旧账目覆盖完成状态,pieces 显示 4/1)
	var (
		stopRun  context.CancelFunc
		stopSeed chan struct{}
	)
	t.mu.Lock()
	if all {
		t.WantedFiles = nil
		t.Size = meta.TotalLength
		t.PiecesTotal = meta.PieceCount()
	} else {
		t.WantedFiles = files
		t.Size = selectedBytes(meta, files)
		t.PiecesTotal = countTrue(meta.WantedPiecesFor(files))
	}
	shrinkDone := (status == StatusCompleted || status == StatusSeeding) && covered
	if shrinkDone {
		t.PiecesDone = t.PiecesTotal
		// 账面收口:arrived 是旧选择的落盘量,缩选后必须夹到新口径内,
		// 否则详情页出现"已下 50KB / 大小 15B"式倒挂(实测)
		t.arrived = btClampArrived(t.Size, t.arrived)
		t.Arrived = t.arrived
	}
	switch status {
	case StatusChoosing:
		t.Status = StatusWaiting
	case StatusRunning, StatusSeeding, StatusCompleted:
		if !shrinkDone {
			// Paused 先挡住调度器;会话由下面的停止句柄收尾,runTask 的
			// 归属守卫见状态非 running 不会改账。等 btcl 清零(句柄释放、
			// 攒块缓冲落盘)后再排队重启
			t.Status = StatusPaused
			if status == StatusRunning {
				stopRun = t.cancel
			} else if status == StatusSeeding {
				stopSeed = t.seedStop
			}
		}
	}
	t.mu.Unlock()
	if stopRun != nil {
		stopRun()
	}
	if stopSeed != nil {
		close(stopSeed)
	}
	m.persist(t)
	m.bump()
	switch {
	case status == StatusChoosing:
		m.kick() // 选择完成,立即排队下载
	case shrinkDone:
		// 就地生效,无需重启
	case status == StatusRunning || status == StatusSeeding || status == StatusCompleted:
		// 异步重启:接口立即返回新账目的视图,旧会话退出(句柄释放、
		// 攒块缓冲落盘)后由后台协程重新排队。同步等会把不可达 tracker
		// 的会话收尾拖进 HTTP handler(实测改选 5028ms,用户感知为
		// "保存转圈 5 秒");状态经 paused→waiting→running 的 SSE 推送,
		// UI 无需感知等待过程
		go m.restartBTForSelection(t)
	}
	return t.View(), nil
}

// btSelectionCovered 报告任务的位图是否覆盖全部新需求 piece(改选只删不增;
// files 为 nil = 全选)。内部取 t.mu,调用方不得已持有该锁。
func btSelectionCovered(t *Task, meta *bt.MetaInfo, files []string) bool {
	t.mu.Lock()
	bitfield := t.Bitfield
	t.mu.Unlock()
	have := btDecodeBitfield(bitfield, meta.PieceCount())
	if len(have) != meta.PieceCount() {
		return false
	}
	for i, w := range meta.WantedPiecesFor(files) {
		if w && !have[i] {
			return false
		}
	}
	return true
}

// restartBTForSelection 等活动会话完全退出(存储句柄释放、攒块缓冲落盘)
// 后把任务重新排队。绝不能在旧会话收尾前就让新会话开写:旧会话 Close 时
// 的整片溢写会把零占位块盖到新会话已落定的数据上。
// 由 ChooseBTFiles 异步调用:等待期间用户可能取消/删除/手动恢复,回置
// waiting 前必须校验状态仍停在 paused(改动选重启的路标),否则会覆盖
// 用户操作后的状态。
func (m *Manager) restartBTForSelection(t *Task) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		t.mu.Lock()
		cl := t.btcl
		t.mu.Unlock()
		if cl == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.mu.Lock()
	if t.removed || t.Status != StatusPaused {
		t.mu.Unlock()
		return
	}
	t.Status = StatusWaiting
	t.mu.Unlock()
	m.persist(t)
	m.bump()
	m.kick()
}

// btTrackersFor 汇总任务可用 tracker:任务级 + 种子内置 + (可选)公共列表。
// withPublic 控制是否附带公共列表:磁力元数据抓取恒为 true;下载阶段由
// 设置项 autoAddTrackers 决定(私有种子在任何路径都不附带)。
func (m *Manager) btTrackersFor(t *Task, meta *bt.MetaInfo, withPublic bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u = strings.TrimSpace(u); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	t.mu.Lock()
	for _, u := range t.Trackers {
		add(u)
	}
	t.mu.Unlock()
	if meta != nil {
		add(meta.Announce)
		for _, tier := range meta.AnnounceList {
			for _, u := range tier {
				add(u)
			}
		}
	}
	if withPublic && (meta == nil || !meta.Private) {
		m.mu.Lock()
		for _, u := range m.cfg.BT.PublicTrackers {
			add(u)
		}
		m.mu.Unlock()
	}
	return out
}

// autoAddPublicTrackers 读取"自动添加到 BT 任务"开关(加锁读配置)。
func (m *Manager) autoAddPublicTrackers() bool {
	m.mu.Lock()
	v := m.cfg.BT.AutoAddTrackers
	m.mu.Unlock()
	return v
}

// btPeerDialer 组装 peer 拨号器。配置了手动代理时采用"双栈"策略:
// 先直连(短超时快速失败),失败再经代理——国内 swarm 直连可达性普遍
// 高于代理出口(BT peer 不适合一律走代理,与 BitComet 行为一致)。
func (m *Manager) btPeerDialer(t *Task) func(ctx context.Context, network, addr string) (net.Conn, error) {
	proxied := proxy.DialContext(m.proxyOptsFor(t, "peer"))
	t.mu.Lock()
	po := t.ProxyOverride
	t.mu.Unlock()
	manual := po != "" && po != "none"
	if !manual && m.cfg.Proxy.PeerOn() &&
		(m.cfg.Proxy.Mode == "manual" || (m.cfg.Proxy.Mode == "env" && proxy.EnvProxyConfigured())) {
		manual = true
	}
	if !manual {
		return proxied
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 8 * time.Second}
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil {
			return conn, nil
		}
		return proxied(ctx, network, addr)
	}
}

// btClientConfig 组装会话配置;限速取任务级与全局的较严者。
func (m *Manager) btClientConfig(t *Task) bt.ClientConfig {
	// 任务级限速:任务未单独限速时取"默认每任务限速"(与 httpdl 同口径,
	// 0 = 不设任务级上限)。全局预算不在此折算——旧实现把全局值直接当每
	// 任务配额(会话各自一只限速器),N 个任务总量超标 N 倍;现在由共享
	// 全局限速器在全部会话(下载含 HTTP)间统一仲裁,改全局即时生效。
	t.mu.Lock()
	down, up := t.SpeedLimit, t.UPLimit
	t.mu.Unlock()
	if down <= 0 {
		down = m.cfg.TaskLimit
	}
	// BT 各链路独立解析代理出口(tracker/peer/webseed 粒度开关见设置);
	// 所有 tracker 的域名解析统一走 DoH(本地 DNS 存在污染)
	trackerOpts := m.proxyOptsFor(t, "tracker")
	webSeedOpts := m.proxyOptsFor(t, "webseed")
	relay := m.getUDPRelay()
	trackerTr := proxy.Transport(trackerOpts)
	m.dohWrapTransport(trackerTr)
	webSeedTr := proxy.Transport(webSeedOpts)
	m.dohWrapTransport(webSeedTr)
	// 共享监听端口:port 为 0 时 Hub 为 nil,会话回退独立随机端口
	port, hub := m.btPortAcquire()
	return bt.ClientConfig{
		ListenPort:      port,
		Hub:             hub,
		MaxPeers:        m.cfg.BT.MaxPeers,
		DownLimit:       down,
		UpLimit:         up,
		GlobalDown:      m.gLim,
		GlobalUp:        m.btULim,
		AutoBanPoison:   m.cfg.BT.AutoBanPoison,
		PoisonRate:      m.cfg.BT.PoisonRate,
		PoisonStrikes:   m.cfg.BT.PoisonStrikes,
		ClientBlacklist: m.cfg.BT.ClientBlacklist,
		BanList:         m.banList,
		SpoofName:       m.cfg.BT.EffectiveSpoofName(m.cfg.UserAgent),
		DHT:             m.getDHT(),
		DHT6:            m.getDHT6(),
		HTTPClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: trackerTr,
		},
		WebSeedHTTPClient: &http.Client{
			Timeout:   90 * time.Second,
			Transport: webSeedTr,
		},
		Dialer:          m.btPeerDialer(t),
		UDPRelay:        relay,
		Resolver:        m.dohResolver(),
		DialConcurrency: m.btPeerDialConcurrency(t),
	}
}

// btPeerDialConcurrency 并发拨号上限:peer 直连时放大——tracker/DHT 返回的
// 候选里大量是死地址,并发过低时一轮筛完要十几分钟,活 peer 迟迟连不上
// (同 swarm 的 BitComet 能连三十多条,我们只连个位数)。经代理时收紧,
// 避免并发 CONNECT 打满代理出口。
func (m *Manager) btPeerDialConcurrency(t *Task) int {
	o := m.proxyOptsFor(t, "peer")
	if o.Manual == "none" {
		return 64
	}
	if o.Manual != "" || o.Mode == "manual" || (o.Mode == "env" && proxy.EnvProxyConfigured()) {
		return 12
	}
	return 64
}

// dohWrapTransport 给直连传输层的拨号加 DoH 解析(本地 DNS 存在污染)。
// 经代理的传输层域名由代理侧解析,无需处理。
func (m *Manager) dohWrapTransport(tr *http.Transport) {
	if tr.Proxy != nil || tr.DialContext == nil {
		return
	}
	res := m.dohResolver()
	if res == nil { // DNS 走代理已关闭:保持系统解析
		return
	}
	base := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err == nil && net.ParseIP(host) == nil {
			if ips, e := res(ctx, host); e == nil && len(ips) > 0 {
				addr = net.JoinHostPort(ips[0].String(), port)
			}
		}
		return base(ctx, network, addr)
	}
}

// getDHT 返回共享 DHT 节点(未启用返回 nil)。
func (m *Manager) getDHT() *bt.DHTServer {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	return m.dhtSrv
}

// getDHT6 返回 IPv6 DHT 独立实例(未启用/无 v6 网络返回 nil)。
func (m *Manager) getDHT6() *bt.DHTServer {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	return m.dhtSrv6
}

// btAllocPort 探测一个可绑定的 BT 监听端口(从设置端口起顺延 10 个)。
func (m *Manager) btAllocPort() int {
	base := m.cfg.BT.Port
	if base <= 0 {
		base = 6881
	}
	for p := base; p < base+10; p++ {
		ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return 0 // 全部占用则随机端口(无入站能力,仍可出站下载)
}

// btPump 把会话统计同步到任务字段;速度折算由 Manager ticker 统一处理。
func (m *Manager) btPump(ctx context.Context, t *Task, cl *bt.Client, meta *bt.MetaInfo) {
	tk := time.NewTicker(500 * time.Millisecond)
	defer tk.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		st := cl.Stats()
		pieces := cl.PiecesDone()
		t.mu.Lock()
		// 会话归属守卫:任务已取消/暂停/被新会话接管时,本会话不得再回写
		// 账目与位图——取消路径稍后会作废并落盘位图,这里一次晚到的周期
		// 持久化就会让描述已删数据的位图在 JSON 里复活
		if t.Status != StatusRunning || t.btcl != cl {
			t.mu.Unlock()
			return
		}
		// Downloaded 为累计接收流量(分享率口径);Arrived 为落盘去重后的
		// 字节(进度条口径:块粒度平滑推进,重复/陈旧交付不重复计数,
		// 上界为任务总量,不会超过 100%)
		t.Downloaded = t.downloadBase + st.Downloaded
		t.Uploaded = t.uploadBase + st.Uploaded
		t.Connections = st.Peers
		t.PiecesDone = pieces
		t.arrived = btClampArrived(t.Size, cl.ArrivedBytes())
		t.Arrived = t.arrived // 同步到导出字段随任务 JSON 持久化,重启后暂停态也能显示真实进度
		t.mu.Unlock()
		if cl.Completed() {
			m.btSaveBitfield(t, cl, meta)
			m.persist(t)
			return
		}
		// 周期落盘位图与 peer 历史(~2 分钟):强杀进程也能保住最近已知
		// 的候选,重启不必从零发现 peer
		if i%240 == 0 {
			m.btSaveBitfieldIfOwned(t, cl, meta)
		}
		if i%30 == 0 { // 每 15 秒持久化位图与上传量
			m.btSaveBitfieldIfOwned(t, cl, meta)
		}
		if i%60 == 30 { // 周期检查边下边播空闲回收(无请求超时还原优先级)
			m.btStreamIdleReclaim(t, cl)
		}
	}
}

// btSaveBitfieldIfOwned 仅当 cl 仍是任务的当前会话时持久化位图。周期持久化
// 与取消收尾并发:取消路径会清空位图并落盘,这里晚到的一拍若不检查归属,
// 就会把已作废的位图重新写回任务 JSON。
func (m *Manager) btSaveBitfieldIfOwned(t *Task, cl *bt.Client, meta *bt.MetaInfo) {
	t.mu.Lock()
	owned := t.btcl == cl
	t.mu.Unlock()
	if owned {
		m.btSaveBitfield(t, cl, meta)
		m.persist(t)
	}
}

// ---------- 做种 ----------

func (m *Manager) shouldSeed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.BT.AutoSeed
}

// goSeeding 接管已完成的客户端进入做种;不占调度槽位。
func (m *Manager) goSeeding(t *Task, cl *bt.Client) {
	t.mu.Lock()
	t.uploadBase = t.Uploaded
	t.Status = StatusSeeding
	t.SeedStarted = time.Now()
	t.Err = ""
	t.BlockBits = ""
	t.btcl = cl
	t.lastBytes = 0
	// 速度数据源从落盘进度切到累计上传量,旧基线必须作废:否则做种首
	// 个样本按 (Uploaded-0) 作差,把历史累计上传量虚报成瞬时速度。
	// speed 同拍清零:本函数随即 bump 刷新 UI,冻结的旧值会先闪现一拍
	// (首个测速 tick 在 ~500ms 后才到)
	t.speed = 0
	t.speedPrimed = false
	t.seedStop = make(chan struct{})
	t.mu.Unlock()
	m.persist(t)
	m.bump()
	log.Printf("做种开始 %s", t.Filename)

	m.seedWG.Add(1)
	go func() {
		defer m.seedWG.Done()
		defer func() { // 覆盖所有退出路径:停会话、释放句柄、持久化
			st := cl.Stats()
			cl.Stop()
			// 做种会话结束,撤销其监听端口的 NAT 映射
			m.btPortRelease(cl.ListenPort())
			cl.Close()
			t.mu.Lock()
			t.Uploaded = t.uploadBase + st.Uploaded
			// 归属判定同 runBT 收尾:策略达标/新会话接管的竞态下,
			// t.btcl 已不是本会话的 client 时不得清引用与连接数。
			// 摘除 btcl 放在 Close/persist 之后(同 runBT 收尾):改选
			// 重启以 btcl 清零为准判断旧会话句柄已释放。
			removed := t.removed
			delData := t.delData
			t.mu.Unlock()
			if removed {
				// 任务已被删除:记录文件已在 Remove 时删除,此处不再 persist
				// (persist 对 removed 任务本身也会跳过);句柄已释放,可在
				// 此时安全删除最终数据。
				if delData {
					m.removeFinalData(t)
				}
				t.mu.Lock()
				if t.btcl == cl {
					t.btcl = nil
					t.stream = nil
				}
				t.mu.Unlock()
				return
			}
			m.persist(t)
			t.mu.Lock()
			if t.btcl == cl {
				t.Connections = 0 // 会话结束,连接数清零
				t.btcl = nil
				t.stream = nil
			}
			t.mu.Unlock()
		}()
		// 500ms:必须与 Manager ticker 的测速节拍一致。旧值 1s 会让
		// 测速采样隔次读到 0 增量、下次读到一整秒的量,EWMA 稳态在
		// 真实速率的 2/3 与 4/3 之间摆动(UI 实测 130↔270,真值 200)。
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-m.rootCtx.Done():
				cl.Stop()
				return
			case <-t.seedStop:
				return
			case <-tk.C:
			}
			st := cl.Stats()
			t.mu.Lock()
			t.Uploaded = t.uploadBase + st.Uploaded
			t.Connections = st.Peers // btPump 已退出,做种期连接数由本循环维护
			ratio := 0.0
			if t.Downloaded > 0 {
				ratio = float64(t.Uploaded) / float64(t.Downloaded)
			}
			dur := time.Since(t.SeedStarted)
			t.mu.Unlock()

			if m.seedPolicyReached(t, ratio, dur) {
				t.mu.Lock()
				t.Status = StatusCompleted
				t.CompletedAt = time.Now()
				// btcl 引用留给收尾 defer 按归属判定统一清理(此处置 nil 会让
				// defer 的归属检查失真,漏记最终上传统计)
				t.mu.Unlock()
				m.persist(t)
				m.bump()
				log.Printf("做种结束(策略达标) %s 上传 %s", t.Filename, fmt.Sprintf("%d 字节", st.Uploaded))
				return
			}
		}
	}()
}

// seedPolicyReached 判断做种策略是否达标(任一条件满足即停;均未设 = 永久做种)。
func (m *Manager) seedPolicyReached(t *Task, ratio float64, dur time.Duration) bool {
	m.mu.Lock()
	cfg := m.cfg.BT
	m.mu.Unlock()
	t.mu.Lock()
	target, minutes := t.SeedRatio, t.SeedMinutes
	t.mu.Unlock()
	if target <= 0 {
		target = cfg.SeedRatio
	}
	if minutes <= 0 {
		minutes = cfg.SeedMinutes
	}
	if target <= 0 && minutes <= 0 {
		return false
	}
	if target > 0 && ratio >= target {
		return true
	}
	if minutes > 0 && dur >= time.Duration(minutes)*time.Minute {
		return true
	}
	return false
}

// StopSeeding 停止做种:任务归入已完成。
func (m *Manager) StopSeeding(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	if t.Status != StatusSeeding {
		st := t.Status
		t.mu.Unlock()
		return fmt.Errorf("状态 %s 不在做种", st)
	}
	stop := t.seedStop
	t.Status = StatusCompleted
	t.CompletedAt = time.Now()
	t.mu.Unlock()
	if stop != nil {
		close(stop) // 做种协程负责 cl.Stop 与持久化
	}
	m.bump()
	return nil
}

// ---------- 位图持久化 ----------

// btSaveBitfield 把已校验 piece 位图 base64 后存入任务字段,并同步快照
// peer 历史(候选池按最近确认排序截断,重启回灌用)。
// 落盘前先写回归档种子:缺 layers 的 v2 磁力在下载期反推出的 piece layers
// 必须与"已校验 piece"同时持久化,否则重启后盘上位图对应的期望哈希丢失,
// 会误判为未校验而整文件重下。
// blockBitsCap 限制块级位图持久化体积(极端大种子退回整 piece 恢复)。
const blockBitsCap = 4 << 20

func (m *Manager) btSaveBitfield(t *Task, cl *bt.Client, meta *bt.MetaInfo) {
	m.archiveDerivedLayers(t, meta)
	have := cl.HaveBits()
	b := make([]byte, (len(have)+7)/8)
	for i, h := range have {
		if h {
			b[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	blocks := cl.SnapshotBlocks()
	t.mu.Lock()
	t.Bitfield = base64.StdEncoding.EncodeToString(b)
	if len(blocks) <= blockBitsCap {
		t.BlockBits = base64.StdEncoding.EncodeToString(blocks)
	}
	t.PeerHistory = cl.PeerHistory()
	t.mu.Unlock()
}

// archiveDerivedLayers 把下载期反推出的 v2 piece layers 写回任务目录的归档
// 种子。磁力经 ut_metadata 只拿到 info dict(BEP-52 的顶层 piece layers 不
// 在其中),整文件 merkle 校验通过后回填的层哈希只有写回种子,重启才能直接
// 逐片校验,而不必重新反推或被误判重下。
func (m *Manager) archiveDerivedLayers(t *Task, meta *bt.MetaInfo) {
	if meta == nil || !meta.LayersDirty() {
		return
	}
	t.mu.Lock()
	tpath := t.TorrentPath
	announce := ""
	if len(t.Trackers) > 0 {
		announce = t.Trackers[0]
	}
	t.mu.Unlock()
	if tpath == "" {
		return
	}
	raw, err := bt.BuildTorrentBytes(meta, announce, m.cfg.BT.EffectiveSpoofName(m.cfg.UserAgent))
	if err != nil {
		log.Printf("v2 piece layers 写回失败(重建种子): %v", err)
		return
	}
	if err := os.WriteFile(tpath, raw, 0o644); err != nil {
		log.Printf("v2 piece layers 写回失败: %v", err)
		return
	}
	meta.ClearLayersDirty()
	log.Printf("v2 piece layers 已写回归档种子 %s", tpath)
}

// btDecodeBitfield 解码持久化位图;长度不符视为无位图(全新下载)。
func btDecodeBitfield(b64 string, n int) []bool {
	if b64 == "" || n <= 0 {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != (n+7)/8 {
		return nil
	}
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		out[i] = raw[i/8]&(1<<(7-uint(i%8))) != 0
	}
	return out
}

// selectedFileWanted 文件需求位图(文件选择下载的展示口径);files 为空 = 全部。
func selectedFileWanted(meta *bt.MetaInfo, files []string) []bool {
	if len(files) == 0 {
		return nil
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[strings.Join(strings.Split(f, "\\"), "/")] = true
	}
	out := make([]bool, len(meta.Files))
	for i, f := range meta.Files {
		out[i] = set[strings.Join(f.Path, "/")]
	}
	return out
}

// btClampArrived 落盘进度上界钳到任务总量。arrived 按 piece 粒度统计,文件
// 选择任务里跨越边界的 piece 会对选中文件整块计入(其尾部字节属于下一个
// 文件),总和可略超所选文件字节;进度显示不得破百。
func btClampArrived(size, v int64) int64 {
	if size > 0 && v > size {
		return size
	}
	return v
}

// btNameOccupied 判断目标名在下载目录里是否被本任务之外的数据占用。
// 就地布局:本任务自己的数据(位图非空的续传,或目录内带 .!zd 未完成
// 标记的残留)不算占用,续传必须原地名继续;其他 BT 任务的同名数据或
// 外来文件才算占用,首启冲突改名一次。
func (m *Manager) btNameOccupied(t *Task, name string) bool {
	p := filepath.Join(t.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return false // 目标名不存在:随便用
	}
	t.mu.Lock()
	bf := t.Bitfield
	t.mu.Unlock()
	if bf != "" {
		return false // 位图非空:就是本任务续传中的数据
	}
	if m.otherTaskUsesName(t, name) {
		return true // 同名种子的另一个任务占着这个名字
	}
	// 无位图且无其他任务认领:目录带 .!zd 标记视为本任务上次会话的残留
	// (刚开即退出),续用并按位图/扫描校验;裸目录/裸文件是外来数据,改名避开
	return !bt.HasIncompleteMark(p)
}

// otherTaskUsesName 报告是否有别的 BT 任务登记着 (Dir, Filename)。
func (m *Manager) otherTaskUsesName(t *Task, name string) bool {
	td := filepath.Clean(t.Dir)
	ln := strings.ToLower(name)
	for _, v := range m.List() {
		if v.ID == t.ID || v.Kind != KindBT {
			continue
		}
		if filepath.Clean(v.Dir) == td && strings.ToLower(v.Filename) == ln {
			return true
		}
	}
	return false
}

// selectedBytes 汇总所选文件的字节数;files 为空返回全量。
func selectedBytes(meta *bt.MetaInfo, files []string) int64 {
	if len(files) == 0 {
		return meta.TotalLength
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[strings.Join(strings.Split(f, "\\"), "/")] = true
	}
	var total int64
	for _, f := range meta.Files {
		if set[strings.Join(f.Path, "/")] {
			total += f.Length
		}
	}
	return total
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

func allTrue(b []bool) bool {
	for _, v := range b {
		if !v {
			return false
		}
	}
	return true
}
