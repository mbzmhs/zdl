package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zdownloader/internal/bt"
	"zdownloader/internal/conf"
	"zdownloader/internal/limiter"
	"zdownloader/internal/proxy"
	"zdownloader/internal/rss"
	"zdownloader/internal/util"
)

var ErrNotFound = errors.New("任务不存在")

// reqSpec 是一次任务运行期间不可变的请求参数,worker 使用无需加锁。
type reqSpec struct {
	url     string
	ua      string
	referer string
	headers map[string]string
	ifRange string // 续传一致性校验:ETag 优先,其次 Last-Modified
}

type Manager struct {
	cfg      conf.Config
	cfgPath  string
	tasksDir string

	mu      sync.Mutex
	tasks   map[string]*Task
	order   []string
	running int

	// taskName:已确定最终文件名的任务(taskID -> filename,仅非空)。与 m.mu 同锁,
	// 作为"同目录文件名唯一"的判重依据:既感知在途兄弟任务(它们尚未落盘),
	// 配合磁盘检查兜底已落定文件,从而避免多个任务写同一个 .zdl.part 互相覆盖。
	// 不变式:taskName 的键是 tasks 的子集——创建/载入时登记,删除时移除。
	taskName map[string]string

	wake     chan struct{}
	version  atomic.Uint64
	quitting atomic.Bool // Shutdown 进行中:ctx 取消的任务按"等待续传"落盘而非暂停

	// OnNotify 系统通知回调(桌面层注入 tray.Notify;RSS 自动下载完成提醒)。
	// 可从任意 goroutine 调用;nil = 无桌面层(测试/无界面)时静默跳过。
	OnNotify func(title, text string)
	// OnSeedConflict 种子自动打开冲突提醒:HTTP 下载的 .torrent 完成后,若列表里
	// 已存在同 InfoHash 的 BT 任务(任意状态),不再静默跳过,而是经此回调交由前端
	// 弹确认框让用户自行决定是否仍要创建。桌面层注入(转发到前端"待办槽");
	// 可从任意 goroutine 调用;nil = 无桌面层时静默跳过。
	OnSeedConflict func(name, torrentPath, dir string)
	// OnChoosePrompt 任务进入 choosing(磁力元数据异步就绪需选文件,或单文件关联
	// 启动需先选保存目录)时回调:这是异步状态迁移,聚合器只广播快照而不唤窗,窗口
	// 自托盘隐藏时 WebView2 挂起、前端 maybePromptChoose 自身弹不出窗,须由服务端
	// 主动唤窗+强制唤醒渲染进程。桌面层注入(转发 ShowUI(false));可从任意 goroutine
	// 调用;nil = 无桌面层(测试/无界面)时静默跳过。
	OnChoosePrompt func(id string)
	// notifySeen RSS 完成提醒的状态基线(id -> "notified"/当前状态,notifyScan 维护)
	notifySeen map[string]string
	// notifyBaseline 首次扫描基线已建立(false = 启动后还没扫过)
	notifyBaseline bool
	notifyMu       sync.Mutex // notifyScan 串行化(ticker 与测试可并发调用)

	gLim         *limiter.Limiter // 全局下载限速(HTTP 与 BT 全部任务共享)
	btULim       *limiter.Limiter // BT 全局上传限速(全部 BT 会话共享)
	transportsMu sync.Mutex
	transports   map[string]*http.Transport

	rootCtx context.Context
	wg      sync.WaitGroup // 进行中的 runTask 计数,退出时等待
	seedWG  sync.WaitGroup // BT 做种协程(不占调度槽位)

	dhtMu     sync.Mutex
	dhtSrv    *bt.DHTServer // 共享 DHT 节点(cfg.BT.DHT 开启时存在)
	dhtSrv6   *bt.DHTServer // IPv6 DHT 独立实例(BEP-32;无 v6 网络时为 nil)
	dhtFailed string        // DHT 启动失败原因(日志/UI 提示用)

	udpRelayMu sync.Mutex
	udpRelay   net.PacketConn // SOCKS5 UDP 中继(udp tracker 与 DHT 共用,懒创建)

	lsdMu sync.Mutex
	lsd   *bt.LSD // 局域网发现(BEP-14);端口被占时为 nil

	upnpMu    sync.Mutex
	upnp      string        // NAT 穿透方法说明("nat-pmp@网关" / "upnp"),展示用
	extIP     string        // 穿透成功后的网关公网 IP,展示用
	upnpStop  chan struct{} // NAT 映射循环退出信号
	btPorts   map[int]bool  // 实际在用的 BT 监听端口(运行中的客户端会话)
	btHub     *bt.Hub       // 全局共享监听端口(全部会话同端口,按 infohash 路由)
	btHubRefs int           // 接入共享端口的会话引用数;归零即停 Hub、撤端口

	banList *bt.BanList // 全局封禁名单(全部 BT 会话共用,持久化 bans.json)

	rssStore *rss.Store // RSS 订阅(文件夹/订阅/条目/已读,持久化 rss.json)
	rssSt    rssState   // 抓取运行态(进行中的订阅集合)
	rssGate  atomic.Bool
	rssGrace time.Time // "启动时立即更新"关闭时的静默期截止;零值 = 不静默
}

func NewManager(cfg conf.Config, cfgPath, tasksDir string) *Manager {
	m := &Manager{
		cfg:        cfg,
		cfgPath:    cfgPath,
		tasksDir:   tasksDir,
		tasks:      map[string]*Task{},
		taskName:   map[string]string{},
		wake:       make(chan struct{}, 1),
		gLim:       limiter.New(cfg.GlobalLimit),
		btULim:     limiter.New(cfg.BT.UPSpeed),
		transports: map[string]*http.Transport{},
		// 封禁名单与 config.json 同目录(bans.json),重启后继续生效
		banList: bt.NewBanList(filepath.Join(filepath.Dir(cfgPath), "bans.json")),
	}
	m.rssInit(cfgPath) // 订阅存储与 config.json 同目录(rss.json)
	return m
}

// Run 载入历史任务并启动调度器、速度统计与周期持久化。
func (m *Manager) Run(ctx context.Context) {
	m.rootCtx = ctx
	_ = os.MkdirAll(m.tasksDir, 0o755)
	m.loadTasks()
	m.cleanupOrphanTemps() // 此时调度器未启动,无下载在写临时文件
	m.startDHT()
	m.startLSD()
	m.natStart()
	m.bump()
	go m.scheduler(ctx)
	go m.ticker(ctx)
	go m.rssLoop(ctx)
	m.kick() // 唤醒调度器:重启前排队的任务(状态已是 waiting)需要立即续传
}

// ---------- 局域网发现(BEP-14) ----------

// startLSD 启动局域网发现并挂接分发回调(幂等)。
func (m *Manager) startLSD() {
	m.lsdMu.Lock()
	defer m.lsdMu.Unlock()
	if m.lsd != nil {
		return
	}
	l, err := bt.StartLSD(m.cfg.BT.Port, func(ih [20]byte, addr string) {
		m.dispatchLSDPeer(ih, addr)
	})
	if err != nil {
		log.Printf("LSD 不可用(不影响 BT): %v", err)
		return
	}
	l.Announce()
	m.lsd = l
}

// stopLSD 停止局域网发现(幂等)。
func (m *Manager) stopLSD() {
	m.lsdMu.Lock()
	l := m.lsd
	m.lsd = nil
	m.lsdMu.Unlock()
	l.Stop()
}

// dispatchLSDPeer 把局域网广播发现的 peer 交给对应任务的会话。
func (m *Manager) dispatchLSDPeer(ih [20]byte, addr string) {
	key := fmt.Sprintf("%x", ih)
	for _, t := range m.snapshotTasks() {
		t.mu.Lock()
		kind, infoHash, cl := t.Kind, t.InfoHash, t.btcl
		t.mu.Unlock()
		if kind != KindBT || infoHash != key || cl == nil {
			continue
		}
		if err := cl.AddPeerAddr(addr); err == nil {
			log.Printf("LSD 发现 peer %s(%s)", addr, shortInfoHash(key))
		}
	}
}

// lsdAnnounceTask 会话启动后注册并立即广播一次。
func (m *Manager) lsdAnnounceTask(t *Task) {
	m.lsdMu.Lock()
	l := m.lsd
	m.lsdMu.Unlock()
	if l == nil {
		return
	}
	t.mu.Lock()
	ih, ok := parseInfoHashHexStr(t.InfoHash)
	t.mu.Unlock()
	if !ok {
		return
	}
	l.Register(ih)
	l.Announce()
}

func (m *Manager) lsdUnregisterTask(t *Task) {
	m.lsdMu.Lock()
	l := m.lsd
	m.lsdMu.Unlock()
	if l == nil {
		return
	}
	t.mu.Lock()
	ih, ok := parseInfoHashHexStr(t.InfoHash)
	t.mu.Unlock()
	if ok {
		l.Unregister(ih)
	}
}

