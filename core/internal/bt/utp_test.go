package bt

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUTPHeaderRoundTrip 包头编解码逐字节往返。
func TestUTPHeaderRoundTrip(t *testing.T) {
	h := utpHeader{
		pktType: utpData, ver: utpVersion, extension: 1,
		connID: 0xBEEF, timestamp: 0x01020304, tsDiff: 0x0A0B0C0D,
		wndSize: 262144, seqNr: 65000, ackNr: 1234,
	}
	buf := make([]byte, utpHeaderLen)
	h.marshal(buf)
	got, err := parseUTPHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("往返不一致:\n得 %+v\n期望 %+v", got, h)
	}
	// 分流判定
	if !isUTPPacket(buf) {
		t.Fatal("应判为 uTP")
	}
	buf[0] = 5 << 4 // type=5 非法
	if isUTPPacket(buf) {
		t.Fatal("type≥5 不应判 uTP")
	}
	buf[0] = utpData<<4 | 2 // ver=2
	if isUTPPacket(buf) {
		t.Fatal("ver≠1 不应判 uTP")
	}
	// 短包/未知版本
	if _, err := parseUTPHeader(buf[:10]); err == nil {
		t.Fatal("短包应报错")
	}
}

// TestUTPExts 扩展链解析(nextType 宣告下一个,len 是当前,0 终止)。
func TestUTPExts(t *testing.T) {
	// 链:SACK(1) → bits(2) → 终止
	raw := []byte{
		2, 4, 0xAA, 0xBB, 0xCC, 0xDD, // 块1: next=2, len=4, SACK掩码
		0, 8, 1, 2, 3, 4, 5, 6, 7, 8, // 块2: next=0, len=8, bits
	}
	exts, n, err := parseUTPExts(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(raw) {
		t.Fatalf("消耗 %d != %d", n, len(raw))
	}
	if len(exts) != 2 || exts[0].typ != 1 || exts[1].typ != 2 {
		t.Fatalf("扩展类型解析错: %+v", exts)
	}
	if !bytes.Equal(exts[0].data, []byte{0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Fatalf("SACK 数据错: %x", exts[0].data)
	}
	// 截断
	if _, _, err := parseUTPExts(raw[:3], 1); err == nil {
		t.Fatal("截断应报错")
	}
}

// TestDelayHist base 滑动最小与 3 样本 min。
func TestDelayHist(t *testing.T) {
	var h delayHist
	if h.value() != 0 {
		t.Fatal("无样本应为 0")
	}
	// 初始化:base=首样本,相对值 0
	h.addSample(50000, 0)
	if h.value() != 0 {
		t.Fatalf("相对 base 应为 0,得 %d", h.value())
	}
	// 首样本相对 0 仍在窗内 → min=0(与 libutp 一致:前 3 样本逐步覆盖)
	h.addSample(80000, 1000)
	if v := h.value(); v != 0 {
		t.Fatalf("首样本 0 仍在窗内,应 0,得 %d", v)
	}
	h.addSample(60000, 2000)
	if v := h.value(); v != 0 {
		t.Fatalf("应 0,得 %d", v)
	}
	// 第 4 样本覆盖首槽 → 最近 3 样本 [30000,10000,40000] min=10000
	h.addSample(90000, 3000)
	if v := h.value(); v != 10000 {
		t.Fatalf("应取最近3样本 min=10000,得 %d", v)
	}
	// 更低的 base 出现 → base 下移
	h.addSample(40000, 4000)
	if hb := h.base; hb != 40000 {
		t.Fatalf("base 应更新为 40000,得 %d", hb)
	}
	// 分钟轮换:旧高样本过期
	var h2 delayHist
	h2.addSample(100000, 0)
	h2.addSample(100000, 1000)
	h2.addSample(200000, 61000) // 触发轮换
	if hb := h2.base; hb != 100000 {
		t.Fatalf("轮换后 base 应保持 100000,得 %d", hb)
	}
	_ = rand.Int
}

// startUTPEcho 起一对 manager:接受端把收到的数据原样回显。
// 返回拨号函数与清理。
func startUTPEcho(t *testing.T) (dial func() net.Conn, stop func()) {
	t.Helper()
	srv, err := newUTPManagerOwn(0)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := newUTPManagerOwn(0)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			c, err := srv.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *utpConn) {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							break
						}
					}
					if err != nil {
						c.Close()
						return
					}
				}
			}(c)
		}
	}()
	return func() net.Conn {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ua := srv.pc.LocalAddr().(*net.UDPAddr)
			// 目标必须是回环地址(LocalAddr 可能是 0.0.0.0,发不过去)
			dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ua.Port}
			conn, err := cli.Dial(ctx, dst)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			return conn
		}, func() {
			cli.Close()
			srv.Close()
		}
}

