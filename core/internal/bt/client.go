package bt

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"zdownloader/internal/limiter"
)

// btDebug 环境变量 ZD_BT_DEBUG 非空时输出诊断日志(现场排查用,生产默认静默)。
func btDebug(format string, args ...any) {
	if os.Getenv("ZD_BT_DEBUG") != "" {
		log.Printf("[bt-dbg] "+format, args...)
	}
}

// ClientConfig 会话参数。
type ClientConfig struct {
	ListenPort int // 入站监听端口;0 = 随机可用
	// Hub 全局共享监听端口路由(引擎注入;nil = 本会话独立监听 ListenPort,
	// 测试与 Hub 建立失败时的降级路径)。注入后 ListenPort 被覆盖为 Hub
	// 的实际端口,入站连接由 Hub 按 infohash 路由交付。
	Hub *Hub
	// NameOverride 覆盖存储/改名使用的名称(任务层冲突改名后传入;空 = meta.Name)
	NameOverride string
	MaxPeers     int // 单任务最大连接 peer 数;0 = 60
	// DialConcurrency 并发拨号上限(候选地址同时在建连的数量);0 = 缺省。
	// 引擎按 peer 是否走代理注入:直连放大以快速穿透大量死地址,代理收紧。
	DialConcurrency int
	DownLimit       int64 // 下载限速 B/s(0 不限)
	UpLimit         int64 // 上传限速 B/s(0 不限)
	// GlobalDown/GlobalUp 引擎持有的共享全局限速器:下载全局与 HTTP 引擎
	// 共用同一只,上传全局为全部 BT 会话共用。会话自身 limiter 只描述任务
	// 级限速,全局预算在所有会话间统一仲裁(限速值变更由引擎对共享实例
	// SetRate 热更新,无需逐会话通知)。nil = 无全局约束(测试/降级)。
	GlobalDown *limiter.Limiter
	GlobalUp   *limiter.Limiter
	// DHT 共享 DHT 节点(可 nil):启用后周期 get_peers 发现 peer 并 announce_peer
	DHT *DHTServer
	// DHT6 IPv6 DHT 独立实例(BEP-32;可 nil)。有则与 DHT 并行查找/宣告。
	DHT6 *DHTServer
	// WantedPieces piece 需求位图(文件选择下载;nil = 全部)。
	// 排除的 piece 不认领、不校验、不对外声明持有;完成判定只看需要的 piece。
	WantedPieces []bool
	// FileWanted 文件级需求位图(按 meta.Files 序号;文件选择下载;nil = 全部)。
	// 决定存储预分配与进度展示口径:相邻文件共享边界 piece,按 WantedPieces
	// 推算会把未选的邻居一并预分配/误标需求(实测选 2 个文件 5 个文件被撑到
	// 全尺寸)。下载/校验/完成判定仍只看 WantedPieces。nil 时按 WantedPieces
	// 推算回退。
	FileWanted []bool
	// HTTPClient 用于 http/https tracker announce(引擎注入代理配置;
	// nil = 默认直连)。udp tracker 与 DHT 无法走 http 代理,始终直连。
	HTTPClient *http.Client
	// WebSeedHTTPClient 用于 WebSeed(BEP-19)拉取;nil 时沿用 HTTPClient 的传输层。
	WebSeedHTTPClient *http.Client
	// Dialer 用于 BT peer 原始 TCP 连接(引擎注入代理;nil = 直连)。
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// UDPRelay 可选的共享 UDP 中继(引擎按代理配置创建):udp tracker 经其发收。
	UDPRelay net.PacketConn
	// Resolver 自定义 DNS 解析(DoH,规避本地 DNS 污染);nil = 系统 DNS。
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
	// SpoofName 对外呈现的客户端名(扩展握手 v / tracker UA);空 = "BitComet 1.99"。
	// peer_id 前缀固定 -BC0199-(兼容性白名单),不受此名称影响。
	SpoofName string
	// AutoBanPoison 坏块归因自动封禁:piece 校验失败时对来源 peer 记打击,
	// 累计 PoisonStrikes 次自动拉黑。false = 仅记录来源不封禁。
	AutoBanPoison bool
	// PoisonRate 坏块白名单的有效率阈值(百分比):该 IP 参与过的 piece 中
	// 通过校验的比例 ≥ 该值才豁免自动封禁,低于即踢出白名单。0 = 默认 80,
	// 钳在 50..100(见 normPoisonRate)。
	PoisonRate int
	// PoisonStrikes 自动封禁的坏块宽容次数:同一 IP 累计供坏块达到该次数即
	// 拉黑(校验有效率跌破白名单阈值后才开始计)。0 = 默认 3,钳在 1..10
	// (见 normPoisonStrikes)。
	PoisonStrikes int
	// ClientBlacklist 客户端黑名单:通配符模式(* 任意长度、? 单字符,
	// 大小写不敏感)匹配对端客户端名(扩展握手 v 优先,退回 peer id 前缀
	// 推断),命中即写入全局封禁名单并断开——握手期直接拒绝,扩展握手
	// v 字段到齐后补核一次。空 = 不启用。
	ClientBlacklist []string
	// BanList 全局封禁名单(引擎注入,全部会话共用,跨任务/重启生效);
	// nil = 会话内私有名单(测试)。封禁广播断连所有注册会话的现存连接。
	BanList *BanList
}

// Client 是单个任务的 BT 会话:下载、校验、落盘、做种一体。
type Client struct {
	cfg    ClientConfig
	meta   *MetaInfo
	pid    [20]byte
	picker *PiecePicker

	// store 用原子指针:完成时从临时形态切换为只读的最终路径形态(做种)。
	store atomic.Pointer[Storage]

	dlLim, ulLim *limiter.Limiter
	httpClient   *http.Client

	trackers []string
	trkMu    sync.Mutex // 保护 trackers 列表(动态增删)
	started  atomic.Bool

	webSeeds []string // BEP-19 HTTP 种子源

	pMu     sync.Mutex
	peers   map[string]*peerConn
	dialing map[string]bool

	banList *BanList // 全局封禁名单(拨号/入站一律拒绝;nil 前由 newSession 兜底)

	candMu sync.Mutex
	cands  map[string]candInfo // 候选池:全部已知 peer(announce/DHT/PEX 累计),回补重拨的数据源

	ln      net.Listener
	hub     *Hub        // 共享监听端口路由(nil = 独立监听)
	utpm    *utpManager // uTP 传输(BEP-29):与 TCP 同端口 UDP;nil = 未启用
	stopCh  chan struct{}
	stopOnc sync.Once
	baseCtx context.Context
	baseCxl context.CancelFunc
	wg      sync.WaitGroup

	completeCh   chan struct{}
	completeOnce sync.Once
	failCh       chan error // 落定(Finalize)失败:WaitComplete 借此返回错误,否则引擎侧永久挂起
	completed    atomic.Bool
	finalPath    atomic.Pointer[string]

	// pieceMu 是按 piece 取模分片的锁:让"判重+落盘+登记"(onPiece)与
	// "校验+标记完成"(onPieceVerified)对同一 piece 互斥,杜绝迟到/重复块
	// 覆盖已校验 piece 的数据(见 onPiece 注释)。
	pieceMu [64]sync.Mutex

	// 坏块归因:piece → 各 peer 交付的块数(onPiece 记录,校验出结果即清),
	// 以及 peer 供坏块打击记分(累计 poisonStrikes 次自动封禁;piece 有效率
	// 达阈值(poisonRate)的 peer 进白名单,坏块只观察不自动封——uTP 接收
	// 瑕疵等我方原因也可能偶发坏块,不能拿大量供过好数据的正常 peer 补速度)。
	// AutoBanPoison 开关运行中可热切换(autoBan),无需重建会话。
	blameMu     sync.Mutex
	pieceBlame  map[int]map[string]int
	peerStrikes map[string]int
	// piece 有效率(按 IP 归口):参与并通过校验 / 参与但校验失败的 piece
	// 数,以及其中"独占来源"的失败数。既是连接详情展示的"掺假率",也是
	// 白名单判定的依据——校验通过才算好块,交付即计数等于给掺沙子放行。
	peerOKPieces  map[string]int
	peerBadPieces map[string]int
	peerSoleBad   map[string]int
	autoBan       atomic.Bool
	// poisonRate 白名单有效率阈值(百分比,atomic.Int32;设置保存热生效)
	poisonRate atomic.Int32
	// poisonStrikes 坏块宽容次数阈值(atomic.Int32;设置保存热生效):
	// 跌破白名单的 peer 累计供坏块达到该次数即自动封禁。
	poisonStrikes atomic.Int32
	// clientBL 客户端黑名单模式表(atomic.Pointer;设置保存热生效,热切换
	// 时同步扫描现存连接)。nil = 未配置。
	clientBL atomic.Pointer[[]string]

	// 缺 piece layers 的 v2 文件整文件延迟校验状态(见 verifyOrDefer):
	// 按文件累计各 piece 的 16K 块级节点哈希,集齐后重构 merkle 根对拍。
	dflMu  sync.Mutex
	dfiles map[int]*fileVerifyState

	upTotal, downTotal atomic.Int64
	utpPeers           atomic.Int32 // 经 uTP(BEP-29) 建连的 peer 数(诊断/测试)
	pexRecv            atomic.Int32 // PEX 发现的 peer 累计数(展示)

	kickCh chan struct{} // 完成后唤醒 announce 循环发送 completed

	kickChoker chan struct{} // 事件踢醒 choker(interested 变化/断开/完成)
	chokerSt   chokerState   // choker 轮换状态(仅 choker 协程访问)

	trMu     sync.Mutex
	trStates map[string]*trackerState

	errMu sync.Mutex
	errV  error

	// 边下边播:piece 落定广播(闭旧换新;等待方拿当前代 channel select,
	// 被唤醒后重查可用性)与流模式开关(状态展示用,优先级本体在 picker)。
	nmu      sync.Mutex
	nch      chan struct{}
	streamOn atomic.Bool
}

type trackerState struct {
	url         string
	lastErr     string
	lastOK      time.Time
	peers       int64
	recentPeers []string // 最近通告发现的 peer 地址(汇总去重用,上限 50)

	seeds    int64     // 通告响应的 complete(做种数)
	leechers int64     // 通告响应的 incomplete(下载中数)
	retries  int       // 连续失败次数(成功清零)
	nextAt   time.Time // 计划中的下次通告时刻(倒计时展示)
	msg      string    // 最近一次通告的结果描述(成功/失败一行)
}

// candInfo 是候选池中一条已知 peer 的记录。
type candInfo struct {
	addr        PeerAddr
	lastAttempt time.Time // 零值 = 从未尝试(立即可拨)
	seen        time.Time // 最近一次确认已知(发现/拨出/收包),历史导出排序用
	utp         bool      // PEX 声明支持 uTP(BEP-11 flags 0x04),拨号优先走 uTP
	seed        bool      // PEX 声明对端是种子(BEP-11 flags 0x02):补拨最优先
	src         string    // 来源:tracker/dht/pex/manual/history(展示用;空 = 未知)
	good        bool      // 本会话/历史中真正向我们供过数据(持久化与补拨优先)
}

// candRank 是候选补拨优先级:数值小者先拨。已证明能供数的最高;其次 PEX
// 明确声明为种子的(对端持有全部 piece,连上即可供数——用户观察到的
// "BitComet 的 peer 大多 100%"正对应这类);tracker/PEX/手动再次;DHT
// 广撒网拉来的地址命中率极低(实测多数为空 leecher),排到最后。
func candRank(ci candInfo) int {
	if ci.good {
		return 0
	}
	if ci.seed {
		return 1
	}
	switch ci.src {
	case "tracker", "pex", "manual":
		return 2
	case "history":
		return 3
	case "dht":
		return 5
	default:
		return 4
	}
}

// markCandGood 把候选标记为"供过数据",持久化 peer 历史时只保留这类地址
// (对照 BitComet 的"历史"来源:值得重启后直拨的是真正给过数据的邻居,
// 而不是 DHT 拉来的一次性空地址)。入站连接的对端未必在候选池里——它们
// 是经 tracker/DHT 找到我们的——此时按地址建档,否则供数 peer 会全部
// 漏出历史(实测 20+ 供数连接只留下 2 条)。
func (c *Client) markCandGood(addr string, utp bool) {
	c.candMu.Lock()
	ci, ok := c.cands[addr]
	if !ok {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			c.candMu.Unlock()
			return
		}
		pn, err := net.LookupPort("tcp", port)
		if err != nil {
			c.candMu.Unlock()
			return
		}
		ip := net.ParseIP(host)
		if ip == nil {
			c.candMu.Unlock()
			return
		}
		ci = candInfo{addr: PeerAddr{IP: ip, Port: uint16(pn)}, src: "connected"}
	}
	if !ci.good {
		ci.good = true
		ci.utp = ci.utp || utp
		ci.seen = time.Now()
	}
	c.cands[addr] = ci
	c.candMu.Unlock()
}

// TrackerState 是对外展示的 tracker 状态快照。
type TrackerState struct {
	URL         string   `json:"url"`
	LastErr     string   `json:"lastErr,omitempty"`
	LastOK      string   `json:"lastOk,omitempty"` // RFC3339,空 = 从未成功
	Peers       int64    `json:"peers"`
	RecentPeers []string `json:"recentPeers,omitempty"`
	// 扩展展示字段(Tracker 列表 tab)
	Seeds        int64  `json:"seeds"`                  // complete
	Leechers     int64  `json:"leechers"`               // incomplete
	Retries      int    `json:"retries"`                // 连续失败次数
	NextAnnounce string `json:"nextAnnounce,omitempty"` // RFC3339 下次通告时刻
	Message      string `json:"message,omitempty"`      // 最近一次结果描述
}

