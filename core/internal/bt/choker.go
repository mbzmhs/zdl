package bt

import (
	"sort"
	"time"
)

// 上传侧 choking 策略(简化 BEP 风格 tit-for-tat):
//
//	下载期——按"对端给我们的下行速率"互惠放行:4 个常规槽 + 1 个乐观槽
//	        (乐观槽 30s 轮换,给从未证明过自己的新面孔机会);
//	做种期——无从互惠,8 个槽按等待时长轮换,每 30s 换血一半,
//	        上传平摊给全部对端而不是砸给最先连上的几个;
//	其余一律 choke。对端不感兴趣时不占槽(它用不上这份上传)。
const (
	chokerSlots      = 4               // 下载期 tit-for-tat 常规上传槽
	chokerOptimistic = 1               // 下载期乐观 unchoke 槽(含在下述"轮换"语义里)
	chokerSeedSlots  = 8               // 做种期上传槽
	chokerSeedHalve  = 4               // 做种换血:一次让位/补位的数量
	chokerTick       = 5 * time.Second // 决策周期
	chokerRotate     = 30 * time.Second
	// 速率分桶迟滞:同一 64KB/s 桶内现任优先,防排名贴着阈值抖动时
	// 反复 choke/unchoke 磨掉对方管线
	chokerRateBucket = 64 << 10
)

// chokerPeer 是决策所需的单 peer 快照(由 chokerOnce 从连接上采样)。
type chokerPeer struct {
	addr       string
	rate       int64     // 近期对端→我们的下行速率 B/s(它给得越多,越该放行)
	interested bool      // 对端想要我们的数据
	unchoked   bool      // 我们当前是否已放行
	unchokeAt  time.Time // 当前放行窗口起点
	chokedAt   time.Time // 我们最后一次 choke 它的时刻(零 = 从未,视为等待最久)
}

// chokerState 是跨轮换周期的状态(乐观槽身份与两个时间窗起点)。
type chokerState struct {
	optAddr   string
	optSince  time.Time
	seedSince time.Time
}

// chokerDecision 是一轮决策:把每个 peer 完整划分到放行/阻塞两侧。
// 调用方按差量应用,幂等。
type chokerDecision struct {
	unchoke []string
	choke   []string
	state   chokerState
}

// decideChokes 纯决策(不碰连接,便于单测)。seed 走做种轮换,否则 tit-for-tat。
func decideChokes(peers []chokerPeer, seed bool, st chokerState, now time.Time) chokerDecision {
	dec := chokerDecision{state: st}
	var eligible []chokerPeer
	for _, p := range peers {
		if p.interested {
			eligible = append(eligible, p)
		} else if p.unchoked {
			dec.choke = append(dec.choke, p.addr) // 失去兴趣:让出槽位
		}
	}
	if seed {
		dec.applySeed(eligible, now)
	} else {
		dec.applyDownload(eligible, now)
	}
	return dec
}

// applyDownload 下载期:速率前4 常规槽 + 1 个乐观槽。
func (d *chokerDecision) applyDownload(eligible []chokerPeer, now time.Time) {
	// 排序:速率分桶降序 → 现任优先(桶内) → 桶内速率降序 → 同类平手:
	// 现任按服务最久留任(零速率 peer 集体同桶时防每 tick 乱序换血),
	// 阻塞者按等待最久补位。
	sort.SliceStable(eligible, func(i, j int) bool {
		bi, bj := eligible[i].rate/chokerRateBucket, eligible[j].rate/chokerRateBucket
		if bi != bj {
			return bi > bj
		}
		if eligible[i].unchoked != eligible[j].unchoked {
			return eligible[i].unchoked
		}
		if eligible[i].rate != eligible[j].rate {
			return eligible[i].rate > eligible[j].rate
		}
		if eligible[i].unchoked {
			return eligible[i].unchokeAt.Before(eligible[j].unchokeAt)
		}
		return eligible[i].chokedAt.Before(eligible[j].chokedAt)
	})
	slots := chokerSlots
	if slots > len(eligible) {
		slots = len(eligible)
	}
	inTop := make(map[string]bool, slots)
	for i := 0; i < slots; i++ {
		inTop[eligible[i].addr] = true
		d.unchoke = append(d.unchoke, eligible[i].addr)
	}
	// 乐观槽:身份仍有效(还在 eligible、未进常规槽、未到期)则保留,否则换人
	opt := d.state.optAddr
	valid := false
	if opt != "" && now.Sub(d.state.optSince) < chokerRotate {
		for i := range eligible {
			if eligible[i].addr == opt && !inTop[opt] {
				valid = true
				break
			}
		}
	}
	if !valid {
		opt = ""
		var bestAddr string
		var bestAt time.Time
		for i := range eligible {
			p := &eligible[i]
			if inTop[p.addr] || p.unchoked { // 只从阻塞中的挑(新面孔 chokedAt 为零,最优先)
				continue
			}
			if bestAddr == "" || p.chokedAt.Before(bestAt) {
				bestAddr, bestAt = p.addr, p.chokedAt
			}
		}
		opt = bestAddr
	}
	if opt != d.state.optAddr {
		d.state.optAddr = opt
		d.state.optSince = now
	}
	if opt != "" {
		d.unchoke = append(d.unchoke, opt)
	}
	d.splitRemaining(eligible)
}

