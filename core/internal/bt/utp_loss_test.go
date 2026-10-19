package bt

import (
	"context"
	crand "crypto/rand"
	"io"
	"math/rand"
	"sync/atomic"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lossyRelay 模拟真实 NAT/公网路径的 UDP 中继:随机丢包、乱序(随机延迟)、
// 重复包。单 socket 双向转发——先到的一方视为"内网侧"(NAT 内),另一方为
// 外网侧,与 NAT 的映射建立语义一致。
type lossyRelay struct {
	conn     *net.UDPConn
	seed     *net.UDPAddr
	client   *net.UDPAddr
	drop     float64       // 丢包概率
	dup      float64       // 重复包概率
	maxDelay time.Duration // 最大随机延迟(乱序源)
	toSeed   int64         // 调试:已转发到 seed 的包数
	toClient int64         // 调试:已转发到 client 的包数
}

func (r *lossyRelay) run() {
	buf := make([]byte, 65536)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var dst *net.UDPAddr
		if r.seed != nil && src.IP.Equal(r.seed.IP) && src.Port == r.seed.Port {
			dst = r.client // 外网侧回包 → 内网客户端
			atomic.AddInt64(&r.toClient, 1)
		} else {
			r.client = src // 内网客户端先到:记录映射
			dst = r.seed
			atomic.AddInt64(&r.toSeed, 1)
		}
		if dst == nil {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		r.forward(pkt, dst)
	}
}

func (r *lossyRelay) forward(pkt []byte, dst *net.UDPAddr) {
	send := func() {
		r.conn.WriteToUDP(pkt, dst)
	}
	if rand.Float64() < r.drop {
		return
	}
	if rand.Float64() < r.dup { // 重复包(先发一份副本,制造乱序重复)
		go time.AfterFunc(time.Duration(rand.Int63n(int64(r.maxDelay))), send)
	}
	go time.AfterFunc(time.Duration(rand.Int63n(int64(r.maxDelay))), send)
}

// TestUTPDownloadThroughLossyPath:uTP 全量下载经过丢包+乱序+重复的
// NAT 式路径。回环测试零损耗掩盖不了真实路径的鲁棒性问题(现场:同
// swarm 里 BitComet 全走 TCP 能用,我们到 qB 的 uTP 连接一律 0 字节)。
func TestUTPDownloadThroughLossyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("长测")
	}
	// 512KB 单文件、16KB piece → 32 piece / 256 块,足够触发多次丢包重传
	root := t.TempDir()
	bin := make([]byte, 512<<10)
	if _, err := crand.Read(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := NewSeedClient(meta, root, ClientConfig{MaxPeers: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()
	seedHost, seedPortStr, err := net.SplitHostPort(seed.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}
	seedPort, err := net.LookupPort("tcp", seedPortStr)
	if err != nil {
		t.Fatal(err)
	}
	seedIP := net.ParseIP(seedHost)
	if seedIP == nil || seedIP.IsUnspecified() {
		seedIP = net.IPv4(127, 0, 0, 1) // ListenAddr 可能是通配形式,回环拨同 dialOutUTP
	}
	seedAddr := &net.UDPAddr{IP: seedIP, Port: seedPort}

	// 中继:监听一个 UDP 端口,转发到 seed,丢包 1% / 重复 2% / 延迟 0-120ms
	rlyLn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	rly := &lossyRelay{conn: rlyLn, seed: seedAddr, drop: 0.01, dup: 0.02, maxDelay: 120 * time.Millisecond}
	go rly.run()
	defer rlyLn.Close()

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	// 损耗路径下建连可能因 SYN/SYN-ACK 被丢而失败,生产中等价于 reconnectLoop
	// 的重拨:这里循环重试,直到连接注册成功或超时。
	deadline := time.Now().Add(60 * time.Second)
	for len(leech.peers) == 0 && time.Now().Before(deadline) {
		done := make(chan bool, 1)
		go func() { done <- leech.dialOutUTP(rlyLn.LocalAddr().String()) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			// dialOutUTP 成功后会阻塞在连接读循环不返回:超时视为已接上
		}
		time.Sleep(500 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := leech.WaitComplete(ctx); err != nil {
		t.Fatalf("经损耗路径的 uTP 下载未完成: %v (done=%d/%d, 连接=%d, 中继→seed=%d →client=%d)",
			err, leech.PiecesDone(), meta.PieceCount(), len(leech.peers),
			atomic.LoadInt64(&rly.toSeed), atomic.LoadInt64(&rly.toClient))
	}
	if got := leech.Stats().Downloaded; got < int64(len(bin)) {
		t.Fatalf("下载字节数异常: %d < %d", got, len(bin))
	}
}

// natRelay 带映射过期的 NAT:客户端侧连续 natTimeout 没有出站包即视为
// 映射失效,期间外网侧→客户端的包一律丢弃;客户端任何出站包重建映射。
type natRelay struct {
	conn       *net.UDPConn
	peer       *net.UDPAddr
	client     *net.UDPAddr
	lastOut    time.Time
	natTimeout time.Duration
	dropped    int64
}

func (r *natRelay) run() {
	buf := make([]byte, 65536)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if r.peer != nil && src.IP.Equal(r.peer.IP) && src.Port == r.peer.Port {
			// 外网→内网:映射存活才放行
			if time.Since(r.lastOut) > r.natTimeout {
				atomic.AddInt64(&r.dropped, 1)
				continue
			}
			r.conn.WriteToUDP(pkt, r.client)
			continue
		}
		r.client = src
		r.lastOut = time.Now() // 内网出站包刷新/重建映射
		r.conn.WriteToUDP(pkt, r.peer)
	}
}

// TestUTPKeepaliveSurvivesNATIdle:被 choke 等待的静默期(无任何数据交互)
// 超过 NAT UDP 映射超时后,对端写入的数据必须仍能送达——uTP 空闲
// keepalive 持续刷新映射(BEP-29 §1.2)。旧实现整段静默期零发包,
// 映射一过期双向聋哑,表现即"连接在、数据 0、传着传着停"。
func TestUTPKeepaliveSurvivesNATIdle(t *testing.T) {
	srv, err := newUTPManagerOwn(0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cli, err := newUTPManagerOwn(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	peerAddr := srv.pc.LocalAddr().(*net.UDPAddr)
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: peerAddr.Port}
	rlyLn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rlyLn.Close()
	rly := &natRelay{conn: rlyLn, peer: peer, natTimeout: 3 * time.Second}
	go rly.run()

	accCh := make(chan *utpConn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := srv.Accept(ctx)
		if err != nil {
			return
		}
		accCh <- c
	}()

	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := cli.Dial(dctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rlyLn.LocalAddr().(*net.UDPAddr).Port})
	cancel()
	if err != nil {
		t.Fatalf("经 NAT 建连失败: %v", err)
	}
	defer conn.Close()
	srvConn := <-accCh
	defer srvConn.Close()

	// 映射存活期的基础传输(校验测试通路本身)
	if _, err := srvConn.Write([]byte("ping1")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf[:5]); err != nil {
		t.Fatalf("存活期读回环数据失败: %v", err)
	}

	// 静默 6 秒(> NAT 3s 超时,映射已死),对端再写数据:
	// 只有 keepalive 刷新映射后,重传才能送达。
	time.Sleep(6 * time.Second)
	go func() {
		srvConn.SetWriteDeadline(time.Now().Add(80 * time.Second))
		srvConn.Write([]byte("ping2"))
	}()
	conn.SetReadDeadline(time.Now().Add(80 * time.Second))
	if _, err := io.ReadFull(conn, buf[:5]); err != nil {
		t.Fatalf("NAT 静默期后的数据未送达(keepalive 失效?): %v, NAT 丢弃=%d",
			err, atomic.LoadInt64(&rly.dropped))
	}
}