// PeerStat 是单个连接的展示快照(Peers 列表 tab)。
type PeerStat struct {
	Addr     string  `json:"addr"`
	PeerID   string  `json:"peerId,omitempty"` // 20 字节原文(可能不可打印)
	Client   string  `json:"client"`           // 由 peer id 前缀推断的客户端名
	Down     int64   `json:"down"`             // 本连接累计下载字节
	Up       int64   `json:"up"`               // 本连接累计上传字节
	Progress float64 `json:"progress"`         // 对端持有比例 0-1
	// 扩展展示字段
	DownRate     float64 `json:"downRate"`             // B/s(快照间采样)
	UpRate       float64 `json:"upRate"`               // B/s
	ListenPort   int64   `json:"listenPort,omitempty"` // 扩展握手 p 字段
	Inbound      bool    `json:"inbound"`              // true = 对端连入,false = 我方拨出
	UTP          bool    `json:"utp"`                  // 协议 TCP/uTP
	Encrypted    bool    `json:"encrypted"`            // MSE 加密中
	PEX          bool    `json:"pex"`                  // 对端声明支持 ut_pex
	Source       string  `json:"source,omitempty"`     // tracker/dht/pex/manual/incoming
	Bitfield     string  `json:"bitfield,omitempty"`   // 对端 piece 位图(base64,分块图)
	ConnectedFor float64 `json:"connectedFor"`         // 连接时长(秒)
	Flags        string  `json:"flags,omitempty"`      // D=在从我们下载 I=想要 C=choke着我
	// piece 有效率(连接详情新增列):该 IP 交付的块参与过的 piece 中,
	// 通过校验 / 校验失败(其中独占来源)的数量。一眼看出"掺沙子"的掺假率。
	PiecesOK  int `json:"piecesOK"`  // 参与并通过校验的 piece 数
	PiecesBad int `json:"piecesBad"` // 参与但校验失败的 piece 数
	SoleBad   int `json:"soleBad"`   // 其中"唯一来源"的失败数(铁证度更高)
}

// FileProg 是单个文件的完成进度。
type FileProg struct {
	Path []string `json:"path"`
	Size int64    `json:"size"`
	Done int64    `json:"done"`
	// First/Last 文件占用的 piece 序号区间(详情页"分块序号"列;
	// 零长文件 last < first 表示空区间)
	First  int  `json:"first"`
	Last   int  `json:"last"`
	Wanted bool `json:"wanted"` // 是否在文件选择内(未选 = 跳过不下载)
}

// Stats 是会话统计快照。
type Stats struct {
	Uploaded   int64
	Downloaded int64
	Peers      int
}

// NewClient 创建下载模式会话。initialHave 为续传恢复的 piece 位图(可为 nil),
// 其中每个标记的 piece 都会重新校验,防止断电写坏的数据静默通过。
func NewClient(meta *MetaInfo, downloadDir string, cfg ClientConfig, initialHave []bool) (*Client, error) {
	if meta == nil {
		return nil, errors.New("bt: meta 为空")
	}
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = 60
	}
	stMeta := meta
	if cfg.NameOverride != "" && cfg.NameOverride != meta.Name {
		stMeta = meta.NamedCopy(cfg.NameOverride) // 仅存储路径改名;层哈希数组共享,锁独立
	}
	have := initialHave
	if have != nil && len(have) != meta.PieceCount() {
		return nil, errors.New("bt: 续传位图长度不符")
	}
	if have == nil {
		have = make([]bool, meta.PieceCount())
	}
	scanPart := partExistsFull(stMeta, downloadDir)
	// 预分配口径优先用文件级精确选择(cfg.FileWanted):按 piece 区间推算会
	// 把共享边界 piece 的相邻文件也撑到全尺寸(稀疏但逻辑大小吓人,实测
	// 选 2 个文件文件夹里 5 个文件都显示数百 MB)。未选文件由跨界 piece
	// 的块写入按需惰性创建,只长出真实需要的边界字节。
	fw := cfg.FileWanted
	if fw == nil {
		fw = fileWantedFor(meta, cfg.WantedPieces)
	}
	st, err := OpenStorage(stMeta, downloadDir, fw, have)
	if err != nil {
		return nil, err
	}
	verified := make([]bool, meta.PieceCount())
	for i, h := range have {
		if !h {
			continue
		}
		// 文件选择排除的 piece 数据不存在,跳过校验直接保留
		if cfg.WantedPieces != nil && !cfg.WantedPieces[i] {
			verified[i] = true
			continue
		}
		if st.VerifyPiece(i) {
			verified[i] = true
		}
	}
	if scanPart {
		// 恢复扫描:重哈希位图之外的需求 piece。块数据早已落盘但 piece
		// 未及校验落定就崩溃/位图保存滞后时,直接恢复,避免整文件重下
		// (否则进度会严重落后于盘上数据,任务表现为"下完了还在下")。
		for i := range verified {
			if verified[i] {
				continue
			}
			if cfg.WantedPieces != nil && !cfg.WantedPieces[i] {
				continue
			}
			if st.VerifyPiece(i) {
				verified[i] = true
			}
		}
	}
	return newSession(meta, st, cfg, verified)
}

// partExistsFull 报告就地落盘数据已存在(任务曾写过数据),用于决定是否
// 执行恢复重哈希扫描:多文件看数据目录,单文件看正式名或 .!zd 形态。
func partExistsFull(stMeta *MetaInfo, downloadDir string) bool {
	if stMeta.Multi {
		fi, err := os.Stat(filepath.Join(downloadDir, stMeta.Name))
		return err == nil && fi.IsDir()
	}
	name := filepath.Join(downloadDir, stMeta.Name)
	if fi, err := os.Stat(name); err == nil && !fi.IsDir() {
		return true
	}
	_, err := os.Stat(name + IncompleteSuffix)
	return err == nil
}

// fileWantedFor 依据 piece 需求位图推算文件级需求(存储预分配跳过不需要的文件)。
func fileWantedFor(meta *MetaInfo, wanted []bool) []bool {
	if wanted == nil {
		return nil
	}
	out := make([]bool, len(meta.Files))
	for i := range meta.Files {
		first, last := meta.FilePieceRange(i)
		for pi := first; pi <= last && pi < len(wanted); pi++ {
			if wanted[pi] {
				out[i] = true
				break
			}
		}
	}
	return out
}

// NewSeedClient 创建做种模式会话:数据已在最终路径,只做上传服务。
// finalPath 为 Finalize 返回的落定路径(单文件文件 / 多文件目录)。
func NewSeedClient(meta *MetaInfo, finalPath string, cfg ClientConfig) (*Client, error) {
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = 60
	}
	st, err := openSeedingStorage(meta, finalPath)
	if err != nil {
		return nil, err
	}
	have := make([]bool, meta.PieceCount())
	for i := range have {
		have[i] = true
	}
	c, err := newSession(meta, st, cfg, have)
	if err != nil {
		return nil, err
	}
	c.completed.Store(true)
	p := finalPath
	c.finalPath.Store(&p)
	c.completeOnce.Do(func() { close(c.completeCh) })
	return c, nil
}

func newSession(meta *MetaInfo, st *Storage, cfg ClientConfig, initialHave []bool) (*Client, error) {
	baseCtx, cancel := context.WithCancel(context.Background())
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	c := &Client{
		cfg:        cfg,
		meta:       meta,
		pid:        PeerID(),
		picker:     NewPicker(meta.PieceCount(), meta.PieceLength, meta.TotalLength, meta.PieceLen, initialHave, cfg.WantedPieces),
		httpClient: hc,
		peers:      map[string]*peerConn{},
		dialing:    map[string]bool{},
		cands:      map[string]candInfo{},
		stopCh:     make(chan struct{}),
		baseCtx:    baseCtx,
		baseCxl:    cancel,
		completeCh: make(chan struct{}),
		failCh:     make(chan error, 1),
		kickCh:     make(chan struct{}, 1),
		kickChoker: make(chan struct{}, 1),
		trStates:   map[string]*trackerState{},
	}
	c.store.Store(st)
	c.dlLim = limiter.New(cfg.DownLimit)
	c.ulLim = limiter.New(cfg.UpLimit)
	c.pieceBlame = map[int]map[string]int{}
	c.peerStrikes = map[string]int{}
	c.peerOKPieces = map[string]int{}
	c.peerBadPieces = map[string]int{}
	c.peerSoleBad = map[string]int{}
	c.poisonRate.Store(int32(normPoisonRate(cfg.PoisonRate)))
	c.poisonStrikes.Store(int32(normPoisonStrikes(cfg.PoisonStrikes)))
	c.autoBan.Store(cfg.AutoBanPoison)
	c.clientBL.Store(blPtr(cfg.ClientBlacklist))
	c.banList = cfg.BanList
	if c.banList == nil {
		c.banList = NewBanList("") // 未注入全局名单时退化为会话内私有表
	}

	// tracker 列表:内置 announce/announce-list + 用户追加,保序去重
	seen := map[string]bool{}
	add := func(u string) {
		if u = strings.TrimSpace(u); u != "" && !seen[u] {
			seen[u] = true
			c.trackers = append(c.trackers, u)
		}
	}
	add(meta.Announce)
	for _, tier := range meta.AnnounceList {
		for _, u := range tier {
			add(u)
		}
	}
	// 空种子(0 piece)直接视为完成
	if meta.PieceCount() == 0 {
		c.finishDownload()
	}
	return c, nil
}

// AddTrackers 追加 tracker(去重),并对新地址立即启动 announce 循环。
// 需在 Start 之前调用(运行中追加用 AddTracker)。
func (c *Client) AddTrackers(urls []string) {
	c.trkMu.Lock()
	defer c.trkMu.Unlock()
	seen := map[string]bool{}
	for _, u := range c.trackers {
		seen[TrackerKey(u)] = true
	}
	for _, u := range urls {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if k := TrackerKey(u); !seen[k] {
			seen[k] = true
			c.trackers = append(c.trackers, u)
		}
	}
}

// DedupeTrackers 去重:同 host:port(不同协议 scheme)视为重复,
// 每组保留"最近通告成功"的一个,无成功记录时保留先添加的。
// 被删条目逐个走 RemoveTracker(停止通告并清理状态),返回删除列表。
func (c *Client) DedupeTrackers() []string {
	c.trkMu.Lock()
	type entry struct {
		url   string
		lastO time.Time
	}
	groups := map[string][]entry{}
	var order []string // 首次出现的 key 顺序(结果稳定)
	for _, u := range c.trackers {
		k := TrackerKey(u)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		e := entry{url: u}
		c.trMu.Lock()
		if st, ok := c.trStates[u]; ok {
			e.lastO = st.lastOK
		}
		c.trMu.Unlock()
		groups[k] = append(groups[k], e)
	}
	var remove []string
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		keep := 0
		for i, e := range g {
			if e.lastO.After(g[keep].lastO) {
				keep = i
			}
		}
		for i, e := range g {
			if i != keep {
				remove = append(remove, e.url)
			}
		}
	}
	c.trkMu.Unlock()
	for _, u := range remove {
		c.RemoveTracker(u)
	}
	return remove
}

// Start 启动监听与 announce 循环。重复调用无效果。
func (c *Client) Start() {
	if c.started.Load() {
		return
	}
	c.started.Store(true)
	c.banList.attach(c) // 注册全局封禁广播:名单变动时立即断连本会话
	if h := c.cfg.Hub; h != nil {
		// 共享监听端口:入站(TCP/uTP)由 Hub 按 infohash 路由交付;
		// 本会话不监听,只注册。utpm 借用 Hub 的(出站 uTP 拨号用),
		// shared 标记保证 Stop 不会关掉 Hub 持有的 socket。
		c.hub = h
		h.Add(c)
		c.cfg.ListenPort = h.Port()
		if h.utpm != nil {
			c.utpm = h.utpm
		}
		log.Printf("[bt] 会话接入共享监听端口 %d", h.Port())
	} else {
		c.startOwnListener()
	}
	for _, u := range c.trackerList() {
		c.wg.Add(1)
		go c.announceLoop(u)
	}
	c.wg.Add(1)
	go c.reconnectLoop()
	c.wg.Add(1)
	go c.chokerLoop()
	c.wg.Add(1)
	go c.evictLoop()
	if len(c.webSeeds) > 0 {
		c.StartWebSeedLoops()
	}
	if c.cfg.DHT != nil {
		c.wg.Add(1)
		go c.dhtLoop()
	}
}

// startOwnListener 独立监听路径(Hub 未注入时的降级/测试形态):自绑
// TCP 端口与 uTP UDP(端口被共享 DHT 占用时回退 BEP-29 §8.1 同端口复用,
// 由 DHT 读循环分发 uTP 包)。
func (c *Client) startOwnListener() {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", c.cfg.ListenPort))
	if err != nil {
		// 端口被占/无入站能力不致命:仍可出站下载
		ln = nil
	} else {
		c.ln = ln
		if addr, ok := ln.Addr().(*net.TCPAddr); ok {
			c.cfg.ListenPort = addr.Port
		}
		go c.acceptLoop()
	}
	// uTP(BEP-29):与 TCP 监听同端口的 UDP。首选自建 socket;端口已被
	// 共享 DHT 占用时(首个任务的监听端口 = DHT UDP 端口)回退 BEP-29 §8.1
	// 同端口复用模式,由 DHT 读循环分发 uTP 包。都不可用则无 uTP。
	if m, err := newUTPManagerOwn(c.cfg.ListenPort); err == nil {
		c.utpm = m
		c.wg.Add(1)
		go c.utpAcceptLoop()
		log.Printf("[bt] uTP 监听 udp:%d", c.cfg.ListenPort)
	} else if d := c.cfg.DHT; d != nil && d.utpm != nil {
		c.utpm = d.utpm
		c.wg.Add(1)
		go c.utpAcceptLoop()
		log.Printf("[bt] uTP 复用 DHT 端口 udp:%d(同端口模式)", d.Port())
	} else {
		log.Printf("[bt] uTP 不可用: udp:%d bind 失败(%v)且无共享 DHT socket", c.cfg.ListenPort, err)
	}
}

// StartWebSeedLoops 为每个 HTTP 种子源起一个拉取协程。
func (c *Client) StartWebSeedLoops() {
	for _, u := range c.webSeeds {
		c.wg.Add(1)
		go c.webSeedLoop(u)
	}
}

// trackerList 快照 tracker 列表。
func (c *Client) trackerList() []string {
	c.trkMu.Lock()
	defer c.trkMu.Unlock()
	return append([]string(nil), c.trackers...)
}

func (c *Client) hasTracker(url string) bool {
	c.trkMu.Lock()
	defer c.trkMu.Unlock()
	for _, u := range c.trackers {
		if u == url {
			return true
		}
	}
	return false
}

// AddTracker 追加 tracker(去重);会话已启动时立即开始 announce。
// TrackerKey 返回 tracker 的等价键(同 host:port 视为同一服务器):
// http:// 与 udp:// 指向同 host:port 时是重复条目,去重用。
// 无显式端口的按协议默认端口补齐(http 80 / https 443)。
func TrackerKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	host := strings.ToLower(u.Host)
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		switch strings.ToLower(u.Scheme) {
		case "http":
			host += ":80"
		case "https":
			host += ":443"
		}
	}
	return host
}

func (c *Client) AddTracker(url string) bool {
	url = strings.TrimSpace(url)
	if url == "" {
		return false
	}
	c.trkMu.Lock()
	key := TrackerKey(url)
	for _, u := range c.trackers {
		if TrackerKey(u) == key {
			c.trkMu.Unlock()
			return false
		}
	}
	c.trackers = append(c.trackers, url)
	c.trkMu.Unlock()
	if c.started.Load() && !c.stopped() {
		c.wg.Add(1)
		go c.announceLoop(url)
	}
	return true
}

