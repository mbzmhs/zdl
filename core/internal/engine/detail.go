package engine

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zdownloader/internal/bt"
)

// TaskDetailView 是任务详情侧边栏的快照:基础视图 + BT 运行时信息。
type TaskDetailView struct {
	TaskView
	Trackers []bt.TrackerState `json:"trackers"` // 运行时会话的 tracker 状态(未运行则空)
	// TrackerStat tracker 汇总(总数/成功数/发现的去重 peer 数),
	// 长列表场景下侧边栏优先展示这一行
	TrackerStat *TrackerStat  `json:"trackerStat,omitempty"`
	Peers       []bt.PeerStat `json:"peers"`                 // 当前连接
	Files       []bt.FileProg `json:"files"`                 // 文件进度(BT 多文件/单文件)
	Seeding     bool          `json:"seeding"`               // 当前处于做种会话
	DHTNodes    int           `json:"dhtNodes,omitempty"`    // 共享 DHT 路由表节点数(虚拟行)
	PEXReceived int32         `json:"pexReceived,omitempty"` // PEX 累计发现 peer 数(虚拟行)
	Banned      []string      `json:"banned,omitempty"`      // 用户封锁的 peer IP 名单
}

// TrackerStat 汇总 tracker 列表的关键指标。
type TrackerStat struct {
	Total int `json:"total"` // tracker 总数
	OK    int `json:"ok"`    // 最近一次通告成功的数量
	Peers int `json:"peers"` // 通告发现的去重 peer 数
}

// summarizeTrackers 汇总 tracker 状态。
func summarizeTrackers(ts []bt.TrackerState) *TrackerStat {
	if ts == nil {
		return nil
	}
	st := &TrackerStat{Total: len(ts)}
	seen := map[string]bool{}
	for _, tr := range ts {
		if tr.LastErr == "" && tr.LastOK != "" {
			st.OK++
		}
		for _, p := range tr.RecentPeers {
			if !seen[p] {
				seen[p] = true
				st.Peers++
			}
		}
	}
	return st
}

// TaskDetail 组装详情快照;BT 会话运行中时附带 tracker/peer/文件信息。
func (m *Manager) TaskDetail(id string) (*TaskDetailView, error) {
	t := m.get(id)
	if t == nil {
		return nil, ErrNotFound
	}
	v := t.View() // View 自带锁,勿在其之上再持 t.mu
	t.mu.Lock()
	cl := t.btcl
	kind, tpath, bitfield := t.Kind, t.TorrentPath, t.Bitfield
	wantedFiles := append([]string(nil), t.WantedFiles...)
	t.mu.Unlock()

	d := &TaskDetailView{TaskView: v}
	if cl != nil {
		d.Trackers = cl.TrackerStates()
		d.TrackerStat = summarizeTrackers(d.Trackers)
		d.Peers = cl.PeerStats()
		d.Files = cl.FileProgress()
		d.Seeding = cl.Completed()
		d.DHTNodes = m.dhtNodeCount()
		d.PEXReceived = cl.PEXReceived()
		d.Banned = cl.BannedIPs()
	} else if kind == KindBT && tpath != "" {
		// 会话已结束:从归档种子 + 持久化位图推算文件进度
		if raw, err := os.ReadFile(tpath); err == nil {
			if meta, perr := bt.ParseTorrent(raw); perr == nil {
				have := btDecodeBitfield(bitfield, meta.PieceCount())
				d.Files = bt.FileProgressOf(meta, have, selectedFileWanted(meta, wantedFiles))
			}
		}
	}
	return d, nil
}

// ---------- 任务级 peer 操作 ----------

// BanTaskPeer 封锁一个 peer(按其 IP):写入全局封禁名单(广播断开各
// 会话现存连接并落盘),此后所有任务的拨号与入站一律拒绝。
func (m *Manager) BanTaskPeer(id, addr string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	cl := t.btcl
	if cl == nil {
		return fmt.Errorf("会话未运行")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("非法 peer 地址 %q", addr)
	}
	cl.BanIP(host)
	m.bump()
	return nil
}

// KickTaskPeer 断开指定 peer 连接(不封锁;对端可能被重新发现并重连)。
func (m *Manager) KickTaskPeer(id, addr string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	cl := t.btcl
	if cl == nil {
		return fmt.Errorf("会话未运行")
	}
	if !cl.KickPeer(addr) {
		return fmt.Errorf("连接不存在或已断开")
	}
	m.bump()
	return nil
}

