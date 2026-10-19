package bt

import (
	"bufio"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// reqKey 标识一个已发出、未到货的请求。
type reqKey struct {
	piece  int
	begin  uint32
	length uint32
	sentAt time.Time // 发出时刻,用于请求超时判定
}

// outbound 是发往 peer 的出站帧。
type outbound struct {
	id      uint8
	payload []byte
}

const (
	sendQueueCap   = 128
	pipelineWindow = 16 // 单 peer 初始请求窗口(块);此后按交付/拒绝自适应
	winMin         = 4  // 自适应窗口下限
	winMax         = 64 // 自适应窗口上限(64×16KB=1MB 在途,高 RTT 链路也能喂饱)
	// defaultDialConcurrency 并发拨号上限缺省值:tacker/DHT 返回的候选里
	// 大量是死地址(超时/拒连),并发过低时一轮下来要十几分钟,活 peer 迟迟
	// 连不上(实测只连上 7 条而同 swarm 的 BitComet 连了三十多条)。直连时
	// 由 ClientConfig.DialConcurrency 放大;经代理时引擎会收紧,避免并发
	// CONNECT 打满代理出口。
	defaultDialConcurrency = 64
	keepaliveGap           = 100 * time.Second
	writeTimeout           = 60 * time.Second
	// reqTimeout:单个请求超过该时长仍未到货即取消归还(picker 借出释放,
	// 窗口槽位留给能正常交付的 peer)。慢于它交付的请求若最终到达,按陈旧
	// 交付处理(重复块有 dup 判定,件级校验兜底)。
	reqTimeout = 30 * time.Second
	// stallTimeout:未 choke 且有在途请求、却长时间零交付的连接视为死连接,
	// 直接断开换人(候选池会回补重拨)。
	stallTimeout = 60 * time.Second
)

// peerConn 是一条已握手的 peer 连接。读侧只属于 run goroutine;
// 写侧统一经由 send 队列由 writeLoop 独占,避免多协程交叉写帧。
type peerConn struct {
	cl   *Client
	conn net.Conn
	addr string
	id   [20]byte

	mu          sync.Mutex
	bitfield    []bool
	peerChoke   bool
	lastData    time.Time // 最近一次收到有效数据(停滞判定;mu 保护)
	extCap      bool      // 对端握手 reserved 声明支持扩展协议
	fastCap     bool      // 对端声明支持 fast extension(BEP-6)
	ext         *ExtHandshake
	outstanding []reqKey
	pending     int
	pexShared   bool

	// 自适应请求窗口(mu 保护):winSucc 连续满一个 win 就 +1(加性增),
	// 超时/拒绝减半(乘性减,5s 防抖);AIMD 对冲固定窗口在高 RTT 下
	// 喂不饱单 peer 吞吐、低质对端又霸占在途的问题。
	win      int
	winSucc  int
	lastLoss time.Time

	// 上传侧 choker 状态(mu 保护):初始 unchokedByUs=false,
	// 即 BEP-3 的"连接始于 choked",由 choker 决策何时放行。
	peerInterested bool      // 对端想要我们的数据(收到 interested)
	unchokedByUs   bool      // 我们已放行它(serveRequest 按此闸门服务)
	unchokeAt      time.Time // 当前放行窗口起点
	chokedAt       time.Time // 我们最后一次 choke 它的时刻

	// 下行速率采样(仅 choker 协程访问,无需锁)
	rxAt   time.Time
	rxLast int64
	rxRate int64

	connectedAt time.Time // 连接建立时刻(淘汰评估用,创建后只读)

	downTotal, upTotal atomic.Int64 // 本连接累计收发字节数
	haveCount          atomic.Int32 // 对端声明的 piece 数(进度展示用)
	// 只取不予检测基线:首次真正向它供数的时刻与当时它声明的 piece 数。
	// 之后若"索取巨大 + 回报为零 + piece 数纹丝不动",即疑似 PCDN(见
	// client.leechSweep)。serveAt 0 = 尚未供过数;读协程写、扫描协程读,
	// 故用原子量。
	serveAt     atomic.Int64
	haveAtServe atomic.Int32
	utp                bool         // 本连接走 uTP 传输(BEP-29;PEX 分享用)
	src                string       // peer 来源:tracker/dht/pex/manual/incoming(展示)
	inbound            bool         // 对端主动连入(false = 我方拨出)
	proven             bool         // 已向候选池标记"供过数据"(仅读循环写;见 onPiece)

	// 展示速率采样基线(PeerStats 快照间做差;仅快照协程访问)。
	spAt   time.Time
	spDown int64
	spUp   int64

	send chan outbound
	done chan struct{}
	once sync.Once
}

func newPeerConn(cl *Client, conn net.Conn, addr string, id [20]byte) *peerConn {
	return &peerConn{
		cl: cl, conn: conn, addr: addr, id: id,
		peerChoke:   true,
		lastData:    time.Now(),
		rxAt:        time.Now(),
		connectedAt: time.Now(),
		win:         pipelineWindow,
		send:        make(chan outbound, sendQueueCap),
		done:        make(chan struct{}),
	}
}

func (pc *peerConn) shutdown() {
	pc.once.Do(func() {
		close(pc.done)
		pc.conn.Close()
	})
}

func (pc *peerConn) closed() bool {
	select {
	case <-pc.done:
		return true
	default:
		return false
	}
}

// push 阻塞投递出站帧(任务停止时返回 false)。
func (pc *peerConn) push(o outbound) bool {
	select {
	case pc.send <- o:
		return true
	case <-pc.done:
		return false
	case <-pc.cl.stopCh:
		return false
	}
}

// tryPush 非阻塞投递,队满即弃(cancel/keepalive 类可丢弃帧使用)。
func (pc *peerConn) tryPush(o outbound) bool {
	select {
	case pc.send <- o:
		return true
	default:
		return false
	}
}

func (pc *peerConn) writeLoop() {
	// 复用组帧缓冲:热路径(piece 块 16KB 级)每消息零分配、单次写
	var wbuf []byte
	for {
		select {
		case o := <-pc.send:
			pc.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if o.id == MsgKeepalive {
				if err := WriteKeepalive(pc.conn); err != nil {
					pc.shutdown()
					return
				}
				continue
			}
			n := 5 + len(o.payload)
			if cap(wbuf) < n {
				wbuf = make([]byte, n+4096)
			}
			buf := wbuf[:n]
			binary.BigEndian.PutUint32(buf, uint32(1+len(o.payload)))
			buf[4] = o.id
			copy(buf[5:], o.payload)
			if _, err := pc.conn.Write(buf); err != nil {
				pc.shutdown()
				return
			}
		case <-pc.done:
			return
		}
	}
}

// has 供 picker 认领时查询本 peer 是否持有某 piece(只在 run goroutine 调用)。
func (pc *peerConn) has(i int) bool {
	return pc.bitfield != nil && i < len(pc.bitfield) && pc.bitfield[i]
}

// fill 在管道未满时尽量多认领并发送 request。
// 可从 run 协程与补位协程并发调用:先在锁内取 bitfield 快照与 choke 状态,
// 再在锁外认领(picker 本身并发安全),避免补位协程对 pc.bitfield 的无锁读
// 与 setBitfield 竞争。被 choke 时对端不应答请求,一律空转:在途借出已由
// onChoke 归还,unchoke 或补位协程触发时重新认领。
func (pc *peerConn) fill() {
	if pc.closed() || pc.cl.completed.Load() {
		return
	}
	pc.mu.Lock()
	if pc.peerChoke {
		pc.mu.Unlock()
		return
	}
	var have func(int) bool
	if pc.bitfield != nil {
		snap := make([]bool, len(pc.bitfield))
		copy(snap, pc.bitfield)
		have = func(i int) bool { return i < len(snap) && snap[i] }
	}
	pc.mu.Unlock()
	if have == nil {
		return
	}
	for {
		pc.mu.Lock()
		full := pc.pending >= pc.win
		pc.mu.Unlock()
		if full {
			return
		}
		piece, begin, length, ok := pc.cl.picker.Claim(have)
		if !ok {
			return
		}
		payload := BuildRequest(uint32(piece), uint32(begin), uint32(length))
		pc.mu.Lock()
		if pc.closed() {
			// 与 cleanup 竞态:连接已终结,entry 追加进去就再也没人核销
			// (cleanup 已把 outstanding 收走)。立即归还本次认领。
			pc.mu.Unlock()
			pc.cl.picker.Cancel(piece, uint32(begin))
			return
		}
		pc.outstanding = append(pc.outstanding,
			reqKey{piece: piece, begin: uint32(begin), length: uint32(length), sentAt: time.Now()})
		pc.pending++
		pc.mu.Unlock()
		pc.push(outbound{msgRequest, payload})
	}
}

// refillLoop 周期性补位请求。修复两类停滞:
//  1. unchoke 早于 bitfield 到达时 fill 空转(此时无可认领块),之后若无
//     数据/拒绝帧到达,fill 再无触发,管道永久为空;
//  2. seeder 周期性 choke 再以乐观 unchoke 放行,窗口短且不定,错过后
//     请求同样归零。
//
// 每 3 秒补一轮;非 fast 对端被 choke 时 fill 自行空转,无副作用。
func (pc *peerConn) refillLoop() {
	tk := time.NewTicker(3 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-pc.done:
			return
		case <-pc.cl.stopCh:
			return
		case <-tk.C:
			pc.expireStale()
			if pc.stallCheck() {
				pc.shutdown()
				return
			}
			pc.fill()
		}
	}
}

