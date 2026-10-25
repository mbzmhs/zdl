package bt

import (
	"sync"
)

// streamDepthBytes 顺序深度目标字节:窗口内可并行认领的前沿宽度。
// 4MB 足够喂饱常见 peer 在途窗口(单 peer ≤1MB),同时把完成顺序约束在
// 播放需要的前沿内。
const streamDepthBytes = 4 << 20

// PiecePicker 管理单个任务的 piece/块认领状态。
// 策略:稀有优先(可用 peer 数最少者先下),同类内轮转起点避免各 peer 挤同一 piece;
// 全部未完成块都借出后进入末局模式,允许向多 peer 重复请求同一块。
type PiecePicker struct {
	mu       sync.Mutex
	nPieces  int
	pieceLen int64 // 满 piece 长度(SnapshotBlocks 步长);各 piece 实际长用 lenFn
	total    int64
	lenFn    func(int) int64 // 逐 piece 实际长度(v2 各文件末 piece 短)

	avail []int32 // 每 piece 当前有多少 peer 持有
	done  []bool  // piece 已通过校验并落定
	pcs   []pieceState

	donePieces int
	unfiBlocks int64 // 未完成 piece 的总块数
	freeBlocks int64 // 未完成且未被借出的块数
	inflBlocks int64 // 借出(含末局重复)未归还的请求数
	cursor     uint32
	wanted     []bool // piece 需求位图(文件选择;nil = 全部需要)
	stream     *streamState // 边下边播优先级(nil = 普通模式,零开销)
}

// streamState 边下边播状态。只影响 Claim 的认领顺序,块借出/归还/账目
// 语义与普通模式完全一致:三级优先 = pin(文件头尾元数据 piece)→ 播放
// 窗口(前沿深度内严格顺序)→ 原有稀有优先(窗口末端之后,总进度不受影响)。
type streamState struct {
	cursor  int   // 播放头所在 piece(顺序窗口起点)
	ahead   int   // 顺序窗口宽度(piece 数)
	depth   int   // 顺序深度:自窗口内首个未完成 piece 起可认领的 piece 数。
	              // 窗口全宽开放时多 peer 按各自持有度四处借块,完成顺序在
	              // 窗口内散射(实测大窗口表现为"区域内随机下")——服务是
	              // 严格顺序的,前沿缺口就卡播放。深度限制把 peer 集中在前
	              // 沿,piece 按播放顺序集齐,深度随 piece 宽度反比收缩
	pinHead []int // 元数据 pin:文件头部 piece(容器头/索引,最高优先)
	pinTail []int // 元数据 pin:文件尾部 piece(moov/cues 常在文件尾)
	tailOn  bool  // 尾部 pin 生效中(faststart 确认后可解除)
}

type pieceState struct {
	blocks    int32
	have      int32
	req       int32  // 在途请求份数(末局同一块可被多 peer 借出)
	freeCnt   int32  // 空闲块数(未持有且未被借出);显式计数,避免派生公式在末局下失真
	haveB     []byte // 块位图,首次认领时才分配(大种子省内存)
	reqB      []byte
	haveBytes int64 // 已收到并落盘的字节(块粒度进度;ResetPiece 清零)
}

func (ps *pieceState) free() int32 { return ps.freeCnt }

