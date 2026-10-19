//go:build !windows

package bt

import "net"

// disableUDPConnReset 非 Windows 无此语义(套接字不会因 ICMP 记错状态)。
func disableUDPConnReset(conn *net.UDPConn) {}
