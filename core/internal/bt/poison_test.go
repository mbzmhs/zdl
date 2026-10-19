package bt

import (
	"testing"

	"zdownloader/internal/limiter"
)

// TestBlameStrikesAutoBan 坏块归因追责:累计 poisonStrikes 次封禁、白名单
// peer(供过足够正常块)豁免、开关热切换只观察不处罚。
func TestBlameStrikesAutoBan(t *testing.T) {
	mk := func(autoBan bool) *Client {
		meta := buildHubMeta(t, "poison.bin")
		c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: autoBan, GlobalDown: limiter.New(0)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}

	c := mk(true)
	// 前两次打击不封禁(阈值 3)
	banned, suspects := c.blameStrikes(map[string]int{"1.1.1.1:1": 2, "2.2.2.2:2": 3})
	if len(banned) != 0 || len(suspects) != 2 {
		t.Fatalf("首犯不应封禁: banned=%v suspects=%v", banned, suspects)
	}
	// 整块唯一来源也不再"铁证立即判罚":同样按次数累计
	banned, suspects = c.blameStrikes(map[string]int{"3.3.3.3:3": 512})
	if len(banned) != 0 || len(suspects) != 1 || suspects[0] != "3.3.3.3" {
		t.Fatalf("唯一来源首次不应直接封禁: banned=%v suspects=%v", banned, suspects)
	}
	// 第三次打击:1.1.1.1 与 2.2.2.2 都到 2 次,仍未达阈值
	banned, suspects = c.blameStrikes(map[string]int{"1.1.1.1:1": 1, "2.2.2.2:2": 1})
	if len(banned) != 0 || len(suspects) != 2 {
		t.Fatalf("2 次不应封禁: banned=%v suspects=%v", banned, suspects)
	}
	// 第四次:1.1.1.1 累计 3 次 → 封禁;2.2.2.2 停在 2 次继续观察
	banned, suspects = c.blameStrikes(map[string]int{"1.1.1.1:1": 1})
	if len(banned) != 1 || banned[0] != "1.1.1.1:1" {
		t.Fatalf("累计 3 次应封禁: banned=%v", banned)
	}
	if len(suspects) != 0 {
		t.Fatalf("封禁项不应同时出现在观察列表: %v", suspects)
	}

	// 白名单:piece 有效率(参与并通过校验的比例)达阈值(默认 80%)才豁免。
	// 4.4.4.4 有效率高(9 通过 / 1 失败 = 90%):多次坏块也只观察不封
	for i := 0; i < 9; i++ {
		c.countPieceResult(map[string]int{"4.4.4.4:4": 8}, true)
	}
	c.countPieceResult(map[string]int{"4.4.4.4:4": 8}, false)
	for i := 0; i < poisonStrikesDefault+2; i++ {
		banned, _ = c.blameStrikes(map[string]int{"4.4.4.4:4": 99})
		if len(banned) != 0 {
			t.Fatalf("高有效率 peer 不应被自动封禁: %v", banned)
		}
	}
	if c.peerStrikes["4.4.4.4"] != 0 {
		t.Fatalf("白名单 peer 不应累计打击: %d", c.peerStrikes["4.4.4.4"])
	}

	// blame 登记与清空
	c.recordBlame(7, "5.5.5.5:5")
	c.recordBlame(7, "5.5.5.5:5")
	if m := c.takeBlame(7); m["5.5.5.5:5"] != 2 {
		t.Fatalf("takeBlame 应返回来源计数: %v", m)
	}
	if m := c.takeBlame(7); len(m) != 0 {
		t.Fatalf("takeBlame 应清空: %v", m)
	}

	// 开关关闭(构造即关):不记打击、不封禁
	off := mk(false)
	for i := 0; i < 5; i++ {
		banned, _ = off.blameStrikes(map[string]int{"6.6.6.6:6": 9})
		if len(banned) != 0 || off.peerStrikes["6.6.6.6"] != 0 {
			t.Fatalf("开关关闭不应记打击/封禁: banned=%v strikes=%d", banned, off.peerStrikes["6.6.6.6"])
		}
	}
	// 开关热切换:运行中打开后立即生效(设置保存对运行会话生效的口径)
	off.SetAutoBanPoison(true)
	off.blameStrikes(map[string]int{"7.7.7.7:7": 1})
	off.blameStrikes(map[string]int{"7.7.7.7:7": 1})
	banned, _ = off.blameStrikes(map[string]int{"7.7.7.7:7": 1})
	if len(banned) != 1 {
		t.Fatalf("热打开后应正常计次封禁: banned=%v", banned)
	}
}

