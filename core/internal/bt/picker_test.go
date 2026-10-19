package bt

import (
	"testing"
)

func allHas(int) bool { return true }

// BlockWanted 必须在写盘前拦住迟到/重复块:已持有的块与已完成 piece 一律
// 拒绝,否则迟到的重复交付会覆盖已校验 piece 上的好数据(实测 807 件中
// 8 件磁盘 SHA1 不符却仍被标记完成)。
func TestPickerBlockWanted(t *testing.T) {
	p := NewPicker(2, 16384, 2*16384, nil, nil, nil)
	if !p.BlockWanted(0, 0) {
		t.Fatal("未持有的块应可写")
	}
	if _, done := p.RecordBlock(0, 0); !done {
		t.Fatal("piece0 仅一块,登记后应完成")
	}
	if p.BlockWanted(0, 0) {
		t.Fatal("已持有的块不得再写")
	}
	p.MarkDone(0)
	if p.BlockWanted(0, 0) {
		t.Fatal("已完成 piece 的块不得再写")
	}
	if !p.BlockWanted(1, 0) {
		t.Fatal("未完成 piece 的新块应可写")
	}
}

func TestPickerRarestFirst(t *testing.T) {
	// 3 pieces,各 1 块;A 持有 {0,1},B 持有 {1,2} → piece1 可用度 2,0/2 为 1
	p := NewPicker(3, 16384, 3*16384, nil, nil, nil)
	a := []bool{true, true, false}
	b := []bool{false, true, true}
	p.AddAvailability(a)
	p.AddAvailability(b)

	first, _, _, ok := p.Claim(allHas)
	if !ok || (first != 0 && first != 2) {
		t.Fatalf("首个认领应取可用度 1 的 piece: %d", first)
	}
	second, _, _, _ := p.Claim(allHas)
	if second == first || (second != 0 && second != 2) {
		t.Fatalf("第二个应取另一个可用度 1 的 piece: %d", second)
	}
	third, _, _, _ := p.Claim(allHas)
	if third != 1 {
		t.Fatalf("第三个应取可用度 2 的 piece 1: %d", third)
	}
	// 三块全部借出 → 末局模式
	if !p.Endgame() {
		t.Fatal("应进入末局")
	}
	f4, b4, _, ok4 := p.Claim(allHas)
	if !ok4 || b4 != 0 {
		t.Fatalf("末局应允许重复借出(单块 piece 借块 0): ok=%v b=%d", ok4, b4)
	}
	if p.Inflight() != 4 {
		t.Fatalf("在途应为 4(3+1 重复): %d", p.Inflight())
	}
	// 末局块到货:该块原本已被借出,到货只满足一份;该 piece 的另一份(末局
	// 重复)借出由 RecordBlock 的完成分支整体核销。末局重复落在哪个 piece
	// 由选择策略决定,这里按实际认领的 piece 断言。
	if nb, done := p.RecordBlock(f4, 0); !nb || !done {
		t.Fatalf("单块 piece 到货应完成: new=%v done=%v", nb, done)
	}
	if p.Inflight() != 2 {
		t.Fatalf("完成核销后在途应为 2: %d", p.Inflight())
	}
	// 另一 piece 的块到货两次:第二次应被识别为重复
	g := 0
	if g == f4 {
		g = 1
	}
	if nb, _ := p.RecordBlock(g, 0); !nb {
		t.Fatal("块到货应为新块")
	}
	if nb, _ := p.RecordBlock(g, 0); nb {
		t.Fatal("重复到货应被识别")
	}
	if p.Inflight() != 1 {
		t.Fatalf("仅剩一份借出在途: %d", p.Inflight())
	}
}

