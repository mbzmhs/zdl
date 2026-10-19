package bt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const protocolID = "BitTorrent protocol"

// reserved 位:扩展协议(BEP-10)在 reserved[5],DHT(BEP-5)与
// fast extension(BEP-6)在 reserved[7]。
const (
	extProtocolBit = 0x10
	dhtBit         = 0x01
	fastBit        = 0x04
)

// peer wire 消息 ID(BEP-3);msgExtended 的载荷首字节为扩展消息 ID。
const (
	msgChoke         = 0
	msgUnchoke       = 1
	msgInterested    = 2
	msgNotInterested = 3
	msgHave          = 4
	msgBitfield      = 5
	msgRequest       = 6
	msgPiece         = 7
	msgCancel        = 8
	msgExtended      = 20
	// fast extension(BEP-6):HaveAll=14 / HaveNone=15 / Reject=16。
	// 旧实现三者错位(14 当 Reject、15 当 HaveAll、16 当 HaveNone)——
	// qBittorrent 等默认用 fast extension 的客户端以 HaveAll 代替完整
	// 位图,被当成 Reject 丢弃后这些种子在我们眼里永远是 0%、一帧不供;
	// 而我方发出的 HaveAll 也带错 ID,对端以为我们是空 leecher,做种
	// 形同虚设。回环测试两侧共用同一张错表,对称抵消,故从未暴露。
	msgSuggest     = 13
	msgHaveAll     = 14
	msgHaveNone    = 15
	msgRejectReq   = 16
	msgAllowedFast = 17
	// MsgKeepalive 不是线上协议值:长度为 0 的 keepalive 帧在 ReadMsg 中映射为它。
	MsgKeepalive byte = 0xFF
)

// maxMsgLen 单帧上限。piece 帧 = 9 + 块长(标准 16KiB),留出大块客户端余量。
const maxMsgLen = 4 << 20

// BlockSize 是请求块大小(16KiB,BEP-3 事实标准)。
const BlockSize = 1 << 14

// Msg 是一条已解帧的 peer 消息。
type Msg struct {
	ID      uint8
	Payload []byte
}

// Handshake 是 BT 握手(68 字节)。
type Handshake struct {
	Reserved [8]byte
	InfoHash [20]byte
	PeerID   [20]byte
}

// SupportsExt / SupportsDHT / SupportsFast 读取对端 reserved 能力位。
func (h *Handshake) SupportsExt() bool  { return h.Reserved[5]&extProtocolBit != 0 }
func (h *Handshake) SupportsDHT() bool  { return h.Reserved[7]&dhtBit != 0 }
func (h *Handshake) SupportsFast() bool { return h.Reserved[7]&fastBit != 0 }

// WriteHandshake 写出握手帧。
func WriteHandshake(w io.Writer, h Handshake, ext, dht, fast bool) error {
	buf := make([]byte, 0, 68)
	buf = append(buf, byte(len(protocolID)))
	buf = append(buf, protocolID...)
	reserved := h.Reserved
	if ext {
		reserved[5] |= extProtocolBit
	}
	if dht {
		reserved[7] |= dhtBit
	}
	if fast {
		reserved[7] |= fastBit
	}
	buf = append(buf, reserved[:]...)
	buf = append(buf, h.InfoHash[:]...)
	buf = append(buf, h.PeerID[:]...)
	_, err := w.Write(buf)
	return err
}

// ReadHandshake 读取对端握手帧。
func ReadHandshake(r io.Reader) (*Handshake, error) {
	var l [1]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	if l[0] != byte(len(protocolID)) {
		return nil, fmt.Errorf("握手协议名长度非法: %d", l[0])
	}
	name := make([]byte, len(protocolID))
	if _, err := io.ReadFull(r, name); err != nil {
		return nil, err
	}
	if string(name) != protocolID {
		return nil, errors.New("对端不是 BitTorrent 协议")
	}
	rest := make([]byte, 48)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	h := &Handshake{}
	copy(h.Reserved[:], rest[0:8])
	copy(h.InfoHash[:], rest[8:28])
	copy(h.PeerID[:], rest[28:48])
	return h, nil
}