// RemoveTracker 移除 tracker:其 announce 循环在下一周期退出,
// 并尽力向该 tracker 发送 stopped。
func (c *Client) RemoveTracker(url string) bool {
	url = strings.TrimSpace(url)
	c.trkMu.Lock()
	found := false
	kept := c.trackers[:0]
	for _, u := range c.trackers {
		if u == url {
			found = true
		} else {
			kept = append(kept, u)
		}
	}
	c.trackers = kept
	c.trkMu.Unlock()
	if !found {
		return false
	}
	c.trMu.Lock()
	delete(c.trStates, url)
	c.trMu.Unlock()
	if !c.stopped() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := AnnounceReq{
				InfoHash: c.meta.InfoHash, PeerID: c.pid, Port: uint16(c.cfg.ListenPort),
				Uploaded: c.upTotal.Load(), Downloaded: c.downTotal.Load(),
				Left: c.left(), Event: EventStopped,
			}
			c.announceOnce(ctx, url, req)
		}()
	}
	return true
}

// Reannounce 唤醒全部 announce 循环立即重新通告。
func (c *Client) Reannounce() {
	select {
	case c.kickCh <- struct{}{}:
	default:
	}
}

// dhtLoop 周期执行 DHT 查找:发现的 peer 交给 AddPeer,同时宣告本机端口
// (下载与做种都宣告——做种宣告是 trackerless swarm 能找到我们的唯一途径)。
func (c *Client) dhtLoop() {
	defer c.wg.Done()
	first := true
	for {
		isFirst := first
		if first {
			first = false
		} else {
			select {
			case <-c.stopCh:
				return
			case <-time.After(90 * time.Second):
			}
		}
		if c.stopped() {
			return
		}
		ctx, cancel := context.WithTimeout(c.baseCtx, 60*time.Second)
		n := c.cfg.DHT.Lookup(ctx, c.meta.InfoHash, c.cfg.ListenPort, func(p PeerAddr) {
			c.AddPeerSrc(p, "dht")
		})
		// IPv6 DHT 并行查找/宣告(BEP-32);v4 主实例挂了也能靠它继续
		var n6 int
		var nodes6 int
		if c.cfg.DHT6 != nil {
			n6 = c.cfg.DHT6.Lookup(ctx, c.meta.InfoHash, c.cfg.ListenPort, func(p PeerAddr) {
				c.AddPeerSrc(p, "dht")
			})
			nodes6 = c.cfg.DHT6.NodeCount()
		}
		cancel()
		n += n6
		// 首轮无论结果都记日志(观察冷启动),之后只在有收获时记
		if n > 0 || isFirst {
			log.Printf("[bt] DHT 查找 %d 个 peer(v6 %d,路由表 %d+%d 节点)",
				n, n6, c.cfg.DHT.NodeCount(), nodes6)
		}
	}
}

func (c *Client) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// AddWebSeeds 追加 BEP-19 HTTP 种子源;Start 时开始整 piece 拉取。
func (c *Client) AddWebSeeds(urls []string) {
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			c.webSeeds = append(c.webSeeds, u)
		}
	}
}

// webSeedSeg 是 piece 在某个文件内的一段(URL + 文件内区间)。
type webSeedSeg struct {
	url        string
	start, end int64 // 文件内 [start,end)
}

// pieceFileSegments 把 piece 映射到它跨越的每个文件区段(BEP-19 URL 规则)。
// piece 是磁盘上连续的字节区间,HTTP 语义下需按文件逐段拉取后拼接。
func (c *Client) pieceFileSegments(seedURL string, piece int) ([]webSeedSeg, int64, bool) {
	pstart := c.meta.PieceOffset(piece)
	pend := min(pstart+c.meta.PieceLen(piece), c.meta.TotalLength)
	if pstart >= pend {
		return nil, 0, false
	}
	mkURL := func(rel []string) string {
		parts := strings.Split(c.meta.Name+"/"+strings.Join(rel, "/"), "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		return seedURL + "/" + strings.Join(parts, "/")
	}
	var segs []webSeedSeg
	if len(c.meta.Files) == 0 {
		segs = append(segs, webSeedSeg{url: mkURL(nil), start: pstart, end: pend})
		return segs, pend - pstart, true
	}
	for _, f := range c.meta.Files {
		fend := f.Offset + f.Length
		lo, hi := max(pstart, f.Offset), min(pend, fend)
		if hi <= lo {
			continue
		}
		segs = append(segs, webSeedSeg{url: mkURL(f.Path), start: lo - f.Offset, end: hi - f.Offset})
	}
	if len(segs) == 0 {
		return nil, 0, false
	}
	return segs, pend - pstart, true
}

// webSeedLoop 单个 HTTP 种子源:预留完整 piece → Range 拉取 → 写盘校验落定。
func (c *Client) webSeedLoop(seedURL string) {
	defer c.wg.Done()
	hc := &http.Client{Timeout: 90 * time.Second}
	// WebSeed 代理出口独立配置;未配置时沿用 tracker 的传输层
	switch {
	case c.cfg.WebSeedHTTPClient != nil:
		hc.Transport = c.cfg.WebSeedHTTPClient.Transport
	case c.cfg.HTTPClient != nil:
		hc.Transport = c.cfg.HTTPClient.Transport
	}
	for {
		if c.completed.Load() || c.stopped() {
			return
		}
		idx, ok := c.picker.ReserveWholePiece(nil)
		if !ok {
			select {
			case <-c.stopCh:
				return
			case <-c.completeCh:
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if !c.fetchWebSeedPiece(hc, seedURL, idx) {
			c.picker.ResetPiece(idx)
			select {
			case <-c.stopCh:
				return
			case <-time.After(10 * time.Second): // 失败退避
			}
			continue
		}
		c.onWholePiece(idx)
	}
}

// fetchWebSeedPiece 按文件区段 Range 拉取一个 piece 的完整数据并写入存储。
func (c *Client) fetchWebSeedPiece(hc *http.Client, seedURL string, idx int) bool {
	segs, total, ok := c.pieceFileSegments(seedURL, idx)
	if !ok || total <= 0 {
		return false
	}
	buf := make([]byte, 0, total)
	for _, seg := range segs {
		ctx, cancel := context.WithTimeout(c.baseCtx, 90*time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", seg.url, nil)
		if err != nil {
			cancel()
			return false
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.start, seg.end-1))
		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			return false
		}
		// 200 全量响应仅在区段起点为 0 时可定位,206 任意区间皆可
		if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == http.StatusOK && seg.start == 0) {
			resp.Body.Close()
			cancel()
			return false
		}
		want := seg.end - seg.start
		body, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
		resp.Body.Close()
		cancel()
		if err != nil || int64(len(body)) != want {
			return false
		}
		buf = append(buf, body...)
	}
	if err := c.waitDL(c.baseCtx, total); err != nil {
		return false
	}
	st := c.store.Load()
	if st == nil {
		return false
	}
	// 整 piece 写入:块内偏移为 0
	if err := st.WriteBlock(idx, 0, buf); err != nil {
		return false
	}
	c.downTotal.Add(total)
	return true
}

// fileVerifyState 一个缺 layers 的 v2 文件的延迟校验累计状态。
type fileVerifyState struct {
	base  int // 该文件首个 piece 的全局下标
	nodes [][32]byte
	got   []bool
}

// verifyOrDefer 校验一个全块到货的 piece。v2 且该文件层哈希未知(磁力经
// ut_metadata 只拿到 info dict,BEP-52 规定顶层 piece layers 不在 info 内)时
// 无法逐片校验:改为把数据落盘、累计该 piece 的块级节点哈希,等整个文件到齐
// 后重构 16K 块级 merkle 根与文件 pieces root 对拍——通过则一次性回填该文件的
// 层哈希并落定其全部 piece(此后逐片校验/上传/做种恢复正常),失败则整文件
// 打回重下(无层信息无法定位坏片)。
// 返回 (ok, parked):ok=true 可落定;parked=true 表示已暂存(全块已到但文件
// 未集齐,picker 会保持该 piece 停泊,不落定也不重下)。
func (c *Client) verifyOrDefer(idx int) (ok, parked bool) {
	st := c.store.Load()
	if st == nil {
		return false, false
	}
	fi := c.meta.PieceFile(idx)
	if fi < 0 || c.meta.LayerKnown(fi) {
		return st.VerifyPiece(idx), false
	}
	node, done := st.CommitPieceNode(idx)
	if !done {
		return false, false
	}
	first, last := c.meta.FilePieceRange(fi)
	n := last - first + 1
	if n <= 0 {
		return false, false
	}
	c.dflMu.Lock()
	if c.dfiles == nil {
		c.dfiles = map[int]*fileVerifyState{}
	}
	fl := c.dfiles[fi]
	if fl == nil {
		fl = &fileVerifyState{base: first, nodes: make([][32]byte, n), got: make([]bool, n)}
		c.dfiles[fi] = fl
	}
	if k := idx - fl.base; k >= 0 && k < n && !fl.got[k] {
		fl.got[k] = true
		fl.nodes[k] = node
	}
	complete := true
	for _, g := range fl.got {
		if !g {
			complete = false
			break
		}
	}
	c.dflMu.Unlock()
	if !complete {
		return false, true
	}
	nBlocks := int((c.meta.Files[fi].Length + v2BlockSize - 1) / v2BlockSize)
	if v2RootFromPieces(fl.nodes, int(c.meta.PieceLength/v2BlockSize), nBlocks) == c.meta.FileRoot(fi) {
		c.meta.SetFileLayer(fi, fl.nodes)
		pieces := make([]int, 0, last-first+1)
		for p := first; p <= last; p++ {
			pieces = append(pieces, p)
			c.picker.FinalizePiece(p)
			c.broadcastHave(p)
			c.cancelPiece(p)
		}
		c.pieceDoneNotify() // v2 整文件落定批量唤醒(流媒体 v2 暂不开放,保持口径一致)
		// 逐文件完成记账:数据早已落盘,这里统一补记,集齐的文件摘 .!zd 后缀
		if st := c.store.Load(); st != nil {
			st.MarkPiecesVerified(pieces...)
		}
		log.Printf("[bt] v2 文件 %s 整文件校验通过,反推 piece layers(%d 片)", strings.Join(c.meta.Files[fi].Path, "/"), n)
		return true, false
	}
	for p := first; p <= last; p++ {
		c.picker.ResetPiece(p)
	}
	c.dflMu.Lock()
	delete(c.dfiles, fi)
	c.dflMu.Unlock()
	log.Printf("[bt] v2 文件 %s 整文件校验失败,整文件重下(缺 piece layers,无法定位坏片)", strings.Join(c.meta.Files[fi].Path, "/"))
	return false, false
}

// onWholePiece 整 piece 拉取完成:校验、落定、广播;全部完成时收尾。
func (c *Client) onWholePiece(idx int) {
	if c.store.Load() == nil {
		c.picker.ResetPiece(idx)
		return
	}
	ok, parked := c.verifyOrDefer(idx)
	if parked {
		return
	}
	if !ok {
		c.picker.ResetPiece(idx)
		return
	}
	c.picker.ResolveWholePiece(idx)
	c.broadcastHave(idx)
	c.cancelPiece(idx)
	c.pieceDoneNotify() // WebSeed 整片落定同样唤醒流媒体等待方
	if c.picker.AllDone() {
		c.finishDownload()
	}
}

// ListenAddr 返回入站监听地址(未监听返回空)。Hub 模式无自建 socket,
// 返回共享端口的本机回环形态(本机互连/测试拨号用;对外端口口径以
// ListenPort 为准)。
func (c *Client) ListenAddr() string {
	if c.ln != nil {
		return c.ln.Addr().String()
	}
	if c.hub != nil {
		return fmt.Sprintf("127.0.0.1:%d", c.hub.Port())
	}
	return ""
}

// ListenPort 返回会话监听端口(Start 后为实际绑定值,NAT 映射按此对账)。
func (c *Client) ListenPort() int {
	return c.cfg.ListenPort
}

func (c *Client) acceptLoop() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		tuneConn(conn)
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.acceptOne(conn)
		}()
	}
}

func (c *Client) acceptOne(conn net.Conn) {
	if ra, ok := conn.RemoteAddr().(*net.TCPAddr); ok && c.isBannedIP(ra.IP.String()) {
		conn.Close()
		return
	}
	hs, wire, enc := c.inboundHandshake(conn)
	if hs == nil {
		return
	}
	addr := wire.RemoteAddr().String()
	if pc := c.register(addr, hs, wire); pc != nil {
		pc.inbound = true
		pc.src = "incoming"
		log.Printf("[bt] 入站 peer %s (client=%q enc=%v)", addr, hs.PeerID, enc)
		pc.run()
	}
}

// inboundHandshake 完成入站嗅探、(可选)MSE 协商与 BT 握手回写,TCP 与
// uTP 入站共用。失败返回 (nil,nil,false) 并已关闭连接。
func (c *Client) inboundHandshake(conn net.Conn) (*Handshake, net.Conn, bool) {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	// 封锁名单:TCP/uTP 入站握手前统一拦截(TCP 已在 acceptOne 拦过一次)
	if ra, ok := conn.RemoteAddr().(*net.UDPAddr); ok && c.isBannedIP(ra.IP.String()) {
		conn.Close()
		return nil, nil, false
	}
	// 入站嗅探:明文 BT 握手(裸 0x13 前缀)直接续读;否则按 MSE 步1 处理。
	pre, plain, err := sniffHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, nil, false
	}
	var wire net.Conn
	if plain {
		wire = &mseConn{Conn: conn, pre: pre}
	} else {
		wire, err = mseAccept(conn, pre, c.meta.InfoHash)
		if err != nil {
			conn.Close()
			return nil, nil, false
		}
	}
	hs, err := ReadHandshake(wire)
	if err != nil {
		wire.Close()
		return nil, nil, false
	}
	if hs.InfoHash != c.meta.InfoHash || hs.PeerID == c.pid {
		wire.Close()
		return nil, nil, false
	}
	if err := WriteHandshake(wire, Handshake{InfoHash: c.meta.InfoHash, PeerID: c.pid}, true, true, true); err != nil {
		wire.Close()
		return nil, nil, false
	}
	conn.SetDeadline(time.Time{})
	return hs, wire, !plain
}

