// Package engine 是 ZDownloader 的下载核心:HTTP 多线程分段下载、HLS 流媒体、
// 断点续传、调度与状态持久化。
package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/bt"
)

type Status string

const (
	StatusWaiting   Status = "waiting"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusError     Status = "error"
	StatusCanceled  Status = "canceled"
	StatusSeeding   Status = "seeding"  // BT:下载完成,正在做种
	StatusChoosing  Status = "choosing" // BT:元数据就绪,等待用户选择下载文件
)

// TaskKind 任务类型。
const (
	KindHTTP = "http"
	KindHLS  = "hls"
	KindBT   = "bt"
)

// Segment 表示一个下载区间,Start/End 为文件内绝对偏移(闭区间),Done 为已写入字节数。
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

func (s Segment) Len() int64 { return s.End - s.Start + 1 }
func (s Segment) Remaining() int64 {
	if r := s.Len() - s.Done; r > 0 {
		return r
	}
	return 0
}

// Task 是一个下载任务的全部持久化状态。
type Task struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	FinalURL    string    `json:"finalUrl,omitempty"`
	Filename    string    `json:"filename"`
	Dir         string    `json:"dir"`
	Size        int64     `json:"size"` // -1 表示未知
	Downloaded  int64     `json:"downloaded"`
	Connections int       `json:"connections"`
	Status      Status    `json:"status"`
	Err         string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	CompletedAt time.Time `json:"completedAt,omitempty"`

	// 续传校验
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	AcceptRanges bool   `json:"acceptRanges"`

	// 请求定制
	Headers       map[string]string `json:"headers,omitempty"`
	Referer       string            `json:"referer,omitempty"`
	ProxyOverride string            `json:"proxyOverride,omitempty"` // ""=跟随全局, "none"=直连, 或代理 URL
	SpeedLimit    int64             `json:"speedLimit,omitempty"`    // 字节/秒,0 不限
	Checksum      string            `json:"checksum,omitempty"`      // md5:/sha1:/sha256:/crc32:
	// Source 任务来源标记("rss-notify"/"rss-silent" = RSS 自动下载创建;
	// 前端按 rss-notify 在任务完成时提醒)。空 = 用户手动创建。
	Source string `json:"source,omitempty"`

	// HLS 专用
	SegTotal  int  `json:"segTotal,omitempty"`
	SegDone   int  `json:"segDone,omitempty"`
	Encrypted bool `json:"encrypted,omitempty"`

	// BT 专用
	InfoHash    string             `json:"infoHash,omitempty"`    // 40 位十六进制
	Trackers    []string           `json:"trackers,omitempty"`    // 任务级 tracker(torrent 内置之外追加)
	TorrentPath string             `json:"torrentPath,omitempty"` // .torrent 存档(tasks 目录内)
	Uploaded    int64              `json:"uploaded,omitempty"`    // 累计上传量(跨重启累计,分享率用)
	UPLimit     int64              `json:"upLimit,omitempty"`     // 每任务上传限速 B/s,0 跟随全局
	SeedRatio   float64            `json:"seedRatio,omitempty"`   // 任务级做种策略覆盖,0 跟随全局
	SeedMinutes int                `json:"seedMinutes,omitempty"` // 任务级做种时长覆盖,0 跟随全局
	PiecesTotal int                `json:"piecesTotal,omitempty"`
	PiecesDone  int                `json:"piecesDone,omitempty"`
	Arrived     int64              `json:"arrived,omitempty"`     // BT:已落盘去重字节(持久化;暂停/重启后进度条口径)
	Bitfield    string             `json:"bitfield,omitempty"`    // base64 piece 位图(续传)
	BlockBits   string             `json:"blockBits,omitempty"`   // base64 未完成 piece 的块级位图(暂停/重启只补缺块,不整 piece 重下)
	PeerHistory []bt.PeerHistEntry `json:"peerHistory,omitempty"` // peer 历史(重启回灌;对照 BitComet 历史来源)
	SeedStarted time.Time          `json:"seedStarted,omitempty"` // 本次做种开始时间
	WantedFiles []string           `json:"wantedFiles,omitempty"` // 文件选择下载:选中的相对路径(空 = 全部)
	// ChoosePending 磁力创建时要求"下载前选择文件":元数据就绪后挂起为
	// choosing 状态(单文件种子直接放行)。确认选择或点继续后清除。
	ChoosePending bool `json:"choosePending,omitempty"`
	// HoldAny 关联启动(ChooseAny)专用:单文件也挂起——用户至少要能
	// 选保存目录。仅影响 btHoldForChoose 的单文件放行判断,挂起即清除。
	HoldAny bool `json:"holdAny,omitempty"`
	// TorrentFile 用户可见的 .torrent 副本(下载目录/<名>.torrent),
	// choosing 挂起时落盘:创建对话框据此预填,任务删除后仍可再创建。
	TorrentFile string `json:"torrentFile,omitempty"`

	// ---- 运行时字段 ----
	mu          sync.Mutex
	Segs        []Segment `json:"segs,omitempty"` // 分块快照,持久化与恢复用
	speed       int64
	lastBytes   int64
	speedPrimed bool // 速度采样已立基线(重启后首个样本只记基线不作差,见 speedStep)
	upSpeed     int64
	lastUp      int64
	upPrimed    bool // 上传速度采样基线(口径同 speedPrimed;数据源为 BT 会话累计上传量)
	cancel      context.CancelFunc
	removed     bool
	pool        *segPool
	limits      *taskLimiter // 每任务限速句柄(可热更新),运行时创建
	runID       int64        // 每次运行递增,防止旧运行协程退出时覆盖新一轮状态

	// BT 运行时
	btcl         *bt.Client    // 当前会话(下载/做种)
	uploadBase   int64         // 本次会话前的历史累计上传量
	downloadBase int64         // 本次会话前的历史累计接收量(进度=基准+会话接收,校验前也可见)
	seedStop     chan struct{} // 关闭即停止做种协程
	arrived      int64         // 已落盘去重字节(运行时实时值;与导出字段 Arrived 同步,随 JSON 持久化)
	delData      bool          // 删除任务时是否连最终数据一起删(做种协程收尾后执行,避开句柄占用)
	stream       *taskStream   // 边下边播运行时会话(nil=未开启;见 bt_stream.go,不持久化)
}

