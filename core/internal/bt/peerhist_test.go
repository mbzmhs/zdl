package bt

import (
	"net"
	"testing"
)

// TestPeerHistoryRoundTrip 验证 peer 历史只持久化"真正供过数据"与手动添加的
// 候选:DHT 广撒网拉来的空地址不落盘(否则重启后会浪费拨号预算去重试死地址)。
func TestPeerHistoryRoundTrip(t *testing.T) {
	root, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	pGood := PeerAddr{IP: net.IPv4(10, 0, 0, 1), Port: 100}
	pManual := PeerAddr{IP: net.IPv4(10, 0, 0, 2), Port: 200}
	pDHT := PeerAddr{IP: net.IPv4(10, 0, 0, 3), Port: 300}
	cl.rememberCandUTP(pGood, true, "pex")
	cl.rememberCandUTP(pManual, false, "manual")
	cl.rememberCandUTP(pDHT, false, "dht")
	cl.markCandGood(pGood.String(), true) // 只有它能供数

	hist := cl.PeerHistory()
	if len(hist) != 2 {
		t.Fatalf("历史 %d 条,期望 2(供过数据的 + 手动添加)", len(hist))
	}
	foundUTP := false
	for _, e := range hist {
		if e.Addr == pDHT.String() {
			t.Fatal("DHT 空候选不应进入持久化历史")
		}
		if e.UTP && e.Addr == pGood.String() {
			foundUTP = true
		}
	}
	if !foundUTP {
		t.Fatal("uTP 标记未导出")
	}

	// 新客户端灌入:数量正确、来源标记 history、uTP 保留
	cl2, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl2.Close()
	if n := cl2.LoadPeerHistory(hist); n != 2 {
		t.Fatalf("回灌 %d 条,期望 2", n)
	}
	if again := cl2.LoadPeerHistory(hist); again != 0 {
		t.Fatalf("重复回灌应去重,实际 %d", again)
	}
	for addr, wantUTP := range map[string]bool{pGood.String(): true, pManual.String(): false} {
		utp, src := cl2.candMeta(addr)
		if utp != wantUTP {
			t.Fatalf("%s uTP 标记 = %v,期望 %v", addr, utp, wantUTP)
		}
		if src != "history" {
			t.Fatalf("%s 来源 %q,期望 history", addr, src)
		}
	}
}

// TestCandRank 候选补拨优先级:供过数据最高,tracker/PEX/手动次之,
// DHT 最低(避免空地址挤占补拨额度)。
func TestCandRank(t *testing.T) {
	if candRank(candInfo{good: true, src: "dht"}) >= candRank(candInfo{seed: true, src: "pex"}) {
		t.Fatal("供过数据的候选应优先于种子候选")
	}
	if candRank(candInfo{seed: true, src: "pex"}) >= candRank(candInfo{src: "tracker"}) {
		t.Fatal("PEX 种子候选应优先于 tracker")
	}
	if candRank(candInfo{src: "tracker"}) >= candRank(candInfo{src: "dht"}) {
		t.Fatal("tracker 候选应优先于 DHT")
	}
	if candRank(candInfo{src: "pex"}) >= candRank(candInfo{src: "dht"}) {
		t.Fatal("PEX 候选应优先于 DHT")
	}
}

// TestAddPeerPEXSeedHint 校验 BEP-11 added.f 0x02(种子)被记录并抬升补拨优先级。
func TestAddPeerPEXSeedHint(t *testing.T) {
	root, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	p := PeerAddr{IP: net.IPv4(10, 0, 0, 9), Port: 9}
	cl.AddPeerPEX(p, 0x02) // 声明为种子
	cl.candMu.Lock()
	ci := cl.cands[p.String()]
	cl.candMu.Unlock()
	if !ci.seed {
		t.Fatal("PEX 种子标志 0x02 未被记录")
	}
	if ci.utp {
		t.Fatal("0x02 不应被误判为 uTP(0x04)")
	}
	if candRank(ci) != 1 {
		t.Fatalf("种子候选应排第 1 档,得到 %d", candRank(ci))
	}
}
// 入站 peer(经 tracker/DHT 找到我们)不在候选池:供数后必须建档进历史,
// 否则 20+ 供数连接只留下恰好出站拨号过的少数几条(实测回归)。
func TestPeerHistoryInboundGood(t *testing.T) {
	root, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	inbound := PeerAddr{IP: net.IPv4(10, 0, 1, 9), Port: 5050}
	cl.markCandGood(inbound.String(), true) // 从未 rememberCand,直接供数

	hist := cl.PeerHistory()
	if len(hist) != 1 || hist[0].Addr != inbound.String() {
		t.Fatalf("入站供数 peer 应建档进历史: %+v", hist)
	}
	if !hist[0].UTP {
		t.Fatal("uTP 传输标记应保留")
	}
	if r := candRank(cl.cands[inbound.String()]); r != 0 {
		t.Fatalf("供过数据的候选 rank=%d,期望 0", r)
	}
	// 非法地址:静默忽略,不 panic
	cl.markCandGood("not-an-addr", false)
}
