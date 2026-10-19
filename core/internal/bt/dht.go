package bt

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DHT(BEP-5):KRPC over UDP,共享于全部 BT 任务。
// 只实现 IPv4 节点(nodes/compact peer 6 字节),IPv6(nodes6)留待后续。

const (
	dhtK          = 16              // 路由桶容量 / 查询返回节点数(大桶养大表,对照 BitComet 数千节点)
	dhtAlpha      = 8               // 每轮并发查询数
	dhtMaxRounds  = 10              // 迭代查找最大轮数
	dhtQueryWait  = 3 * time.Second // 单个查询超时
	dhtStaleNode  = 15 * time.Minute
	dhtPeerExpire = 30 * time.Minute
	dhtMaxPeersIH = 256 // 单 infohash 记录的宣告 peer 上限

	dhtGoodNodes   = 128             // 入网健康目标:引导/自愈以表中至少这么多节点为准
	dhtThinNodes   = 16              // 低于此视为薄表,触发自愈重引导
	dhtMaxFails    = 3               // 节点连续失败上限,达到即移出路由表
	dhtHealthEvery = time.Minute     // 健康检查周期
	dhtHealQuiet   = 3 * time.Minute // 距最近一次出站应答超过此时长 = 失联
	dhtPersistMax  = 2560            // 路由表落盘节点上限(取最后可见最新的);= 表容量(160 桶 × dhtK 16),
	// 即全量落盘不再截断:外部回灌的大路由表(数千节点)重启后完整保留,
	// 死节点由 markFailed/陈旧度自然淘汰。2560 节点落盘文件约 260KB,无压力。

	DefaultDHTPort = 6881
)

// DHTBootstrapNodes 默认引导节点。
var DHTBootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.bitcomet.com:6881",
}

// DHTConfig DHT 节点参数。
type DHTConfig struct {
	Port      int      // UDP 监听端口;0 = 随机
	NodeID    [20]byte // 空 = 随机生成
	Bootstrap []string // 引导节点 host:port;空 = 内置公共节点
	// Network 监听地址族:"udp4"(默认)或 "udp6"(IPv6 DHT 独立实例)。
	Network string
	// Conn 预绑定的 UDP 通道(如 SOCKS5 UDP 中继);nil = 本机监听 Port。
	// 注入时 Port() 返回本机端口,对外实际端口由代理决定。
	Conn net.PacketConn
	// Resolver 自定义 DNS 解析(DNS 走代理的 DoH);nil = 系统 DNS。
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
	// PersistFile 路由表落盘路径;空 = 不持久化。重启回灌已知节点,
	// 引导域名被墙/污染时也能靠仍然活着的旧邻居冷启动。
	PersistFile string
}

func (cfg *DHTConfig) network() string {
	if cfg.Network == "udp6" {
		return "udp6"
	}
	return "udp4"
}

// DHTServer 是一个 DHT 参与节点:维护路由表,响应 KRPC 查询,
// 并为任务提供 get_peers 迭代查找与 announce_peer 宣告。
type DHTServer struct {
	cfg  DHTConfig
	conn net.PacketConn
	id   [20]byte
	rt   *routingTable

	// utpm 与 DHT 共用同一 UDP socket 的 uTP manager(BEP-29 §8.1 同端口
	// 复用;仅 v4 —— 客户端 uTP 拨号路径为 v4)。nil = v6 实例。
	utpm *utpManager

	txMu   sync.Mutex
	txNext uint32
	tx     map[string]chan dhtReply // 事务 ID → 应答通道

	ihMu   sync.Mutex
	annSeq map[[20]byte]*ihAnnounced // 他人宣告的 peer(供 get_peers 回应)

	secretMu sync.Mutex
	secret   []byte
	prev     []byte // 上一轮令牌密钥(轮换窗口内的宣告仍合法)
	secretAt time.Time

	lastGood atomic.Int64 // 最近一次收到出站应答的 unix 纳秒;失联判定依据
	saveOnc  sync.Once    // Stop 收尾落盘,防并发重复写

	wg      sync.WaitGroup
	stopCh  chan struct{}
	stopOnc sync.Once
}

type dhtReply struct {
	r dict  // y=r 时的 r 字典
	e []any // y=e 时的 e 列表
}

type dict = map[string]any

// ihAnnounced 记录某个 infohash 下他人宣告的 BT 监听端口。
type ihAnnounced struct {
	mu    sync.Mutex
	peers map[string]dhtPeerPort // "ip" → 记录(同一 IP 保留最新端口)
}

type dhtPeerPort struct {
	port uint16
	seen time.Time
}

