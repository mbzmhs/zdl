package bt

import "net"

// MSE/PEP(Message Stream Encryption,事实标准的 BitTorrent 链路加密,
// 规范原文见 .zcode/notes/mse-spec.md)。分层:
//   - mse_negotiate.go:DH 密钥交换、SHA1 派生、5 步握手与流同步;
//   - mse.go(本文件):RC4 原语与 net.Conn 封装,协商完成后握手与
//     peer wire 全部帧(含 BT 握手本身)都过此层,上层无需感知密文。

// rc4Cipher 是标准 ARC4(KSA + PRGA),产出连续密钥流。MSE 在密钥派生后
// 丢弃每方向最初 1024 字节密钥流(见 newMSEStream),本原语不做丢弃。
// 读写方向各持一个实例,状态独立。
type rc4Cipher struct {
	s [256]byte
	i byte
	j byte
}

func newRC4(key []byte) *rc4Cipher {
	c := &rc4Cipher{}
	for i := range c.s {
		c.s[i] = byte(i)
	}
	var j byte
	for i := 0; i < 256; i++ {
		j += c.s[i] + key[i%len(key)]
		c.s[i], c.s[j] = c.s[j], c.s[i]
	}
	return c
}

// newMSEStream 建立 MSE 方向流:RC4 初始化后丢弃前 1024 字节密钥流。
// 同一方向的协商段与 payload 段必须共用本实例(规范:流连续,不重置)。
func newMSEStream(key []byte) *rc4Cipher {
	c := newRC4(key)
	var burn [1024]byte
	c.xorKeyStream(burn[:], burn[:])
	return c
}

// xorKeyStream 把 src 异或密钥流写入 dst(dst 可与 src 同底层数组)。
// 调用方保证 len(dst) >= len(src)(net.Conn 写契约要求不得改写调用方
// 缓冲,加密时始终写进内部缓冲)。
func (c *rc4Cipher) xorKeyStream(dst, src []byte) {
	i, j := c.i, c.j
	for k, b := range src {
		i++
		j += c.s[i]
		c.s[i], c.s[j] = c.s[j], c.s[i]
		dst[k] = b ^ c.s[c.s[i]+c.s[j]]
	}
	c.i, c.j = i, j
}

// mseConn 是协商完成后的连接形态,三种模式统一在一个类型:
//   - enc/dec 均非 nil:RC4 模式(crypto_select=0x02);
//   - enc/dec 均 nil 且 pre 有值:明文模式但有回填字节(B 的 IA);
//   - enc/dec 均 nil 且 pre 空:纯透传(入站嗅探为明文握手)。
//
// 内嵌 net.Conn 使 SetDeadline 等透传,上层 peerConn 毫无感知。
type mseConn struct {
	net.Conn
	enc *rc4Cipher // 发方向:明文 → 密文
	dec *rc4Cipher // 收方向:密文 → 明文
	pre []byte     // 待回填的预读/协商期载荷(优先于底层读出)
	// encrypted 区分 RC4 模式(true)与明文透传(select=plaintext 或
	// 嗅探为明文握手的回填壳),展示层判定加密用。
	encrypted bool
	// wbuf 发送加密暂存(Write 契约不得改写调用方缓冲)。
	wbuf []byte
}

func (m *mseConn) Read(p []byte) (int, error) {
	if len(m.pre) > 0 {
		n := copy(p, m.pre)
		m.pre = m.pre[n:]
		return n, nil
	}
	n, err := m.Conn.Read(p)
	if n > 0 && m.dec != nil {
		m.dec.xorKeyStream(p[:n], p[:n])
	}
	return n, err
}

func (m *mseConn) Write(p []byte) (int, error) {
	if m.enc == nil || len(p) == 0 {
		return m.Conn.Write(p)
	}
	if cap(m.wbuf) < len(p) {
		m.wbuf = make([]byte, len(p))
	}
	buf := m.wbuf[:len(p)]
	m.enc.xorKeyStream(buf, p)
	return m.Conn.Write(buf)
}
