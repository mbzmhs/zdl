package bt

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NetConfig 元数据抓取的网络出口(代理注入点;零值 = 直连默认)。
type NetConfig struct {
	// HTTPClient 用于 http/https tracker announce;nil 用默认直连客户端。
	HTTPClient *http.Client
	// Dialer 用于 peer 原始 TCP 连接;nil 直连。
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// UDPRelay 可选共享 UDP 中继:udp tracker announce 经其发收。
	UDPRelay net.PacketConn
	// Resolver 自定义 DNS 解析(DoH,规避本地 DNS 污染);nil = 系统 DNS。
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
	// SpoofName 对外呈现的客户端名(扩展握手 v / tracker UA);空 = 默认。
	SpoofName string
}

// FetchMetadata 为磁力链接获取元数据(BEP-9):经 tracker/DHT/direct peer 拨号,
// 向支持 ut_metadata 的对端分片拉取 info dict,按 v1 SHA-1 校验后解析。
// 无 tracker 且无 x.pe 的磁链依赖 dht(DHTServer)发现 peer;三者皆缺时返回明确错误。
func FetchMetadata(ctx context.Context, ih [20]byte, pid [20]byte, trackers []string, directPeers []PeerAddr, announcePort int, dht *DHTServer, nc NetConfig) (*MetaInfo, error) {
	return fetchMeta(ctx, ih, nil, pid, trackers, directPeers, announcePort, dht, nc)
}

// FetchMetadataV2 同 FetchMetadata,但按 BEP-52 校验交付的 info dict:
// SHA-256(info) 须等于 v2(btmh 携带的完整哈希)。v2 为 nil 时退回 v1 校验。
// 握手/DHT/tracker 仍用 20 字节截断 infohash(ih),v2 swarm 在线路上就是它。
func FetchMetadataV2(ctx context.Context, ih [20]byte, v2 *[32]byte, pid [20]byte, trackers []string, directPeers []PeerAddr, announcePort int, dht *DHTServer, nc NetConfig) (*MetaInfo, error) {
	return fetchMeta(ctx, ih, v2, pid, trackers, directPeers, announcePort, dht, nc)
}

func fetchMeta(ctx context.Context, ih [20]byte, v2 *[32]byte, pid [20]byte, trackers []string, directPeers []PeerAddr, announcePort int, dht *DHTServer, nc NetConfig) (*MetaInfo, error) {
	h := &metaHolder{ih: ih, v2: v2, pieces: map[int][]byte{}}

	// uTP(BEP-29)回退通道:与 DHT 共享 UDP 端口(同端口复用模式)。真实 swarm
	// 里大量 peer 只经 uTP 可达(TCP 被防火墙拦),纯 TCP 的元数据抓取实测
	// 359 个候选拨 77 个零成功——而下载会话靠 uTP 回退能连上其中几十个。
	var utpm *utpManager
	if dht != nil {
		utpm = dht.utpm
	}
	if utpm == nil {
		// DHT 未启用/无共享 socket:自建一个临时 uTP 通道(随机端口),
		// 抓取结束后关闭。没有它,磁力抓取在只开 uTP 的 swarm 上全军覆没。
		if m, err := newUTPManagerOwn(0); err == nil {
			utpm = m
			defer m.Close()
		}
	}

	peerCh := make(chan PeerAddr, 256)
	var srcWG sync.WaitGroup
	closeOnce := sync.Once{}
	closePeers := func() { closeOnce.Do(func() { close(peerCh) }) }

	for _, p := range directPeers {
		select {
		case peerCh <- p:
		default:
		}
	}
	for _, tr := range trackers {
		srcWG.Add(1)
		go func(u string) {
			defer srcWG.Done()
			req := AnnounceReq{
				InfoHash: ih, PeerID: pid, Port: uint16(announcePort),
				Event: EventStarted, NumWant: 100,
			}
			c2, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			resp, err := announceStandalone(c2, u, req, nc)

			if err != nil {
				return
			}
			for _, p := range resp.Peers {
				select {
				case peerCh <- p:
				default:
				}
			}
		}(tr)
	}
	if dht != nil {
		srcWG.Add(1)
		go func() {
			defer srcWG.Done()
			dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			dht.Lookup(dctx, ih, -1, func(p PeerAddr) {
				select {
				case peerCh <- p:
				default:
				}
			})
		}()
	}
	go func() {
		srcWG.Wait()
		closePeers()
	}()

	sem := make(chan struct{}, 16)
	var fwg sync.WaitGroup
	var tried, found atomic.Int64
	spawn := func(p PeerAddr) {
		tried.Add(1)
		fwg.Add(1)
		go func() {
			defer fwg.Done()
			defer func() { <-sem }()
			fetchMetaFromPeer(ctx, p, pid, ih, h, nc.Dialer, nc.SpoofName, utpm)
		}()
	}
	// 候选队列:并发占满时不丢弃 peer,排队等空位(tracker 应答几秒内涌入
	// 上百个候选,直接丢弃会把活 peer 扔掉——这正是元数据获取偶发失败的根源)
	queue := make([]PeerAddr, 0, 128)
	seen := make(map[string]struct{}, 256)
	ch := peerCh
	fail := func() (*MetaInfo, error) {
		if mi := h.result(); mi != nil {
			return mi, nil
		}
		log.Printf("元数据获取失败(infohash %x): tracker/DHT 共发现 %d 个候选 peer,已尝试拨号 %d 个,均未能完成元数据交换(swarm 无活 peer 或对端不支持)", ih, found.Load(), tried.Load())
		return nil, errors.New("未能从 peer 获取元数据(tracker/DHT 不可用或 swarm 已死)")
	}
	for {
		if mi := h.result(); mi != nil {
			return mi, nil
		}
		// 非阻塞收取新候选入队
		for ch != nil {
			select {
			case p, ok := <-ch:
				if !ok {
					ch = nil
					continue
				}
				key := p.String()
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					queue = append(queue, p)
					found.Add(1)
				}
				continue
			default:
			}
			break
		}
		// 有候选就领空位派发
		if len(queue) > 0 {
			select {
			case sem <- struct{}{}:
				p := queue[0]
				queue = queue[1:]
				spawn(p)
				continue
			default:
			}
		}
		// 源耗尽且队列清空:等在途拨号收尾后判定成败
		if ch == nil && len(queue) == 0 {
			done := make(chan struct{})
			go func() {
				fwg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}
			return fail()
		}
		// 源未耗尽(等 DHT 继续)或并发满:阻塞等任一事件
		select {
		case p, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			key := p.String()
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				queue = append(queue, p)
				found.Add(1)
			}
		case sem <- struct{}{}:
			if len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				spawn(p)
			} else {
				<-sem // 空位暂无候选,释放
			}
		case <-ctx.Done():
			return fail()
		}
	}
}