func shortInfoHash(h string) string {
	if len(h) > 8 {
		return h[:8] + "…"
	}
	return h
}

func parseInfoHashHexStr(s string) ([20]byte, bool) {
	var ih [20]byte
	if len(s) != 40 {
		return ih, false
	}
	// bt 包提供解析:复用 magnet 的十六进制路径
	mg, err := bt.ParseMagnet("magnet:?xt=urn:btih:" + s)
	if err != nil {
		return ih, false
	}
	return mg.InfoHash, true
}

// NATStatus NAT 穿透状态(展示用):method 为 nil 表示未穿透。
func (m *Manager) NATStatus() map[string]any {
	m.upnpMu.Lock()
	defer m.upnpMu.Unlock()
	method := any(nil)
	if m.upnp != "" {
		method = m.upnp
	}
	return map[string]any{"method": method, "externalIP": m.extIP}
}

// ---------- NAT 穿透(NAT-PMP 优先,UPnP 回退;按实际监听端口动态映射) ----------

// btPortAcquire 返回共享监听端口与其 Hub(全部 BT 会话同端口,BitComet
// 等客户端的标准形态)。首个会话拉起 Hub(顺延探测 10 个端口),最后一个
// 会话结束时撤销;NAT 按 btPorts(始终至多一项)对账映射。失败返回
// (0, nil):会话回退独立随机端口,仅无入站能力。
func (m *Manager) btPortAcquire() (int, *bt.Hub) {
	m.upnpMu.Lock()
	defer m.upnpMu.Unlock()
	if m.btHub == nil {
		hub, err := bt.StartHub(m.cfg.BT.Port, m.dhtServer())
		if err != nil {
			log.Printf("共享 BT 监听端口创建失败,回退每会话独立端口: %v", err)
			return 0, nil
		}
		m.btHub = hub
		m.btPorts = map[int]bool{hub.Port(): true}
	}
	m.btHubRefs++
	return m.btHub.Port(), m.btHub
}

// btPortRelease BT 会话结束后注销引用(最后一个会话退出即停 Hub、撤销
// 监听,NAT 随之撤销映射)。
func (m *Manager) btPortRelease(port int) {
	if port <= 0 {
		return
	}
	m.upnpMu.Lock()
	defer m.upnpMu.Unlock()
	if m.btHubRefs > 0 {
		m.btHubRefs--
	}
	if m.btHubRefs == 0 && m.btHub != nil {
		m.btHub.Stop()
		m.btHub = nil
		m.btPorts = map[int]bool{}
	}
}

// btHubStopAll 退出兜底:任务取消超时等异常路径可能残留 Hub,关停(幂等)。
func (m *Manager) btHubStopAll() {
	m.upnpMu.Lock()
	hub := m.btHub
	m.btHub = nil
	m.btPorts = map[int]bool{}
	m.btHubRefs = 0
	m.upnpMu.Unlock()
	if hub != nil {
		hub.Stop()
	}
}

// dhtServer 读取共享 DHT 实例(Hub 复用其 UDP socket 时的入参)。
func (m *Manager) dhtServer() *bt.DHTServer {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	return m.dhtSrv
}

func (m *Manager) btPortsSnapshot() map[int]bool {
	m.upnpMu.Lock()
	defer m.upnpMu.Unlock()
	out := make(map[int]bool, len(m.btPorts))
	for p := range m.btPorts {
		out[p] = true
	}
	return out
}

// natStart 启动 NAT 映射对账循环(异步,不阻塞启动;幂等)。
func (m *Manager) natStart() {
	m.upnpMu.Lock()
	defer m.upnpMu.Unlock()
	if m.upnpStop != nil || m.rootCtx == nil {
		return
	}
	stop := make(chan struct{})
	m.upnpStop = stop
	go m.natLoop(stop)
}

