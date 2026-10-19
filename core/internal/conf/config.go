// Package conf 管理 ZDownloader 全局配置(JSON 持久化,缺省值合并)。
package conf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zdownloader/internal/util"
)

// Proxy 代理配置。
type Proxy struct {
	Mode string `json:"mode"` // none | manual | env
	URL  string `json:"url"`  // http(s)://host:port 或 socks5://user:pass@host:port
	// 用途粒度开关(仅手动代理生效;指针缺省 = 开启,兼容旧配置文件)
	ForHTTP    *bool `json:"forHTTP,omitempty"`    // HTTP/HTTPS 下载任务
	ForTracker *bool `json:"forTracker,omitempty"` // BT tracker 连接
	ForPeer    *bool `json:"forPeer,omitempty"`    // BT peer 连接
	ForWebSeed *bool `json:"forWebSeed,omitempty"` // BT WebSeed(长效种子)拉取
	// DohDNS DNS 解析走代理(DoH,防本地污染);缺省 = 开启
	DohDNS *bool `json:"dohDNS,omitempty"`
}

func (p Proxy) DNSOn() bool { return boolOn(p.DohDNS) }

func boolOn(b *bool) bool { return b == nil || *b }

func (p Proxy) HTTPOn() bool    { return boolOn(p.ForHTTP) }
func (p Proxy) TrackerOn() bool { return boolOn(p.ForTracker) }
func (p Proxy) PeerOn() bool    { return boolOn(p.ForPeer) }
func (p Proxy) WebSeedOn() bool { return boolOn(p.ForWebSeed) }

type Config struct {
	Port           int    `json:"port"`
	DownloadDir    string `json:"downloadDir"`
	MaxActiveTasks int    `json:"maxActiveTasks"` // 同时进行的任务数
	MaxConnections int    `json:"maxConnections"` // 默认每任务连接数
	UserAgent      string `json:"userAgent"`
	GlobalLimit    int64  `json:"globalLimit"` // 全局限速 字节/秒,0 不限
	TaskLimit      int64  `json:"taskLimit"`   // 默认每任务限速,0 不限
	SkipTLSVerify  bool   `json:"skipTLSVerify"`
	OpenBrowser    bool   `json:"openBrowser"`
	// AutoOpenTorrent HTTP 下载完成的 .torrent 自动创建 BT 任务
	// (设置"文件关联"页可关;缺省开启,旧配置缺字段沿用默认值)
	AutoOpenTorrent bool `json:"autoOpenTorrent"`
	// ShowFloatWindow 桌面版悬浮下载窗(置顶显示总速度与任务进度)。
	// 浏览器版忽略此字段;默认开启,旧配置缺字段时 Load 沿用默认值。
	ShowFloatWindow bool `json:"showFloatWindow"`
	// 悬浮窗位置与尺寸记忆(桌面版拖动/缩放结束后写回,重启恢复)
	FloatX      int  `json:"floatX"`
	FloatY      int  `json:"floatY"`
	FloatW      int  `json:"floatW"` // 0=未手动调整(自动宽度)
	FloatH      int  `json:"floatH"` // 0=未手动调整(随行数自适应)
	FloatPosSet bool `json:"floatPosSet"`
	// 主窗位置与尺寸记忆(桌面版周期采样写回,重启恢复;WinPosSet=false 首次居中)
	WinX      int  `json:"winX"`
	WinY      int  `json:"winY"`
	WinW      int  `json:"winW"`
	WinH      int  `json:"winH"`
	WinPosSet bool `json:"winPosSet"`
	// Theme UI 主题(dark/light),桌面版 toast/悬浮窗配色跟随
	Theme string `json:"theme"`
	Proxy Proxy  `json:"proxy"`
	// RecentDirs 最近使用的保存目录(仅创建对话框里用户手动选择/输入且成功建任务
	// 的路径;自动预填、留空默认、RSS 自动下载不记录)。最新在前,下拉直接展示。
	RecentDirs []string `json:"recentDirs"`
	// BT BT 下载设置(磁力/种子任务、做种)
	BT BTConfig `json:"bt"`
	// RSS RSS 订阅设置(自动更新频率与时段)
	RSS RSSConfig `json:"rss"`
}