// TaskView 是对外的只读快照(go vet 不允许拷贝含锁结构体)。
type TaskView struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	URL           string            `json:"url"`
	FinalURL      string            `json:"finalUrl,omitempty"`
	Filename      string            `json:"filename"`
	Dir           string            `json:"dir"`
	Size          int64             `json:"size"`
	Downloaded    int64             `json:"downloaded"`
	Connections   int               `json:"connections"`
	Status        Status            `json:"status"`
	Err           string            `json:"error,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	CompletedAt   time.Time         `json:"completedAt,omitempty"`
	Speed         int64             `json:"speed"`
	UpSpeed       int64             `json:"upSpeed,omitempty"` // 实时上传速度(BT;做种态=Speed,运行态按累计上传量增量)
	AcceptRanges  bool              `json:"acceptRanges"`
	SpeedLimit    int64             `json:"speedLimit,omitempty"`
	Checksum      string            `json:"checksum,omitempty"`
	Source        string            `json:"source,omitempty"` // "rss-notify"/"rss-silent" = RSS 自动下载
	Referer       string            `json:"referer,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	SegTotal      int               `json:"segTotal"`
	SegDone       int               `json:"segDone"`
	Encrypted     bool              `json:"encrypted,omitempty"`
	ProxyOverride string            `json:"proxyOverride,omitempty"`
	Segments      []Segment         `json:"segments,omitempty"`
	// BT 展示字段
	InfoHash    string   `json:"infoHash,omitempty"`
	Trackers    []string `json:"trackers,omitempty"`
	Uploaded    int64    `json:"uploaded,omitempty"`
	UPLimit     int64    `json:"upLimit,omitempty"`
	SeedRatio   float64  `json:"seedRatio,omitempty"`
	SeedMinutes int      `json:"seedMinutes,omitempty"`
	PiecesTotal int      `json:"piecesTotal,omitempty"`
	PiecesDone  int      `json:"piecesDone,omitempty"`
	Arrived     int64    `json:"arrived,omitempty"` // BT:已落盘去重字节(进度条口径;HTTP/HLS 恒 0,UI 回退 downloaded)
	// TorrentFile 用户可见的 .torrent 副本路径(磁力 choosing 任务;UI 预填创建对话框)
	TorrentFile string `json:"torrentFile,omitempty"`
}