// natLoop 有 BT 会话在跑时确保穿透通道(NAT-PMP 探测 ~2s,不通再 UPnP
// SSDP 发现),按"在用端口集合"对账:新端口建映射、停用端口撤销映射,
// 并周期性重申租约(UPnP 1h / NAT-PMP 2h)。探测失败退避 2 分钟。
func (m *Manager) natLoop(stop chan struct{}) {
	var gw *bt.UPnP
	var pmpGW net.IP
	method := ""
	mapped := map[int]bool{}
	cycles := 0
	backoff := time.Time{}
	for {
		ports := m.btPortsSnapshot()
		if method == "" && len(ports) > 0 && !time.Now().Before(backoff) {
			// ① NAT-PMP 快探
			gwIP, extIP, err := bt.NATPMPProbe()
			if err == nil {
				pmpGW = gwIP
				method = fmt.Sprintf("nat-pmp@%s", gwIP)
				m.upnpMu.Lock()
				m.upnp = method
				m.extIP = extIP
				m.upnpMu.Unlock()
				log.Printf("NAT-PMP 通道就绪: 网关 %s,外部 IP %s", gwIP, extIP)
			} else {
				// ② UPnP 回退
				ctx, cancel := context.WithTimeout(m.rootCtx, 40*time.Second)
				g, uerr := bt.DiscoverUPnP(ctx)
				cancel()
				if uerr == nil {
					gw = g
					method = "upnp"
					m.upnpMu.Lock()
					m.upnp = method
					m.extIP = g.ExternalIP()
					m.upnpMu.Unlock()
					log.Printf("UPnP 通道就绪: 外部 IP %s", g.ExternalIP())
				} else {
					log.Printf("NAT 穿透不可用(不影响下载,仅无入站加速),2 分钟后重试: %v", uerr)
					backoff = time.Now().Add(2 * time.Minute)
				}
			}
			// 发现窗口可长达 40s,期间端口集合可能已清空(会话全部退出):
			// 重取快照,避免给已无监听的端口建映射(僵尸映射要到下轮才撤销)
			if method != "" && len(ports) > 0 {
				ports = m.btPortsSnapshot()
			}
		}
		// 对账:缺失端口建映射;已无监听的端口撤销;周期性全量重申租约
		if method != "" {
			renew := cycles%40 == 0 // 30s × 40 ≈ 20 分钟
			for p := range ports {
				if mapped[p] && !renew {
					continue
				}
				if err := natMapPort(m, gw, pmpGW, p); err != nil {
					log.Printf("端口 %d 映射失败(下轮重试): %v", p, err)
					continue
				}
				if !mapped[p] {
					mapped[p] = true
					log.Printf("NAT 映射建立: %d TCP+UDP(%s)", p, method)
				}
			}
			for p := range mapped {
				if ports[p] {
					continue
				}
				natUnmapPort(m, gw, pmpGW, p)
				delete(mapped, p)
				log.Printf("NAT 映射撤销: %d(已无监听会话)", p)
			}
		}
		cycles++
		select {
		case <-stop:
			for p := range mapped {
				natUnmapPort(m, gw, pmpGW, p)
			}
			if len(mapped) > 0 {
				log.Printf("NAT 映射已清除(%d 条)", len(mapped))
			}
			m.upnpMu.Lock()
			m.upnp = ""
			m.extIP = ""
			m.upnpMu.Unlock()
			return
		case <-m.rootCtx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func natMapPort(m *Manager, gw *bt.UPnP, pmpGW net.IP, port int) error {
	if pmpGW != nil {
		return bt.NATPMPMap(pmpGW, port)
	}
	if gw == nil {
		return errors.New("NAT 通道未就绪")
	}
	ctx, cancel := context.WithTimeout(m.rootCtx, 15*time.Second)
	defer cancel()
	if err := gw.AddPortMapping(ctx, port, "tcp", port, "ZDownloader BT"); err != nil {
		return err
	}
	return gw.AddPortMapping(ctx, port, "udp", port, "ZDownloader DHT")
}

func natUnmapPort(m *Manager, gw *bt.UPnP, pmpGW net.IP, port int) {
	if pmpGW != nil {
		bt.NATPMPUnmap(pmpGW, port)
		return
	}
	if gw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.rootCtx, 10*time.Second)
	defer cancel()
	_ = gw.DeletePortMapping(ctx, port, "tcp")
	_ = gw.DeletePortMapping(ctx, port, "udp")
}

// natStop 停止对账循环(循环自身会撤销全部映射)。
func (m *Manager) natStop() {
	m.upnpMu.Lock()
	stop := m.upnpStop
	m.upnpStop = nil
	m.upnpMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// startDHT 按配置启动共享 DHT 节点(幂等)。
func (m *Manager) startDHT() {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	if !m.cfg.BT.DHT || m.dhtSrv != nil {
		return
	}
	srv, err := bt.StartDHT(bt.DHTConfig{
		Port:     m.cfg.BT.Port,
		Resolver: m.dohResolver(),
		// 路由表随 config 落盘(dht.json):重启回灌旧节点,
		// 引导域名被墙/污染时也能靠活着的旧邻居入网
		PersistFile: filepath.Join(filepath.Dir(m.cfgPath), "dht.json"),
	})
	if err != nil {
		m.dhtFailed = err.Error() // 不致命:BT 仍可走 tracker
		log.Printf("DHT 启动失败(仅 tracker 模式): %v", err)
		return
	}
	m.dhtFailed = ""
	log.Printf("DHT 节点已启动 udp:%d(直连)", srv.Port())
	m.dhtSrv = srv

	// IPv6 DHT(BEP-32)独立实例:独立路由表与套接字,host 有 v6 网络
	// 时多一张网入 peer;失败不致命(v4 主实例照常)
	srv6, err := bt.StartDHT(bt.DHTConfig{
		Network:  "udp6",
		Port:     m.cfg.BT.Port,
		Resolver: m.dohResolver(),
		// 独立落盘,避免双实例写同一文件互相覆盖
		PersistFile: filepath.Join(filepath.Dir(m.cfgPath), "dht6.json"),
	})
	if err != nil {
		log.Printf("IPv6 DHT 未启用(无 v6 网络时不影响): %v", err)
		return
	}
	m.dhtSrv6 = srv6
	log.Printf("IPv6 DHT 节点已启动 udp6:%d", srv6.Port())
}

// dohCacheItem DoH 解析缓存项。
type dohCacheItem struct {
	ips []net.IP
	at  time.Time
}

// dohResolver 构建 DNS-over-HTTPS 解析器(1.1.1.1/8.8.8.8 JSON API,
// 经代理出口访问),规避本地 DNS 污染(引导域名被解析到假 IP 会导致
// DHT 冷启动失败);DoH 失败回退系统解析,结果缓存 1 小时。
func (m *Manager) dohResolver() func(ctx context.Context, host string) ([]net.IP, error) {
	if !m.cfg.Proxy.DNSOn() { // 用户关闭 DNS 走代理:使用系统解析
		return nil
	}
	tr := proxy.Transport(proxy.Options{
		Mode:          m.cfg.Proxy.Mode,
		URL:           m.cfg.Proxy.URL,
		SkipTLSVerify: m.cfg.SkipTLSVerify,
	})
	hc := &http.Client{Timeout: 8 * time.Second, Transport: tr}
	var mu sync.Mutex
	cache := map[string]dohCacheItem{}
	return func(ctx context.Context, host string) ([]net.IP, error) {
		mu.Lock()
		c, ok := cache[host]
		mu.Unlock()
		if ok && time.Now().Before(c.at.Add(time.Hour)) {
			return c.ips, nil
		}
		ips := dohQuery(ctx, hc, host)
		if len(ips) == 0 { // 系统解析兜底
			if addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
				for _, a := range addrs {
					ips = append(ips, a.IP)
				}
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("解析失败: %s", host)
		}
		mu.Lock()
		cache[host] = dohCacheItem{ips: ips, at: time.Now()}
		mu.Unlock()
		log.Printf("DoH 解析 %s -> %s", host, ips[0])
		return ips, nil
	}
}

// dohQuery 依次尝试两个 DoH 端点,返回 A 记录 IPv4 列表。
func dohQuery(ctx context.Context, hc *http.Client, host string) []net.IP {
	for _, ep := range []string{
		"https://1.1.1.1/dns-query?name=%s&type=A",
		"https://8.8.8.8/resolve?name=%s&type=A",
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(ep, url.QueryEscape(host)), nil)
		if err != nil {
			continue
		}
		req.Header.Set("accept", "application/dns-json")
		resp, err := hc.Do(req)
		if err != nil {
			continue
		}
		var d struct {
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			} `json:"Answer"`
		}
		err = json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range d.Answer {
			if a.Type == 1 {
				if ip := net.ParseIP(a.Data); ip != nil && ip.To4() != nil {
					ips = append(ips, ip)
				}
			}
		}
		if len(ips) > 0 {
			return ips
		}
	}
	return nil
}

// getUDPRelay 返回共享 SOCKS5 UDP 中继(懒创建;未配置 socks5 代理或
// tracker 粒度关闭时返回 nil,调用方按直连处理)。
// 手动模式取配置的代理 URL;跟随系统模式取动态解析结果(环境变量或
// Windows 系统代理)。http 代理无法转发 UDP,一律直连。
func (m *Manager) getUDPRelay() net.PacketConn {
	m.udpRelayMu.Lock()
	defer m.udpRelayMu.Unlock()
	if m.udpRelay != nil {
		return m.udpRelay
	}
	if !m.cfg.Proxy.TrackerOn() {
		return nil
	}
	var pu *url.URL
	switch m.cfg.Proxy.Mode {
	case "manual":
		u, err := url.Parse(m.cfg.Proxy.URL)
		if err != nil {
			return nil
		}
		pu = u
	case "env":
		pu = proxy.FollowSystemProxy()
	default: // none
		return nil
	}
	if pu == nil {
		return nil
	}
	switch pu.Scheme {
	case "socks5", "socks5h":
	default:
		return nil // http 代理无法转发 UDP
	}
	conn, err := proxy.SOCKS5UDPRelay(pu)
	if err != nil {
		log.Printf("SOCKS5 UDP 中继建立失败(udp tracker 与 DHT 将直连): %v", err)
		return nil
	}
	log.Printf("SOCKS5 UDP 中继已建立 %s(udp tracker 与 DHT 经代理出口)", pu.Host)
	m.udpRelay = conn
	return conn
}

// closeUDPRelay 关闭并清空共享 UDP 中继(配置变更/退出时)。
func (m *Manager) closeUDPRelay() {
	m.udpRelayMu.Lock()
	conn := m.udpRelay
	m.udpRelay = nil
	m.udpRelayMu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// stopDHT 停止并清空共享 DHT 节点(幂等)。
func (m *Manager) stopDHT() {
	m.dhtMu.Lock()
	srv := m.dhtSrv
	m.dhtSrv = nil
	srv6 := m.dhtSrv6
	m.dhtSrv6 = nil
	m.dhtMu.Unlock()
	srv.Stop()
	if srv6 != nil {
		srv6.Stop()
	}
}

// DHTPort 返回共享 DHT 节点的 UDP 端口(未启用返回 0)。
func (m *Manager) DHTPort() int {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	return m.dhtSrv.Port()
}

// dhtNodeCount 共享 DHT 路由表节点数(v4+v6 合计,UI 健康展示用)。
func (m *Manager) dhtNodeCount() int {
	m.dhtMu.Lock()
	defer m.dhtMu.Unlock()
	if m.dhtSrv == nil {
		return 0
	}
	n := m.dhtSrv.NodeCount()
	if m.dhtSrv6 != nil {
		n += m.dhtSrv6.NodeCount()
	}
	return n
}

func (m *Manager) Version() uint64 { return m.version.Load() }

func (m *Manager) bump() { m.version.Add(1) }

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Config() conf.Config { return m.cfg }

// AddRecentDir 记录最近使用的保存目录(前端创建成功后调用):去重排序后落盘,
// 返回最新配置(含完整列表,供接口直接回给前端刷新下拉)。
func (m *Manager) AddRecentDir(dir string) conf.Config {
	if !m.cfg.AddRecentDir(dir) {
		return m.cfg
	}
	_ = m.cfg.Save(m.cfgPath)
	return m.cfg
}

// RemoveRecentDir 删除一条最近使用的保存目录(下拉内逐条清除),落盘并返回最新配置。
func (m *Manager) RemoveRecentDir(dir string) conf.Config {
	if !m.cfg.RemoveRecentDir(dir) {
		return m.cfg
	}
	_ = m.cfg.Save(m.cfgPath)
	return m.cfg
}

// UpdateWindowPos 记住主窗位置与尺寸(桌面版周期采样调用),落盘,不动其他配置。
func (m *Manager) UpdateWindowPos(x, y, w, h int) {
	m.cfg.WinX, m.cfg.WinY, m.cfg.WinW, m.cfg.WinH, m.cfg.WinPosSet = x, y, w, h, true
	_ = m.cfg.Save(m.cfgPath)
}

func (m *Manager) UpdateConfig(cfg conf.Config) error {
	old := m.cfg
	cfg.Save(m.cfgPath)
	m.cfg = cfg
	m.gLim.SetRate(cfg.GlobalLimit)
	m.btULim.SetRate(cfg.BT.UPSpeed)
	// 坏块自动封禁开关、白名单阈值、坏块宽容次数与客户端黑名单热同步:
	// 四者经 ClientConfig 只在会话创建时注入,旧实现改开关后运行中会话
	// 仍按旧值继续封禁(实测"关了还在积累封禁 IP")
	for _, t := range m.snapshotTasks() {
		t.mu.Lock()
		btcl := t.btcl
		t.mu.Unlock()
		if btcl != nil {
			btcl.SetAutoBanPoison(cfg.BT.AutoBanPoison)
			btcl.SetPoisonRate(cfg.BT.PoisonRate)
			btcl.SetPoisonStrikes(cfg.BT.PoisonStrikes)
			btcl.SetClientBlacklist(cfg.BT.ClientBlacklist)
		}
	}
	m.transportsMu.Lock()
	m.transports = map[string]*http.Transport{}
	m.transportsMu.Unlock()
	// 代理出口变化:UDP 中继与 DHT 节点都需重建(旧中继连接已失效)
	proxyChanged := old.Proxy.Mode != cfg.Proxy.Mode || old.Proxy.URL != cfg.Proxy.URL ||
		old.Proxy.TrackerOn() != cfg.Proxy.TrackerOn()
	// DHT 开关热生效:开启即起节点,关闭即停(进行中会话只是失去 DHT 来源)
	if cfg.BT.DHT {
		if proxyChanged {
			m.closeUDPRelay()
			m.stopDHT()
		}
		m.startDHT()
	} else {
		m.stopDHT()
		if proxyChanged {
			m.closeUDPRelay()
		}
	}
	m.bump()
	m.kick()
	return nil
}

// ---------- 任务增删查改 ----------

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 无法解析")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https")
	}
	if u.Host == "" {
		return fmt.Errorf("URL 缺少主机名")
	}
	return nil
}

