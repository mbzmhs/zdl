package bt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// specPeer:按 BEP-29 规范 + libutp 参考语义独立实现的最小 uTP 端点,
// 与本包 utpConn 零共享代码,用于交叉验证标准兼容性:
//   - SYN 携带发起方 recv_id;接受方 send = SYN.conn_id,recv = SYN.conn_id+1;
//   - SYN 消耗 seq;SYN-ACK 不消耗 seq(首个数据包与 SYN-ACK 同 seq);
//   - 纯 ST_STATE 不消耗 seq;逐包立即 ack(不停顿)。

const specMSS = 1400

type specPeer struct {
	pc    *net.UDPConn
	raddr *net.UDPAddr

	sendID uint16
	recvID uint16
	seq    uint16 // 下一个要发的 seq(SYN 已消耗)
	ack    uint16 // 对端方向最后按序收到的 seq

	poisonState uint32  // 非零:STATE 包的 ts_diff 毒化为该值(测试 LEDBAT 防抖)
	lossRate    float64 // 入包丢弃率(模拟损管网,0 = 无损)
	debug       bool

	mu       sync.Mutex
	cond     *sync.Cond
	theirAck uint16            // 对端已确认的我方最大 seq
	unacked  map[uint16][]byte // 在途待确认(seq → payload)
	reorder  map[uint16][]byte
	inBuf    []byte
	readDL   time.Time
	closed   chan struct{}
	once     sync.Once
	ackCh    chan uint16 // 入站 STATE 的 ackNr 通知(流式测试用,nil 时不上报)
}

func (s *specPeer) logf(format string, args ...any) {
	if s.debug {
		fmt.Printf("[spec] "+format+"\n", args...)
	}
}

func (s *specPeer) close() {
	s.once.Do(func() {
		close(s.closed)
		s.pc.Close()
	})
}

// sendPkt 发一个包(STATE 不消耗 seq,DATA 由调用方管理 seq)。
func (s *specPeer) sendPkt(pktType byte, seq uint16, payload []byte) {
	var h utpHeader
	h.pktType, h.ver = pktType, utpVersion
	h.connID = s.sendID
	h.timestamp = utpNowUS()
	h.wndSize = 1 << 20
	h.seqNr = seq
	s.mu.Lock()
	h.ackNr = s.ack
	s.mu.Unlock()
	buf := make([]byte, utpHeaderLen+len(payload))
	h.marshal(buf[:utpHeaderLen])
	copy(buf[utpHeaderLen:], payload)
	s.pc.WriteTo(buf, s.raddr)
}

// loop 收包循环:DATA 按序交付并立即 STATE 确认;STATE 更新 theirAck。
func (s *specPeer) loop() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		h, err := parseUTPHeader(buf[:n])
		if err != nil {
			continue
		}
		if s.lossRate > 0 && randFloat() < s.lossRate {
			continue // 损管网:丢包
		}
		s.logf("收包 type=%d connID=%d seq=%d ack=%d len=%d", h.pktType, h.connID, h.seqNr, h.ackNr, n-utpHeaderLen)
		rest := buf[utpHeaderLen:n]
		// 跳过扩展链
		if h.extension != 0 {
			off := 0
			next := h.extension
			for next != 0 && off+2 <= len(rest) {
				next = rest[off]
				off += 2 + int(rest[off+1])
			}
			if off > len(rest) {
				continue
			}
			rest = rest[off:]
		}
		switch h.pktType {
		case utpState:
			s.logf("STATE seq=%d ack=%d", h.seqNr, h.ackNr)
			s.mu.Lock()
			s.theirAck = h.ackNr
			s.cond.Broadcast()
			s.mu.Unlock()
			if s.ackCh != nil {
				select {
				case s.ackCh <- h.ackNr:
				default:
				}
			}
		case utpData:
			if h.connID != s.recvID {
				s.logf("DATA connID 不符 丢弃")
				continue
			}
			s.logf("DATA seq=%d len=%d 期望=%d", h.seqNr, len(rest), (s.ack+1)&0xFFFF)
			s.mu.Lock()
			dist := (h.seqNr - s.ack - 1) & 0xFFFF
			switch {
			case dist == 0:
				s.inBuf = append(s.inBuf, rest...)
				s.ack = h.seqNr
				for {
					next := (s.ack + 1) & 0xFFFF
					b, ok := s.reorder[next]
					if !ok {
						break
					}
					delete(s.reorder, next)
					s.inBuf = append(s.inBuf, b...)
					s.ack = next
				}
			case dist < 1024:
				if _, dup := s.reorder[h.seqNr]; !dup {
					p := make([]byte, len(rest))
					copy(p, rest)
					s.reorder[h.seqNr] = p
				}
			}
			// 立即确认(简化实现,不停顿)
			h2 := utpHeader{pktType: utpState, ver: utpVersion, connID: s.sendID,
				timestamp: utpNowUS(), wndSize: 1 << 20, seqNr: s.seq, ackNr: s.ack}
			if s.poisonState != 0 {
				h2.tsDiff = s.poisonState // 毒化:回传垃圾时延样本
			}
			out := make([]byte, utpHeaderLen)
			h2.marshal(out)
			s.cond.Broadcast() // 唤醒阻塞中的 Read
			s.mu.Unlock()
			s.pc.WriteTo(out, addr)
		}
	}
}

