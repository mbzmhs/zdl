package bt

import (
	"net"
	"testing"
	"time"
)

// mkLeechPeer 建一条"现存连接"桩并按参数摆好计数值,注册进 c.peers。
// servedAgo < 0 表示从未供过数(serveAt 保持 0)。
func mkLeechPeer(t *testing.T, c *Client, addr string, up, down int64, servedAgo time.Duration, haveAtServe, haveNow int32) *peerConn {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	pc := &peerConn{cl: c, addr: addr, conn: cli, send: make(chan outbound, 1), done: make(chan struct{})}
	pc.upTotal.Store(up)
	pc.downTotal.Store(down)
	if servedAgo >= 0 {
		pc.serveAt.Store(time.Now().Add(-servedAgo).UnixNano())
	}
	pc.haveAtServe.Store(haveAtServe)
	pc.haveCount.Store(haveNow)
	c.pMu.Lock()
	c.peers[addr] = pc
	c.pMu.Unlock()
	return pc
}

// TestLeechSweepDownloading 下载期两条独立判据:
//   - 字节判据(不看可伪造的自报进度):供出 ≥8MiB 满 10 分钟、回传不足
//     索取量 1/16 且不到 512KiB 即封;
//   - 进度判据(复活旧判据,不再要求回报恰为零):piece 声明数自首次
//     供数起纹丝不动即封——专防"做字节互惠骗过字节判据"的低级 PCDN。
//
// 有进度且回传达标/未达阈值/开关关闭均不封。
func TestLeechSweepDownloading(t *testing.T) {
	meta := buildHubMeta(t, "leech.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	now := time.Now()
	// 伪装成 qBittorrent 4.6.7 的 PCDN:索取 9MB、零回传,还假增进度
	// (have 0→3)——字节判据不看进度,照封
	mkLeechPeer(t, c, "6.6.6.6:40000", 9<<20, 0, 11*time.Minute, 0, 3)
	// 假增进度 + 微量回传(1KB):仍在"回传几乎为零"内,同样该封
	mkLeechPeer(t, c, "6.6.6.7:40007", 9<<20, 1<<10, 11*time.Minute, 0, 3)
	// 回传 1MB(超 512KiB 上限)骗过字节判据,但进度纹丝不动:
	// 低级 PCDN,进度判据该封
	mkLeechPeer(t, c, "8.8.8.8:40002", 9<<20, 1<<20, 11*time.Minute, 0, 0)
	// 回传 1MB 且进度在推进:真互惠 peer,不封
	mkLeechPeer(t, c, "8.8.8.9:40009", 9<<20, 1<<20, 11*time.Minute, 0, 2)
	// 索取量未达阈值:不封
	mkLeechPeer(t, c, "9.9.9.9:40003", 4<<20, 0, 11*time.Minute, 0, 0)
	// 供数时间未达阈值:不封
	mkLeechPeer(t, c, "10.0.0.1:40004", 9<<20, 0, 3*time.Minute, 0, 0)
	// 从未被供过数:不封
	mkLeechPeer(t, c, "10.0.0.2:40005", 9<<20, 0, -1, 0, 0)

	c.leechSweep(now)
	for _, ip := range []string{"6.6.6.6", "6.6.6.7", "8.8.8.8"} {
		if !c.banList.Banned(ip) {
			t.Fatalf("%s 吸血 peer 应被封禁", ip)
		}
	}
	for _, ip := range []string{"8.8.8.9", "9.9.9.9", "10.0.0.1", "10.0.0.2"} {
		if c.banList.Banned(ip) {
			t.Fatalf("%s 不应被封禁", ip)
		}
	}
	// 封禁原因按 IP 断言(sweep 遍历 c.peers 的 map 序随机,取 List()[0] 不稳)
	reasonOf := make(map[string]string)
	for _, e := range c.banList.List() {
		reasonOf[e.IP] = e.Reason
	}
	if r := reasonOf["6.6.6.6"]; r != "只取不予:索取大回报微(疑似PCDN)" {
		t.Fatalf("6.6.6.6 封禁原因不符: %q", r)
	}
	if r := reasonOf["8.8.8.8"]; r != "只取不予:索取大进度未变(疑似PCDN)" {
		t.Fatalf("8.8.8.8 封禁原因不符: %q", r)
	}

	// 开关关闭:全条件命中也不封
	mkLeechPeer(t, c, "11.0.0.1:40006", 9<<20, 0, 11*time.Minute, 0, 3)
	c.SetAutoBanPoison(false)
	c.leechSweep(now)
	if c.banList.Banned("11.0.0.1") {
		t.Fatal("AutoBanPoison 关闭时不应触发只取不予封禁")
	}
}

// TestLeechSweepSeeding 做种期判据:对端"只下载不回报"可能是合法纯下载
// 者,字节上与假进度吸血无法区分,故沿用保守老判据——回报恰为零且进度
// 声明纹丝不动;有回传或进度推进都不封。
func TestLeechSweepSeeding(t *testing.T) {
	meta := buildHubMeta(t, "seed.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.completed.Store(true) // 本任务已完成 = 做种期

	now := time.Now()
	// 回报零 + 进度静止:命中老判据
	mkLeechPeer(t, c, "6.6.6.6:40000", 9<<20, 0, 11*time.Minute, 0, 0)
	// 进度在推进(have 增量):做种期不判
	mkLeechPeer(t, c, "7.7.7.7:40001", 9<<20, 0, 11*time.Minute, 0, 3)
	// 有回报(哪怕 1 字节):不判
	mkLeechPeer(t, c, "8.8.8.8:40002", 9<<20, 1, 11*time.Minute, 0, 0)
	// 索取量未达阈值
	mkLeechPeer(t, c, "9.9.9.9:40003", 4<<20, 0, 11*time.Minute, 0, 0)
	// 供数时间未达阈值
	mkLeechPeer(t, c, "10.0.0.1:40004", 9<<20, 0, 3*time.Minute, 0, 0)

	c.leechSweep(now)
	if !c.banList.Banned("6.6.6.6") {
		t.Fatal("做种期零回报且进度静止的 peer 应被封禁")
	}
	for _, ip := range []string{"7.7.7.7", "8.8.8.8", "9.9.9.9", "10.0.0.1"} {
		if c.banList.Banned(ip) {
			t.Fatalf("%s 不应被封禁", ip)
		}
	}
	if e := c.banList.List()[0]; e.Reason != "只取不予:索取大回报零且进度不变(疑似PCDN)" {
		t.Fatalf("封禁原因不符: %q", e.Reason)
	}
}

// TestHaveWithoutBitfieldExempt 从未发过 bitfield(协议允许)但持续发 have
// 的 peer:haveCount 必须真实增长,leechSweep 不得误判"进度不变"。
// (做种期判据要求回报恰为零且进度不动,置为已完成状态验证。)
func TestHaveWithoutBitfieldExempt(t *testing.T) {
	meta := buildHubMeta(t, "havebf.bin")
	c, err := NewClient(meta, t.TempDir(), ClientConfig{AutoBanPoison: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.completed.Store(true)

	srv, cli := net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	pc := &peerConn{cl: c, addr: "12.0.0.1:40010", conn: cli, send: make(chan outbound, 1), done: make(chan struct{})}
	c.pMu.Lock()
	c.peers[pc.addr] = pc
	c.pMu.Unlock()

	// 先供数定格基线(此时对端 piece 数 0),再陆续收到 have 报告进度
	pc.serveAt.Store(time.Now().Add(-11 * time.Minute).UnixNano())
	pc.haveAtServe.Store(0)
	pc.upTotal.Store(9 << 20)

	have := func(idx uint32) {
		p := make([]byte, 4)
		p[0], p[1], p[2], p[3] = byte(idx>>24), byte(idx>>16), byte(idx>>8), byte(idx)
		pc.onHave(p)
	}
	have(0)
	have(1)
	if pc.haveCount.Load() != 2 {
		t.Fatalf("无 bitfield 时 have 也应计入进度,haveCount=%d", pc.haveCount.Load())
	}
	c.leechSweep(time.Now())
	if c.banList.Banned("12.0.0.1") {
		t.Fatal("有 have 增量的 peer 不应被误封")
	}
	// 同一 piece 的重复 have 不应重复计数
	have(0)
	if pc.haveCount.Load() != 2 {
		t.Fatalf("重复 have 不应重复计数,haveCount=%d", pc.haveCount.Load())
	}
}