// parseTrackerURL 校验并解析 tracker 地址。
func parseTrackerURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("tracker 地址非法: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "udp":
	default:
		return nil, errTrackerScheme
	}
	return u, nil
}

// announceStandalone 独立的 announce 分发(不依赖 Client 会话)。
func announceStandalone(ctx context.Context, rawURL string, req AnnounceReq, nc NetConfig) (*AnnounceResp, error) {
	u, err := parseTrackerURL(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "udp":
		// 直连优先;失败且配置了中继时再经代理重试(独立超时预算)
		resp, err := UDPAnnounceConn(ctx, rawURL, req, nil, nc.Resolver)
		if err == nil || nc.UDPRelay == nil {
			return resp, err
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		return UDPAnnounceConn(rctx, rawURL, req, nc.UDPRelay, nc.Resolver)
	case "http", "https":
		hc := nc.HTTPClient
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Second}
		}
		return HTTPAnnounce(ctx, hc, rawURL, req, nc.SpoofName)
	default:
		return nil, errTrackerScheme
	}
}

// metaHolder 汇总多个 peer 的元数据分片。
type metaHolder struct {
	mu      sync.Mutex
	ih      [20]byte
	v2      *[32]byte // 非 nil 时按 BEP-52 SHA-256(info) 校验
	pieces  map[int][]byte
	total   int64
	result_ *MetaInfo
}

func (h *metaHolder) add(piece int, data []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pieces == nil {
		return false
	}
	if _, dup := h.pieces[piece]; dup {
		return false
	}
	h.pieces[piece] = data
	return true
}

func (h *metaHolder) setTotal(n int64) {
	h.mu.Lock()
	if h.total == 0 {
		h.total = n
	}
	h.mu.Unlock()
}

// done 报告元数据是否已齐(由持有分片的协程在收齐后调用 finish)。
func (h *metaHolder) complete(total int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total == 0 {
		h.total = total
	}
	n := int((h.total + metaPieceSize - 1) / metaPieceSize)
	for i := 0; i < n; i++ {
		if _, ok := h.pieces[i]; !ok {
			return false
		}
	}
	return true
}

func (h *metaHolder) result() *MetaInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result_
}

// assemble 校验并解析;成功后冻结 holder。
func (h *metaHolder) assemble() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result_ != nil {
		return
	}
	buf := make([]byte, 0, h.total)
	for i := 0; ; i++ {
		d, ok := h.pieces[i]
		if !ok {
			break
		}
		buf = append(buf, d...)
	}
	if !h.verify(buf) {
		// 分片被对端污染:全部丢弃重来(实际场景极罕见)
		h.pieces = map[int][]byte{}
		return
	}
	mi, err := parseFromInfo(buf)
	if err != nil {
		h.pieces = map[int][]byte{}
		return
	}
	h.result_ = mi
}

