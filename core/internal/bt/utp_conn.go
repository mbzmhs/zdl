package bt

import (
	"errors"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"
)

// utp 状态机(规范 §2.4:合并 libutp/libtorrent)。
const (
	utpStateNone = iota
	utpStateSynSent
	utpStateConnected
	utpStateFinSent // 写方向已关,仍可收
	utpStateClosed  // 双向终结
	utpStateReset
)

const (
	utpMSS        = 1416      // 载荷上限(1436 - 20 头;不做 MTU 探测的固定值)
	utpRecvCap    = 256 << 10 // 接收缓冲上限(= 最大广告窗口)
	utpReorderMax = 1024      // 重组缓冲容量(BEP-29 乱序上限)
	utpTickMs     = 50        // 发送循环周期(ms)
	utpAckDelay   = 25        // 延迟 ack 冲刷上限(ms,TR 25~75ms 口径取下限:
	// 收包侧每多等一拍,发送端的慢启动就少一轮翻倍——对端向我们上传的
	// 爬坡速度直接受此值钳制)
	utpFinResends = 2    // FIN 重发次数
	utpSynResends = 2    // SYN 重发次数
	utpConnectTO  = 3000 // 建连总超时(ms)
	// utpKeepalive 空闲保活间隔(ms):BEP-29 §1.2 建议 ~30s 一个 ST_STATE。
	// NAT 的 UDP 映射普遍 30~60s 过期——被 choke 等待放行、等 unchoke 轮换
	// 期间双方都不发数据,映射一过期即双向聋哑(连接在、数据 0)。20s 发一次
	// keepalive 持续刷新映射,并对端可据此判活。
	utpKeepalive = 20 * 1000
	// utpMaxDelaySample 时延样本上限(µs):LED BAT 目标 100ms,真实排队
	// 时延超过 1s 的样本只可能是时钟漂移/对端实现缺陷,一律视为垃圾。
	utpMaxDelaySample = 1_000_000
)

var (
	errUTPClosed = errors.New("uTP 连接已关闭")
	errUTPReset  = errors.New("uTP 连接被对端重置")
)

// utpClock 为 uTP 提供单调微秒时戳(32 位自然回绕,全用回绕运算)。
var utpClockStart = time.Now()

func utpNowUS() uint32 { return uint32(time.Since(utpClockStart).Microseconds()) }
func utpNowMS() uint64 { return uint64(time.Since(utpClockStart).Milliseconds()) }

// utpOutPkt 是在途/待重传的出站包(消息语义:重传复用原包原 seq 原 payload)。
type utpOutPkt struct {
	pktType byte
	seq     uint16
	payload []byte
	sentAt  uint64 // 最近发送时刻(ms)
	sends   int
	firstUS uint32 // 首次发送时戳(Karn:重发不再取 RTT)
}

// utpConn 是一条 uTP 连接,实现 net.Conn。manager 读协程调用 input,
// senderLoop 驱动 SYN/FIN 重传与延迟 ack,Read/Write 与它们 cond 同步。
type utpConn struct {
	mgr    *utpManager
	addr   *net.UDPAddr
	sendID uint16 // 发包用
	recvID uint16 // 收包校验用

	mu    sync.Mutex
	cond  *sync.Cond
	state int

	createdMs uint64 // 建连起点(超时判定)
	seqNr     uint16 // 下一个要发的 seq
	ackNr     uint16 // 对端方向最后按序收到的 seq(期望下一包 = ackNr+1)
	initSeq   uint16 // 接受方 SYN-ACK 使用的初始 seq(不递增)

	outbuf map[uint16]*utpOutPkt // 在途(seq → 包)
	curWin int64                 // 在途字节

	reorder      map[uint16][]byte // 乱序暂存
	reorderBytes int               // 乱序暂存字节数(计入接收预算,写进窗口广告)
	recvBuf      []byte            // 已按序、等待 Read 的数据
	gotFin       bool
	eofSeq       uint16
	readEOF      bool

	advWnd  uint32 // 对端广告窗口
	replyUS uint32 // 最近单向时延样本(回填发出包 tsDiff)
	ourHist delayHist
	cc      *utpCC
	rtt     utpRTT

	needAck  bool
	ackAt    uint64
	dupAcks  int
	ackedSeq uint16 // 已确认推进到的 seq
	ackedOK  bool
	lastSend uint64 // 最近一次发出任何包的时刻(ms);空闲 keepalive 依据

	readDL  time.Time
	writeDL time.Time

	sendCh   chan struct{}
	closed   chan struct{}
	once     sync.Once
	termDone bool // closed 通道已关闭(terminate 幂等)
}

