package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// httptestTarget 启动一个返回固定内容的 HTTP 目标服务器。
func httptestTarget(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello via socks5"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startSOCKS5Server 极简 SOCKS5 服务端(仅无认证),返回其监听地址。
func startSOCKS5Server(t *testing.T, upstream func(host string, port int) net.Conn) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				head := make([]byte, 2)
				if _, err := io.ReadFull(br, head); err != nil {
					return
				}
				nMethods := int(head[1])
				methods := make([]byte, nMethods)
				if _, err := io.ReadFull(br, methods); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				if _, err := io.ReadFull(br, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case 0x01:
					ip := make([]byte, 4)
					io.ReadFull(br, ip)
					host = net.IP(ip).String()
				case 0x03:
					l := make([]byte, 1)
					io.ReadFull(br, l)
					d := make([]byte, l[0])
					io.ReadFull(br, d)
					host = string(d)
				case 0x04:
					ip := make([]byte, 16)
					io.ReadFull(br, ip)
					host = net.IP(ip).String()
				}
				pb := make([]byte, 2)
				io.ReadFull(br, pb)
				port := int(binary.BigEndian.Uint16(pb))
				target := upstream(host, port)
				if target == nil {
					c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer target.Close()
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				go io.Copy(target, br)
				io.Copy(c, target)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestSOCKS5Transport(t *testing.T) {
	// 上游:SOCKS5 把流量转给本地 httptest 目标
	target := httptestTarget(t)
	addr := startSOCKS5Server(t, func(host string, port int) net.Conn {
		c, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			return nil
		}
		return c
	})

	u, _ := url.Parse("socks5://" + addr)
	tr := &http.Transport{DialContext: SOCKS5DialContext(u)}
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://" + target.Listener.Addr().String() + "/hello")
	if err != nil {
		t.Fatalf("经 SOCKS5 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello via socks5" {
		t.Fatalf("响应不符: %q", string(body))
	}
}

func TestSOCKS5DialContextFailure(t *testing.T) {
	// 无监听端口的代理 → 应返回错误而非挂起
	u, _ := url.Parse("socks5://127.0.0.1:1")
	tr := &http.Transport{DialContext: SOCKS5DialContext(u)}
	client := &http.Client{Transport: tr}
	_, err := client.Get("http://example.invalid/")
	if err == nil {
		t.Fatal("应返回错误")
	}
}

func TestTransportOptions(t *testing.T) {
	tr := Transport(Options{Mode: "none"})
	if tr.Proxy != nil {
		t.Fatal("none 模式不应设置代理")
	}
	tr2 := Transport(Options{Manual: "http://127.0.0.1:9999"})
	if tr2.Proxy == nil {
		t.Fatal("manual HTTP 代理应设置 Proxy")
	}
}

// TestHTTPConnectDialer 用伪造的 CONNECT 代理验证隧道拨号与内网绕行。
func TestHTTPConnectDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method != http.MethodConnect {
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		// 之后当原始管道用:回显一行
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()

	dial := DialContext(Options{Manual: "http://" + ln.Addr().String()})
	ctx := context.Background()
	conn, err := dial(ctx, "tcp", "93.184.216.34:12345")
	if err != nil {
		t.Fatalf("CONNECT 拨号失败: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 16)
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "ping" {
		t.Fatalf("隧道数据不符: %q", buf[:n])
	}
}

// TestDialContextLANBypass 内网/回环地址始终直连(不经过代理)。
func TestDialContextLANBypass(t *testing.T) {
	// 无监听的代理地址:若走了代理会连接失败
	dial := DialContext(Options{Manual: "http://127.0.0.1:1"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	conn, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("回环地址应绕过代理直连: %v", err)
	}
	conn.Close()
}

// TestDialContextEnvProxy:env 模式从环境变量取代理;NO_PROXY 例外直连;
// 未配置环境变量时直连。
func TestDialContextEnvProxy(t *testing.T) {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}

	// 清空环境变量后来源不应是 env(本机可能有 Windows 系统代理,属合法)
	if st := FollowSystemStatus(); st.Source == "env" {
		t.Fatalf("清空环境变量后不应报告 env 来源: %+v", st)
	}
	// 未配置:直连(目标为内网回环,始终直连;错误不应含代理痕迹)
	followRefresh() // 绕过 15s 缓存,让上面的环境变量清空立即生效
	d := DialContext(Options{Mode: "env"})
	_, err := d(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil || strings.Contains(err.Error(), "代理") {
		t.Fatalf("无环境变量应直连,实际: %v", err)
	}

	// ALL_PROXY 优先级最高(目标非内网,确保不触发 LAN 直通)
	t.Setenv("ALL_PROXY", "http://invalid-proxy-host-enum-1:9")
	t.Setenv("HTTPS_PROXY", "http://invalid-proxy-host-enum-2:9")
	followRefresh() // 绕过 15s 缓存,让刚设置的环境变量立即生效
	if !EnvProxyConfigured() {
		t.Fatal("配置环境变量后应报告已配置")
	}
	d = DialContext(Options{Mode: "env"})
	_, err = d(context.Background(), "tcp", "93.184.216.34:1")
	if err == nil || !strings.Contains(err.Error(), "invalid-proxy-host-enum-1") {
		t.Fatalf("应经 ALL_PROXY 拨号,实际: %v", err)
	}

	// NO_PROXY 命中:绕过代理直连(错误为直连超时/拒绝,不含代理主机名)
	t.Setenv("NO_PROXY", "93.184.216.34, .example.com")
	conn, derr := d(context.Background(), "tcp", "93.184.216.34:1")
	if derr == nil {
		conn.Close()
	}
	if derr != nil && strings.Contains(derr.Error(), "invalid-proxy-host-enum-1") {
		t.Fatalf("NO_PROXY 命中应直连,实际经代理: %v", derr)
	}
}

// TestSysByPassMatch 覆盖系统代理例外列表语义(Windows ProxyOverride /
// macOS ExceptionsList 同一套匹配规则)。
func TestSysByPassMatch(t *testing.T) {
	bl := []string{"localhost", "*.local", "169.254.1.1", "<local>"}
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"myhost.local", true},   // *.local 通配
		{"169.254.1.1", true},    // 精确
		{"myserver", true},       // <local>:不带点的主机名
		{"myserver.local", true}, // 后缀也命中 *.local
		{"example.com", false},
		{"192.168.1.5", false},
		{"notlocalhost.org", false}, // 精确匹配不作后缀
	}
	for _, c := range cases {
		if got := sysByPassMatch(bl, c.host); got != c.want {
			t.Fatalf("sysByPassMatch(%q) = %v, 期望 %v", c.host, got, c.want)
		}
	}
	if !sysByPassMatch([]string{"*"}, "anything.com") {
		t.Fatal("* 应全部绕过")
	}
}

// TestNormalizeProxyURL 覆盖环境变量/系统代理值的归一化。
func TestNormalizeProxyURL(t *testing.T) {
	if u := normalizeProxyURL("127.0.0.1:10809"); u == nil || u.Scheme != "http" || u.Host != "127.0.0.1:10809" {
		t.Fatalf("无 scheme 应按 http 补全: %+v", u)
	}
	if u := normalizeProxyURL("socks://127.0.0.1:1080"); u == nil || u.Scheme != "socks5" {
		t.Fatalf("socks:// 应归一为 socks5://: %+v", u)
	}
	if u := normalizeProxyURL("socks5://u:p@127.0.0.1:1080"); u == nil || u.Scheme != "socks5" || u.User == nil {
		t.Fatalf("带认证的 socks5 应保留 user: %+v", u)
	}
	if normalizeProxyURL("") != nil || normalizeProxyURL("   ") != nil {
		t.Fatal("空值应返回 nil")
	}
}

// TestFollowSystemStatusWindows 在启用了系统代理的机器上验证注册表读取。
// 先清空环境变量排除宿主环境干扰;未启用时跳过(机器状态相关)。
func TestFollowSystemStatusWindows(t *testing.T) {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
	}
	st := FollowSystemStatus()
	if st.Source != "windows" {
		t.Skipf("本机未启用 Windows 系统代理(source=%q),跳过", st.Source)
	}
	if st.Addr == "" || st.URL == "" {
		t.Fatalf("windows 来源应返回地址: %+v", st)
	}
}

// TestTransportFollowSystemProxy 验证 env 模式 Transport 的动态代理解析
// (环境变量优先,请求级生效;直连断言用 NO_PROXY 保证与注册表状态无关)。
func TestTransportFollowSystemProxy(t *testing.T) {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	tr := Transport(Options{Mode: "env"})
	if tr.Proxy == nil {
		t.Fatal("env 模式应设置自定义 Proxy 函数")
	}
	req, _ := http.NewRequest("GET", "http://93.184.216.34/file.bin", nil)
	followRefresh() // 绕过 15s 缓存
	// 本机可能启用了 Windows 系统代理(env 清空后合法来源):
	// 直连验证改用 NO_PROXY 命中,与代理来源无关
	t.Setenv("NO_PROXY", "93.184.216.34")
	if u, err := tr.Proxy(req); err != nil || u != nil {
		t.Fatalf("NO_PROXY 命中应直连: %v %v", u, err)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("HTTP_PROXY", "http://invalid-proxy-host-enum-3:9")
	followRefresh()
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "invalid-proxy-host-enum-3:9" {
		t.Fatalf("env 命中后应返回代理: %v %v", u, err)
	}
	// 内网直连
	reqLAN, _ := http.NewRequest("GET", "http://192.168.1.10/file.bin", nil)
	if u, _ := tr.Proxy(reqLAN); u != nil {
		t.Fatalf("内网地址应直连: %v", u)
	}
}