func TestPickerRotationAndBlocks(t *testing.T) {
	// 单 piece 65536+1024 字节 → 5 块(4×16384 + 1024)
	pieceLen := int64(66560)
	p := NewPicker(1, pieceLen, pieceLen, nil, nil, nil)
	p.AddAvailability([]bool{true})
	seen := map[int]bool{}
	for i := 0; i < 5; i++ {
		piece, begin, length, ok := p.Claim(allHas)
		if !ok || piece != 0 {
			t.Fatalf("claim %d: %v piece=%d", i, ok, piece)
		}
		if seen[begin] {
			t.Fatalf("块偏移重复: %d", begin)
		}
		seen[begin] = true
		wantLen := 16384
		if begin == 4*16384 {
			wantLen = 1024
		}
		if length != wantLen {
			t.Fatalf("begin=%d 长度=%d want %d", begin, length, wantLen)
		}
	}
	if _, _, _, ok := p.Claim(allHas); !ok {
		t.Fatal("应进入末局允许重复")
	}
	// 依次到货,最后一块触发 pieceDone
	for begin := 0; begin < 4*16384; begin += 16384 {
		_, done := p.RecordBlock(0, uint32(begin))
		if begin < 4*16384 && done {
			t.Fatal("不应提前完成")
		}
	}
	_, done := p.RecordBlock(0, 4*16384)
	if !done {
		t.Fatal("末块应触发完成")
	}
	if !p.Endgame() {
		t.Fatal("校验前仍算未落定")
	}
	p.MarkDone(0)
	if !p.AllDone() || p.CompletedPieces() != 1 {
		t.Fatal("MarkDone 后应全部完成")
	}
	if _, _, _, ok := p.Claim(allHas); ok {
		t.Fatal("完成后不应再认领")
	}
}

func TestPickerVerifyFailReset(t *testing.T) {
	p := NewPicker(1, 16384, 16384, nil, nil, nil)
	p.AddAvailability([]bool{true})
	p.Claim(allHas)
	if _, done := p.RecordBlock(0, 0); !done {
		t.Fatal("应完成")
	}
	if p.Endgame() { // have=blocks,free=0,unfi=1块 → 校验未过时仍算未完成
		t.Log("piece 到货但未落定:不可再认领新块是预期(无空闲块)")
	}
	p.ResetPiece(0)
	if p.Endgame() {
		t.Fatal("重置后不应处于末局")
	}
	if p.Inflight() != 0 {
		t.Fatalf("重置后在途应为 0: %d", p.Inflight())
	}
	piece, begin, _, ok := p.Claim(allHas)
	if !ok || piece != 0 || begin != 0 {
		t.Fatalf("重置后应可重新认领: %v %d %d", ok, piece, begin)
	}
}

func TestPickerResumeAndPeerLeave(t *testing.T) {
	have := []bool{true, true, false, false}
	p := NewPicker(4, 16384, 4*16384, nil, have, nil)
	if p.CompletedPieces() != 2 {
		t.Fatalf("续传恢复应有 2 个完成 piece: %d", p.CompletedPieces())
	}
	peer := []bool{false, false, true, true}
	p.AddAvailability(peer)
	if _, _, _, ok := p.Claim(func(i int) bool { return peer[i] }); !ok {
		t.Fatal("应认领 piece 2/3")
	}
	p.RemoveAvailability(peer)
	// peer 离开后,has 只允许它持有的 piece → 无块可认领
	if _, _, _, ok := p.Claim(func(i int) bool { return peer[i] }); ok {
		t.Fatal("peer 全部离开后不应认领到块")
	}
	// 另一个持有 piece 2/3 的 peer 加入后仍可认领
	p.AddAvailability(peer)
	if _, _, _, ok := p.Claim(func(i int) bool { return i >= 2 }); !ok {
		t.Fatal("应仍可认领 piece 2/3")
	}
}

func TestPickerHaveFilter(t *testing.T) {
	p := NewPicker(2, 16384, 2*16384, nil, nil, nil)
	p.AddAvailability([]bool{true, false})
	// 只持有 piece0 的 peer
	piece, _, _, ok := p.Claim(func(i int) bool { return i == 0 })
	if !ok || piece != 0 {
		t.Fatalf("应只认领 piece0: %d %v", piece, ok)
	}
	if _, _, _, ok := p.Claim(func(i int) bool { return i == 0 }); ok {
		t.Fatal("piece0 已借出且未到末局,不应重复")
	}
}

