package bt

import "testing"

// 边下边播优先级:三级扫描顺序(元数据 pin → 播放窗口严格顺序 → 稀有优先)、
// 游标跳转、尾部 pin 解除、关闭恢复。pieceLen=BlockSize 使每片恰一块,
// 一次 Claim 即借空整片,便于断言顺序。

func newStreamPicker(n int) *PiecePicker {
	plen := int64(BlockSize)
	return NewPicker(n, plen, plen*int64(n), nil, nil, nil)
}

// claimDone 认领并交付当前优先级最高的 piece,返回其下标。
func claimDone(t *testing.T, p *PiecePicker, has func(int) bool) int {
	t.Helper()
	piece, _, _, ok := p.Claim(has)
	if !ok {
		t.Fatal("无可认领块")
	}
	if _, done := p.RecordBlock(piece, 0); !done {
		t.Fatalf("piece %d 单块 piece 应即完成", piece)
	}
	p.MarkDone(piece)
	return piece
}

func allHave(int) bool { return true }

func TestPickerStreamPriorityOrder(t *testing.T) {
	p := newStreamPicker(20)
	// 无可用度信息时稀有优先不可用:流模式分支只依赖 has()
	p.EnableStreaming([]int{0}, []int{19}, 4)
	p.SetStreamCursor(2)
	if got := claimDone(t, p, allHave); got != 0 {
		t.Fatalf("一级应为头部 pin(piece 0),得到 %d", got)
	}
	if got := claimDone(t, p, allHave); got != 19 {
		t.Fatalf("一级应为尾部 pin(piece 19),得到 %d", got)
	}
	for want := 2; want <= 5; want++ { // 窗口 [2,2+4):严格顺序
		if got := claimDone(t, p, allHave); got != want {
			t.Fatalf("窗口顺序应为 %d,得到 %d", want, got)
		}
	}
	// 窗口耗尽:流模式分支找不到可认领块,回落稀有优先(需有可用度记录)
	allTrue := make([]bool, 20)
	for i := range allTrue {
		allTrue[i] = true
	}
	p.AddAvailability(allTrue)
	if piece, _, _, ok := p.Claim(allHave); !ok {
		t.Fatal("窗口耗尽后应回落稀有优先")
	} else if piece >= 2 && piece <= 5 || piece == 0 || piece == 19 {
		t.Fatalf("回落稀有优先不应重复已完成/窗口内 piece,得到 %d", piece)
	}
}

func TestPickerStreamCursorJump(t *testing.T) {
	p := newStreamPicker(20)
	p.EnableStreaming(nil, nil, 4)
	p.SetStreamCursor(2)
	if got := claimDone(t, p, allHave); got != 2 {
		t.Fatalf("应从游标 2 开始,得到 %d", got)
	}
	p.SetStreamCursor(10) // 拖动:窗口整体滑移
	if got := claimDone(t, p, allHave); got != 10 {
		t.Fatalf("跳转后应从 10 开始,得到 %d", got)
	}
}

func TestPickerStreamTailUnpin(t *testing.T) {
	p := newStreamPicker(20)
	p.EnableStreaming([]int{0}, []int{19}, 2)
	p.SetStreamCursor(5)
	if got := claimDone(t, p, allHave); got != 0 {
		t.Fatalf("头部 pin 优先,得到 %d", got)
	}
	p.UnpinStreamTail() // faststart 确认:尾部不再优先
	if got := claimDone(t, p, allHave); got != 5 {
		t.Fatalf("解除尾部 pin 后应进窗口(piece 5),得到 %d", got)
	}
}