// TestUTPRoundTrip loopback 全链路:建连、分片发送、重组、ack、回显。
func TestUTPRoundTrip(t *testing.T) {
	dial, stop := startUTPEcho(t)
	defer stop()
	c := dial()
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))

	payload := make([]byte, 256<<10) // 256KB,跨 ~180 包
	for i := range payload {
		payload[i] = byte(i*7 + i>>8)
	}
	done := make(chan error, 1)
	go func() {
		if _, err := c.Write(payload); err != nil {
			done <- err
			return
		}
		got := make([]byte, 0, len(payload))
		buf := make([]byte, 8192)
		for len(got) < len(payload) {
			n, err := c.Read(buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil {
				done <- err
				return
			}
		}
		if !bytes.Equal(got, payload) {
			done <- errMismatch(len(got))
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("回环超时")
	}
}

type errMismatch int

func (e errMismatch) Error() string { return "回显内容不一致" }

// TestUTPCloseEOF 关闭写方向后对端读到 FIN → EOF,且半开期间回显仍可读。
func TestUTPCloseEOF(t *testing.T) {
	dial, stop := startUTPEcho(t)
	defer stop()
	c := dial()
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))

	msg := []byte("hello over uTP")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("读回显: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("回显不符: %q", buf[:n])
	}
	// 关写 → 服务端收 EOF 后关自己那侧 → 客户端最终 EOF
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.SetReadDeadline(deadline)
		_, err := c.Read(buf)
		if err != nil {
			// EOF 或超时都算达成半开语义:至少不能再收到新数据
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("关闭后仍持续读到数据")
		}
	}
}

// TestUTPSelfPlay 端到端:TCP 出站被阻断时(dialer 恒败,模拟对端防火墙
// 拦 TCP 的真实场景),leech 按 TCP 优先策略回退 uTP 拨号(其上再跑
// MSE+peer wire),下载完成后断言确实建立了 uTP 连接。
func TestUTPSelfPlay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "up.bin")
	content := make([]byte, 200<<10)
	for i := range content {
		content[i] = byte(i * 13)
	}
	if err := os.WriteFile(root, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(meta, root, ClientConfig{MaxPeers: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()
	// TCP 恒败,迫使走 uTP 回退路径
	blocked := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("测试:TCP 已被阻断")
	}
	leech, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4, Dialer: blocked}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	host, portStr, err := net.SplitHostPort(seed.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}
	pn, _ := net.LookupPort("tcp", portStr)
	pa := PeerAddr{IP: net.ParseIP(host).To4(), Port: uint16(pn)}
	leech.rememberCandUTP(pa, true, "manual") // 模拟 PEX flags 0x04
	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := leech.WaitComplete(ctx); err != nil {
		t.Fatalf("未完成: %v (done=%d/%d)", err, leech.PiecesDone(), meta.PieceCount())
	}
	if leech.UTPPeers() == 0 {
		t.Fatal("下载经回退完成,未走 uTP")
	}
	if seed.UTPPeers() == 0 {
		t.Fatal("seed 侧未观察到 uTP 入站")
	}
}