// maxRecentDirs 「最近使用的保存目录」下拉最多保留的条数。
const maxRecentDirs = 10

// AddRecentDir 记录一个最近使用的保存目录:去重(Windows 路径大小写不敏感,命中
// 时保留原写法)、最新的排最前、超上限裁掉最旧。copy-on-write 更新切片,不与并发
// 读取 Config() 拷贝的调用方共享底层数组。返回是否发生变化。
func (c *Config) AddRecentDir(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	if dir == "." || dir == ".." || dir == string(filepath.Separator) {
		return false
	}
	front, rest := dir, make([]string, 0, len(c.RecentDirs))
	for _, p := range c.RecentDirs {
		if strings.EqualFold(p, dir) {
			front = p // 命中已有条目:保留原写法,仅提到最前
			continue
		}
		rest = append(rest, p)
	}
	out := make([]string, 0, len(rest)+1)
	out = append(out, front)
	out = append(out, rest...)
	if len(out) > maxRecentDirs {
		out = out[:maxRecentDirs]
	}
	changed := len(out) != len(c.RecentDirs)
	if !changed {
		for i := range out {
			if out[i] != c.RecentDirs[i] {
				changed = true
				break
			}
		}
	}
	c.RecentDirs = out
	return changed
}

// RemoveRecentDir 从最近使用列表中删除一个目录(大小写不敏感),返回是否发生变化。
// copy-on-write,与 AddRecentDir 一致。
func (c *Config) RemoveRecentDir(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" || len(c.RecentDirs) == 0 {
		return false
	}
	dir = filepath.Clean(dir) // 与 AddRecentDir 同一归一化(尾斜杠等)
	out := make([]string, 0, len(c.RecentDirs)-1)
	removed := false
	for _, p := range c.RecentDirs {
		if !removed && strings.EqualFold(p, dir) {
			removed = true // 列表本就无重复,只删首个命中
			continue
		}
		out = append(out, p)
	}
	if !removed {
		return false
	}
	c.RecentDirs = out
	return true
}

// RSSConfig 是 RSS 订阅的全局设置。
type RSSConfig struct {
	AutoUpdate    bool   `json:"autoUpdate"`    // 定时自动更新
	IntervalMin   int    `json:"intervalMin"`   // 更新间隔(分钟)
	UpdateOnStart bool   `json:"updateOnStart"` // 启动后立即更新一次
	WindowEnabled bool   `json:"windowEnabled"` // 仅在指定时间段内自动更新
	WindowStart   string `json:"windowStart"`   // "HH:MM"
	WindowEnd     string `json:"windowEnd"`     // "HH:MM"
	MaxItems      int    `json:"maxItems"`      // 每订阅保留条目数
	// AutoDL 全局自动下载模式:off = 不自动下载(默认),notify = 自动下载
	// 完成后提醒,silent = 静默自动下载。订阅的 per-feed 设置可设为继承全局。
	AutoDL string `json:"autoDl"`
}

// DefaultRSS 返回 RSS 默认设置;旧配置缺字段时 Load 以此为底合并。
func DefaultRSS() RSSConfig {
	return RSSConfig{
		AutoUpdate:    true,
		IntervalMin:   60,
		UpdateOnStart: true,
		WindowStart:   "00:00",
		WindowEnd:     "23:59",
		MaxItems:      200,
		AutoDL:        "off",
	}
}

// Interval 自动更新间隔。
func (r RSSConfig) Interval() time.Duration {
	return time.Duration(r.IntervalMin) * time.Minute
}

