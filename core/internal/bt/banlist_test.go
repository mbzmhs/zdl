package bt

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestBanListGlobalSharedAndPersist 全局封禁名单:跨会话共享(一端封禁、
// 两端生效并广播断连)、IP/时间/原因/次数持久化、解除与清空。
func TestBanListGlobalSharedAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	bl := NewBanList(path)

	meta := buildHubMeta(t, "ban.bin")
	mk := func() *Client {
		c, err := NewClient(meta, t.TempDir(), ClientConfig{BanList: bl}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		bl.attach(c) // 等价 Start 中的注册(测试不起会话协程)
		return c
	}
	c1, c2 := mk(), mk()

	// c1 上挂一条来自 6.6.6.6 的"现存连接"(net.Pipe 桩,closes 不报错)
	srv, cli := net.Pipe()
	defer srv.Close()
	pc := &peerConn{cl: c1, addr: "6.6.6.6:40000", conn: cli, send: make(chan outbound, 1), done: make(chan struct{})}
	c1.pMu.Lock()
	c1.peers["6.6.6.6:40000"] = pc
	c1.pMu.Unlock()

	// c2 侧手动封禁 → 名单两端可见,c1 的连接被广播踢断
	c2.BanIP("6.6.6.6")
	select {
	case <-pc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("封禁未广播断连 c1 的现存连接")
	}
	if !c1.isBannedIP("6.6.6.6") || !c2.isBannedIP("6.6.6.6") {
		t.Fatal("全局名单应对两个会话同时生效")
	}
	if got := c1.BannedIPs(); len(got) != 1 || got[0] != "6.6.6.6" {
		t.Fatalf("BannedIPs 快照不符: %v", got)
	}

	// 持久化:重新加载记录仍在(原因、次数、时间齐全)
	bl2 := NewBanList(path)
	es := bl2.List()
	if len(es) != 1 || es[0].IP != "6.6.6.6" || es[0].Count != 1 || es[0].Reason != "手动封禁" || es[0].Time.IsZero() {
		t.Fatalf("持久化记录不符: %+v", es)
	}
	// 重复封禁:次数累计、原因刷新、客户端名留档
	bl2.Ban("6.6.6.6", "毒源:累计供坏块 3 次", "qBittorrent 5.2.3")
	if e := bl2.List()[0]; e.Count != 2 || e.Reason != "毒源:累计供坏块 3 次" || e.Client != "qBittorrent 5.2.3" {
		t.Fatalf("重复封禁应累计次数并刷新原因/客户端: %+v", e)
	}
	// 解除单条后落盘,重启(重新加载)不再出现
	if !bl2.Remove("6.6.6.6") || bl2.Banned("6.6.6.6") {
		t.Fatal("Remove 应解除封禁")
	}
	// 清空
	bl2.Ban("7.7.7.7", "测试", "")
	if n := bl2.Clear(); n != 1 || bl2.Banned("7.7.7.7") {
		t.Fatalf("Clear 应清空全部(清了 %d 条)", n)
	}
	if es := NewBanList(path).List(); len(es) != 0 {
		t.Fatalf("解除/清空应持久化: %+v", es)
	}
}