// ---- net.Conn 接口 ----

func (s *specPeer) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if len(s.inBuf) > 0 {
			n := copy(p, s.inBuf)
			s.inBuf = s.inBuf[n:]
			return n, nil
		}
		select {
		case <-s.closed:
			return 0, io.EOF
		default:
		}
		s.logf("Read 阻塞")
		if !s.readDL.IsZero() {
			if time.Now().After(s.readDL) {
				return 0, errTimeout("spec 读超时")
			}
			dl := s.readDL
			t := time.AfterFunc(time.Until(dl), func() {
				s.mu.Lock()
				s.cond.Broadcast()
				s.mu.Unlock()
			})
			s.cond.Wait()
			t.Stop()
			continue
		}
		s.cond.Wait()
	}
}

func (s *specPeer) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := s.writeChunk(p)
		total += n
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// writeChunk 停等发送:发一个 MSS,等待对端 ack(每 200ms 重发兜底)。
func (s *specPeer) writeChunk(p []byte) (int, error) {
	chunk := p
	if len(chunk) > specMSS {
		chunk = chunk[:specMSS]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.seq
	s.seq++
	payload := make([]byte, len(chunk))
	copy(payload, chunk)
	s.unacked[seq] = payload
	send := func() {
		if s.lossRate > 0 && randFloat() < s.lossRate {
			return // 损管网:出包丢弃(靠重传恢复)
		}
		h := utpHeader{pktType: utpData, ver: utpVersion, connID: s.sendID,
			timestamp: utpNowUS(), wndSize: 1 << 20, seqNr: seq, ackNr: s.ack}
		buf := make([]byte, utpHeaderLen+len(payload))
		h.marshal(buf[:utpHeaderLen])
		copy(buf[utpHeaderLen:], payload)
		s.pc.WriteTo(buf, s.raddr)
	}
	send()
	s.logf("发送 DATA seq=%d len=%d", seq, len(payload))
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-s.closed:
			return 0, io.EOF
		default:
		}
		// seq 已被对端确认:seq 落在 theirAck 上或其后方(环上距离 > 半环)
		dist := (seq - s.theirAck) & 0xFFFF
		if dist == 0 || dist > 0x8000 {
			delete(s.unacked, seq)
			return len(chunk), nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("spec 写超时: seq %d 未确认", seq)
		}
		send() // 未确认:重发(200ms 节奏由下方 cond 唤醒约束)
		t := time.AfterFunc(200*time.Millisecond, func() {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		})
		s.cond.Wait()
		t.Stop()
	}
}

func (s *specPeer) termDone() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *specPeer) Close() error         { s.close(); return nil }
func (s *specPeer) LocalAddr() net.Addr  { return s.pc.LocalAddr() }
func (s *specPeer) RemoteAddr() net.Addr { return s.raddr }
func (s *specPeer) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDL = t
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}
func (s *specPeer) SetReadDeadline(t time.Time) error  { return s.SetDeadline(t) }
func (s *specPeer) SetWriteDeadline(t time.Time) error { return nil }