// NewPicker 建立选择器;initialHave 为续传恢复的已有 piece(可为 nil),
// wanted 为 piece 需求位图(文件选择下载;nil = 全部需要)。
// 不需要的 piece 直接标记为已落定:不参与认领、不校验、不计入完成判定之外。
func NewPicker(nPieces int, pieceLen, total int64, lenFn func(int) int64, initialHave, wanted []bool) *PiecePicker {
	if lenFn == nil {
		lenFn = func(i int) int64 { return pieceLenOf(i, pieceLen, total) }
	}
	p := &PiecePicker{
		nPieces:  nPieces,
		pieceLen: pieceLen,
		total:    total,
		lenFn:    lenFn,
		avail:    make([]int32, nPieces),
		done:     make([]bool, nPieces),
		pcs:      make([]pieceState, nPieces),
		wanted:   wanted,
	}
	for i := range p.pcs {
		p.pcs[i].blocks = int32((lenFn(i) + BlockSize - 1) / BlockSize)
		p.pcs[i].freeCnt = p.pcs[i].blocks
	}
	for i := 0; i < nPieces; i++ {
		if initialHave != nil && initialHave[i] {
			ps := &p.pcs[i]
			ps.have = ps.blocks
			ps.freeCnt = 0
			ps.haveBytes = lenFn(i) // 续传位图 = 盘上已校验数据,计入进度
			p.done[i] = true        // 初始位图视为已校验数据(启动校验在 client 层先行处理)
			p.donePieces++
			continue
		}
		if wanted != nil && !wanted[i] {
			p.done[i] = true // 文件选择:不需要的 piece 视作落定(数据不落盘校验)
			p.donePieces++
			continue
		}
		p.unfiBlocks += int64(p.pcs[i].blocks)
		p.freeBlocks += int64(p.pcs[i].blocks)
	}
	return p
}

func pieceLenOf(i int, pieceLen, total int64) int64 {
	if end := int64(i+1) * pieceLen; end > total {
		return total - int64(i)*pieceLen
	}
	return pieceLen
}

func (p *PiecePicker) nBlocks(i int) int32 { return p.pcs[i].blocks }

// AddAvailability 记录一个 peer 的 bitfield。
func (p *PiecePicker) AddAvailability(have []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, h := range have {
		if h && i < p.nPieces {
			p.avail[i]++
		}
	}
}

// AddHave 记录对端 announce 的一条 have。
func (p *PiecePicker) AddHave(idx int) {
	if idx < 0 || idx >= p.nPieces {
		return
	}
	p.mu.Lock()
	p.avail[idx]++
	p.mu.Unlock()
}

// RemoveAvailability 在 peer 断开时回退其 bitfield 贡献。
func (p *PiecePicker) RemoveAvailability(have []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, h := range have {
		if h && i < p.nPieces && p.avail[i] > 0 {
			p.avail[i]--
		}
	}
}

// Claim 为一个 peer 认领下一个待下载块。has 返回该 peer 是否持有某 piece。
// 返回 piece 下标、块内偏移与块长;无块可下时 ok=false。
func (p *PiecePicker) Claim(has func(int) bool) (piece, begin int, length int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unfiBlocks == 0 {
		return 0, 0, 0, false
	}
	// 边下边播优先级:元数据 pin → 播放窗口(严格顺序)→ 稀有优先(只向前)。
	// 只改认领顺序,借出/账目路径与普通模式完全共用(claimBlock)。
	if p.stream != nil && p.freeBlocks > 0 {
		for _, idx := range p.stream.pinHead {
			if pi, b, l, c := p.claimFrom(idx, has); c {
				return pi, b, l, true
			}
		}
		if p.stream.tailOn {
			for _, idx := range p.stream.pinTail {
				if pi, b, l, c := p.claimFrom(idx, has); c {
					return pi, b, l, true
				}
			}
		}
		first := -1
		for i := 0; i < p.stream.ahead; i++ {
			idx := p.stream.cursor + i
			if idx >= p.nPieces {
				break
			}
			if p.done[idx] {
				continue // 已完成:前沿探测继续后移
			}
			if first < 0 {
				first = idx // 窗口内首个未完成 piece = 顺序服务的前沿
			}
			if idx >= first+p.stream.depth {
				break // 顺序深度外:先集齐前沿再前进,防窗口内散射
			}
			if pi, b, l, c := p.claimFrom(idx, has); c {
				return pi, b, l, true
			}
		}
		// 窗口头 mini-endgame:窗口内最靠前的未完成 piece 块已全部借出但仍未
		// 集齐(某块滞留在慢 peer 手里)时,允许其他 peer 重复借其未到货块——
		// 单块滞留不再卡住整片播放(全局末局要等全部空闲块耗尽才触发,大种子
		// 下载中段几乎不可达)。重复到货由 RecordBlock 判重 + cancelDup 收敛。
		if h := p.streamHeadPieceLocked(has); h >= 0 {
			if pick := p.claimBlock(h, true); pick >= 0 {
				return h, pick * BlockSize, int(p.blockLen(int64(h), int64(pick))), true
			}
		}
		// 三级:流模式下后台只向前(窗口末端之后的 piece)。跳过/播过的旧位置
		// 不再与窗口抢带宽;前方确无可下块时经下方无过滤扫描兜底放开,保证
		// 任务始终能推进到完成。
		end := p.stream.cursor + p.stream.ahead
		if pi, b, l, c := p.claimRarestLocked(has, func(i int) bool { return i >= end }); c {
			return pi, b, l, true
		}
	}
	if p.freeBlocks > 0 {
		if pi, b, l, c := p.claimRarestLocked(has, nil); c {
			return pi, b, l, true
		}
	}
	// 末局模式:仅当全局再无空闲块(全部已借出)时,允许向多 peer 重复借出未到块
	if p.unfiBlocks > 0 && p.freeBlocks == 0 {
		start := p.cursor % uint32(p.nPieces)
		for i := 0; i < p.nPieces; i++ {
			idx := int((start + uint32(i)) % uint32(p.nPieces))
			if p.done[idx] || p.pcs[idx].have >= p.pcs[idx].blocks || !has(idx) {
				continue
			}
			p.cursor++
			if pick := p.claimBlock(idx, true); pick >= 0 {
				return idx, pick * BlockSize, int(p.blockLen(int64(idx), int64(pick))), true
			}
		}
	}
	return 0, 0, 0, false
}

