// SOCKS5 UDP 中继(RFC 1928 UDP ASSOCIATE):为 DHT、udp tracker 等
// UDP 流量提供经代理出口的 net.PacketConn。控制连接在 Close 前保持。
package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// SOCKS5UDPRelay 通过 SOCKS5 代理建立 UDP ASSOCIATE,返回中继 PacketConn。
// ReadFrom/WriteTo 的地址均为 *net.UDPAddr;数据报格式由本包封解。
func SOCKS5UDPRelay(proxyURL *url.URL) (net.PacketConn, error) {
	host := proxyURL.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "1080")
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	ctrl, err := d.Dial("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("socks5 连接代理失败: %w", err)
	}
	bnd, err := socks5Request(ctrl, proxyURL.User, cmdUDPAssociate, "0.0.0.0:0")
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	// BND.ADDR 为 0.0.0.0 时按规范语义代指客户端所连的代理地址
	if bnd.IP.IsUnspecified() || bnd.IP.IsLoopback() && !isLoopbackHost(host) {
		if ip := net.ParseIP(hostOnly(host)); ip != nil {
			bnd.IP = ip
		} else if ips, err := net.LookupIP(hostOnly(host)); err == nil && len(ips) > 0 {
			bnd.IP = ips[0]
		}
	}
	udp, err := net.ListenUDP("udp4", nil)
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	return &socks5UDPConn{udp: udp, ctrl: ctrl, relay: bnd}, nil
}

const (
	cmdConnect      = 0x01
	cmdUDPAssociate = 0x03
)

// socks5Request 复用方法协商,按 cmd 发起请求并解析 BND 地址。
func socks5Request(conn net.Conn, u *url.Userinfo, cmd byte, target string) (*net.UDPAddr, error) {
	if err := socks5Negotiate(conn, u); err != nil {
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("socks5 非法目标地址 %q: %w", target, err)
	}
	port, err := parsePort(portStr)
	if err != nil {
		return nil, err
	}
	req := []byte{0x05, cmd, 0x00}
	req = appendSOCKS5Addr(req, host, port)
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	return readSOCKS5Reply(conn)
}

// socks5Negotiate 方法协商与账号密码认证(供 CONNECT 与 UDP ASSOCIATE 复用)。
func socks5Negotiate(conn net.Conn, u *url.Userinfo) error {
	var user, pass string
	if u != nil {
		user = u.Username()
		pass, _ = u.Password()
	}
	var methods []byte
	if user != "" || pass != "" {
		methods = []byte{0x00, 0x02}
	} else {
		methods = []byte{0x00}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	rsp := make([]byte, 2)
	if _, err := io.ReadFull(conn, rsp); err != nil {
		return err
	}
	if rsp[0] != 0x05 {
		return fmt.Errorf("socks5 非法协议版本 %#x", rsp[0])
	}
	switch rsp[1] {
	case 0x00:
	case 0x02:
		if err := socks5Auth(conn, user, pass); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5 代理不接受认证方式(需要账号密码?)")
	}
	return nil
}

func parsePort(portStr string) (int, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("socks5 非法端口 %q", portStr)
	}
	return port, nil
}

func appendSOCKS5Addr(req []byte, host string, port int) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			host = "0.0.0.0" // 过长域名无法编码,退化为通配
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(port))
	return append(req, pb[:]...)
}

// readSOCKS5Reply 解析 VER REP RSV ATYP BND.ADDR BND.PORT。
func readSOCKS5Reply(conn net.Conn) (*net.UDPAddr, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if head[0] != 0x05 {
		return nil, fmt.Errorf("socks5 非法应答版本")
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("socks5 请求被拒,代码 %d", head[1])
	}
	var addr []byte
	switch head[3] {
	case 0x01:
		addr = make([]byte, 4)
	case 0x03:
		if _, err := io.ReadFull(conn, head[:1]); err != nil {
			return nil, err
		}
		addr = make([]byte, head[0])
	case 0x04:
		addr = make([]byte, 16)
	default:
		return nil, fmt.Errorf("socks5 非法地址类型 %d", head[3])
	}
	if _, err := io.ReadFull(conn, addr); err != nil {
		return nil, err
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: addr, Port: int(binary.BigEndian.Uint16(port))}, nil
}

// socks5UDPConn 是经 SOCKS5 中继的 UDP PacketConn。
// 每个数据报带 RSV(2) FRG(1) ATYP(1) DST.ADDR DST.PORT 头。
type socks5UDPConn struct {
	udp   *net.UDPConn
	ctrl  net.Conn // 关闭控制连接即解除 ASSOCIATE
	relay *net.UDPAddr

	woMu sync.Mutex
}

func (c *socks5UDPConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, _, err := c.udp.ReadFrom(p)
		if err != nil {
			return 0, nil, err
		}
		// 解析中继头:RSV(2) FRG(1) ATYP(1) ADDR PORT(2)
		if n < 10 || len(p) < n {
			continue
		}
		atyp := p[3]
		var hdr int
		var ip net.IP
		switch atyp {
		case 0x01:
			if n < 10 {
				continue
			}
			hdr = 10
			ip = net.IP(p[4:8])
		case 0x04:
			if n < 22 {
				continue
			}
			hdr = 22
			ip = net.IP(p[4:20])
		case 0x03: // 域名回包极罕见,解析长度后丢弃
			if n < 5 {
				continue
			}
			hdr = 5 + int(p[4]) + 2
			if n < hdr {
				continue
			}
			continue
		default:
			continue
		}
		port := binary.BigEndian.Uint16(p[hdr-2 : hdr])
		cp := copy(p, p[hdr:n])
		return cp, &net.UDPAddr{IP: ip, Port: int(port)}, nil
	}
}

func (c *socks5UDPConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("socks5 中继仅支持 UDP 地址")
	}
	buf := make([]byte, 0, 10+len(p))
	buf = append(buf, 0, 0, 0) // RSV FRG
	buf = appendSOCKS5Addr(buf, ua.IP.String(), ua.Port)
	buf = append(buf, p...)
	c.woMu.Lock()
	defer c.woMu.Unlock()
	if _, err := c.udp.WriteTo(buf, c.relay); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *socks5UDPConn) Close() error {
	c.ctrl.Close()
	return c.udp.Close()
}

func (c *socks5UDPConn) LocalAddr() net.Addr                { return c.udp.LocalAddr() }
func (c *socks5UDPConn) SetDeadline(t time.Time) error      { return c.udp.SetDeadline(t) }
func (c *socks5UDPConn) SetReadDeadline(t time.Time) error  { return c.udp.SetReadDeadline(t) }
func (c *socks5UDPConn) SetWriteDeadline(t time.Time) error { return c.udp.SetWriteDeadline(t) }

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(hostOnly(host))
	return ip != nil && ip.IsLoopback()
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