// terminateLocked 幂等地终结连接(closed 通道只关一次)。持 mu 调用。
func (c *utpConn) terminateLocked() {
	if !c.termDone {
		c.termDone = true
		close(c.closed)
	}
	c.cond.Broadcast()
}

func newUTPConn(mgr *utpManager, addr *net.UDPAddr, sendID, recvID uint16, outbound bool) *utpConn {
	c := &utpConn{
		mgr: mgr, addr: addr, sendID: sendID, recvID: recvID,
		createdMs: utpNowMS(),
		outbuf:    make(map[uint16]*utpOutPkt),
		reorder:   make(map[uint16][]byte),
		advWnd:    utpRecvCap,
		cc:        newUtpCC(utpMSS * 4),
		sendCh:    make(chan struct{}, 1),
		closed:    make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)
	if outbound {
		c.state = utpStateSynSent
	} else {
		c.state = utpStateConnected
	}
	c.seqNr = uint16(rand.Uint32() & 0xFFFF) // 实现口径:随机初值(BEP 写 1)
	go c.senderLoop()
	return c
}

func (c *utpConn) wake() {
	select {
	case c.sendCh <- struct{}{}:
	default:
	}
}

// ---- 发送原语(调用方持 mu) ---------------------------------------------

// writePkt 构造并写出一个包。ext/sack 用于 SACK 扩展(二者同时用)。
func (c *utpConn) writePkt(pktType, ext byte, seq uint16, payload, sack []byte) error {
	if os.Getenv("ZD_UTP_DEBUG") != "" {
		// 注意:调用方多已持有 c.mu,这里不得再加锁(非重入,会死锁)
		log.Printf("[utp-dbg] %p(%s) 发包 type=%d seq=%d ack=%d len=%d", c, c.addr, pktType, seq, c.ackNr, len(payload))
	}
	var h utpHeader
	h.pktType = pktType
	h.ver = utpVersion
	h.extension = ext
	h.connID = c.sendID
	c.lastSend = utpNowMS()
	if pktType == utpSyn {
		// 规范 §2.1:SYN 携带的是本端 recv_id(对端以此推得两端 ID)
		h.connID = c.recvID
	}
	h.timestamp = utpNowUS()
	h.tsDiff = c.replyUS
	h.wndSize = c.recvWndLocked() // 0 = 满(含乱序暂存占用)
	h.seqNr = seq
	h.ackNr = c.ackNr

	buf := make([]byte, utpHeaderLen+len(sack)+len(payload))
	h.marshal(buf[:utpHeaderLen])
	copy(buf[utpHeaderLen:], sack)
	copy(buf[utpHeaderLen+len(sack):], payload)
	_, err := c.mgr.pc.WriteTo(buf, c.addr)
	return err
}

// sendState 发纯 ACK(不推进 seq);sack 可为 nil。
func (c *utpConn) sendState(sack []byte) error {
	var ext byte
	if len(sack) > 0 {
		ext = 1
	}
	return c.writePkt(utpState, ext, c.seqNr, nil, sack)
}

// recvWndLocked 当前接收窗口广告值 = 预算 − (recvBuf + 乱序暂存)。
// 乱序段计入占用:它们也会在缺口补齐时一次性涌入 recvBuf,不计数会让
// 发送方在乱序场景拿到虚高窗口而超发。须持 c.mu。
func (c *utpConn) recvWndLocked() uint32 {
	if used := len(c.recvBuf) + c.reorderBytes; used < utpRecvCap {
		return uint32(utpRecvCap - used)
	}
	return 0
}