// specDial 以发起方身份按规范建连。
func specDial(raddr *net.UDPAddr) (*specPeer, error) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	s := &specPeer{
		pc: pc, raddr: raddr,
		recvID:  uint16(randU16()),
		unacked: map[uint16][]byte{}, reorder: map[uint16][]byte{},
		closed: make(chan struct{}),
	}
	s.sendID = s.recvID + 1
	s.cond = sync.NewCond(&s.mu)

	// SYN:携带本端 recv_id,消耗一个 seq
	synSeq := uint16(randU16())
	s.mu.Lock()
	s.seq = synSeq + 1
	s.mu.Unlock()
	var h utpHeader
	h.pktType, h.ver, h.connID = utpSyn, utpVersion, s.recvID
	h.timestamp = utpNowUS()
	h.wndSize = 1 << 20
	h.seqNr = synSeq
	buf := make([]byte, utpHeaderLen)
	h.marshal(buf)
	deadline := time.Now().Add(5 * time.Second)
	for {
		pc.WriteTo(buf, raddr)
		pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		pkt := make([]byte, 65536)
		n, _, err := pc.ReadFrom(pkt)
		if err == nil {
			rh, perr := parseUTPHeader(pkt[:n])
			if perr == nil && rh.pktType == utpState && rh.ackNr == synSeq {
				s.mu.Lock()
				s.ack = rh.seqNr - 1 // SYN-ACK 不消耗 seq:期望首个数据 = rh.seqNr
				s.theirAck = synSeq
				s.mu.Unlock()
				pc.SetReadDeadline(time.Time{}) // 清掉建连轮询的短 deadline
				go s.loop()                     // 建连完成才起收包循环(避免与建连等待抢包)
				return s, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("spec 建连超时")
		}
	}
}

func randU16() uint16 { return uint16(os.Getpid()) ^ uint16(time.Now().UnixNano()) }

var specRNG = rand.New(rand.NewSource(time.Now().UnixNano()))

func randFloat() float64 { return specRNG.Float64() }

// specAccept 等待规范对端的 SYN 并完成接受方初始化。
func specAccept(pc *net.UDPConn, timeout time.Duration) (*specPeer, error) {
	deadline := time.Now().Add(timeout)
	pkt := make([]byte, 65536)
	for {
		pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, addr, err := pc.ReadFrom(pkt)
		if err != nil {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("spec 等待 SYN 超时")
			}
			continue
		}
		h, err := parseUTPHeader(pkt[:n])
		if err != nil || h.pktType != utpSyn {
			continue
		}
		ua := addr.(*net.UDPAddr)
		s := &specPeer{
			pc: pc, raddr: ua,
			sendID:  h.connID,     // 规范:接受方 send = SYN.conn_id
			recvID:  h.connID + 1, // 接受方 recv = SYN.conn_id + 1
			ack:     h.seqNr,      // SYN 消耗 seq:期望对端下一包 = SYN.seq+1
			unacked: map[uint16][]byte{}, reorder: map[uint16][]byte{},
			closed: make(chan struct{}),
			ackCh:  make(chan uint16, 512),
		}
		s.cond = sync.NewCond(&s.mu)
		s.mu.Lock()
		s.seq = uint16(randU16()) // SYN-ACK 用当前 seq 且不消耗:首个数据同 seq
		h2 := utpHeader{pktType: utpState, ver: utpVersion, connID: s.sendID,
			timestamp: utpNowUS(), wndSize: 1 << 20, seqNr: s.seq, ackNr: s.ack}
		out := make([]byte, utpHeaderLen)
		h2.marshal(out)
		s.mu.Unlock()
		pc.WriteTo(out, ua)
		pc.SetReadDeadline(time.Time{}) // 清掉轮询期的短 deadline,再起收包循环
		go s.loop()
		return s, nil
	}
}

// ---- BT 层:假种子端 ----