// claimFrom 尝试从 piece idx 借出一块(该 peer 持有且有空闲块)。
// 须持 p.mu;流模式三级扫描共用,命中即返回块信息。
func (p *PiecePicker) claimFrom(idx int, has func(int) bool) (piece, begin, length int, ok bool) {
	if idx < 0 || idx >= p.nPieces || p.done[idx] || !has(idx) {
		return 0, 0, 0, false
	}
	if pick := p.claimBlock(idx, false); pick >= 0 {
		return idx, pick * BlockSize, int(p.blockLen(int64(idx), int64(pick))), true
	}
	return 0, 0, 0, false
}

// claimRarestLocked 稀有优先认领(原 Claim 主体两遍扫描抽出):filter 非空时
// 只在满足条件的 piece 里选(流模式三级"只向前"过滤;nil = 不过滤)。
// 第一遍在"该 peer 持有且还有空闲块"的候选里找最小可用度;第二遍在同类
// (最小可用度)piece 里优先"借出最多"(在途块最多)的热 piece——全部 seeder
// 都是 complete 时所有 piece 同可用度,纯轮转会把在途块摊到几百个 piece 上,
// 单个 piece 需等一轮轮转完才集齐块;向热 piece 借块加速完成,平手按轮转
// 起点打破。须持 p.mu。
func (p *PiecePicker) claimRarestLocked(has func(int) bool, filter func(int) bool) (piece, begin, length int, ok bool) {
	minAvail := int32(1<<31 - 1)
	for i := 0; i < p.nPieces; i++ {
		if p.done[i] || p.avail[i] == 0 || p.pcs[i].free() <= 0 || !has(i) {
			continue
		}
		if filter != nil && !filter(i) {
			continue
		}
		if p.avail[i] < minAvail {
			minAvail = p.avail[i]
		}
	}
	if minAvail == int32(1<<31-1) {
		return 0, 0, 0, false
	}
	start := p.cursor % uint32(p.nPieces)
	best := -1
	var bestReq int32
	for i := 0; i < p.nPieces; i++ {
		idx := int((start + uint32(i)) % uint32(p.nPieces))
		if p.done[idx] || p.avail[idx] != minAvail || p.pcs[idx].free() <= 0 || !has(idx) {
			continue
		}
		if filter != nil && !filter(idx) {
			continue
		}
		if r := p.pcs[idx].req; best < 0 || r > bestReq {
			best, bestReq = idx, r
		}
	}
	if best >= 0 {
		p.cursor = uint32((best + 1) % p.nPieces)
		if pick := p.claimBlock(best, false); pick >= 0 {
			return best, pick * BlockSize, int(p.blockLen(int64(best), int64(pick))), true
		}
	}
	return 0, 0, 0, false
}