func detectKind(raw string) string {
	lu := strings.ToLower(raw)
	if strings.Contains(lu, ".m3u8") {
		return KindHLS
	}
	if strings.Contains(lu, ".mpd") {
		return KindHLS // HLS 运行器会对 DASH 给出明确报错
	}
	return KindHTTP
}

// Create 批量创建任务,返回成功创建的任务与逐条错误。
func (m *Manager) Create(req CreateReq) ([]TaskView, []string) {
	urls := req.URLList()
	if len(urls) == 0 {
		return nil, []string{"缺少 URL"}
	}

	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = m.cfg.DownloadDir
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(m.cfg.DownloadDir, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, []string{"无法创建下载目录: " + err.Error()}
	}

	// 指定文件名(浏览器捕获/前端传入);留空则探测时由 CD/URL 推导。
	// 注意 SanitizeFilename 对空串兜底返回 "download",必须先判空。
	specName := strings.TrimSpace(req.Filename)
	if specName != "" {
		specName = util.SanitizeFilename(specName)
	}

	conns := req.Connections
	if conns <= 0 {
		conns = m.cfg.MaxConnections
	}
	if conns < 1 {
		conns = 1
	}
	if conns > 64 {
		conns = 64
	}

	var views []TaskView
	var errs []string
	for _, u := range urls {
		if err := checkURL(u); err != nil {
			errs = append(errs, u+" — "+err.Error())
			continue
		}
		kind := req.Kind
		if kind == "" {
			kind = detectKind(u)
		}
		t := &Task{
			ID:            util.RandomID(),
			Kind:          kind,
			URL:           u,
			Dir:           dir,
			Filename:      "", // 指定文件名时入表后占用唯一名;留空则探测时按 CD/URL 推导
			Size:          -1,
			Connections:   conns,
			Status:        StatusWaiting,
			CreatedAt:     time.Now(),
			Headers:       req.Headers,
			Referer:       req.Referer,
			ProxyOverride: strings.TrimSpace(req.Proxy),
			SpeedLimit:    req.SpeedLimit,
			Checksum:      strings.TrimSpace(req.Checksum),
			Source:        req.Source,
		}
		m.mu.Lock()
		m.tasks[t.ID] = t
		m.order = append(m.order, t.ID)
		m.mu.Unlock()
		// 指定文件名 + 批量:同名任务会写同一个 .zdl.part、下载中互相覆盖。
		// 入表后逐个占用"同目录唯一"文件名(顺延编号,跳过在途任务与磁盘已有文件)。
		if specName != "" {
			fn := m.claimFilename(dir, specName, t.ID)
			t.mu.Lock()
			t.Filename = fn
			t.mu.Unlock()
		}
		m.persist(t)
		log.Printf("创建任务 %s kind=%s 线程=%d -> %s", t.URL, kind, t.Connections, dir)
		views = append(views, t.View())
	}
	m.bump()
	m.kick()
	return views, errs
}

// claimFilename 在"同目录已占用名 + 磁盘"中为 base 挑一个不冲突的文件名并登记
// 到 taskName,返回它。全程持 m.mu,并发的创建/探测不会选到同一个名字。
// base 为空原样返回空(由调用方按 CD/URL 推导后再判空)。
func (m *Manager) claimFilename(dir, base, id string) string {
	if base == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	taken := make([]string, 0, len(m.taskName))
	for id2, n := range m.taskName {
		if id2 == id {
			continue
		}
		if t2 := m.tasks[id2]; t2 != nil && t2.Dir == dir {
			taken = append(taken, n)
		}
	}
	cand := util.UniqueName(dir, base, taken)
	if m.taskName == nil {
		m.taskName = map[string]string{}
	}
	m.taskName[id] = cand
	return cand
}

func (m *Manager) get(id string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// List 按 创建顺序 返回任务快照。
func (m *Manager) List() []TaskView {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	out := make([]TaskView, 0, len(ids))
	for _, id := range ids {
		if t := m.tasks[id]; t != nil {
			out = append(out, t.View())
		}
	}
	return out
}

func (m *Manager) Get(id string) (TaskView, bool) {
	t := m.get(id)
	if t == nil {
		return TaskView{}, false
	}
	return t.View(), true
}

func (m *Manager) Pause(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	switch t.Status {
	case StatusRunning:
		t.Status = StatusPaused
		if t.cancel != nil {
			t.cancel()
		}
	case StatusWaiting, StatusChoosing:
		// choosing 暂停 = 放弃选择等待:ChoosePending 已在挂起时清除,
		// 之后点继续会按全部文件下载
		t.Status = StatusPaused
	case StatusSeeding:
		// 停止做种:归入已完成,做种协程负责收尾
		t.Status = StatusCompleted
		t.CompletedAt = time.Now()
		stop := t.seedStop
		t.mu.Unlock()
		if stop != nil {
			close(stop)
		}
		m.bump()
		return nil
	default:
		st := t.Status
		t.mu.Unlock()
		return fmt.Errorf("状态 %s 不可暂停", st)
	}
	t.mu.Unlock()
	m.bump()
	return nil
}

// Update 修改未完成任务的可变属性。当前支持每任务限速(字节/秒,0 不限):
// 运行中的任务通过限速句柄即时生效,等待/暂停中的任务在下次运行时生效。
func (m *Manager) Update(id string, speedLimit, upLimit int64) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	switch t.Status {
	case StatusCanceled:
		t.mu.Unlock()
		return fmt.Errorf("任务已取消,不可修改")
	}
	if speedLimit < 0 {
		speedLimit = 0
	}
	t.SpeedLimit = speedLimit
	if t.limits != nil {
		t.limits.set(speedLimit)
	}
	if upLimit >= 0 && t.Kind == KindBT {
		t.UPLimit = upLimit
		if t.btcl != nil {
			btc := t.btcl
			// 任务级限速热更新:0 = 不设任务级上限,由共享全局限速器兜底
			// (全局值变更走 UpdateConfig 对 gLim/btULim SetRate,运行中
			// 会话经共享实例即时生效)。旧实现把全局值折算进会话限速器,
			// 改全局后已运行会话拿不到新值,多任务时预算还按任务数翻倍;
			// 且 upLimit=0 需特判回填全局上传限速,否则被静默抹成不限速。
			down := speedLimit
			if down <= 0 {
				down = m.cfg.TaskLimit
			}
			t.mu.Unlock()
			btc.SetLimits(down, upLimit)
			m.persist(t)
			m.bump()
			return nil
		}
	}
	t.mu.Unlock()
	m.persist(t)
	m.bump()
	return nil
}

// Clear 清扫指定状态的全部任务记录(仅允许 completed/canceled),
// delFile 时连同已下载文件一起删除,返回删除条数。
func (m *Manager) Clear(status Status, delFile bool) (int, error) {
	if status != StatusCompleted && status != StatusCanceled {
		return 0, fmt.Errorf("仅允许清扫已完成或已取消分类")
	}
	n := 0
	for _, v := range m.List() {
		if v.Status != status {
			continue
		}
		if err := m.Remove(v.ID, delFile); err == nil {
			n++
		}
	}
	return n, nil
}

