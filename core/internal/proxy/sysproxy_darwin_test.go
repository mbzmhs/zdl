//go:build darwin

package proxy

import "testing"

// scutilSample 取自本机真实输出(代理客户端开启"自动配置系统代理"时):
// HTTP/HTTPS/SOCKS 同源,ExceptionsList 三条。
const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : localhost
    1 : 127.0.0.0/8
    2 : ::1
  }
  HTTPEnable : 1
  HTTPPort : 10808
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 10808
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 1
  SOCKSPort : 10808
  SOCKSProxy : 127.0.0.1
}`

// TestParseScutilProxy 覆盖 scutil --proxy 输出的主要形态。
func TestParseScutilProxy(t *testing.T) {
	// 真实形态:HTTPS 优先于 HTTP(同为 127.0.0.1:10808,结果一致但路径正确)
	info := parseScutilProxy(scutilSample)
	if info == nil || info.Server != "127.0.0.1:10808" || info.Socks {
		t.Fatalf("真实样例解析错误: %+v", info)
	}
	if len(info.Bypass) != 3 || info.Bypass[0] != "localhost" || info.Bypass[2] != "::1" {
		t.Fatalf("ExceptionsList 应原样入列: %v", info.Bypass)
	}

	// 仅 SOCKS 启用:按 socks5 返回
	socksOnly := `<dictionary> {
  SOCKSEnable : 1
  SOCKSPort : 1080
  SOCKSProxy : 10.0.0.4
}`
	info = parseScutilProxy(socksOnly)
	if info == nil || info.Server != "10.0.0.4:1080" || !info.Socks {
		t.Fatalf("仅 SOCKS 应按 socks5 返回: %+v", info)
	}

	// HTTP/HTTPS/SOCKS 全关:未启用系统代理
	allOff := `<dictionary> {
  HTTPEnable : 0
  HTTPPort : 0
  HTTPProxy : 127.0.0.1
  SOCKSEnable : 0
}`
	if info = parseScutilProxy(allOff); info != nil {
		t.Fatalf("全关应返回 nil: %+v", info)
	}

	// ExcludeSimpleHostnames:计入 "<local>" 且排在 ExceptionsList 前
	simple := `<dictionary> {
  ExcludeSimpleHostnames : 1
  ExceptionsList : <array> {
    0 : localhost
  }
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : 10.0.0.1
}`
	info = parseScutilProxy(simple)
	if info == nil || len(info.Bypass) != 2 || info.Bypass[0] != "<local>" {
		t.Fatalf("ExcludeSimpleHostnames 应计入 <local>: %+v", info)
	}

	// 端口缺失(HTTPEnable=1 但无 Port):视为未配置
	noPort := `<dictionary> {
  HTTPEnable : 1
  HTTPProxy : 10.0.0.1
}`
	if info = parseScutilProxy(noPort); info != nil {
		t.Fatalf("无端口应返回 nil: %+v", info)
	}

	// 空输出/垃圾输入:返回 nil
	if parseScutilProxy("") != nil || parseScutilProxy("not scutil output") != nil {
		t.Fatal("垃圾输入应返回 nil")
	}
}