// StartDHT 启动共享 DHT 节点。
func StartDHT(cfg DHTConfig) (*DHTServer, error) {
	if cfg.NodeID == ([20]byte{}) {
		// 持久化的本机 id 优先:重启沿用旧身份,其他节点表里的我们不失效
		if p := readDHTPersist(cfg.PersistFile); p != nil {
			if b, err := hex.DecodeString(p.Self); err == nil && len(b) == 20 {
				copy(cfg.NodeID[:], b)
			}
		}
	}
	if cfg.NodeID == ([20]byte{}) {
		rand.Read(cfg.NodeID[:])
	}
	if cfg.Port <= 0 {
		cfg.Port = 0 // 让内核分配
	}
	conn := cfg.Conn
	if conn == nil {
		uc, err := listenDHTUDP(cfg.network(), cfg.Port)
		if err != nil {
			return nil, fmt.Errorf("DHT 监听失败: %w", err)
		}
		uc.SetReadBuffer(2 << 20) // DHT 风暴期批量收包
		conn = uc
	}
	s := &DHTServer{
		cfg:    cfg,
		conn:   conn,
		id:     cfg.NodeID,
		rt:     newRoutingTable(cfg.NodeID),
		tx:     map[string]chan dhtReply{},
		annSeq: map[[20]byte]*ihAnnounced{},
		secret: randSecret(),
		stopCh: make(chan struct{}),
	}
	// v4 实例挂载同端口 uTP 复用:BT 监听端口 = DHT UDP 端口,
	// 入站 uTP SYN 只能从这个 socket 进来
	if cfg.network() != "udp6" {
		s.utpm = newUTPManagerShared(conn)
	}
	// 初始按"待验证"处理:持久化表可能整表是死节点,"表够数"不代表健康,
	// 引导与首轮自愈都以真正收到出站应答为准。
	s.lastGood.Store(time.Now().Add(-dhtHealQuiet - time.Second).UnixNano())
	s.loadRouting() // goroutine 启动前回灌,免并发
	s.wg.Add(2)
	go s.readLoop()
	go s.maintenanceLoop()
	// 引导入网完全异步:DNS 解析公网路由域名可能阻塞数秒,不得拖慢启动
	s.wg.Add(1)
	go s.bootstrapLoop()
	return s, nil
}

func randSecret() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}

