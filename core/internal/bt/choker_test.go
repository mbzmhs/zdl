package bt

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func hasAddr(list []string, addr string) bool {
	for _, a := range list {
		if a == addr {
			return true
		}
	}
	return false
}

// TestChokerDownloadRanking 下载期:速率前 4 进常规槽,乐观槽给阻塞中
// 等待最久的;失去兴趣的现任立即让位;无兴趣新连接不占名单。
func TestChokerDownloadRanking(t *testing.T) {
	now := time.Now()
	peers := []chokerPeer{
		{addr: "e", rate: 50 << 10, interested: true, chokedAt: now.Add(-10 * time.Second)},
		{addr: "a", rate: 500 << 10, interested: true},
		{addr: "f", rate: 10 << 10, interested: true, chokedAt: now.Add(-50 * time.Second)},
		{addr: "b", rate: 300 << 10, interested: true},
		{addr: "c", rate: 200 << 10, interested: true},
		{addr: "d", rate: 100 << 10, interested: true},
		{addr: "g", rate: 900 << 10, interested: false, unchoked: true},
		{addr: "h", rate: 900 << 10, interested: false},
	}
	dec := decideChokes(peers, false, chokerState{}, now)

	// 常规槽 = 速率最高的 a,b,c,d;乐观槽 = 阻塞中等待最久的 f(-50s < -10s)
	for _, a := range []string{"a", "b", "c", "d", "f"} {
		if !hasAddr(dec.unchoke, a) {
			t.Errorf("期望放行 %s,got %v", a, dec.unchoke)
		}
	}
	if hasAddr(dec.unchoke, "e") {
		t.Errorf("e 未入选且非乐观槽,不应放行: %v", dec.unchoke)
	}
	// 失去兴趣的现任 g 让位;无兴趣新连接 h 不进任何名单
	if !hasAddr(dec.choke, "g") {
		t.Errorf("失去兴趣的现任 g 应被 choke,got %v", dec.choke)
	}
	if hasAddr(dec.choke, "h") || hasAddr(dec.unchoke, "h") {
		t.Errorf("无兴趣且未放行的 h 不应出现在名单: unchoke=%v choke=%v", dec.unchoke, dec.choke)
	}
}

// TestChokerOptimisticRotation 乐观槽 30s 轮换:到期换给等待次久者,
// 且它到期时已被常规槽遗弃(零速率进不了桶)→ 被 choke。
func TestChokerOptimisticRotation(t *testing.T) {
	now := time.Now()
	mk := func(unchoked map[string]bool, optAt time.Time) []chokerPeer {
		rates := map[string]int64{"a": 500 << 10, "b": 300 << 10, "c": 200 << 10, "d": 100 << 10}
		waits := map[string]time.Duration{"p4": 10 * time.Second, "p5": 100 * time.Second, "p6": 50 * time.Second}
		var out []chokerPeer
		for _, addr := range []string{"a", "b", "c", "d", "p4", "p5", "p6"} {
			p := chokerPeer{addr: addr, interested: true}
			if r, ok := rates[addr]; ok {
				p.rate = r
			}
			if w, ok := waits[addr]; ok {
				p.chokedAt = now.Add(-w)
			}
			if unchoked[addr] {
				p.unchoked = true
				p.unchokeAt = optAt
			}
			out = append(out, p)
		}
		return out
	}

	dec := decideChokes(mk(nil, time.Time{}), false, chokerState{}, now)
	if !hasAddr(dec.unchoke, "p5") {
		t.Fatalf("首轮乐观槽应给等待最久的 p5: %v", dec.unchoke)
	}
	if dec.state.optAddr != "p5" {
		t.Fatalf("state.optAddr=p5,got %q", dec.state.optAddr)
	}
	// 应用:5 个放行;31s 后重评
	apply := map[string]bool{}
	for _, a := range dec.unchoke {
		apply[a] = true
	}
	dec2 := decideChokes(mk(apply, now), false, dec.state, now.Add(chokerRotate+time.Second))
	// p5 仍是放行态但零速率不在常规槽;乐观槽到期 → 换给 p6(次久)
	if !hasAddr(dec2.unchoke, "p6") {
		t.Fatalf("轮换后乐观槽应为 p6: %v", dec2.unchoke)
	}
	if hasAddr(dec2.unchoke, "p5") {
		t.Fatalf("p5 乐观槽到期且零速率,应被 choke: %v", dec2.unchoke)
	}
	if !hasAddr(dec2.choke, "p5") {
		t.Fatalf("p5 应出现在 choke 名单: %v", dec2.choke)
	}
	if dec2.state.optAddr != "p6" {
		t.Fatalf("state.optAddr=p6,got %q", dec2.state.optAddr)
	}
}