// deliverInbound Hub 路由完成的入站交付:嗅探/解密协商/读握手已在 Hub
// 侧做完(hs.InfoHash 已对准本会话),这里补封禁校验、握手回写与 peer
// 注册。交付成功后连接归本会话;停止中的会话一律拒收。
func (c *Client) deliverInbound(wire net.Conn, hs *Handshake, enc, utp bool) {
	if c.stopped() || hs.PeerID == c.pid {
		wire.Close()
		return
	}
	if host, _, err := net.SplitHostPort(wire.RemoteAddr().String()); err == nil && c.isBannedIP(host) {
		wire.Close()
		return
	}
	if err := WriteHandshake(wire, Handshake{InfoHash: c.meta.InfoHash, PeerID: c.pid}, true, true, true); err != nil {
		wire.Close()
		return
	}
	wire.SetDeadline(time.Time{})
	addr := wire.RemoteAddr().String()
	if pc := c.register(addr, hs, wire); pc != nil {
		pc.inbound = true
		pc.src = "incoming"
		tag := ""
		if utp {
			pc.utp = true
			c.utpPeers.Add(1)
			tag = " utp"
		}
		log.Printf("[bt] 入站 peer %s (client=%q enc=%v%s)", addr, hs.PeerID, enc, tag)
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			pc.run()
		}()
	} else {
		wire.Close()
	}
}

// utpAcceptLoop 接受 uTP 入站连接(与 TCP acceptOne 同一握手流程)。
func (c *Client) utpAcceptLoop() {
	defer c.wg.Done()
	for {
		ctx, cancel := context.WithCancel(c.baseCtx)
		conn, err := c.utpm.Accept(ctx)
		cancel()
		if err != nil {
			return
		}
		c.wg.Add(1)
		go func(conn net.Conn) {
			defer c.wg.Done()
			hs, wire, enc := c.inboundHandshake(conn)
			if hs == nil {
				return
			}
			addr := wire.RemoteAddr().String()
			if pc := c.register(addr, hs, wire); pc != nil {
				pc.utp = true
				pc.inbound = true
				pc.src = "incoming"
				c.utpPeers.Add(1)
				log.Printf("[bt] 入站 peer %s (client=%q enc=%v utp)", addr, hs.PeerID, enc)
				pc.run()
			}
		}(conn)
	}
}

// AddPeer 登记一个候选 peer(进候选池供回补重拨)并立即异步拨号。
func (c *Client) AddPeer(p PeerAddr) {
	c.AddPeerSrc(p, "")
}

// AddPeerSrc 同 AddPeer 并记录来源(tracker/dht/pex/manual,展示用)。
func (c *Client) AddPeerSrc(p PeerAddr, src string) {
	if c.completed.Load() || p.IP == nil || p.Port == 0 {
		return
	}
	c.rememberCandUTP(p, false, src)
	c.tryDial(p)
}

// AddPeerUTP 登记 PEX 声明支持 uTP 的候选(flags 0x04):优先走 uTP 拨号。
func (c *Client) AddPeerUTP(p PeerAddr) {
	if c.completed.Load() || p.IP == nil || p.Port == 0 {
		return
	}
	c.pexRecv.Add(int32(1)) // PEX 发现的 peer 计数(虚拟行展示)
	c.rememberCandFull(p, true, false, "pex")
	c.tryDial(p)
}

// AddPeerPEX 登记 PEX 候选并按 BEP-11 added.f 记录能力位:
// 0x02 = 对端是种子(持有全部 piece),0x04 = 支持 uTP。种子标记让该地址
// 在补拨排序里优先——swarm 里真正能立刻供数的正是这些 100% 的 peer。
func (c *Client) AddPeerPEX(p PeerAddr, flags byte) {
	if c.completed.Load() || p.IP == nil || p.Port == 0 {
		return
	}
	c.pexRecv.Add(int32(1))
	c.rememberCandFull(p, flags&0x04 != 0, flags&0x02 != 0, "pex")
	c.tryDial(p)
}

// rememberCand 把已知 peer 记入候选池(池是回补重拨的数据源,上限 1024)。
func (c *Client) rememberCand(p PeerAddr) {
	c.rememberCandUTP(p, false, "")
}

func (c *Client) rememberCandUTP(p PeerAddr, utp bool, src string) {
	c.rememberCandFull(p, utp, false, src)
}

func (c *Client) rememberCandFull(p PeerAddr, utp, seed bool, src string) {
	c.candMu.Lock()
	if ci, ok := c.cands[p.String()]; ok {
		now := time.Now()
		if utp && !ci.utp { // 已知候选补标记(不回退)
			ci.utp = true
		}
		if seed && !ci.seed {
			ci.seed = true
		}
		if ci.src == "" && src != "" { // 补来源(首个非空来源优先)
			ci.src = src
		}
		ci.seen = now
		c.cands[p.String()] = ci
		c.candMu.Unlock()
		return
	}
	if len(c.cands) >= 1024 {
		var oldest string
		var oldT time.Time
		for a, ci := range c.cands {
			if oldest == "" || ci.seen.Before(oldT) {
				oldest, oldT = a, ci.seen
			}
		}
		if oldest != "" {
			delete(c.cands, oldest)
		}
	}
	c.cands[p.String()] = candInfo{addr: p, seen: time.Now(), utp: utp, seed: seed, src: src}
	c.candMu.Unlock()
}

// PeerHistEntry 是持久化的 peer 历史记录(对照 BitComet"历史"来源:
// 真正给我们供过数据的邻居,重启后直接重拨)。
type PeerHistEntry struct {
	Addr string `json:"addr"`          // "ip:port"
	UTP  bool   `json:"utp,omitempty"` // 支持 uTP
	Src  string `json:"src,omitempty"` // 原始来源(展示用)
	Seen int64  `json:"seen"`          // 最近确认已知,unix 秒
}

// peerHistCap 持久化历史上限(按最近确认排序截断)。
const peerHistCap = 500

// PeerHistory 导出候选池快照(按最近确认排序,截断到上限)。
// 只导出真正供过数据的候选以及用户手动添加的地址:空转/DHT 一次性地址
// 不落盘,否则重启后会花大量拨号预算去重试一堆永远连不上的死地址。
func (c *Client) PeerHistory() []PeerHistEntry {
	c.candMu.Lock()
	defer c.candMu.Unlock()
	out := make([]PeerHistEntry, 0, len(c.cands))
	for addr, ci := range c.cands {
		if !ci.good && ci.src != "manual" {
			continue
		}
		var seen int64
		if !ci.seen.IsZero() {
			seen = ci.seen.Unix()
		}
		out = append(out, PeerHistEntry{Addr: addr, UTP: ci.utp, Src: ci.src, Seen: seen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seen > out[j].Seen })
	if len(out) > peerHistCap {
		out = out[:peerHistCap]
	}
	return out
}

// LoadPeerHistory 灌入上次会话的 peer 历史:仅补池中缺失项,来源标记为
// history(展示"历史"),不触发立即拨号——由回补循环按最久未试顺序消化。
func (c *Client) LoadPeerHistory(entries []PeerHistEntry) int {
	n := 0
	c.candMu.Lock()
	for _, e := range entries {
		if e.Addr == "" || e.Seen <= 0 {
			continue
		}
		host, portStr, err := net.SplitHostPort(e.Addr)
		if err != nil {
			continue
		}
		pn, err := net.LookupPort("tcp", portStr)
		if err != nil || pn <= 0 {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		if _, ok := c.cands[e.Addr]; ok {
			continue
		}
		if len(c.cands) >= 1024 {
			break
		}
		c.cands[e.Addr] = candInfo{
			addr: PeerAddr{IP: ip, Port: uint16(pn)},
			seen: time.Unix(e.Seen, 0),
			utp:  e.UTP,
			src:  "history",
		}
		n++
	}
	c.candMu.Unlock()
	return n
}

// candIsUTP 查询候选是否声明支持 uTP。
func (c *Client) candIsUTP(addr string) bool {
	c.candMu.Lock()
	defer c.candMu.Unlock()
	return c.cands[addr].utp
}

// AddManualPeers 手动添加 peer 地址("host:port" 列表,一行一个):
// 记入候选池(来源 manual)并立即拨号——用于直接连接从其他客户端
// (如 BitComet)连接列表里复制来的 peer。
// 返回成功接受的条数;非法/已存在条目跳过。
func (c *Client) AddManualPeers(addrs []string) int {
	n := 0
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		pn, err := net.LookupPort("tcp", portStr)
		if err != nil || pn <= 0 {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// 允许域名(极少见,但 tracker 域名形式的 peer 也有)
			ips, err := net.LookupIP(host)
			if err != nil || len(ips) == 0 {
				continue
			}
			ip = ips[0]
		}
		p := PeerAddr{IP: ip, Port: uint16(pn)}
		c.rememberCandUTP(p, false, "manual")
		c.tryDial(p)
		n++
	}
	return n
}

// candMeta 查询候选的 uTP 标记与来源。
func (c *Client) candMeta(addr string) (utp bool, src string) {
	c.candMu.Lock()
	defer c.candMu.Unlock()
	ci := c.cands[addr]
	return ci.utp, ci.src
}

// noteDial 一次拨号尝试结束(失败立即返回;成功则在对端断开后返回):
// 刷新候选池时间戳,该地址按退避间隔参与后续回补重拨。
func (c *Client) noteDial(addr string) {
	c.candMu.Lock()
	if ci, ok := c.cands[addr]; ok {
		ci.lastAttempt = time.Now()
		ci.seen = ci.lastAttempt
		c.cands[addr] = ci
	}
	c.candMu.Unlock()
}

// tryDial 登记候选并异步拨号(去重、受连接上限约束)。
// dialLimit 返回本会话的并发拨号上限(引擎注入,0 或负值取缺省)。
func (c *Client) dialLimit() int {
	if c.cfg.DialConcurrency > 0 {
		return c.cfg.DialConcurrency
	}
	return defaultDialConcurrency
}

func (c *Client) tryDial(p PeerAddr) {
	if p.IP == nil || p.Port == 0 {
		return
	}
	if c.isBannedIP(p.IP.String()) {
		return
	}
	addr := p.String()
	c.rememberCand(p)
	c.pMu.Lock()
	// 并发拨号上限:DHT 发现风暴会瞬时注入数百候选,无上限时数百个
	// 并发代理 CONNECT 会把代理出口打满,拖垮全部拨号与既有连接。
	concFull := len(c.dialing) >= c.dialLimit()
	// 在途拨号不占用 MaxPeers 名额:它由 dialLimit 单独限流。若把拨号也
	// 计入,高并发拨号会瞬间吃满预算(实测 27 已连 + 53 拨号中 = 上限 80),
	// 连上 27 条后就不再扩连,远低于同 swarm 的 BitComet(三十多条)。
	peerFull := len(c.peers) >= c.cfg.MaxPeers
	if concFull || c.peers[addr] != nil || c.dialing[addr] || peerFull {
		c.pMu.Unlock()
		btDebug("tryDial 跳过 %s 已连=%v 拨号中=%v 并发满=%v(%d) peer满=%v",
			addr, c.peers[addr] != nil, c.dialing[addr], concFull, len(c.dialing), peerFull)
		return
	}
	c.dialing[addr] = true
	c.pMu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		// 看门狗:拨号链路任一环节意外卡死时兜底释放 dialing 槽位,
		// 防止补拨循环被卡死的槽位永久堵死(槽位重复释放无副作用)。
		watchdog := time.AfterFunc(90*time.Second, func() {
			c.pMu.Lock()
			delete(c.dialing, addr)
			c.pMu.Unlock()
		})
		defer watchdog.Stop()
		c.dialOut(addr)
		c.noteDial(addr)
	}()
}

// reconnectLoop 周期性回补连接:从候选池中挑选距上次尝试最久的地址重拨,
// 直到连接数达到上限。没有它时,tracker 通告间隔(可达 4 小时)内断开的
// peer 永远不会重连,而通告返回的列表又大量是死地址——这是 peer 数长期
// 个位数的直接原因。
func (c *Client) reconnectLoop() {
	defer c.wg.Done()
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	// 每轮补拨数需不小于并发上限,否则拨号管道长期半空:候选里多数是
	// 死地址,一轮要试完几十个才可能撞上一个活的。
	perTick := c.dialLimit()
	backoff := 60 * time.Second // 单地址两次尝试的最小间隔
	for {
		select {
		case <-c.stopCh:
			return
		case <-tk.C:
		}
		if c.completed.Load() || c.stopped() {
			continue
		}
		c.pMu.Lock()
		used := len(c.peers) // 目标只看已建连接;拨号由 dialLimit 限流
		skip := make(map[string]struct{}, len(c.peers)+len(c.dialing))
		for addr := range c.peers {
			skip[addr] = struct{}{}
		}
		for addr := range c.dialing {
			skip[addr] = struct{}{}
		}
		c.pMu.Unlock()
		need := c.cfg.MaxPeers - used
		if need > perTick {
			need = perTick
		}
		if need <= 0 {
			continue
		}
		now := time.Now()
		c.candMu.Lock()
		type pick struct {
			addr string
			t    time.Time
			rank int
		}
		var picks []pick
		for addr, ci := range c.cands {
			if _, busy := skip[addr]; busy {
				continue
			}
			if now.Sub(ci.lastAttempt) < backoff {
				continue
			}
			picks = append(picks, pick{addr, ci.lastAttempt, candRank(ci)})
		}
		// 先按来源优先级(供过数据/tracker/PEX 优先,DHT 最后),同档内最久
		// 未尝试的优先(零值 = 从未尝试,最先补)。没有它时 DHT 拉来的大量
		// 空地址会与有用的候选一起竞争补拨额度,活种子迟迟进不来。
		sort.Slice(picks, func(i, j int) bool {
			if picks[i].rank != picks[j].rank {
				return picks[i].rank < picks[j].rank
			}
			return picks[i].t.Before(picks[j].t)
		})
		var batch []PeerAddr
		for _, pk := range picks {
			if len(batch) >= need {
				break
			}
			ci := c.cands[pk.addr]
			ci.lastAttempt = now // 先占位,避免下一轮重复选中
			c.cands[pk.addr] = ci
			batch = append(batch, ci.addr)
		}
		c.candMu.Unlock()
		for _, p := range batch {
			c.tryDial(p)
		}
	}
}

// AddPeerAddr 便捷入口:host:port 字符串(测试与 x.pe 直连使用)。
func (c *Client) AddPeerAddr(hostport string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return err
	}
	pn, err := net.LookupPort("tcp", port)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("非法地址 %q", hostport)
	}
	c.AddPeerSrc(PeerAddr{IP: ip, Port: uint16(pn)}, "manual")
	return nil
}