// expireStale 取消滞留超过 reqTimeout 的在途请求:归还 picker 借出(块重新
// 可借给其它 peer)、释放窗口槽位。无超时机制时,沉默 peer 的请求会永久占住
// picker 在途计数,可用度失真、热点 piece 停滞。
func (pc *peerConn) expireStale() {
	now := time.Now()
	pc.mu.Lock()
	var stale []reqKey
	kept := pc.outstanding[:0]
	for _, k := range pc.outstanding {
		if now.Sub(k.sentAt) > reqTimeout {
			stale = append(stale, k)
			continue
		}
		kept = append(kept, k)
	}
	pc.outstanding = kept
	pc.pending -= len(stale)
	pc.mu.Unlock()
	if len(stale) > 0 {
		btDebug("请求超时 %s n=%d(30s 未交付,归还重借)", pc.addr, len(stale))
		pc.noteLoss() // 超时 = 对端交付能力差,收缩窗口
	}
	for _, k := range stale {
		pc.cl.picker.Cancel(k.piece, k.begin)
	}
}

// noteDelivery 记一次成功交付:连续满一个窗口且未达上限则 +1(加性增),
// 与 noteLoss 的减半构成 AIMD。
func (pc *peerConn) noteDelivery() {
	pc.mu.Lock()
	pc.winSucc++
	if pc.winSucc >= pc.win && pc.win < winMax {
		pc.win++
		pc.winSucc = 0
	}
	pc.mu.Unlock()
}