// sendOutPkt 首发或重发一个在途包。
func (c *utpConn) sendOutPkt(p *utpOutPkt) {
	err := c.writePkt(p.pktType, 0, p.seq, p.payload, nil)
	if err != nil {
		// 网络错误(如 Windows ICMP 触发的 WSAECONNRESET):包未出网,
		// 必须保持可重传——记 sentAt 使其进入超时扫描,不得静默丢弃。
		if os.Getenv("ZD_UTP_DEBUG") != "" {
			log.Printf("[utp-dbg] %p(%s) 发送失败 seq=%d err=%v", c, c.addr, p.seq, err)
		}
		p.sentAt = utpNowMS()
		return
	}
	p.sentAt = utpNowMS()
	p.sends++
}

// sendFinLocked 发 FIN(消耗 seq、进重发窗口——libutp 口径;接收侧对
// 不消耗 seq 的实现同样兼容)。
func (c *utpConn) sendFinLocked() {
	p := &utpOutPkt{pktType: utpFin, seq: c.seqNr, firstUS: utpNowUS()}
	c.outbuf[p.seq] = p
	c.seqNr++
	c.curWin += utpHeaderLen
	c.sendOutPkt(p)
}

// ---- 包判定与输入 -------------------------------------------------------

// isUTPPacket 判定 UDP 载荷是否像 uTP(共享端口分流;多数派口径
// type<5 && ver==1 && ext<3)。
func isUTPPacket(b []byte) bool {
	if len(b) < utpHeaderLen {
		return false
	}
	if b[0]>>4 > utpSyn || b[0]&0x0F != utpVersion || b[1] >= 3 {
		return false
	}
	return true
}