// Port 返回实际监听的 UDP 端口。
func (s *DHTServer) Port() int {
	if s == nil || s.conn == nil {
		return 0
	}
	if a, ok := s.conn.LocalAddr().(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

// Routing 暴露路由表(测试与跨节点引导注入用)。
func (s *DHTServer) Routing() *routingTable { return s.rt }

// Insert 手动注入一个节点(测试与引导用)。
func (rt *routingTable) Insert(id [20]byte, addr *net.UDPAddr) { rt.insert(id, addr) }

// Stop 关闭节点。
func (s *DHTServer) Stop() {
	if s == nil {
		return
	}
	s.stopOnc.Do(func() {
		close(s.stopCh)
		s.conn.Close()
		if s.utpm != nil {
			s.utpm.Close() // 仅终结 uTP 连接(共享模式不动 socket)
		}
	})
	s.wg.Wait()
	s.saveOnc.Do(s.saveRouting) // 收尾落盘(与周期落盘互斥,重复 Stop 不重写)
}

func (s *DHTServer) stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// bootstrapAddrs 解析引导节点;ctx 超时约束 DNS 解析。
// 配置了 Resolver(DNS 走代理的 DoH)时优先使用:系统 DNS 可能被污染,
// 引导域名解析到假 IP 会导致 DHT 冷启动完全失败。
func (s *DHTServer) bootstrapAddrs(ctx context.Context) []*net.UDPAddr {
	list := s.cfg.Bootstrap
	if len(list) == 0 {
		list = DHTBootstrapNodes
	}
	var out []*net.UDPAddr
	for _, hp := range list {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		var ips []net.IP
		if s.cfg.Resolver != nil {
			ips, err = s.cfg.Resolver(ctx, host)
		} else {
			var addrs []net.IPAddr
			addrs, err = net.DefaultResolver.LookupIPAddr(ctx, host)
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		if err != nil || len(ips) == 0 {
			continue
		}
		pn, err := net.LookupPort("udp", port)
		if err != nil {
			continue
		}
		// 地址族匹配:udp6 实例只用 AAAA,udp4 只用 A
		wantV6 := s.cfg.network() == "udp6"
		var chosen net.IP
		for _, ip := range ips {
			if (ip.To4() == nil) == wantV6 {
				chosen = ip
				break
			}
		}
		if chosen == nil {
			continue
		}
		out = append(out, &net.UDPAddr{IP: chosen, Port: pn})
	}
	return out
}

// bootstrapLoop 引导入网:反复 ping 引导节点,直到表中节点数达到目标且
// 真正收到过出站应答——两者缺一不可:持久化回灌的死节点会让"表够数"说谎。
// 2 分钟未达标则交给维护循环继续养。出站查询在 Stop 后立即中止
// (避免 wg.Wait 被 DNS/超时拖住)。
func (s *DHTServer) bootstrapLoop() {
	defer s.wg.Done()
	deadline := time.Now().Add(2 * time.Minute)
	warned := false
	for time.Now().Before(deadline) {
		if s.rt.size() >= dhtGoodNodes &&
			time.Since(time.Unix(0, s.lastGood.Load())) < dhtHealQuiet {
			log.Printf("DHT 路由表已入网: %d 个节点 udp:%d", s.rt.size(), s.Port())
			return
		}
		if time.Since(dbgT0) > 100*time.Second && !warned && s.rt.size() == 0 {
			log.Printf("DHT 引导尚未成功(路由表 0 节点,udp:%d):引导服务器均无应答", s.Port())
			warned = true
		}
		s.pingBootstrap()
		if s.rt.size() > 0 {
			// 从现有邻居向外迭代扩散一层:仅靠固定引导域名的应答入表,
			// 增长封顶有限,到不了健康目标;随机目标一轮 find_node 会
			// 把表中近邻的邻居成批带回(每个应答最多 dhtK 个)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			var target [20]byte
			rand.Read(target[:])
			_, _ = s.iterateFind(ctx, target, nil)
			cancel()
		}
		select {
		case <-s.stopCh:
			return
		case <-time.After(5 * time.Second):
		}
	}
	if s.stopped() {
		return
	}
	if n := s.rt.size(); n == 0 {
		log.Printf("DHT 引导失败(0 节点):维护循环将持续重试")
	} else {
		log.Printf("DHT 引导到期: %d 个节点,交由维护循环继续养", n)
	}
}

// pingBootstrap 向引导节点发 find_node 一轮:应答者与返回的节点都入表
// (应答只送回事务通道而节点被丢弃的话,路由表永远为空)。
func (s *DHTServer) pingBootstrap() {
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	addrs := s.bootstrapAddrs(rctx)
	rcancel()
	for _, a := range addrs {
		if s.stopped() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.bootstrapQuery(ctx, a)
		cancel()
	}
}

// insertCompactNode 把 26 字节紧凑节点信息(20 id + 4 ip + 2 port)解析并入路由表。
func (s *DHTServer) insertCompactNode(nb [26]byte) {
	var id [20]byte
	copy(id[:], nb[:20])
	s.rt.insert(id, &net.UDPAddr{
		IP:   net.IP(nb[20:24]),
		Port: int(nb[24])<<8 | int(nb[25]),
	})
}

// replyNodes 按本实例地址族回节点表:udp4 用 "nodes"(26B 条目),
// udp6 用 "nodes6"(38B 条目,BEP-32)。
func (s *DHTServer) replyNodes(r dict, target [20]byte) {
	if s.cfg.network() == "udp6" {
		r["nodes6"] = s.rt.closestCompact(target, dhtK)
		return
	}
	r["nodes"] = s.rt.closestCompact(target, dhtK)
}

// insertCompactNode6 解析 38 字节紧凑节点(20 id + 16 ip + 2 port)。
func (s *DHTServer) insertCompactNode6(nb [38]byte) {
	var id [20]byte
	copy(id[:], nb[:20])
	s.rt.insert(id, &net.UDPAddr{
		IP:   net.IP(nb[20:36]),
		Port: int(nb[36])<<8 | int(nb[37]),
	})
}

// parseReplyNodes 解析应答里的节点表并入库:只认与本实例地址族匹配的字段。
func (s *DHTServer) parseReplyNodes(rep dhtReply) {
	if s.cfg.network() == "udp6" {
		if nodes, ok := rep.r["nodes6"].(string); ok {
			for i := 0; i+38 <= len(nodes); i += 38 {
				var nb [38]byte
				copy(nb[:], nodes[i:i+38])
				s.insertCompactNode6(nb)
			}
		}
		return
	}
	if nodes, ok := rep.r["nodes"].(string); ok {
		for i := 0; i+26 <= len(nodes); i += 26 {
			var nb [26]byte
			copy(nb[:], nodes[i:i+26])
			s.insertCompactNode(nb)
		}
	}
}

// bootstrapQuery 向引导节点发 find_node,把应答者与其返回的节点全部入表。
func (s *DHTServer) bootstrapQuery(ctx context.Context, addr *net.UDPAddr) {
	rep, err := s.queryNode(ctx, addr, "find_node", dict{"id": string(s.id[:]), "target": string(s.id[:])})
	if err != nil {
		return
	}
	if nid, ok := rep.r["id"].(string); ok && len(nid) == 20 {
		var id [20]byte
		copy(id[:], nid)
		s.rt.insert(id, addr)
	}
	s.parseReplyNodes(rep)
}

// maintenanceLoop 周期性维护:每分钟健康检查(薄表/失联 → 自愈重引导),
// 每 10 分钟随机目标刷新路由桶,每 5 分钟把路由表落盘。
func (s *DHTServer) maintenanceLoop() {
	defer s.wg.Done()
	tk := time.NewTicker(dhtHealthEvery)
	defer tk.Stop()
	var lastHeal time.Time
	for tick := 0; ; tick++ {
		select {
		case <-s.stopCh:
			return
		case <-tk.C:
		}
		// 自愈单轮可能耗时数十秒,兜 30s 冷却避免失联期间背靠背锤引导域名
		if s.needsHeal() && time.Since(lastHeal) >= 30*time.Second {
			lastHeal = time.Now()
			s.heal()
		}
		if tick%2 == 1 { // 每 2 分钟:随机目标巡游(近似桶刷新),持续养大路由表
			var target [20]byte
			rand.Read(target[:])
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, _ = s.iterateFind(ctx, target, nil)
			cancel()
		}
		if tick%5 == 4 { // 每 5 分钟落盘
			s.saveRouting()
		}
	}
}

// needsHeal 判断是否需要重新引导:节点太少(薄表),或太久没收到任何出站
// 应答——后者抓"表里全是死节点"的情形,光数节点会被僵尸节点骗过。
func (s *DHTServer) needsHeal() bool {
	if s.rt.size() < dhtThinNodes {
		return true
	}
	return time.Since(time.Unix(0, s.lastGood.Load())) > dhtHealQuiet
}

// heal 一轮自愈:先敲引导节点,再从现有邻居向外扩(任一路径通即可)。
func (s *DHTServer) heal() {
	s.pingBootstrap()
	if s.rt.size() > 0 {
		// 引导域名全灭时,活着的旧邻居仍能把表带起来
		var target [20]byte
		rand.Read(target[:])
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = s.iterateFind(ctx, target, nil)
		cancel()
	}
	if n := s.rt.size(); n >= dhtThinNodes &&
		time.Since(time.Unix(0, s.lastGood.Load())) < dhtHealQuiet {
		log.Printf("DHT 自愈完成: 路由表 %d 个节点 udp:%d", n, s.Port())
	}
}

func (s *DHTServer) readLoop() {
	defer s.wg.Done()
	buf := make([]byte, 8192)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok || n < 2 {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		// uTP 包交给同端口复用的 manager;其余按 KRPC 处理
		if s.utpm != nil && isUTPPacket(pkt) {
			s.utpm.Input(pkt, ua)
			continue
		}
		s.handlePacket(pkt, ua)
	}
}

// ---------- KRPC 收发 ----------

func (s *DHTServer) sendQuery(addr *net.UDPAddr, method string, args dict) (string, chan dhtReply) {
	s.txMu.Lock()
	s.txNext++
	tid := strconv.FormatUint(uint64(s.txNext), 36)
	ch := make(chan dhtReply, 1)
	s.tx[tid] = ch
	s.txMu.Unlock()

	msg := dict{"t": tid, "y": "q", "q": method, "a": args}
	s.conn.WriteTo(MustEncode(msg), addr)
	return tid, ch
}

func (s *DHTServer) queryNode(ctx context.Context, addr *net.UDPAddr, method string, args dict) (dhtReply, error) {
	_, ch := s.sendQuery(addr, method, args)
	select {
	case r := <-ch:
		if r.e != nil {
			return r, fmt.Errorf("KRPC 错误 %v", r.e)
		}
		return r, nil
	case <-ctx.Done():
		return dhtReply{}, ctx.Err()
	case <-time.After(dhtQueryWait):
		return dhtReply{}, errors.New("DHT 查询超时")
	case <-s.stopCh:
		return dhtReply{}, errors.New("DHT 已停止")
	}
}

func (s *DHTServer) handlePacket(pkt []byte, addr *net.UDPAddr) {
	v, err := Decode(pkt)
	if err != nil {
		return
	}
	m, ok := v.(dict)
	if !ok {
		return
	}
	t, _ := m["t"].(string)
	y, _ := m["y"].(string)
	switch y {
	case "q":
		s.handleQuery(t, m, addr)
	case "r", "e":
		// 收到出站应答即证明 DHT 出口畅通(哪怕事务已超时被丢弃),
		// 供 needsHeal 区分"表里有节点"和"表是活的"
		s.lastGood.Store(time.Now().UnixNano())
		if t == "" {
			return
		}
		s.txMu.Lock()
		ch, ok := s.tx[t]
		if ok {
			delete(s.tx, t)
		}
		s.txMu.Unlock()
		if !ok {
			return
		}
		rep := dhtReply{}
		if y == "r" {
			r, _ := m["r"].(dict)
			rep.r = r
		} else {
			e, _ := m["e"].([]any)
			rep.e = e
		}
		select {
		case ch <- rep:
		default:
		}
	}
}

func (s *DHTServer) handleQuery(t string, m dict, addr *net.UDPAddr) {
	q, _ := m["q"].(string)
	a, _ := m["a"].(dict)
	if a == nil || t == "" {
		return
	}
	var nid [20]byte
	if id, ok := a["id"].(string); ok && len(id) == 20 {
		copy(nid[:], id)
		s.rt.insert(nid, addr) // 有效查询者入表
	}
	r := dict{"id": string(s.id[:])}
	switch q {
	case "ping":
	case "find_node":
		var target [20]byte
		if tgt, ok := a["target"].(string); ok && len(tgt) == 20 {
			copy(target[:], tgt)
		}
		s.replyNodes(r, target)
	case "get_peers":
		var ih [20]byte
		if h, ok := a["info_hash"].(string); ok && len(h) == 20 {
			copy(ih[:], h)
		}
		r["token"] = string(s.tokenFor(addr))
		s.replyNodes(r, ih)
		if peers := s.knownPeers(ih); len(peers) > 0 {
			vals := make([]any, 0, len(peers))
			for _, p := range peers {
				vals = append(vals, string(p))
			}
			r["values"] = vals
		}
	case "announce_peer":
		var ih [20]byte
		if h, ok := a["info_hash"].(string); ok && len(h) == 20 {
			copy(ih[:], h)
		}
		if !s.tokenOK(addr, a["token"]) {
			s.replyError(t, addr, 203, "令牌无效")
			return
		}
		port := uint16(0)
		if implied, ok := a["implied_port"].(int64); ok && implied == 1 {
			port = uint16(addr.Port)
		} else if p, ok := a["port"].(int64); ok {
			port = uint16(p)
		}
		if port > 0 {
			s.recordAnnounced(ih, addr.IP.To4(), port)
		}
	default:
		s.replyError(t, addr, 204, "未知方法 "+q)
		return
	}
	s.conn.WriteTo(MustEncode(dict{"t": t, "y": "r", "r": r}), addr)
}

func (s *DHTServer) replyError(t string, addr *net.UDPAddr, code int, msg string) {
	s.conn.WriteTo(MustEncode(dict{
		"t": t, "y": "e",
		"e": []any{int64(code), msg},
	}), addr)
}

// ---------- 令牌(BEP-5 安全令牌,应答方职责) ----------

func (s *DHTServer) rotateSecret() {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if time.Since(s.secretAt) > 5*time.Minute {
		s.prev, s.secret = s.secret, randSecret()
		s.secretAt = time.Now()
	}
}

func (s *DHTServer) tokenFor(addr *net.UDPAddr) []byte {
	s.rotateSecret()
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	h := sha1.Sum(append(append([]byte{}, s.secret...), addr.IP.To4()...))
	return h[:8]
}

func (s *DHTServer) tokenOK(addr *net.UDPAddr, tok any) bool {
	ts, ok := tok.(string)
	if !ok {
		return false
	}
	s.rotateSecret()
	s.secretMu.Lock()
	cur, prev := s.secret, s.prev
	s.secretMu.Unlock()
	// 校验当前与上一轮令牌(轮换窗口内的宣告都合法)
	for _, sec := range [][]byte{cur, prev} {
		h := sha1.Sum(append(append([]byte{}, sec...), addr.IP.To4()...))
		if string(h[:8]) == ts {
			return true
		}
	}
	return false
}

// ---------- 宣告记录(应答 get_peers 的 values) ----------

func (s *DHTServer) recordAnnounced(ih [20]byte, ip net.IP, port uint16) {
	if ip == nil {
		return
	}
	key := ip.String()
	s.ihMu.Lock()
	g := s.annSeq[ih]
	if g == nil {
		g = &ihAnnounced{peers: map[string]dhtPeerPort{}}
		s.annSeq[ih] = g
	}
	s.ihMu.Unlock()
	g.mu.Lock()
	g.peers[key] = dhtPeerPort{port: port, seen: time.Now()}
	if len(g.peers) > dhtMaxPeersIH { // 简单过期:剔除最旧
		var oldest string
		var oldT time.Time
		for k, p := range g.peers {
			if oldT.IsZero() || p.seen.Before(oldT) {
				oldest, oldT = k, p.seen
			}
		}
		delete(g.peers, oldest)
	}
	g.mu.Unlock()
}

// knownPeers 返回该 infohash 下仍然新鲜的宣告 peer(compact 6 字节)。
func (s *DHTServer) knownPeers(ih [20]byte) [][]byte {
	s.ihMu.Lock()
	g := s.annSeq[ih]
	s.ihMu.Unlock()
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [][]byte
	for ip, p := range g.peers {
		if time.Since(p.seen) > dhtPeerExpire {
			delete(g.peers, ip)
			continue
		}
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		if ip4 := parsed.To4(); ip4 != nil {
			b := make([]byte, 6)
			copy(b, ip4)
			b[4], b[5] = byte(p.port>>8), byte(p.port)
			out = append(out, b)
		} else if ip16 := parsed.To16(); ip16 != nil {
			b := make([]byte, 18)
			copy(b, ip16)
			b[16], b[17] = byte(p.port>>8), byte(p.port)
			out = append(out, b)
		}
	}
	return out
}

// ---------- 路由表 ----------

// routingTable 是按 XOR 前缀长度分桶的简化路由表(160 桶 × K)。
// 采用"满桶驱逐陈旧节点"策略,不做逐节点 ping 交叉确认,足以工作。
type routingTable struct {
	mu      sync.Mutex
	self    [20]byte
	buckets [160][]*dhtNode
}

// Count 返回路由表中的节点总数(诊断用)。
func (rt *routingTable) Count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, b := range rt.buckets {
		n += len(b)
	}
	return n
}

// NodeCount 返回路由表节点数(诊断用)。
func (s *DHTServer) NodeCount() int { return s.rt.Count() }

type dhtNode struct {
	id       [20]byte
	addr     *net.UDPAddr
	lastSeen time.Time
	fails    int
}

func newRoutingTable(self [20]byte) *routingTable { return &routingTable{self: self} }

func bucketIndex(self, id [20]byte) int {
	d := xorDist(self, id)
	for i := 0; i < 160; i++ {
		if d[i/8]&(1<<(7-uint(i%8))) != 0 {
			return 159 - i
		}
	}
	return 0
}

func xorDist(a, b [20]byte) [20]byte {
	var out [20]byte
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func (rt *routingTable) size() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, b := range rt.buckets {
		n += len(b)
	}
	return n
}

func (rt *routingTable) insert(id [20]byte, addr *net.UDPAddr) {
	rt.insertSeen(id, addr, time.Now())
}

// insertSeen 按指定"最后可见时间"入表。持久化回灌用原始时间,
// 满桶驱逐的陈旧度判断才继续有效(重启后不把昨天的节点当新鲜的)。
func (rt *routingTable) insertSeen(id [20]byte, addr *net.UDPAddr, seen time.Time) {
	if id == rt.self || id == ([20]byte{}) || addr.IP.To4() == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	idx := bucketIndex(rt.self, id)
	b := rt.buckets[idx]
	for _, n := range b {
		if n.id == id {
			n.addr = addr
			n.lastSeen = seen
			n.fails = 0
			return
		}
	}
	if len(b) < dhtK {
		rt.buckets[idx] = append(b, &dhtNode{id: id, addr: addr, lastSeen: seen})
		return
	}
	// 满桶:驱逐最陈旧/失败最多的节点,否则丢弃新节点
	worst, wi := -1, -1
	for i, n := range b {
		if n.fails > 0 || time.Since(n.lastSeen) > dhtStaleNode {
			score := int(time.Since(n.lastSeen)) + n.fails*1<<30
			if worst < score {
				worst, wi = score, i
			}
		}
	}
	if wi >= 0 {
		b[wi] = &dhtNode{id: id, addr: addr, lastSeen: seen}
	}
}

func (rt *routingTable) markFailed(id [20]byte) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	idx := bucketIndex(rt.self, id)
	b := rt.buckets[idx]
	for i, n := range b {
		if n.id == id {
			n.fails++
			if n.fails >= dhtMaxFails {
				// 连续失败达上限:移出路由表。否则死节点永远占着桶位,
				// 迭代查找每轮先问它们、两轮无收益即弃,表再也长不大
				nb := make([]*dhtNode, 0, len(b)-1)
				nb = append(nb, b[:i]...)
				nb = append(nb, b[i+1:]...)
				rt.buckets[idx] = nb
			}
			return
		}
	}
}

// closest 返回距 target 最近的至多 n 个节点。
func (rt *routingTable) closest(target [20]byte, n int) []*dhtNode {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	all := make([]*dhtNode, 0, 128)
	for _, b := range rt.buckets {
		all = append(all, b...)
	}
	// 插入排序选取 top-n(全表 ≤1280 节点,n≤16,全排序亦不贵)
	sortNodesByDist(all, target)
	if len(all) > n {
		all = all[:n]
	}
	return all
}

func sortNodesByDist(nodes []*dhtNode, target [20]byte) {
	var dists [][20]byte
	for _, n := range nodes {
		dists = append(dists, xorDist(n.id, target))
	}
	// 简单插入排序(节点数有限)
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && lessDist(dists[j], dists[j-1]); j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
			dists[j], dists[j-1] = dists[j-1], dists[j]
		}
	}
}