func TestPickerStreamDisable(t *testing.T) {
	p := newStreamPicker(20)
	p.AddAvailability(make([]bool, 20)) // 全部可用度 1:与流模式分支可区分
	p.EnableStreaming(nil, nil, 2)
	p.SetStreamCursor(5)
	if !p.Streaming() {
		t.Fatal("流模式应已开启")
	}
	if got := claimDone(t, p, allHave); got != 5 {
		t.Fatalf("流模式应从游标 5 开始,得到 %d", got)
	}
	p.DisableStreaming()
	if p.Streaming() {
		t.Fatal("流模式应已关闭")
	}
	// 关闭后不再按窗口顺序:piece 6 可用度人为调高(更稀有者优先)
	for i := 0; i < 3; i++ {
		p.AddHave(7) // piece 7 更稀有?可用度升高=更多 peer 持有=更不稀有
	}
	_ = 7
	// 只验证关闭后仍可正常认领(顺序回归稀有优先语义,细节由既有测试覆盖)
	if _, _, _, ok := p.Claim(allHave); !ok {
		t.Fatal("关闭流模式后应仍可认领")
	}
}

// TestPickerStreamWantedSkip 文件选择排除的 piece(标记落定)不会被流模式
// 三级扫描认领;pin 尾部先于窗口生效。
func TestPickerStreamWantedSkip(t *testing.T) {
	n := 8
	wanted := make([]bool, n)
	for i := range wanted {
		wanted[i] = true
	}
	wanted[0] = false // 头 piece 属未选文件:pin 也不认领
	plen := int64(BlockSize)
	p := NewPicker(n, plen, plen*int64(n), nil, nil, wanted)
	p.EnableStreaming([]int{0}, []int{7}, 4)
	p.SetStreamCursor(0)
	if got := claimDone(t, p, allHave); got != 7 {
		t.Fatalf("未选 piece 0 的头部 pin 应跳过;尾部 pin(7)先于窗口,得到 %d", got)
	}
	if got := claimDone(t, p, allHave); got != 1 {
		t.Fatalf("窗口应跳过未选 piece 0 从 1 开始,得到 %d", got)
	}
}

// TestPickerStreamForwardOnly 流模式三级后台只向前:窗口末端之后的 piece
// 优先于游标之前的旧位置;前方无块可下时才兜底放开旧位置。
func TestPickerStreamForwardOnly(t *testing.T) {
	n := 10
	plen := int64(BlockSize)
	allTrue := make([]bool, n)
	for i := range allTrue {
		allTrue[i] = true
	}
	// 前方(6..9)有货:三级应只认领前方,不碰游标前的 0..3
	have := make([]bool, n)
	for _, i := range []int{4, 5} { // 窗口 [4,6) 已完成
		have[i] = true
	}
	p := NewPicker(n, plen, plen*int64(n), nil, have, nil)
	p.AddAvailability(allTrue)
	p.EnableStreaming(nil, nil, 2)
	p.SetStreamCursor(4)
	piece, _, _, ok := p.Claim(allHave)
	if !ok || piece < 6 {
		t.Fatalf("三级只向前应认领 ≥6 的 piece,得到 %d ok=%v", piece, ok)
	}
	// 前方全部完成:兜底放开旧位置(0..3),任务仍能推进
	have2 := make([]bool, n)
	for i := 4; i < n; i++ {
		have2[i] = true
	}
	p2 := NewPicker(n, plen, plen*int64(n), nil, have2, nil)
	p2.AddAvailability(allTrue)
	p2.EnableStreaming(nil, nil, 2)
	p2.SetStreamCursor(4)
	piece2, _, _, ok2 := p2.Claim(allHave)
	if !ok2 || piece2 > 3 {
		t.Fatalf("前方无货应兜底认领旧位置 ≤3,得到 %d ok=%v", piece2, ok2)
	}
}

// TestPickerStreamHeadEndgame 窗口头 mini-endgame:窗口头 piece 块已全部
// 借出但未交付时,其他 peer 可重复借其未到货块;交付完成后续认领继续前进。
func TestPickerStreamHeadEndgame(t *testing.T) {
	p := newStreamPicker(4)
	all := make([]bool, 4)
	for i := range all {
		all[i] = true
	}
	p.AddAvailability(all)
	p.EnableStreaming(nil, nil, 1) // 窗口 = {cursor}
	p.SetStreamCursor(0)
	// peer A 借走 piece 0 唯一块(未交付)
	pc, _, _, ok := p.Claim(allHave)
	if !ok || pc != 0 {
		t.Fatalf("首借应为 piece 0,得到 %d ok=%v", pc, ok)
	}
	// peer B:piece 0 无空闲块且窗口耗尽 → mini-endgame 重复借出同块
	pc2, _, _, ok2 := p.Claim(allHave)
	if !ok2 || pc2 != 0 {
		t.Fatalf("窗口头滞留应触发 mini-endgame 重复借 piece 0,得到 %d ok=%v", pc2, ok2)
	}
	// 任一副本交付:piece 0 完成,后续认领前进(≥1)
	if _, done := p.RecordBlock(0, 0); !done {
		t.Fatal("单块 piece 应即完成")
	}
	p.MarkDone(0)
	pc3, _, _, ok3 := p.Claim(allHave)
	if !ok3 || pc3 < 1 {
		t.Fatalf("完成后应继续向前认领,得到 %d ok=%v", pc3, ok3)
	}
}