// input 处理一个完整入包 datagram(manager 读协程调用,连接表已命中):
// 内部完成头解析、扩展链剥离,DATA 载荷不含扩展字节。
func (c *utpConn) input(pkt []byte, nowMs uint64) {
	h, err := parseUTPHeader(pkt)
	if err != nil {
		return
	}
	if os.Getenv("ZD_UTP_DEBUG") != "" {
		log.Printf("[utp-dbg] %p(%s) 收包 type=%d seq=%d ack=%d len=%d", c, c.addr, h.pktType, h.seqNr, h.ackNr, len(pkt)-utpHeaderLen)
	}
	rest := pkt[utpHeaderLen:]
	exts, extN, err := parseUTPExts(rest, h.extension)
	if err != nil {
		exts, extN = nil, 0
	}
	payload := rest[extN:]

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == utpStateReset || c.state == utpStateClosed {
		return
	}
	if h.pktType != utpSyn && h.connID != c.recvID {
		return
	}

	// 时延测量(方向见规范 §6.2):
	//   their_delay = now - pkt.ts → replyUS 回填下个发出包;
	//   pkt.tsDiff  = 对端算出的本端方向排队时延 → our_hist 驱动 cwnd。
	// 双向都要防垃圾样本:时钟漂移或坏实现会产生秒级~小时级差值,原样
	// 采信会把 cwnd 砸到下限卡死(LED BAT 目标仅 100ms),原样回显也会
	// 毒化对端(表现为对端发包间隔退避到分钟级)。超界样本按"无样本"
	// 处理(ts_diff=0),对端有正常样本时不受影响。
	if h.timestamp != 0 {
		their := utpNowUS() - h.timestamp
		if their <= utpMaxDelaySample {
			c.replyUS = their
		} else {
			c.replyUS = 0
		}
	}
	if h.tsDiff != 0 && h.tsDiff != ^uint32(0) && h.tsDiff <= utpMaxDelaySample {
		c.ourHist.addSample(h.tsDiff, nowMs)
	}
	c.advWnd = h.wndSize

	// 发起方建连:SYN-ACK(ST_STATE)且 ack == 我方 SYN 的 seq。
	if c.state == utpStateSynSent {
		if h.pktType == utpState && h.ackNr == (c.seqNr-1)&0xFFFF {
			c.state = utpStateConnected
			c.ackedSeq = h.ackNr // SYN 已被确认
			c.ackedOK = true
			delete(c.outbuf, h.ackNr)
			c.ackNr = (h.seqNr - 1) & 0xFFFF // 接受方 STATE:ack = seq-1
			c.cond.Broadcast()
		}
		return // SYN_SENT 阶段只处理建连;数据等下一轮
	}

	// 注意:ackNr 只能由**按序交付**推进(乱序包进重组缓冲、STATE 的
	// seq 不递增),见 deliverDataLocked;此处仅做对本端出站的确认。
	if h.pktType == utpData || h.pktType == utpFin || h.pktType == utpState {
		c.processAckLocked(h, exts, nowMs)
	}

	switch h.pktType {
	case utpData:
		if os.Getenv("ZD_UTP_DEBUG") != "" {
			log.Printf("[utp-dbg] %p(%s) DATA seq=%d ack=%d len=%d state=%d 我方ackNr=%d recvBuf=%d reorder=%d", c, c.addr, h.seqNr, h.ackNr, len(payload), c.state, c.ackNr, len(c.recvBuf), len(c.reorder))
		}
		dist := (h.seqNr - c.ackNr - 1) & 0xFFFF
		c.deliverDataLocked(h.seqNr, payload)
		if dist != 0 {
			// 乱序/重复:立即 ACK 并附 SACK 位图,对端据此秒级快速重传缺口。
			// 延迟 ack 在流式灌包下 ackAt 持续后移、ack 彻底饿死,对端收不到
			// 重复 ack/SACK 只能干等 RTO(实测一个 3 包缺口停 14 秒)。
			c.needAck = false
			_ = c.sendState(c.sackLocked())
		} else if !c.needAck {
			// 按序:延迟 ack;定时以第一个未确认包起算,不被后续包后移
			c.needAck, c.ackAt = true, nowMs
		}
	case utpFin:
		if !c.needAck {
			c.needAck, c.ackAt = true, nowMs
		}
		if !c.gotFin {
			c.gotFin = true
			c.eofSeq = h.seqNr
		}
		// FIN 按序(数据流恰好收齐到 finSeq-1) → 收下并交付 EOF;
		// 乱序 FIN 等缺口补齐后由 deliver 的连续冲出判定。
		if (h.seqNr-c.ackNr-1)&0xFFFF == 0 {
			c.ackNr = h.seqNr
			c.readEOF = true
		}
		c.cond.Broadcast()
	case utpState:
		// 收 STATE 不回 ack(防风暴),不动 ackNr。
	case utpReset:
		// 校验 ack 不超我方已发最大(防伪造,libtorrent 口径)。
		if !c.ackedOK || ((c.seqNr-h.ackNr)&0xFFFF) <= 0x8000 {
			c.state = utpStateReset
			c.cond.Broadcast()
		}
		return
	}
	c.cond.Broadcast()
}

