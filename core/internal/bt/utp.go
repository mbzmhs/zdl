package bt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// uTP(BEP-29,Micro Transport Protocol):在 UDP 上提供类 TCP 的可靠有序
// 传输,拥塞控制用 LEDBAT(目标单向排队延迟 100ms)。对上层是普通
// net.Conn——BT 握手、MSE、peer wire 全部无感知地跑在其上。
// 规范全文见 .zcode/notes/utp-spec.md。

// 包类型(0..4,BEP-29 §Packet types)。
const (
	utpData  byte = 0 // 数据包
	utpFin   byte = 1 // 收尾关闭
	utpState byte = 2 // 纯 ACK(不携带载荷,不增加 seq_nr)
	utpReset byte = 3 // 强制断开(类 TCP RST)
	utpSyn   byte = 4 // 建立连接(类 TCP SYN)
)

const (
	utpVersion   = 1   // 当前协议版本
	utpHeaderLen = 20  // 基本头长度
	utpMinPktLen = 150 // 实现最小包长(BEP-29:窗口最小档)
	// uTP 端口:与 TCP 监听同号(实测部署惯例,BEP-29 未规定;
	// 对端靠 announce 的同端口向我们 UDP 发 SYN)。
)

// utpHeader 是 20 字节基本头(全大端):
//
//	0     type(4)|ver(4)   1     extension    2..3   connection_id
//	4..7  timestamp         8..11 timestamp_diff
//	12..15 wnd_size         16..17 seq_nr      18..19 ack_nr
type utpHeader struct {
	pktType   byte
	ver       byte
	extension byte // 第一个扩展类型;0 = 无扩展
	connID    uint16
	timestamp uint32 // 发送方当前时间(微秒,32 位回绕)
	tsDiff    uint32 // 本机时间 - 对端发送此包时的时间(微秒)
	wndSize   uint32 // 接收窗口(字节)
	seqNr     uint16
	ackNr     uint16
}

func (h *utpHeader) marshal(buf []byte) {
	buf[0] = h.pktType<<4 | h.ver
	buf[1] = h.extension
	binary.BigEndian.PutUint16(buf[2:4], h.connID)
	binary.BigEndian.PutUint32(buf[4:8], h.timestamp)
	binary.BigEndian.PutUint32(buf[8:12], h.tsDiff)
	binary.BigEndian.PutUint32(buf[12:16], h.wndSize)
	binary.BigEndian.PutUint16(buf[16:18], h.seqNr)
	binary.BigEndian.PutUint16(buf[18:20], h.ackNr)
}

func parseUTPHeader(b []byte) (utpHeader, error) {
	if len(b) < utpHeaderLen {
		return utpHeader{}, errors.New("uTP 包过短")
	}
	h := utpHeader{
		pktType:   b[0] >> 4,
		ver:       b[0] & 0x0F,
		extension: b[1],
		connID:    binary.BigEndian.Uint16(b[2:4]),
		timestamp: binary.BigEndian.Uint32(b[4:8]),
		tsDiff:    binary.BigEndian.Uint32(b[8:12]),
		wndSize:   binary.BigEndian.Uint32(b[12:16]),
		seqNr:     binary.BigEndian.Uint16(b[16:18]),
		ackNr:     binary.BigEndian.Uint16(b[18:20]),
	}
	if h.ver != utpVersion {
		return utpHeader{}, fmt.Errorf("uTP 版本不支持: %d", h.ver)
	}
	if h.pktType > utpSyn {
		return utpHeader{}, fmt.Errorf("uTP 包类型非法: %d", h.pktType)
	}
	return h, nil
}

// utpExt 是扩展链上的一个扩展。链格式(BEP-29):
// 主头 extension 字节 = 首扩展类型;其后每块 [nextType(1), len(1),
// data(len)],nextType=0 结束链。未知扩展按 len 跳过。
type utpExt struct {
	typ  byte
	data []byte
}

// parseUTPExts 解析基本头之后的扩展链;hdrExt 为首扩展类型(0=无扩展)。
func parseUTPExts(b []byte, hdrExt byte) ([]utpExt, int, error) {
	if hdrExt == 0 {
		return nil, 0, nil
	}
	var out []utpExt
	consumed := 0
	next := hdrExt
	for next != 0 {
		if len(b)-consumed < 2 {
			return nil, consumed, errors.New("uTP 扩展链截断")
		}
		nType := b[consumed]
		l := int(b[consumed+1])
		if len(b)-consumed-2 < l {
			return nil, consumed, errors.New("uTP 扩展数据截断")
		}
		out = append(out, utpExt{typ: next, data: b[consumed+2 : consumed+2+l]})
		consumed += 2 + l
		next = nType
	}
	return out, consumed, nil
}