// noteLoss 记一次损失(超时/拒绝):窗口减半,下限 winMin。
// 5s 防抖——拒绝常整批到达,逐条减半会直接砸穿下限。
func (pc *peerConn) noteLoss() {
	pc.mu.Lock()
	if time.Since(pc.lastLoss) >= 5*time.Second {
		pc.lastLoss = time.Now()
		pc.win /= 2
		if pc.win < winMin {
			pc.win = winMin
		}
		pc.winSucc = 0
		btDebug("窗口减半 %s utp=%v win=%d", pc.addr, pc.utp, pc.win)
	}
	pc.mu.Unlock()
}

// stallCheck 停滞判定:未 choke、有在途请求、但 stallTimeout 内零交付
// (含件校验数据)→ 对端多半是只握手不干活或网络半死,断开换人。
// 返回 true 表示应断开。
func (pc *peerConn) stallCheck() bool {
	pc.mu.Lock()
	stall := !pc.peerChoke && pc.pending > 0 && time.Since(pc.lastData) > stallTimeout
	pc.mu.Unlock()
	if stall {
		btDebug("停滞断开 %s utp=%v pending=%d win=%d", pc.addr, pc.utp, pc.pending, pc.win)
	}
	return stall
}

// run 是 peer 连接的读侧主循环。仅在拨号/接收协程中调用一次。
func (pc *peerConn) run() {
	defer pc.cleanup()
	go pc.writeLoop()
	go pc.refillLoop()
	// 初始消息:bitfield(fast:haveall/havenone)→ 扩展握手 → interested
	// (unchoke 由 choker 决策,见 choker.go)
	// 初始持有图必须诚实:文件选择任务的未选 piece 在 picker 里也计入
	// CompletedPieces(标记落定以便跳过认领),拿它判断会误发 HAVEALL——
	// 对端以为我们全量持有,转而来请求未选 piece,只能被拒绝/丢弃白耗管线
	have := pc.cl.picker.Have()
	n := 0
	for _, h := range have {
		if h {
			n++
		}
	}
	switch {
	case pc.fastCap && n == len(have):
		pc.push(outbound{msgHaveAll, nil})
	case pc.fastCap && n == 0:
		pc.push(outbound{msgHaveNone, nil})
	case n > 0:
		pc.push(outbound{msgBitfield, bitfieldBytes(have)})
	}
	if pc.extCap {
		eh := append([]byte{ExtHandshakeID},
			BuildExtHandshake(int64(len(pc.cl.meta.RawInfo())), pc.cl.cfg.ListenPort, pc.cl.cfg.SpoofName)...)
		pc.push(outbound{msgExtended, eh})
	} else {
		btDebug("对端未声明 BEP-10,不发扩展握手 %s", pc.addr)
	}
	if !pc.cl.completed.Load() {
		pc.push(outbound{msgInterested, nil})
	}
	// 不在此无条件 unchoke:BEP-3 连接始于 choked,放行由 choker
	// 决策(对端发 interested 后事件踢醒,常规 5s 周期兜底)

	// 读侧 bufio:ReadMsg 对裸 conn 是"4 字节头 + 体"两次独立 syscall,
	// bufio 把粘包的后续帧头合并进一次读取,syscall 数大幅下降。
	br := bufio.NewReaderSize(pc.conn, 64<<10)
	for {
		select {
		case <-pc.cl.stopCh:
			return
		case <-pc.done:
			return
		default:
		}
		pc.conn.SetReadDeadline(time.Now().Add(keepaliveGap))
		m, err := ReadMsg(br)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				pc.tryPush(outbound{id: MsgKeepalive})
				continue
			}
			return
		}
		if m.ID != msgPiece { // piece 帧洪流会淹没环形日志窗口
			btDebug("帧 %s in=%v type=%d len=%d", pc.addr, pc.inbound, m.ID, len(m.Payload))
		}
		switch m.ID {
		case MsgKeepalive:
		case msgChoke:
			pc.onChoke()
		case msgUnchoke:
			pc.mu.Lock()
			pc.peerChoke = false
			pc.mu.Unlock()
			pc.fill()
		case msgInterested:
			pc.mu.Lock()
			pc.peerInterested = true
			pc.mu.Unlock()
			pc.cl.kickChokerNow() // 槽位空闲时立即放行,不等周期
		case msgNotInterested:
			pc.mu.Lock()
			pc.peerInterested = false
			pc.mu.Unlock()
			pc.cl.kickChokerNow() // 让出的槽位立即补位
		case msgHave:
			pc.onHave(m.Payload)
		case msgBitfield:
			if bf, err := parseBitfield(m.Payload, pc.cl.meta.PieceCount()); err == nil {
				pc.setBitfield(bf)
			}
		case msgHaveAll:
			bf := make([]bool, pc.cl.meta.PieceCount())
			for i := range bf {
				bf[i] = true
			}
			pc.setBitfield(bf)
		case msgHaveNone:
			pc.setBitfield(make([]bool, pc.cl.meta.PieceCount()))
		case msgSuggest, msgAllowedFast:
			// 提示类消息,当前策略不特判(allowedfast 不改变 choke 下的正常请求)
		case msgRejectReq:
			pc.onReject(m.Payload)
		case msgRequest:
			pc.serveRequest(m.Payload)
		case msgPiece:
			pc.onPiece(m.Payload)
		case msgCancel:
		case msgExtended:
			pc.onExtended(m.Payload)
		}
	}
}