// processAckLocked 清除被确认的出站包、更新 RTT/CC、dup ack 与丢包。
func (c *utpConn) processAckLocked(h utpHeader, exts []utpExt, nowMs uint64) {
	ack := h.ackNr
	if !c.ackedOK {
		c.ackedSeq = (ack - 1) & 0xFFFF
		c.ackedOK = true
	}
	dist := (ack - c.ackedSeq) & 0xFFFF // 本次确认跨越的 seq 数
	if dist == 0 {
		// dup ack:仅计 ST_STATE 且有在途(规范 §4.3)。
		if h.pktType == utpState && len(c.outbuf) > 0 {
			c.dupAcks++
			if c.dupAcks >= 3 {
				c.retransmitOldestLocked()
				c.cc.onLoss()
				c.dupAcks = 0
			}
		}
		return
	}
	if dist > 0x8000 {
		return // 旧 ack(回退超出容忍)
	}
	if int(dist) > len(c.outbuf)+16 {
		return // 明显伪造的超前 ack(在途根本没这么多)
	}

	ackedBytes := int64(0)
	for i := uint16(1); i <= dist; i++ {
		s := (c.ackedSeq + i) & 0xFFFF
		if p, ok := c.outbuf[s]; ok {
			sz := int64(len(p.payload)) + utpHeaderLen
			ackedBytes += sz
			c.curWin -= sz
			if p.sends == 1 { // Karn:仅首发包取 RTT
				c.rtt.sample(float64(utpNowUS() - p.firstUS))
			}
			delete(c.outbuf, s)
		}
	}
	c.ackedSeq = ack
	if h.pktType != utpState {
		c.dupAcks = 0 // ack 有推进
	}

	// SACK 扩展:逐置位清除并各计 1 个 dup ack。
	if len(exts) > 0 {
		c.handleSACKLocked(exts, ack, &ackedBytes)
	}

	if c.dupAcks >= 3 {
		c.retransmitOldestLocked()
		c.cc.onLoss()
		c.dupAcks = 0
	}

	if ackedBytes > 0 {
		delay := c.ourHist.value()
		if c.rtt.valid {
			// 钳 minRTT(坏样本防护):单向排队时延不可能超过 RTT。
			// srtt 本就是 µs,不得再除以 1000(旧实现单位错位把 delay
			// 恒钳到 ≈0,LEDBAT 的时延退避实际永不触发)
			minRTT := uint32(c.rtt.srtt)
			if minRTT < delay {
				delay = minRTT
			}
		}
		if c.curWin >= int64(c.cc.maxWindow) {
			c.cc.markSaturated(nowMs)
		}
		c.cc.onAck(delay, float64(ackedBytes), utpMSS, nowMs)
		c.cond.Broadcast() // 释放等待窗口的 Write
	}
}

// handleSACKLocked 解析 SACK 扩展:首位 = ack+2,字节内 LSB=小偏移。
func (c *utpConn) handleSACKLocked(exts []utpExt, ack uint16, ackedBytes *int64) {
	base := (ack + 2) & 0xFFFF
	for _, e := range exts {
		if e.typ != 1 || len(e.data) < 4 {
			continue
		}
		for j := 0; j < len(e.data); j++ {
			for k := 0; k < 8; k++ {
				if e.data[j]&(1<<uint(k)) == 0 {
					continue
				}
				s := (base + uint16(j*8+k)) & 0xFFFF
				if p, ok := c.outbuf[s]; ok {
					sz := int64(len(p.payload)) + utpHeaderLen
					*ackedBytes += sz
					c.curWin -= sz
					if p.sends == 1 {
						c.rtt.sample(float64(utpNowUS() - p.firstUS))
					}
					delete(c.outbuf, s)
				}
				c.dupAcks++ // SACK 每置位计 1 dup ack
			}
		}
	}
}

// deliverDataLocked 按序交付:期望 seq=ackNr+1,乱序进重组缓冲。
func (c *utpConn) deliverDataLocked(seq uint16, payload []byte) {
	if c.gotFin && seq != c.eofSeq && seqAfter(c.eofSeq, seq) {
		return // FIN 之后的包一律丢(规范 §3.1)
	}
	// 接收预算 = recvBuf + 乱序暂存。超限说明对端在无视窗口超发(作弊或
	// 我方通告瑕疵)——丢弃本包并立即回 ACK 通告真实窗口,靠对端重传恢复。
	// 绝不能截断 recvBuf"防御":截断会把已交付流的中段静默挖掉,后续
	// 字节全部错位,上层解出的 BT 块必然校验失败——曾据此把 100% 做种的
	// 正常 qBittorrent 误判成 PCDN 毒源(坏块全来自 uTP 连接)。
	if len(c.recvBuf)+len(payload)+c.reorderBytes > utpRecvCap {
		c.needAck = false
		_ = c.sendState(c.sackLocked())
		return
	}
	dist := (seq - c.ackNr - 1) & 0xFFFF
	switch {
	case dist == 0:
		c.recvBuf = append(c.recvBuf, payload...)
		c.ackNr = seq
		for { // 冲出重组缓冲中连续的后续段
			next := (c.ackNr + 1) & 0xFFFF
			b, ok := c.reorder[next]
			if !ok {
				break
			}
			delete(c.reorder, next)
			c.reorderBytes -= len(b)
			c.recvBuf = append(c.recvBuf, b...)
			c.ackNr = next
		}
		if c.gotFin && c.ackNr == c.eofSeq {
			c.readEOF = true
		}
	case dist < utpReorderMax:
		if _, dup := c.reorder[seq]; !dup {
			p := make([]byte, len(payload))
			copy(p, payload)
			c.reorder[seq] = p
			c.reorderBytes += len(p)
		}
	default:
		// 过旧/过远:静默丢
	}
}

