//go:build darwin

package proxy

// macOS 系统代理读取:scutil --proxy 输出系统设置 → 网络 → 代理 的当前配置
// (存于 /Library/Preferences/SystemConfiguration,与"自动配置系统代理"工具写入的一致)。
// 仅静态代理;PAC 脚本不解析——零依赖下无法执行脚本,与 Windows 侧同理。

import (
	"os/exec"
	"strconv"
	"strings"
)

// sysProxySourceName FollowSystemStatus 报告的来源标识(followsystem.go 消费)。
const sysProxySourceName = "macos"

// sysProxyInfo 是 scutil 读出的系统代理配置。
type sysProxyInfo struct {
	Server string   // host:port
	Socks  bool     // 仅 SOCKS 启用时为 true(socks5)
	Bypass []string // ExceptionsList 例外列表;ExcludeSimpleHostnames 计入 "<local>"
}

// readSysProxy 读当前系统代理;未启用或 scutil 异常返回 nil。
func readSysProxy() *sysProxyInfo {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return nil
	}
	return parseScutilProxy(string(out))
}

// parseScutilProxy 解析 scutil --proxy 输出。顶层键值形如 "  HTTPEnable : 1";
// ExceptionsList 为嵌套数组(每元素一行 "N : value",以 "}" 收束)。
// 协议优先级与 Windows 侧 parseProxyServerValue 一致:HTTPS > HTTP,
// 仅 SOCKS 启用时按 SOCKS5 返回。
func parseScutilProxy(out string) *sysProxyInfo {
	var (
		httpEn, httpsEn, socksEn       bool
		httpHost, httpsHost, socksHost string
		httpPort, httpsPort, socksPort int
		excludeSimple                  bool
		inExceptions                   bool
		bypass                         []string
	)
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if inExceptions {
			if trimmed == "}" {
				inExceptions = false
				continue
			}
			if _, v, ok := strings.Cut(trimmed, " : "); ok {
				if v = strings.TrimSpace(v); v != "" {
					bypass = append(bypass, v)
				}
			}
			continue
		}
		k, v, ok := strings.Cut(trimmed, " : ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "HTTPEnable":
			httpEn = v == "1"
		case "HTTPSEnable":
			httpsEn = v == "1"
		case "SOCKSEnable":
			socksEn = v == "1"
		case "HTTPProxy":
			httpHost = v
		case "HTTPPort":
			httpPort, _ = strconv.Atoi(v)
		case "HTTPSProxy":
			httpsHost = v
		case "HTTPSPort":
			httpsPort, _ = strconv.Atoi(v)
		case "SOCKSProxy":
			socksHost = v
		case "SOCKSPort":
			socksPort, _ = strconv.Atoi(v)
		case "ExcludeSimpleHostnames":
			excludeSimple = v == "1"
		case "ExceptionsList":
			if strings.HasPrefix(v, "<array>") {
				inExceptions = true
			}
		}
	}
	hostPort := func(en bool, h string, p int) string {
		if !en || h == "" || p <= 0 || p > 65535 {
			return ""
		}
		return h + ":" + strconv.Itoa(p)
	}
	var info *sysProxyInfo
	if s := hostPort(httpsEn, httpsHost, httpsPort); s != "" {
		info = &sysProxyInfo{Server: s}
	} else if s := hostPort(httpEn, httpHost, httpPort); s != "" {
		info = &sysProxyInfo{Server: s}
	} else if s := hostPort(socksEn, socksHost, socksPort); s != "" {
		info = &sysProxyInfo{Server: s, Socks: true}
	} else {
		return nil
	}
	if excludeSimple {
		info.Bypass = append(info.Bypass, "<local>")
	}
	info.Bypass = append(info.Bypass, bypass...)
	return info
}