func (c *Client) dialOut(addr string) {
	// 出口统一释放 dialing 槽位:内层路径(TCP 失败/uTP 各失败分支)不再
	// 各自清理,任何新增的提前 return 都不会泄漏槽位堵塞补拨循环。
	defer func() {
		c.pMu.Lock()
		delete(c.dialing, addr)
		c.pMu.Unlock()
	}()
	// TCP 优先:uTP 的 LEDBAT 是礼让型拥塞控制(目标排队延迟 100ms,
	// 主动给 TCP 让路),同链路吞吐明显低于 TCP,下载速度优先时 TCP 先行。
	// TCP 不通(对端防火墙/对称 NAT)再回退 uTP —— 后者恰能穿透此类环境。
	tcpOK := c.dialOutTCP(addr)
	if !tcpOK && c.utpm == nil {
		btDebug("uTP 回退跳过 %s(本会话无 uTP 传输)", addr)
	}
	if !tcpOK && c.utpm != nil {
		c.dialOutUTP(addr)
	}
}

func (c *Client) dialOutTCP(addr string) bool {
	dial := func() (net.Conn, error) {
		var conn net.Conn
		var err error
		if c.cfg.Dialer != nil {
			dctx, cancel := context.WithTimeout(c.baseCtx, 15*time.Second)
			conn, err = c.cfg.Dialer(dctx, "tcp", addr)
			cancel()
		} else {
			d := net.Dialer{Timeout: 10 * time.Second}
			conn, err = d.DialContext(c.baseCtx, "tcp", addr)
		}
		if err == nil {
			// MSE 协商与 BT 握手共用此预算(回退重拨同样生效)
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			tuneConn(conn)
		}
		return conn, err
	}
	// MSE 加密优先;对端不支持时自动明文回退重拨。
	wire, enc, err := mseDialPrefer(dial, c.meta.InfoHash)
	if err != nil {
		c.pMu.Lock()
		delete(c.dialing, addr)
		c.pMu.Unlock()
		log.Printf("[bt] 拨号 %s 失败: %v", addr, err)
		return false
	}
	if !enc {
		log.Printf("[bt] %s 未通过加密协商,明文回退连接", addr)
	}
	got, ok := c.outboundHandshake(wire, addr)
	if !ok {
		return false
	}
	if pc := c.register(addr, got, wire); pc != nil {
		_, pc.src = c.candMeta(addr)
		log.Printf("[bt] 已连接 peer %s (client=%q enc=%v)", addr, got.PeerID, enc)
		pc.run()
	} else {
		wire.Close()
	}
	return true
}

// dialOutUTP 尝试 uTP 出站建连+握手;成功即注册并跑连接,返回 true。
func (c *Client) dialOutUTP(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	pn, err := net.LookupPort("tcp", portStr) // uTP 与 TCP 同端口
	if err != nil {
		return false
	}
	dctx, cancel := context.WithTimeout(c.baseCtx, 3*time.Second)
	defer cancel()
	ip := net.ParseIP(host)
	if ip != nil && ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1) // 本机监听地址:未指定形式按回环拨
	}
	udpAddr := &net.UDPAddr{IP: ip, Port: pn}
	wire, err := c.utpm.Dial(dctx, udpAddr)
	if err != nil {
		log.Printf("[bt] uTP 拨号 %s 失败: %v", addr, err)
		return false
	}
	wire.SetDeadline(time.Now().Add(15 * time.Second))
	// uTP 上同样 MSE 优先(协商失败整体回退 TCP,不再明文重试)。
	mwire, err := mseInitiate(wire, c.meta.InfoHash)
	if err != nil {
		wire.Close()
		log.Printf("[bt] uTP %s 加密协商失败,回退 TCP: %v", addr, err)
		return false
	}
	got, ok := c.outboundHandshake(mwire, addr)
	if !ok {
		mwire.Close()
		return false
	}
	if pc := c.register(addr, got, mwire); pc != nil {
		pc.utp = true
		_, pc.src = c.candMeta(addr)
		c.utpPeers.Add(1)
		log.Printf("[bt] 已连接 peer %s (client=%q utp)", addr, got.PeerID)
		pc.run()
		return true
	}
	mwire.Close()
	return false
}

// outboundHandshake 出站握手收发(写我方握手、读对端握手并校验)。
// 失败时关闭 wire 并记 dialing 清理由调用方处理。
func (c *Client) outboundHandshake(wire net.Conn, addr string) (*Handshake, bool) {
	wire.SetDeadline(time.Now().Add(15 * time.Second))
	hs := Handshake{InfoHash: c.meta.InfoHash, PeerID: c.pid}
	if err := WriteHandshake(wire, hs, true, true, true); err != nil {
		wire.Close()
		log.Printf("[bt] 握手发送 %s 失败: %v", addr, err)
		return nil, false
	}
	got, err := ReadHandshake(wire)
	if err != nil || got.InfoHash != c.meta.InfoHash || got.PeerID == c.pid {
		wire.Close()
		log.Printf("[bt] 握手应答 %s 失败: err=%v infohashOK=%v", addr, err, err == nil && got.InfoHash == c.meta.InfoHash)
		return nil, false
	}
	wire.SetDeadline(time.Time{})
	return got, true
}

func (c *Client) register(addr string, hs *Handshake, conn net.Conn) *peerConn {
	// 客户端黑名单:握手期按 peer id 推断名即可判定,直接拒绝(连接由
	// 调用方关闭);扩展握手 v 字段的全名到齐后还会补核一次(onExtended)。
	name := ClientName(hs.PeerID)
	if pat, ok := c.clientBlacklisted(name); ok {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		log.Printf("[bt] 拒绝黑名单客户端 %s(客户端 %q 命中 %s)", addr, name, pat)
		c.banList.Ban(host, "客户端黑名单:匹配 "+pat, name)
		return nil
	}
	c.pMu.Lock()
	defer c.pMu.Unlock()
	delete(c.dialing, addr)
	if c.peers[addr] != nil {
		return nil
	}
	// 多宿主去重:同一客户端经局域网/VPN 多地址接入时 peer_id 相同,
	// 两条平行连接会互抢 piece 租约、轮流饿死(表现为供数断续),
	// choker 还会把上传槽位浪费在同一个客户端上——只保留先建的一条。
	for _, other := range c.peers {
		if other.id == hs.PeerID { // [20]byte 数组,可直接比较
			log.Printf("[bt] 拒绝重复 peer %s(%s):已连接同 id 的 %s",
				addr, ClientName(hs.PeerID), other.addr)
			return nil
		}
	}
	pc := newPeerConn(c, conn, addr, hs.PeerID)
	pc.extCap = hs.SupportsExt()
	pc.fastCap = hs.SupportsFast()
	c.peers[addr] = pc
	return pc
}

func (c *Client) removePeer(addr string) {
	c.pMu.Lock()
	delete(c.peers, addr)
	c.pMu.Unlock()
	c.kickChokerNow() // 腾出的槽位立即补位
}

// isBannedIP 判断 IP 是否在(全局)封锁名单。
func (c *Client) isBannedIP(ip string) bool {
	return c.banList.Banned(ip)
}

// tuneConn 提高 TCP 收发缓冲:高带宽延迟积的公网链路上,默认缓冲会把
// 单连接吞吐限制在数 MB/s;2MB 接收缓冲对应 50ms RTT 下 40MB/s。
// 注意此调用会关闭 Windows 的接收窗口自动调优,故只设得足够大。
func tuneConn(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetReadBuffer(2 << 20)
		tc.SetWriteBuffer(1 << 20)
	}
}

// banHost 判断 addr("ip:port")的 IP 是否在封锁名单;解析失败按未封锁。
func (c *Client) banHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return c.isBannedIP(host)
}

// BanIP 封锁一个 peer IP:写入全局名单(广播断开各会话现存连接,落盘),
// 此后所有任务的拨号与入站一律拒绝。
func (c *Client) BanIP(ip string) {
	c.BanIPReason(ip, "手动封禁")
}

// BanIPReason 同 BanIP,带封禁原因(自动封禁时写明毒源依据);从现存连接
// 抓取对端客户端名一并入档,便于用户在封禁列表里分辨(连接已断则留空)。
func (c *Client) BanIPReason(ip, reason string) {
	c.banList.Ban(ip, reason, c.peerClientName(ip))
}

// SetAutoBanPoison 热切换坏块自动封禁开关(设置保存时对运行中会话生效)。
func (c *Client) SetAutoBanPoison(on bool) {
	c.autoBan.Store(on)
}

// peerClientName 在现存连接里查找该 IP 的对端客户端名(扩展握手 v;查不到
// 返回空串)。名字仅用于封禁记录展示,不影响封禁逻辑。
func (c *Client) peerClientName(ip string) string {
	c.pMu.Lock()
	var name string
	for addr, pc := range c.peers {
		if host, _, err := net.SplitHostPort(addr); err != nil || host != ip {
			continue
		}
		pc.mu.Lock()
		if pc.ext != nil && pc.ext.Version != "" {
			name = pc.ext.Version // 扩展握手 v 字段(完整客户端名)
		} else {
			name = ClientName(pc.id) // 握手期退回 peerID 前缀推断
		}
		pc.mu.Unlock()
		break
	}
	c.pMu.Unlock()
	return name
}

// blPtr 规整黑名单模式(去首尾空白、丢空行)并包装成原子指针。
func blPtr(pats []string) *[]string {
	out := make([]string, 0, len(pats))
	for _, p := range pats {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return &out
}

// matchWildcard 大小写不敏感的通配符匹配:* 匹配任意长度(含零),?
// 匹配单个字符。不用 path.Match——它把 [] 当字符类,而客户端名里方括号
// 应当按字面量处理。经典双指针回溯,O(len)。
func matchWildcard(pattern, s string) bool {
	pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	star, mark := -1, 0
	var pi, si int
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// clientBlacklisted 返回命中的黑名单模式;未配置或未命中返回 false。
func (c *Client) clientBlacklisted(name string) (string, bool) {
	if p := c.clientBL.Load(); p != nil {
		for _, pat := range *p {
			if matchWildcard(pat, name) {
				return pat, true
			}
		}
	}
	return "", false
}

// SetClientBlacklist 热更新客户端黑名单(设置保存时对运行中会话生效):
// 之后的新连接在握手期即拒;现存连接里命中的(客户端名以扩展握手 v 优先)
// 写入全局封禁名单并断开。
func (c *Client) SetClientBlacklist(pats []string) {
	c.clientBL.Store(blPtr(pats))
	type blHit struct {
		pc              *peerConn
		host, name, pat string
	}
	var hits []blHit
	c.pMu.Lock()
	for addr, pc := range c.peers {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		label := pc.clientLabel()
		if pat, ok := c.clientBlacklisted(label); ok {
			hits = append(hits, blHit{pc: pc, host: host, name: label, pat: pat})
		}
	}
	c.pMu.Unlock()
	for _, h := range hits {
		log.Printf("[bt] 热封禁黑名单客户端 %s(客户端 %q 命中 %s)", h.host, h.name, h.pat)
		c.banList.Ban(h.host, "客户端黑名单:匹配 "+h.pat, h.name)
		h.pc.shutdown()
	}
}

// normClientName 客户端名归一(小写 + 去常见分隔符):同一客户端在扩展
// 握手 v 全名与 peer id 推断名之间呈现不同分隔形态(如 "qBittorrent/4.6.7"
// 与 "qBittorrent 4.6.7"),按同款匹配时忽略这些装饰符。
func normClientName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '/', '-', '_', '.':
			return -1
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}

// ClientBlacklistPattern 客户端名对应的黑名单模式:完整名的精确形式(仅去
// 首尾空白,两端不加通配符)。右键「封禁所有该客户端」据此把完整客户端名
// 并入持久客户端黑名单,按精确(大小写不敏感)口径匹配,不剥版本号;空名返回 ""。
func ClientBlacklistPattern(name string) string {
	return strings.TrimSpace(name)
}

// BanClient 封禁对端客户端名等于 name 的现存 peer(连接详情右键
// 「封禁所有该客户端」):写入全局封禁名单并断开其全部连接;同一 IP 的
// 多条连接只记一次名单。返回本次新封禁的 IP 列表(供调用方计数)。
// 名字按双口径归一匹配——扩展握手 v 全名(clientLabel)或 peer id 前缀
// 推断名任一归一后相等即封:同一客户端在扩展握手完成前后呈现两个名字,
// 右键任一形态都要全数封掉。
func (c *Client) BanClient(name string) []string {
	if name == "" {
		return nil
	}
	want := normClientName(name)
	var pcs []*peerConn
	c.pMu.Lock()
	for _, pc := range c.peers {
		if normClientName(pc.clientLabel()) == want || normClientName(ClientName(pc.id)) == want {
			pcs = append(pcs, pc)
		}
	}
	c.pMu.Unlock()
	var banned []string
	seen := map[string]bool{}
	for _, pc := range pcs {
		host, _, err := net.SplitHostPort(pc.addr)
		if err != nil {
			host = pc.addr
		}
		if !seen[host] {
			seen[host] = true
			log.Printf("[bt] 封禁黑名单客户端 %s(客户端 %q)", host, name)
			c.BanIPReason(host, "客户端黑名单:"+name)
			banned = append(banned, host)
		}
		pc.shutdown()
	}
	return banned
}

// KickPeer 断开指定连接(不封锁;对端可能经 DHT/PEX 重新被发现并重连)。
func (c *Client) KickPeer(addr string) bool {
	c.pMu.Lock()
	pc := c.peers[addr]
	c.pMu.Unlock()
	if pc == nil {
		return false
	}
	pc.shutdown()
	return true
}

// BannedIPs 返回封锁名单快照(展示用;全局名单,按 IP 排序)。
func (c *Client) BannedIPs() []string {
	entries := c.banList.List()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.IP)
	}
	sort.Strings(out)
	return out
}

// Peers 返回当前连接数。
func (c *Client) Peers() int {
	c.pMu.Lock()
	defer c.pMu.Unlock()
	return len(c.peers)
}

// ---------- piece 完成路径 ----------

// poisonStrikes 自动封禁的打击阈值(次数,见 ClientConfig.PoisonStrikes,
// 默认 3、设置页可改)。白名单不再看"供过多少块"(交付即计数等于给掺沙子
// 放行),改看 **piece 有效率**:该 IP 参与过的 piece 中通过校验的比例达到
// 阈值(见 ClientConfig.PoisonRate,默认 80%)才豁免自动封禁,一旦低于阈值
// 立即踢出白名单、按一般 peer 累计打击。计数只在 piece 校验通过时进账
// (countPieceResult),即"校验通过才算好块"。
const (
	// poisonStrikesDefault 坏块宽容次数默认值;范围见 normPoisonStrikes。
	poisonStrikesDefault = 3
	// poisonRateDefault 白名单有效率阈值默认值(百分比);范围见 normPoisonRate。
	poisonRateDefault = 80
)
const (
	// poisonStrikesMin/Max 坏块宽容次数的钳制范围:1 = 掺一片就封(最严,
	// 我方 uTP 接收瑕疵误伤风险最高),10 ≈ 基本不自动封(只留观察日志)。
	poisonStrikesMin = 1
	poisonStrikesMax = 10
)