// seqAfter 报告环上 a 是否严格在 b 之后且在半环内。
func seqAfter(b, a uint16) bool {
	d := (a - b) & 0xFFFF
	return d > 0 && d < 0x8000
}

// ---- 发送循环(SYN/FIN 重传、延迟 ack) ----------------------------------

func (c *utpConn) senderLoop() {
	tk := time.NewTicker(utpTickMs * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-c.sendCh:
		case <-tk.C:
		}
		nowMs := utpNowMS()
		c.mu.Lock()
		if c.state == utpStateReset || c.state == utpStateClosed {
			c.mu.Unlock()
			return
		}

		// SYN 重发与建连超时。
		if c.state == utpStateSynSent {
			if c.synDue(nowMs) {
				c.sendSynLocked()
				if c.synResends() > utpSynResends && nowMs-c.createdMs > utpConnectTO {
					c.state = utpStateReset // 建连失败:静默终结
					c.cond.Broadcast()
					c.mu.Unlock()
					c.mgr.dropConn(c)
					return
				}
			}
		}

		// 延迟 ack 冲刷(≤50ms)。
		if c.needAck && nowMs-c.ackAt >= utpAckDelay {
			c.needAck = false
			_ = c.sendState(c.sackLocked())
		}

		// 数据/FIN 超时重传。
		if c.state != utpStateSynSent {
			c.retransmitTickLocked(nowMs)
		}

		// 空闲 keepalive:已连上但整 utpKeepalive 没发过任何包(被 choke
		// 等待、无块可请求)时发一个 ST_STATE。持续刷新 NAT 的 UDP 映射,
		// 并让对端确认本端存活;绝不给 FIN/SYN 态发(它们有自己的重传)。
		if c.state == utpStateConnected && nowMs-c.lastSend >= utpKeepalive {
			_ = c.sendState(nil)
		}

		// FIN 收尾:FIN 已被 ack 或重发耗尽,且对端 FIN 也到齐。
		if c.state == utpStateFinSent {
			finAlive := false
			for _, p := range c.outbuf {
				if p.pktType == utpFin {
					finAlive = true
					break
				}
			}
			if (!finAlive && c.gotFin) || c.finResends() > utpFinResends {
				c.state = utpStateClosed
				c.terminateLocked()
				c.mu.Unlock()
				c.mgr.dropConn(c)
				return
			}
		}
		c.mu.Unlock()
	}
}

func (c *utpConn) synResends() int {
	for _, p := range c.outbuf {
		if p.pktType == utpSyn {
			return p.sends
		}
	}
	return 0
}

func (c *utpConn) finResends() int {
	for _, p := range c.outbuf {
		if p.pktType == utpFin {
			return p.sends
		}
	}
	return 0
}

// synDue 判断 SYN 是否到重发时刻(首发立即,之后按 3s 指数退避)。
func (c *utpConn) synDue(nowMs uint64) bool {
	for _, p := range c.outbuf {
		if p.pktType != utpSyn {
			continue
		}
		if p.sends == 0 {
			return true
		}
		to := int64(utpSynRTO) << (p.sends - 1)
		if to > utpMaxRTO {
			to = utpMaxRTO
		}
		return nowMs-p.sentAt >= uint64(to)
	}
	return true // 尚未发过
}

func (c *utpConn) sendSynLocked() {
	p, ok := c.outbuf[c.seqNr]
	if !ok {
		p = &utpOutPkt{pktType: utpSyn, seq: c.seqNr, firstUS: utpNowUS()}
		c.outbuf[c.seqNr] = p
		c.seqNr++ // SYN 消耗 seq(接受方以 ack=S 确认)
	}
	c.sendOutPkt(p)
}

