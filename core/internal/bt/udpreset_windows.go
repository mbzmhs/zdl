//go:build windows

package bt

import (
	"net"
	"syscall"
	"unsafe"
)

// SIO_UDP_CONNRESET:Windows 默认在 UDP socket 收到 ICMP 端口不可达后,
// 令后续 sendto/recvfrom 以 WSAECONNRESET 失败。DHT 场景下向死节点发包
// 是常态,该行为会让同 socket 的 uTP 出包间歇性静默失败。置 0 关闭之
// (libutp/libtorrent 在 Windows 上的标准做法)。
const sioUDPConnReset = 0x9800000C

// disableUDPConnReset 关闭 socket 的 ICMP 触发 RESET 行为;失败不致命。
func disableUDPConnReset(conn *net.UDPConn) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		v := uint32(0)
		var ret uint32
		_ = syscall.WSAIoctl(syscall.Handle(fd), sioUDPConnReset,
			(*byte)(unsafe.Pointer(&v)), uint32(unsafe.Sizeof(v)),
			nil, 0, &ret, nil, 0)
	})
}