func frameMsg(id byte, payload []byte) []byte {
	out := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(out, uint32(1+len(payload)))
	out[4] = id
	copy(out[5:], payload)
	return out
}

// specServeSeeder 在已建立的 uTP 连接上跑最小 BT 种子端。speakFirst 表示
// 本端是传输发起方(BT 约定:传输发起方先发握手)。握手 → 全量 bitfield →
// unchoke → 按 request 供数据。
func specServeSeeder(t *testing.T, conn net.Conn, meta *MetaInfo, data []byte, speakFirst bool) {
	t.Helper()
	defer conn.Close()
	fmt.Printf("[spec] seeder 开始服务(speakFirst=%v)\n", speakFirst)
	conn.SetDeadline(time.Now().Add(120 * time.Second))
	var got *Handshake
	var err error
	if speakFirst {
		if err := WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: [20]byte{'S', 'P'}}, true, true, true); err != nil {
			fmt.Printf("[spec] 写握手失败: %v\n", err)
			return
		}
		got, err = ReadHandshake(conn)
	} else {
		got, err = ReadHandshake(conn)
	}
	if err != nil {
		t.Logf("spec seeder: 读握手失败: %v", err)
		return
	}
	fmt.Printf("[spec] 读到握手 infohash匹配=%v\n", got.InfoHash == meta.InfoHash)
	if got.InfoHash != meta.InfoHash {
		t.Logf("spec seeder: infohash 不符")
		return
	}
	if !speakFirst {
		if err := WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: [20]byte{'S', 'P'}}, true, true, true); err != nil {
			fmt.Printf("[spec] 写握手失败: %v\n", err)
			return
		}
	}
	fmt.Printf("[spec] 握手完成\n")
	// 全量 bitfield
	total := meta.PieceCount()
	bf := make([]bool, total)
	for i := range bf {
		bf[i] = true
	}
	if _, err := conn.Write(frameMsg(5, bitfieldBytes(bf))); err != nil {
		return
	}
	// unchoke
	if _, err := conn.Write(frameMsg(1, nil)); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	for {
		m, err := ReadMsg(br)
		if err != nil {
			return
		}
		switch m.ID {
		case 2: // interested
		case 6: // request
			idx, begin, length, err := ParseRequest(m.Payload)
			if err != nil {
				return
			}
			// piece 起始偏移按全局流算:sum(前 idx 件的长度)+begin
			off := 0
			for i := 0; i < int(idx); i++ {
				off += int(meta.PieceLen(i))
			}
			start := off + int(begin)
			end := start + int(length)
			if end > len(data) {
				end = len(data)
			}
			pl := make([]byte, 8, 8+end-start)
			binary.BigEndian.PutUint32(pl[0:4], idx)
			binary.BigEndian.PutUint32(pl[4:8], begin)
			pl = append(pl, data[start:end]...)
			if _, err := conn.Write(frameMsg(7, pl)); err != nil {
				return
			}
		}
	}
}

// buildSingleFileMeta 造一个单文件种子与其源数据(供假种子端按流偏移服务)。
func buildSingleFileMeta(t *testing.T) (*MetaInfo, []byte) {
	return buildSingleFileMetaN(t, 400<<10)
}