// AddTaskPeers 手动添加 peer 地址(ip:port 列表):候选池入池并立即拨号。
// 用于直接连接从其他客户端(如 BitComet)连接列表复制来的地址。
func (m *Manager) AddTaskPeers(id string, addrs []string) (int, error) {
	t := m.get(id)
	if t == nil {
		return 0, ErrNotFound
	}
	cl := t.btcl
	if cl == nil {
		return 0, fmt.Errorf("会话未运行")
	}
	n := cl.AddManualPeers(addrs)
	if n == 0 {
		return 0, fmt.Errorf("没有可用的地址(需 ip:port 格式)")
	}
	m.bump()
	return n, nil
}

// BannedIPs 任务会话的封锁名单(展示用;未运行返回空)。
func (m *Manager) BannedIPs(id string) []string {
	t := m.get(id)
	if t == nil || t.btcl == nil {
		return nil
	}
	return t.btcl.BannedIPs()
}

// ---------- 全局封禁名单(设置页查看/解除) ----------

// Bans 全局封禁记录(按封禁时间倒序)。
func (m *Manager) Bans() []bt.BanEntry { return m.banList.List() }

// BanRemove 解除一条封禁记录;返回是否存在。
func (m *Manager) BanRemove(ip string) bool { return m.banList.Remove(ip) }

// BansClear 清空封禁名单,返回清除条数。
func (m *Manager) BansClear() int { return m.banList.Clear() }

// BanClientName 按客户端名封禁全部运行中 BT 会话里的匹配 peer(连接详情
// 右键「封禁所有该客户端」):每个会话写入全局封禁名单并断开其连接,并把
// 完整客户端名并入持久客户端黑名单(之后任意 IP 出现该客户端握手期即自动
// 封禁);返回去重后的封禁 IP 数。
func (m *Manager) BanClientName(name string) int {
	seen := map[string]bool{}
	for _, t := range m.snapshotTasks() {
		t.mu.Lock()
		btcl := t.btcl
		t.mu.Unlock()
		if btcl == nil {
			continue
		}
		for _, ip := range btcl.BanClient(name) {
			seen[ip] = true
		}
	}
	// 把完整客户端名并入持久客户端黑名单(落盘 + 对运行中会话热生效):
	// 此前只封了现存同名 IP,换个 IP / 下次再出现就不会被自动封。
	m.AddClientBlacklistPattern(bt.ClientBlacklistPattern(name))
	return len(seen)
}

// UpdateClientBlacklist 更新客户端黑名单(设置模态窗保存):归一后写回
// 配置并整体热同步(UpdateConfig 内部落盘 + 对运行中会话 SetClientBlacklist,
// 现存连接命中即封)。返回落库后的最终模式表。
func (m *Manager) UpdateClientBlacklist(patterns []string) []string {
	cfg := m.cfg
	cfg.BT.ClientBlacklist = patterns
	if err := m.UpdateConfig(cfg); err != nil {
		log.Printf("[engine] 客户端黑名单保存失败: %v", err)
		return m.cfg.BT.ClientBlacklist
	}
	return m.cfg.BT.ClientBlacklist
}

// AddClientBlacklistPattern 把一个客户端黑名单模式去重并入持久黑名单:命中
// 已存在(大小写不敏感)则幂等跳过,否则追加后走 UpdateClientBlacklist 落盘
// 并对运行中会话热生效(现存命中即封)。空模式忽略。右键「封禁所有该客户端」
// 据此把完整客户端名记入黑名单,使之后任意 IP 出现该客户端都会被自动封禁。
func (m *Manager) AddClientBlacklistPattern(pattern string) {
	if strings.TrimSpace(pattern) == "" {
		return
	}
	cfg := m.cfg
	for _, p := range cfg.BT.ClientBlacklist {
		if strings.EqualFold(strings.TrimSpace(p), pattern) {
			return // 已在名单,幂等
		}
	}
	merged := make([]string, 0, len(cfg.BT.ClientBlacklist)+1)
	merged = append(merged, cfg.BT.ClientBlacklist...)
	merged = append(merged, pattern)
	m.UpdateClientBlacklist(merged)
}

// ---------- 任务级 tracker 管理 ----------

