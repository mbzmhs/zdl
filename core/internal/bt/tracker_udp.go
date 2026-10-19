package bt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

const udpMagic = uint64(0x41727101980)

// UDPAnnounce 完成一次 connect + announce(BEP-15),直连。
func UDPAnnounce(ctx context.Context, rawURL string, req AnnounceReq) (*AnnounceResp, error) {
	return UDPAnnounceConn(ctx, rawURL, req, nil, nil)
}

// UDPAnnounceConn 同上;relay 非空时数据报经该 PacketConn 发收
// (SOCKS5 UDP 中继,代理场景下绕过本机 UDP 封锁)。
// resolve 非空时 tracker 域名经其解析(DoH,规避本地 DNS 污染)。
func UDPAnnounceConn(ctx context.Context, rawURL string, req AnnounceReq, relay net.PacketConn, resolve func(ctx context.Context, host string) ([]net.IP, error)) (*AnnounceResp, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("tracker 地址非法: %w", err)
	}
	if u.Scheme != "udp" {
		return nil, errTrackerScheme
	}
	if u.Port() == "" {
		return nil, errors.New("udp tracker 缺少端口(应为 udp://host:port)")
	}
	// tracker 域名解析:优先注入的 DoH 解析器,系统 DNS 可能被污染
	host := u.Host
	if resolve != nil && net.ParseIP(u.Hostname()) == nil {
		if ips, err := resolve(ctx, u.Hostname()); err == nil && len(ips) > 0 {
			host = net.JoinHostPort(ips[0].String(), u.Port())
		}
	}
	x := &udpXport{pc: relay}
	if relay == nil {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "udp", host)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		x.conn = conn
	} else {
		ra, err := net.ResolveUDPAddr("udp4", host)
		if err != nil {
			return nil, err
		}
		x.addr = ra
	}

	txid := uint32(time.Now().UnixNano()) ^ req.Key

	// connect
	connID, err := udpConnect(ctx, x, txid)
	if err != nil {
		return nil, err
	}

	// announce
	buf, err := udpRequest(ctx, x, txid+1, udpAnnouncePacket(connID, txid+1, req), 20)
	if err != nil {
		return nil, err
	}
	resp := &AnnounceResp{
		Interval:   int64(binary.BigEndian.Uint32(buf[8:12])),
		Incomplete: int64(binary.BigEndian.Uint32(buf[12:16])),
		Complete:   int64(binary.BigEndian.Uint32(buf[16:20])),
	}
	if resp.Interval <= 0 {
		resp.Interval = 1800
	}
	resp.Peers = dedupPeers(decodeCompactPeers(string(buf[20:])))
	return resp, nil
}

// udpXport 统一直连 UDP 与中继 PacketConn 两种收发路径。
type udpXport struct {
	conn net.Conn       // 直连路径
	pc   net.PacketConn // 中继路径
	addr *net.UDPAddr   // 中继目标(仅 pc 路径使用)
}

func (x *udpXport) write(p []byte) error {
	if x.pc != nil {
		_, err := x.pc.WriteTo(p, x.addr)
		return err
	}
	_, err := x.conn.Write(p)
	return err
}

func (x *udpXport) read(buf []byte) (int, error) {
	if x.pc != nil {
		n, _, err := x.pc.ReadFrom(buf)
		return n, err
	}
	return x.conn.Read(buf)
}

func udpConnect(ctx context.Context, x *udpXport, txid uint32) (int64, error) {
	pkt := make([]byte, 16)
	binary.BigEndian.PutUint64(pkt[0:8], udpMagic)
	binary.BigEndian.PutUint32(pkt[8:12], 0)
	binary.BigEndian.PutUint32(pkt[12:16], txid)
	buf, err := udpRequest(ctx, x, txid, pkt, 16)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(buf[8:16])), nil
}

func udpAnnouncePacket(connID int64, txid uint32, req AnnounceReq) []byte {
	numwant := req.NumWant
	if numwant <= 0 {
		numwant = 100
	}
	pkt := make([]byte, 98)
	binary.BigEndian.PutUint64(pkt[0:8], uint64(connID))
	binary.BigEndian.PutUint32(pkt[8:12], 1)
	binary.BigEndian.PutUint32(pkt[12:16], txid)
	copy(pkt[16:36], req.InfoHash[:])
	copy(pkt[36:56], req.PeerID[:])
	binary.BigEndian.PutUint64(pkt[56:64], uint64(req.Downloaded))
	binary.BigEndian.PutUint64(pkt[64:72], uint64(req.Left))
	binary.BigEndian.PutUint64(pkt[72:80], uint64(req.Uploaded))
	binary.BigEndian.PutUint32(pkt[80:84], uint32(req.Event))
	binary.BigEndian.PutUint32(pkt[84:88], req.IP)
	binary.BigEndian.PutUint32(pkt[88:92], req.Key)
	binary.BigEndian.PutUint32(pkt[92:96], uint32(numwant))
	binary.BigEndian.PutUint16(pkt[96:98], req.Port)
	return pkt
}

// udpRequest 发送一个请求,超时按 BEP-15 指数重传(最多 3 次),
// 直到收到匹配 txid 的应答或服务器返回 action=3 错误。返回原始应答缓冲。
func udpRequest(ctx context.Context, x *udpXport, txid uint32, pkt []byte, minResp int) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		deadline := time.Now().Add(15 * time.Second << attempt)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if x.conn != nil {
			x.conn.SetDeadline(deadline)
		} else {
			d := deadline
			x.pc.SetWriteDeadline(d)
			x.pc.SetReadDeadline(d)
		}
		if err := x.write(pkt); err != nil {
			return nil, err
		}
		buf := make([]byte, 4096)
		n, err := x.read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return nil, err
		}
		buf = buf[:n]
		if n < minResp {
			return nil, fmt.Errorf("udp tracker 应答过短(%d 字节)", n)
		}
		if action := binary.BigEndian.Uint32(buf[0:4]); action == 3 {
			return nil, fmt.Errorf("udp tracker 错误: %s", string(buf[8:]))
		}
		if binary.BigEndian.Uint32(buf[4:8]) != txid {
			continue // 陈旧应答,重传
		}
		return buf, nil
	}
	return nil, errors.New("udp tracker 应答超时(已重试 3 次)")
}
