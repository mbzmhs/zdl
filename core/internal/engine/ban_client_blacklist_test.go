package engine

import (
	"strings"
	"testing"

	"zdownloader/internal/conf"
)

// TestBanClientNameRecordsBlacklist 右键「封禁所有该客户端」:除封现存同名
// IP 外,须把完整客户端名并入持久客户端黑名单(落盘 + 对运行中会话热生效),
// 使之后任意 IP 出现该客户端握手期即被自动封禁;同名幂等不重复、不同名追加。
func TestBanClientNameRecordsBlacklist(t *testing.T) {
	// DHT 关掉,避免 UpdateConfig 热同步时起停 DHT 节点(测试里不联网)。
	m := newTestManager(t, func(c *conf.Config) { c.BT.DHT = false })

	// 无运行中任务:封 0 个现存 IP,但黑名单仍应记下完整名(精确、无通配)。
	if n := m.BanClientName("qBittorrent 4.6.7"); n != 0 {
		t.Fatalf("无任务应封 0 个现存 IP, got %d", n)
	}
	bl := m.cfg.BT.ClientBlacklist
	if len(bl) != 1 || bl[0] != "qBittorrent 4.6.7" {
		t.Fatalf("应按完整名并入客户端黑名单, got %v", bl)
	}

	// 幂等:重复封同名不再重复入名单
	m.BanClientName("qBittorrent 4.6.7")
	if len(m.cfg.BT.ClientBlacklist) != 1 {
		t.Fatalf("重复封禁不应重复入名单, got %v", m.cfg.BT.ClientBlacklist)
	}

	// 不同完整名追加
	m.BanClientName("BitComet")
	got := m.cfg.BT.ClientBlacklist
	if len(got) != 2 {
		t.Fatalf("不同客户端应追加为 2 条, got %v", got)
	}
	found := false
	for _, p := range got {
		if strings.EqualFold(p, "BitComet") {
			found = true
		}
	}
	if !found {
		t.Fatalf("黑名单应含 BitComet, got %v", got)
	}
}