func (m *Manager) Resume(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	needPersist := false
	switch t.Status {
	case StatusPaused, StatusError, StatusCanceled, StatusChoosing:
		// choosing 点继续 = 跳过选择,按全部文件下载
		fromCanceled := t.Status == StatusCanceled
		t.Status = StatusWaiting
		t.Err = ""
		t.speed = 0
		t.lastBytes = 0
		if fromCanceled && t.Kind == KindBT {
			// 存量记录自愈:更早版本取消的任务 JSON 里进度账目还留着旧值
			// (数据已被取消清理),不清的话重新下载后进度条先跳到旧位置、
			// 等 pump 首个 tick 才归零(实测)。盘上若有更早版本遗留的数据
			// 被续用,pump 首个 tick 会按扫描校验结果重算,不丢真进度
			t.Arrived, t.arrived, t.PiecesDone, t.Downloaded = 0, 0, 0, 0
			needPersist = true
		}
	default:
		st := t.Status
		t.mu.Unlock()
		return fmt.Errorf("状态 %s 无法继续", st)
	}
	t.mu.Unlock()
	if needPersist {
		m.persist(t)
	}
	m.bump()
	m.kick()
	return nil
}

func (m *Manager) Cancel(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	unfinishedBT := false
	switch t.Status {
	case StatusRunning:
		t.Status = StatusCanceled
		if t.cancel != nil {
			t.cancel()
		}
	case StatusWaiting, StatusPaused, StatusChoosing, StatusError:
		// 这些状态没有活动会话(句柄已释放),取消即清理:就地半成品与
		// 描述它的续传状态一起作废,与"运行中取消"同语义——否则从暂停
		// 取消的数据目录会遗留,重新开始时又被扫描恢复,同一个"取消"
		// 按钮两种结局。进度账目一并清零:数据已丢弃,旧账留着会让
		// "重新下载"的进度条先跳到旧位置,等 pump 首个 tick 才归零(实测)
		t.Status = StatusCanceled
		t.Err = ""
		if t.Kind == KindBT {
			t.Bitfield, t.BlockBits = "", ""
			t.Arrived, t.arrived, t.PiecesDone, t.Downloaded = 0, 0, 0, 0
			unfinishedBT = true
		}
	case StatusSeeding:
		t.Status = StatusCanceled
		t.Err = ""
		stop := t.seedStop
		btc := t.btcl
		t.mu.Unlock()
		if stop != nil {
			close(stop)
		}
		if btc != nil {
			btc.Stop()
		}
		m.cleanupTemp(t)
		m.bump()
		m.kick()
		return nil
	default:
		st := t.Status
		t.mu.Unlock()
		return fmt.Errorf("状态 %s 无需取消", st)
	}
	t.mu.Unlock()
	if unfinishedBT {
		m.cleanupBTPartial(t)
		m.persist(t) // 位图已作废,立即落盘干净状态
	}
	m.bump()
	m.kick()
	return nil
}

// Remove 移除任务记录;deleteFile 时连同已完成的文件/目录一起删除。
// 记录从列表与磁盘即刻移除(UI 即时消失);运行/做种协程若存在,仅被通知
// 收尾(释放句柄、断开 peer),不再自行持久化或复活记录。
func (m *Manager) Remove(id string, deleteFile bool) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st := t.Status
	// removed 置位后 persist 一律跳过:运行/做种协程收尾、周期持久化都不会
	// 把已删除任务的 JSON 重新写盘(旧实现里做种协程收尾 persist 会让
	// 刚删除的任务在重启后复活)。
	t.removed = true
	var stop chan struct{}
	var btc *bt.Client
	switch st {
	case StatusRunning:
		if t.cancel != nil {
			t.cancel()
		}
	case StatusSeeding:
		t.Status = StatusCanceled
		stop = t.seedStop
		btc = t.btcl
		// 做种期数据在最终路径、且文件句柄仍被做种会话持有;由做种协程
		// 收尾(Close 后再删),否则 Windows 上删除会因句柄占用静默失败。
		t.delData = deleteFile
	default:
		if st == StatusWaiting {
			t.Status = StatusCanceled
		}
	}
	t.mu.Unlock()

	// 立即从列表移除并删除记录文件:即使随后崩溃也不会在重启时复活
	// (旧实现运行中删除只是标记,任务会滞留到协程退出才消失)。
	m.mu.Lock()
	delete(m.tasks, id)
	delete(m.taskName, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	m.deleteRecord(t)

	// 通知做种协程退出;运行协程由 runTask 的 ctx 取消自行收尾。
	if stop != nil {
		close(stop)
	}
	if btc != nil {
		btc.Stop()
	}

	switch {
	case st == StatusCompleted || st == StatusSeeding:
		// 已完成/做种中:数据已落定到最终路径,仅在要求时删除。
		// 做种中由协程收尾后删除(见 t.delData),此处只处理已完成。
		if deleteFile && st == StatusCompleted {
			m.removeFinalData(t)
		}
	case st == StatusRunning:
		// 文件句柄仍在运行协程手里,临时文件由其 defer 清理,此处不动。
	default:
		// 等待/暂停/choosing/失败/已取消:无活动会话(句柄已释放),
		// 半成品就地数据与"运行中删除"同语义一律清理(记录没了,半成品
		// 无法续传,只会在下载目录里当无主垃圾)——旧实现只删 legacy
		// 临时路径,就地半成品目录必遗留(实测:暂停态删除后数据目录还在)
		m.cleanupTemp(t)
		m.cleanupBTPartial(t)
	}
	m.bump()
	return nil
}

// removeFinalData 删除已完成任务的最终数据。BT 多文件任务的最终路径是目录,
// 必须递归删除(旧实现对目录调用 os.Remove 静默失败,删不干净)。
func (m *Manager) removeFinalData(t *Task) {
	t.mu.Lock()
	p := t.finalPath()
	t.mu.Unlock()
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	if fi.IsDir() {
		_ = os.RemoveAll(p)
		return
	}
	_ = os.Remove(p)
}

func (m *Manager) Stats() map[string]any {
	var active, waiting, done, failed int
	var speed, upSpeed int64
	for _, v := range m.List() {
		switch v.Status {
		case StatusRunning:
			active++
			speed += v.Speed
			upSpeed += v.UpSpeed
		case StatusSeeding:
			// 做种不占下载速度:其 speed/UpSpeed 均为上传速度
			upSpeed += v.UpSpeed
		case StatusWaiting:
			waiting++
		case StatusCompleted:
			done++
		case StatusError:
			failed++
		}
	}
	return map[string]any{
		"totalSpeed":   speed,
		"totalUpSpeed": upSpeed,
		"active":       active,
		"waiting":      waiting,
		"completed":    done,
		"error":        failed,
		"dhtNodes":     m.dhtNodeCount(),
		"nat":          m.NATStatus(),
		"rssUnread":    m.rssUnread(),
	}
}

// OpenLocation 打开任务所在目录:已完成/做种中的任务在文件管理器中
// 定位选中文件;其余状态(文件尚未落定)只打开下载目录。
func (m *Manager) OpenLocation(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st, fn, dir := t.Status, t.Filename, t.Dir
	t.mu.Unlock()
	// 做种任务同样允许定位:数据就在最终路径(旧实现仅 completed,
	// 做种任务点"打开所在文件夹"必报错)。
	if fn != "" && (st == StatusCompleted || st == StatusSeeding) {
		path := filepath.Join(dir, fn)
		if _, err := os.Stat(path); err == nil {
			return util.OpenFileLocation(path)
		}
	}
	if dir == "" {
		return fmt.Errorf("任务没有可打开的目录")
	}
	return util.OpenDir(dir)
}

// openFileHook 供测试替换,避免测试时真的拉起系统程序。
var openFileHook = util.OpenFile

// OpenFile 用系统默认程序打开已完成任务的文件。
// exe 等可执行文件的确认提示由 Web UI 负责。
func (m *Manager) OpenFile(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st, fn, dir := t.Status, t.Filename, t.Dir
	t.mu.Unlock()
	if st != StatusCompleted && st != StatusSeeding {
		return fmt.Errorf("仅已完成/做种中的任务支持打开文件")
	}
	path := filepath.Join(dir, fn)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("文件不存在或已被移动: %s", path)
	}
	return openFileHook(path)
}

// ---------- 调度与运行 ----------

func (m *Manager) scheduler(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
		if m.quitting.Load() {
			return // Shutdown 流程中:退出任务不再复活,等待落盘
		}
		for {
			m.mu.Lock()
			if m.running >= m.cfg.MaxActiveTasks {
				m.mu.Unlock()
				break
			}
			if m.quitting.Load() {
				m.mu.Unlock()
				return
			}
			var next *Task
			for _, id := range m.order {
				if t := m.tasks[id]; t != nil {
					t.mu.Lock()
					st := t.Status
					t.mu.Unlock()
					if st == StatusWaiting {
						next = t
						break
					}
				}
			}
			if next != nil {
				// 状态复核必须在最终持锁时进行:扫描到 Waiting 后到此刻之间,
				// 用户可能已取消/暂停该任务(或已删除)。旧实现直接置 Running,
				// 会把刚被取消的任务强行拉起(取消不生效、竞态下还在下载)。
				next.mu.Lock()
				if next.Status != StatusWaiting || next.removed {
					next.mu.Unlock()
					m.mu.Unlock()
					continue // 状态已变:重新扫描下一个可运行任务
				}
				next.Status = StatusRunning
				next.mu.Unlock()
				m.running++
				m.mu.Unlock()
				m.wg.Add(1)
				go m.runTask(next)
				continue
			}
			m.mu.Unlock()
			break
		}
	}
}

