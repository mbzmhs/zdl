package bt

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Hub 全局共享监听端口:所有 BT 会话共用一个 TCP 端口(与共享 DHT 的
// UDP 端口同号,BitComet/qBittorrent 等客户端的标准形态),入站连接按
// BT 握手中的 infohash 分发给注册的会话:
//   - 明文握手:读满 68 字节取 [28:48] 查表;
//   - MSE 加密握手:步3 的 req2⊕req3 摘要在全部注册会话的 infohash 里
//     匹配(skey 路由),匹配成功才完成密钥协商;
//   - uTP:优先复用共享 DHT socket 的 BEP-29 §8.1 同端口模式,无 DHT 时
//     自建 UDP,Accept 后与 TCP 同一套路由。
//
// 引擎在首个 BT 会话创建时拉起、最后一个会话结束时 Stop:无会话不占端
// 口,NAT 也无需映射。cfg.Hub 为 nil 的会话(测试、Hub 建立失败降级)仍
// 走每会话独立监听的旧路径。
type Hub struct {
	mu      sync.Mutex
	ln      net.Listener
	port    int
	utpm    *utpManager
	utpOwn  bool // true = 自建 UDP socket(Stop 时关闭);false = 借用 DHT 的
	clients map[[20]byte]map[*Client]bool // infohash → 该种子的全部活会话
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc
	once    sync.Once
}

// StartHub 绑定共享监听端口(base 被占则顺延探测 10 个,与 btAllocPort
// 同口径)。dht 非空且其 UDP 端口与 TCP 实际端口同号时,uTP 复用其
// socket;否则自建,UDP bind 失败不致命(TCP-only)。
func StartHub(base int, dht *DHTServer) (*Hub, error) {
	if base <= 0 {
		base = 6881
	}
	var ln net.Listener
	port := base
	for p := base; p < base+10; p++ {
		l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("端口 %d..%d 均被占用", base, base+9)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		ln:      ln,
		port:    port,
		clients: make(map[[20]byte]map[*Client]bool),
		baseCtx: ctx,
		cancel:  cancel,
	}
	if dht != nil && dht.Port() == port && dht.utpm != nil {
		h.utpm = dht.utpm // 借用:DHT 停止时 socket 一并消失,Hub 不再关它
	} else if m, err := newUTPManagerOwn(port); err == nil {
		m.shared = true // 会话侧(Stop 的 !shared 守卫)不得关闭 Hub 持有的 socket
		h.utpm, h.utpOwn = m, true
	}
	utpMode := "无 uTP"
	if h.utpm != nil {
		utpMode = "uTP 同端口"
		if h.utpOwn {
			utpMode = "uTP 自建 socket"
		}
	}
	log.Printf("[bt] 共享监听端口 %d(TCP+%s)", port, utpMode)
	h.wg.Add(2)
	go h.tcpLoop()
	go func() {
		defer h.wg.Done()
		if h.utpm != nil {
			h.utpLoop()
		}
	}()
	return h, nil
}

// Port 返回实际绑定的 TCP 监听端口(announce/DHT/NAT 统一口径)。
func (h *Hub) Port() int { return h.port }

// Add 注册会话(按 infohash 路由入站)。同一种子开多个任务是合法状态
// (不同保存目录/补种等):全部登记,入站按 pick 分流;任何会话退出都
// 不影响其余会话的路由。
func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	set := h.clients[c.meta.InfoHash]
	if set == nil {
		set = make(map[*Client]bool)
		h.clients[c.meta.InfoHash] = set
	}
	set[c] = true
	h.mu.Unlock()
}

// Remove 注销会话:仅从自己的 infohash 集合里摘除;同种子还有别的活会话
// 时路由自动落到它们身上(旧实现按 infohash 单槽登记,先到者退出即断掉
// 同种子其余会话的全部入站——BitComet 等对端表现为"连不上监听端口")。
func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	set := h.clients[c.meta.InfoHash]
	if set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.meta.InfoHash)
		}
	}
	h.mu.Unlock()
}

// pick 选一个该 infohash 的活会话接纳入站。优先仍在下载的(缺口更需要
// peer);同为做种态时任选——Go map 遍历序随机,天然在多个会话间分摊。
// 只在锁内收集候选,Completed 判定在锁外做(避免与客户端内部锁耦合)。
func (h *Hub) pick(ih [20]byte) *Client {
	h.mu.Lock()
	var all []*Client
	for c := range h.clients[ih] {
		all = append(all, c)
	}
	h.mu.Unlock()
	var any *Client
	for _, c := range all {
		if any == nil {
			any = c
		}
		if !c.Completed() {
			return c
		}
	}
	return any
}

