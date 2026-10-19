//go:build windows

package bt

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// listenDHTUDP 按地址族监听 UDP。udp6 显式设 IPV6_V6ONLY=1:Go 通配
// 套接字默认双栈,与已绑定的 v4 同号端口(DHT/uTP 共用)冲突而 bind 失败。
func listenDHTUDP(network string, port int) (*net.UDPConn, error) {
	if network != "udp6" {
		uc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, err
		}
		disableUDPConnReset(uc) // DHT 向死节点发包是常态,防 ICMP 毒化 socket
		return uc, nil
	}
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
			// 同样关闭 ICMP 触发 RESET(v6 的 ICMPv6 不可达同样毒化 socket)
			v := uint32(0)
			var ret uint32
			_ = syscall.WSAIoctl(syscall.Handle(fd), sioUDPConnReset,
				(*byte)(unsafe.Pointer(&v)), uint32(unsafe.Sizeof(v)),
				nil, 0, &ret, nil, 0)
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