// InWindow 判断时刻是否落在允许自动更新的时段内;支持跨天窗口(如 23:00-06:00)。
func (r RSSConfig) InWindow(t time.Time) bool {
	if !r.WindowEnabled {
		return true
	}
	start, ok1 := parseClock(r.WindowStart)
	end, ok2 := parseClock(r.WindowEnd)
	if !ok1 || !ok2 || start == end {
		return true
	}
	cur := t.Hour()*60 + t.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

// parseClock 解析 "HH:MM" 为当日分钟数。
func parseClock(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// BTConfig 是 BT 下载的全局设置。
type BTConfig struct {
	Port        int     `json:"port"`     // BT 监听端口,0 = 自动
	MaxPeers    int     `json:"maxPeers"` // 单任务最大 peer 连接数
	UPSpeed     int64   `json:"upSpeed"`  // 全局上传限速 B/s,0 不限
	DHT         bool    `json:"dht"`
	PEX         bool    `json:"pex"`
	AutoSeed    bool    `json:"autoSeed"`    // 完成后自动做种
	SeedRatio   float64 `json:"seedRatio"`   // 目标分享率,0 = 不按比率停
	SeedMinutes int     `json:"seedMinutes"` // 最短做种时长(分),0 = 不按时长停
	// DeleteGeneratedTorrent 创建 BT 任务后删除本程序生成的种子文件:磁链抓取
	// 产物 / HTTP 下载转的种子。引擎保留内部存档(重启/做种/另存为都靠它),
	// 删的只是可见副本,详情页可随时「另存为种子」取回。
	DeleteGeneratedTorrent bool `json:"deleteGeneratedTorrent"`
	// DeleteSourceTorrent 创建 BT 任务后删除双击/手动打开的源种子文件。默认
	// 关闭——多数用户想保留自己的种子库,故单独一个开关。
	DeleteSourceTorrent bool     `json:"deleteSourceTorrent"`
	PublicTrackers      []string `json:"publicTrackers"`  // 公共 tracker 列表;磁力元数据抓取始终使用
	AutoAddTrackers     bool     `json:"autoAddTrackers"` // 下载阶段自动把公共列表追加到非私有 BT 任务
	SpoofUA             bool     `json:"spoofUA"`         // BT 客户端伪装名继承全局 User-Agent(默认)
	SpoofName           string   `json:"spoofName"`       // 独立伪装名(SpoofUA=false 时生效)
	AutoBanPoison       bool     `json:"autoBanPoison"`   // 坏块归因自动封禁:供坏块的 peer 拉黑(PCDN 毒源防护)
	PoisonRate          int      `json:"poisonRate"`      // 坏块白名单 piece 有效率阈值(%),默认 80,钳 50~100
	PoisonStrikes       int      `json:"poisonStrikes"`   // 坏块宽容次数:同一 IP 累计供坏块达该次数封禁,默认 3,钳 1~10
	// ClientBlacklist 客户端黑名单:通配符模式(* 任意长度、? 单字符,大小写
	// 不敏感)匹配对端客户端名,命中即写入全局封禁名单并断开;空 = 不启用
	ClientBlacklist []string `json:"clientBlacklist"`
}

// EffectiveSpoofName 返回 BT 对外呈现的客户端名(扩展握手 v 字段、
// tracker 通告 UA、制种 created by)。继承 User-Agent 时为空回退默认。
func (b BTConfig) EffectiveSpoofName(ua string) string {
	const fallback = "BitComet 1.99"
	if b.SpoofUA {
		if s := strings.TrimSpace(ua); s != "" {
			return s
		}
		return fallback
	}
	if s := strings.TrimSpace(b.SpoofName); s != "" {
		return s
	}
	return fallback
}

// DefaultBT 返回 BT 设置默认值;旧配置缺字段时 Load 以此为底合并。
func DefaultBT() BTConfig {
	return BTConfig{
		MaxPeers:        60,
		DHT:             true,
		PEX:             true,
		AutoSeed:        true,
		SeedRatio:       1.0,
		AutoAddTrackers: true,
		SpoofUA:         true,
		SpoofName:       "BitComet 1.99",
		AutoBanPoison:   true,
		PoisonRate:      80,
		PoisonStrikes:   3,
		ClientBlacklist: []string{},
		PublicTrackers: []string{
			"udp://tracker.opentrackr.org:1337/announce",
			"udp://open.tracker.cl:1337/announce",
			"udp://tracker.openbittorrent.com:6969/announce",
			"udp://exodus.desync.com:6969/announce",
			"udp://www.torrent.eu.org:451/announce",
			"udp://opentracker.io:6969/announce",
			"http://tracker.openbittorrent.com:80/announce",
			"http://open.acgnxtracker.com:80/announce",
		},
	}
}

func Default(downloadDir string) Config {
	return Config{
		Port:            8620,
		DownloadDir:     downloadDir,
		MaxActiveTasks:  3,
		MaxConnections:  8,
		UserAgent:       "ZDownloader/1.0",
		OpenBrowser:     true,
		AutoOpenTorrent: true,
		ShowFloatWindow: true,
		Proxy:           Proxy{Mode: "none"},
		BT:              DefaultBT(),
		RSS:             DefaultRSS(),
	}
}

// Load 读取配置;文件缺失或字段缺失时使用默认值。
func Load(path, defaultDownloadDir string) Config {
	c := Default(defaultDownloadDir)
	_ = util.ReadJSON(path, &c) // 读取失败(含不存在)时保留默认值
	c.normalize()
	return c
}

func (c *Config) Save(path string) error {
	c.normalize()
	return util.WriteJSONAtomic(path, c)
}

func (c *Config) normalize() {
	if c.Port < 1 || c.Port > 65535 {
		c.Port = 8620
	}
	if c.MaxActiveTasks < 1 {
		c.MaxActiveTasks = 1
	}
	if c.MaxActiveTasks > 10 {
		c.MaxActiveTasks = 10
	}
	if c.MaxConnections < 1 {
		c.MaxConnections = 1
	}
	if c.MaxConnections > 64 {
		c.MaxConnections = 64
	}
	if c.GlobalLimit < 0 {
		c.GlobalLimit = 0
	}
	if c.TaskLimit < 0 {
		c.TaskLimit = 0
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = "ZDownloader/1.0"
	}
	if c.DownloadDir == "" {
		c.DownloadDir = defaultDownloadDir()
	}
	switch c.Proxy.Mode {
	case "none", "manual", "env":
	default:
		c.Proxy.Mode = "none"
	}
	if strings.TrimSpace(c.BT.SpoofName) == "" {
		c.BT.SpoofName = "BitComet 1.99"
	}
	// 坏块白名单有效率阈值:默认 80%,钳在 50 ~ 100(低于 50 相当于让
	// 多数供坏块的 peer 继续豁免,与防护目的相悖)
	if c.BT.PoisonRate <= 0 {
		c.BT.PoisonRate = 80
	}
	if c.BT.PoisonRate < 50 {
		c.BT.PoisonRate = 50
	}
	if c.BT.PoisonRate > 100 {
		c.BT.PoisonRate = 100
	}
	// 坏块宽容次数:默认 3 次,钳在 1 ~ 10(1 = 掺一片即封;上限防把自动
	// 封禁事实关掉,真要只观察请关 AutoBanPoison 开关)
	if c.BT.PoisonStrikes <= 0 {
		c.BT.PoisonStrikes = 3
	}
	if c.BT.PoisonStrikes > 10 {
		c.BT.PoisonStrikes = 10
	}
	// 客户端黑名单:去首尾空白、丢空行(模式本身不校验,通配符语义见
	// bt.matchWildcard;空表 = 不启用)
	if c.BT.ClientBlacklist != nil {
		out := make([]string, 0, len(c.BT.ClientBlacklist))
		for _, p := range c.BT.ClientBlacklist {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		c.BT.ClientBlacklist = out
	}
	// RSS:间隔 5 分钟 ~ 24 小时;保留条数 20 ~ 5000
	if c.RSS.IntervalMin < 5 {
		c.RSS.IntervalMin = 5
	}
	if c.RSS.IntervalMin > 1440 {
		c.RSS.IntervalMin = 1440
	}
	if c.RSS.MaxItems < 20 {
		c.RSS.MaxItems = 20
	}
	if c.RSS.MaxItems > 5000 {
		c.RSS.MaxItems = 5000
	}
	// RSS 全局自动下载模式:仅认 off/notify/silent,其余(含空)回退 off
	switch c.RSS.AutoDL {
	case "off", "notify", "silent":
	default:
		c.RSS.AutoDL = "off"
	}
	if _, ok := parseClock(c.RSS.WindowStart); !ok {
		c.RSS.WindowStart = "00:00"
	}
	if _, ok := parseClock(c.RSS.WindowEnd); !ok {
		c.RSS.WindowEnd = "23:59"
	}
}

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, "Downloads")
}