// buildSingleFileMetaN 同上,样本大小可调;损网用例用小样本缩短在途
// 轮次(停止等待协议下样本大小直接决定损网暴露面)。
func buildSingleFileMetaN(t *testing.T, size int) (*MetaInfo, []byte) {
	t.Helper()
	dir := t.TempDir()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(dir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	return meta, data
}

// retryNet 时序敏感用例(损网/真实计时)的环境抖动护栏:机器负载、
// 丢包雪崩等偶发因素允许整体重跑,attempts 次全败才判 FAIL。
// body 内不得 t.Fatal,一律返回错误。
func retryNet(t *testing.T, attempts int, body func(t *testing.T) error) {
	t.Helper()
	var last error
	for i := 1; i <= attempts; i++ {
		err := body(t)
		if err == nil {
			if i > 1 {
				t.Logf("损网用例第 %d 次尝试通过(前次为环境抖动)", i)
			}
			return
		}
		last = err
		t.Logf("损网用例第 %d/%d 次尝试失败: %v", i, attempts, err)
	}
	t.Fatalf("重试 %d 次仍失败(非环境抖动?): %v", attempts, last)
}

// waitDoneErr 等待下载完成的错误返回版(供 retryNet 的 body 使用)。
func waitDoneErr(c *Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.WaitComplete(ctx)
}

// TestUTPSpecInbound:规范对端发起 uTP 入站(用户截图场景)→ 我方下载完成。
func TestUTPSpecInbound(t *testing.T) {
	meta, data := buildSingleFileMeta(t)

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	port := leech.cfg.ListenPort

	// 规范对端:等待我们的 uTP SYN?不——对端是发起方:向我们的 UDP 端口发 SYN
	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	seeder, err := specDial(udpAddr)
	if err != nil {
		t.Fatalf("规范对端建连失败: %v", err)
	}
	defer seeder.close()
	go specServeSeeder(t, seeder, meta, data, true)

	waitDone(t, leech)
	if leech.FinalPath() == "" {
		t.Fatal("下载未产出文件")
	}
}

// TestUTPSpecOutbound:我方发起 uTP 出站 → 规范对端(接受方)供数据。
func TestUTPSpecOutbound(t *testing.T) {
	meta, data := buildSingleFileMeta(t)

	// 规范对端先监听
	spc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer spc.Close()
	fakePort := spc.LocalAddr().(*net.UDPAddr).Port

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	// 后台跑假种子端(接受 SYN 后供数据)
	served := make(chan struct{})
	go func() {
		sp, err := specAccept(spc, 30*time.Second)
		if err != nil {
			t.Logf("spec accept: %v", err)
			return
		}
		defer sp.close()
		close(served)
		specServeSeeder(t, sp, meta, data, false)
	}()

	// 手动 uTP 出站(绕过 MSE,专门验证传输层):与 dialOutUTP 同构,明文握手
	addr := fmt.Sprintf("127.0.0.1:%d", fakePort)
	dctx, cancel := context.WithTimeout(leech.baseCtx, 5*time.Second)
	wire, err := leech.utpm.Dial(dctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fakePort})
	cancel()
	if err != nil {
		t.Fatalf("uTP 出站拨号失败: %v", err)
	}
	got, ok := leech.outboundHandshake(wire, addr)
	if !ok {
		t.Fatal("uTP 出站握手失败")
	}
	registered := leech.register(addr, got, wire)
	if registered == nil {
		t.Fatal("uTP 出站连接注册失败")
	}
	registered.utp = true
	// run 会阻塞在连接读循环上直到连接关闭(生产中由独立拨号协程调用)。
	// 完成后连接不再自行断开,故放到协程里跑,由 waitDone 等下载完成。
	go registered.run()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
	}
	waitDone(t, leech)
}

// TestUTPSpecInboundLossy:损管网(双向 5% 丢包)下的入站交叉验证——
// 锻炼我方快速重传/RTO 恢复(我方→对端)与乱序重组(对端→我方)。
// 真实计时+随机丢包存在环境抖动,retryNet 兜底。
func TestUTPSpecInboundLossy(t *testing.T) {
	retryNet(t, 3, utpInboundLossyOnce)
}

func utpInboundLossyOnce(t *testing.T) error {
	meta, data := buildSingleFileMetaN(t, 200<<10)

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		return err
	}
	leech.Start()
	defer leech.Close()

	seeder, err := specDial(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: leech.cfg.ListenPort})
	if err != nil {
		return fmt.Errorf("规范对端建连失败: %w", err)
	}
	defer seeder.close()
	seeder.lossRate = 0.05
	seeder.debug = true
	go specServeSeeder(t, seeder, meta, data, true)

	return waitDoneErr(leech, 60*time.Second)
}

// TestUTPSpecOutboundLossy:损管网下的出站交叉验证。
func TestUTPSpecOutboundLossy(t *testing.T) {
	retryNet(t, 3, utpOutboundLossyOnce)
}