// 只取不予(疑似 PCDN)判据:对端从我们这里索取到 leechMinServed 满
// leechMinElapsed 后,下载期看两条独立线索——回传"几乎为零"(字节判据,
// 不采信可伪造的自报进度),或 piece 声明数自首次供数起纹丝不动(进度
// 判据,专防"做字节互惠骗过字节判据"的低级 PCDN)。阈值取保守值:正常
// 客户端拿到 8MiB(4MiB piece 即 2 片)多少会有回报且必然推进;10 分钟
// 足以排除"还没来得及组装播报"的新手。AutoBanPoison 关闭时同样跳过
// (与毒源封禁同一开关)。
//
// 前两项阈值可用环境变量下调,仅供联调复现(默认值要在真实网络观察十几
// 分钟;小值可几十秒内复现"假进度吸血"封禁):
//
//	ZD_LEECH_MIN_SERVED=262144  ZD_LEECH_MIN_ELAPSED=20s
var (
	leechMinServed  = envInt64("ZD_LEECH_MIN_SERVED", 8<<20)
	leechMinElapsed = envDuration("ZD_LEECH_MIN_ELAPSED", 10*time.Minute)
)

const (
	// 回传"几乎为零"的界线(仅下载期使用):不足索取量的 1/16 且不到
	// 512KiB。512KiB≈32 个块,与毒源白名单门槛同量级——回传达到这个量
	// 就算互惠 peer,不再按吸血处置。
	leechMaxReturn = 512 << 10
	leechRatioDiv  = 16
)

// envInt64 / envDuration 读联调用的数值覆盖,非法值一律回退默认。
func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// recordBlame 记录 piece 的块交付来源(onPiece 落盘成功后调用)。
func (c *Client) recordBlame(piece int, addr string) {
	c.blameMu.Lock()
	if c.pieceBlame[piece] == nil {
		c.pieceBlame[piece] = map[string]int{}
	}
	c.pieceBlame[piece][addr]++
	c.blameMu.Unlock()
}

// normPoisonRate 归一白名单有效率阈值:0 = 默认 80;钳在 50..100(低于
// 50 相当于让"多数供坏块"的 peer 继续豁免,与防护目的相悖)。
func normPoisonRate(pct int) int {
	switch {
	case pct <= 0:
		return poisonRateDefault
	case pct < 50:
		return 50
	case pct > 100:
		return 100
	}
	return pct
}

// whitelistedLocked 白名单判定(须持 blameMu):piece 有效率 = 参与并通过
// 校验的 piece 数 / 参与过的 piece 总数,达到阈值即豁免自动封禁。无样本
// (从未参与过任何 piece 校验)不算白名单——没被校验过的贡献不予担保。
func (c *Client) whitelistedLocked(host string) bool {
	ok, bad := c.peerOKPieces[host], c.peerBadPieces[host]
	if ok+bad == 0 {
		return false
	}
	return int64(ok)*100 >= int64(ok+bad)*int64(c.poisonRate.Load())
}

// SetPoisonRate 热切换白名单有效率阈值(设置保存时对运行中会话生效)。
func (c *Client) SetPoisonRate(pct int) {
	c.poisonRate.Store(int32(normPoisonRate(pct)))
}

// normPoisonStrikes 归一坏块宽容次数:0 = 默认 3;钳在 1..10(0 次等于
// 首次坏块即封,1 是显式设置的下限;上限防把自动封禁事实关掉)。
func normPoisonStrikes(n int) int {
	switch {
	case n <= 0:
		return poisonStrikesDefault
	case n < poisonStrikesMin:
		return poisonStrikesMin
	case n > poisonStrikesMax:
		return poisonStrikesMax
	}
	return n
}

// SetPoisonStrikes 热切换坏块宽容次数(设置保存时对运行中会话生效)。
func (c *Client) SetPoisonStrikes(n int) {
	c.poisonStrikes.Store(int32(normPoisonStrikes(n)))
}

// countPieceResult 记一次 piece 校验结果,把该 piece 的全部块来源计入
// "参与片数"(通过/失败),失败且唯一来源时额外记独占数。仅用于连接详情
// 展示"piece 有效率",与封禁策略无关(白名单豁免的 peer 同样照实记)。
func (c *Client) countPieceResult(contributors map[string]int, ok bool) {
	if len(contributors) == 0 {
		return
	}
	sole := !ok && len(contributors) == 1
	c.blameMu.Lock()
	for addr := range contributors {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if ok {
			c.peerOKPieces[host]++
		} else {
			c.peerBadPieces[host]++
			if sole {
				c.peerSoleBad[host]++
			}
		}
	}
	c.blameMu.Unlock()
}

// takeBlame 取出并清除 piece 的来源表(校验出结果后调用,防泄漏)。
func (c *Client) takeBlame(piece int) map[string]int {
	c.blameMu.Lock()
	defer c.blameMu.Unlock()
	m := c.pieceBlame[piece]
	delete(c.pieceBlame, piece)
	return m
}

// blameStrikes 校验失败后的坏块追责。策略:
//   - 白名单:piece 有效率(参与并通过校验的比例)达阈值(默认 80%,设置页
//     可改)的 peer 不自动封禁(只继续观察)——100% 做种的正常 qB 也可能因
//     我方 uTP 接收瑕疵"供出"坏块,把大量供过好数据的 peer 封掉会显著拖慢
//     整组下载;但掺沙子的混毒源(真块挣白名单 + 掺坏块)会让有效率跌破阈值,
//     自动踢出白名单后按一般 peer 记打击。
//   - 其余 peer 累计 poisonStrikes 次打击封禁(次数设置页可改,默认 3);
//     整块唯一来源同样按次数累计(不再"铁证立即判罚":实测唯一来源也可能是
//     唯一还在线的正常源)。
// AutoBanPoison 关闭时只观察不计(热切换,无需重建会话)。
func (c *Client) blameStrikes(contributors map[string]int) (banned []string, suspects []string) {
	limit := int(c.poisonStrikes.Load())
	c.blameMu.Lock()
	defer c.blameMu.Unlock()
	for addr := range contributors {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if !c.autoBan.Load() || c.whitelistedLocked(host) {
			suspects = append(suspects, host)
			continue
		}
		c.peerStrikes[host]++
		if c.peerStrikes[host] >= limit {
			banned = append(banned, addr)
			continue
		}
		suspects = append(suspects, host)
	}
	return banned, suspects
}

// leechSweep 周期扫描"只取不予"的疑似 PCDN:长期向我们索取数据,却几乎
// 不回传。命中即写入全局封禁名单(广播断开现存连接);AutoBanPoison 关闭
// 时整段跳过,与毒源封禁同一开关。
//
// 判据分两档(阈值为 leechMinServed/leechMinElapsed/leechMaxReturn):
//   - 下载期(本任务尚未完成):对端自报的 piece 进度可以伪造——实测有 PCDN
//     伪装成 qBittorrent 4.6.7 边假增进度边吸血,故字节判据不采信进度:
//     供出 ≥8MiB 且满 10 分钟、回传不足索取量的 1/16 且不到 512KiB 即封。
//     另有独立的进度判据(复活旧判据,不再要求回报恰为零):自首次供数起
//     piece 声明数纹丝不动也照封——低级 PCDN 会做字节互惠骗过字节判据,
//     却从不真正完成任何 piece。两条判据命中其一即封。
//   - 做种期(本任务已完成):对端"只下载不回报"可能是合法纯下载者,字节
//     上无法与假进度吸血区分,沿用保守判据——回传恰为零且进度声明纹丝
//     不动(连 have 都没有)。
func (c *Client) leechSweep(now time.Time) {
	if !c.autoBan.Load() {
		return
	}
	seeding := c.completed.Load()
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()
	for _, pc := range list {
		servedAt := pc.serveAt.Load()
		if servedAt == 0 || pc.upTotal.Load() < leechMinServed {
			continue // 尚未供过数 / 索取量未达阈值
		}
		elapsed := now.Sub(time.Unix(0, servedAt))
		if elapsed < leechMinElapsed {
			continue
		}
		down, up := pc.downTotal.Load(), pc.upTotal.Load()
		var reason string
		if seeding {
			if down != 0 || pc.haveCount.Load() != pc.haveAtServe.Load() {
				continue // 有回报 / 进度在推进:做种期不判只取不予
			}
			reason = "只取不予:索取大回报零且进度不变(疑似PCDN)"
		} else {
			// 进度判据(复活旧判据,不再要求回报恰为零):自首次供数起
			// piece 声明数纹丝不动——低级 PCDN 收了我们的上传却从不完成
			// 任何 piece,连一个 have 都不增,做字节互惠也照封。字节判据
			// 恰好管不住这类(回传达微回报即视为互惠),两判据互补。
			if pc.haveCount.Load() == pc.haveAtServe.Load() {
				reason = "只取不予:索取大进度未变(疑似PCDN)"
			} else {
				if down >= leechMaxReturn || down*leechRatioDiv > up {
					continue // 进度在推进且回传达"微回报"以上:互惠 peer
				}
				reason = "只取不予:索取大回报微(疑似PCDN)"
			}
		}
		host, _, err := net.SplitHostPort(pc.addr)
		if err != nil {
			host = pc.addr
		}
		log.Printf("[bt] 自动封禁只取不予 %s(%s): 索取 %dMB/回传 %dKB/供数 %d 分钟",
			host, reason, up>>20, down>>10, int(elapsed.Minutes()))
		c.BanIPReason(host, reason)
	}
}

// onPieceVerified 在某 piece 全块到货后校验、落定、广播;全部完成时收尾。
// 在触发它的 peer 读协程中执行。
func (c *Client) onPieceVerified(piece int) {
	// 与 onPiece 的落盘路径取同一 piece 锁:校验+标记完成期间不允许任何
	// 新块落盘,否则块写入可能插进校验读与 MarkDone 之间把已通过的数据改坏。
	m := c.lockPiece(piece)
	if c.store.Load() == nil {
		c.picker.ResetPiece(piece)
		c.takeBlame(piece)
		m.Unlock()
		return
	}
	ok, parked := c.verifyOrDefer(piece)
	if parked {
		// v2 缺 layers:数据已落盘暂存,等整个文件到齐后统一校验落定
		m.Unlock()
		return
	}
	if !ok {
		c.picker.ResetPiece(piece)
		contributors := c.takeBlame(piece)
		// 先记这次失败(有效率随之下调、低于阈值即失去白名单),再追责:
		// "一旦低于则踢出"要当场生效,不能让本次坏块还吃旧荣誉的豁免。
		c.countPieceResult(contributors, false)
		banned, suspects := c.blameStrikes(contributors)
		m.Unlock()
		log.Printf("[bt] %x piece %d 校验失败,丢弃重下(来源 %v)", c.meta.InfoHash[:6], piece, contributors)
		for _, addr := range banned {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			reason := fmt.Sprintf("毒源:累计供坏块 %d 次", c.poisonStrikes.Load())
			log.Printf("[bt] 自动封禁毒源 %s(%s)", host, reason)
			c.BanIPReason(host, reason)
		}
		if len(suspects) > 0 {
			log.Printf("[bt] 坏块来源观察中(有效率达标留白名单或累计未达 %d 次): %v", c.poisonStrikes.Load(), suspects)
		}
		return
	}
	c.countPieceResult(c.takeBlame(piece), true) // 通过:连同来源表一起计入有效率
	c.picker.MarkDone(piece)
	m.Unlock()
	c.broadcastHave(piece)
	c.cancelPiece(piece) // 该 piece 已完整,取消所有 peer 对它的在途请求
	c.pieceDoneNotify()  // 唤醒边下边播等待方(流媒体 handler)
	if c.picker.AllDone() {
		c.finishDownload()
	}
}

func (c *Client) finishDownload() {
	c.completeOnce.Do(func() {
		st := c.store.Load()
		final, err := st.Finalize()
		if err != nil {
			werr := fmt.Errorf("落盘失败: %w", err)
			c.setErr(werr)
			// 通知 WaitComplete:只 setErr 的话引擎侧会永远等下去
			// (任务显示"运行中、速度 0"卡死),典型诱因是文件被播放器
			// 占用导致改名失败。错误上报后任务转 error,用户重试时会
			// 走启动重哈希再补一次收尾
			select {
			case c.failCh <- werr:
			default:
			}
			return
		}
		// 切换到最终路径的只读形态继续做种
		if seedSt, serr := openSeedingStorage(c.meta, final); serr == nil {
			c.store.Store(seedSt)
		}
		c.completed.Store(true)
		p := final
		c.finalPath.Store(&p)
		close(c.completeCh)
		select {
		case c.kickCh <- struct{}{}:
		default:
		}
		c.kickChokerNow() // 切换到做种轮换策略
	})
}

// lockPiece 取该 piece 所在分片的锁并返回;调用方负责 Unlock。
func (c *Client) lockPiece(piece int) *sync.Mutex {
	m := &c.pieceMu[uint(piece)%uint(len(c.pieceMu))]
	m.Lock()
	return m
}

func (c *Client) broadcastHave(piece int) {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()
	for _, pc := range list {
		pc.tryPush(outbound{msgHave, BuildHave(uint32(piece))})
	}
}

// cancelDup 重复到货:取消其余 peer 对同一块的在途请求。
func (c *Client) cancelDup(piece int, begin uint32, except *peerConn) {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		if pc != except {
			list = append(list, pc)
		}
	}
	c.pMu.Unlock()
	for _, pc := range list {
		pc.mu.Lock()
		length := uint32(0)
		for i, k := range pc.outstanding {
			if k.piece == piece && k.begin == begin {
				length = k.length
				pc.outstanding = append(pc.outstanding[:i], pc.outstanding[i+1:]...)
				if pc.pending > 0 {
					pc.pending--
				}
				break
			}
		}
		pc.mu.Unlock()
		if length > 0 {
			c.picker.Cancel(piece, begin)
			pc.tryPush(outbound{msgCancel, BuildCancel(uint32(piece), begin, length)})
		}
	}
}

