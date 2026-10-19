package bt

import (
	"crypto/rand"
	"errors"
	"strings"
)

// 扩展消息 ID(BEP-10):扩展握手固定为 0;其余由我们在 m 字典里声明,
// 对端在它的 m 字典里声明它给我们的 ID——两个方向 ID 不必一致。
const (
	ExtHandshakeID uint8 = 0
	// 本地声明的 ID(出现在我们发送的 m 字典中)
	LocalMetadataID  uint8 = 1
	LocalPEXID       uint8 = 2
	LocalHolepunchID uint8 = 3
)

// ExtHandshake 是扩展握手 dict 的解析视图。
type ExtHandshake struct {
	M            map[string]uint8 // 对端为各扩展分配的 ID(按对端视角)
	MetadataSize int64            // ut_metadata 元数据总长
	ListenPort   int64            // p:对端监听端口(NAT 后与实际来源端口可能不同)
	Version      string           // v:客户端名
	ReqQ         int64            // reqq:对端允许的未完成请求数
}

// SupportsMetadata / SupportsPEX 查询对端是否声明对应扩展。
func (e *ExtHandshake) SupportsMetadata() bool { return e.M["ut_metadata"] != 0 }
func (e *ExtHandshake) SupportsPEX() bool      { return e.M["ut_pex"] != 0 }

// SupportsHolepunch 对端声明 BEP-12 ut_holepunch(可协助 UDP 打孔)。
func (e *ExtHandshake) SupportsHolepunch() bool { return e.M["ut_holepunch"] != 0 }

// PeerExtID 返回对端给某扩展分配的 ID;未声明返回 0(0 保留给握手,表示不支持)。
func (e *ExtHandshake) PeerExtID(name string) uint8 { return e.M[name] }

// ParseExtHandshake 解析扩展握手载荷(bencoded dict)。
func ParseExtHandshake(payload []byte) (*ExtHandshake, error) {
	v, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("扩展握手不是 dict")
	}
	e := &ExtHandshake{M: map[string]uint8{}}
	if md, ok := d["m"].(map[string]any); ok {
		for name, id := range md {
			n, _ := id.(int64)
			if n > 0 && n <= 255 {
				e.M[name] = uint8(n)
			}
		}
	}
	e.MetadataSize, _ = d["metadata_size"].(int64)
	if e.MetadataSize < 0 {
		e.MetadataSize = 0
	}
	e.ListenPort, _ = d["p"].(int64)
	e.Version, _ = d["v"].(string)
	e.ReqQ, _ = d["reqq"].(int64)
	return e, nil
}

// BuildExtHandshake 构造我们的扩展握手载荷。
// v 是对外呈现的客户端名(与 peer_id 伪装 -BC0199- 保持一致才能
// 维持 BitComet 身份,自定义名称会显示在对端界面上),空则用默认。
func BuildExtHandshake(metadataSize int64, listenPort int, v string) []byte {
	if v = strings.TrimSpace(v); v == "" {
		v = "BitComet 1.99"
	}
	d := map[string]any{
		"m": map[string]any{
			"ut_metadata":  int64(LocalMetadataID),
			"ut_pex":       int64(LocalPEXID),
			"ut_holepunch": int64(LocalHolepunchID), // BEP-12:能力通告,对端界面据此刻 UDP 打孔标位
		},
		"p":    int64(listenPort),
		"v":    v,
		"reqq": int64(250),
	}
	if metadataSize > 0 {
		d["metadata_size"] = metadataSize
	}
	return MustEncode(d)
}

// ---------- ut_metadata(BEP-9) ----------

const (
	metaReq    = 0
	metaData   = 1
	metaReject = 2
)

// maxMetadataSize 元数据(info dict)上限。规范建议 1MB 起步上限,
// 个别大种子可达数 MB,取 16MB 与 bencode 字符串上限一致。
const maxMetadataSize = maxStr

// MetaMsg 是一条 ut_metadata 消息。
type MetaMsg struct {
	Type      int64 // metaReq / metaData / metaReject
	Piece     int64
	TotalSize int64  // 仅数据消息
	Data      []byte // 仅数据消息:原始 info dict 分片
}

// BuildMetadataMsg 编码一条不含数据的 ut_metadata 消息(请求/拒绝)。
func BuildMetadataMsg(msgType, piece int64) []byte {
	return MustEncode(map[string]any{"msg_type": msgType, "piece": piece})
}

