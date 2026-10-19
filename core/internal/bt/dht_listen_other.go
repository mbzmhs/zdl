//go:build !windows

package bt

import (
	"net"
)

// listenDHTUDP 非 Windows 平台的回退实现(未做 V6ONLY 处理)。
func listenDHTUDP(network string, port int) (*net.UDPConn, error) {
	return net.ListenUDP(network, &net.UDPAddr{Port: port})
}
