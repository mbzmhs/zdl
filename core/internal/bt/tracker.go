package bt

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
)

// AnnounceEvent 是 tracker 事件。
type AnnounceEvent int

const (
	EventNone      AnnounceEvent = iota // 周期性汇报
	EventStarted                        // 任务开始
	EventCompleted                      // 下载完成(做种开始)
	EventStopped                        // 任务停止/暂停
)

func (e AnnounceEvent) String() string {
	switch e {
	case EventStarted:
		return "started"
	case EventCompleted:
		return "completed"
	case EventStopped:
		return "stopped"
	default:
		return ""
	}
}

// AnnounceReq 是 announce 请求参数(BEP-3)。
type AnnounceReq struct {
	InfoHash   [20]byte
	PeerID     [20]byte
	Port       uint16 // 本端监听端口(0 = 无入站能力)
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      AnnounceEvent
	Key        uint32 // 同一任务跨重启的身份键
	NumWant    int32
	IP         uint32 // 0 = 让 tracker 自行判断
}

// PeerAddr 是一个 peer 的地址。
type PeerAddr struct {
	IP   net.IP
	Port uint16
}

func (p PeerAddr) String() string { return net.JoinHostPort(p.IP.String(), strconv.Itoa(int(p.Port))) }

// AnnounceResp 是 announce 应答。
type AnnounceResp struct {
	Interval    int64 // 建议的 announce 间隔(秒)
	MinInterval int64 // 服务器允许的最小间隔(0 = 未提供)
	Complete    int64 // 做种数
	Incomplete  int64 // 下载中 peer 数
	Peers       []PeerAddr
}

// decodeCompactPeers 解析 BEP-23 compact 形式(6 字节 IPv4:port)。
func decodeCompactPeers(b string) []PeerAddr {
	if len(b)%6 != 0 {
		b = b[:len(b)/6*6]
	}
	out := make([]PeerAddr, 0, len(b)/6)
	for i := 0; i+6 <= len(b); i += 6 {
		out = append(out, PeerAddr{
			IP:   net.IP([]byte(b[i : i+4])).To4(),
			Port: uint16(b[i+4])<<8 | uint16(b[i+5]),
		})
	}
	return out
}

// decodeCompactPeers6 解析 BEP-7 IPv6 compact 形式(18 字节)。
func decodeCompactPeers6(b string) []PeerAddr {
	if len(b)%18 != 0 {
		b = b[:len(b)/18*18]
	}
	out := make([]PeerAddr, 0, len(b)/18)
	for i := 0; i+18 <= len(b); i += 18 {
		out = append(out, PeerAddr{
			IP:   net.IP([]byte(b[i : i+16])),
			Port: uint16(b[i+16])<<8 | uint16(b[i+17]),
		})
	}
	return out
}

// dedupPeers 去重并剔除零地址。
func dedupPeers(peers []PeerAddr) []PeerAddr {
	seen := map[string]bool{}
	out := peers[:0]
	for _, p := range peers {
		if p.IP == nil || p.Port == 0 {
			continue
		}
		k := p.IP.String() + ":" + strconv.Itoa(int(p.Port))
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// sortPeers 让测试与日志输出确定化。
func sortPeers(peers []PeerAddr) {
	sort.Slice(peers, func(i, j int) bool {
		a, b := peers[i].IP.String(), peers[j].IP.String()
		if a != b {
			return a < b
		}
		return peers[i].Port < peers[j].Port
	})
}

// announceQuery 组装 HTTP tracker 的公共查询参数(UDP 层复用字段值)。
func announceQuery(req AnnounceReq) url.Values {
	q := url.Values{}
	q.Set("info_hash", string(req.InfoHash[:]))
	q.Set("peer_id", string(req.PeerID[:]))
	q.Set("port", strconv.Itoa(int(req.Port)))
	q.Set("uploaded", strconv.FormatInt(req.Uploaded, 10))
	q.Set("downloaded", strconv.FormatInt(req.Downloaded, 10))
	q.Set("left", strconv.FormatInt(req.Left, 10))
	q.Set("compact", "1")
	q.Set("key", strconv.FormatUint(uint64(req.Key), 10))
	if req.NumWant > 0 {
		q.Set("numwant", strconv.Itoa(int(req.NumWant)))
	} else {
		q.Set("numwant", "50")
	}
	if req.Event != EventNone {
		q.Set("event", req.Event.String())
	}
	if req.IP != 0 {
		q.Set("ip", fmt.Sprintf("%d.%d.%d.%d", byte(req.IP>>24), byte(req.IP>>16), byte(req.IP>>8), byte(req.IP)))
	}
	return q
}

var errTrackerScheme = errors.New("不支持的 tracker 协议(应为 http/https/udp/udps)")
