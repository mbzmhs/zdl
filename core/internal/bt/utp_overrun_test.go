package bt

import (
	"bytes"
	"net"
	"testing"
)

// mkTestUTPConn 构造一条不走网络的测试连接(pc 用 UDP socket 桩)。
func mkTestUTPConn(t *testing.T) *utpConn {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	m := &utpManager{pc: pc, conns: map[string]*utpConn{}}
	return newUTPConn(m, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, 1, 2, false)
}

// TestUTPOverrunDropsNotTruncates 回归:接收预算超限必须丢包(靠对端重传),
// 绝不允许截断 recvBuf——旧实现把流中段静默挖掉,上层 BT 块全部错位,
// 据此把 100% 做种的正常 qBittorrent 误判成毒源封禁(坏块全部来自 uTP)。
func TestUTPOverrunDropsNotTruncates(t *testing.T) {
	c := mkTestUTPConn(t)
	defer c.Close()

	// 灌入 250KB 按序数据(seq 1..N,内容可校验)
	seg := 1416
	payload := make([]byte, seg)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// 按序灌包直到恰好填满预算(utpRecvCap):记录首个被拒的 seq
	seq := uint16(1)
	for {
		if len(c.recvBuf)+seg > utpRecvCap {
			break
		}
		c.deliverDataLocked(seq, payload)
		seq++
	}
	total := len(c.recvBuf)
	if total == 0 || total+seg <= utpRecvCap {
		t.Fatalf("前置: recvBuf=%d 未逼近预算上限", total)
	}
	if len(c.recvBuf) != total {
		t.Fatalf("前置: recvBuf=%d 期望 %d", len(c.recvBuf), total)
	}

	// 超限包(按序):必须整包丢弃,缓冲保持完整、不被"截断防御"挖短
	c.deliverDataLocked(seq, payload)
	if len(c.recvBuf) != total {
		t.Fatalf("超限包被截断缓冲: recvBuf=%d", len(c.recvBuf))
	}
	// 乱序超限包:同样丢弃,乱序暂存不吞
	c.deliverDataLocked(seq+1, payload)
	if len(c.recvBuf) != total || len(c.reorder) != 0 {
		t.Fatalf("乱序超限包未被丢弃: recvBuf=%d reorder=%d", len(c.recvBuf), len(c.reorder))
	}

	// 读出全部数据逐字节校验:流必须完好无损
	got := make([]byte, 0, total)
	buf := make([]byte, 64*1024)
	for len(got) < total {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	want := bytes.Repeat(payload, total/seg+1)[:total]
	if !bytes.Equal(got, want) {
		t.Fatal("uTP 流内容与交付序列不一致(存在数据损坏)")
	}
	// 读空后窗口应恢复满额,重传的包可正常交付
	if w := c.recvWndLocked(); w != utpRecvCap {
		t.Fatalf("读空后窗口=%d 期望 %d", w, utpRecvCap)
	}
	c.deliverDataLocked(seq, payload)
	if c.ackNr != seq {
		t.Fatalf("预算恢复后重传包应交付: ackNr=%d 期望 %d", c.ackNr, seq)
	}
}

// TestUTPWindowCountsReorder 乱序暂存的字节必须计入窗口广告:缺口补齐瞬间
// 整段涌入 recvBuf,不计数会让发送方拿到虚高窗口超发。
func TestUTPWindowCountsReorder(t *testing.T) {
	c := mkTestUTPConn(t)
	defer c.Close()

	p := make([]byte, 1000)
	// seq 2、3 乱序到达(缺 1):暂存 2000 字节,recvBuf 为空
	c.deliverDataLocked(2, p)
	c.deliverDataLocked(3, p)
	if c.reorderBytes != 2000 {
		t.Fatalf("reorderBytes=%d 期望 2000", c.reorderBytes)
	}
	if w := c.recvWndLocked(); w != uint32(utpRecvCap-2000) {
		t.Fatalf("窗口=%d 期望 %d(应扣除乱序占用)", w, utpRecvCap-2000)
	}
	// 补齐 seq 1:暂存冲入 recvBuf,总占用不变
	c.deliverDataLocked(1, p)
	if c.reorderBytes != 0 || len(c.recvBuf) != 3000 {
		t.Fatalf("补齐后 reorderBytes=%d recvBuf=%d", c.reorderBytes, len(c.recvBuf))
	}
	if w := c.recvWndLocked(); w != uint32(utpRecvCap-3000) {
		t.Fatalf("补齐后窗口=%d 期望 %d", w, utpRecvCap-3000)
	}
}