// onHave 处理对端的 have:懒分配持有图(协议允许 0-piece 的下载方不发
// bitfield,首个 have 到达时才建)——否则 haveCount 恒 0,leechSweep 会把
// "正常推进但从未发过 bitfield"的 peer 误判进度不变;fill 的认领快照与
// 断开时的可用度回退也一并生效。
func (pc *peerConn) onHave(payload []byte) {
	if len(payload) != 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload))
	pc.cl.picker.AddHave(idx)
	pc.mu.Lock()
	if pc.bitfield == nil {
		pc.bitfield = make([]bool, pc.cl.meta.PieceCount())
	}
	if idx < len(pc.bitfield) && !pc.bitfield[idx] {
		pc.bitfield[idx] = true
		pc.haveCount.Add(1)
	}
	pc.mu.Unlock()
}

// setBitfield 登记 haveall/havenone/bitfield 三种形式的对端持有图。
func (pc *peerConn) setBitfield(bf []bool) {
	pc.mu.Lock()
	pc.bitfield = bf
	pc.mu.Unlock()
	n := 0
	for _, h := range bf {
		if h {
			n++
		}
	}
	pc.haveCount.Store(int32(n))
	pc.cl.picker.AddAvailability(bf)
}

// onReject 处理对端拒绝(fast extension):归还借出的块并补位新请求。
func (pc *peerConn) onReject(payload []byte) {
	idx, begin, _, err := ParseRequest(payload)
	if err != nil {
		return
	}
	pc.mu.Lock()
	kept := pc.outstanding[:0]
	found := false
	for _, k := range pc.outstanding {
		if k.piece == int(idx) && k.begin == begin {
			found = true
			if pc.pending > 0 {
				pc.pending--
			}
			continue
		}
		kept = append(kept, k)
	}
	pc.outstanding = kept
	pc.mu.Unlock()
	if found {
		btDebug("对端拒绝 %s piece=%d begin=%d", pc.addr, idx, begin)
		pc.cl.picker.Cancel(int(idx), begin)
		pc.noteLoss() // 对端拒服 = 负面信号,收缩窗口
		pc.fill()
	}
}