// streamHeadPieceLocked 找流媒体服务被卡住的"窗口头":窗口内最靠前的未完成
// piece。仅当该 peer 持有它、其空闲块已耗尽且确有在途借出(req>0,块滞留在
// 别的 peer 手里)时返回下标——此时正常认领无块可给,mini-endgame 重复借出
// 是唯一解。头部还有空闲块(等待正常认领)或该 peer 不持有时返回 -1:重复借
// 后面的 piece 救不了被卡住的顺序服务。须持 p.mu。
func (p *PiecePicker) streamHeadPieceLocked(has func(int) bool) int {
	for i := 0; i < p.stream.ahead; i++ {
		idx := p.stream.cursor + i
		if idx >= p.nPieces {
			return -1
		}
		if p.done[idx] {
			continue // 已完成:窗口头后移
		}
		if !has(idx) {
			return -1 // 该 peer 没有头部:重复借后面的无意义
		}
		ps := &p.pcs[idx]
		if ps.free() > 0 {
			return -1 // 还有空闲块:正常认领路径可用
		}
		if ps.req > 0 {
			return idx // 全部借出且未集齐:确有滞留在途块
		}
		return -1
	}
	return -1
}

// claimBlock 在 piece 内选定块并登记借出;返回块下标,-1 表示该 piece 无可借块。
func (p *PiecePicker) claimBlock(idx int, endgame bool) int {
	ps := &p.pcs[idx]
	if ps.reqB == nil {
		ps.reqB = make([]byte, (ps.blocks+7)/8)
	}
	if !endgame {
		for b := int32(0); b < ps.blocks; b++ {
			if ps.reqB[b/8]&(1<<(7-uint(b%8))) != 0 {
				continue
			}
			if ps.haveB != nil && ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0 {
				// 已持有块绝不能再借出:请求会被对端重发,而 RecordBlock 对
				// 重复副本早退,借出位永不核销——在途计数成孤儿、freeCnt 被
				// 多扣,空闲数提前归零后该 piece 再也领不到块(piecesDone 停滞)。
				continue
			}
			ps.reqB[b/8] |= 1 << (7 - uint(b%8))
			ps.req++
			ps.freeCnt--
			p.freeBlocks--
			p.inflBlocks++
			return int(b)
		}
		return -1
	}
	// endgame:重复借出"未到货"的块(优先尚未重复借出的)
	for b := int32(0); b < ps.blocks; b++ {
		if ps.haveB != nil && ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0 {
			continue
		}
		ps.reqB[b/8] |= 1 << (7 - uint(b%8))
		ps.req++
		p.inflBlocks++
		return int(b)
	}
	return -1
}

func (p *PiecePicker) blockLen(piece, block int64) int64 {
	begin := block * BlockSize
	if rest := p.lenFn(int(piece)) - begin; rest < BlockSize {
		return rest
	}
	return BlockSize
}

// RecordBlock 登记到货块。newBlock=false 表示重复到货(调用方应向其余借出方发 cancel);
// pieceDone=true 表示该 piece 全部块到货,可进入校验。
// BlockWanted 只读判定:该块是否值得落盘。piece 已完成(已校验)、越界、
// 或该块已持有,一律 false——调用方据此在写盘前丢弃迟到/重复块,避免覆盖
// 已校验 piece 上的好数据(写盘发生在 RecordBlock 之前的老实现会覆盖)。
func (p *PiecePicker) BlockWanted(piece int, begin uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if piece < 0 || piece >= p.nPieces || p.done[piece] {
		return false
	}
	ps := &p.pcs[piece]
	b := int(begin) / BlockSize
	if int64(b) >= int64(ps.blocks) {
		return false
	}
	if ps.haveB != nil && ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0 {
		return false
	}
	return true
}