// AddTaskTracker 追加任务级 tracker;会话运行中则热加入 announce。
// AddTaskTracker 添加任务级 tracker。返回是否真正加入:
// 同 host:port(不同协议 scheme)已存在时视为重复,不重复入库。
func (m *Manager) AddTaskTracker(id, url string) (bool, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return false, fmt.Errorf("tracker 地址为空")
	}
	t := m.get(id)
	if t == nil {
		return false, ErrNotFound
	}
	t.mu.Lock()
	// 同 host:port(不同协议 scheme)视为重复:与客户端侧 AddTracker 的
	// 去重口径一致,避免 http://x:8000 与 udp://x:8000 并存
	key := bt.TrackerKey(url)
	has := false
	for _, u := range t.Trackers {
		if bt.TrackerKey(u) == key {
			has = true
		}
	}
	if !has {
		t.Trackers = append(t.Trackers, url)
	}
	cl := t.btcl
	t.mu.Unlock()
	if has {
		return false, nil
	}
	m.persist(t)
	m.bump()
	if cl != nil {
		cl.AddTracker(url)
	}
	return true, nil
}

// ReseedTask 让已完成的 BT 任务重新开始做种:校验数据仍在后置为等待,
// 调度器拾取后 runBT 走"位图满 + 文件存在 → 做种恢复"捷径。
func (m *Manager) ReseedTask(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	kind, st, fn, dir := t.Kind, t.Status, t.Filename, t.Dir
	t.mu.Unlock()
	if kind != KindBT {
		return fmt.Errorf("仅 BT 任务支持重新做种")
	}
	if st != StatusCompleted {
		return fmt.Errorf("仅已完成的任务可重新做种")
	}
	if fn == "" {
		return fmt.Errorf("任务文件名缺失")
	}
	if _, err := os.Stat(filepath.Join(dir, fn)); err != nil {
		return fmt.Errorf("数据文件不存在或已被移动,无法做种")
	}
	t.mu.Lock()
	if t.Status != StatusCompleted { // 并发竞态防御:状态已被别人改走
		t.mu.Unlock()
		return nil
	}
	t.Status = StatusWaiting
	t.SeedStarted = time.Time{}
	t.mu.Unlock()
	m.bump()
	m.kick()
	return nil
}

// DedupeTaskTrackers 去重任务 tracker:同 host:port(不同协议 scheme)// 每组保留"最近通告成功"的一个,删除其余(同步任务持久化列表与会话)。
// 返回被删的 URL 列表;无重复时返回空。
func (m *Manager) DedupeTaskTrackers(id string) ([]string, error) {
	t := m.get(id)
	if t == nil {
		return nil, ErrNotFound
	}
	cl := t.btcl
	var removed []string
	if cl != nil {
		removed = cl.DedupeTrackers()
	} else {
		// 会话未运行:按等价键对持久化列表去重(每组保留首个)
		t.mu.Lock()
		seen := map[string]bool{}
		kept := t.Trackers[:0]
		for _, u := range t.Trackers {
			k := bt.TrackerKey(u)
			if seen[k] {
				removed = append(removed, u)
				continue
			}
			seen[k] = true
			kept = append(kept, u)
		}
		t.Trackers = kept
		t.mu.Unlock()
	}
	if len(removed) > 0 {
		t.mu.Lock()
		gone := make(map[string]bool, len(removed))
		for _, u := range removed {
			gone[u] = true
		}
		kept := t.Trackers[:0]
		for _, u := range t.Trackers {
			if !gone[u] {
				kept = append(kept, u)
			}
		}
		t.Trackers = kept
		t.mu.Unlock()
		m.persist(t)
		m.bump()
	}
	return removed, nil
}

// RemoveTaskTracker 移除任务级 tracker;会话运行中则热移除(发送 stopped)。
func (m *Manager) RemoveTaskTracker(id, url string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	kept := t.Trackers[:0]
	for _, u := range t.Trackers {
		if u != url {
			kept = append(kept, u)
		}
	}
	t.Trackers = kept
	cl := t.btcl
	t.mu.Unlock()
	m.persist(t)
	m.bump()
	if cl != nil {
		cl.RemoveTracker(url)
	}
	return nil
}

// ReannounceTask 触发任务立即重新通告全部 tracker。
func (m *Manager) ReannounceTask(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	cl := t.btcl
	t.mu.Unlock()
	if cl == nil {
		return fmt.Errorf("任务未在运行,无法通告")
	}
	cl.Reannounce()
	return nil
}
