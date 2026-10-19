package bt

import (
	"bytes"
	cryptorc4 "crypto/rc4"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// TestRC4MatchesStdlib 用标准库 crypto/rc4(仅测试用)交叉验证自实现:
// 同密钥同明文,密钥流必须逐字节一致。stdlib 已弃用但仍在树内,作
// 参考实现最可靠——不依赖任何凭记忆的测试向量。
func TestRC4MatchesStdlib(t *testing.T) {
	keys := [][]byte{
		{0x01, 0x02, 0x03, 0x04, 0x05},
		bytes.Repeat([]byte{0xAB}, 16),
		{0x42},                         // 单字节密钥
		bytes.Repeat([]byte{0x7F}, 60), // MSE 典型密钥长度区间
	}
	plain := bytes.Repeat([]byte("BitTorrent protocol"), 40) // ~760B,跨多轮 PRGA
	for ki, key := range keys {
		ref, err := cryptorc4.NewCipher(key)
		if err != nil {
			t.Fatalf("key %d: stdlib rc4: %v", ki, err)
		}
		ours := newRC4(key)
		got := make([]byte, len(plain))
		want := make([]byte, len(plain))
		ref.XORKeyStream(want, plain)
		ours.xorKeyStream(got, plain)
		if !bytes.Equal(got, want) {
			t.Fatalf("key %d: 密钥流与 stdlib 不一致", ki)
		}
		// 流性质:继续加密后续数据仍逐字节一致(状态连续推进)。
		tail := []byte("follow-on wire messages")
		ours.xorKeyStream(got[:len(tail)], tail)
		ref.XORKeyStream(want[:len(tail)], tail)
		if !bytes.Equal(got[:len(tail)], want[:len(tail)]) {
			t.Fatalf("key %d: 续流不一致", ki)
		}
	}
}

// TestRC4RoundTrip 加解密同流互逆(收发两实例,方向密钥不同是常态,
// 这里验证同一密钥下流互逆的基本代数性质)。
func TestRC4RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x5A}, 24)
	msg := []byte("length-prefixed peer wire frames走在这里")
	enc := newRC4(key)
	ct := make([]byte, len(msg))
	enc.xorKeyStream(ct, msg)
	if bytes.Equal(ct, msg) {
		t.Fatal("密文与明文相同")
	}
	dec := newRC4(key)
	pt := make([]byte, len(ct))
	dec.xorKeyStream(pt, ct)
	if !bytes.Equal(pt, msg) {
		t.Fatalf("往返不一致: %q", pt)
	}
}

// TestMSEConn:net.Pipe 两端各包一层(方向密钥互换),写明文读到明文;
// 且 Write 不得改动调用方缓冲(io.Writer 契约)。
func TestMSEConn(t *testing.T) {
	a, b := net.Pipe()
	keyAB := []byte("A-direction stream key 011")
	keyBA := []byte("B-direction stream key 022")
	ma := &mseConn{Conn: a, enc: newMSEStream(keyAB), dec: newMSEStream(keyBA)}
	mb := &mseConn{Conn: b, enc: newMSEStream(keyBA), dec: newMSEStream(keyAB)}

	payload := []byte("000002b7 加密层之上的明文 peer wire 帧")
	sent := make([]byte, len(payload))
	copy(sent, payload)

	gotCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 256)
		var out []byte
		for len(out) < len(payload) {
			n, err := mb.Read(buf)
			if err != nil {
				gotCh <- nil
				return
			}
			out = append(out, buf[:n]...)
		}
		gotCh <- out
	}()
	if _, err := ma.Write(payload); err != nil {
		t.Fatalf("写: %v", err)
	}
	got := <-gotCh
	if !bytes.Equal(got, payload) {
		t.Fatalf("对端解出 %q, 期望 %q", got, payload)
	}
	if !bytes.Equal(sent, payload) {
		t.Fatal("Write 改动了调用方缓冲")
	}
	_ = ma.Close()
	mb.Close()
}