func (p *PiecePicker) RecordBlock(piece int, begin uint32) (newBlock, pieceDone bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if piece < 0 || piece >= p.nPieces || p.done[piece] {
		// 已落定 piece 的陈旧/重复交付:忽略。若重新登记,downTotal 会被
		// 陈旧副本反复累加(进度虚高超过 100%),位图状态也会被污染。
		return false, false
	}
	ps := &p.pcs[piece]
	b := int(begin) / BlockSize
	if int64(b) >= int64(ps.blocks) {
		return false, false
	}
	if ps.haveB == nil {
		ps.haveB = make([]byte, (ps.blocks+7)/8)
	}
	if ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0 {
		return false, false
	}
	ps.haveB[b/8] |= 1 << (7 - uint(b%8))
	ps.have++
	ps.haveBytes += p.blockLen(int64(piece), int64(b)) // 块粒度进度:到货即推进
	// 到货即满足本次借出:清除请求标记并核销在途计数。
	// 否则 free() 持续失真,多块 piece 会被误判为无块可领而停摆。
	if ps.reqB != nil && ps.reqB[b/8]&(1<<(7-uint(b%8))) != 0 {
		ps.reqB[b/8] &^= 1 << (7 - uint(b%8))
		ps.req--
		p.inflBlocks--
	} else if ps.freeCnt > 0 {
		// 请求已先被取消(超时/断开)并把块归还空闲池,数据这才到达:
		// 块现归我们持有,必须移出池子,否则会被再次认领(见 claimBlock
		// 的 haveB 检查)。freeCnt 为 0 说明池已清空(piece 已全块到货),
		// 无需处理。
		ps.freeCnt--
		p.freeBlocks--
	}
	newBlock = true
	if ps.have == ps.blocks {
		pieceDone = true
		// 全部块已持有:空闲计数清零,仍在途的重复请求随之作废
		p.freeBlocks -= int64(ps.freeCnt)
		ps.freeCnt = 0
		p.inflBlocks -= int64(ps.req)
		ps.req = 0
		ps.reqB = nil
	}
	return newBlock, pieceDone
}

// MarkDone 在 piece 校验通过后落定。
func (p *PiecePicker) MarkDone(piece int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markDoneLocked(piece)
}

func (p *PiecePicker) markDoneLocked(piece int) {
	if p.done[piece] {
		return
	}
	p.done[piece] = true
	p.donePieces++
	ps := &p.pcs[piece]
	p.unfiBlocks -= int64(ps.blocks)
	p.freeBlocks -= int64(ps.freeCnt)
	ps.freeCnt = 0
	ps.haveB = nil
	ps.reqB = nil
	ps.have = ps.blocks
	ps.req = 0
}

// ResetPiece 在校验失败后把 piece 打回待下载。
func (p *PiecePicker) ResetPiece(piece int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done[piece] {
		return
	}
	ps := &p.pcs[piece]
	// 归还在途借出;全局空闲数按该 piece 原贡献差额回补
	p.inflBlocks -= int64(ps.req)
	p.freeBlocks += int64(ps.blocks) - int64(ps.freeCnt)
	ps.have = 0
	ps.haveBytes = 0 // 校验失败:数据不可信,进度回退
	ps.req = 0
	ps.freeCnt = ps.blocks
	ps.haveB = nil
	ps.reqB = nil
}

