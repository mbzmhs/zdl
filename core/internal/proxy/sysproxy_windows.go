//go:build windows

package proxy

import (
	"strings"
	"syscall"
	"unsafe"
)

// Windows 系统代理(Internet 选项 / WinINET)读取:注册表
// HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings 的
// ProxyEnable / ProxyServer / ProxyOverride。
// Windows 的"系统代理"不通过环境变量暴露,浏览器等读的是注册表;
// "跟随系统"模式两者都要认(环境变量优先)。
// 仅走静态代理;PAC 脚本(AutoConfigURL)不解析——零依赖下无法执行脚本。

const (
	sysProxySubKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

	regHKCU   = 0x80000001 // HKEY_CURRENT_USER
	regRead   = 0x20019    // KEY_READ
	regTypeSZ = 1          // REG_SZ
	regTypeDW = 4          // REG_DWORD
)

var (
	modadvapi32          = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW    = modadvapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = modadvapi32.NewProc("RegQueryValueExW")
	procRegCloseKey      = modadvapi32.NewProc("RegCloseKey")
)

// sysProxySourceName FollowSystemStatus 报告的来源标识(followsystem.go 消费)。
const sysProxySourceName = "windows"

// sysProxyInfo 是注册表里读出的系统代理配置。
type sysProxyInfo struct {
	Server string   // host:port(分协议格式已归一)
	Socks  bool     // 仅 socks= 条目时为 true(socks5)
	Bypass []string // ProxyOverride 例外列表(分号拆分;"<local>" 原样保留)
}

// readSysProxy 读当前系统代理;未启用或配置异常返回 nil。
func readSysProxy() *sysProxyInfo {
	h, err := regOpenKey(sysProxySubKey)
	if err != nil {
		return nil
	}
	defer procRegCloseKey.Call(uintptr(h))
	if en, _ := regQueryDword(h, "ProxyEnable"); en == 0 {
		return nil
	}
	server, socks := parseProxyServerValue(regQueryString(h, "ProxyServer"))
	if server == "" {
		return nil
	}
	info := &sysProxyInfo{Server: server, Socks: socks}
	if ov := regQueryString(h, "ProxyOverride"); ov != "" {
		for _, p := range strings.Split(ov, ";") {
			if p = strings.TrimSpace(p); p != "" {
				info.Bypass = append(info.Bypass, p)
			}
		}
	}
	return info
}

// parseProxyServerValue 解析 ProxyServer 的两种格式:
// "host:port"(全协议)与 "http=h:p;https=h:p;socks=h:p"(分协议)。
// 分协议时 https 优先、其次 http;仅 socks 条目时按 SOCKS5 返回。
func parseProxyServerValue(s string) (server string, socks bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, "=") {
		return s, false
	}
	var httpS, httpsS, socksS string
	for _, kv := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "https":
			httpsS = v
		case "http":
			httpS = v
		case "socks":
			socksS = v
		}
	}
	if socksS != "" && httpS == "" && httpsS == "" {
		return socksS, true
	}
	if httpsS != "" {
		return httpsS, false
	}
	return httpS, false
}

func regOpenKey(sub string) (syscall.Handle, error) {
	subp, err := syscall.UTF16PtrFromString(sub)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	r1, _, e := procRegOpenKeyExW.Call(
		uintptr(regHKCU), uintptr(unsafe.Pointer(subp)), 0, uintptr(regRead), uintptr(unsafe.Pointer(&h)))
	if r1 != 0 {
		return 0, e
	}
	return h, nil
}

func regQueryString(h syscall.Handle, name string) string {
	typ, buf, err := regQueryValue(h, name)
	if err != nil || typ != regTypeSZ || len(buf) < 2 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), len(buf)/2))
}

func regQueryDword(h syscall.Handle, name string) (uint32, bool) {
	typ, buf, err := regQueryValue(h, name)
	if err != nil || typ != regTypeDW || len(buf) < 4 {
		return 0, false
	}
	return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24, true
}

func regQueryValue(h syscall.Handle, name string) (typ uint32, buf []byte, err error) {
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, nil, err
	}
	var cb uint32
	r1, _, e := procRegQueryValueExW.Call(
		uintptr(h), uintptr(unsafe.Pointer(namep)), 0, uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&cb)))
	if r1 != 0 {
		return 0, nil, e
	}
	if cb == 0 {
		return typ, nil, nil
	}
	buf = make([]byte, cb)
	r1, _, e = procRegQueryValueExW.Call(
		uintptr(h), uintptr(unsafe.Pointer(namep)), 0, uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&cb)))
	if r1 != 0 {
		return 0, nil, e
	}
	return typ, buf, nil
}