// BuildMetadataData 编码数据消息:dict + 原始字节。
func BuildMetadataData(piece, totalSize int64, data []byte) []byte {
	dict := MustEncode(map[string]any{"msg_type": int64(metaData), "piece": piece, "total_size": totalSize})
	return append(dict, data...)
}

// ParseMetadataMsg 解析 ut_metadata 载荷(前缀 dict + 可选原始数据)。
func ParseMetadataMsg(payload []byte) (*MetaMsg, error) {
	v, n, err := DecodePrefix(payload)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("ut_metadata 载荷不是 dict")
	}
	m := &MetaMsg{}
	m.Type, _ = d["msg_type"].(int64)
	m.Piece, _ = d["piece"].(int64)
	m.TotalSize, _ = d["total_size"].(int64)
	if m.Type == metaData {
		rest := payload[n:]
		if int64(len(rest)) > maxMetadataSize {
			return nil, errors.New("ut_metadata 数据超长")
		}
		m.Data = rest
	}
	return m, nil
}

// ---------- PEX(BEP-11) ----------

// PEXMsg 是一条 peer 交换消息。flags 位含义由对端定义,本端不解释。
type PEXMsg struct {
	Added   []PeerAddr
	AddedF  []byte // 与 Added 一一对应的 flags(原样透传)
	Dropped []PeerAddr
}

// ParsePEX 解析 PEX 载荷(added/added.f/dropped,均为 6 字节紧凑地址)。
func ParsePEX(payload []byte) (*PEXMsg, error) {
	v, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("PEX 载荷不是 dict")
	}
	m := &PEXMsg{}
	if a, ok := d["added"].(string); ok {
		m.Added = decodeCompactPeers(a)
	}
	if f, ok := d["added.f"].(string); ok {
		m.AddedF = []byte(f)
	}
	if dr, ok := d["dropped"].(string); ok {
		m.Dropped = decodeCompactPeers(dr)
	}
	// added 与 flags 数量对齐,超长截断
	if len(m.AddedF) > len(m.Added) {
		m.AddedF = m.AddedF[:len(m.Added)]
	}
	return m, nil
}

// BuildPEX 编码 PEX 载荷;IPv4 为主(added6 延后)。
func BuildPEX(added []PeerAddr, addedF []byte, dropped []PeerAddr) []byte {
	d := map[string]any{}
	if len(added) > 0 {
		d["added"] = compactPeersString(added)
		if len(addedF) > 0 {
			d["added.f"] = string(addedF)
		}
	}
	if len(dropped) > 0 {
		d["dropped"] = compactPeersString(dropped)
	}
	return MustEncode(d)
}

func compactPeersString(peers []PeerAddr) string {
	out := make([]byte, 0, len(peers)*6)
	for _, p := range peers {
		ip4 := p.IP.To4()
		if ip4 == nil {
			continue
		}
		out = append(out, ip4...)
		out = append(out, byte(p.Port>>8), byte(p.Port))
	}
	return string(out)
}

// ---------- ut_holepunch(BEP-12) ----------

// BEP-12 消息类型:rendezvous 请对端主动连接 (ip,port) 以打通双方 NAT;
// connect 预告"将有 peer 连入你";error 为失败回执。
const (
	hpRendezvous = 0
	hpConnect    = 1
	hpError      = 2
)

// HolepunchMsg 是一条 ut_holepunch 消息。
type HolepunchMsg struct {
	MsgType  int64
	IP       string
	Port     int64
	ErrorNum int64
	ErrorMsg string
}

// ParseHolepunch 解析 ut_holepunch 载荷。
func ParseHolepunch(payload []byte) (*HolepunchMsg, error) {
	v, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("ut_holepunch 载荷不是 dict")
	}
	m := &HolepunchMsg{}
	m.MsgType, _ = d["msg_type"].(int64)
	m.IP, _ = d["ip"].(string)
	m.Port, _ = d["port"].(int64)
	m.ErrorNum, _ = d["error_num"].(int64)
	m.ErrorMsg, _ = d["error_msg"].(string)
	return m, nil
}

// PeerID 生成 Azureus 风格 peer id。前缀采用 BitComet 1.99("-BC0199-"):
// 部分客户端(BitComet 兼容性白名单等)按 peer_id 前缀过滤非主流客户端,
// 自定义前缀会被对端拒连;且 BT swarm 以 BitComet 居多,故对外呈现为 BitComet。
func PeerID() [20]byte {
	var id [20]byte
	copy(id[:], "-BC0199-")
	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic(err) // 系统熵源不可用属致命环境问题
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i, c := range rnd {
		id[8+i] = alphabet[int(c)%len(alphabet)]
	}
	return id
}