// Cancel 归还一个借出块(peer 断开或重复到货后取消)。
// 借出标记可能已被并发的到货清除(该块已持有):此时只核销在途计数。
func (p *PiecePicker) Cancel(piece int, begin uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if piece < 0 || piece >= p.nPieces || p.done[piece] {
		return
	}
	ps := &p.pcs[piece]
	b := int(begin) / BlockSize
	if int64(b) >= int64(ps.blocks) || ps.reqB == nil {
		// reqB 已清空:piece 完成时借出已被整体核销,不重复扣减
		return
	}
	hadReq := ps.reqB[b/8]&(1<<(7-uint(b%8))) != 0
	var isHave bool
	if ps.haveB != nil {
		isHave = ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0
	}
	if !hadReq && !isHave {
		return
	}
	if hadReq {
		ps.reqB[b/8] &^= 1 << (7 - uint(b%8))
		ps.req--
		if !isHave {
			ps.freeCnt++
			p.freeBlocks++
		}
	}
	p.inflBlocks--
}

// ReserveWholePiece 为 WebSeed 之类的整 piece 拉取者预留一个完整的空闲 piece
// (全部块一次性借出,peer 块级认领将跳过它)。has 为 nil 表示不挑来源。
// 成功后:校验通过走 MarkDone(整体核销);失败走 ResetPiece(整体归还)。
func (p *PiecePicker) ReserveWholePiece(has func(int) bool) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freeBlocks <= 0 || p.unfiBlocks <= 0 {
		return 0, false
	}
	for i := 0; i < p.nPieces; i++ {
		idx := int((p.cursor + uint32(i)) % uint32(p.nPieces))
		if p.done[idx] {
			continue
		}
		ps := &p.pcs[idx]
		if ps.freeCnt != ps.blocks {
			continue // 已有块被认领/持有
		}
		if has != nil && !has(idx) {
			continue
		}
		if ps.reqB == nil {
			ps.reqB = make([]byte, (ps.blocks+7)/8)
		}
		for b := int32(0); b < ps.blocks; b++ {
			ps.reqB[b/8] |= 1 << (7 - uint(b%8))
		}
		ps.freeCnt = 0
		ps.req = ps.blocks
		p.freeBlocks -= int64(ps.blocks)
		p.inflBlocks += int64(ps.blocks)
		p.cursor = uint32((idx + 1) % p.nPieces)
		return idx, true
	}
	return 0, false
}

// ResolveWholePiece 整 piece 借出(WebSeed)成功路径:核销整体借出后落定。
// MarkDone 只负责落定统计,借出的冲销由这里先行完成(正常块路径由
// RecordBlock 的 piece 完成分支冲销)。
func (p *PiecePicker) ResolveWholePiece(idx int) {
	p.FinalizePiece(idx)
}

// FinalizePiece 落定一个已全块到货的 piece,兼容整片预留(WebSeed,req>0)
// 与块级(RecordBlock 已自行核销在途)两条路径。
func (p *PiecePicker) FinalizePiece(idx int) {
	p.mu.Lock()
	ps := &p.pcs[idx]
	if ps.req > 0 {
		p.inflBlocks -= int64(ps.req)
		ps.req = 0
	}
	ps.reqB = nil
	p.mu.Unlock()
	p.MarkDone(idx)
}

// needed 报告 piece 是否属于下载需求(文件选择排除的 piece 虽标记落定,
// 但对外不视作"持有"——数据并不存在,不能广播给 peer,更不能被请求服务)。
func (p *PiecePicker) needed(i int) bool {
	return p.wanted == nil || p.wanted[i]
}

// IsDone 报告某 piece 是否已落定且属于需求(可对外声明/服务)。
func (p *PiecePicker) IsDone(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done[i] && p.needed(i)
}

// Have 返回"已落定且需要"的 piece 位图快照(文件选择排除的 piece 恒为 false)。
func (p *PiecePicker) Have() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]bool, p.nPieces)
	for i := range out {
		out[i] = p.done[i] && p.needed(i)
	}
	return out
}

// CompletedPieces 返回已落定 piece 数。
func (p *PiecePicker) CompletedPieces() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.donePieces
}

// Endgame 报告是否处于末局模式。
func (p *PiecePicker) Endgame() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unfiBlocks > 0 && p.freeBlocks == 0
}