// TestChokerSeedRotation 做种期:超容量按等待最久补 8 个;满员满 30s
// 后换血一半——服务最久的让位,等待最久的顶上。
func TestChokerSeedRotation(t *testing.T) {
	now := time.Now()
	mk := func(unchokeAt map[string]time.Time) []chokerPeer {
		var out []chokerPeer
		for i := 0; i < 10; i++ {
			addr := fmt.Sprintf("p%d", i) // p0..p9;p9 等得最久(-9s)
			p := chokerPeer{
				addr:       addr,
				interested: true,
				chokedAt:   now.Add(-time.Duration(i) * time.Second),
			}
			if at, ok := unchokeAt[addr]; ok {
				p.unchoked = true
				p.unchokeAt = at
			}
			out = append(out, p)
		}
		return out
	}

	dec := decideChokes(mk(nil), true, chokerState{}, now)
	if len(dec.unchoke) != chokerSeedSlots {
		t.Fatalf("首轮应放行 %d 个,got %d: %v", chokerSeedSlots, len(dec.unchoke), dec.unchoke)
	}
	// 等待最久的 8 个:p9..p2(各 -9s..-2s),p0/p1 排队
	for _, a := range []string{"p9", "p8", "p7", "p6", "p5", "p4", "p3", "p2"} {
		if !hasAddr(dec.unchoke, a) {
			t.Errorf("等待最久的 %s 应首批放行: %v", a, dec.unchoke)
		}
	}
	if dec.state.seedSince.IsZero() {
		t.Fatal("满员后 seedSince 应起算")
	}

	// 31s 后:8 个仍在服务(unchokeAt=now),p0/p1 还在等 → 换血
	apply := map[string]time.Time{}
	for _, a := range dec.unchoke {
		apply[a] = now
	}
	dec2 := decideChokes(mk(apply), true, dec.state, now.Add(chokerRotate+time.Second))
	if len(dec2.unchoke) != chokerSeedSlots-chokerSeedHalve+2 {
		t.Fatalf("换血后应为 %d 个(让4补2),got %d: %v",
			chokerSeedSlots-chokerSeedHalve+2, len(dec2.unchoke), dec2.unchoke)
	}
	// 等待最久的 p0/p1 顶上(它们 chokedAt 最接近 now,是 down 里最久的)
	if !hasAddr(dec2.unchoke, "p1") || !hasAddr(dec2.unchoke, "p0") {
		t.Errorf("排队中的 p1/p0 应被补位: %v", dec2.unchoke)
	}
	if len(dec2.choke) != chokerSeedHalve {
		t.Errorf("应恰有 %d 个让位被 choke,got %v", chokerSeedHalve, dec2.choke)
	}
}

// TestChokerSeedUnderCapacity 不超容量:全部 interested 都放行,无名单残留。
func TestChokerSeedUnderCapacity(t *testing.T) {
	now := time.Now()
	peers := []chokerPeer{
		{addr: "x", interested: true},
		{addr: "y", interested: true, unchoked: true, unchokeAt: now},
		{addr: "z", interested: false, unchoked: true},
	}
	dec := decideChokes(peers, true, chokerState{}, now)
	if !hasAddr(dec.unchoke, "x") || !hasAddr(dec.unchoke, "y") {
		t.Errorf("未超容量时 interested 应全放行: %v", dec.unchoke)
	}
	if !hasAddr(dec.choke, "z") {
		t.Errorf("失去兴趣的 z 应让位: %v", dec.choke)
	}
}

// TestChokerDownloadStickiness 同速率桶内现任优先:贴着阈值的小幅
// 速率差不构成换人理由,防反复 choke/unchoke 磨管线。
func TestChokerDownloadStickiness(t *testing.T) {
	now := time.Now()
	// 4 桶内(均 <128KB)的现任 + 一个桶内略高的阻塞者
	peers := []chokerPeer{
		{addr: "n1", rate: 70 << 10, interested: true, unchoked: true},
		{addr: "n2", rate: 66 << 10, interested: true, unchoked: true},
		{addr: "n3", rate: 65 << 10, interested: true, unchoked: true},
		{addr: "n4", rate: 64 << 10, interested: true, unchoked: true},
		{addr: "challenger", rate: 127 << 10, interested: true}, // 同桶(1)、速率最高
	}
	dec := decideChokes(peers, false, chokerState{}, now)
	for _, a := range []string{"n1", "n2", "n3", "n4"} {
		if !hasAddr(dec.unchoke, a) {
			t.Errorf("同桶现任 %s 不应被换掉: %v", a, dec.unchoke)
		}
	}
	// 挑战者同桶且速率更高,但常规槽被现任占满 → 只能进乐观槽(轮换身份),
	// 不构成对现任的换血;此处仅断言它未挤掉任何现任。
	if hasAddr(dec.choke, "n1") || hasAddr(dec.choke, "n2") ||
		hasAddr(dec.choke, "n3") || hasAddr(dec.choke, "n4") {
		t.Errorf("现任不应出现在 choke 名单: %v", dec.choke)
	}
}

