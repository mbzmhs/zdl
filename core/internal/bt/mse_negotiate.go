package bt

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
)

// MSE 5 步握手(全文见 .zcode/notes/mse-spec.md,源出规范原文
// encryption.txt + anacrolix/libtorrent/Transmission 三方交叉核对):
//
//	1 A→B: Ya(96) || PadA(0..512)
//	2 B→A: Yb(96) || PadB(0..512)
//	3 A→B: SHA1("req1"‖S) || SHA1("req2"‖SKEY) XOR SHA1("req3"‖S)
//	       || ENCRYPT(VC||provide||len(PadC)||PadC||len(IA)||IA)
//	4 B→A: ENCRYPT(VC||select||len(PadD)||PadD) || ENCRYPT2(Payload)
//	5 A→B: ENCRYPT2(Payload)
//
// ENCRYPT = RC4(SHA1("keyA/B"‖S‖SKEY)),每方向丢弃前 1024 字节;
// A 发送用 keyA、接收用 keyB,B 反之;协商段与 payload 段共用同一条流。
const (
	// mseSpecP 是规范指定的 768-bit 安全素数(勿与 RFC 2409/3526 混用)。
	mseSpecP = "0xFFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563"

	mseMaxPad = 512 // PadA..PadD 各自上限

	// crypto_provide/select 位:0x01 明文(控制段仍 RC4,协商后负载明文),
	// 0x02 RC4(续用同一条流)。A 可给多 bit,B 必须选单 bit。
	cryptoPlaintext uint32 = 0x01
	cryptoRC4       uint32 = 0x02

	// 同步窗口:Y(96)之后,VC 同步最多再读 512+8=520,req1 同步 512+20=532。
	mseSyncVC   = mseMaxPad + 8
	mseSyncReq1 = mseMaxPad + 20
)

var (
	mseP     *big.Int             // 规范素数
	mseG     = big.NewInt(2)      // 生成元
	mseLabel = map[string][]byte{ // KDF 标签(4 字节 ASCII)
		"req1": []byte("req1"), "req2": []byte("req2"), "req3": []byte("req3"),
		"keyA": []byte("keyA"), "keyB": []byte("keyB"),
	}
	mseVCPrefix [8]byte // VC = 8×0x00
)

func init() {
	p, ok := new(big.Int).SetString(mseSpecP, 0)
	if !ok {
		panic("bt: MSE 素数解析失败")
	}
	mseP = p
}

func mseSHA1(parts ...[]byte) []byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// mseGenKey 生成 160-bit DH 私钥(规范:128..180 bit)。
func mseGenKey() (*big.Int, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b[:]), nil
}

