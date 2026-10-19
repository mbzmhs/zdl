//go:build !windows && !darwin

package proxy

// Linux 等平台没有统一的系统代理来源(桌面环境各自为政),"跟随系统"
// 仅由环境变量承载;readSysProxy 恒 nil。

type sysProxyInfo struct {
	Server string
	Socks  bool
	Bypass []string
}

// sysProxySourceName FollowSystemStatus 报告的来源标识(followsystem.go 消费)。
const sysProxySourceName = ""

func readSysProxy() *sysProxyInfo { return nil }