// Shutdown 取消所有运行中任务,等待 worker 退出并确保状态落盘。
// 运行中的任务按"等待续传"落盘(而非暂停),下次启动自动恢复下载;
// 用户主动暂停的任务在 Pause 里已置 paused,不受影响。
func (m *Manager) Shutdown(ctx context.Context) {
	m.quitting.Store(true)
	for _, t := range m.snapshotTasks() {
		t.mu.Lock()
		if t.Status == StatusRunning && t.cancel != nil {
			t.cancel()
		}
		t.mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		m.seedWG.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	for _, t := range m.snapshotTasks() {
		t.mu.Lock()
		if t.Status == StatusRunning {
			t.Status = StatusWaiting // 兜底:runTask 已退出但状态仍是运行中
		}
		t.mu.Unlock()
		m.persist(t)
	}
	m.btHubStopAll() // 异常滞留的共享监听端口兜底关停(正常路径会话退出时已停)
	m.natStop()
	m.stopLSD()
	m.stopDHT()
	m.closeUDPRelay()
}

func (m *Manager) runTask(t *Task) {
	defer m.wg.Done()
	ctx, cancel := context.WithCancel(m.rootCtx)
	t.mu.Lock()
	t.cancel = cancel
	t.runID++
	myRun := t.runID
	t.mu.Unlock()

	defer func() {
		cancel()
		m.mu.Lock()
		m.running--
		m.mu.Unlock()

		t.mu.Lock()
		sameRun := t.runID == myRun
		removed := t.removed
		t.mu.Unlock()
		m.bump()
		m.kick()
		if !sameRun { // 新一轮运行已接管,不再动状态与持久化
			return
		}
		if removed {
			// 运行中被删除:记录已由 Remove 立即删除,这里只清理临时文件
			// 与就地半成品(旧实现把 !removed 并进 sameRun,导致该分支永不
			// 执行、临时文件残留;就地布局后半成品在最终路径,cleanupTemp
			// 的旧布局路径删不到它,必须走 cleanupBTPartial)。
			m.cleanupTemp(t)
			m.cleanupBTPartial(t)
			m.deleteRecord(t)
		} else {
			m.persist(t)
		}
	}()

	t.mu.Lock()
	kind := t.Kind
	t.mu.Unlock()

	var err error
	switch kind {
	case KindHLS:
		err = m.runHLS(ctx, t)
	case KindBT:
		err = m.runBT(ctx, t)
	default:
		err = m.runHTTP(ctx, t)
	}

	t.mu.Lock()
	if t.runID != myRun {
		t.mu.Unlock()
		return
	}
	if t.Status == StatusRunning {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Status = StatusError
			t.Err = err.Error()
		} else if m.quitting.Load() || m.rootCtx.Err() != nil {
			t.Status = StatusWaiting // 程序关闭:落盘等待,下次启动自动续传
		} else {
			t.Status = StatusPaused // ctx 取消但无人改状态:视为暂停
		}
	}
	st := t.Status
	t.mu.Unlock()
	if kind == KindHTTP && st == StatusCompleted && m.cfg.AutoOpenTorrent {
		// HTTP 下载的 .torrent 完成后自动创建 BT 任务(文件关联页可关);
		// 异步执行,不阻塞运行协程收尾
		go m.autoOpenTorrent(t)
	}
	if st == StatusCanceled {
		m.cleanupTemp(t)
		m.cleanupBTPartial(t)
		// 数据已删,续传状态必须一并作废:块级位图/已校验位图描述的是已
		// 不存在的盘上数据,留着的话重新开始时 RestoreBlocks 会把它们当
		// "上个会话所写"直接标记持有——幻影块既瞬间虚报进度(实测取消
		// 重启凭空多出 80MB),又让 piece 以零块凑数触发校验失败整片重下。
		// 进度账目同作废:数据已丢弃,旧账会让"重新下载"的进度条先跳到
		// 旧位置再被 pump 校正归零(实测)
		t.mu.Lock()
		hadState := t.Bitfield != "" || t.BlockBits != ""
		t.Bitfield, t.BlockBits = "", ""
		if t.Kind == KindBT {
			t.Arrived, t.arrived, t.PiecesDone, t.Downloaded = 0, 0, 0, 0
			hadState = true
		}
		t.mu.Unlock()
		if hadState {
			m.persist(t)
		}
	}
}

// autoOpenTorrent HTTP 下载完成的 .torrent 自动创建 BT 任务(设置
// autoOpenTorrent 关闭)。种子文件也常被用户双击打开(文件关联),
// 同 InfoHash 已有任务时跳过,避免重复任务,并交由前端弹确认框提醒用户、
// 由其自行决定是否仍要创建;仅对本程序 HTTP 引擎下载的种子生效,
// BT/HLS 任务不触发。
func (m *Manager) autoOpenTorrent(t *Task) {
	t.mu.Lock()
	dir := t.Dir // 种子实际落盘目录(可能是用户为批量下载所选的目录,而非全局默认)
	path := filepath.Join(dir, t.Filename)
	t.mu.Unlock()
	if !strings.EqualFold(filepath.Ext(path), ".torrent") {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("自动打开种子失败(读取 %s): %v", path, err)
		return
	}
	meta, err := bt.ParseTorrent(raw)
	if err != nil {
		// 内容不是合法种子(如错误页存成 .torrent):记录后放弃
		log.Printf("自动打开种子失败(解析 %s): %v", path, err)
		return
	}
	ih := fmt.Sprintf("%x", meta.InfoHash)
	for _, v := range m.List() {
		if v.Kind == KindBT && strings.EqualFold(v.InfoHash, ih) {
			// 同 InfoHash 已有任务(任意状态:完成/下载中/取消/做种)时不静默吞掉——
			// 用户看不到日志。交由前端弹确认框提醒"未自动创建",把"是否仍要创建同名
			// 任务"的决定交给用户(引擎无法替用户决定,重复任务风险由用户知情后自行取舍)。
			log.Printf("种子 %s 已有任务(InfoHash %s),跳过自动打开(已提醒用户)", meta.Name, ih)
			if m.OnSeedConflict != nil {
				m.OnSeedConflict(meta.Name, path, dir)
			}
			return
		}
	}
	// 新 BT 任务沿用种子的落盘目录(t.Dir):与"保存种子的同路径"一致,
	// 批量下载时用户为整批(含 .torrent)选的目录即数据应下的目录。
	if _, errs := m.AddBT(CreateReq{TorrentPath: path, Dir: dir, TorrentGenerated: true}); len(errs) > 0 {
		log.Printf("自动打开种子失败: %v", errs)
		return
	}
	log.Printf("已自动打开下载完成的种子:%s(%s)", meta.Name, ih)
}

// cleanupBTPartial 就地布局的取消清理:BT 任务数据自始至终写在最终路径,
// 取消(未完成)时连同半成品一起删除,对齐旧临时形态"取消即清理"的语义
// (旧实现删 <名>.zdlbt/.zdl.part,就地布局后那两个路径已不存在,取消会
// 把整个数据目录遗留在下载目录里)。完成/做种态的数据是成品,不在此清理
// (成品的删除走 removeFinalData,由 deleteFile 口径决定);完成与取消
// 竞态时 runBT 会把状态落成 completed,这里据状态二次防误删。
// 同名守护:另一个 BT 任务(运行/做种中,持有活跃会话)登记着同一
// (Dir, Filename) 时,盘上数据归它——磁力 choosing 任务与用户据其种子
// 创建的下载任务天然同名,取消/删除磁力任务若照删,会把下载任务正写着
// 的目录整个 RemoveAll:已关闭的文件被删、打开中的留下(实测残留恰好
// 一个 .!zd),活跃会话已落盘的数据也被误清。
func (m *Manager) cleanupBTPartial(t *Task) {
	t.mu.Lock()
	fn, dir, kind, st := t.Filename, t.Dir, t.Kind, t.Status
	t.mu.Unlock()
	if fn == "" || dir == "" || kind != KindBT {
		return
	}
	if st == StatusCompleted || st == StatusSeeding {
		return
	}
	if m.activeSiblingUsesName(t, fn) {
		return
	}
	_ = os.RemoveAll(filepath.Join(dir, fn))
	_ = os.Remove(filepath.Join(dir, fn+bt.IncompleteSuffix))
}