func utpOutboundLossyOnce(t *testing.T) error {
	meta, data := buildSingleFileMetaN(t, 200<<10)

	spc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer spc.Close()
	fakePort := spc.LocalAddr().(*net.UDPAddr).Port

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		return err
	}
	leech.Start()
	defer leech.Close()

	served := make(chan struct{})
	go func() {
		sp, err := specAccept(spc, 30*time.Second)
		if err != nil {
			return
		}
		defer sp.close()
		sp.lossRate = 0.05
		close(served)
		specServeSeeder(t, sp, meta, data, false)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", fakePort)
	dctx, cancel := context.WithTimeout(leech.baseCtx, 5*time.Second)
	wire, err := leech.utpm.Dial(dctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fakePort})
	cancel()
	if err != nil {
		return fmt.Errorf("uTP 出站拨号失败: %w", err)
	}
	got, ok := leech.outboundHandshake(wire, addr)
	if !ok {
		return fmt.Errorf("uTP 出站握手失败")
	}
	registered := leech.register(addr, got, wire)
	if registered == nil {
		return fmt.Errorf("uTP 出站连接注册失败")
	}
	registered.utp = true
	// run 会阻塞在连接读循环上直到连接关闭(生产中由独立拨号协程调用)。
	// 完成后连接不再自行断开,故放到协程里跑,由 waitDoneErr 等下载完成。
	go registered.run()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
	}
	return waitDoneErr(leech, 60*time.Second)
}

// TestUTPSeederServesSpecLeech:入站 uTP leech(规范对端)从我们做种端
// 下载——握手应答 → have-all/bitfield → interested → unchoke → request →
// piece 数据,全链路验证做种侧服务能力。
func TestUTPSeederServesSpecLeech(t *testing.T) {
	// 造一个带数据的单文件种子目录,做种端直接以该目录做种
	dir := t.TempDir()
	data := make([]byte, 400<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(dir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := NewSeedClient(meta, dir, ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	sp, err := specDial(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seed.cfg.ListenPort})
	if err != nil {
		t.Fatalf("spec 建连失败: %v", err)
	}
	defer sp.close()
	conn := net.Conn(sp)
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// 我方=传输发起方:先发 BT 握手
	if err := WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: [20]byte{'L', 'E'}}, true, true, true); err != nil {
		t.Fatalf("写握手: %v", err)
	}
	if _, err := ReadHandshake(conn); err != nil {
		t.Fatalf("读握手应答: %v", err)
	}
	// 表达兴趣:做种端应据此放行(unchoke)
	if _, err := conn.Write(frameMsg(2, nil)); err != nil {
		t.Fatalf("写 interested: %v", err)
	}

	br := bufio.NewReader(conn)
	gotBitfield := false
	gotUnchoke := false
	var piece []byte
	deadline := time.Now().Add(25 * time.Second)
	requested := false
	for time.Now().Before(deadline) {
		if gotBitfield && gotUnchoke && piece != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(25 * time.Second))
		m, err := ReadMsg(br)
		if err != nil {
			t.Fatalf("读消息(bitfield=%v unchoke=%v piece=%v): %v", gotBitfield, gotUnchoke, piece != nil, err)
		}
		switch m.ID {
		case 14, 5: // have-all(BEP-6=0x0E) / bitfield
			gotBitfield = true
		case 1: // unchoke
			gotUnchoke = true
		case 0:
			if gotBitfield {
				t.Fatal("做种端在 interested 后仍 choke 我们")
			}
		}
		// 位图+放行就绪后立即请求 piece0
		if gotBitfield && gotUnchoke && !requested {
			requested = true
			if _, err := conn.Write(frameMsg(6, BuildRequest(0, 0, uint32(meta.PieceLen(0))))); err != nil {
				t.Fatalf("写 request: %v", err)
			}
		}
		if m.ID == 7 {
			piece = m.Payload
		}
	}
	if !gotBitfield {
		t.Fatal("做种端从未发送 have-all/bitfield")
	}
	if !gotUnchoke {
		t.Fatal("做种端从未 unchoke(对 interested 的入站 leech)")
	}
	if piece == nil {
		t.Fatal("未收到 piece 数据")
	}
	idx, begin, blk, err := ParsePiece(piece)
	if err != nil || idx != 0 || begin != 0 {
		t.Fatalf("piece 头不符: idx=%d begin=%d err=%v", idx, begin, err)
	}
	if !bytes.Equal(blk, data[:len(blk)]) {
		t.Fatalf("piece 数据不符: len=%d", len(blk))
	}
}

