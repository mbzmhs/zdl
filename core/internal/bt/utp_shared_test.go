package bt

import (
	"net"
	"testing"
	"time"
)

// TestUTPSharedWithDHT:DHT 占用 UDP 端口时,BT 客户端回退 BEP-29 §8.1
// 同端口复用,uTP 入站/出站建连与数据传输照常工作。
func TestUTPSharedWithDHT(t *testing.T) {
	// 挑一个空闲端口:同时作为 DHT 的 UDP 端口与做种端的 BT 监听端口
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	t0 := time.Now()

	srv, err := StartDHT(DHTConfig{Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if srv.utpm == nil {
		t.Fatal("v4 DHT 应挂载共享 uTP manager")
	}

	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("t0=%v 种子数据就绪", time.Since(t0))
	seed, err := NewSeedClient(meta, srcRoot, ClientConfig{MaxPeers: 8, ListenPort: port, DHT: srv})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()
	t.Logf("t0=%v seed.Start 返回", time.Since(t0))
	if seed.utpm != srv.utpm {
		t.Fatal("端口被 DHT 占用,做种端应回退共享 uTP manager")
	}

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	t.Logf("t0=%v leech.Start 返回", time.Since(t0))

	// 直接走 uTP 出站(绕过 TCP 优先策略,专门验证共享端口路径)。
	// dialOutUTP 会阻塞在该连接的读循环上(生产中由独立拨号协程调用),
	// 完成下载后连接仍保持做种,不会自行关闭——故此处放协程里拨号,
	// 再等下载完成,并以 UTPPeers 确认两端确实建立了 uTP 连接。
	go leech.dialOutUTP(seed.ListenAddr())
	waitDone(t, leech)
	t.Logf("t0=%v 下载完成", time.Since(t0))
	if seed.UTPPeers() == 0 {
		t.Fatal("做种端应统计到 uTP 入站连接")
	}
	if leech.UTPPeers() == 0 {
		t.Fatal("下载端应统计到 uTP 出站连接")
	}
}