// msePadLeft 左侧补零到定长(Y/S 的 96 字节大端表示)。
func msePadLeft(b []byte, n int) []byte {
	if len(b) == n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// mseCheckPub 校验对端公钥 ∈ [2, P-2](防恶意输入把共享密钥退化)。
func mseCheckPub(y *big.Int) error {
	if y.Cmp(big.NewInt(2)) < 0 || y.Cmp(new(big.Int).Sub(mseP, big.NewInt(2))) > 0 {
		return errors.New("MSE 对端公钥越界")
	}
	return nil
}

// mseRandPad 生成 0..512 字节随机填充(步1/2 的 PadA/PadB 用随机内容,
// 步3/4 的 PadC/PadD 规范建议置零,由调用方用 zeroPad)。
func mseRandPad() []byte {
	n, err := rand.Int(rand.Reader, big.NewInt(mseMaxPad+1))
	if err != nil {
		panic(err) // crypto/rand 失败不可恢复
	}
	p := make([]byte, int(n.Int64()))
	rand.Read(p)
	return p
}

// suffix-free 滑窗同步:从 r 读取直到模式 b 完整出现,不多读模式之后的
// 字节(模式尾字节即最后读入的字节)。总读取量由 r 侧 LimitReader 截断。
func readUntil(r io.Reader, b []byte) error {
	win := make([]byte, len(b))
	if _, err := io.ReadFull(r, win); err != nil {
		return err
	}
	for !bytes.Equal(win, b) {
		copy(win, win[1:])
		if _, err := io.ReadFull(r, win[len(win)-1:]); err != nil {
			return err
		}
	}
	return nil
}

// readEnc 从 r 读 n 字节并用 c 解密(推进 c 的流状态)。
func readEnc(r io.Reader, c *rc4Cipher, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	c.xorKeyStream(buf, buf)
	return buf, nil
}

// writeEnc 用 c 加密 src 并写入 w(一次调用对应规范的一次 ENCRYPT,
// 流连续不重置)。
func writeEnc(w io.Writer, c *rc4Cipher, src []byte) error {
	buf := make([]byte, len(src))
	c.xorKeyStream(buf, src)
	_, err := w.Write(buf)
	return err
}

// mseInitiate 执行 A 侧(发起方)握手。成功返回协商后的连接:
// RC4 模式为 mseConn(enc=keyA 流, dec=keyB 流),select=明文则透传。
// IA 固定为 0(BT 握手留在协商后走现有 WriteHandshake,规范允许)。
func mseInitiate(conn net.Conn, skey [20]byte) (net.Conn, error) {
	return mseInitiateOpts(conn, skey, cryptoPlaintext|cryptoRC4, nil)
}

// mseInitiateOpts 参数化版:provide 与 IA 可控(测试覆盖明文选择、
// IA 回填等非默认组合;生产路径只用默认值)。
func mseInitiateOpts(conn net.Conn, skey [20]byte, provide uint32, ia []byte) (net.Conn, error) {
	x, err := mseGenKey()
	if err != nil {
		return nil, err
	}
	y := new(big.Int).Exp(mseG, x, mseP)
	// 步1:Ya || PadA
	if _, err := conn.Write(msePadLeft(y.Bytes(), 96)); err != nil {
		return nil, fmt.Errorf("MSE 发送 Ya: %w", err)
	}
	if _, err := conn.Write(mseRandPad()); err != nil {
		return nil, fmt.Errorf("MSE 发送 PadA: %w", err)
	}
	// 步2:读 Yb(96 定长;其后 PadB 留在流中由 readUntil 同步)
	var yb [96]byte
	if _, err := io.ReadFull(conn, yb[:]); err != nil {
		return nil, fmt.Errorf("MSE 读 Yb: %w", err)
	}
	Y := new(big.Int).SetBytes(yb[:])
	if err := mseCheckPub(Y); err != nil {
		return nil, err
	}
	S := new(big.Int).Exp(Y, x, mseP)
	s := msePadLeft(S.Bytes(), 96)

	enc := newMSEStream(mseSHA1(mseLabel["keyA"], s, skey[:])) // A 发送
	dec := newMSEStream(mseSHA1(mseLabel["keyB"], s, skey[:])) // A 接收

	// 步3:req1hash || xorhash(明文) || ENCRYPT(VC||provide||PadC||IA)
	if _, err := conn.Write(mseSHA1(mseLabel["req1"], s)); err != nil {
		return nil, fmt.Errorf("MSE 发送 req1: %w", err)
	}
	xorh := make([]byte, 20)
	a := mseSHA1(mseLabel["req2"], skey[:])
	b := mseSHA1(mseLabel["req3"], s)
	for i := range xorh {
		xorh[i] = a[i] ^ b[i]
	}
	if _, err := conn.Write(xorh); err != nil {
		return nil, fmt.Errorf("MSE 发送 req2/3: %w", err)
	}
	padC := make([]byte, mseRandPadLen()) // PadC 置零(规范建议)
	body := make([]byte, 0, 8+4+2+len(padC)+2+len(ia))
	body = append(body, mseVCPrefix[:]...)
	body = binary.BigEndian.AppendUint32(body, provide)
	body = binary.BigEndian.AppendUint16(body, uint16(len(padC)))
	body = append(body, padC...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(ia)))
	body = append(body, ia...)
	if err := writeEnc(conn, enc, body); err != nil {
		return nil, fmt.Errorf("MSE 发送步3: %w", err)
	}

	// 步4 同步:在 PadB(明文)后的密文中定位 ENCRYPT(VC)。
	// eVC = dec 密钥流前 8 字节 XOR VC(VC 全 0 → eVC 即密钥流前 8 字节);
	// 生成 eVC 已消费 8 字节,匹配的正是 VC 密文,之后流状态恰好对齐。
	eVC := make([]byte, 8)
	dec.xorKeyStream(eVC, mseVCPrefix[:])
	if err := readUntil(io.LimitReader(conn, mseSyncVC), eVC); err != nil {
		return nil, fmt.Errorf("MSE 同步 VC: %w", err)
	}
	selBuf, err := readEnc(conn, dec, 6) // crypto_select(4) + len(PadD)(2)
	if err != nil {
		return nil, fmt.Errorf("MSE 读 select: %w", err)
	}
	selected := binary.BigEndian.Uint32(selBuf[0:4])
	if selected != cryptoRC4 && selected != cryptoPlaintext {
		return nil, fmt.Errorf("MSE 选择了不支持的方法: %#x", selected)
	}
	padLen := binary.BigEndian.Uint16(selBuf[4:6])
	if padLen > 0 {
		if _, err := readEnc(conn, dec, int(padLen)); err != nil {
			return nil, fmt.Errorf("MSE 读 PadD: %w", err)
		}
	}
	if selected == cryptoRC4 {
		return &mseConn{Conn: conn, enc: enc, dec: dec, encrypted: true}, nil
	}
	return &mseConn{Conn: conn}, nil // select=明文:透传
}