// activeSiblingUsesName 报告是否有别的 BT 任务登记着同一 (Dir, Filename)
// 且处于运行/做种态(可能持有着数据文件句柄)。等待/暂停/choosing 的
// 同名任务没有会话与句柄,不阻止清理——否则被取消任务的半成品反而
// 借同名兄弟"续命",留下无主垃圾。
func (m *Manager) activeSiblingUsesName(t *Task, name string) bool {
	for _, v := range m.List() {
		if v.ID == t.ID || v.Kind != KindBT {
			continue
		}
		if v.Status != StatusRunning && v.Status != StatusSeeding {
			continue
		}
		if filepath.Clean(v.Dir) == filepath.Clean(t.Dir) && strings.EqualFold(v.Filename, name) {
			return true
		}
	}
	return false
}

// btDataOnDisk 报告 BT 任务的就地数据是否在盘上(多文件看数据目录,单文件
// 看正式名或 .!zd 形态)。用于决定持久化的块级位图能否被信任——位图的
// 前提是"数据是上个会话所写",数据没了位图就是毒药(见 runBT 恢复处)。
func btDataOnDisk(t *Task) bool {
	t.mu.Lock()
	fn, dir := t.Filename, t.Dir
	t.mu.Unlock()
	if fn == "" || dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, fn)); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, fn) + bt.IncompleteSuffix)
	return err == nil
}

// ticker 每 500ms 汇总速度并推进持久化。
// speedStep 计算 500ms→秒折算速度(EWMA 平滑)。首个样本只立基线不作差:
// cur 里含重启前的累计量(下载基准/已落盘字节),直接作差会把历史总量
// 虚报成本次速度(重启后 UI 冲出十几 MB/s 再缓慢衰减的尾巴即源于此)。
// 返回 (新速度, 新基线, 已立基线)。
func speedStep(primed bool, speed int64, last, cur int64) (int64, int64, bool) {
	if !primed {
		return 0, cur, true
	}
	inst := (cur - last) * 2
	if speed == 0 {
		speed = inst
	} else if inst <= -speed {
		speed = 0 // 计数器大幅回退:立即清零,不走慢衰减
	} else {
		// α=1/4(时间常数约 2s):α=1/2 时两个 500ms 独立 ticker 间的
		// 相位漂移会让增量按块粒度跳动,EWMA 残余 ±8% 的可见锯齿;
		// 1/4 压到 ±2% 以内,响应延迟 ~2s 对 UI 无感。
		speed = speed - speed/4 + inst/4
	}
	if speed < 0 {
		speed = 0
	}
	return speed, cur, true
}

// stepTaskSpeed 更新单个任务的实时下载/上传速度,返回任务是否处于
// 运行/做种态(需要触发 UI 刷新与持久化)。
func (m *Manager) stepTaskSpeed(t *Task) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.Status {
	case StatusSeeding:
		// 做种:速度来自上传量增量(首样本同样只立基线,
		// Uploaded 含重启前的历史累计),上传速度即 speed
		t.speed, t.lastBytes, t.speedPrimed = speedStep(t.speedPrimed, t.speed, t.lastBytes, t.Uploaded)
		t.upSpeed = t.speed
		return true
	case StatusRunning:
		var cur int64
		if t.pool != nil && t.Size > 0 {
			cur = t.Size - t.pool.Remaining()
			t.Downloaded = cur
		} else {
			cur = t.Downloaded
		}
		t.speed, t.lastBytes, t.speedPrimed = speedStep(t.speedPrimed, t.speed, t.lastBytes, cur)
		// 上传速度按累计上传量增量独立采样(btPump 每 500ms 回写一次
		// t.Uploaded;HTTP/HLS 无上传,增量恒 0)
		t.upSpeed, t.lastUp, t.upPrimed = speedStep(t.upPrimed, t.upSpeed, t.lastUp, t.Uploaded)
		return true
	default:
		// 非运行/做种态速度恒 0(不变式)。做种退出路径(策略达标/手动
		// 停止/暂停)不落在此分支的零化范围外——旧实现只重置基线不清
		// speed,冻结的最后一拍 EWMA 会卡在 completed 任务上,下次重新
		// 做种时闪现成"上传速度突变"
		t.speed = 0
		t.upSpeed = 0
		t.speedPrimed = false // 离开运行态:下次进入重新立基线
		t.upPrimed = false
		return false
	}
}

func (m *Manager) ticker(ctx context.Context) {
	tk := time.NewTicker(500 * time.Millisecond)
	defer tk.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		n++
		dirty := false
		for _, t := range m.snapshotTasks() {
			if !m.stepTaskSpeed(t) {
				continue
			}
			dirty = true

			if n%4 == 0 { // 每 2 秒持久化一次进行中任务
				m.persist(t)
			}
		}
		if n%2 == 0 { // 每 1 秒扫一次 RSS 自动下载完成迁移(系统通知)
			m.notifyScan()
		}
		if dirty {
			m.bump()
		}
	}
}

// notifyScan 检测「RSS 自动下载(完成后提醒)」任务的完成时刻,经 OnNotify
// 弹 Windows 系统通知(托盘气泡,操作中心可见)。
//   - 进程启动后的首次扫描只立基线:已经处于完成/做种态的任务直接标记
//     "已提醒",避免重启后给历史任务补发通知;
//   - 此后凡首次观察到完成/做种态(且此前未提醒过)即触发——不依赖捕捉
//     "运行→完成"的中间快照,生命周期短于扫描间隔的本地小任务也能命中;
//   - 删除的任务顺带清理基线,防止长期运行下的 map 增长。
func (m *Manager) notifyScan() {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	if !m.notifyBaseline {
		m.notifyBaseline = true
		m.notifySeen = make(map[string]string, len(ids))
		for _, id := range ids {
			t := m.get(id)
			if t == nil {
				continue
			}
			t.mu.Lock()
			st := string(t.Status)
			t.mu.Unlock()
			if st == "completed" || st == "seeding" {
				m.notifySeen[id] = "notified" // 启动时已完成的:不补报
			}
		}
		return
	}
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		t := m.get(id)
		if t == nil {
			continue
		}
		t.mu.Lock()
		st, src, name := string(t.Status), t.Source, t.Filename
		t.mu.Unlock()
		if m.notifySeen[id] == "notified" {
			seen[id] = "notified" // 已提醒过:跨扫描保留标记,防止重复通知
			continue
		}
		seen[id] = st
		if src != "rss-notify" {
			continue
		}
		if st == "completed" || st == "seeding" {
			seen[id] = "notified"
			log.Printf("[rss] 系统通知:自动下载完成 %s", name)
			m.OnNotify("RSS 自动下载完成", name)
		}
	}
	m.notifySeen = seen
}

func (m *Manager) snapshotTasks() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	return out
}

// ---------- 持久化 ----------

func (m *Manager) taskJSONPath(t *Task) string {
	return filepath.Join(m.tasksDir, t.ID+".json")
}

func (m *Manager) persist(t *Task) {
	t.mu.Lock()
	if t.removed {
		// 已删除任务:任何收尾/周期持久化都不得把记录写回磁盘。
		t.mu.Unlock()
		return
	}
	if t.pool != nil {
		t.Segs = t.pool.Snapshot()
		if t.Size > 0 {
			t.Downloaded = t.Size - t.pool.Remaining()
		}
	}
	t.mu.Unlock()
	_ = util.WriteJSONAtomic(m.taskJSONPath(t), t)
}

func (m *Manager) deleteRecord(t *Task) {
	t.mu.Lock()
	tp := t.TorrentPath // 隐藏任务目录里的种子归档(TorrentPath 恒在 tasksDir 下)
	t.mu.Unlock()
	_ = os.Remove(m.taskJSONPath(t))
	if tp != "" {
		_ = os.Remove(tp) // 归档随任务一并清除,隐藏任务目录不堆积
	}
}

func (m *Manager) cleanupTemp(t *Task) {
	t.mu.Lock()
	fn := t.Filename
	dir := t.Dir
	t.mu.Unlock()
	if fn == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, fn+".zdl.part"))
	_ = os.RemoveAll(filepath.Join(dir, fn+".zdlbt")) // BT 多文件临时目录
}

// cleanupOrphanTemps 清扫崩溃遗留的无主临时文件(*.zdl.part / *.zdlwork)。
// 只有 等待/暂停/失败 状态的任务(可能续传)认领自己的临时文件予以保留;
// 已完成/已取消任务的遗留一律删除。扫描范围仅限本实例任务记录引用的目录:
// 多实例可能共享同一下载目录,别的实例的临时文件对这里而言不是"无主",
// 因此不做默认目录的全盘扫描(记录已删且目录未知的孤儿无法定位,尽力而为)。
// 须在 loadTasks 之后、调度器启动之前调用。
func (m *Manager) cleanupOrphanTemps() {
	m.mu.Lock()
	dirs := map[string]bool{}
	claimed := map[string]bool{}
	for _, t := range m.tasks {
		t.mu.Lock()
		dir, fn, st, kind := t.Dir, t.Filename, t.Status, t.Kind
		t.mu.Unlock()
		if dir == "" || fn == "" {
			continue
		}
		dirs[dir] = true
		if st != StatusWaiting && st != StatusPaused && st != StatusError {
			continue // 已完成/已取消:临时文件不会再被使用
		}
		claimed[filepath.Join(dir, fn+".zdl.part")] = true
		if kind == KindHLS {
			claimed[filepath.Join(dir, fn+".zdlwork")] = true
		}
	}
	m.mu.Unlock()

	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			isPart := strings.HasSuffix(name, ".zdl.part")
			isWork := e.IsDir() && strings.HasSuffix(name, ".zdlwork")
			if !isPart && !isWork {
				continue
			}
			if full := filepath.Join(dir, name); !claimed[full] {
				_ = os.RemoveAll(full)
			}
		}
	}
}