// 块级位图快照/还原:暂停重启后只补缺块,已收块不重下(消除进度倒退)。
func TestPickerSnapshotRestoreBlocks(t *testing.T) {
	const pieceLen = 4 * 16384 // 每 piece 4 块
	np := 4
	total := int64(np * pieceLen)
	p := NewPicker(np, pieceLen, total, nil, nil, nil)
	// piece0: 全部 4 块(模拟暂停时校验未及完成)
	// piece1: 块 0、2(部分)
	// piece2: 空;piece3: 空
	for b := 0; b < 4; b++ {
		p.RecordBlock(0, uint32(b*16384))
	}
	p.RecordBlock(1, 0)
	p.RecordBlock(1, 2*16384)

	snap := p.SnapshotBlocks()
	if len(snap) != np { // 4 块/piece → 4 bit → 1 字节步长
		t.Fatalf("快照步长不符: %d", len(snap))
	}
	// 已落定 piece 位图必须为 0:还原时跳过,交给 Bitfield 走校验路径
	p.MarkDone(2)
	snap2 := p.SnapshotBlocks()
	for i, v := range snap2[2:3] {
		if v != 0 {
			t.Fatalf("已落定 piece2 位图非零: off=%d v=%d", i, v)
		}
	}

	// 还原:piece0 校验通过落定;新会话只认领缺块
	r := NewPicker(np, pieceLen, total, nil, nil, nil)
	verified := map[int]bool{}
	r.RestoreBlocks(snap2, func(idx int) bool { verified[idx] = true; return true })
	if !verified[0] {
		t.Fatal("全块到齐的 piece0 应触发校验")
	}
	if !r.done[0] {
		t.Fatal("piece0 校验通过后应落定")
	}
	// piece1 还原后已收 2 块:进度口径 = piece0 全长 + 2 块
	wantBytes := int64(pieceLen + 2*16384)
	if got := r.ArrivedBytes() - r.ArrivedBytes()%1; got != wantBytes {
		t.Fatalf("还原后到货字节不符: got=%d want=%d", got, wantBytes)
	}
	// 认领不得落在已持有块上:piece1 的块 0/2 已持有
	seen := map[int]int{}
	for i := 0; i < 2; i++ {
		piece, begin, _, ok := r.Claim(allHas)
		if !ok {
			break
		}
		if piece != 1 && piece != 3 {
			t.Fatalf("不应认领 piece%d", piece)
		}
		seen[begin]++
		if piece == 1 && (begin == 0 || begin == 2*16384) {
			t.Fatalf("piece1 已持有块被重复认领: begin=%d", begin)
		}
	}
	// 校验失败:全块到齐的 piece 保持待下且不落定
	r2 := NewPicker(np, pieceLen, total, nil, nil, nil)
	r2.RestoreBlocks(snap2, func(int) bool { return false })
	if r2.done[0] {
		t.Fatal("校验失败的 piece0 不得落定")
	}
	if got := r2.ArrivedBytes(); got != 2*16384 {
		t.Fatalf("校验失败 piece0 不计进度,部分还原的 piece1 计 2 块: %d", got)
	}
	// 长度不符:整体忽略,不 panic
	r3 := NewPicker(np, pieceLen, total, nil, nil, nil)
	r3.RestoreBlocks([]byte{1, 2, 3}, func(int) bool { return true })
}

// ArrivedBytes 只计需求内的 piece:改选/文件选择前会话留在盘上的非需求
// piece(经初始位图带出 haveBytes)不得计入进度,否则 arrived 会大于所选
// 文件总量(实测 457MB 任务显示已下 684MB)。
func TestPickerArrivedBytesWanted(t *testing.T) {
	// piece0 需求且已有;piece1 非需求但盘上有数据(初始位图);piece2 需求待下
	wanted := []bool{true, false, true}
	have := []bool{true, true, false}
	p := NewPicker(3, 16384, 3*16384, nil, have, wanted)
	if got := p.ArrivedBytes(); got != 16384 {
		t.Fatalf("非需求 piece 的盘上数据不计进度: got=%d want=16384", got)
	}
	if _, done := p.RecordBlock(2, 0); !done {
		t.Fatal("单块 piece 应登记完成")
	}
	if got := p.ArrivedBytes(); got != 2*16384 {
		t.Fatalf("需求 piece 新收块应计入: got=%d want=%d", got, 2*16384)
	}
}
