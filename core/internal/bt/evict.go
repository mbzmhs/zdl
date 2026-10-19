package bt

import (
	"log"
	"sort"
	"time"
)

// 劣质 peer 淘汰:连接槽位是稀缺资源(MaxPeers),零贡献的连接——既不给
// 我们数据、我们也没在上传——白占位置,挡住候选池里可能更好的对端。
// 规则分档,宁可多留不误杀;被淘汰者经候选池回补重拨(60s 退避),
// 不会立刻原样弹回。
const (
	evictZeroDur = 150 * time.Second // 双向零贡献淘汰门槛
	evictSnub    = 5 * time.Minute   // 被 choke 且零下行的淘汰门槛(下载中才判)
	// evictNoBitfield:接通后仍迟迟收不到 bitfield 的连接容忍窗。对端连持有
	// 图都不通告,我们既无法向其请求任何块,也无从判断它能给什么——实测
	// DHT 广撒网拉来的大量空 qBittorrent 连接正是这种形态(52 条里 40 条无
	// bitfield、零收发),旧实现把"bitfield 未到"当作有价值而给 10 分钟长窗,
	// 结果是它们长期占满连接池,真正能供数据的种子被挡在门外。
	evictNoBitfield = 60 * time.Second
	// evictUselessBitfield:已通告 bitfield、但其中没有任何我们仍缺的 piece
	// 的连接。它对我们的下载进度零价值(只可能被我们上传),短窗清退把名额
	// 让给候选池里可能持有稀缺 piece 的 peer。
	evictUselessBitfield = 90 * time.Second
	// evictNeededWindow:持有我们所需 piece 的 peer 的零流量容忍窗。
	// 肥沃 swarm 上种子放行(unchoke)排队常超 5 分钟——它对我们零流量
	// 只说明还没轮到,短窗淘汰会在放行前杀掉连接,永远等不到数据
	// (表现为"整场只有个别 peer 在传")。
	evictNeededWindow = 10 * time.Minute
)

// junkShareNum/junkShareDen:双向零贡献连接占连接池的比例上限(超出即从
// 最老的开始清退)。即使单条规则都还没到点,也不允许空转连接独占池子——
// 它们只是"尚未到期",但已经挡住了候选池里的新 peer。
const (
	junkShareNum = 1
	junkShareDen = 2
)

// evictPeer 是淘汰评估所需的连接快照。
type evictPeer struct {
	age        time.Duration // 连接已存活时长
	down, up   int64         // 本连接累计收/发字节
	interested bool          // 对端想要我们的数据(保留参考;当前规则不依赖)
	choked     bool          // 对端把我们 choke(不给数据)
	hasNeeded  bool          // 对端持有我们仍缺的 piece
	bfKnown    bool          // 已收到 bitfield/haveall/havenone(持有图已知)
}

// evictReason 返回淘汰原因,空串 = 保留。completed 时跳过 choke 判定:
// 做种期我们不下载,"被 choke" 无意义,而 up>0 的下载者永远过不了零贡献门槛。
func evictReason(p evictPeer, completed bool) string {
	if p.age < evictZeroDur {
		return ""
	}
	if !completed && p.hasNeeded {
		// 有数据可换的 peer:零流量只意味着还没被放行,只适用长窗。
		// (hasNeeded 仅在已收到 bitfield 时才会被算出,见 evictPass。)
		if p.age >= evictNeededWindow && p.down == 0 {
			return "10 分钟零下行(有需 piece)"
		}
		return ""
	}
	if p.down == 0 && p.up == 0 {
		return "150s 双向零贡献"
	}
	if !completed && p.age >= evictSnub && p.choked && p.down == 0 {
		return "被 choke 5 分钟且零下行"
	}
	return ""
}