// cancelPiece piece 完成:取消所有 peer 对该 piece 全部在途请求。
func (c *Client) cancelPiece(piece int) {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()
	for _, pc := range list {
		pc.mu.Lock()
		var cancels []reqKey
		kept := pc.outstanding[:0]
		for _, k := range pc.outstanding {
			if k.piece == piece {
				cancels = append(cancels, k)
			} else {
				kept = append(kept, k)
			}
		}
		pc.outstanding = kept
		pc.pending -= len(cancels)
		if pc.pending < 0 {
			pc.pending = 0
		}
		pc.mu.Unlock()
		for _, k := range cancels {
			pc.tryPush(outbound{msgCancel, BuildCancel(uint32(k.piece), k.begin, k.length)})
		}
	}
}

// gatherPEX 抽取最多 n 个已知 peer 地址(供 PEX 分享)。flags 按 BEP-11:
// 0x01 加密偏好、0x02 seed、0x04 支持 uTP。
func (c *Client) gatherPEX(n int, exclude string) ([]PeerAddr, []byte) {
	c.pMu.Lock()
	defer c.pMu.Unlock()
	out := make([]PeerAddr, 0, n)
	flags := make([]byte, 0, n)
	for addr, pc := range c.peers {
		if addr == exclude {
			continue
		}
		ha, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		ip := net.ParseIP(ha)
		if ip == nil || ip.To4() == nil || port == "0" {
			continue
		}
		pn, _ := net.LookupPort("tcp", port)
		out = append(out, PeerAddr{IP: ip.To4(), Port: uint16(pn)})
		// BEP-11:added.f 描述的是被通告的 peer(0x04=支持 uTP),
		// 不是本端——把本端能力盖到所有人头上会诱导对端对纯 TCP
		// peer 白白发 uTP 拨号。
		var f byte
		if pc.utp {
			f |= 0x04
		}
		flags = append(flags, f)
		if len(out) >= n {
			break
		}
	}
	return out, flags
}

// ---------- announce ----------

func (c *Client) announceLoop(rawURL string) {
	defer c.wg.Done()
	st := c.regTracker(rawURL)
	first := true
	completedSent := false
	for {
		if !c.hasTracker(rawURL) {
			return // tracker 已被移除
		}
		ev := EventNone
		if first {
			ev = EventStarted
		} else if c.completed.Load() && !completedSent {
			ev = EventCompleted
		}
		req := AnnounceReq{
			InfoHash:   c.meta.InfoHash,
			PeerID:     c.pid,
			Port:       uint16(c.cfg.ListenPort),
			Uploaded:   c.upTotal.Load(),
			Downloaded: c.downTotal.Load(),
			Left:       c.left(),
			Event:      ev,
			Key:        binary.BigEndian.Uint32(c.pid[16:20]),
			NumWant:    100,
		}
		ctx, cancel := context.WithTimeout(c.baseCtx, 20*time.Second)
		resp, err := c.announceOnce(ctx, rawURL, req)
		cancel()
		now := time.Now()
		c.trMu.Lock()
		if err != nil {
			st.lastErr = err.Error()
			st.retries++
			st.msg = "通告失败: " + err.Error()
		} else {
			st.lastErr = ""
			st.lastOK = now
			st.peers = resp.Complete + resp.Incomplete
			st.seeds = resp.Complete
			st.leechers = resp.Incomplete
			st.retries = 0
			st.msg = fmt.Sprintf("通告成功: 返回 %d 个 peer", len(resp.Peers))
			for _, p := range resp.Peers {
				st.recentPeers = append(st.recentPeers, p.String())
			}
			if len(st.recentPeers) > 50 { // 只留最近 50 个,避免长 tracker 列表撑爆内存
				st.recentPeers = append([]string(nil), st.recentPeers[len(st.recentPeers)-50:]...)
			}
		}
		c.trMu.Unlock()

		wait := 60 * time.Second
		if err == nil {
			if ev == EventCompleted {
				completedSent = true
			}
			first = false
			for _, p := range resp.Peers {
				c.AddPeerSrc(p, "tracker")
			}
			wait = time.Duration(resp.Interval) * time.Second
			if resp.MinInterval > 0 && time.Duration(resp.MinInterval)*time.Second < wait {
				wait = time.Duration(resp.MinInterval) * time.Second
			}
			wait = max(wait, 30*time.Second)
			wait = min(wait, 4*time.Hour)
		}
		c.trMu.Lock()
		st.nextAt = time.Now().Add(wait)
		c.trMu.Unlock()
		select {
		case <-c.stopCh:
			return
		case <-c.kickCh:
			continue
		case <-time.After(wait):
		}
	}
}

func (c *Client) regTracker(url string) *trackerState {
	c.trMu.Lock()
	defer c.trMu.Unlock()
	st, ok := c.trStates[url]
	if !ok {
		st = &trackerState{url: url}
		c.trStates[url] = st
	}
	return st
}

// TrackerStates 返回 tracker 状态快照(按 URL 排序由调用方处理)。
func (c *Client) TrackerStates() []TrackerState {
	c.trMu.Lock()
	defer c.trMu.Unlock()
	out := make([]TrackerState, 0, len(c.trStates))
	for _, st := range c.trStates {
		lastOK := ""
		if !st.lastOK.IsZero() {
			lastOK = st.lastOK.Format(time.RFC3339)
		}
		next := ""
		if !st.nextAt.IsZero() {
			next = st.nextAt.Format(time.RFC3339)
		}
		out = append(out, TrackerState{
			URL:          st.url,
			LastErr:      st.lastErr,
			LastOK:       lastOK,
			Peers:        st.peers,
			RecentPeers:  append([]string(nil), st.recentPeers...),
			Seeds:        st.seeds,
			Leechers:     st.leechers,
			Retries:      st.retries,
			NextAnnounce: next,
			Message:      st.msg,
		})
	}
	return out
}

// PeerStats 返回全部连接的展示快照(按累计下载降序,活跃 peer 在前)。
func (c *Client) PeerStats() []PeerStat {
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()
	total := c.meta.PieceCount()
	now := time.Now()
	// piece 有效率快照(按 IP 归口,连接同一 IP 的多个连接共享一份账)
	c.blameMu.Lock()
	okPieces := make(map[string]int, len(c.peerOKPieces))
	badPieces := make(map[string]int, len(c.peerBadPieces))
	soleBad := make(map[string]int, len(c.peerSoleBad))
	for k, v := range c.peerOKPieces {
		okPieces[k] = v
	}
	for k, v := range c.peerBadPieces {
		badPieces[k] = v
	}
	for k, v := range c.peerSoleBad {
		soleBad[k] = v
	}
	c.blameMu.Unlock()
	out := make([]PeerStat, 0, len(list))
	for _, pc := range list {
		var prog float64
		if total > 0 {
			prog = float64(pc.haveCount.Load()) / float64(total)
		}
		phost, _, perr := net.SplitHostPort(pc.addr)
		if perr != nil {
			phost = pc.addr
		}
		ps := PeerStat{
			Addr: pc.addr,
			// 客户端名与 clientLabel(封禁匹配/名单归档)同口径:扩展握手 v
			// 全名优先,退回 peer id 前缀推断——右键「封禁所有该客户端」
			// 拿展示名精确匹配,两处口径不一就会 0 封禁(实测教训)。
			PeerID: string(pc.id[:]),
			Client: pc.clientLabel(),
			Down:      pc.downTotal.Load(),
			Up:        pc.upTotal.Load(),
			Progress:  prog,
			UTP:       pc.utp,
			Inbound:   pc.inbound,
			PiecesOK:  okPieces[phost],
			PiecesBad: badPieces[phost],
			SoleBad:   soleBad[phost],
			Source:   pc.src,
		}
		if !pc.connectedAt.IsZero() {
			ps.ConnectedFor = now.Sub(pc.connectedAt).Seconds()
		}
		pc.mu.Lock()
		// 速率:与上次快照做差(首个快照只立基线)
		down, up := pc.downTotal.Load(), pc.upTotal.Load()
		if !pc.spAt.IsZero() && now.After(pc.spAt) {
			dt := now.Sub(pc.spAt).Seconds()
			ps.DownRate = float64(down-pc.spDown) / dt
			ps.UpRate = float64(up-pc.spUp) / dt
			if ps.DownRate < 0 {
				ps.DownRate = 0
			}
			if ps.UpRate < 0 {
				ps.UpRate = 0
			}
		}
		pc.spAt, pc.spDown, pc.spUp = now, down, up
		// 能力与位图(mu 保护字段)
		if pc.ext != nil {
			ps.ListenPort = pc.ext.ListenPort
			ps.PEX = pc.ext.SupportsPEX()
		}
		// qBittorrent(libtorrent)扩展握手 p=0 不回传监听端口:出站连接
		// 用拨号目标端口兜底——tracker/PEX 给的正是对方的 BT 监听端口
		if ps.ListenPort == 0 && !pc.inbound {
			if _, sp, err := net.SplitHostPort(pc.addr); err == nil {
				if pn, perr := net.LookupPort("tcp", sp); perr == nil {
					ps.ListenPort = int64(pn)
				}
			}
		}
		if len(pc.bitfield) > 0 {
			ps.Bitfield = base64.StdEncoding.EncodeToString(bitfieldBytes(pc.bitfield))
		}
		var flags []byte
		if pc.peerInterested && pc.unchokedByUs {
			flags = append(flags, 'D') // 正在从我们下载
		} else if pc.peerInterested {
			flags = append(flags, 'I') // 想要我们的数据
		}
		if pc.peerChoke {
			flags = append(flags, 'C') // 对端 choke 着我们
		}
		ps.Flags = string(flags)
		pc.mu.Unlock()
		if mc, ok := pc.conn.(*mseConn); ok {
			ps.Encrypted = mc.encrypted
		}
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DownRate != out[j].DownRate {
			return out[i].DownRate > out[j].DownRate
		}
		return out[i].Down > out[j].Down
	})
	return out
}

// PEXReceived 返回 PEX 发现的 peer 累计数(虚拟行展示)。
func (c *Client) PEXReceived() int32 { return c.pexRecv.Load() }

// ClientName 由 peer id 前缀推断客户端名称与版本(Azureus 风格
// "-XXmmmm":两字符代码 + 版本数字;未知返回可打印前缀)。
func ClientName(pid [20]byte) string {
	s := string(pid[:])
	if len(s) < 8 {
		return "未知"
	}
	// 非 Azureus 定长形式(老客户端)优先精确匹配
	prefixes := map[string]string{
		"-bT": "BTNextGen", "-grl": "GetRight", "-lt0": "libTorrent",
		"-lt1": "libTorrent", "-lt2": "libTorrent", "-lt3": "libTorrent",
		"-S1": "Seed (Shadow)", "Mbrst": "Burst", "exbc": "BitComet 0.x",
		"-FG0": "Free Download Manager", "-FG1": "Free Download Manager",
	}
	for p, name := range prefixes {
		if strings.HasPrefix(s, p) {
			return name
		}
	}
	if s[0] != '-' {
		return "未知"
	}
	// Azureus 风格:两字符代码 + 版本数字
	clientCodes := map[string]string{
		"ZD": "ZDownloader", "AZ": "Azureus", "AB": "AnyEvent", "AG": "Ares",
		"AR": "Arctic", "AT": "Artemis", "AV": "Avicora", "BB": "BitBuddy",
		"BC": "BitComet", "BE": "BitTorrent SDK", "BF": "Bitflu", "BG": "BTGetit",
		"BM": "BitMagnet", "BN": "btdownloadcsharp", "BQ": "Burst",
		"BT": "BitTorrent", "BX": "Bittorrent X", "CD": "CTorrent",
		"CT": "CTorrent", "DP": "Propagate Data", "EB": "EBit",
		"ES": "Electric Sheep", "FC": "FileCroc", "FD": "Free Download Manager",
		"FT": "FoxTorrent", "GT": "GetTorrent", "HK": "Hekate", "HN": "Hook",
		"IL": "iLivid", "JS": "JSTorrent", "KT": "KTorrent", "LC": "LeechCraft",
		"LH": "LH-ABC", "LP": "Lphant", "LR": "LibTorrent (Rakshasa)",
		"LT": "libtorrent", "MG": "MediaGet", "MO": "MonoTorrent", "MR": "Miro",
		"MT": "Moonlight", "NX": "Net Transport", "PD": "Pando",
		"PI": "PicoTorrent", "QD": "QQDownload", "QQ": "QQDownload",
		"qB": "qBittorrent", "QT": "Qt 4 Torrent", "RT": "Retriever",
		"SB": "Swiftbit", "SD": "Thunder", "SK": "Thunder", "SP": "BitSpirit",
		"SS": "SwarmScope", "ST": "Storm", "SZ": "Shareaza", "TB": "Toro",
		"TD": "Tudou", "TE": "TorrentElectron", "TK": "TorrentKitten",
		"TL": "TuoTu", "TM": "TorrentMan", "TN": "TorrentDotNET",
		"TR": "Transmission", "TS": "TorrentStorm", "TT": "TuoTu",
		"UL": "uLeecher", "UM": "uTorrent Mac", "UP": "uTorrent",
		"UT": "uTorrent", "UW": "uTorrent Web", "VG": "Vagaa", "WG": "WGET",
		"WW": "WebTorrent", "WT": "BitLet", "XL": "Xunlei", "XS": "XSwifter",
		"XT": "Xtorrent", "XX": "Xtorrent", "ZT": "ZipTorrent",
	}
	code := s[1:3]
	name, ok := clientCodes[code]
	if !ok {
		if s[3] >= '0' && s[3] <= '9' {
			return code // 未知 Azureus 代码:显示原始代码
		}
		return s[1:5]
	}
	if ver := azureusVersion(s[3:], code == "BC"); ver != "" {
		return name + " " + ver
	}
	return name
}

// azureusVersion 解析 Azureus 编码的版本数字串:每位一段,尾部多余的
// 分隔 0 去除(保留至少两段);BitComet 是两位一段的特例,且版本串以
// 0 开头(如 -BC0192- = 1.92)。
func azureusVersion(d string, twoPerPart bool) string {
	var digits []byte
	for i := 0; i < len(d) && d[i] >= '0' && d[i] <= '9'; i++ {
		digits = append(digits, d[i])
	}
	if len(digits) == 0 {
		return ""
	}
	var parts []string
	if twoPerPart {
		if digits[0] == '0' {
			digits = digits[1:]
		}
		// BitComet 惯例:主版本 1 位,副版本两位一组(-BC0192- = 1.92)
		parts = append(parts, string(digits[0]))
		for i := 1; i < len(digits); i += 2 {
			e := i + 2
			if e > len(digits) {
				e = len(digits)
			}
			parts = append(parts, string(digits[i:e]))
		}
	} else {
		for _, c := range digits {
			parts = append(parts, string(c))
		}
		for len(parts) > 2 && parts[len(parts)-1] == "0" {
			parts = parts[:len(parts)-1]
		}
	}
	return strings.Join(parts, ".")
}