// verify 校验对端交付的完整 info dict。v1 比对 SHA-1;携带 btmh 时比对
// SHA-256——hybrid 磁链两者并存,任一匹配即接受(同一 info dict 的
// SHA-1 = btih、SHA-256 = btmh)。
func (h *metaHolder) verify(buf []byte) bool {
	if sha1.Sum(buf) == h.ih {
		return true
	}
	if h.v2 != nil {
		return sha256.Sum256(buf) == *h.v2
	}
	return false
}

func fetchMetaFromPeer(ctx context.Context, p PeerAddr, pid [20]byte, ih [20]byte, h *metaHolder, dialer func(ctx context.Context, network, addr string) (net.Conn, error), spoof string, utpm *utpManager) {
	tcpDial := func() (net.Conn, error) {
		if dialer != nil {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			conn, err := dialer(dctx, "tcp", p.String())
			cancel()
			return conn, err
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		return d.DialContext(ctx, "tcp", p.String())
	}
	// MSE 加密优先,失败明文回退(与 dialOut 同策略)。TCP 不通时回退 uTP:
	// 大量 peer(尤其国内/家宽)只开 uTP,TCP 一律被防火墙丢弃。
	conn, _, err := mseDialPrefer(tcpDial, ih)
	if err != nil && utpm != nil && p.IP != nil {
		ip4 := p.IP.To4()
		if ip4 != nil {
			udpDial := func() (net.Conn, error) {
				dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				defer cancel()
				c, derr := utpm.Dial(dctx, &net.UDPAddr{IP: ip4, Port: int(p.Port)})
				if derr == nil {
					c.SetDeadline(time.Now().Add(15 * time.Second))
				}
				return c, derr
			}
			conn, _, err = mseDialPrefer(udpDial, ih)
		}
	}
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := WriteHandshake(conn, Handshake{InfoHash: ih, PeerID: pid}, true, true, true); err != nil {
		return
	}
	got, err := ReadHandshake(conn)
	if err != nil || got.InfoHash != ih {
		return
	}
	if !got.SupportsExt() {
		return
	}
	if err := WriteMsg(conn, msgExtended, append([]byte{ExtHandshakeID}, BuildExtHandshake(0, 0, spoof)...)); err != nil {
		return
	}
	// 读对端消息直到拿到扩展握手(做种端通常先发 bitfield/unchoke)
	var e *ExtHandshake
	hardDeadline := time.Now().Add(25 * time.Second)
	for e == nil {
		if time.Now().After(hardDeadline) {
			return
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := ReadMsg(conn)
		if err != nil {
			return
		}
		if m.ID != msgExtended || len(m.Payload) == 0 || m.Payload[0] != ExtHandshakeID {
			continue
		}
		e, err = ParseExtHandshake(m.Payload[1:])
		if err != nil {
			return
		}
	}
	if !e.SupportsMetadata() || e.MetadataSize <= 0 || e.MetadataSize > maxMetadataSize {
		return
	}
	peerID := e.PeerExtID("ut_metadata")
	total := e.MetadataSize
	h.setTotal(total)
	n := int((total + metaPieceSize - 1) / metaPieceSize)

	pending := map[int]bool{}
	request := func(i int) {
		if err := WriteMsg(conn, msgExtended, append([]byte{peerID}, BuildMetadataMsg(metaReq, int64(i))...)); err != nil {
			return
		}
		pending[i] = true
	}
	for i := 0; i < n && i < 4; i++ {
		request(i)
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := ReadMsg(conn)
		if err != nil {
			return
		}
		if m.ID != msgExtended || len(m.Payload) == 0 {
			continue
		}
		switch m.Payload[0] {
		case peerID:
			msg, err := ParseMetadataMsg(m.Payload[1:])
			if err != nil || msg.Type != metaData {
				continue
			}
			if int64(msg.Piece) < 0 {
				continue
			}
			if h.add(int(msg.Piece), msg.Data) && h.complete(total) {
				h.assemble()
				if h.result() != nil {
					return
				}
			}
			delete(pending, int(msg.Piece))
			// 请求后续分片(保持少量在途)
			for i := 0; i < n; i++ {
				if !pending[i] {
					request(i)
					break
				}
			}
		}
		if h.result() != nil {
			return
		}
	}
}

const metaPieceSize = 1 << 14

// parseFromInfo 从原始 info dict 字节构造 MetaInfo(磁力路径)。
func parseFromInfo(raw []byte) (*MetaInfo, error) {
	wrapped := MustEncode(map[string]any{"info": Raw(raw)})
	mi, err := ParseTorrent(wrapped)
	if err != nil {
		return nil, fmt.Errorf("元数据解析失败: %w", err)
	}
	return mi, nil
}

var dbgT0 = time.Now()

func elapsed() float64 { return time.Since(dbgT0).Seconds() }