// TestUTPSpecMetadataRequest:磁力 leech(规范对端)经 uTP 请求 ut_metadata,
// 验证做种端应答元数据数据块——uTorrent 类磁力 peer 卡元数据的场景。
func TestUTPSpecMetadataRequest(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 400<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(dir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(meta, dir, ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	sp, err := specDial(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seed.cfg.ListenPort})
	if err != nil {
		t.Fatalf("spec 建连失败: %v", err)
	}
	defer sp.close()
	conn := net.Conn(sp)
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: [20]byte{'M', 'L'}}, true, true, true); err != nil {
		t.Fatalf("写握手: %v", err)
	}
	if _, err := ReadHandshake(conn); err != nil {
		t.Fatalf("读握手应答: %v", err)
	}

	// 扩展握手:声明我们的 ut_metadata 收包 ID = 3
	ourM := MustEncode(dict{"m": dict{"ut_metadata": int64(3)}})
	if _, err := conn.Write(frameMsg(20, append([]byte{0}, ourM...))); err != nil {
		t.Fatalf("写扩展握手: %v", err)
	}
	// 元数据请求:用“我们通告的” ut_metadata ID(=3)发起 piece0 请求
	req := BuildMetadataMsg(metaReq, 0)
	if _, err := conn.Write(frameMsg(20, append([]byte{1}, req...))); err != nil {
		t.Fatalf("写元数据请求: %v", err)
	}

	br := bufio.NewReader(conn)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(25 * time.Second))
		m, err := ReadMsg(br)
		if err != nil {
			t.Fatalf("读应答: %v", err)
		}
		if m.ID != 20 { // msgExtended
			continue
		}
		if len(m.Payload) < 2 || m.Payload[0] != 3 {
			continue // 非 ut_metadata 通道
		}
		mm, err := ParseMetadataMsg(m.Payload[1:])
		if err != nil {
			t.Fatalf("解析元数据应答: %v", err)
		}
		if mm.Type != metaData {
			t.Fatalf("应答类型=%d(应为数据 %d)", mm.Type, metaData)
		}
		want := meta.RawInfo()
		if !bytes.Equal(mm.Data, want) {
			t.Fatalf("元数据不符: got=%d want=%d", len(mm.Data), len(want))
		}
		return
	}
	t.Fatal("做种端从未应答 ut_metadata 请求")
}

// TestUTPGarbageTsDiffNoCollapse:对端 STATE 回传垃圾 ts_diff(时钟漂移
// 量级,~70 分钟)时,我方 cwnd 不得被砸穿卡死——过滤后吞吐应保持正常。
func TestUTPGarbageTsDiffNoCollapse(t *testing.T) {
	meta, data := buildSingleFileMeta(t)

	spc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer spc.Close()
	fakePort := spc.LocalAddr().(*net.UDPAddr).Port

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	served := make(chan struct{})
	go func() {
		sp, err := specAccept(spc, 30*time.Second)
		if err != nil {
			t.Logf("spec accept: %v", err)
			return
		}
		defer sp.close()
		sp.poisonState = uint32(4_200_000_000) // 毒化 STATE 的 ts_diff 字段
		close(served)
		specServeSeeder(t, sp, meta, data, false)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", fakePort)
	dctx, cancel := context.WithTimeout(leech.baseCtx, 5*time.Second)
	wire, err := leech.utpm.Dial(dctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fakePort})
	cancel()
	if err != nil {
		t.Fatalf("uTP 出站拨号失败: %v", err)
	}
	got, ok := leech.outboundHandshake(wire, addr)
	if !ok {
		t.Fatal("uTP 出站握手失败")
	}
	registered := leech.register(addr, got, wire)
	if registered == nil {
		t.Fatal("uTP 出站连接注册失败")
	}
	registered.utp = true
	// run 会阻塞在连接读循环上直到连接关闭(生产中由独立拨号协程调用)。
	// 完成后连接不再自行断开,故放到协程里跑,由 waitDone 等下载完成。
	go registered.run()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
	}
	waitDone(t, leech)
}