// TestMSEWireMessages 定位级测试:完整走"嗅探→协商→BT 握手→wire 消息"
// 全链路,双向各发一条消息验证加密流在握手之后仍然对齐。
func TestMSEWireMessages(t *testing.T) {
	var skey [20]byte
	copy(skey[:], "01234567890123456789")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	bErr := make(chan error, 1)
	go func() { // B:入站完整路径
		conn, err := ln.Accept()
		if err != nil {
			bErr <- err
			return
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		pre, plain, err := sniffHandshake(conn)
		if err != nil {
			bErr <- fmt.Errorf("sniff: %w", err)
			return
		}
		var wire net.Conn = &mseConn{Conn: conn, pre: pre}
		if !plain {
			wire, err = mseAccept(conn, pre, skey)
			if err != nil {
				bErr <- fmt.Errorf("mseAccept: %w", err)
				return
			}
		}
		hs, err := ReadHandshake(wire)
		if err != nil {
			bErr <- fmt.Errorf("B读握手: %w", err)
			return
		}
		if hs.InfoHash != skey {
			bErr <- fmt.Errorf("B infohash 不符: %x", hs.InfoHash)
			return
		}
		if err := WriteHandshake(wire, Handshake{InfoHash: skey, PeerID: [20]byte{'B'}}, true, true, true); err != nil {
			bErr <- fmt.Errorf("B写握手: %w", err)
			return
		}
		// B 收 A 的 interested,再发自己的 bitfield
		m, err := ReadMsg(wire)
		if err != nil {
			bErr <- fmt.Errorf("B读消息: %w", err)
			return
		}
		if m.ID != msgInterested {
			bErr <- fmt.Errorf("B 收到 %d,期望 interested", m.ID)
			return
		}
		if err := WriteMsg(wire, msgBitfield, []byte{0xC0}); err != nil {
			bErr <- fmt.Errorf("B写bitfield: %w", err)
			return
		}
		bErr <- nil
	}()

	wire, enc, err := mseDialPrefer(func() (net.Conn, error) {
		return net.Dial("tcp4", addr)
	}, skey)
	if err != nil {
		t.Fatalf("A拨号: %v", err)
	}
	if !enc {
		t.Fatal("A 应协商成功加密,却走了明文回退")
	}
	wire.SetDeadline(time.Now().Add(10 * time.Second))
	if err := WriteHandshake(wire, Handshake{InfoHash: skey, PeerID: [20]byte{'A'}}, true, true, true); err != nil {
		t.Fatalf("A写握手: %v", err)
	}
	got, err := ReadHandshake(wire)
	if err != nil {
		t.Fatalf("A读握手: %v", err)
	}
	if got.PeerID != [20]byte{'B'} {
		t.Fatalf("A 读到的 PeerID %q 不是 B", got.PeerID)
	}
	if err := WriteMsg(wire, msgInterested, nil); err != nil {
		t.Fatalf("A写消息: %v", err)
	}
	m, err := ReadMsg(wire)
	if err != nil {
		t.Fatalf("A读消息: %v", err)
	}
	if m.ID != msgBitfield || len(m.Payload) != 1 || m.Payload[0] != 0xC0 {
		t.Fatalf("A 收到异常 bitfield: id=%d payload=%v", m.ID, m.Payload)
	}
	if err := <-bErr; err != nil {
		t.Fatal(err)
	}
}

// TestMSEIAPlaintextSelect:A 只提供明文(provide=0x01)且携带 IA(BT 握手),
// 验证 B 选择明文方法、IA 回填到读侧、协商后负载明文互通。
func TestMSEIAPlaintextSelect(t *testing.T) {
	var skey [20]byte
	copy(skey[:], "01234567890123456789")
	ia := make([]byte, 68)
	ia[0] = byte(len(protocolID))
	copy(ia[1:], protocolID)
	copy(ia[29:], skey[:]) // info_hash@29..49
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			bErr <- err
			return
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		defer conn.Close()
		pre, plain, err := sniffHandshake(conn)
		if err != nil {
			bErr <- fmt.Errorf("sniff: %w", err)
			return
		}
		if plain {
			bErr <- fmt.Errorf("应嗅探为 MSE")
			return
		}
		wire, err := mseAccept(conn, pre, skey)
		if err != nil {
			bErr <- fmt.Errorf("mseAccept: %w", err)
			return
		}
		mc, ok := wire.(*mseConn)
		if !ok || mc.enc != nil {
			bErr <- fmt.Errorf("select 应为明文(enc==nil),实际 %+v", wire)
			return
		}
		got := make([]byte, 68)
		if _, err := io.ReadFull(wire, got); err != nil {
			bErr <- fmt.Errorf("读回填 IA: %w", err)
			return
		}
		if !bytes.Equal(got, ia) {
			bErr <- fmt.Errorf("IA 回填不符")
			return
		}
		// 等 A 的明文消息,证明协商后负载互通(过早 Close 会 RST 掉 A 的写)
		m, err := ReadMsg(wire)
		if err != nil {
			bErr <- fmt.Errorf("B 读明文消息: %w", err)
			return
		}
		if m.ID != msgInterested {
			bErr <- fmt.Errorf("B 收到 %d,期望 interested", m.ID)
			return
		}
		bErr <- nil
	}()

	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	wire, err := mseInitiateOpts(conn, skey, cryptoPlaintext, ia)
	if err != nil {
		t.Fatalf("A 协商: %v", err)
	}
	mc, ok := wire.(*mseConn)
	if !ok || mc.enc != nil {
		t.Fatalf("A 侧 select 应为明文,实际 %+v", wire)
	}
	// 明文负载互通:A 发 interested,B 收 IA 后读到它
	if err := WriteMsg(wire, msgInterested, nil); err != nil {
		t.Fatalf("A 写: %v (B侧: %v)", err, <-bErr)
	}
	if err := <-bErr; err != nil {
		t.Fatal(err)
	}
}

