package bt

import (
	"context"
	"net"
	"testing"
	"time"
)

// fakeForeignPeer 模拟异构对端(如 libutp 系):时钟基不同(+offsetC µs)、
// 延迟 ack(delayMs)、广告窗口 advWnd。只实现接受 SYN 与累计 ack。
type fakeForeignPeer struct {
	pc       *net.UDPConn
	offsetC  uint32 // 对端时戳基偏移(µs):本端时戳与 ts_diff 都叠加它
	delayMs  int    // 延迟 ack
	advWnd   uint32
	lastTs   uint32   // 对端最近发包时戳(ts_diff 回显用)
	lastAddr net.Addr // 最近来包地址(定时冲刷 ack 用)
}

func (f *fakeForeignPeer) nowUs() uint32 { return uint32(time.Since(utpClockStart).Microseconds()) }

func (f *fakeForeignPeer) run(t *testing.T) {
	buf := make([]byte, 65536)
	var ackNr uint16
	var highest uint16
	var pending bool
	var pendAt time.Time
	var synConnID uint16
	for {
		// pending 时把读超时顶到 ack 冲刷点:模拟对端的独立定时器,
		// 否则延迟 ack 只能等下一个包到达才发出(与发送端互等死锁)
		if pending {
			f.pc.SetReadDeadline(pendAt)
		} else {
			f.pc.SetReadDeadline(time.Now().Add(30 * time.Second))
		}
		n, addr, err := f.pc.ReadFrom(buf)
		if err != nil {
			if pending {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					pending = false
					ackNr = highest
					f.reply(f.lastAddr, synConnID, ackNr)
					continue
				}
			}
			return
		}
		f.lastAddr = addr
		if !isUTPPacket(buf[:n]) {
			continue
		}
		h, err := parseUTPHeader(buf[:n])
		if err != nil {
			continue
		}
		f.lastTs = h.timestamp
		switch h.pktType {
		case utpSyn:
			ackNr = h.seqNr
			synConnID = h.connID
			f.reply(addr, synConnID, ackNr)
		case utpData:
			if h.seqNr != ackNr {
				highest = h.seqNr
				if !pending {
					pending = true
					pendAt = time.Now().Add(time.Duration(f.delayMs) * time.Millisecond)
				}
			}
		}
		if pending && !time.Now().Before(pendAt) {
			pending = false
			ackNr = highest
			f.reply(addr, synConnID, ackNr)
		}
	}
}

// reply 回 ST_STATE:timestamp 用我方时钟+偏移;ts_diff 按 uTP 语义回显
// 我方时钟 - 对端最近发包时戳(跨时钟基时即"带常数偏移的垃圾样本")。
func (f *fakeForeignPeer) reply(addr net.Addr, connID uint16, ackNr uint16) {
	h := utpHeader{
		pktType: utpState, ver: utpVersion,
		connID:    connID,
		timestamp: f.nowUs() + f.offsetC,
		tsDiff:    (f.nowUs() + f.offsetC) - f.lastTs,
		wndSize:   f.advWnd,
		seqNr:     0x1234,
		ackNr:     ackNr,
	}
	buf := make([]byte, utpHeaderLen)
	h.marshal(buf)
	f.pc.WriteTo(buf, addr)
}

// TestUTPThroughputForeignPeer 异构对端参数扫描:定位跨实现 uTP 上传瓶颈。
func TestUTPThroughputForeignPeer(t *testing.T) {
	cases := []struct {
		name    string
		offsetC uint32
		delayMs int
		advWnd  uint32
	}{
		{"同基时钟_立即ack_大窗口", 0, 0, 1 << 20},
		{"偏移3分钟_立即ack_大窗口", 3 * 60 * 1000000, 0, 1 << 20},
		{"偏移60分钟(近回绕)_立即ack_大窗口", 60 * 60 * 1000000, 0, 1 << 20},
		{"同基时钟_延迟40ms_大窗口", 0, 40, 1 << 20},
		{"同基时钟_立即ack_64K窗口", 0, 0, 64 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgA, err := newUTPManagerOwn(0)
			if err != nil {
				t.Fatal(err)
			}
			defer mgA.Close()
			fpc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer fpc.Close()
			fp := &fakeForeignPeer{pc: fpc, offsetC: tc.offsetC, delayMs: tc.delayMs, advWnd: tc.advWnd}
			go fp.run(t)

			conn, err := mgA.Dial(context.Background(), fpc.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatalf("建连: %v", err)
			}
			defer conn.Close()

			payload := make([]byte, utpMSS)
			const total = 16 << 20
			start := time.Now()
			written := 0
			for written < total {
				n, err := conn.Write(payload)
				if err != nil {
					t.Fatalf("写失败 @%d: %v", written, err)
				}
				written += n
			}
			elapsed := time.Since(start)
			mbps := float64(total) / (1 << 20) / elapsed.Seconds()
			t.Logf("吞吐: %.2f MB/s (16MB in %v)", mbps, elapsed)
			// 回归阈值:延迟 ack 场景曾是 3.66MB/s(慢启动按"每 ack +1 包"
			// 被 ack 批次钳死);字节计数后应稳定在 10MB/s 以上
			if tc.delayMs > 0 && mbps < 10 {
				t.Fatalf("延迟 ack 吞吐回归: %.2f MB/s", mbps)
			}
		})
	}
}