// TestChokerWireSlots 线上行为(真实 TCP 对做种会话):
//  1. 未发 interested 不得收到 unchoke(BEP-3 连接始于 choked);
//  2. interested 后放行上限 4+1,第 6 者保持 choked;
//  3. choked 下发 request(fast)→ 显式 reject;
//  4. 断开一个放行者 →腾出的槽位立即补给第 6 者。
func TestChokerWireSlots(t *testing.T) {
	var dialSeq byte
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	// 被测方必须处于"下载中"(initialHave 全空 → 未完成),才走 tit-for-tat
	// 的 4+1 槽;做种模式是 8 槽轮换(见 TestChokerSeedRotation)。
	srv, err := NewClient(meta, t.TempDir(), ClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.ListenAddr())
	if err != nil {
		t.Fatalf("解析监听地址: %v", err)
	}

	type rawConn struct {
		conn net.Conn
		got  chan byte
	}
	dial := func() *rawConn {
		t.Helper()
		c, err := net.Dial("tcp4", "127.0.0.1:"+portStr)
		if err != nil {
			t.Fatalf("拨号: %v", err)
		}
		var pid [20]byte
		pid[0], pid[1] = '-', 'T'
		pid[2] = dialSeq // 每条连接唯一 peer_id,否则被 register 的同 id 去重拒绝
		dialSeq++
		// fast=true 以启用显式 reject;ext/dht 不需要
		if err := WriteHandshake(c, Handshake{InfoHash: meta.InfoHash, PeerID: pid}, false, false, true); err != nil {
			t.Fatalf("发送握手: %v", err)
		}
		if _, err := ReadHandshake(c); err != nil {
			t.Fatalf("读握手应答: %v", err)
		}
		r := &rawConn{conn: c, got: make(chan byte, 128)}
		go func() {
			defer close(r.got)
			for {
				m, err := ReadMsg(c)
				if err != nil {
					return
				}
				if m.ID == MsgKeepalive {
					continue
				}
				select {
				case r.got <- m.ID:
				default:
				}
			}
		}()
		return r
	}
	drain := func(r *rawConn, d time.Duration) []byte {
		var out []byte
		deadline := time.After(d)
		for {
			select {
			case id, ok := <-r.got:
				if !ok {
					return out
				}
				out = append(out, id)
			case <-deadline:
				return out
			}
		}
	}
	hasID := func(ids []byte, id byte) bool {
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}

	rs := make([]*rawConn, 6)
	for i := range rs {
		rs[i] = dial()
		defer rs[i].conn.Close()
	}
	// 1) 未发 interested:只有初始帧(haveall 等),不得有 unchoke/choke
	time.Sleep(1200 * time.Millisecond)
	for i, r := range rs {
		if ids := drain(r, 300*time.Millisecond); hasID(ids, msgUnchoke) {
			t.Fatalf("peer%d 在 interested 前收到 unchoke: %v", i, ids)
		}
	}
	// 2) 串行 interested → 放行上限 4+1
	for i, r := range rs {
		if err := WriteMsg(r.conn, msgInterested, nil); err != nil {
			t.Fatalf("peer%d 发送 interested: %v", i, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	unchokedAt := make([]bool, len(rs))
	for i, r := range rs {
		unchokedAt[i] = hasID(drain(r, 1500*time.Millisecond), msgUnchoke)
	}
	count, odd := 0, -1
	for i, u := range unchokedAt {
		if u {
			count++
		} else {
			odd = i
		}
	}
	if count != chokerSlots+chokerOptimistic {
		t.Fatalf("放行数应为 %d(4+1),got %d(%v)", chokerSlots+chokerOptimistic, count, unchokedAt)
	}
	if odd < 0 {
		t.Fatal("6 个 interested 应恰有 1 个保持 choked")
	}
	// 3) choked 者请求(fast)→ 显式 reject
	if err := WriteMsg(rs[odd].conn, msgRequest, BuildRequest(0, 0, 16384)); err != nil {
		t.Fatal(err)
	}
	if ids := drain(rs[odd], 2*time.Second); !hasID(ids, msgRejectReq) {
		t.Fatalf("choked 下的 request 应收到 reject,got %v", ids)
	}
	// 4) 断开一个放行者 →槽位立即补给被 choke 的第 6 者
	closed := false
	for i, u := range unchokedAt {
		if u {
			rs[i].conn.Close()
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("无可断开的放行者")
	}
	if ids := drain(rs[odd], 3*time.Second); !hasID(ids, msgUnchoke) {
		t.Fatalf("腾位后应补放行被 choke 的 peer,got %v", ids)
	}
}
