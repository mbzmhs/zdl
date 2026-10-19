// Package proxy 构建 HTTP Transport,支持直连、环境变量、HTTP(S) 与 SOCKS5 代理。
// SOCKS5 客户端为手写实现(RFC 1928),保持零第三方依赖。
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Options 决定一次下载所用的网络出口。
type Options struct {
	Manual        string // ""=跟随 Mode;或完整代理 URL;特殊值 "none"=强制直连
	Mode          string // 全局模式: none | manual | env
	URL           string // 全局手动代理 URL
	SkipTLSVerify bool
}

// Transport 构建专用 *http.Transport(可安全并发使用)。
func Transport(o Options) *http.Transport {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   24, // 多段并发需要同主机大量连接
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: o.SkipTLSVerify},
	}

	manual := o.Manual
	if manual == "" {
		switch o.Mode {
		case "manual":
			manual = o.URL
		case "env":
			// 跟随系统:环境变量优先,其次 Windows 系统代理(注册表)。
			// 每个请求动态解析(NO_PROXY / ProxyOverride 例外生效),
			// 代理切换后最多一个缓存 TTL 生效
			tr.Proxy = func(req *http.Request) (*url.URL, error) {
				if noProxyMatch(req.URL.Host) || isLANHost(req.URL.Host) {
					return nil, nil
				}
				if u := FollowSystemProxy(); u != nil {
					if sysByPassMatch(followBypass(), req.URL.Hostname()) {
						return nil, nil
					}
					return u, nil
				}
				return nil, nil
			}
			return tr
		default: // none
			return tr
		}
	}
	if manual == "none" {
		return tr
	}

	u, err := url.Parse(manual)
	if err != nil || u.Host == "" {
		// 代理配置非法时退回直连,避免整体不可用
		return tr
	}
	switch u.Scheme {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		// 域名统一交给代理解析(socks5h 语义)
		tr.DialContext = SOCKS5DialContext(u)
	default:
	}
	return tr
}

// DialFunc 通用网络拨号函数(BT peer 连接等非 HTTP 场景使用)。
type DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// DialContext 返回面向原始 TCP 连接的拨号函数:SOCKS5 走 CONNECT,
// http/https 代理走 HTTP CONNECT 隧道,none/非法配置/内网地址直连
// (内网例外:BT 局域网发现等本机网段流量不应绕道代理)。
// env 模式动态解析"跟随系统":环境变量(ALL_PROXY > HTTPS_PROXY >
// HTTP_PROXY)优先,其次 Windows 系统代理(注册表);NO_PROXY 与
// ProxyOverride 例外生效——与 http.Transport 的语义对齐。
func DialContext(o Options) DialFunc {
	direct := func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	manual := o.Manual
	envMode := false
	if manual == "" {
		switch o.Mode {
		case "manual":
			manual = o.URL
		case "env":
			envMode = true // 每次拨号动态解析(环境变量 → Windows 系统代理)
		default: // none
			return direct
		}
	}
	if manual == "none" {
		return direct
	}
	if envMode {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if noProxyMatch(addr) || isLANAddr(addr) {
				return direct(ctx, network, addr)
			}
			u := FollowSystemProxy()
			if u == nil {
				return direct(ctx, network, addr)
			}
			if sysByPassMatch(followBypass(), hostOnly(addr)) {
				return direct(ctx, network, addr)
			}
			switch u.Scheme {
			case "socks5", "socks5h":
				return SOCKS5DialContext(u)(ctx, network, addr)
			case "http", "https":
				return httpConnectDialer(u, o.SkipTLSVerify, direct)(ctx, network, addr)
			default:
				return direct(ctx, network, addr)
			}
		}
	}
	u, err := url.Parse(manual)
	if err != nil || u.Host == "" {
		return direct
	}
	var out DialFunc
	switch u.Scheme {
	case "socks5", "socks5h":
		socks := SOCKS5DialContext(u)
		out = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if isLANAddr(addr) {
				return direct(ctx, network, addr)
			}
			return socks(ctx, network, addr)
		}
	case "http", "https":
		out = httpConnectDialer(u, o.SkipTLSVerify, direct)
	default:
		out = direct
	}
	return out
}

// envProxyForDial 从环境变量解析原始 TCP 拨号用的代理地址。
func envProxyForDial() string {
	v, _ := envProxyWithSource()
	return v
}

// envProxyWithSource 返回环境变量代理值与命中的变量名(展示用)。
func envProxyWithSource() (val, name string) {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, strings.ToUpper(strings.SplitN(k, "_", 2)[0] + "_PROXY")
		}
	}
	return "", ""
}