// TestPickerStreamDepth 顺序深度:窗口内自首个未完成 piece 起只认领 depth
// 个 piece——多 peer 持有度不同也不会在窗口里散射,完成顺序贴播放顺序;
// 前沿完成即滑移。1 块/piece 使顺序可精确断言(depth 钳 16 < ahead 20)。
func TestPickerStreamDepth(t *testing.T) {
	n := 24
	p := newStreamPicker(n)
	p.EnableStreaming(nil, nil, 20) // 窗口 [0,20);16KB piece → depth=16
	_, ahead, depth, _, _, _, on := p.StreamState()
	if !on || ahead != 20 || depth != 16 {
		t.Fatalf("窗口/深度应为 20/16,得到 %d/%d on=%v", ahead, depth, on)
	}
	p.SetStreamCursor(0)
	// 依次借出前沿 0..15(深度内):全持有 peer 严格顺序推进
	for want := 0; want < 16; want++ {
		if pc, _, _, ok := p.Claim(allHave); !ok || pc != want {
			t.Fatalf("深度内应严格顺序 %d,得到 %d ok=%v", want, pc, ok)
		}
	}
	// 深度耗尽(0..15 全借出未完成):16 在窗口内但深度外,不得认领;
	// 触发前沿 mini-endgame 重复借 piece 0(peer 集中补前沿而非跳走)
	if pc, _, _, ok := p.Claim(allHave); !ok || pc != 0 {
		t.Fatalf("深度耗尽应 mini-endgame 补前沿 0,得到 %d ok=%v", pc, ok)
	}
	// 前沿 piece 0 完成:深度窗口滑到 [1,17),16 进入可认领
	if _, done := p.RecordBlock(0, 0); !done {
		t.Fatal("单块 piece 应即完成")
	}
	p.MarkDone(0)
	if pc, _, _, ok := p.Claim(allHave); !ok || pc != 16 {
		t.Fatalf("前沿完成后 16 应可认领,得到 %d ok=%v", pc, ok)
	}
	// 深度窗口 [1,17) 内 1..16 全部借出未完成:mini-endgame 补前沿 1;
	// 逐片交付使前沿滑移(借出≠完成,前沿不动深度不滑——真实场景里交付
	// 持续发生),之后 17..19 依序进入深度被认领
	if pc, _, _, ok := p.Claim(allHave); !ok || pc != 1 {
		t.Fatalf("深度耗尽应 mini-endgame 补前沿 1,得到 %d ok=%v", pc, ok)
	}
	for k := 1; k <= 16; k++ {
		if _, done := p.RecordBlock(k, 0); !done {
			t.Fatalf("piece %d 单块应即完成", k)
		}
		p.MarkDone(k)
	}
	for want := 17; want < 20; want++ {
		if pc, _, _, ok := p.Claim(allHave); !ok || pc != want {
			t.Fatalf("窗口内应顺序 %d,得到 %d", want, pc)
		}
	}
	// 窗口耗尽且前沿(17)滞留:mini-endgame 优先补前沿而非三级跳走
	//(三级"只向前"语义由 TestPickerStreamForwardOnly 覆盖)
	if pc, _, _, ok := p.Claim(allHave); !ok || pc != 17 {
		t.Fatalf("窗口耗尽应 mini-endgame 补前沿 17,得到 %d ok=%v", pc, ok)
	}
}