func lessDist(a, b [20]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// closestCompact 生成紧凑节点信息串:v4 节点 26 字节(20 id + 4 ip + 2 port),
// v6 节点 38 字节(20 id + 16 ip + 2 port)。表内节点实际单族(套接字族决定)。
func (rt *routingTable) closestCompact(target [20]byte, n int) string {
	var buf []byte
	for _, nd := range rt.closest(target, n) {
		buf = append(buf, nd.id[:]...)
		if ip4 := nd.addr.IP.To4(); ip4 != nil {
			buf = append(buf, ip4...)
		} else if ip16 := nd.addr.IP.To16(); ip16 != nil {
			buf = append(buf, ip16...)
		} else {
			continue
		}
		buf = append(buf, byte(nd.addr.Port>>8), byte(nd.addr.Port))
	}
	return string(buf)
}

// ---------- 路由表持久化 ----------

// dhtPersist 是 dht.json 的结构:本机 id + 已知节点(按最后可见时间新→旧)。
type dhtPersist struct {
	Self  string         `json:"self"`  // 本机节点 id(40 hex)
	Nodes []dhtSavedNode `json:"nodes"` // 已知节点
}

type dhtSavedNode struct {
	ID   string `json:"id"`   // 节点 id(40 hex)
	IP   string `json:"ip"`   // IPv4
	Port int    `json:"port"` // UDP 端口
	Seen int64  `json:"seen"` // 最后可见(unix 秒)
}

// readDHTPersist 读取持久化文件;不存在返回 nil,损坏记日志后返回 nil。
func readDHTPersist(path string) *dhtPersist {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("DHT 路由表文件读取失败,忽略: %v", err)
		}
		return nil
	}
	var p dhtPersist
	if err := json.Unmarshal(b, &p); err != nil {
		log.Printf("DHT 路由表文件损坏,忽略: %v", err)
		return nil
	}
	return &p
}