// onChoke 对端把我们 choke:置位 choke 状态(fill 停止认领),并按 BEP-3 把
// 在途请求全部归还 picker 供其他 peer 借出。fast 对端虽可显式 reject 甚至
// 在 choke 下仍服务,但实测多数 seeder choke 时直接忽略请求,保留在途会变成
// 永不核销的"僵尸借出",占满 picker 可用度(实测 inflight 无界增长、热
// piece 长时间停滞);unchoke 后由 fill/补位协程重新认领。
func (pc *peerConn) onChoke() {
	pc.mu.Lock()
	pc.peerChoke = true
	out := pc.outstanding
	pc.outstanding = nil
	pc.pending = 0
	pc.mu.Unlock()
	for _, k := range out {
		pc.cl.picker.Cancel(k.piece, k.begin)
	}
}

// removeOutstanding 移除一个已到货请求(每个请求恰好在到货时移除一次)。
func (pc *peerConn) removeOutstanding(piece, begin uint32) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for i, k := range pc.outstanding {
		if k.piece == int(piece) && k.begin == begin {
			pc.outstanding = append(pc.outstanding[:i], pc.outstanding[i+1:]...)
			if pc.pending > 0 {
				pc.pending--
			}
			return true
		}
	}
	return false
}

// onPiece 处理 piece 数据帧。仅 run goroutine 调用。
func (pc *peerConn) onPiece(payload []byte) {
	idx, begin, data, err := ParsePiece(payload)
	if err != nil || int(idx) >= pc.cl.meta.PieceCount() {
		return
	}
	plen := pc.cl.meta.PieceLen(int(idx))
	if int64(begin)+int64(len(data)) > plen {
		return
	}
	// 限速在登记之前:阻塞本 peer 的读循环形成 TCP 背压
	if err := pc.cl.waitDL(pc.cl.baseCtx, int64(len(data))); err != nil {
		return
	}
	st := pc.cl.store.Load()
	if st == nil {
		// store 未就绪:归还本次借出,避免占住窗口与 picker 在途计数。
		pc.removeOutstanding(idx, begin)
		pc.cl.picker.Cancel(int(idx), begin)
		return
	}
	// 判重→落盘→登记 必须在 piece 锁内原子完成,并与 onPieceVerified 的
	// "校验+标记完成"互斥。旧实现先写盘再判重:末局重复借出的迟到块即使被
	// 判为 dup,好数据已被覆盖;若该 piece 已校验完成则永不重验,损坏被永久
	// 标记为完成(实测 807 件中 8 件磁盘 SHA1 不符,任务却已 seeding)。
	m := pc.cl.lockPiece(int(idx))
	if !pc.cl.picker.BlockWanted(int(idx), begin) {
		m.Unlock()
		pc.removeOutstanding(idx, begin)
		pc.cl.cancelDup(int(idx), begin, pc)
		return
	}
	if err := st.WriteBlock(int(idx), begin, data); err != nil {
		// 落盘失败(磁盘异常):归还借出并断开该连接——管道里的块再也
		// 无法落盘,留着只会占满在途计数。
		m.Unlock()
		pc.removeOutstanding(idx, begin)
		pc.cl.picker.Cancel(int(idx), begin)
		pc.shutdown()
		return
	}
	pc.removeOutstanding(idx, begin)
	new, done := pc.cl.picker.RecordBlock(int(idx), begin)
	if new {
		// 归因记入 piece 锁内:校验失败时 takeBlame 同持 piece 锁取表,
		// 锁外记账会在"最后一块触发的校验"抢先下丢条目(来源统计失真)。
		// 好块计数不在这里:piece 校验通过时才算数(校验通过才算好块)。
		pc.cl.recordBlame(int(idx), pc.addr)
	}
	m.Unlock()
	if !new {
		// 判重与登记之间被抢先(极小竞态):不再重复计数与回调。
		pc.cl.cancelDup(int(idx), begin, pc)
		return
	}
	pc.cl.downTotal.Add(int64(len(data)))
	pc.downTotal.Add(int64(len(data)))
	if !pc.proven {
		// 首次收到有效块即认定"能供数":进候选池 good 集合,持久化到 peer
		// 历史并享受最高补拨优先级(空转连接永远不会进这里)。
		pc.proven = true
		pc.cl.markCandGood(pc.addr, pc.utp)
	}
	pc.mu.Lock()
	pc.lastData = time.Now()
	pc.mu.Unlock()
	pc.noteDelivery()
	if done {
		pc.cl.onPieceVerified(int(idx))
	}
	pc.fill()
}