// mseAccept 执行 B 侧(应答方)握手。pre 是入站嗅探预读字节(1..20,
// 步1 的 Ya 前缀);skey 必须与 A 的 info_hash 一致,否则步3 校验失败断连。
// 成功返回的连接已把 A 的 IA(BT 握手等,可为空)置于读缓冲前端。
func mseAccept(conn net.Conn, pre []byte, skey [20]byte) (net.Conn, error) {
	return mseAcceptFn(conn, pre, func(S []byte, xorh *[20]byte) ([20]byte, bool) {
		a := mseSHA1(mseLabel["req2"], skey[:])
		b := mseSHA1(mseLabel["req3"], S)
		var want [20]byte
		for i := range want {
			want[i] = a[i] ^ b[i]
		}
		return skey, want == *xorh
	})
}

// mseAcceptFn 同 mseAccept,但 SKEY 在步3 由 findSkey 动态选定:入参为共
// 享密钥 S 与对端发来的 req2⊕req3 摘要,返回本端认可的 info_hash 及是否
// 匹配。多会话共享一个监听端口时,由它在全部注册会话里挑出对端要连的那
// 个(等价 libtorrent 的 skey→torrent 路由表)。
func mseAcceptFn(conn net.Conn, pre []byte, findSkey func(S []byte, xorh *[20]byte) ([20]byte, bool)) (net.Conn, error) {
	var r io.Reader = conn
	if len(pre) > 0 {
		r = io.MultiReader(bytes.NewReader(pre), conn)
	}
	// 步1:读 Ya
	var ya [96]byte
	if _, err := io.ReadFull(r, ya[:]); err != nil {
		return nil, fmt.Errorf("MSE 读 Ya: %w", err)
	}
	Y := new(big.Int).SetBytes(ya[:])
	if err := mseCheckPub(Y); err != nil {
		return nil, err
	}
	x, err := mseGenKey()
	if err != nil {
		return nil, err
	}
	S := new(big.Int).Exp(Y, x, mseP)
	s := msePadLeft(S.Bytes(), 96)

	// 步2:Yb || PadB
	y := new(big.Int).Exp(mseG, x, mseP)
	if _, err := conn.Write(msePadLeft(y.Bytes(), 96)); err != nil {
		return nil, fmt.Errorf("MSE 发送 Yb: %w", err)
	}
	if _, err := conn.Write(mseRandPad()); err != nil {
		return nil, fmt.Errorf("MSE 发送 PadB: %w", err)
	}

	// 步3 同步:PadA 之后找明文 req1hash。
	if err := readUntil(io.LimitReader(conn, mseSyncReq1), mseSHA1(mseLabel["req1"], s)); err != nil {
		return nil, fmt.Errorf("MSE 同步 req1: %w", err)
	}
	var xorh [20]byte
	if _, err := io.ReadFull(conn, xorh[:]); err != nil {
		return nil, fmt.Errorf("MSE 读 req2/3: %w", err)
	}
	// SKEY 选路:SHA1("req2"‖SKEY) XOR SHA1("req3"‖S) 必须等于收到的值。
	skey, ok := findSkey(s, &xorh)
	if !ok {
		return nil, errors.New("MSE SKEY 不匹配")
	}

	dec := newMSEStream(mseSHA1(mseLabel["keyA"], s, skey[:])) // 收 A 的流
	enc := newMSEStream(mseSHA1(mseLabel["keyB"], s, skey[:])) // 发己方流

	vc, err := readEnc(conn, dec, 8)
	if err != nil {
		return nil, fmt.Errorf("MSE 读 VC: %w", err)
	}
	if !bytes.Equal(vc, mseVCPrefix[:]) {
		return nil, errors.New("MSE VC 校验失败")
	}
	provBuf, err := readEnc(conn, dec, 6) // crypto_provide(4) + len(PadC)(2)
	if err != nil {
		return nil, fmt.Errorf("MSE 读 provide: %w", err)
	}
	provide := binary.BigEndian.Uint32(provBuf[0:4])
	chosen := provide & cryptoRC4
	if chosen == 0 {
		chosen = provide & cryptoPlaintext
	}
	if chosen == 0 {
		return nil, fmt.Errorf("MSE 对端未提供可用加密方式: %#x", provide)
	}
	padLen := binary.BigEndian.Uint16(provBuf[4:6])
	if padLen > 0 {
		if _, err := readEnc(conn, dec, int(padLen)); err != nil {
			return nil, fmt.Errorf("MSE 读 PadC: %w", err)
		}
	}
	lenIABuf, err := readEnc(conn, dec, 2)
	if err != nil {
		return nil, fmt.Errorf("MSE 读 len(IA): %w", err)
	}
	lenIA := binary.BigEndian.Uint16(lenIABuf)
	var ia []byte
	if lenIA > 0 {
		if ia, err = readEnc(conn, dec, int(lenIA)); err != nil {
			return nil, fmt.Errorf("MSE 读 IA: %w", err)
		}
	}

	// 步4:ENCRYPT(VC||select||PadD),与后续 payload 共用 enc 流。
	padD := make([]byte, mseRandPadLen())
	resp := make([]byte, 0, 8+4+2+len(padD))
	resp = append(resp, mseVCPrefix[:]...)
	resp = binary.BigEndian.AppendUint32(resp, chosen)
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(padD)))
	resp = append(resp, padD...)
	if err := writeEnc(conn, enc, resp); err != nil {
		return nil, fmt.Errorf("MSE 发送步4: %w", err)
	}
	if chosen == cryptoRC4 {
		return &mseConn{Conn: conn, enc: enc, dec: dec, pre: ia, encrypted: true}, nil
	}
	return &mseConn{Conn: conn, pre: ia}, nil
}

