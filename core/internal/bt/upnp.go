package bt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// UPnP IGD(Internet Gateway Device)端口映射:
// SSDP 多播发现网关 → 拉取设备描述 XML 找 WANIP/WANPPP 服务的 controlURL
// → SOAP AddPortMapping/GetExternalIP/DeletePortMapping。
// 只做 NAT 端口转发这一件事,不碰媒体渲染等其他 UPnP 设备。

const (
	upnpMulticast = "239.255.255.250:1900"
	upnpLeaseSec  = 3600 // 映射租期 1 小时,由调用方周期续约
)

// httpNoProxy 直连传输:SSDP 描述/控制 URL 都是内网地址,走环境代理必挂
// (系统代理路由不到内网网关)。
var httpNoProxy = &http.Transport{Proxy: nil}

// UPnP 是一个已发现的网关控制通道。
type UPnP struct {
	ctrlURL     string // WAN*Connection 服务的 SOAP control URL
	serviceType string // 如 urn:schemas-upnp-org:service:WANIPConnection:2
	externalIP  string
	hc          *http.Client
	localIP     string // 映射目标:本机内网 IP
}

// DiscoverUPnP 在给定预算内完成 SSDP 发现与网关握手。
// 找不到网关 / 家用路由不支持时返回错误,调用方降级(仅出站模式)。
func DiscoverUPnP(ctx context.Context) (*UPnP, error) {
	locations := ssdpSearch(ctx)
	if len(locations) == 0 {
		return nil, errors.New("UPnP: 未发现网关(SSDP 无应答)")
	}
	var lastErr error
	for _, loc := range locations {
		gw, err := describeGateway(ctx, loc)
		if err != nil {
			lastErr = err
			continue
		}
		// 外部 IP 拿不到不拦路:部分网关在 PPPoE 未连接等状态对该动作返回
		// SOAP fault,但 AddPortMapping 仍可用
		if ip, err := gw.getExternalIP(ctx); err == nil {
			gw.externalIP = ip
		}
		return gw, nil
	}
	if lastErr == nil {
		lastErr = errors.New("UPnP: 网关无 WANIP/WANPPP 服务")
	}
	return nil, lastErr
}

