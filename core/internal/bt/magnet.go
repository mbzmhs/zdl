package bt

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Magnet 是解析后的磁力链接。
type Magnet struct {
	// InfoHash 是用于 DHT/tracker/peer 握手的 20 字节信息哈希:v1 为
	// SHA-1(info);v2-only(btmh)为 SHA-256(info) 截断前 20 字节(BEP-52)。
	InfoHash [20]byte
	// V2Hash 是 btmh 携带的完整 SHA-256(info)(v2/hybrid 磁链非 nil),
	// 用于元数据交付后的 v2 校验。
	V2Hash   *[32]byte
	Name     string   // dn 显示名(可能为空,取到元数据后以元数据为准)
	Trackers []string // tr 列表,保序去重
	Peers    []string // x.pe 直连地址 host:port
}

// IsV2 报告磁链是否携带 v2 信息哈希。
func (m *Magnet) IsV2() bool { return m != nil && m.V2Hash != nil }

// ParseMagnet 解析 magnet URI。支持:
//   - xt=urn:btih: 40 位十六进制 / 32 位 base32(v1);
//   - xt=urn:btmh: 64 位十六进制(BEP-52 v2,SHA-256(info)),带或不带
//     "1220" multihash 前缀均可;hybrid 磁链同含 btih+btmh 时两者并存,
//     抓取用 btih、校验接受任一。非法 x.pe 忽略。
func ParseMagnet(uri string) (*Magnet, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("磁力链接格式错误: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "magnet") {
		return nil, errors.New("不是磁力链接(应以 magnet:?xt=urn:btih: 开头)")
	}
	q := u.Query()
	m := &Magnet{}
	m.Name = q.Get("dn")
	var got bool
	for _, xt := range q["xt"] {
		xt = strings.TrimSpace(xt)
		switch {
		case len(xt) >= 9 && strings.EqualFold(xt[:9], "urn:btmh:"):
			full, v1, err := decodeBTMH(xt[9:])
			if err != nil {
				return nil, err
			}
			if full != nil {
				m.V2Hash = full
			} else {
				// 罕见的 btmh 承载 v1 sha1 形式(1114 前缀)
				m.InfoHash = v1
				got = true
			}
		case len(xt) >= 9 && strings.EqualFold(xt[:9], "urn:btih:"):
			ih, err := decodeInfoHash(xt[9:])
			if err != nil {
				return nil, err
			}
			m.InfoHash = ih
			got = true
		}
	}
	if !got {
		if m.V2Hash == nil {
			return nil, errors.New("磁力链接缺少 xt=urn:btih: 或 xt=urn:btmh: 信息哈希")
		}
		// v2-only:BEP-52 规定 DHT/tracker/握手用 SHA-256(info) 截断 20 字节
		copy(m.InfoHash[:], m.V2Hash[:20])
	}
	seen := map[string]bool{}
	for _, tr := range q["tr"] {
		if tr = strings.TrimSpace(tr); tr == "" || seen[tr] {
			continue
		}
		seen[tr] = true
		m.Trackers = append(m.Trackers, tr)
	}
	for _, p := range q["x.pe"] {
		if _, _, err := net.SplitHostPort(strings.TrimSpace(p)); err == nil {
			m.Peers = append(m.Peers, strings.TrimSpace(p))
		}
	}
	return m, nil
}

// ParseDirectPeers 把磁力 x.pe 的 host:port 列表解析为 PeerAddr:IP 字面量
// 直接采用,域名经系统 DNS 解析;非法项忽略。
func ParseDirectPeers(list []string) []PeerAddr {
	var out []PeerAddr
	seen := map[string]bool{}
	for _, s := range list {
		host, portStr, err := net.SplitHostPort(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		pn, err := strconv.Atoi(portStr)
		if err != nil || pn <= 0 || pn > 65535 {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			ips, err := net.LookupIP(host)
			if err != nil || len(ips) == 0 {
				continue
			}
			ip = ips[0]
		}
		key := net.JoinHostPort(ip.String(), portStr)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, PeerAddr{IP: ip, Port: uint16(pn)})
	}
	return out
}

func decodeInfoHash(s string) ([20]byte, error) {
	var ih [20]byte
	s = strings.TrimSpace(s)
	switch len(s) {
	case 40:
		b, err := hex.DecodeString(strings.ToLower(s))
		if err != nil {
			return ih, fmt.Errorf("磁力链接 infohash 非法: %w", err)
		}
		copy(ih[:], b)
	case 32:
		b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
		if err != nil {
			return ih, fmt.Errorf("磁力链接 infohash 非法: %w", err)
		}
		copy(ih[:], b)
	default:
		return ih, fmt.Errorf("磁力链接 infohash 长度非法: %d", len(s))
	}
	return ih, nil
}

// decodeBTMH 解析 urn:btmh: 值。规范形态是多哈希编码:sha2-256 为
// "1220" + 64 位十六进制(BEP-52 示例),sha1 为 "1114" + 40 位十六进制;
// 也接受省略前缀的裸 64/40 位十六进制。
// 返回 (完整 32 字节 v2 哈希 | nil, v1 20 字节哈希, err)。
func decodeBTMH(s string) (*[32]byte, [20]byte, error) {
	var ih [20]byte
	s = strings.TrimSpace(s)
	switch {
	case len(s) == 68 && strings.HasPrefix(s, "1220"):
		s = s[4:]
	case len(s) == 44 && strings.HasPrefix(s, "1114"):
		s = s[4:]
	}
	switch len(s) {
	case 64:
		b, err := hex.DecodeString(strings.ToLower(s))
		if err != nil {
			return nil, ih, fmt.Errorf("磁力链接 v2 信息哈希非法: %w", err)
		}
		var h [32]byte
		copy(h[:], b)
		return &h, ih, nil
	case 40:
		b, err := hex.DecodeString(strings.ToLower(s))
		if err != nil {
			return nil, ih, fmt.Errorf("磁力链接 v2 信息哈希非法: %w", err)
		}
		copy(ih[:], b)
		return nil, ih, nil
	default:
		return nil, ih, fmt.Errorf("磁力链接 v2 信息哈希长度非法: %d", len(s))
	}
}
