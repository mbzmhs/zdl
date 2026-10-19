package proxy

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// "跟随系统"代理的统一解析:环境变量优先,其次 Windows 系统代理(注册表)。
// 结果带 15 秒缓存——用户在代理客户端/Internet 选项切换后最多延迟一个
// TTL 生效,同时避免 BT 拨号风暴时反复查注册表。

const followSysTTL = 15 * time.Second

var (
	followMu  sync.Mutex
	followAt  time.Time
	followEnv string        // 原始环境变量值(可能为空)
	followSys *sysProxyInfo // Windows 系统代理(非 Windows 恒 nil)
)

// followGet 返回缓存的一轮解析结果。
func followGet() (envStr string, sys *sysProxyInfo) {
	followMu.Lock()
	defer followMu.Unlock()
	if time.Since(followAt) > followSysTTL {
		followAt = time.Now()
		followEnv = envProxyForDial()
		followSys = readSysProxy()
	}
	return followEnv, followSys
}

// followRefresh 强制重新解析(设置页回显用,保证刚切换的配置立即可见)。
func followRefresh() (envStr string, sys *sysProxyInfo) {
	followMu.Lock()
	defer followMu.Unlock()
	followAt = time.Now()
	followEnv = envProxyForDial()
	followSys = readSysProxy()
	return followEnv, followSys
}

// FollowSystemProxy 解析当前应使用的代理 URL;nil = 直连。
// 返回的 URL 已归一化(无 scheme 的环境变量值按 http 代理处理)。
func FollowSystemProxy() *url.URL {
	envStr, sys := followGet()
	if envStr != "" {
		return normalizeProxyURL(envStr)
	}
	if sys != nil {
		scheme := "http"
		if sys.Socks {
			scheme = "socks5"
		}
		return normalizeProxyURL(scheme + "://" + sys.Server)
	}
	return nil
}

// followBypass 返回当前实际来源的例外列表:环境变量来源用 NO_PROXY 语义
// (由 noProxyMatch 处理,返回 nil),注册表来源返回 ProxyOverride。
func followBypass() []string {
	envStr, sys := followGet()
	if envStr != "" {
		return nil
	}
	if sys != nil {
		return sys.Bypass
	}
	return nil
}

// SystemProxyStatus 供设置页只读回显"跟随系统"实际检测到的代理。
type SystemProxyStatus struct {
	Source string `json:"source"` // ""=未检测到;"env"=环境变量;sysProxySourceName=平台系统代理(Windows 注册表 / macOS scutil)
	EnvVar string `json:"envVar,omitempty"`
	Addr   string `json:"addr,omitempty"`
	URL    string `json:"url,omitempty"`
}

// FollowSystemStatus 实时检测并返回展示信息(绕过缓存)。
func FollowSystemStatus() SystemProxyStatus {
	envStr, envVar := envProxyWithSource()
	if envStr != "" {
		if u := normalizeProxyURL(envStr); u != nil {
			return SystemProxyStatus{Source: "env", EnvVar: envVar, Addr: envStr, URL: u.String()}
		}
		return SystemProxyStatus{Source: "env", EnvVar: envVar, Addr: envStr}
	}
	if sys := readSysProxy(); sys != nil {
		scheme := "http"
		if sys.Socks {
			scheme = "socks5"
		}
		if u := normalizeProxyURL(scheme + "://" + sys.Server); u != nil {
			return SystemProxyStatus{Source: sysProxySourceName, Addr: sys.Server, URL: u.String()}
		}
	}
	return SystemProxyStatus{}
}

// sysByPassMatch 判断 host 是否命中系统代理例外列表(Windows ProxyOverride /
// macOS ExceptionsList 同语义):"*" 全部直连;"<local>" 为不带点的主机名;
// 其余按精确 / 后缀匹配(前导 "." 或 "*" 均视为后缀通配)。
// CIDR 写法(如 macOS 常见的 "127.0.0.0/8")不在此解析——回环/内网已由
// isLANAddr 直接放行,该条目形同冗余。
func sysByPassMatch(bypass []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, p := range bypass {
		if strings.EqualFold(p, "*") {
			return true
		}
		if strings.EqualFold(p, "<local>") && !strings.Contains(host, ".") {
			return true
		}
		p = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "*"), "."))
		if p != "" && (host == p || strings.HasSuffix(host, "."+p)) {
			return true
		}
	}
	return false
}

// normalizeProxyURL 归一化代理描述为可解析 URL:
// 缺 scheme 的值按 HTTP 代理补 "http://";"socks://" 视为 "socks5://"。
func normalizeProxyURL(raw string) *url.URL {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	if strings.HasPrefix(strings.ToLower(v), "socks://") {
		v = "socks5://" + v[len("socks://"):]
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}