// retransmitTickLocked 检查最老超时包并重发;连续超时按 2^n 退避封顶 60s。
// 不跳过 sends==0(首发即失败的包):它们 sentAt 已记,同样按超时重发。
func (c *utpConn) retransmitTickLocked(nowMs uint64) {
	var oldest *utpOutPkt
	for _, p := range c.outbuf {
		if p.pktType == utpSyn {
			continue
		}
		if oldest == nil || p.sentAt < oldest.sentAt {
			oldest = p
		}
	}
	if oldest == nil {
		return
	}
	to := c.rtt.timeoutMs()
	if oldest.sends > 1 {
		to <<= (oldest.sends - 1)
		if to > utpMaxRTO {
			to = utpMaxRTO
		}
	}
	if nowMs-oldest.sentAt >= uint64(to) {
		c.sendOutPkt(oldest)
		c.cc.onTimeout(float64(c.curWin))
		c.cond.Broadcast()
	}
}

// retransmitOldestLocked 快速重发最老未确认包(dup ack/SACK 触发)。
func (c *utpConn) retransmitOldestLocked() {
	var oldest *utpOutPkt
	for _, p := range c.outbuf {
		if p.pktType == utpSyn {
			continue
		}
		if oldest == nil || p.sentAt < oldest.sentAt {
			oldest = p
		}
	}
	if oldest != nil {
		c.sendOutPkt(oldest)
	}
}

// sackLocked 组装 SACK 扩展块 [next(1)=0, len(1), mask]:仅重组缓冲
// 非空(有跳号)时返回。掩码首位 = ackNr+2,字节内 LSB=小偏移(LE 位图)。
func (c *utpConn) sackLocked() []byte {
	if len(c.reorder) == 0 {
		return nil
	}
	base := (c.ackNr + 2) & 0xFFFF
	maxOff := 0
	for s := range c.reorder {
		off := int((s - base) & 0xFFFF)
		if off < 0x8000 && off > maxOff {
			maxOff = off
		}
	}
	nbytes := (maxOff + 8) / 8
	if nbytes < 4 {
		nbytes = 4
	}
	if nbytes > 64 {
		nbytes = 64
	}
	mask := make([]byte, nbytes)
	for s := range c.reorder {
		off := int((s - base) & 0xFFFF)
		if off >= 0 && off < nbytes*8 {
			mask[off/8] |= 1 << uint(off%8)
		}
	}
	out := make([]byte, 2+nbytes)
	out[0] = 0 // next extension = 终止
	out[1] = byte(nbytes)
	copy(out[2:], mask)
	return out
}

// ---- net.Conn 接口 ------------------------------------------------------

func (c *utpConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if len(c.recvBuf) > 0 {
			n := copy(p, c.recvBuf)
			c.recvBuf = c.recvBuf[n:]
			if len(c.recvBuf) == 0 {
				c.recvBuf = nil
			}
			if c.needAck { // 读腾出空间 → 立即广告新窗口
				c.needAck = false
				_ = c.sendState(c.sackLocked())
			}
			return n, nil
		}
		switch c.state {
		case utpStateReset:
			return 0, errUTPReset
		case utpStateClosed:
			return 0, io.EOF
		}
		if c.readEOF {
			return 0, io.EOF
		}
		if !c.readDL.IsZero() {
			if time.Now().After(c.readDL) {
				return 0, errTimeout("uTP 读超时")
			}
			dl := c.readDL
			t := time.AfterFunc(time.Until(dl), func() {
				c.mu.Lock()
				c.cond.Broadcast()
				c.mu.Unlock()
			})
			c.cond.Wait()
			t.Stop()
			continue
		}
		c.cond.Wait()
	}
}

