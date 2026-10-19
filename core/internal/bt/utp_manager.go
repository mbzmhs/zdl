package bt

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"sync"
	"time"
)

// utpManager 管理一个 UDP socket 上的全部 uTP 连接:
//   - own=true:自己 bind 端口并跑读循环(无 DHT 时);
//   - own=false:由外部(共享端口的 DHT socket)调用 Input 分发,
//     即规范 §8.1 的"同端口复用"模式。
//
// 连接表按 (对端地址, 本端 recvID) 索引;未知 connID 的 ST_SYN 进入
// accept 队列,其余未知包静默丢(防火墙口径)。
type utpManager struct {
	pc       net.PacketConn
	own      bool // true = 自有 socket,Close 时一并关闭
	shared   bool // true = 挂在外部(DHT)socket 上,Close 只终结连接
	mu       sync.Mutex
	conns    map[string]*utpConn // key = addr|recvID
	acceptCh chan *utpConn
	closed   bool
	wg       sync.WaitGroup
}

// newUTPManagerOwn 创建独立端口的 manager(自带读循环)。
func newUTPManagerOwn(port int) (*utpManager, error) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	disableUDPConnReset(pc)   // 防 ICMP 端口不可达毒化 socket(共享 DHT 端口场景)
	pc.SetReadBuffer(2 << 20) // uTP 高速收包,默认缓冲不够
	pc.SetWriteBuffer(1 << 20)
	m := &utpManager{
		pc:       pc,
		own:      true,
		conns:    make(map[string]*utpConn),
		acceptCh: make(chan *utpConn, 16),
	}
	m.wg.Add(1)
	go m.readLoop()
	return m, nil
}

// newUTPManagerShared 创建共享模式 manager(包由外部 Input 注入)。
func newUTPManagerShared(pc net.PacketConn) *utpManager {
	return &utpManager{
		pc:       pc,
		shared:   true,
		conns:    make(map[string]*utpConn),
		acceptCh: make(chan *utpConn, 16),
	}
}

func utpKey(addr net.Addr, recvID uint16) string {
	var b [2]byte
	b[0], b[1] = byte(recvID>>8), byte(recvID)
	return addr.String() + "|" + string(b[:])
}

func (m *utpManager) readLoop() {
	defer m.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, addr, err := m.pc.ReadFrom(buf)
		if err != nil {
			m.mu.Lock()
			closed := m.closed
			m.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		m.Input(pkt, addr)
	}
}

// Input 分发一个 uTP datagram(共享模式的外部入口)。
func (m *utpManager) Input(pkt []byte, addr net.Addr) {
	if !isUTPPacket(pkt) {
		return
	}
	h, err := parseUTPHeader(pkt)
	if err != nil {
		return
	}
	nowMs := utpNowMS()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if h.pktType == utpSyn {
		// SYN 的 connID 是对端期待的收包 ID → 我方收包 ID = +1;
		// 按 (addr, 我方 recvID) 查重:已存在 = 重复 SYN,忽略。
		recvID := h.connID + 1
		if _, dup := m.conns[utpKey(addr, recvID)]; dup {
			m.mu.Unlock()
			return
		}
		udpAddr, _ := addr.(*net.UDPAddr)
		c := newUTPConn(m, udpAddr, h.connID, recvID, false)
		m.conns[utpKey(addr, recvID)] = c
		m.mu.Unlock()
		c.acceptFromSYN(h)
		select {
		case m.acceptCh <- c:
		default: // 队列满:丢弃(上层没在收)
			m.dropConn(c)
		}
		return
	}
	c := m.conns[utpKey(addr, h.connID)]
	m.mu.Unlock()
	if c == nil {
		// 未知连接的非 SYN 包:静默丢(防反射放大;两库对 SYN 均静默)。
		return
	}
	c.input(pkt, nowMs)
}

// Accept 返回下一个入站连接(阻塞)。
func (m *utpManager) Accept(ctx context.Context) (*utpConn, error) {
	select {
	case c := <-m.acceptCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Dial 发起出站 uTP 连接:发 SYN 并等建连(总超时 3s)。
func (m *utpManager) Dial(ctx context.Context, addr *net.UDPAddr) (net.Conn, error) {
	recvID := uint16(rand.Uint32() & 0xFFFF)
	c := newUTPConn(m, addr, recvID+1, recvID, true)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		c.Close()
		return nil, errors.New("uTP manager 已关闭")
	}
	m.conns[utpKey(addr, recvID)] = c
	m.mu.Unlock()

	// 发首个 SYN 并等待(建连状态由 senderLoop 维护重发)。
	c.mu.Lock()
	c.sendSynLocked()
	c.mu.Unlock()

	deadline := time.Now().Add(utpConnectTO * time.Millisecond)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	c.mu.Lock()
	for c.state == utpStateSynSent && time.Now().Before(deadline) {
		wait := time.Until(deadline)
		if wait <= 0 {
			break
		}
		t := time.AfterFunc(wait, func() {
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		})
		c.cond.Wait()
		t.Stop()
	}
	st := c.state
	c.mu.Unlock()
	if st != utpStateConnected {
		c.Close()
		return nil, errors.New("uTP 建连超时或失败")
	}
	return c, nil
}

// dropConn 从连接表移除(幂等)。
func (m *utpManager) dropConn(c *utpConn) {
	m.mu.Lock()
	delete(m.conns, utpKey(c.addr, c.recvID))
	m.mu.Unlock()
}

// Close 关闭 manager:停读循环、终结全部连接;own 模式同时关 socket。
func (m *utpManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	all := make([]*utpConn, 0, len(m.conns))
	for _, c := range m.conns {
		all = append(all, c)
	}
	m.conns = make(map[string]*utpConn)
	m.mu.Unlock()

	for _, c := range all {
		c.mu.Lock()
		if c.state != utpStateReset && c.state != utpStateClosed {
			c.state = utpStateClosed
		}
		c.terminateLocked()
		c.mu.Unlock()
	}
	if m.own {
		m.pc.Close()
	}
	m.wg.Wait()
}
