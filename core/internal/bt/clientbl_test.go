package bt

import (
	"net"
	"strings"
	"testing"
)

// TestMatchWildcard 通配符匹配:大小写不敏感,* 任意长度(含零),? 单字符;
// 客户端名里的 [] 按字面量(不用 path.Match 的字符类语义)。
func TestMatchWildcard(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*Xunlei*", "Xunlei 2.2.1", true},
		{"*xunlei*", "Xunlei 2.2.1", true}, // 大小写不敏感
		{"XL*", "xl0011", true},
		{"*4.6.7", "qBittorrent 4.6.7", true},
		{"qB*", "qBittorrent 4.6.7", true},
		{"Thunder ?", "Thunder 5", true},
		{"Thunder ??", "Thunder 5", false}, // ? 严格单字符
		{"Xunlei", "Xunlei 2.2.1", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "abc", true}, // * 可匹配零长度
		{"*", "任意内容", true},
		{"", "", true},
		{"", "x", false},
		{"BitComet 1.99", "BitComet 1.99", true},
		{"*[ANi]*", "peer [ANi] web-dl", true}, // [] 是字面量
		{"*ANi*", "peer [ani] web-dl", true},
	}
	for _, c := range cases {
		if got := matchWildcard(c.pat, c.s); got != c.want {
			t.Errorf("matchWildcard(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

// TestBlPtrNorm 模式表规整:去首尾空白、丢空行。
func TestBlPtrNorm(t *testing.T) {
	p := blPtr([]string{" *Xunlei* ", "", "  ", "*Thunder*"})
	got := *p
	if len(got) != 2 || got[0] != "*Xunlei*" || got[1] != "*Thunder*" {
		t.Fatalf("规整结果不符: %#v", got)
	}
	if len(*blPtr(nil)) != 0 || len(*blPtr([]string{"", " "})) != 0 {
		t.Fatal("空模式应被全部丢弃")
	}
}

// TestClientBlacklistPattern 客户端名→黑名单模式:完整名的精确形式(仅去首尾
// 空白,两端不加通配符)。并校验精确模式大小写不敏感命中自身、不命中别的版本。
func TestClientBlacklistPattern(t *testing.T) {
	cases := []struct{ name, want string }{
		{"qBittorrent 4.6.7", "qBittorrent 4.6.7"},
		{"BitComet", "BitComet"},
		{"  Xunlei 9.9  ", "Xunlei 9.9"}, // 去首尾空白
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := ClientBlacklistPattern(c.name); got != c.want {
			t.Errorf("ClientBlacklistPattern(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	// 精确(大小写不敏感)匹配:命中同名
	if !matchWildcard(ClientBlacklistPattern("qBittorrent 4.6.7"), "QBITTORRENT 4.6.7") {
		t.Error("精确模式应大小写不敏感地命中同名")
	}
	// 按完整名称口径:不命中别的版本
	if matchWildcard(ClientBlacklistPattern("qBittorrent 4.6.7"), "qBittorrent 5.0.0") {
		t.Error("精确模式不应命中别的版本")
	}
}

// TestClientBlacklistRegister 握手期拦截:命中黑名单的 peer 在注册期直接
// 拒绝并写入全局封禁名单;未命中的照常注册。
func TestClientBlacklistRegister(t *testing.T) {
	meta := buildHubMeta(t, "bl.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{
		ClientBlacklist: []string{"*Xunlei*"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var xlPID [20]byte
	copy(xlPID[:], "-XL0011-xunlei-leech000")
	if !strings.Contains(ClientName(xlPID), "Xunlei") {
		t.Fatalf("前置校验:-XL0011- 应识别为 Xunlei,得到 %q", ClientName(xlPID))
	}
	srv1, cli1 := net.Pipe()
	t.Cleanup(func() { srv1.Close(); cli1.Close() })
	if pc := c.register("6.6.6.6:40000", &Handshake{PeerID: xlPID}, cli1); pc != nil {
		t.Fatal("命中黑名单的 peer 应在注册期被拒绝")
	}
	if !c.banList.Banned("6.6.6.6") {
		t.Fatal("命中黑名单的 IP 应写入封禁名单")
	}
	found := false
	for _, e := range c.banList.List() {
		if e.IP == "6.6.6.6" && strings.Contains(e.Reason, "*Xunlei*") &&
			strings.Contains(e.Reason, "客户端黑名单") && strings.Contains(e.Client, "Xunlei") {
			found = true
		}
	}
	if !found {
		t.Fatalf("封禁记录应含模式与客户端名: %#v", c.banList.List())
	}

	// 未命中的客户端照常注册
	var qbPID [20]byte
	copy(qbPID[:], "-qB4670-normalpeer000")
	srv2, cli2 := net.Pipe()
	t.Cleanup(func() { srv2.Close(); cli2.Close() })
	if pc := c.register("7.7.7.7:40001", &Handshake{PeerID: qbPID}, cli2); pc == nil {
		t.Fatal("未命中黑名单的 peer 应正常注册")
	} else if c.banList.Banned("7.7.7.7") {
		t.Fatal("未命中黑名单的 IP 不应被封禁")
	}
}

// TestSetClientBlacklistHot 热更新:设置黑名单后现存连接里命中的即封禁 +
// 断开;清空名单后不再封。
func TestSetClientBlacklistHot(t *testing.T) {
	meta := buildHubMeta(t, "blhot.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var xlPID [20]byte
	copy(xlPID[:], "-XL0011-xunlei-leech000")
	mkLeechPeer(t, c, "6.6.6.6:40000", 0, 0, -1, 0, 0).id = xlPID
	mkLeechPeer(t, c, "7.7.7.7:40001", 0, 0, -1, 0, 0) // id 全零 → 未知,不命中

	c.SetClientBlacklist([]string{"*xunlei*"})
	if !c.banList.Banned("6.6.6.6") {
		t.Fatal("热切换后现存命中连接应被封禁")
	}
	if c.banList.Banned("7.7.7.7") {
		t.Fatal("未命中的 peer 不应被封禁")
	}

	// 清空名单:不再新增封禁(已封记录按名单语义保留)
	c.SetClientBlacklist(nil)
	if _, hit := c.clientBlacklisted("Xunlei 9.9.9"); hit {
		t.Fatal("清空名单后不应再命中")
	}
}

// TestBanClientBanClient 按客户端名封禁现存 peer:同名的跨 IP 全封、
// 异名不封、同 IP 多连接只记一次名单。
func TestBanClient(t *testing.T) {
	meta := buildHubMeta(t, "blname.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var xlPID [20]byte
	copy(xlPID[:], "-XL0011-xunlei-leech000")
	name := ClientName(xlPID)

	// 同名两 IP + 同 IP 第二条连接 + 异名一条
	mkLeechPeer(t, c, "6.6.6.6:40000", 0, 0, -1, 0, 0).id = xlPID
	mkLeechPeer(t, c, "6.6.6.6:40001", 0, 0, -1, 0, 0).id = xlPID
	mkLeechPeer(t, c, "7.7.7.7:40002", 0, 0, -1, 0, 0).id = xlPID
	mkLeechPeer(t, c, "8.8.8.8:40003", 0, 0, -1, 0, 0) // 未知客户端

	banned := c.BanClient(name)
	if len(banned) != 2 {
		t.Fatalf("应封禁 2 个 IP(同 IP 多连接去重),得到 %v", banned)
	}
	for _, ip := range []string{"6.6.6.6", "7.7.7.7"} {
		if !c.banList.Banned(ip) {
			t.Fatalf("%s 应被封禁", ip)
		}
	}
	if c.banList.Banned("8.8.8.8") {
		t.Fatal("异名客户端不应被封禁")
	}
	if e := c.banList.List()[0]; e.Reason != "客户端黑名单:"+name {
		t.Fatalf("封禁原因不符: %q", e.Reason)
	}
	// 空名直接忽略
	if got := c.BanClient(""); got != nil {
		t.Fatalf("空客户端名应返回 nil,得到 %v", got)
	}
}

// TestBanClientExtVersionMismatch 实测教训:真实客户端会发扩展握手 v 字段
// (如 "qBittorrent/4.6.7"),与 peer id 推断名("qBittorrent 4.6.7")不同形。
// 展示名(PeerStat.Client)与封禁匹配必须同口径,否则右键封禁 0 命中。
func TestBanClientExtVersionMismatch(t *testing.T) {
	meta := buildHubMeta(t, "blext.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var qbPID [20]byte
	copy(qbPID[:], "-qB4670-abcdegHIJKLmno")
	pcA := mkLeechPeer(t, c, "6.6.6.6:40000", 0, 0, -1, 0, 0)
	pcA.id = qbPID
	pcA.mu.Lock()
	pcA.ext = &ExtHandshake{Version: "qBittorrent/4.6.7"}
	pcA.mu.Unlock()
	mkLeechPeer(t, c, "7.7.7.7:40001", 0, 0, -1, 0, 0).id = qbPID // 未发扩展握手,只有推断名

	// 展示名与 clientLabel 同口径:发了 v 的显示 v 全名
	for _, ps := range c.PeerStats() {
		if ps.Addr == "6.6.6.6:40000" && ps.Client != "qBittorrent/4.6.7" {
			t.Fatalf("展示名应取扩展握手 v 全名,得到 %q", ps.Client)
		}
		if ps.Addr == "7.7.7.7:40001" && ps.Client != "qBittorrent 4.6.7" {
			t.Fatalf("未发 v 的应显示推断名,得到 %q", ps.Client)
		}
	}

	// 右键任一形态名,两个形态都要被封(双口径匹配)
	if got := c.BanClient("qBittorrent/4.6.7"); len(got) != 2 {
		t.Fatalf("按 v 名封禁应命中 2 个 IP,得到 %v", got)
	}
	if !c.banList.Banned("6.6.6.6") || !c.banList.Banned("7.7.7.7") {
		t.Fatal("两个形态的同款客户端都应被封禁")
	}
}