// TestMSESkeyMismatch:B 的 info_hash 与 A 不同时步3 校验必须失败断连。
func TestMSESkeyMismatch(t *testing.T) {
	var skeyA, skeyB [20]byte
	copy(skeyA[:], "aaaaaaaaaaaaaaaaaaaa")
	copy(skeyB[:], "bbbbbbbbbbbbbbbbbbbb")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	bErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			bErr <- err
			return
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		defer conn.Close()
		pre, _, err := sniffHandshake(conn)
		if err != nil {
			bErr <- err
			return
		}
		if _, err := mseAccept(conn, pre, skeyB); err == nil {
			bErr <- fmt.Errorf("skey 不匹配却协商成功")
			return
		}
		bErr <- nil
	}()

	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := mseInitiate(conn, skeyA); err == nil {
		t.Fatal("对端拒绝后 A 应协商失败(B 已断连,步4 不可达)")
	}
	if err := <-bErr; err != nil {
		t.Fatal(err)
	}
}

// TestSniffHandshake:入站嗅探三分支——随机首字节(MSE)、0x13+协议名
// (明文)、0x13+非协议名(MSE,Ya 最高字节恰为 0x13 的碰撞)。
func TestSniffHandshake(t *testing.T) {
	cases := []struct {
		name  string
		head  []byte
		plain bool
		preN  int
	}{
		{"MSE随机首字节", []byte{0x7F, 1, 2}, false, 1},
		{"明文握手", append([]byte{byte(len(protocolID))}, protocolID...), true, 20},
		{"0x13碰撞", append([]byte{byte(len(protocolID))}, "NOTBTProtocol!!!!!!"...), false, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			go func() {
				b.Write(tc.head)
				time.Sleep(50 * time.Millisecond)
				b.Close()
			}()
			pre, plain, err := sniffHandshake(a)
			if err != nil {
				t.Fatalf("sniff: %v", err)
			}
			if plain != tc.plain || len(pre) != tc.preN {
				t.Fatalf("plain=%v pre=%d,期望 plain=%v pre=%d", plain, len(pre), tc.plain, tc.preN)
			}
		})
	}
}

// TestMSEDialPreferFallback:对端读不懂 MSE 立即断开时,应关闭原连接、
// 明文重拨成功(encrypted=false)。
func TestMSEDialPreferFallback(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// 第 1 次:模拟明文客户端,读到 MSE 首字节即断
		if c, err := ln.Accept(); err == nil {
			c.Read(make([]byte, 8))
			c.Close()
		}
		// 第 2 次:明文回退连接,保持到测试结束
		if c, err := ln.Accept(); err == nil {
			time.Sleep(3 * time.Second)
			c.Close()
		}
	}()
	var skey [20]byte
	copy(skey[:], "01234567890123456789")
	wire, enc, err := mseDialPrefer(func() (net.Conn, error) {
		return net.Dial("tcp4", ln.Addr().String())
	}, skey)
	if err != nil {
		t.Fatalf("回退失败: %v", err)
	}
	defer wire.Close()
	if enc {
		t.Fatal("对端不支持 MSE,应走明文回退")
	}
}
