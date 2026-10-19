package bt

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"zdownloader/internal/proxy"
)

// TestManualProxyAnnounce 手动端到端:用真实代理 + 真实 tracker 验证 announce。
// 默认跳过;运行方式:
//
//	ZD_MANUAL_PROXY_E2E=1 ZD_MANUAL_PROXY_URL="socks5://127.0.0.1:10810" \
//	ZD_MANUAL_TRACKER="http://1337.abcvg.info:80/announce" \
//	ZD_MANUAL_IH=ee764093450ae6581ffcce43617b158f30dd9238 ./testbin/bt.test.exe -test.run Manual
func TestManualProxyAnnounce(t *testing.T) {
	if os.Getenv("ZD_MANUAL_PROXY_E2E") == "" {
		t.Skip("手动端到端测试:需设置 ZD_MANUAL_PROXY_E2E=1")
	}
	tr := proxy.Transport(proxy.Options{Manual: os.Getenv("ZD_MANUAL_PROXY_URL")})
	hc := &http.Client{Timeout: 20 * time.Second, Transport: tr}
	var ih [20]byte
	raw := os.Getenv("ZD_MANUAL_IH")
	if len(raw) != 40 {
		t.Fatal("ZD_MANUAL_IH 需为 40 位 hex")
	}
	for i := 0; i < 20; i++ {
		ih[i] = byte(hexVal(raw[2*i])<<4 | hexVal(raw[2*i+1]))
	}
	req := AnnounceReq{InfoHash: ih, PeerID: PeerID(), Port: 6881, Event: EventStarted, NumWant: 50}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := HTTPAnnounce(ctx, hc, os.Getenv("ZD_MANUAL_TRACKER"), req, "")
	if err != nil {
		t.Fatalf("经代理 announce 失败: %v", err)
	}
	t.Logf("announce 成功: complete=%d incomplete=%d peers=%d", resp.Complete, resp.Incomplete, len(resp.Peers))
	for _, p := range resp.Peers {
		t.Logf("  peer %s", p.String())
	}
}

// TestManualDHTBootstrap 手动验证 DHT 直连引导(路由表增长与 peer 发现)。
// ZD_MANUAL_DHT=1 ZD_MANUAL_IH=<40位hex> ./testbin/bt.test.exe -test.run TestManualDHTBootstrap
func TestManualDHTBootstrap(t *testing.T) {
	if os.Getenv("ZD_MANUAL_DHT") == "" {
		t.Skip("手动 DHT 探针:需设置 ZD_MANUAL_DHT=1")
	}
	srv, err := StartDHT(DHTConfig{Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(90 * time.Second)
	t.Logf("引导 90s 后路由表节点数: %d", srv.NodeCount())
	var ih [20]byte
	raw := os.Getenv("ZD_MANUAL_IH")
	for i := 0; i < 20 && i*2+1 < len(raw); i++ {
		ih[i] = byte(hexVal(raw[2*i])<<4 | hexVal(raw[2*i+1]))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n := 0
	srv.Lookup(ctx, ih, -1, func(p PeerAddr) {
		n++
		if n <= 10 {
			t.Logf("DHT peer: %s", p.String())
		}
	})
	t.Logf("60s Lookup 发现 peer: %d", n)
}