// applySeed 做种期:8 满位轮换,空位补等待最久,满位且有人等时每轮换血一半。
func (d *chokerDecision) applySeed(eligible []chokerPeer, now time.Time) {
	wanted := make(map[string]bool, len(eligible))
	var up, down []chokerPeer
	for _, p := range eligible {
		if p.unchoked {
			up = append(up, p)
		} else {
			down = append(down, p)
		}
	}
	// 服务最久在前 / 等待最久在前(chokedAt 零 = 从未放行,最优先)
	sort.SliceStable(up, func(i, j int) bool { return up[i].unchokeAt.Before(up[j].unchokeAt) })
	sort.SliceStable(down, func(i, j int) bool { return down[i].chokedAt.Before(down[j].chokedAt) })

	for _, p := range up {
		wanted[p.addr] = true
	}
	fill := chokerSeedSlots - len(up)
	if fill < 0 {
		fill = 0
	}
	if fill > len(down) {
		fill = len(down)
	}
	for i := 0; i < fill; i++ {
		wanted[down[i].addr] = true
	}
	down = down[fill:]
	if len(up)+fill >= chokerSeedSlots {
		if d.state.seedSince.IsZero() {
			d.state.seedSince = now // 刚满员:换血窗从现在起算
		}
	} else {
		d.state.seedSince = now // 未满:窗重新起算,满员满 30s 才换血
	}
	// 满员且仍有人等:到期换血——服务最久的让位,等待最久的顶上
	if len(up) >= chokerSeedSlots && len(down) > 0 &&
		now.Sub(d.state.seedSince) >= chokerRotate {
		rot := chokerSeedHalve
		if rot > len(up) {
			rot = len(up)
		}
		for i := 0; i < rot; i++ {
			delete(wanted, up[i].addr)
		}
		if rot > len(down) {
			rot = len(down)
		}
		for i := 0; i < rot; i++ {
			wanted[down[i].addr] = true
		}
		d.state.seedSince = now
	}
	for _, p := range eligible {
		if wanted[p.addr] {
			d.unchoke = append(d.unchoke, p.addr)
		} else if p.unchoked {
			d.choke = append(d.choke, p.addr)
		}
	}
}

// splitRemaining 下载期收尾:未入选却仍在放行中的 eligible 进入阻塞名单。
func (d *chokerDecision) splitRemaining(eligible []chokerPeer) {
	will := make(map[string]bool, len(d.unchoke))
	for _, a := range d.unchoke {
		will[a] = true
	}
	for _, p := range eligible {
		if !will[p.addr] && p.unchoked {
			d.choke = append(d.choke, p.addr)
		}
	}
}

// kickChokerNow 事件踢醒 choker(interested 变化/peer 断开/下载完成),
// 槽位变化立即生效,不等下个周期。
func (c *Client) kickChokerNow() {
	select {
	case c.kickChoker <- struct{}{}:
	default:
	}
}

// chokerLoop 周期 + 事件驱动的 choking 决策。
func (c *Client) chokerLoop() {
	defer c.wg.Done()
	tk := time.NewTicker(chokerTick)
	defer tk.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-tk.C:
		case <-c.kickChoker:
		}
		c.chokerOnce()
		c.leechSweep(time.Now()) // 只取不予扫描:O(peers) 原子读,搭 choker 周期顺风车
	}
}

// chokerOnce 采样全部连接 → 纯决策 → 应用差量。
// chokerSt 仅本协程(或直接调用本方法的测试)访问。
func (c *Client) chokerOnce() {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()

	now := time.Now()
	snap := make([]chokerPeer, 0, len(list))
	byAddr := make(map[string]*peerConn, len(list))
	for _, pc := range list {
		interested, unchoked, uAt, cAt := pc.chokerView()
		snap = append(snap, chokerPeer{
			addr:       pc.addr,
			rate:       pc.chokerSample(now),
			interested: interested,
			unchoked:   unchoked,
			unchokeAt:  uAt,
			chokedAt:   cAt,
		})
		byAddr[pc.addr] = pc
	}
	dec := decideChokes(snap, c.completed.Load(), c.chokerSt, now)
	c.chokerSt = dec.state
	if len(dec.unchoke) > 0 || len(dec.choke) > 0 {
		btDebug("choker unchoke=%v choke=%v", dec.unchoke, dec.choke)
	}
	for _, a := range dec.unchoke {
		if pc := byAddr[a]; pc != nil {
			pc.setUnchoke(true)
		}
	}
	for _, a := range dec.choke {
		if pc := byAddr[a]; pc != nil {
			pc.setUnchoke(false)
		}
	}
}