// chokerSample 刷新并返回该连接的近期下行速率(EWMA,B/s)。仅 choker 协程调用。
func (pc *peerConn) chokerSample(now time.Time) int64 {
	cur := pc.downTotal.Load()
	if dt := now.Sub(pc.rxAt).Seconds(); dt >= 0.2 {
		inst := int64(float64(cur-pc.rxLast) / dt)
		if pc.rxRate == 0 {
			pc.rxRate = inst
		} else {
			pc.rxRate = (pc.rxRate*3 + inst) / 4
		}
		pc.rxLast, pc.rxAt = cur, now
	}
	return pc.rxRate
}

// chokerView 返回 choker 决策所需的连接状态快照。
func (pc *peerConn) chokerView() (interested, unchoked bool, uAt, cAt time.Time) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.peerInterested, pc.unchokedByUs, pc.unchokeAt, pc.chokedAt
}

// setUnchoke 应用放行/阻塞状态;状态有变化才发消息(幂等)。
func (pc *peerConn) setUnchoke(v bool) {
	pc.mu.Lock()
	if pc.unchokedByUs == v {
		pc.mu.Unlock()
		return
	}
	pc.unchokedByUs = v
	if v {
		pc.unchokeAt = time.Now()
	} else {
		pc.chokedAt = time.Now()
	}
	pc.mu.Unlock()
	if v {
		pc.push(outbound{msgUnchoke, nil})
	} else {
		pc.push(outbound{msgChoke, nil})
	}
}