// Inflight 返回在途请求块数。
func (p *PiecePicker) Inflight() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflBlocks
}

// ArrivedBytes 返回已收到并落盘的字节(块粒度,含未完成 piece 的已收块)。
// 作为进度条口径:每 16KB 到货即平滑推进,不等 piece 校验;重复/陈旧
// 交付不重复计数,校验失败(ResetPiece)会回退。文件选择下载时只统计
// 需求内的 piece——改选前全量会话留在盘上的非需求 piece 不算进度,
// 否则 arrived 会大于所选文件总量(实测 457MB 任务显示已下 684MB)。
func (p *PiecePicker) ArrivedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for i := range p.pcs {
		if p.wanted != nil && !p.wanted[i] {
			continue
		}
		n += p.pcs[i].haveBytes
	}
	return n
}

// SnapshotBlocks 导出未完成 piece 的已收块位图:按固定步长(满 piece 的
// 块数向上取整字节)逐 piece 拼接;已落定 piece 的位为 0。配合 Bitfield
// 持久化,暂停/重启后可还原在途 piece 的部分块,不再整 piece 重下。
// SnapshotBlocks 导出未完成 piece 的已收块位图(调用方负责先落盘缓冲,
// 保证账里的块都在盘上——见 Client.SnapshotBlocks)。
func (p *PiecePicker) SnapshotBlocks() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	stride := (int(p.pieceLen+BlockSize-1)/BlockSize + 7) / 8
	out := make([]byte, p.nPieces*stride)
	for i := range p.pcs {
		if p.done[i] || p.pcs[i].haveB == nil {
			continue
		}
		copy(out[i*stride:(i+1)*stride], p.pcs[i].haveB)
	}
	return out
}

// RestoreBlocks 还原暂停前保存的块级位图(data 为 SnapshotBlocks 的输出)。
// 已收块直接标记持有——盘上数据是上个会话所写,信任之,最终仍由 piece
// 校验兜底(坏块导致校验失败时整 piece 重下,与旧行为一致)。只补缺块,
// 不再整 piece 重下。全块到齐的 piece 立即走 verify 校验,通过则落定。
// data 长度不符(piece 数/块大小变了)时整体忽略,退回整 piece 恢复。
func (p *PiecePicker) RestoreBlocks(data []byte, verify func(piece int) bool) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stride := (int(p.pieceLen+BlockSize-1)/BlockSize + 7) / 8
	if len(data) != p.nPieces*stride {
		return
	}
	for i := range p.pcs {
		if p.done[i] {
			continue
		}
		ps := &p.pcs[i]
		bits := data[i*stride : (i+1)*stride]
		cnt := int32(0)
		for b := int32(0); b < ps.blocks; b++ {
			if bits[b/8]&(1<<(7-uint(b%8))) != 0 {
				cnt++
			}
		}
		if cnt == 0 {
			continue
		}
		if cnt == ps.blocks {
			// 暂停时块已收齐但校验未及完成:与 onWholePiece 同路径落定
			if verify != nil && verify(i) {
				ps.have = ps.blocks
				ps.haveBytes = p.lenFn(i)
				p.markDoneLocked(i)
			}
			continue // 校验失败:数据不可信,保持全块空闲重下
		}
		if ps.haveB == nil {
			ps.haveB = make([]byte, (ps.blocks+7)/8)
		}
		copy(ps.haveB, bits)
		var bytes int64
		for b := int32(0); b < ps.blocks; b++ {
			if ps.haveB[b/8]&(1<<(7-uint(b%8))) != 0 {
				bytes += p.blockLen(int64(i), int64(b))
			}
		}
		ps.have = cnt
		ps.haveBytes = bytes
		ps.freeCnt = ps.blocks - cnt
		p.freeBlocks -= int64(cnt)
	}
}

// AllDone 报告是否全部落定。
func (p *PiecePicker) AllDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.donePieces == p.nPieces
}

// ---------- 边下边播优先级 ----------

