package bt

// bt.82 回归:stopped 通告不得阻塞 Stop 返回。旧实现同步等待全部 tracker
// 的 stopped 响应(5s 整体预算),列表里有不可达 tracker 时预算必被拖满:
// 改选文件接口同步轮询旧会话退出,实测 5028ms;做种任务的取消/删除接口
// 也在 handler 里同步调 Stop,同样被拖住。

import (
	"net"
	"testing"
	"time"
)

func TestStopNotBlockedByHangingTracker(t *testing.T) {
	// 黑洞 tracker:接受连接但不响应,通告必然挂满整个预算。比不可路由
	// IP 稳(后者可能因"网络不可达"立即报错,测不出阻塞)。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { time.Sleep(60 * time.Second); c.Close() }(conn)
		}
	}()

	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl.AddTracker("http://" + ln.Addr().String() + "/announce")
	t0 := time.Now()
	cl.Start()
	t.Logf("Start: %v", time.Since(t0))
	time.Sleep(200 * time.Millisecond) // start 通告进入在途

	start := time.Now()
	cl.Stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop 被 stopped 通告阻塞 %v(应后台发送、立即返回)", d)
	}
	t.Logf("Stop: %v", time.Since(start))
	start = time.Now()
	cl.Stop() // 幂等:重复调用不得再阻塞,也不得重复派发通告
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("重复 Stop 阻塞 %v", d)
	}
	t.Logf("Stop#2: %v", time.Since(start))
	start = time.Now()
	cl.Close()
	t.Logf("Close: %v", time.Since(start))
}