// serveRequest 做种/互通:对端向我们请求数据。仅 run goroutine 调用。
func (pc *peerConn) serveRequest(payload []byte) {
	idx, begin, length, err := ParseRequest(payload)
	if err != nil || int(idx) >= pc.cl.meta.PieceCount() || length == 0 || length > 4*BlockSize {
		return
	}
	pc.mu.Lock()
	allowed := pc.unchokedByUs
	pc.mu.Unlock()
	if !allowed {
		// 已被我方 choke:fast 显式拒绝,非 fast 静默丢弃(BEP-3 允许)
		if pc.fastCap {
			pc.tryPush(outbound{msgRejectReq, BuildRequest(idx, begin, length)})
		}
		return
	}
	if !pc.cl.picker.IsDone(int(idx)) {
		// fast 语义必须显式拒绝;非 fast 忽略即协议允许的做法
		if pc.fastCap {
			pc.tryPush(outbound{msgRejectReq, BuildRequest(idx, begin, length)})
		}
		return
	}
	plen := pc.cl.meta.PieceLen(int(idx))
	if int64(begin)+int64(length) > plen {
		length = uint32(plen - int64(begin))
		if length == 0 {
			return
		}
	}
	buf := make([]byte, length)
	if err := pc.cl.store.Load().ReadForUpload(pc.cl.meta.PieceOffset(int(idx))+int64(begin), buf); err != nil {
		return
	}
	if err := pc.cl.waitUL(pc.cl.baseCtx, int64(length)); err != nil {
		return
	}
	p := make([]byte, 8, 8+len(buf))
	p[0] = byte(idx >> 24)
	p[1] = byte(idx >> 16)
	p[2] = byte(idx >> 8)
	p[3] = byte(idx)
	p[4] = byte(begin >> 24)
	p[5] = byte(begin >> 16)
	p[6] = byte(begin >> 8)
	p[7] = byte(begin)
	pc.push(outbound{msgPiece, append(p, buf...)})
	pc.cl.upTotal.Add(int64(length))
	pc.upTotal.Add(int64(length))
	// 首次成功供数:定格对端此刻的 piece 声明数,作为"进度是否变化"基线
	if pc.serveAt.CompareAndSwap(0, time.Now().UnixNano()) {
		pc.haveAtServe.Store(pc.haveCount.Load())
	}
}

// clientLabel 对端客户端名:扩展握手 v(对端自报的完整名称)优先,退回
// peer id 前缀推断。与连接详情展示、封禁名单归档、客户端黑名单匹配共用
// 同一口径。调用方若已持有 cl.pMu 需注意锁序(pMu → pc.mu)。
func (pc *peerConn) clientLabel() string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.ext != nil && pc.ext.Version != "" {
		return pc.ext.Version
	}
	return ClientName(pc.id)
}

// onExtended 处理扩展消息。入站 ID 按我们声明的 m 字典解释
// (BEP-10:发往对端时才查对端的 m 字典)。
func (pc *peerConn) onExtended(payload []byte) {
	if len(payload) == 0 {
		return
	}
	switch extID := payload[0]; extID {
	case ExtHandshakeID:
		e, err := ParseExtHandshake(payload[1:])
		if err != nil {
			btDebug("扩展握手解析失败 %s: %v (len=%d)", pc.addr, err, len(payload)-1)
			return
		}
		btDebug("扩展握手到达 %s p=%d pex=%v holepunch=%v m=%v", pc.addr,
			e.ListenPort, e.SupportsPEX(), e.SupportsHolepunch(), e.M)
		pc.mu.Lock()
		pc.ext = e
		shared := pc.pexShared
		pc.pexShared = true
		pc.mu.Unlock()
		// 客户端黑名单补核:扩展握手 v 字段的对端全名(如 "qBittorrent
		// 4.6.7")此刻才到齐——peer id 伪装绕过握手期检查的在这里拦下。
		if e.Version != "" {
			if pat, ok := pc.cl.clientBlacklisted(e.Version); ok {
				host, _, err := net.SplitHostPort(pc.addr)
				if err != nil {
					host = pc.addr
				}
				log.Printf("[bt] 拒绝黑名单客户端 %s(客户端 %q 命中 %s)", pc.addr, e.Version, pat)
				pc.cl.banList.Ban(host, "客户端黑名单:匹配 "+pat, e.Version)
				pc.shutdown()
				return
			}
		}
		if !shared && e.SupportsPEX() && !pc.cl.meta.Private {
			if peers, f := pc.cl.gatherPEX(50, pc.addr); len(peers) > 0 {
				if id := e.PeerExtID("ut_pex"); id != 0 {
					pc.tryPush(outbound{msgExtended, append([]byte{id}, BuildPEX(peers, f, nil)...)})
				}
			}
		}
	case LocalMetadataID:
		pc.onMetadata(payload[1:])
	case LocalPEXID:
		if pc.cl.meta.Private {
			return
		}
		m, err := ParsePEX(payload[1:])
		if err != nil {
			return
		}
		for i, a := range m.Added {
			var f byte
			if i < len(m.AddedF) {
				f = m.AddedF[i]
			}
			pc.cl.AddPeerPEX(a, f) // 0x02=种子 / 0x04=支持 uTP 参与补拨排序
		}
	case LocalHolepunchID:
		pc.onHolepunch(payload[1:])
	}
}