// mseRandPadLen 返回 0..512 的随机填充长度(PadC/PadD 置零用)。
func mseRandPadLen() int {
	n, err := rand.Int(rand.Reader, big.NewInt(mseMaxPad+1))
	if err != nil {
		panic(err)
	}
	return int(n.Int64())
}

// sniffHandshake 入站嗅探:预读最多 20 字节判定明文 BT 握手还是 MSE。
// 明文首 20 字节为 0x13 "BitTorrent protocol";MSE 步1 首字节是 Ya 的
// 最高字节(随机),恰为 0x13 时读满 20 比对即可区分(碰撞概率 2^-152)。
// 返回 pre(须回填给后续读)与是否明文。
func sniffHandshake(conn net.Conn) (pre []byte, plain bool, err error) {
	var b [1]byte
	if _, err = io.ReadFull(conn, b[:]); err != nil {
		return nil, false, err
	}
	if b[0] != byte(len(protocolID)) {
		return b[:], false, nil // MSE:预读 1 字节
	}
	var rest [19]byte
	if _, err = io.ReadFull(conn, rest[:]); err != nil {
		return nil, false, err
	}
	pre = append(b[:], rest[:]...)
	return pre, string(rest[:]) == protocolID, nil
}

// mseDialPrefer 以加密优先建立出站连接:MSE 协商失败(对端不支持或主动
// 拒绝)立即关闭原连接、用同一 dial 明文重拨(规范:回退无延迟)。
// 返回 (连接, 是否已加密, 错误);明文重拨也失败时返回其错误。
func mseDialPrefer(dial func() (net.Conn, error), skey [20]byte) (net.Conn, bool, error) {
	conn, err := dial()
	if err != nil {
		return nil, false, err
	}
	wire, err := mseInitiate(conn, skey)
	if err == nil {
		return wire, true, nil
	}
	conn.Close()
	conn2, err2 := dial()
	if err2 != nil {
		return nil, false, fmt.Errorf("MSE 失败(%v),明文重拨也失败: %w", err, err2)
	}
	return conn2, false, nil
}
