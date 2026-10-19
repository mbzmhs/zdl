package bt

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPipelineWindowAIMD 自适应窗口状态机:加性增至上限,乘性减至下限,
// 拒绝风暴 5s 防抖只减一次。
func TestPipelineWindowAIMD(t *testing.T) {
	pc := &peerConn{win: pipelineWindow}
	// 交付满一个窗口 → +1
	for i := 0; i < pipelineWindow; i++ {
		pc.noteDelivery()
	}
	if pc.win != pipelineWindow+1 {
		t.Fatalf("满窗交付后应 %d,got %d", pipelineWindow+1, pc.win)
	}
	// 到上限封顶
	pc.mu.Lock()
	pc.win = winMax
	pc.winSucc = 0
	pc.mu.Unlock()
	for i := 0; i < winMax*3; i++ {
		pc.noteDelivery()
	}
	if pc.win != winMax {
		t.Fatalf("窗口应封顶 %d,got %d", winMax, pc.win)
	}
	// 乘性减 + 下限
	pc.mu.Lock()
	pc.win = 16
	pc.mu.Unlock()
	pc.noteLoss()
	if pc.win != 8 {
		t.Fatalf("损失后应减半到 8,got %d", pc.win)
	}
	pc.noteLoss() // 5s 防抖:忽略
	if pc.win != 8 {
		t.Fatalf("5s 内重复损失应忽略,got %d", pc.win)
	}
	pc.mu.Lock()
	pc.lastLoss = time.Now().Add(-6 * time.Second)
	pc.mu.Unlock()
	pc.noteLoss()
	if pc.win != 4 {
		t.Fatalf("再减半应到 4,got %d", pc.win)
	}
	pc.mu.Lock()
	pc.lastLoss = time.Now().Add(-6 * time.Second)
	pc.mu.Unlock()
	pc.noteLoss()
	if pc.win != winMin {
		t.Fatalf("窗口不应低于下限 %d,got %d", winMin, pc.win)
	}
}

// TestPipelineWindowGrowsOnServe 线上验证:对端每 500ms 批量延迟应答,
// 请求窗口随交付加性增长(第 9 批明显大于首批);整批 reject 后减半收缩;
// 全程不超过 winMax。
func TestPipelineWindowGrowsOnServe(t *testing.T) {
	// 2MB 单文件种子:128 个块,远超 winMax,窗口才有意义(buildTestTree 只有 5 块)
	root := t.TempDir()
	blob := make([]byte, 2<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewClient(meta, t.TempDir(), ClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp4", "127.0.0.1:"+portStr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid [20]byte
	pid[0], pid[1] = '-', 'W'
	if err := WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: pid}, false, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandshake(conn); err != nil {
		t.Fatal(err)
	}

	type req struct{ idx, begin, length uint32 }
	var (
		mu      sync.Mutex
		queue   []req
		seenInt bool
	)
	// 读协程:登记 interested 与 request(其余忽略)
	go func() {
		for {
			m, err := ReadMsg(conn)
			if err != nil {
				return
			}
			switch m.ID {
			case msgInterested:
				mu.Lock()
				seenInt = true
				mu.Unlock()
			case msgRequest:
				idx, begin, length, err := ParseRequest(m.Payload)
				if err != nil {
					continue
				}
				mu.Lock()
				queue = append(queue, req{idx, begin, length})
				mu.Unlock()
			}
		}
	}()
	// 等 srv interested → 发 haveall + unchoke(否则 fill 因 choke/bitfield 空转)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := seenInt
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("5s 未收到 interested")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := WriteMsg(conn, msgHaveAll, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteMsg(conn, msgUnchoke, nil); err != nil {
		t.Fatal(err)
	}

	drain := func() []req {
		mu.Lock()
		b := queue
		queue = nil
		mu.Unlock()
		return b
	}
	sendPiece := func(r req) error {
		p := make([]byte, 8, 8+int(r.length))
		binary.BigEndian.PutUint32(p[0:4], r.idx)
		binary.BigEndian.PutUint32(p[4:8], r.begin)
		p = append(p, make([]byte, int(r.length))...)
		return WriteMsg(conn, msgPiece, p)
	}

	var serveBatches []int
	var rejectBatch, afterReject int
	stage := 0 // 0=增长观察 1=整批拒绝 2=收缩验证
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(30 * time.Second)
loop:
	for {
		select {
		case <-tick.C:
			b := drain()
			if len(b) == 0 {
				continue
			}
			switch stage {
			case 0:
				serveBatches = append(serveBatches, len(b))
				for _, r := range b {
					if err := sendPiece(r); err != nil {
						t.Errorf("回 piece: %v", err)
						break loop
					}
				}
				if len(serveBatches) >= 9 {
					stage = 1
				}
			case 1:
				for _, r := range b {
					if err := WriteMsg(conn, msgRejectReq, BuildRequest(r.idx, r.begin, r.length)); err != nil {
						t.Errorf("回 reject: %v", err)
						break loop
					}
				}
				rejectBatch = len(b)
				stage = 2
			case 2:
				afterReject = len(b)
				for _, r := range b {
					if err := sendPiece(r); err != nil {
						t.Errorf("回 piece: %v", err)
						break loop
					}
				}
				break loop
			}
		case <-timeout:
			t.Errorf("超时,批记录=%v reject=%d after=%d", serveBatches, rejectBatch, afterReject)
			break loop
		}
	}

	if len(serveBatches) < 9 {
		t.Fatalf("增长观察批数不足: %v", serveBatches)
	}
	first, last := serveBatches[0], serveBatches[len(serveBatches)-1]
	if last < first+5 {
		t.Errorf("窗口未增长: 首批 %d → 第 %d 批 %d,全记录 %v", first, len(serveBatches), last, serveBatches)
	}
	maxB := 0
	for _, n := range serveBatches {
		if n > maxB {
			maxB = n
		}
	}
	if maxB > winMax {
		t.Errorf("窗口超上限: 单批 %d > %d", maxB, winMax)
	}
	if rejectBatch == 0 || afterReject == 0 {
		t.Fatalf("拒绝/收缩批缺失: reject=%d after=%d", rejectBatch, afterReject)
	}
	if afterReject*4 >= rejectBatch*3 {
		t.Errorf("整批拒绝后未收缩: %d → %d", rejectBatch, afterReject)
	}
}