// ssdpSearch 做一轮 SSDP 发现,收集网关描述文件 LOCATION(去重)。
// 逐接口绑定源 IP 发送——多宿主(虚拟网卡/VPN)机器上绑定 0.0.0.0 出口
// 选错接口,网关不应答(实测华为网关对未绑定的 M-SEARCH 静默丢弃)。
// 同时单播直发候选网关(HOST 头与目的一致,部分网关严格校验),多播组
// 加监听接住把应答发回组的非标设备;NOTIFY 宣告的 LOCATION 一并接受。
func ssdpSearch(ctx context.Context) []string {
	gaddr, err := net.ResolveUDPAddr("udp4", upnpMulticast)
	if err != nil {
		return nil
	}
	sts := []string{
		"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
		"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
		"urn:schemas-upnp-org:device:WANDevice:2",
		"urn:schemas-upnp-org:device:WANDevice:1",
		"urn:schemas-upnp-org:service:WANIPConnection:2",
		"urn:schemas-upnp-org:service:WANIPConnection:1",
		"urn:schemas-upnp-org:service:WANPPPConnection:1",
		"upnp:rootdevice",
	}
	// 每个有 IPv4 的 Up 接口一对 socket:绑定源 IP 的单播收发口 + 多播组监听口
	type ifaceSock struct {
		uc, mc *net.UDPConn
	}
	var socks []ifaceSock
	defer func() {
		for _, s := range socks {
			s.uc.Close()
			if s.mc != nil {
				s.mc.Close()
			}
		}
	}()
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip4, Port: 0})
			if err != nil {
				break
			}
			mc, _ := net.ListenMulticastUDP("udp4", ifc, gaddr)
			socks = append(socks, ifaceSock{uc, mc})
			break // 每接口只取第一个 IPv4
		}
	}
	if len(socks) == 0 {
		return nil
	}
	// 发送:多播(HOST=组地址)+ 单播直发候选网关(HOST=网关地址)。
	// 单播取主要 ST 压时长。
	gws := gatewayCandidates()
	for _, s := range socks {
		for _, st := range sts {
			if ctx.Err() != nil {
				return nil
			}
			s.uc.WriteToUDP(msearchMsg(upnpMulticast, st), gaddr)
			time.Sleep(30 * time.Millisecond)
		}
		for _, gw := range gws {
			ga := &net.UDPAddr{IP: gw, Port: 1900}
			for _, st := range sts[:4] {
				if ctx.Err() != nil {
					return nil
				}
				s.uc.WriteToUDP(msearchMsg(ga.String(), st), ga)
				time.Sleep(30 * time.Millisecond)
			}
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	hits := make(chan string, 128)
	for _, s := range socks {
		for _, c := range []*net.UDPConn{s.uc, s.mc} {
			if c == nil {
				continue
			}
			go func(c *net.UDPConn) {
				buf := make([]byte, 4096)
				for {
					c.SetReadDeadline(deadline)
					n, _, err := c.ReadFromUDP(buf)
					if err != nil {
						return
					}
					if loc := locationOf(string(buf[:n])); loc != "" {
						select {
						case hits <- loc:
						default:
						}
					}
				}
			}(c)
		}
	}
	seen := map[string]bool{}
	var out []string
	for {
		select {
		case loc := <-hits:
			if !seen[loc] {
				seen[loc] = true
				out = append(out, loc)
			}
		case <-time.After(time.Until(deadline) + 100*time.Millisecond):
			return out
		case <-ctx.Done():
			return out
		}
	}
}

// msearchMsg 构造单个 M-SEARCH 请求。host 必须与实际发送目的一致
// (多播=组地址,单播=网关地址),部分网关严格校验 HOST 头。
func msearchMsg(host, st string) []byte {
	return []byte(fmt.Sprintf("M-SEARCH * HTTP/1.1\r\nHOST: %s\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: %s\r\n\r\n", host, st))
}

// locationOf 从 SSDP 报文(M-SEARCH 应答或 NOTIFY 宣告)里取 LOCATION 头。
func locationOf(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimRight(line, "\r")
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "LOCATION") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var (
	reServiceType = regexp.MustCompile(`<serviceType>\s*urn:schemas-upnp-org:service:(WANIPConnection|WANPPPConnection):(\d+)\s*</serviceType>`)
	reControlURL  = regexp.MustCompile(`<controlURL>\s*([^<]+?)\s*</controlURL>`)
)

// describeGateway 拉取设备描述 XML,定位 WAN*Connection 服务的 controlURL。
// 多设备嵌套时取任意一个 WAN 连接服务即可(家用网关普遍只有一个)。
func describeGateway(ctx context.Context, location string) (*UPnP, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second, Transport: httpNoProxy}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, err
	}
	xml := string(body)
	// 服务段成对出现:取 serviceType 匹配段落里的 controlURL
	idx := 0
	for {
		stLoc := reServiceType.FindStringSubmatchIndex(xml[idx:])
		if stLoc == nil {
			return nil, errors.New("UPnP: 描述文件中没有 WANIP/WANPPP 服务")
		}
		segStart := idx + stLoc[0]
		segEnd := min(idx+stLoc[1]+4096, len(xml))
		seg := xml[segStart:segEnd]
		cu := reControlURL.FindStringSubmatch(seg)
		if cu != nil {
			// controlURL 多为根绝对路径(/ctrlu/...),必须按 URL 规范对
			// 描述文件地址做引用解析——直接字符串拼接会把描述文件的
			// 路径段(如 /xxx/upnpdev.xml)带进 control 路径,网关按未
			// 知路由返回泛化 402
			resolved := cu[1]
			if !strings.Contains(resolved, "://") {
				base, err := url.Parse(location)
				if err == nil {
					ref, rerr := url.Parse(resolved)
					if rerr == nil {
						resolved = base.ResolveReference(ref).String()
					} else if !strings.HasPrefix(resolved, "/") {
						resolved = "/" + resolved
					}
				} else if !strings.HasPrefix(resolved, "/") {
					resolved = "/" + resolved
				}
			}
			st := xml[idx+stLoc[2] : idx+stLoc[3]] // 完整 serviceType 值
			return &UPnP{
				ctrlURL:     resolved,
				serviceType: st,
				hc:          &http.Client{Timeout: 10 * time.Second, Transport: httpNoProxy},
				localIP:     localIPv4(),
			}, nil
		}
		idx = segStart + stLoc[1]
	}
}