// loadRouting 回灌上次落盘的节点。死节点由健康检查与 markFailed 淘汰,
// 活着的则省去从引导域名冷启动的开销(引导域名被墙时全靠它们)。
func (s *DHTServer) loadRouting() {
	p := readDHTPersist(s.cfg.PersistFile)
	if p == nil {
		return
	}
	n := 0
	for _, sn := range p.Nodes {
		idb, err := hex.DecodeString(sn.ID)
		if err != nil || len(idb) != 20 || sn.Port <= 0 {
			continue
		}
		ip := net.ParseIP(sn.IP)
		if ip == nil {
			continue
		}
		var id [20]byte
		copy(id[:], idb)
		s.rt.insertSeen(id, &net.UDPAddr{IP: ip, Port: sn.Port}, time.Unix(sn.Seen, 0))
		n++
	}
	if n > 0 {
		log.Printf("DHT 回灌持久化节点: %d 个", n)
	}
}

// snapshotLive 返回可落盘节点(查询失败中的不存),按最后可见时间取最新 n 个。
func (rt *routingTable) snapshotLive(n int) []dhtNode {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var all []dhtNode
	for _, b := range rt.buckets {
		for _, nd := range b {
			if nd.fails == 0 {
				all = append(all, *nd)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lastSeen.After(all[j].lastSeen) })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// saveRouting 落盘路由表(临时文件 + rename,防写一半被下次启动读到)。
// Stop 收尾必落一次,运行中每 5 分钟落一次——崩溃也能带走大部分已知节点。
func (s *DHTServer) saveRouting() {
	if s.cfg.PersistFile == "" {
		return
	}
	p := dhtPersist{Self: hex.EncodeToString(s.id[:])}
	for _, nd := range s.rt.snapshotLive(dhtPersistMax) {
		ip4 := nd.addr.IP.To4()
		if ip4 == nil {
			continue
		}
		p.Nodes = append(p.Nodes, dhtSavedNode{
			ID:   hex.EncodeToString(nd.id[:]),
			IP:   ip4.String(),
			Port: nd.addr.Port,
			Seen: nd.lastSeen.Unix(),
		})
	}
	b, err := json.Marshal(&p)
	if err != nil {
		return
	}
	tmp := s.cfg.PersistFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		err = os.Rename(tmp, s.cfg.PersistFile)
	}
	if err != nil {
		log.Printf("DHT 路由表落盘失败: %v", err)
	}
}

// ---------- 迭代查找(get_peers) ----------

// Lookup 对 infohash 执行迭代 get_peers 查找。
// 发现的 peer 逐个回调 cb;announcePort >= 0 时向返回令牌的最近节点宣告。
// 返回发现的 peer 数(尽力而为)。
func (s *DHTServer) Lookup(ctx context.Context, ih [20]byte, announcePort int, cb func(PeerAddr)) int {
	found, holders := s.iterateFind(ctx, ih, cb)
	if announcePort >= 0 && ctx.Err() == nil {
		s.announceTo(ctx, ih, uint16(announcePort), holders)
	}
	return found
}

type dhtCandidate struct {
	id    [20]byte
	addr  *net.UDPAddr
	token string
}

// iterateFind 迭代查找核心:每轮向未查询的最近 alpha 候选并发 get_peers,
// 收获 peer/values 回调并收集令牌;连续两轮无新候选即收敛。
// 返回发现的 peer 数与带令牌的最近候选(供 announce_peer)。
func (s *DHTServer) iterateFind(ctx context.Context, target [20]byte, cb func(PeerAddr)) (int, []dhtCandidate) {
	var closest []dhtCandidate
	queried := map[string]bool{}
	found := 0
	addCandidate := func(id [20]byte, addr *net.UDPAddr) {
		if addr == nil || addr.IP.To4() == nil || queried[addr.String()] {
			return
		}
		for _, c := range closest {
			if c.id == id {
				return
			}
		}
		closest = append(closest, dhtCandidate{id: id, addr: addr})
		sortNodesByDistPtr(closest, target)
		if len(closest) > 32 {
			closest = closest[:32]
		}
	}

	roundsNoGain := 0
	var holders []dhtCandidate
	// 起始候选:路由表中距目标最近的节点
	for _, n := range s.rt.closest(target, dhtAlpha*2) {
		addCandidate(n.id, n.addr)
	}
	for round := 0; round < dhtMaxRounds && roundsNoGain < 2; round++ {
		if ctx.Err() != nil {
			break
		}
		// 本轮目标:未查询的最近 alpha 个(主协程预标记,杜绝并发写 map)
		var batch []*dhtCandidate
		for i := range closest {
			if !queried[closest[i].addr.String()] {
				queried[closest[i].addr.String()] = true
				batch = append(batch, &closest[i])
				if len(batch) >= dhtAlpha {
					break
				}
			}
		}
		if len(batch) == 0 {
			break
		}
		before := len(closest)
		type qres struct {
			idx    int
			id     [20]byte
			token  string
			peers  []PeerAddr
			nodes  [][26]byte
			failed bool
		}
		results := make(chan qres, len(batch))
		for bi, cand := range batch {
			go func(bi int, c *dhtCandidate) {
				res := qres{idx: bi}
				qctx, cancel := context.WithTimeout(ctx, dhtQueryWait)
				defer cancel()
				rep, err := s.queryNode(qctx, c.addr, "get_peers", dict{
					"id": string(s.id[:]), "info_hash": string(target[:]),
				})
				if err != nil {
					res.failed = true
					results <- res
					return
				}
				if id, ok := rep.r["id"].(string); ok && len(id) == 20 {
					copy(res.id[:], id)
				}
				res.token, _ = rep.r["token"].(string)
				if vals, ok := rep.r["values"].([]any); ok {
					for _, v := range vals {
						sv, ok := v.(string)
						if !ok {
							continue
						}
						if s.cfg.network() == "udp6" && len(sv) == 18 {
							res.peers = append(res.peers, decodeCompactPeers6(sv)...)
						} else if len(sv) == 6 {
							res.peers = append(res.peers, decodeCompactPeers(sv)...)
						}
					}
				}
				if nodes, ok := rep.r["nodes"].(string); ok && s.cfg.network() == "udp4" {
					for i := 0; i+26 <= len(nodes); i += 26 {
						var nb [26]byte
						copy(nb[:], nodes[i:i+26])
						res.nodes = append(res.nodes, nb)
					}
				}
				if nodes6, ok := rep.r["nodes6"].(string); ok && s.cfg.network() == "udp6" {
					for i := 0; i+38 <= len(nodes6); i += 38 {
						var nb [38]byte
						copy(nb[:], nodes6[i:i+38])
						// 入表即可:lookup 候选表只承载地址
						s.insertCompactNode6(nb)
					}
				}
				results <- res
			}(bi, cand)
		}
		for range batch {
			res := <-results
			c := batch[res.idx]
			if res.failed {
				// Stop 中止的查询不算节点失败,避免收尾时把活节点标脏
				if c.id != ([20]byte{}) && !s.stopped() {
					s.rt.markFailed(c.id)
				}
				continue
			}
			if res.id != ([20]byte{}) {
				c.id = res.id
				s.rt.insert(res.id, c.addr)
			}
			c.token = res.token
			if c.token != "" {
				holders = append(holders, *c)
			}
			for _, p := range res.peers {
				found++
				if cb != nil {
					cb(p)
				}
			}
			for _, nb := range res.nodes {
				// 返回节点同样入路由表(而不只进本次查找的本地候选列表):
				// 否则冷启动只有个位数节点时,路由表仅靠直接应答者缓慢增长,
				// 每次查找都从极小表重新探路,DHT 永远雪崩不起来
				s.insertCompactNode(nb)
				addCandidate([20]byte(nb[0:20]), &net.UDPAddr{
					IP:   net.IPv4(nb[20], nb[21], nb[22], nb[23]),
					Port: int(uint16(nb[24])<<8 | uint16(nb[25])),
				})
			}
		}
		if len(closest) == before {
			roundsNoGain++
		} else {
			roundsNoGain = 0
		}
	}
	sortNodesByDistPtr(holders, target)
	if len(holders) > dhtK {
		holders = holders[:dhtK]
	}
	return found, holders
}

func sortNodesByDistPtr(nodes []dhtCandidate, target [20]byte) {
	var dists [][20]byte
	for _, n := range nodes {
		dists = append(dists, xorDist(n.id, target))
	}
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && lessDist(dists[j], dists[j-1]); j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
			dists[j], dists[j-1] = dists[j-1], dists[j]
		}
	}
}

// announceTo 向查找期间返回过令牌的最近节点发送 announce_peer。
func (s *DHTServer) announceTo(ctx context.Context, ih [20]byte, port uint16, holders []dhtCandidate) {
	var wg sync.WaitGroup
	for _, h := range holders {
		wg.Add(1)
		go func(c dhtCandidate) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, dhtQueryWait)
			defer cancel()
			s.queryNode(actx, c.addr, "announce_peer", dict{
				"id":        string(s.id[:]),
				"info_hash": string(ih[:]),
				"port":      int64(port),
				"token":     c.token,
			})
		}(h)
	}
	wg.Wait()
}