// Stop 关闭监听与路由循环;自建的 uTP socket 一并关闭(借用的归 DHT)。
func (h *Hub) Stop() {
	h.once.Do(func() {
		h.cancel()
		h.ln.Close()
		if h.utpOwn && h.utpm != nil {
			h.utpm.Close()
		}
		log.Printf("[bt] 共享监听端口 %d 已关闭(无活动会话)", h.port)
	})
	h.wg.Wait()
}

func (h *Hub) tcpLoop() {
	defer h.wg.Done()
	// 入站连接的 route 协程由本循环自己的 routeWG 跟踪:退出前 drain。
	// 绝不能把它们挂进 h.wg——Stop 的 h.wg.Wait 与 Accept 返回后的
	// Add(+1) 并发是 Go 明令禁止的用法(panic: WaitGroup misuse),
	// 快速双暂停触发 hub.Stop 时恰有入站连接被 Accept 即崩(实测)。
	var routeWG sync.WaitGroup
	defer routeWG.Wait()
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		tuneConn(conn)
		routeWG.Add(1)
		go func() {
			defer routeWG.Done()
			h.route(conn, false)
		}()
	}
}

func (h *Hub) utpLoop() {
	var routeWG sync.WaitGroup
	defer routeWG.Wait()
	for {
		ctx, cancel := context.WithCancel(h.baseCtx)
		conn, err := h.utpm.Accept(ctx)
		cancel()
		if err != nil {
			return
		}
		routeWG.Add(1)
		go func(conn net.Conn) {
			defer routeWG.Done()
			h.route(conn, true)
		}(conn)
	}
}

// route 入站连接路由:嗅探→(明文读满握手 | MSE skey 匹配协商)→按
// infohash 交付会话,由会话侧补封禁校验、握手回写与 peer 注册。交付成功
// 后连接归会话所有;其余路径一律就地关闭。
func (h *Hub) route(conn net.Conn, utp bool) {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	pre, plain, err := sniffHandshake(conn)
	if err != nil {
		conn.Close()
		return
	}
	if plain {
		// 嗅探已预读 1..20 字节,补满整帧 68 字节后按 [28:48] 查表
		hsBuf := make([]byte, 68)
		copy(hsBuf, pre)
		if _, err := io.ReadFull(conn, hsBuf[len(pre):]); err != nil {
			conn.Close()
			return
		}
		var ih [20]byte
		copy(ih[:], hsBuf[28:48])
		c := h.pick(ih)
		if c == nil {
			conn.Close() // 非本端会话的 infohash:静默断(同旧握手不匹配口径)
			return
		}
		wire := &mseConn{Conn: conn, pre: hsBuf} // 预读帧回填进读缓冲
		hs, err := ReadHandshake(wire)
		if err != nil {
			wire.Close()
			return
		}
		c.deliverInbound(wire, hs, false, utp)
		return
	}
	// MSE:步3 由注册表路由 skey,协商完成后 IA 里的 BT 握手已可读
	wire, err := mseAcceptFn(conn, pre, h.matchSkey)
	if err != nil {
		conn.Close()
		return
	}
	hs, err := ReadHandshake(wire)
	if err != nil {
		wire.Close()
		return
	}
	c := h.pick(hs.InfoHash)
	if c == nil {
		wire.Close()
		return
	}
	c.deliverInbound(wire, hs, true, utp)
}

// matchSkey MSE skey 路由:在注册会话里找 SHA1("req2"‖ih)⊕SHA1("req3"‖S)
// 等于对端摘要的那个 infohash。会话数通常是个位数,逐个比对足够快。
func (h *Hub) matchSkey(S []byte, xorh *[20]byte) ([20]byte, bool) {
	b := mseSHA1(mseLabel["req3"], S)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ih, set := range h.clients {
		if len(set) == 0 {
			continue
		}
		a := mseSHA1(mseLabel["req2"], ih[:])
		var want [20]byte
		for i := range want {
			want[i] = a[i] ^ b[i]
		}
		if want == *xorh {
			return ih, true
		}
	}
	return [20]byte{}, false
}