// TestPoisonWhitelistRateGate 白名单按 piece 有效率动态维持(bt.130):
// 校验通过才算好块 → 参与片数进账;达阈值豁免自动封禁,跌破阈值当场踢出、
// 按一般 peer 累计打击;无样本不豁免;阈值热切换立即生效。
func TestPoisonWhitelistRateGate(t *testing.T) {
	meta := buildHubMeta(t, "rategate.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true, GlobalDown: limiter.New(0)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	// 模拟校验结果落定时的记账顺序:先记这次结果(有效率随之变化),再追责
	fail := func(addr string) (banned []string) {
		c.countPieceResult(map[string]int{addr: 8}, false)
		banned, _ = c.blameStrikes(map[string]int{addr: 8})
		return banned
	}
	ok := func(addr string) { c.countPieceResult(map[string]int{addr: 8}, true) }

	// 无样本:交付路径登记过来源(recordBlame)但没有一片通过校验 → 不在白名单
	c.recordBlame(1, "1.1.1.1:1")
	if banned := fail("1.1.1.1:1"); len(banned) != 0 {
		t.Fatalf("无样本不应立即封禁: %v", banned)
	}
	if c.peerStrikes["1.1.1.1"] != 1 {
		t.Fatalf("无样本 peer 应累计打击, got %d", c.peerStrikes["1.1.1.1"])
	}

	// 8 片全通过(100%)→ 豁免;随后掺坏块逐步把有效率拖下去
	for i := 0; i < 8; i++ {
		ok("2.2.2.2:2")
	}
	// 8/1=88.9%、8/2=80%(恰好达阈值)都还留在白名单,不计打击
	for i := 0; i < 2; i++ {
		if banned := fail("2.2.2.2:2"); len(banned) != 0 {
			t.Fatalf("达阈值期间不应封禁: %v", banned)
		}
	}
	if c.peerStrikes["2.2.2.2"] != 0 {
		t.Fatalf("达阈值期间不应累计打击, got %d", c.peerStrikes["2.2.2.2"])
	}
	// 8/3=72.7% < 80%:当场踢出白名单,开始计打击;第 3 次打击(累计)封禁
	for i := 0; i < 3; i++ {
		banned := fail("2.2.2.2:2")
		if i < 2 && len(banned) != 0 {
			t.Fatalf("第 %d 次跌破后不应立即封禁: %v", i+1, banned)
		}
		if i == 2 && len(banned) != 1 {
			t.Fatalf("跌破阈值后累计 3 次打击应封禁: %v", banned)
		}
	}
	// 实际封禁由调用方落名单(onPieceVerified → BanIPReason),这里补上一环验证链路
	c.BanIPReason("2.2.2.2", "毒源:累计供坏块 3 次")
	if !c.banList.Banned("2.2.2.2") {
		t.Fatal("跌破白名单阈值后应被封禁")
	}

	// 阈值热切换:99%(99 通过 / 1 失败)在默认 80 下豁免
	for i := 0; i < 99; i++ {
		ok("3.3.3.3:3")
	}
	if banned := fail("3.3.3.3:3"); len(banned) != 0 {
		t.Fatalf("99%% 有效率不应封禁: %v", banned)
	}
	if c.peerStrikes["3.3.3.3"] != 0 {
		t.Fatalf("80%% 阈值下应豁免, got strikes=%d", c.peerStrikes["3.3.3.3"])
	}
	// 改成 100%:再掺一片就跌破(99/1=99% < 100),当场失去豁免
	c.SetPoisonRate(100)
	if banned := fail("3.3.3.3:3"); len(banned) != 0 {
		t.Fatalf("热切阈值后首次不应立即封禁: %v", banned)
	}
	if c.peerStrikes["3.3.3.3"] != 1 {
		t.Fatalf("阈值 100%% 下掺坏块应记打击, got %d", c.peerStrikes["3.3.3.3"])
	}
	// 阈值归一:0 → 默认 80(恢复豁免),<50 钳到 50,>100 钳到 100
	c.SetPoisonRate(0)
	if !c.whitelistedLocked("3.3.3.3") {
		t.Fatal("阈值回落到 80 后应恢复白名单(99%≥80%)")
	}
	for _, tc := range []struct{ in, want int }{{10, 50}, {50, 50}, {85, 85}, {500, 100}} {
		if got := normPoisonRate(tc.in); got != tc.want {
			t.Fatalf("normPoisonRate(%d)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestPoisonStrikesConfigurable 坏块宽容次数可配置(bt.131):默认 3 次;
// 次数以 norm 钳在 1..10;运行中热切换立即按新次数判定封禁。
func TestPoisonStrikesConfigurable(t *testing.T) {
	meta := buildHubMeta(t, "strikes.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true, GlobalDown: limiter.New(0)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	// 不传 PoisonStrikes(0)→ 默认 3
	if n := c.poisonStrikes.Load(); n != poisonStrikesDefault {
		t.Fatalf("默认宽容次数应为 %d, got %d", poisonStrikesDefault, n)
	}
	// 归一:0 → 3;越界钳 1 / 10
	for _, tc := range []struct{ in, want int }{{0, 3}, {-5, 3}, {1, 1}, {5, 5}, {10, 10}, {99, 10}} {
		if got := normPoisonStrikes(tc.in); got != tc.want {
			t.Fatalf("normPoisonStrikes(%d)=%d, want %d", tc.in, got, tc.want)
		}
	}

	// 热切换到 1:首次坏块即封(不再宽容)
	c.SetPoisonStrikes(1)
	banned, suspects := c.blameStrikes(map[string]int{"1.1.1.1:1": 1})
	if len(banned) != 1 || banned[0] != "1.1.1.1:1" {
		t.Fatalf("宽容次数 1 时首次坏块应封禁: banned=%v suspects=%v", banned, suspects)
	}
	// 热切换到 5:宽容度提高,同一 IP 需累计 5 次才封
	c.SetPoisonStrikes(5)
	for i := 0; i < 4; i++ {
		banned, _ = c.blameStrikes(map[string]int{"2.2.2.2:2": 1})
		if len(banned) != 0 {
			t.Fatalf("第 %d 次(宽容 5)不应封禁: %v", i+1, banned)
		}
	}
	banned, _ = c.blameStrikes(map[string]int{"2.2.2.2:2": 1})
	if len(banned) != 1 {
		t.Fatalf("累计 5 次应封禁: %v", banned)
	}
	// 累计次数跨热切换保留:3.3.3.3 在宽容 5 下已记 3 次未封,调到 3 后
	// 下一次打击即达阈值(不是从零重新数)
	for i := 0; i < 3; i++ {
		c.blameStrikes(map[string]int{"3.3.3.3:3": 1})
	}
	if c.peerStrikes["3.3.3.3"] != 3 {
		t.Fatalf("3.3.3.3 应记 3 次, got %d", c.peerStrikes["3.3.3.3"])
	}
	c.SetPoisonStrikes(3)
	if banned, _ = c.blameStrikes(map[string]int{"3.3.3.3:3": 1}); len(banned) != 1 {
		t.Fatalf("累计次数应跨热切换保留: %v", banned)
	}
	// 构造时注入(非 0):直接生效
	c2, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true, GlobalDown: limiter.New(0), PoisonStrikes: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.Close)
	if n := c2.poisonStrikes.Load(); n != 7 {
		t.Fatalf("构造注入宽容次数应生效, got %d", n)
	}
}
