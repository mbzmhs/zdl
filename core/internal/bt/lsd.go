package bt

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LSD(BEP-14)局域网对等发现:向多播组周期广播本机 BT 端口与 infohash,
// 同时监听组内其他客户端的广播,发现即得直连 peer。私有种子不参与。

const (
	lsdGroup = "239.192.152.143"
	lsdPort  = 6771
	// lsdBusyBackoff 组端口被占(常见:本机开了第二个客户端)时的重试间隔
	lsdRetry = 60 * time.Second
)

// LSD 是一个局域网发现参与者。
type LSD struct {
	conn *net.UDPConn

	mu     sync.Mutex
	ihPort [][20]byte // 本机活动会话的 infohash(周期广播用)
	port   int        // 本机 BT 监听端口

	onPeer func(ih [20]byte, addr string) // 收到他人广播时的分发回调

	wg      sync.WaitGroup
	stopCh  chan struct{}
	stopOnc sync.Once
}

// StartLSD 启动局域网发现。onPeer 在后台协程中被调用,须自行保证并发安全。
// 组播端口 6771 被占(如本机第二个客户端)时返回错误,调用方降级为不启用。
func StartLSD(btPort int, onPeer func(ih [20]byte, addr string)) (*LSD, error) {
	if onPeer == nil {
		return nil, fmt.Errorf("bt: LSD 回调为空")
	}
	gaddr := &net.UDPAddr{IP: net.ParseIP(lsdGroup), Port: lsdPort}
	// ListenMulticastUDP 隐式加入多播组(系统默认接口);桌面单网卡场景足够
	conn, err := net.ListenMulticastUDP("udp4", nil, gaddr)
	if err != nil {
		return nil, fmt.Errorf("LSD 监听失败: %w", err)
	}
	l := &LSD{
		conn:   conn,
		port:   btPort,
		onPeer: onPeer,
		stopCh: make(chan struct{}),
	}
	l.wg.Add(1)
	go l.readLoop()
	return l, nil
}

// Register 注册/更新活动会话(infohash 集合随广播周期发出)。
func (l *LSD) Register(ih [20]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.ihPort {
		if h == ih {
			return
		}
	}
	l.ihPort = append(l.ihPort, ih)
}

// Unregister 会话结束后不再广播其 infohash。
func (l *LSD) Unregister(ih [20]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.ihPort[:0]
	for _, h := range l.ihPort {
		if h != ih {
			kept = append(kept, h)
		}
	}
	l.ihPort = kept
}

// Announce 立即广播一次(会话启动时调用;此后每 5 分钟由 AnnounceLoop 触发)。
func (l *LSD) Announce() {
	l.mu.Lock()
	ihs := append([][20]byte(nil), l.ihPort...)
	port := l.port
	l.mu.Unlock()
	if len(ihs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("BT-SEARCH * HTTP/1.1\r\n")
	fmt.Fprintf(&b, "Host: %s:%d\r\n", lsdGroup, lsdPort)
	fmt.Fprintf(&b, "Port: %d\r\n", port)
	for _, ih := range ihs {
		fmt.Fprintf(&b, "Infohash: %x\r\n", ih)
	}
	b.WriteString("\r\n")
	gaddr := &net.UDPAddr{IP: net.ParseIP(lsdGroup), Port: lsdPort}
	l.conn.WriteToUDP([]byte(b.String()), gaddr)
}

func (l *LSD) readLoop() {
	defer l.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, from, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 20 {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		l.handlePacket(pkt, from)
	}
}

// handlePacket 解析他人广播:取 Port 与全部 Infohash 行,回调分发 peer。
func (l *LSD) handlePacket(pkt []byte, from *net.UDPAddr) {
	head := string(pkt[:min(9, len(pkt))])
	if !strings.HasPrefix(head, "BT-SEARCH") {
		return
	}
	port := 0
	var ihs [][20]byte
	for _, line := range strings.Split(string(pkt), "\r\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Port":
			if pn, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pn > 0 && pn < 65536 {
				port = pn
			}
		case "Infohash":
			ih, err := parseInfoHashHex(strings.TrimSpace(v))
			if err == nil {
				ihs = append(ihs, ih)
			}
		}
	}
	if port == 0 || len(ihs) == 0 {
		return
	}
	ip := from.IP.To4()
	if ip == nil || ip.IsLoopback() && port == l.port {
		return // 自己(同机第二实例按 IP+port 区分,不误吞)
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	for _, ih := range ihs {
		l.onPeer(ih, addr)
	}
}

// Stop 停止并关闭。
func (l *LSD) Stop() {
	if l == nil {
		return
	}
	l.stopOnc.Do(func() {
		close(l.stopCh)
		l.conn.Close()
	})
	l.wg.Wait()
}

// parseInfoHashHex 40 位十六进制 → 20 字节。
func parseInfoHashHex(s string) ([20]byte, error) {
	var ih [20]byte
	if len(s) != 40 {
		return ih, fmt.Errorf("infohash 长度不符")
	}
	for i := 0; i < 20; i++ {
		hi := hexVal(s[i*2])
		lo := hexVal(s[i*2+1])
		if hi < 0 || lo < 0 {
			return ih, fmt.Errorf("infohash 含非十六进制字符")
		}
		ih[i] = byte(hi<<4 | lo)
	}
	return ih, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
