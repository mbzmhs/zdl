package bt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const maxTrackerResp = 4 << 20 // tracker 应答上限 4MB,防御异常服务器

// HTTPAnnounce 向 http/https tracker 发起一次 announce(BEP-3/23)。
// client 允许注入代理与超时;info_hash/peer_id 是二进制串,
// url.Values.Encode 会逐字节百分号编码,符合 tracker 的期望。
// userAgent 为对外呈现的客户端名(tracker 侧可见;空则用默认)。
func HTTPAnnounce(ctx context.Context, client *http.Client, rawURL string, req AnnounceReq, userAgent string) (*AnnounceResp, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("tracker 地址非法: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, errTrackerScheme
	}
	q := u.Query()
	for k, vs := range announceQuery(req) {
		q[k] = vs
	}
	u.RawQuery = q.Encode()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = "BitComet 1.99"
	}
	hreq.Header.Set("User-Agent", ua)
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTrackerResp))
	if err != nil {
		return nil, err
	}
	return parseAnnounceResp(body)
}

// parseAnnounceResp 解析 tracker 的 bencode 应答(compact 与 dict 两种 peers 形态)。
func parseAnnounceResp(body []byte) (*AnnounceResp, error) {
	v, err := Decode(body)
	if err != nil {
		return nil, fmt.Errorf("tracker 应答解析失败: %w", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("tracker 应答不是 dict")
	}
	if fr, ok := d["failure reason"].(string); ok {
		return nil, fmt.Errorf("tracker 拒绝: %s", fr)
	}
	resp := &AnnounceResp{}
	resp.Interval, _ = d["interval"].(int64)
	resp.MinInterval, _ = d["min interval"].(int64)
	resp.Complete, _ = d["complete"].(int64)
	resp.Incomplete, _ = d["incomplete"].(int64)
	if resp.Interval <= 0 {
		resp.Interval = 1800
	}
	switch peers := d["peers"].(type) {
	case string:
		resp.Peers = append(resp.Peers, decodeCompactPeers(peers)...)
	case []any:
		for _, pe := range peers {
			pd, ok := pe.(map[string]any)
			if !ok {
				continue
			}
			ip, _ := pd["ip"].(string)
			port, _ := pd["port"].(int64)
			if ip == "" || port <= 0 || port > 65535 {
				continue
			}
			resp.Peers = append(resp.Peers, PeerAddr{IP: net.ParseIP(ip), Port: uint16(port)})
		}
	}
	if p6, ok := d["peers6"].(string); ok {
		resp.Peers = append(resp.Peers, decodeCompactPeers6(p6)...)
	}
	resp.Peers = dedupPeers(resp.Peers)
	return resp, nil
}