// WriteMsg 写一条消息(自动加 4 字节长度前缀)。整帧合并为单次 Write:
// 拆成头/ID/体三次写在对端会落地成多个 TCP 小段(NODELAY 下无合并),
// syscall 与包数都是三倍,高速时是确定的吞吐损耗。
func WriteMsg(w io.Writer, id uint8, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf[4] = id
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// WriteKeepalive 发送 keepalive(长度为 0 的帧)。
func WriteKeepalive(w io.Writer) error {
	_, err := w.Write([]byte{0, 0, 0, 0})
	return err
}

// ReadMsg 读取一条消息;keepalive 返回 ID=MsgKeepalive。
func ReadMsg(r io.Reader) (Msg, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Msg{}, err
	}
	l := binary.BigEndian.Uint32(head[:])
	if l == 0 {
		return Msg{ID: MsgKeepalive}, nil
	}
	if l > maxMsgLen {
		return Msg{}, fmt.Errorf("消息超长: %d", l)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Msg{}, err
	}
	return Msg{ID: buf[0], Payload: buf[1:]}, nil
}

// requestPayload 编码 request / cancel 载荷:index、begin、length 各 4 字节。
func requestPayload(index, begin, length uint32) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[0:4], index)
	binary.BigEndian.PutUint32(p[4:8], begin)
	binary.BigEndian.PutUint32(p[8:12], length)
	return p
}

// BuildRequest 编码一条 request 消息载荷。
func BuildRequest(index, begin, length uint32) []byte { return requestPayload(index, begin, length) }

// BuildCancel 编码一条 cancel 消息载荷。
func BuildCancel(index, begin, length uint32) []byte { return requestPayload(index, begin, length) }

// BuildHave 编码一条 have 消息载荷。
func BuildHave(index uint32) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, index)
	return p
}

// ParsePiece 解析 piece 消息载荷,返回 piece 下标、块内偏移与块数据。
func ParsePiece(payload []byte) (index, begin uint32, data []byte, err error) {
	if len(payload) < 8 {
		return 0, 0, nil, errors.New("piece 消息过短")
	}
	index = binary.BigEndian.Uint32(payload[0:4])
	begin = binary.BigEndian.Uint32(payload[4:8])
	return index, begin, payload[8:], nil
}

// ParseRequest 解析 request/cancel 载荷。
func ParseRequest(payload []byte) (index, begin, length uint32, err error) {
	if len(payload) < 12 {
		return 0, 0, 0, errors.New("request 消息过短")
	}
	index = binary.BigEndian.Uint32(payload[0:4])
	begin = binary.BigEndian.Uint32(payload[4:8])
	length = binary.BigEndian.Uint32(payload[8:12])
	return index, begin, length, nil
}

// bitfieldBytes 把 have 位图压成 bitfield 载荷(高位在前,按 BEP-3)。
func bitfieldBytes(have []bool) []byte {
	out := make([]byte, (len(have)+7)/8)
	for i, h := range have {
		if h {
			out[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return out
}

// parseBitfield 解析 bitfield 载荷为 have 位图;长度必须恰为 nPieces。
func parseBitfield(payload []byte, nPieces int) ([]bool, error) {
	if nPieces == 0 {
		return nil, nil
	}
	if len(payload) != (nPieces+7)/8 {
		return nil, fmt.Errorf("bitfield 长度不符: %d != %d", len(payload), (nPieces+7)/8)
	}
	out := make([]bool, nPieces)
	for i := 0; i < nPieces; i++ {
		out[i] = payload[i/8]&(1<<(7-uint(i%8))) != 0
	}
	return out, nil
}