// localIPv4 取默认路由出口的内网 IP(UDP 连外部地址不发包即可拿到本机地址)。
func localIPv4() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// soap 调用一次 WAN*Connection 动作。
func (u *UPnP) soap(ctx context.Context, action string, args string) (string, error) {
	if u == nil || u.ctrlURL == "" {
		return "", errors.New("UPnP: 未初始化")
	}
	body := fmt.Sprintf(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%s xmlns:u="%s">%s</u:%s></s:Body></s:Envelope>`,
		action, u.serviceType, args, action)
	req, err := http.NewRequestWithContext(ctx, "POST", u.ctrlURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", fmt.Sprintf(`"%s#%s"`, u.serviceType, action))
	resp, err := u.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		if m := reUPnPError.FindStringSubmatch(string(b)); m != nil {
			return "", fmt.Errorf("UPnP: %s 返回 UPnPError=%s(%s)", action, m[1], m[2])
		}
		return "", fmt.Errorf("UPnP: %s 返回 HTTP %d", action, resp.StatusCode)
	}
	return string(b), nil
}

var reXMLSpecial = regexp.MustCompile(`[<>&"']`)

var reExternalIP = regexp.MustCompile(`<NewExternalIPAddress>\s*([^<\s]+)\s*</NewExternalIPAddress>`)

// reUPnPError 解析 SOAP fault 里的 UPnPError 码与描述,透出给日志定位
// (如华为固件对第三方控制一律回 402 Invalid Args)。
var reUPnPError = regexp.MustCompile(`(?s)<errorCode>\s*(\d+)\s*</errorCode>.*?<errorDescription>\s*([^<]*?)\s*</errorDescription>`)

func (u *UPnP) getExternalIP(ctx context.Context) (string, error) {
	out, err := u.soap(ctx, "GetExternalIPAddress", "")
	if err != nil {
		return "", err
	}
	m := reExternalIP.FindStringSubmatch(out)
	if m == nil {
		return "", errors.New("UPnP: 外部 IP 应答异常")
	}
	return m[1], nil
}

// ExternalIP 返回网关发现时记录的外部 IP。
func (u *UPnP) ExternalIP() string {
	if u == nil {
		return ""
	}
	return u.externalIP
}

// AddPortMapping 建立端口映射。desc 为路由表里的描述文字。
func (u *UPnP) AddPortMapping(ctx context.Context, externalPort int, proto string, internalPort int, desc string) error {
	if u.localIP == "" {
		return errors.New("UPnP: 无法确定本机内网 IP")
	}
	args := fmt.Sprintf(
		`<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort><NewProtocol>%s</NewProtocol><NewInternalPort>%d</NewInternalPort><NewInternalClient>%s</NewInternalClient><NewEnabled>1</NewEnabled><NewPortMappingDescription>%s</NewPortMappingDescription><NewLeaseDuration>%d</NewLeaseDuration>`,
		externalPort, strings.ToUpper(proto), internalPort, u.localIP, xmlEscape(desc), upnpLeaseSec)
	_, err := u.soap(ctx, "AddPortMapping", args)
	return err
}

// DeletePortMapping 删除端口映射。
func (u *UPnP) DeletePortMapping(ctx context.Context, externalPort int, proto string) error {
	args := fmt.Sprintf(`<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort><NewProtocol>%s</NewProtocol>`,
		externalPort, strings.ToUpper(proto))
	_, err := u.soap(ctx, "DeletePortMapping", args)
	return err
}

// ReapplyMapping 续约:重新 Add 同一映射(幂等)。
func (u *UPnP) ReapplyMapping(ctx context.Context, externalPort int, proto string, internalPort int, desc string) error {
	return u.AddPortMapping(ctx, externalPort, proto, internalPort, desc)
}

func xmlEscape(s string) string {
	return reXMLSpecial.ReplaceAllStringFunc(s, func(c string) string {
		switch c {
		case "<":
			return "&lt;"
		case ">":
			return "&gt;"
		case "&":
			return "&amp;"
		case `"`:
			return "&quot;"
		}
		return "&apos;"
	})
}