// onHolepunch 处理 BEP-12 请求。我们只充当接线员:对端请我们主动连接某个
// 目标地址以打开它那一侧的 NAT 洞时,发一个短连接再立即断开(目的仅是让本端
// NAT 放行,不建立会话)。connect 预告与 error 回执只记录。本端不主动发起
// 打孔——我们的入站端口已由 uTP 监听,让对端主导即可。
func (pc *peerConn) onHolepunch(payload []byte) {
	m, err := ParseHolepunch(payload)
	if err != nil {
		return
	}
	switch m.MsgType {
	case hpRendezvous:
		if m.IP == "" || m.Port <= 0 || m.Port > 65535 {
			return
		}
		target := net.JoinHostPort(m.IP, strconv.Itoa(int(m.Port)))
		go func() {
			d := net.Dialer{Timeout: 8 * time.Second}
			conn, err := d.DialContext(pc.cl.baseCtx, "tcp", target)
			if err != nil {
				btDebug("ut_holepunch 打洞连接 %s 失败: %v", target, err)
				return
			}
			conn.Close()
			btDebug("ut_holepunch 已触发打洞 %s", target)
		}()
	case hpConnect:
		btDebug("ut_holepunch 预告入站连接 %s:%d", m.IP, m.Port)
	case hpError:
		btDebug("ut_holepunch 错误 %d: %s", m.ErrorNum, m.ErrorMsg)
	}
}

// onMetadata 处理 ut_metadata 请求/数据。P1 仅做服务端(对端向我们索取)。
func (pc *peerConn) onMetadata(payload []byte) {
	msg, err := ParseMetadataMsg(payload)
	if err != nil {
		return
	}
	raw := pc.cl.meta.RawInfo()
	total := int64(len(raw))
	if msg.Type != metaReq || pc.cl.meta == nil {
		return
	}
	pc.mu.Lock()
	e := pc.ext
	pc.mu.Unlock()
	if e == nil {
		return
	}
	id := e.PeerExtID("ut_metadata")
	if id == 0 {
		return
	}
	const metaPiece = metaPieceSize
	start := msg.Piece * metaPiece
	if start < 0 || start >= total {
		pc.tryPush(outbound{msgExtended, append([]byte{id}, BuildMetadataMsg(metaReject, msg.Piece)...)})
		return
	}
	end := min(start+metaPiece, total)
	pc.tryPush(outbound{msgExtended, append([]byte{id}, BuildMetadataData(msg.Piece, total, raw[start:end])...)})
}

// cleanup 在连接结束时归还资源:在途请求、可用度。
func (pc *peerConn) cleanup() {
	pc.shutdown()
	pc.mu.Lock()
	out := pc.outstanding
	pc.outstanding = nil
	bf := pc.bitfield
	pc.mu.Unlock()
	for _, k := range out {
		pc.cl.picker.Cancel(k.piece, k.begin)
	}
	if bf != nil {
		pc.cl.picker.RemoveAvailability(bf)
	}
	pc.cl.removePeer(pc.addr)
}
