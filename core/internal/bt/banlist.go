package bt

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"time"
)

// BanEntry 一条全局封禁记录。
type BanEntry struct {
	IP     string    `json:"ip"`
	Time   time.Time `json:"time"`   // 最近一次封禁时间
	Reason string    `json:"reason"` // 封禁原因(手动封禁 / 毒源描述)
	Client string    `json:"client,omitempty"` // 对端客户端名(封禁时抓取,便于分辨)
	Count  int       `json:"count"`  // 累计封禁次数(重复犯加一)
}

// BanList 全局封禁名单:全部 BT 会话共用一份,跨任务复用并持久化到
// JSON 文件(进程重启后仍生效)。封禁即时广播给注册在案的会话,断开
// 该 IP 的现存连接;拨号与入站在检查点直接查本表。
// path 为空 = 仅内存(单测)。
type BanList struct {
	mu      sync.Mutex
	entries map[string]BanEntry
	path    string
	clients map[*Client]struct{}
}

// NewBanList 创建全局封禁表;path 非空时加载既有记录。
func NewBanList(path string) *BanList {
	l := &BanList{
		entries: map[string]BanEntry{},
		path:    path,
		clients: map[*Client]struct{}{},
	}
	if path == "" {
		return l
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	var saved struct {
		Bans []BanEntry `json:"bans"`
	}
	if json.Unmarshal(b, &saved) != nil {
		log.Printf("[bt] 封禁列表 %s 解析失败,忽略旧记录", path)
		return l
	}
	for _, e := range saved.Bans {
		if e.IP != "" {
			if e.Count < 1 {
				e.Count = 1
			}
			l.entries[e.IP] = e
		}
	}
	return l
}

// save 持久化(调用方须持锁)。写临时文件再改名,避免半截文件。
func (l *BanList) save() {
	if l.path == "" {
		return
	}
	out := struct {
		Bans []BanEntry `json:"bans"`
	}{Bans: l.listLocked()}
	b, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[bt] 封禁列表写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("[bt] 封禁列表落盘失败: %v", err)
	}
}

func (l *BanList) listLocked() []BanEntry {
	out := make([]BanEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time) // 新封的在前
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// attach 会话注册:封禁发生时立即断开其现存连接。
func (l *BanList) attach(c *Client) {
	l.mu.Lock()
	l.clients[c] = struct{}{}
	l.mu.Unlock()
}

// detach 会话注销。
func (l *BanList) detach(c *Client) {
	l.mu.Lock()
	delete(l.clients, c)
	l.mu.Unlock()
}

// Ban 封禁一个 IP(已封禁则更新时间/原因并累加次数),广播断连并落盘。
// client 为对端客户端名(封禁时从现存连接抓取;可为空)。
func (l *BanList) Ban(ip, reason, client string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	e, existed := l.entries[ip]
	e.IP = ip
	e.Time = time.Now()
	e.Reason = reason
	if client != "" || e.Client == "" {
		e.Client = client
	}
	e.Count++
	l.entries[ip] = e
	kicked := 0
	for c := range l.clients {
		kicked += c.kickIP(ip)
	}
	l.save()
	l.mu.Unlock()
	if existed {
		log.Printf("[bt] 全局封禁 %s(第 %d 次,原因: %s,客户端 %s;断开 %d 个现存连接)", ip, e.Count, reason, displayClient(e.Client), kicked)
	} else {
		log.Printf("[bt] 全局封禁 %s(原因: %s,客户端 %s;断开 %d 个现存连接)", ip, reason, displayClient(e.Client), kicked)
	}
}

// displayClient 封禁日志用:未知客户端显示占位。
func displayClient(name string) string {
	if name == "" {
		return "未知"
	}
	return name
}

// Banned 查询 IP 是否被封。
func (l *BanList) Banned(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.entries[ip]
	return ok
}

// Remove 解除单个 IP 的封禁(下次再犯重新计次)。
func (l *BanList) Remove(ip string) bool {
	l.mu.Lock()
	_, ok := l.entries[ip]
	if ok {
		delete(l.entries, ip)
		l.save()
	}
	l.mu.Unlock()
	return ok
}

// Clear 清空全部封禁记录,返回清除条数。
func (l *BanList) Clear() int {
	l.mu.Lock()
	n := len(l.entries)
	if n > 0 {
		l.entries = map[string]BanEntry{}
		l.save()
	}
	l.mu.Unlock()
	return n
}

// List 返回按封禁时间倒序的快照(展示用)。
func (l *BanList) List() []BanEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listLocked()
}

// kickIP 断开会话内该 IP 的全部现存连接,返回断开数。
func (c *Client) kickIP(ip string) int {
	c.pMu.Lock()
	var kick []*peerConn
	for addr, pc := range c.peers {
		if host, _, err := net.SplitHostPort(addr); err == nil && host == ip {
			kick = append(kick, pc)
		}
	}
	c.pMu.Unlock()
	for _, pc := range kick {
		pc.shutdown()
	}
	return len(kick)
}