// EnableStreaming 开启流模式优先级:pinHead/pinTail 为元数据 piece 区间
// (文件头/尾),ahead 为播放窗口宽度(piece 数)。顺序深度按 4MB 目标字节
// 折算(piece 越大深度越小,frontier 集中越强),钳在 [2,16]。重复调用
// 直接覆盖旧状态。
func (p *PiecePicker) EnableStreaming(pinHead, pinTail []int, ahead int) {
	if ahead < 1 {
		ahead = 1
	}
	depth := int((streamDepthBytes + p.pieceLen - 1) / p.pieceLen)
	if depth < 2 {
		depth = 2
	}
	if depth > 16 {
		depth = 16
	}
	if depth > ahead {
		depth = ahead
	}
	p.mu.Lock()
	p.stream = &streamState{
		cursor:  0,
		ahead:   ahead,
		depth:   depth,
		pinHead: append([]int(nil), pinHead...),
		pinTail: append([]int(nil), pinTail...),
		tailOn:  true,
	}
	p.mu.Unlock()
}

// UpdateStreamPins 元数据探测完成后精化 pin 区间:整体替换头/尾 pin(nil =
// 该项不变)并可同时解除尾部(faststart:moov 在头,尾部无索引可省)。
// pin 下标为全局口径,可超出发开流时的默认 1MB 范围(moov 大于 1MB 时必须
// 扩,否则索引残缺);也用于剧集预热把下一集头尾 piece 并入 pin(引擎侧
// 自行合并后传入)。
func (p *PiecePicker) UpdateStreamPins(pinHead, pinTail []int, tailOn bool) {
	p.mu.Lock()
	if p.stream != nil {
		if pinHead != nil {
			p.stream.pinHead = append([]int(nil), pinHead...)
		}
		if pinTail != nil {
			p.stream.pinTail = append([]int(nil), pinTail...)
		}
		p.stream.tailOn = tailOn
	}
	p.mu.Unlock()
}

// SetStreamAhead 重设播放窗口宽度(piece 数,引擎按探测出的码率 × 目标缓冲
// 秒数折算)。深度随之重钳;游标不动。
func (p *PiecePicker) SetStreamAhead(ahead int) {
	if ahead < 1 {
		ahead = 1
	}
	p.mu.Lock()
	if p.stream != nil {
		p.stream.ahead = ahead
		if p.stream.depth > ahead {
			p.stream.depth = ahead
		}
	}
	p.mu.Unlock()
}

// DisableStreaming 关闭流模式,完全回到稀有优先。幂等。
func (p *PiecePicker) DisableStreaming() {
	p.mu.Lock()
	p.stream = nil
	p.mu.Unlock()
}

// SetStreamCursor 把播放头移到 piece(窗口随之滑移;拖动/顺序播放共用)。
func (p *PiecePicker) SetStreamCursor(piece int) {
	p.mu.Lock()
	if p.stream != nil {
		if piece < 0 {
			piece = 0
		}
		if piece >= p.nPieces {
			piece = p.nPieces - 1
		}
		p.stream.cursor = piece
	}
	p.mu.Unlock()
}

// UnpinStreamTail 解除尾部元数据 pin(faststart 确认后调用,省下带宽给窗口)。
func (p *PiecePicker) UnpinStreamTail() {
	p.mu.Lock()
	if p.stream != nil {
		p.stream.tailOn = false
	}
	p.mu.Unlock()
}

// Streaming 报告流模式是否生效。
func (p *PiecePicker) Streaming() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stream != nil
}

// StreamState 导出流模式优先级状态(播放面板分块图展示用)。
// ok=false 表示未开启流模式;piece 下标均为全局口径。
func (p *PiecePicker) StreamState() (cursor, ahead, depth int, pinHead, pinTail []int, tailOn bool, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stream
	if s == nil {
		return 0, 0, 0, nil, nil, false, false
	}
	return s.cursor, s.ahead, s.depth, s.pinHead, s.pinTail, s.tailOn, true
}