func (m *Manager) loadTasks() {
	entries, err := os.ReadDir(m.tasksDir)
	if err != nil {
		return
	}
	var order []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var t Task
		if err := util.ReadJSON(filepath.Join(m.tasksDir, e.Name()), &t); err != nil || t.ID == "" {
			continue
		}
		switch t.Status {
		case StatusRunning, StatusWaiting, StatusSeeding:
			t.Status = StatusWaiting // 启动时运行中/做种中的任务自动排队续传或恢复做种
		}
		t.speed = 0
		t.lastBytes = 0
		t.cancel = nil
		t.pool = nil
		t.arrived = t.Arrived // 恢复持久化的真实落盘量,暂停态任务也显示正确进度
		if t.Kind == KindBT && t.Arrived == 0 && t.Bitfield != "" && t.PiecesTotal > 0 && t.Size > 0 {
			// 旧版本未持久化 arrived:从位图一次性估算(按完成比例),
			// 会话启动后 btPump 会用精确值覆盖
			if raw, err := base64.StdEncoding.DecodeString(t.Bitfield); err == nil {
				done := 0
				for _, b := range raw {
					for ; b != 0; b &= b - 1 {
						done++
					}
				}
				if done >= t.PiecesTotal {
					t.arrived = t.Size
				} else if done > 0 {
					t.arrived = t.Size * int64(done) / int64(t.PiecesTotal)
				}
				t.Arrived = t.arrived
			}
		}
		if t.Kind == "" {
			t.Kind = KindHTTP
		}
		m.mu.Lock()
		m.tasks[t.ID] = &t
		order = append(order, t.ID)
		m.mu.Unlock()
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := m.tasks[order[i]], m.tasks[order[j]]
		if a == nil || b == nil {
			return false
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	m.mu.Lock()
	m.order = order
	if m.taskName == nil {
		m.taskName = map[string]string{}
	}
	for id, t := range m.tasks {
		if t.Filename != "" {
			m.taskName[id] = t.Filename
		}
	}
	m.mu.Unlock()
}

// ---------- 传输层 ----------

// proxyOptsFor 解析任务的网络出口;purpose 为 "http"/"tracker"/"peer"/"webseed"。
// 全局手动代理按用途粒度开关放行(关闭 = 该用途直连);任务级显式代理不受粒度约束。
func (m *Manager) proxyOptsFor(t *Task, purpose string) proxy.Options {
	t.mu.Lock()
	po := t.ProxyOverride
	t.mu.Unlock()

	opts := proxy.Options{SkipTLSVerify: m.cfg.SkipTLSVerify}
	if po == "none" {
		return opts // 任务显式直连
	}
	if po != "" {
		opts.Manual = po
		return opts
	}
	if !m.proxyPurposeOn(purpose) {
		// 该用途关闭 → 强制直连。手动与环境变量模式一致:环境变量模式下
		// 也必须显式 none,否则拨号器会重新读环境变量绕过粒度开关
		opts.Manual = "none"
		return opts
	}
	opts.Mode = m.cfg.Proxy.Mode
	opts.URL = m.cfg.Proxy.URL
	return opts
}

func (m *Manager) proxyPurposeOn(purpose string) bool {
	switch purpose {
	case "http":
		return m.cfg.Proxy.HTTPOn()
	case "tracker":
		return m.cfg.Proxy.TrackerOn()
	case "peer":
		return m.cfg.Proxy.PeerOn()
	case "webseed":
		return m.cfg.Proxy.WebSeedOn()
	}
	return true
}

func (m *Manager) transportFor(t *Task) *http.Transport {
	opts := m.proxyOptsFor(t, "http")
	key := opts.Mode + ":" + opts.Manual + ":" + opts.URL + "|" + strconv.FormatBool(opts.SkipTLSVerify)

	m.transportsMu.Lock()
	defer m.transportsMu.Unlock()
	if tr := m.transports[key]; tr != nil {
		return tr
	}
	tr := proxy.Transport(opts)
	m.transports[key] = tr
	return tr
}

// probeTransport 为链接预解析构建一次性传输层(不进缓存池):探测频率低,
// 独立构建避免污染任务下载的连接复用。
func (m *Manager) probeTransport(proxyOverride string) *http.Transport {
	opts := proxy.Options{SkipTLSVerify: m.cfg.SkipTLSVerify}
	switch {
	case proxyOverride == "none":
	case proxyOverride != "":
		opts.Manual = proxyOverride
	default:
		opts.Mode = m.cfg.Proxy.Mode
		opts.URL = m.cfg.Proxy.URL
	}
	return proxy.Transport(opts)
}

// cancelOf 安全读取任务的取消函数。
func (m *Manager) cancelOf(t *Task) context.CancelFunc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancel
}

// specOf 生成任务运行期不可变请求参数。
func (m *Manager) specOf(t *Task) reqSpec {
	t.mu.Lock()
	defer t.mu.Unlock()
	hdrs := make(map[string]string, len(t.Headers))
	for k, v := range t.Headers {
		hdrs[k] = v
	}
	s := reqSpec{ua: m.cfg.UserAgent, referer: t.Referer, headers: hdrs}
	if t.FinalURL != "" {
		s.url = t.FinalURL
	} else {
		s.url = t.URL
	}
	// If-Range:携带本运行探测到的校验器。资源在下载途中被替换时,
	// 服务器会对续传 Range 请求回 200 而非 206,引擎据此中止而不是拼接新旧内容
	if t.ETag != "" {
		s.ifRange = t.ETag
	} else if t.LastModified != "" {
		s.ifRange = t.LastModified
	}
	return s
}

func (m *Manager) newRequest(ctx context.Context, method, rawURL string, spec reqSpec) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", spec.ua)
	if spec.referer != "" {
		req.Header.Set("Referer", spec.referer)
	}
	if spec.ifRange != "" {
		req.Header.Set("If-Range", spec.ifRange)
	}
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// taskLimiter 是每任务限速的可变句柄:限速值可在任务运行中热更新,
// 读循环每轮取用当前实例,置 0 即解除限速。
type taskLimiter struct {
	mu  sync.Mutex
	lim *limiter.Limiter
}

func newTaskLimiter(rate int64) *taskLimiter {
	tl := &taskLimiter{}
	tl.set(rate)
	return tl
}

func (tl *taskLimiter) set(rate int64) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if rate <= 0 {
		tl.lim = nil // 不限速
		return
	}
	if tl.lim == nil {
		tl.lim = limiter.New(rate)
	} else {
		tl.lim.SetRate(rate)
	}
}

func (tl *taskLimiter) get() *limiter.Limiter {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.lim
}

// waitLimits 依次通过 每任务 与 全局 限速器。
func (m *Manager) waitLimits(ctx context.Context, tLim *taskLimiter, n int64) error {
	if lim := tLim.get(); lim != nil {
		if err := lim.Wait(ctx, n); err != nil {
			return err
		}
	}
	return m.gLim.Wait(ctx, n)
}

// refundLimits 把预购但未消费的令牌退还给两级限速器。
func (m *Manager) refundLimits(tLim *taskLimiter, n int64) {
	if lim := tLim.get(); lim != nil {
		lim.Refund(n)
	}
	m.gLim.Refund(n)
}

// readSizeFor 按有效限速计算建议读取块:限速越低块越小,输出越平稳;
// 不限速时保持 64KB 大块吞吐。
func (m *Manager) readSizeFor(tLim *taskLimiter) int64 {
	eff := int64(0)
	for _, v := range []int64{m.cfg.GlobalLimit, m.cfg.TaskLimit} {
		if v > 0 && (eff == 0 || v < eff) {
			eff = v
		}
	}
	if lim := tLim.get(); lim != nil {
		if r := lim.Rate(); r > 0 && (eff == 0 || r < eff) {
			eff = r
		}
	}
	if eff <= 0 {
		return readBufSize
	}
	n := eff / 8
	if n < 8<<10 {
		n = 8 << 10
	}
	if n > readBufSize {
		n = readBufSize
	}
	return n
}

func backoffFor(attempt int) time.Duration {
	d := 500 * time.Millisecond
	for i := 1; i < attempt && d < 8*time.Second; i++ {
		d *= 2
	}
	return d
}