func (t *Task) partPath() string  { return filepath.Join(t.Dir, t.Filename+".zdl.part") }
func (t *Task) finalPath() string { return filepath.Join(t.Dir, t.Filename) }

// View 生成线程安全的只读快照。
func (t *Task) View() TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskView{
		ID: t.ID, Kind: t.Kind, URL: t.URL, FinalURL: t.FinalURL,
		Filename: t.Filename, Dir: t.Dir, Size: t.Size, Downloaded: t.Downloaded,
		Connections: t.Connections, Status: t.Status, Err: t.Err,
		CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt, Speed: t.speed,
		UpSpeed:      t.upSpeed,
		AcceptRanges: t.AcceptRanges, SpeedLimit: t.SpeedLimit, Checksum: t.Checksum,
		Referer: t.Referer, Headers: t.Headers,
		Source:   t.Source,
		SegTotal: t.SegTotal, SegDone: t.SegDone, Encrypted: t.Encrypted,
		ProxyOverride: t.ProxyOverride, Segments: t.Segs,
		InfoHash: t.InfoHash, Trackers: t.Trackers, Uploaded: t.Uploaded,
		UPLimit: t.UPLimit, SeedRatio: t.SeedRatio, SeedMinutes: t.SeedMinutes,
		PiecesTotal: t.PiecesTotal, PiecesDone: t.PiecesDone, Arrived: t.arrived,
		TorrentFile: t.TorrentFile,
	}
}

// CreateReq 创建任务请求(URL 支持批量)。
type CreateReq struct {
	URLs        []string          `json:"urls"`
	URL         string            `json:"url"`
	Filename    string            `json:"filename"` // 指定文件名(空则探测时自动推导)
	Dir         string            `json:"dir"`      // 保存目录(绝对路径,或相对默认下载目录)
	Connections int               `json:"connections"`
	Headers     map[string]string `json:"headers"`
	Referer     string            `json:"referer"`
	Proxy       string            `json:"proxy"`
	SpeedLimit  int64             `json:"speedLimit"`
	Checksum    string            `json:"checksum"`
	Kind        string            `json:"kind"`
	Source      string            `json:"source,omitempty"` // 任务来源标记(RSS 自动下载;完成提醒用)

	// BT 创建参数
	Magnet      string `json:"magnet"`      // 磁力链接(与 torrentB64/torrentPath 三选一)
	TorrentB64  string `json:"torrentB64"`  // base64 编码的 .torrent 内容
	TorrentPath string `json:"torrentPath"` // 本机 .torrent 路径(创建对话框预填磁链产物)
	// TorrentGenerated 标记 TorrentPath 指向的是本程序生成的种子(HTTP 下载完成
	// 的种子自动转建;磁链 choosing 产物归档路径在 tasksDir 下,无需此标记即可
	// 判定),而非用户双击/手动打开的源文件。决定"创建后删除种子"走哪个开关。
	TorrentGenerated bool     `json:"torrentGenerated"`
	Trackers         []string `json:"trackers"`  // 追加 tracker
	Files            []string `json:"files"`     // 文件选择下载:选中的相对路径(空 = 全部)
	Choose           bool     `json:"choose"`    // 磁力:元数据就绪后挂起等待用户选择文件
	ChooseAny        bool     `json:"chooseAny"` // 关联启动:单文件也挂起(至少可选保存目录),需与 Choose 同用
	UPLimit          int64    `json:"upLimit"`
	SeedRatio        float64  `json:"seedRatio"`
	SeedMinutes      int      `json:"seedMinutes"`
}

// URLList 返回去重、去空后的 URL 列表。
func (r *CreateReq) URLList() []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range r.URLs {
		add(u)
	}
	add(r.URL)
	return out
}
