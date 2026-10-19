package bt

import (
	"net"
	"testing"
)

// TestPieceEfficiencyAccounting 连接详情「piece 有效率」计数:
// 参与并通过校验记"通过";校验失败记"失败",唯一来源另记"独占";
// 同一 IP 的多条连接共享一份账,PeerStats 按 IP 归口输出。
func TestPieceEfficiencyAccounting(t *testing.T) {
	meta := buildHubMeta(t, "eff.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	// 通过:两个来源各记一片通过
	c.countPieceResult(map[string]int{"1.1.1.1:1000": 4, "2.2.2.2:2000": 4}, true)
	// 失败:唯一来源 → 记失败 + 独占
	c.countPieceResult(map[string]int{"1.1.1.1:1000": 8}, false)
	// 失败:多来源 → 各记失败,不算独占
	c.countPieceResult(map[string]int{"1.1.1.1:1000": 1, "3.3.3.3:3000": 1}, false)
	// 空来源表(理论不该出现):不计任何账,也不 panic
	c.countPieceResult(nil, false)

	mkLeechPeer(t, c, "1.1.1.1:1000", 0, 0, -1, 0, 0)
	mkLeechPeer(t, c, "1.1.1.1:1001", 0, 0, -1, 0, 0) // 同 IP 第二条连接
	mkLeechPeer(t, c, "3.3.3.3:3000", 0, 0, -1, 0, 0)
	mkLeechPeer(t, c, "9.9.9.9:9000", 0, 0, -1, 0, 0) // 无样本

	type key struct{ ok, bad, sole int }
	got := map[string]key{}
	for _, s := range c.PeerStats() {
		pc, _, err := net.SplitHostPort(s.Addr)
		if err != nil {
			pc = s.Addr
		}
		got[pc] = key{s.PiecesOK, s.PiecesBad, s.SoleBad}
	}
	want := map[string]key{
		"1.1.1.1": {1, 2, 1}, // 通过 1 / 失败 2(其中独占 1)
		"2.2.2.2": {0, 0, 0}, // 无连接:不出现在 PeerStats 里,这里仅占位说明
		"3.3.3.3": {0, 1, 0},
		"9.9.9.9": {0, 0, 0},
	}
	for ip := range want {
		if ip == "2.2.2.2" {
			continue
		}
		if got[ip] != want[ip] {
			t.Fatalf("%s piece 有效率计数不符: got %+v want %+v", ip, got[ip], want[ip])
		}
	}
}
