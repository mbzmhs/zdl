//go:build windows

// Windows 专属代理语义测试:parseProxyServerValue/sysByPassMatch 只在
// sysproxy_windows.go 有真实现(_other.go 占位恒 false),非 Windows 平台编译不到。
package proxy

import "testing"

// TestParseProxyServerValue 覆盖 ProxyServer 的两种注册表格式。
func TestParseProxyServerValue(t *testing.T) {
	// 全协议单值
	s, socks := parseProxyServerValue("127.0.0.1:10809")
	if s != "127.0.0.1:10809" || socks {
		t.Fatalf("单值格式解析错误: %q socks=%v", s, socks)
	}
	// 分协议:https 优先
	s, socks = parseProxyServerValue("http=10.0.0.1:8080;https=10.0.0.2:8443;ftp=10.0.0.3:21")
	if s != "10.0.0.2:8443" || socks {
		t.Fatalf("分协议格式应取 https 条目: %q socks=%v", s, socks)
	}
	// 分协议:无 https 取 http
	s, _ = parseProxyServerValue("http=10.0.0.1:8080")
	if s != "10.0.0.1:8080" {
		t.Fatalf("无 https 应取 http 条目: %q", s)
	}
	// 仅 socks 条目:按 SOCKS5 返回
	s, socks = parseProxyServerValue("socks=10.0.0.4:1080")
	if s != "10.0.0.4:1080" || !socks {
		t.Fatalf("仅 socks 条目应报告 socks: %q socks=%v", s, socks)
	}
}
