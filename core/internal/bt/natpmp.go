package bt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// NAT-PMP 客户端(RFC 6886):向网关 UDP 5351 发送端口映射请求。
// 报文极简:请求 12 字节,响应 16 字节,全部大端。

const (
	natpmpPort       = 5351
	natpmpOpMapTCP   = 1
	natpmpOpMapUDP   = 2
	natpmpRetryBase  = 250 * time.Millisecond
	natpmpRetryCount = 3 // 指数退避 250ms/500ms/1s,共约 1.75s 内拿结果
)

var errNATPMP = errors.New("nat-pmp")

const (
	natpmpOpPublicIP = 0 // 公网地址查询(探测网关存活用)
	natpmpLifetime   = 7200
)

// natpmpProbe 用"公网地址请求"(op 0)快速探测网关是否支持 NAT-PMP,
// 单次 250ms 超时、最多两发,避免对一堆候选网关逐个做完整映射重试。
// 成功返回网关公网 IP。
func natpmpProbe(gateway net.IP) (net.IP, error) {
	req := []byte{0, natpmpOpPublicIP}
	resp, err := natpmpRequest(gateway, req)
	if err != nil {
		return nil, err
	}
	if len(resp) != 12 {
		return nil, fmt.Errorf("公网地址响应长度 %d,期望 12", len(resp))
	}
	if code := binary.BigEndian.Uint16(resp[2:]); code != 0 {
		return nil, fmt.Errorf("公网地址查询: %s", natpmpResultMsg(code))
	}
	return net.IPv4(resp[8], resp[9], resp[10], resp[11]), nil
}

// NATPMPProbe 探测候选网关的 NAT-PMP 存活,返回首个存活网关与其公网 IP。
// 只探测不映射——端口映射按实际监听端口由调用方逐个下发。
func NATPMPProbe() (net.IP, string, error) {
	cands := gatewayCandidates()
	if len(cands) == 0 {
		return nil, "", errors.New("无候选网关地址")
	}
	var lastErr error
	for _, gw := range cands {
		extIP, err := natpmpProbe(gw)
		if err != nil {
			lastErr = err
			continue
		}
		return gw, extIP.String(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("所有候选网关均无响应")
	}
	return nil, "", fmt.Errorf("NAT-PMP: %w", lastErr)
}

// NATPMPMap 在网关上映射单端口 TCP+UDP,生命周期 7200s,需周期重申。
func NATPMPMap(gw net.IP, port int) error {
	return natpmpMapPort(gw, uint16(port), 0, natpmpLifetime)
}

// NATPMPUnmap 撤销网关上的单端口映射(lifetime 0 = 删除)。
func NATPMPUnmap(gw net.IP, port int) error {
	return natpmpMapPort(gw, uint16(port), 0, 0)
}

func natpmpResultMsg(code uint16) string {
	switch code {
	case 0:
		return "成功"
	case 1:
		return "不支持的协议版本"
	case 2:
		return "网关拒绝(未授权)"
	case 3:
		return "网络故障"
	case 4:
		return "网关资源耗尽"
	case 5:
		return "不支持的操作"
	default:
		return fmt.Sprintf("错误码 %d", code)
	}
}

// natpmpMapReq 构造端口映射请求;lifetime=0 表示删除映射。
func natpmpMapReq(op uint8, internalPort, externalPort uint16, lifetime uint32) []byte {
	buf := make([]byte, 12)
	buf[0] = 0 // version
	buf[1] = op
	binary.BigEndian.PutUint16(buf[4:], internalPort)
	binary.BigEndian.PutUint16(buf[6:], externalPort)
	binary.BigEndian.PutUint32(buf[8:], lifetime)
	return buf
}

// natpmpMapResp 解析映射响应,返回(外部端口, 剩余生存秒数, 结果码)。
func natpmpMapResp(b []byte) (uint16, uint32, uint16, error) {
	if len(b) != 16 {
		return 0, 0, 0, fmt.Errorf("响应长度 %d,期望 16", len(b))
	}
	if b[0] != 0 {
		return 0, 0, 0, fmt.Errorf("响应版本 %d", b[0])
	}
	if b[1] == 0 || b[1]&0x80 == 0 {
		return 0, 0, 0, fmt.Errorf("响应 opcode 0x%02x 非映射应答", b[1])
	}
	code := binary.BigEndian.Uint16(b[2:])
	publicPort := binary.BigEndian.Uint16(b[8:])
	lifetime := binary.BigEndian.Uint32(b[12:])
	return publicPort, lifetime, code, nil
}

// natpmpRequest 向 gateway 发送一次映射请求并等待应答。
func natpmpRequest(gateway net.IP, req []byte) ([]byte, error) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	// 绑定本地出口地址不可行(多网卡歧义),直接发往网关即可
	gwAddr := &net.UDPAddr{IP: gateway, Port: natpmpPort}
	deadline := time.Now().Add(natpmpRetryBase * (1 << natpmpRetryCount))
	pc.SetDeadline(deadline)
	if _, err := pc.WriteToUDP(req, gwAddr); err != nil {
		return nil, err
	}
	buf := make([]byte, 64)
	// RFC 6886:250ms 起指数退避重发,直到收到应答或超时
	wait := natpmpRetryBase
	for attempt := 0; attempt < natpmpRetryCount; attempt++ {
		pc.SetReadDeadline(time.Now().Add(wait))
		n, _, err := pc.ReadFromUDP(buf)
		if err == nil {
			out := make([]byte, n)
			copy(out, buf[:n])
			return out, nil
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Now().Before(deadline) {
			if _, err := pc.WriteToUDP(req, gwAddr); err != nil {
				return nil, err
			}
			wait *= 2
			continue
		}
		return nil, err
	}
	return nil, errors.New("网关无响应")
}

// natpmpMapPort 对单个网关执行 NAT-PMP 映射(TCP+UDP 各一条)。
// 返回映射到的外部端口;失败返回错误。
func natpmpMapPort(gateway net.IP, internalPort, requestedExternal uint16, lifetime uint32) error {
	for _, op := range []uint8{natpmpOpMapTCP, natpmpOpMapUDP} {
		resp, err := natpmpRequest(gateway, natpmpMapReq(op, internalPort, requestedExternal, lifetime))
		if err != nil {
			return fmt.Errorf("nat-pmp: %w", err)
		}
		_, _, code, err := natpmpMapResp(resp)
		if err != nil {
			return fmt.Errorf("nat-pmp: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("nat-pmp: %s", natpmpResultMsg(code))
		}
	}
	return nil
}

// gatewayCandidates 猜测默认网关:对每个有 IPv4 的接口,取其子网内
// 常见网关地址(.1/.2/.254)。NAT-PMP 没有不解析路由表的轻量取法,
// 逐个尝试 250ms 级超时的请求,代价可接受。
func gatewayCandidates() []net.IP {
	var out []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return
		}
		k := ip.String()
		if !seen[k] {
			seen[k] = true
			out = append(out, ip)
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
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
			if ip4 == nil {
				continue
			}
			mask := ipnet.Mask
			if len(mask) != 4 {
				continue
			}
			base := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				base[i] = ip4[i] & mask[i]
			}
			for _, last := range []byte{1, 2, 254} {
				cand := make(net.IP, 4)
				copy(cand, base)
				cand[3] = last
				add(cand)
			}
		}
	}
	return out
}