// TestUTPStreamingAckFresh 回归:流式灌包下 ACK 不得饿死。
// 修复前:每个 DATA 都把延迟 ack 的 ackAt 后移,包间隔 <50ms 的持续流让
// 冲刷永远不触发,对端丢包后收不到任何重复 ack/SACK,只能干等 RTO
// (线上实测一个 3 包缺口停 14 秒,表现为"uTP 连着但零传输")。
// 修复后:乱序包立即 ACK,对端毫秒级拿到快速重传信号。
func TestUTPStreamingAckFresh(t *testing.T) {
	meta, _ := buildSingleFileMeta(t)

	spc, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer spc.Close()
	fakePort := spc.LocalAddr().(*net.UDPAddr).Port

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	const (
		totalPkts = 100
		pktLen    = specMSS
		dropIdx   = 10 // 第 10 个数据包定点丢弃
	)
	type streamResult struct {
		dropAt   time.Time
		firstDup time.Time // 首个冻结在缺口前的重复 ack 到达时间
		err      error
	}
	resCh := make(chan streamResult, 1)

	go func() {
		sp, err := specAccept(spc, 15*time.Second)
		if err != nil {
			resCh <- streamResult{err: err}
			return
		}
		defer sp.close()
		var res streamResult
		payload := bytes.Repeat([]byte{0xA5}, pktLen)
		base := sp.seq // SYN-ACK 不消耗 seq:首个数据同 seq
		holeSeq := uint16(0)
		var holePayload []byte
		var dups int
		resent := false
		deadline := time.Now().Add(20 * time.Second)
		for sent := 0; sent < totalPkts; {
			if time.Now().After(deadline) {
				resCh <- streamResult{err: fmt.Errorf("流式发送超时: 已发 %d", sent)}
				return
			}
			// 消费入站 ack:缺口前冻结的重复 ack 计数,≥3 触发快速重传
		drain:
			for {
				select {
				case ackNr := <-sp.ackCh:
					if holeSeq != 0 && ackNr == (holeSeq-1)&0xFFFF {
						dups++
						if dups == 1 {
							res.firstDup = time.Now()
						}
					}
				default:
					break drain
				}
			}
			if holeSeq != 0 && dups >= 3 && !resent {
				sp.sendPkt(utpData, holeSeq, holePayload)
				resent = true
			}
			sent++
			seq := (base + uint16(sent) - 1) & 0xFFFF // 首个数据与 SYN-ACK 同 seq(不消耗)
			if sent == dropIdx {
				res.dropAt = time.Now()
				holeSeq = seq
				holePayload = append([]byte(nil), payload...)
				continue // 定点丢弃
			}
			sp.sendPkt(utpData, seq, payload)
			time.Sleep(20 * time.Millisecond) // 慢速流:包间隔远小于 50ms 延迟 ack 窗
		}
		// 全部发完,等缺口补齐的余量 ack 再收尾
		time.Sleep(300 * time.Millisecond)
		resCh <- res
	}()

	dctx, cancel := context.WithTimeout(leech.baseCtx, 5*time.Second)
	conn, err := leech.utpm.Dial(dctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fakePort})
	cancel()
	if err != nil {
		t.Fatalf("uTP 出站拨号失败: %v", err)
	}
	defer conn.Close()

	total := totalPkts * pktLen
	got := make([]byte, total)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("流式读取失败(修复后应无停顿): %v", err)
	}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("规范对端流式发送失败: %v", res.err)
		}
		if res.firstDup.IsZero() {
			t.Fatal("缺口打开后从未收到重复 ack(ACK 饿死回归)")
		}
		if d := res.firstDup.Sub(res.dropAt); d > 500*time.Millisecond {
			t.Fatalf("首个重复 ack 滞后 %v(>500ms,对端只能等 RTO)", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("规范对端未在时限内完成流式发送")
	}
}