func (c *utpConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := c.writeChunk(p)
		total += n
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// writeChunk 写入至多一个 MSS:窗口允许立即发,否则等 ack 释放。
func (c *utpConn) writeChunk(p []byte) (int, error) {
	chunk := p
	if len(chunk) > utpMSS {
		chunk = chunk[:utpMSS]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.state == utpStateReset {
			return 0, errUTPReset
		}
		if c.state != utpStateConnected && c.state != utpStateFinSent {
			return 0, errUTPClosed
		}
		if !c.writeDL.IsZero() && time.Now().After(c.writeDL) {
			return 0, errTimeout("uTP 写超时")
		}
		limit := int64(c.cc.maxWindow)
		if int64(c.advWnd) < limit {
			limit = int64(c.advWnd)
		}
		pktLen := int64(len(chunk)) + utpHeaderLen
		if c.curWin+pktLen <= limit {
			pay := make([]byte, len(chunk))
			copy(pay, chunk)
			pkt := &utpOutPkt{pktType: utpData, seq: c.seqNr, payload: pay, firstUS: utpNowUS()}
			c.outbuf[pkt.seq] = pkt
			c.seqNr++
			c.curWin += pktLen
			c.sendOutPkt(pkt)
			if os.Getenv("ZD_UTP_DEBUG") != "" {
				log.Printf("[utp-dbg] %p(%s) 发送 seq=%d len=%d curWin=%d maxWnd=%.0f advWnd=%d", c, c.addr, pkt.seq, len(chunk), c.curWin, c.cc.maxWindow, c.advWnd)
			}
			return len(chunk), nil
		}
		if os.Getenv("ZD_UTP_DEBUG") != "" {
			log.Printf("[utp-dbg] %p(%s) 写等待 curWin=%d pktLen=%d maxWnd=%.0f advWnd=%d 在途=%d", c, c.addr, c.curWin, pktLen, c.cc.maxWindow, c.advWnd, len(c.outbuf))
		}
		if !c.writeDL.IsZero() {
			dl := c.writeDL
			t := time.AfterFunc(time.Until(dl), func() {
				c.mu.Lock()
				c.cond.Broadcast()
				c.mu.Unlock()
			})
			c.cond.Wait()
			t.Stop()
		} else {
			c.cond.Wait()
		}
	}
}

// Close 发 FIN 关闭写方向(半开:仍可读);封顶 3s 后销毁。
func (c *utpConn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		if c.state == utpStateConnected {
			c.state = utpStateFinSent
			c.sendFinLocked()
			c.mu.Unlock()
			go func() {
				deadline := time.Now().Add(3 * time.Second)
				c.mu.Lock()
				for c.state != utpStateClosed && c.state != utpStateReset && time.Now().Before(deadline) {
					c.mu.Unlock()
					time.Sleep(50 * time.Millisecond)
					c.mu.Lock()
				}
				if c.state != utpStateReset {
					c.state = utpStateClosed
				}
				c.terminateLocked()
				c.mu.Unlock()
				c.mgr.dropConn(c)
			}()
			return
		}
		if c.state != utpStateReset {
			c.state = utpStateClosed
		}
		c.terminateLocked()
		c.mu.Unlock()
		c.mgr.dropConn(c)
	})
	return nil
}

func (c *utpConn) LocalAddr() net.Addr { return c.mgr.pc.LocalAddr() }

func (c *utpConn) RemoteAddr() net.Addr { return c.addr }

func (c *utpConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDL, c.writeDL = t, t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *utpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDL = t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *utpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDL = t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

// errTimeout 是带超时语义的错误(net.Error.Timeout)。
type errTimeout string

func (e errTimeout) Error() string   { return string(e) }
func (e errTimeout) Timeout() bool   { return true }
func (e errTimeout) Temporary() bool { return true }

// acceptFromSYN 接受方收到 SYN 后的初始化:记录对端 seq、生成本端
// 初始 seq 并立即回 SYN-ACK(ST_STATE,seq 不递增——两库口径)。
func (c *utpConn) acceptFromSYN(h utpHeader) {
	c.mu.Lock()
	c.ackNr = h.seqNr
	c.initSeq = uint16(rand.Uint32() & 0xFFFF)
	c.seqNr = c.initSeq
	_ = c.sendState(nil)
	c.cond.Broadcast()
	c.mu.Unlock()
}