// EnvProxyConfigured 报告"跟随系统"模式是否检测到代理:
// 环境变量或 Windows 系统代理(注册表)任一命中即为 true。
func EnvProxyConfigured() bool {
	envStr, sys := followGet()
	return envStr != "" || sys != nil
}

// noProxyMatch 判断 addr 是否命中 NO_PROXY 例外(逗号分隔的 host 或
// .domain 后缀,"*" 全部直连)。
func noProxyMatch(addr string) bool {
	nps := strings.TrimSpace(os.Getenv("NO_PROXY"))
	if nps == "" {
		nps = os.Getenv("no_proxy")
	}
	if nps == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range strings.Split(nps, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "*" {
			return true
		}
		p = strings.TrimPrefix(p, ".")
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// isLANAddr 判断 host:port 是否本机/局域网地址(仅 IP 字面量可判定,域名一律视为远端)。
func isLANAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// isLANHost 判断可能不带端口的主机(如 http.Request.URL.Host)是否局域网,
// 缺端口时按任意端口补齐后再判定。
func isLANHost(host string) bool {
	if isLANAddr(host) {
		return true
	}
	return isLANAddr(net.JoinHostPort(host, "80"))
}

// httpConnectDialer 经 HTTP(S) 代理的 CONNECT 隧道建立原始 TCP 连接。
func httpConnectDialer(proxyURL *url.URL, skipTLSVerify bool, direct DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" {
			return nil, fmt.Errorf("http 代理仅支持 tcp 拨号")
		}
		if isLANAddr(addr) {
			return direct(ctx, network, addr)
		}
		host := proxyURL.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			port := "80"
			if proxyURL.Scheme == "https" {
				port = "443"
			}
			host = net.JoinHostPort(host, port)
		}
		d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, fmt.Errorf("连接代理失败: %w", err)
		}
		if proxyURL.Scheme == "https" {
			conn = tls.Client(conn, &tls.Config{
				InsecureSkipVerify: skipTLSVerify,
				ServerName:         proxyURL.Hostname(),
			})
		}
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Host: addr},
			Host:   addr,
			Header: http.Header{},
		}
		if u := proxyURL.User; u != nil {
			pass, _ := u.Password()
			req.SetBasicAuth(u.Username(), pass)
		}
		// CONNECT 交换必须在 ctx 截止内完成:代理对死目标可能长时间不应答,
		// 无 deadline 时拨号协程永久卡死(dialing 槽位泄漏、补拨停摆)。
		if dl, ok := ctx.Deadline(); ok {
			conn.SetDeadline(dl)
		} else {
			conn.SetDeadline(time.Now().Add(30 * time.Second))
		}
		if err := req.Write(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 应答失败: %w", err)
		}
		conn.SetDeadline(time.Time{}) // 隧道建立:清除拨号期 deadline
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 被拒: HTTP %d", resp.StatusCode)
		}
		return conn, nil
	}
}

// SOCKS5DialContext 返回注入 http.Transport 的 SOCKS5 拨号函数。
func SOCKS5DialContext(proxyURL *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host := proxyURL.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "1080")
		}
		d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, fmt.Errorf("socks5 连接代理失败: %w", err)
		}
		if err := socks5Handshake(ctx, conn, proxyURL.User, addr); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// socks5Handshake 完成方法协商与 CONNECT 请求,成功后 conn 即到目标的原始隧道。
func socks5Handshake(ctx context.Context, conn net.Conn, u *url.Userinfo, target string) error {
	// 握手阶段用 ctx 截止时间兜底
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	if err := socks5Negotiate(conn, u); err != nil {
		return err
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("socks5 非法目标地址 %q: %w", target, err)
	}
	port, err := parsePort(portStr)
	if err != nil {
		return err
	}
	req := append([]byte{0x05, 0x01, 0x00}, appendSOCKS5Addr(nil, host, port)...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	if _, err := readSOCKS5Reply(conn); err != nil {
		return err
	}
	return nil
}

func socks5Auth(conn net.Conn, user, pass string) error {
	ub := []byte(user)
	pb := []byte(pass)
	if len(ub) > 255 || len(pb) > 255 {
		return fmt.Errorf("socks5 认证信息过长")
	}
	req := append([]byte{0x01, byte(len(ub))}, ub...)
	req = append(req, byte(len(pb)))
	req = append(req, pb...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	rsp := make([]byte, 2)
	if _, err := io.ReadFull(conn, rsp); err != nil {
		return err
	}
	if rsp[0] != 0x01 || rsp[1] != 0x00 {
		return fmt.Errorf("socks5 用户名密码认证失败")
	}
	return nil
}