// shortEvictReason 是零贡献/无价值连接的短窗淘汰,只针对"未证实有价值"的连接:
//   - 尚未通告 bitfield:连持有图都没有,绝对无法供数(旧实现的长窗保护即漏洞);
//   - 已通告 bitfield 但不含任何我们所需 piece:对本任务下载零价值。
//
// 两者都要求未在上传(up==0):若我们正在向它上传,连接至少还有上传价值,
// 不在此规则内清退(留给常规零贡献/做种规则)。
func shortEvictReason(p evictPeer, completed bool) string {
	if completed || p.up != 0 || p.down != 0 {
		// 有任一方向流量:交给常规规则(长窗/做种规则)判定,短窗不动。
		return ""
	}
	if !p.bfKnown {
		if p.age >= evictNoBitfield {
			return "60s 未收到 bitfield 且零上行"
		}
		return ""
	}
	if !p.hasNeeded && p.age >= evictUselessBitfield {
		return "90s 无所需 piece 且零下行"
	}
	return ""
}

// evictLoop 定期做淘汰评估。周期短于最短门槛,让空转连接尽快让位。
func (c *Client) evictLoop() {
	defer c.wg.Done()
	tk := time.NewTicker(30 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-tk.C:
		}
		c.evictPass()
	}
}

// evictPass 评估全部连接并断开零贡献者,返回断开数。
// 被断开者走 cleanup → removePeer → kickChokerNow,槽位立即补位。
func (c *Client) evictPass() int {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()

	now := time.Now()
	completed := c.completed.Load()
	have := c.picker.Have()
	type cand struct {
		pc     *peerConn
		p      evictPeer
		reason string
	}
	cands := make([]cand, 0, len(list))
	for _, pc := range list {
		pc.mu.Lock()
		p := evictPeer{
			age:        now.Sub(pc.connectedAt),
			down:       pc.downTotal.Load(),
			up:         pc.upTotal.Load(),
			interested: pc.peerInterested,
			choked:     pc.peerChoke,
			bfKnown:    pc.bitfield != nil,
		}
		if !completed && pc.bitfield != nil {
			for i, b := range pc.bitfield {
				if b && i < len(have) && !have[i] {
					p.hasNeeded = true
					break
				}
			}
		}
		pc.mu.Unlock()
		reason := shortEvictReason(p, completed)
		if reason == "" {
			reason = evictReason(p, completed)
		}
		cands = append(cands, cand{pc: pc, p: p, reason: reason})
	}

	// 零贡献连接占比上限:未到点的空转连接也不允许独占池子。按连接时长
	// 从老到新排序,超出上限的最老者先走;仍在规则内保留的年轻连接不动
	// (刚连上可能还没走完握手/放行)。
	killed := map[*peerConn]bool{}
	n := 0
	kill := func(pc *peerConn, reason string) {
		if killed[pc] {
			return
		}
		killed[pc] = true
		pc.mu.Lock()
		p := evictPeer{
			age:    now.Sub(pc.connectedAt),
			down:   pc.downTotal.Load(),
			up:     pc.upTotal.Load(),
			bfKnown: pc.bitfield != nil,
		}
		pc.mu.Unlock()
		log.Printf("[bt] 淘汰 peer %s(%s): %s,下行 %d 上行 %d,连接 %s",
			pc.addr, ClientName(pc.id), reason, p.down, p.up, p.age.Truncate(time.Second))
		pc.shutdown()
		n++
	}

	for _, cd := range cands {
		if cd.reason != "" {
			kill(cd.pc, cd.reason)
		}
	}

	// 占比上限只对"规则内仍保留"的零贡献连接生效。
	if !completed {
		maxJunk := c.cfg.MaxPeers * junkShareNum / junkShareDen
		if maxJunk < 4 {
			maxJunk = 4
		}
		var junk []cand
		for _, cd := range cands {
			if killed[cd.pc] || cd.p.hasNeeded {
				continue
			}
			if cd.p.down == 0 && cd.p.up == 0 {
				junk = append(junk, cd)
			}
		}
		if len(junk) > maxJunk {
			sort.Slice(junk, func(i, j int) bool { return junk[i].p.age > junk[j].p.age })
			for _, cd := range junk[:len(junk)-maxJunk] {
				kill(cd.pc, "连接池零贡献占比超限")
			}
		}
	}
	return n
}