// FileProgress 按已校验 piece 位图推算每个文件的完成字节数。
func (c *Client) FileProgress() []FileProg {
	fw := c.cfg.FileWanted
	if fw == nil {
		fw = fileWantedFor(c.meta, c.cfg.WantedPieces)
	}
	return FileProgressOf(c.meta, c.picker.Have(), fw)
}

// FileProgressOf 按位图推算文件进度;文件跨 piece 时按区间重叠计算
// (尾部 piece 可能部分完成)。fileWanted 为文件需求位图(nil = 全部):
// 未选文件恒计 0——跨界 piece 为选中文件搭车写入的字节不算它的进度。
// 单文件种子的 Path 为 [Name]。
func FileProgressOf(meta *MetaInfo, have, fileWanted []bool) []FileProg {
	plen := meta.PieceLength
	files := meta.Files
	if len(files) == 0 {
		var done int64
		for i, h := range have {
			if h {
				done += meta.PieceLen(i)
			}
		}
		return []FileProg{{Path: []string{meta.Name}, Size: meta.TotalLength, Done: done, First: 0, Last: meta.PieceCount() - 1, Wanted: true}}
	}
	out := make([]FileProg, 0, len(files))
	for fi, f := range files {
		first, last := meta.FilePieceRange(fi)
		isWanted := fileWanted == nil || fileWanted[fi]
		if !isWanted {
			out = append(out, FileProg{Path: f.Path, Size: f.Length, Done: 0, First: first, Last: last, Wanted: false})
			continue
		}
		if meta.V2 {
			// v2 piece 不跨文件:文件完成字节 = 区间内已校验 piece 之和
			var done int64
			for i := first; i <= last && i < len(have); i++ {
				if have[i] {
					done += meta.PieceLen(i)
				}
			}
			out = append(out, FileProg{Path: f.Path, Size: f.Length, Done: done, First: first, Last: last, Wanted: true})
			continue
		}
		start := f.Offset
		end := f.Offset + f.Length // [start,end)
		var done int64
		for i, h := range have {
			if !h {
				continue
			}
			ps, pe := int64(i)*plen, int64(i)*plen+meta.PieceLen(i)
			lo, hi := max(ps, start), min(pe, end)
			if hi > lo {
				done += hi - lo
			}
		}
		out = append(out, FileProg{Path: f.Path, Size: f.Length, Done: done, First: first, Last: last, Wanted: true})
	}
	return out
}

func (c *Client) announceOnce(ctx context.Context, rawURL string, req AnnounceReq) (*AnnounceResp, error) {
	if strings.HasPrefix(strings.ToLower(rawURL), "udp://") {
		// 直连优先(本机 UDP 通常可达);失败且配置了中继时再经代理重试。
		// 重试用独立超时:直连尝试通常已耗尽传入 ctx 的大部分预算
		resp, err := UDPAnnounceConn(ctx, rawURL, req, nil, c.cfg.Resolver)
		if err == nil || c.cfg.UDPRelay == nil {
			return resp, err
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		return UDPAnnounceConn(rctx, rawURL, req, c.cfg.UDPRelay, c.cfg.Resolver)
	}
	return HTTPAnnounce(ctx, c.httpClient, rawURL, req, c.cfg.SpoofName)
}

// left 返回尚未完成(已校验)的字节数,用于 tracker 汇报。
func (c *Client) left() int64 {
	if c.completed.Load() {
		return 0
	}
	have := c.picker.Have()
	var left int64
	for i, h := range have {
		if !h {
			left += c.meta.PieceLen(i)
		}
	}
	return left
}

// ---------- 生命周期与统计 ----------

// Stop 停止会话:断开全部 peer、关监听,tracker 的 stopped 通告在后台
// 发送(尽力而为,失败无后果)。绝不能同步等通告:公开 tracker 列表里
// 不可达的域名会把预算拖满 5 秒,而 Stop 卡在 HTTP handler 里——改选文件
// 的接口同步轮询旧会话退出(实测 5028ms),做种任务的取消/删除也在此
// 阻塞。通告放进 stopOnc:整个生命周期只发一次(旧实现每次 Stop 都重发)。
func (c *Client) Stop() {
	c.stopOnc.Do(func() {
		close(c.stopCh)
		c.baseCxl()
		c.banList.detach(c) // 注销封禁广播
		if c.hub != nil {
			c.hub.Remove(c) // 共享端口:注销 infohash 路由,Hub 侧立即拒收新入站
		}
		if c.ln != nil {
			c.ln.Close()
		}
		if c.utpm != nil && !c.utpm.shared {
			// 共享模式(DHT 同端口复用)的 manager 归 DHT 所有,不在此关闭
			c.utpm.Close()
		}
		c.stopAnnounceAsync()
	})
	c.pMu.Lock()
	list := make([]*peerConn, 0, len(c.peers))
	for _, pc := range c.peers {
		list = append(list, pc)
	}
	c.pMu.Unlock()
	for _, pc := range list {
		pc.shutdown()
	}
}

// stopAnnounceAsync 后台广播 stopped 通告(整体 5s 预算,与进程退出无同步
// 约定:退出时未发完的通告随进程消失,tracker 侧只表现为 key 提前过期,
// 无正确性影响)。trackers 快照在派发前取出,避免与 teardown 并发读切片。
func (c *Client) stopAnnounceAsync() {
	urls := make([]string, 0, len(c.trackers))
	for _, u := range c.trackers {
		urls = append(urls, u)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var swg sync.WaitGroup
		for _, u := range urls {
			swg.Add(1)
			go func(url string) {
				defer swg.Done()
				req := AnnounceReq{
					InfoHash: c.meta.InfoHash, PeerID: c.pid, Port: uint16(c.cfg.ListenPort),
					Uploaded: c.upTotal.Load(), Downloaded: c.downTotal.Load(),
					Left: c.left(), Event: EventStopped,
					Key: binary.BigEndian.Uint32(c.pid[16:20]),
				}
				c.announceOnce(ctx, url, req)
			}(u)
		}
		swg.Wait()
	}()
}

// Close = Stop + 释放存储句柄。
func (c *Client) Close() {
	c.Stop()
	if st := c.store.Load(); st != nil {
		st.Close()
	}
}

// WaitComplete 等待全部 piece 校验完成并落盘。
func (c *Client) WaitComplete(ctx context.Context) error {
	select {
	case <-c.completeCh:
		return nil
	case err := <-c.failCh:
		return err
	case <-c.stopCh:
		return errors.New("bt: 会话已停止")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Completed 报告是否已完成(进入做种)。
func (c *Client) Completed() bool { return c.completed.Load() }

// FinishIfComplete 在会话已持有全部所需 piece 时立即触发收尾(落定→完成)。
// 用于重启一个位图已满、但最终文件尚未落定的任务:数据在临时文件里已逐
// piece 校验通过,不主动触发就会停在 AllDone 而无人工触发完成
// (任务显示运行中、速度恒 0)。返回是否触发了收尾(重复调用无副作用)。
func (c *Client) FinishIfComplete() bool {
	if !c.picker.AllDone() {
		return false
	}
	c.finishDownload()
	return true
}

// UTPPeers 返回经 uTP(BEP-29) 建连的 peer 数(诊断/测试)。
func (c *Client) UTPPeers() int32 { return c.utpPeers.Load() }

// FinalPath 返回落定后的最终路径(未完成返回空)。
func (c *Client) FinalPath() string {
	if p := c.finalPath.Load(); p != nil {
		return *p
	}
	return ""
}

// Stats 返回统计快照。
func (c *Client) Stats() Stats {
	return Stats{
		Uploaded:   c.upTotal.Load(),
		Downloaded: c.downTotal.Load(),
		Peers:      c.Peers(),
	}
}

// ArrivedBytes 返回已收到并落盘的字节(块粒度,不等 piece 校验)。
// 进度条口径:随下载平滑推进,上界为任务总量。
func (c *Client) ArrivedBytes() int64 {
	return c.picker.ArrivedBytes()
}

// SnapshotBlocks 导出未完成 piece 的已收块位图(暂停时随 Bitfield 一起持久化)。
// 先把内存攒块缓冲整片溢写落盘再导出:不变量是"位图里的块都在盘上"——
// 否则强杀/崩溃时缓冲随进程丢失,而位图已标持有,重启恢复出"账面有、
// 盘上是零"的幻影块,piece 以零块凑数反复校验失败(实测同一 piece 换批
// peer 仍失败,下载死循环)。溢写后同 piece 的后续块走 spill 直写路径,
// 依旧先落盘再登记,不变量持续成立。
func (c *Client) SnapshotBlocks() []byte {
	if st := c.store.Load(); st != nil {
		st.FlushPieceBufs()
	}
	return c.picker.SnapshotBlocks()
}

// RestoreBlocks 还原暂停前保存的块级位图;须在 Start 之前调用。
// 全块到齐的 piece 立即校验落定,部分到齐的只补缺块。
func (c *Client) RestoreBlocks(data []byte) {
	c.picker.RestoreBlocks(data, func(idx int) bool {
		st := c.store.Load()
		return st != nil && st.VerifyPiece(idx)
	})
}

// PiecesDone 返回已校验且属于需求的 piece 数(文件选择时只统计需要的)。
func (c *Client) PiecesDone() int {
	have := c.HaveBits()
	n := 0
	for _, h := range have {
		if h {
			n++
		}
	}
	return n
}

// DoneBytes 返回已校验且属于需求的字节数(文件选择任务的进度口径)。
func (c *Client) DoneBytes() int64 {
	have := c.HaveBits()
	var total int64
	for i, h := range have {
		if h {
			total += c.meta.PieceLen(i)
		}
	}
	return total
}

// HaveBits 返回已校验 piece 位图快照。
func (c *Client) HaveBits() []bool { return c.picker.Have() }

// SetLimits 热更新任务级限速(B/s,0 = 不设任务级上限,仍受全局约束)。
func (c *Client) SetLimits(down, up int64) {
	c.dlLim.SetRate(down)
	c.ulLim.SetRate(up)
}

// waitDL 下载路径两级限速:任务级先过,再进引擎共享的全局限速器(与
// HTTP 任务同一只,全应用下载预算统一仲裁)。
func (c *Client) waitDL(ctx context.Context, n int64) error {
	if err := c.dlLim.Wait(ctx, n); err != nil {
		return err
	}
	if g := c.cfg.GlobalDown; g != nil {
		return g.Wait(ctx, n)
	}
	return nil
}

// waitUL 上传路径两级限速:任务级先过,再进 BT 会话共用的全局上传限速器。
func (c *Client) waitUL(ctx context.Context, n int64) error {
	if err := c.ulLim.Wait(ctx, n); err != nil {
		return err
	}
	if g := c.cfg.GlobalUp; g != nil {
		return g.Wait(ctx, n)
	}
	return nil
}

// Err 返回会话级错误(落盘失败等)。
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.errV
}

func (c *Client) setErr(err error) {
	c.errMu.Lock()
	c.errV = err
	c.errMu.Unlock()
}

// ---------- 边下边播 ----------

// pieceDoneNotify 广播一次"有 piece 落定":关闭当前代 channel 并换新。
// 闭旧换新天然合并多次落定,等待方醒来重查可用性即可,无事件丢失。
func (c *Client) pieceDoneNotify() {
	c.nmu.Lock()
	if c.nch != nil {
		close(c.nch)
	}
	c.nch = make(chan struct{})
	c.nmu.Unlock()
}

// StreamWaitSignal 返回当前代的落定广播 channel(被关闭即代表有新落定)。
func (c *Client) StreamWaitSignal() <-chan struct{} {
	c.nmu.Lock()
	defer c.nmu.Unlock()
	if c.nch == nil {
		c.nch = make(chan struct{})
	}
	return c.nch
}

// SetStreaming 开启边下边播优先级:pinHead/pinTail 为元数据 piece 区间
// (文件头/尾,覆盖容器头与 moov/cues 索引),ahead 为播放窗口宽度。
func (c *Client) SetStreaming(pinHead, pinTail []int, ahead int) {
	c.picker.EnableStreaming(pinHead, pinTail, ahead)
	c.streamOn.Store(true)
}

// ClearStreaming 关闭边下边播优先级,回到纯稀有优先。幂等。
func (c *Client) ClearStreaming() {
	c.picker.DisableStreaming()
	c.streamOn.Store(false)
}

// Streaming 报告边下边播优先级是否生效。
func (c *Client) Streaming() bool { return c.streamOn.Load() }

// StreamState 导出流模式优先级状态(播放面板分块图:游标/窗口/深度/pin 区间)。
func (c *Client) StreamState() (cursor, ahead, depth int, pinHead, pinTail []int, tailOn bool, ok bool) {
	return c.picker.StreamState()
}

// SetStreamCursorOff 按全局字节偏移推进播放头(拖动 seek 与顺序播放共用;
// 每服务完一个 piece 由引擎调用,窗口随播放头滑移)。
func (c *Client) SetStreamCursorOff(off int64) {
	if off < 0 {
		off = 0
	}
	c.picker.SetStreamCursor(int(off / c.meta.PieceLength))
}

// StreamPieceDone 报告覆盖 [off, off+n) 的 piece 是否全部落定(已校验,
// 数据必在盘上)。流媒体只服务落定 piece:未校验数据可能来自毒源 peer。
func (c *Client) StreamPieceDone(off, n int64) bool {
	if n <= 0 {
		return true
	}
	nPieces := c.meta.PieceCount()
	first := int(off / c.meta.PieceLength)
	last := int((off + n - 1) / c.meta.PieceLength)
	if last >= nPieces {
		last = nPieces - 1
	}
	for i := first; i <= last; i++ {
		if !c.picker.IsDone(i) {
			return false
		}
	}
	return true
}

// ErrStreamNotReady 区间尚有未落定 piece(调用方应等待信号后重试)。
var ErrStreamNotReady = errors.New("流式区间尚未就绪")

// StreamReadOff 读取已落定的数据到 buf(前置 StreamPieceDone)。
// 文件完成瞬间 renameToBare 关旧句柄开新句柄是合法瞬态(ErrFileClosed),
// 重取 store 重试一次;其余错误原样上抛。
func (c *Client) StreamReadOff(off int64, buf []byte) error {
	if !c.StreamPieceDone(off, int64(len(buf))) {
		return ErrStreamNotReady
	}
	st := c.store.Load()
	if st == nil {
		return errors.New("会话未就绪")
	}
	err := st.readFileRange(off, buf, false)
	if errors.Is(err, fs.ErrClosed) {
		if st2 := c.store.Load(); st2 != nil {
			err = st2.readFileRange(off, buf, false)
		}
	}
	return err
}
