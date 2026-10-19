package bt

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebSeedOnlyDownload:无 peer,仅一个 BEP-19 HTTP 种子源(支持 Range),
// 客户端应经整 piece 拉取完成下载。
func TestWebSeedOnlyDownload(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	// HTTP 静态服务:文件路径 = <name>/<relpath>,ServeContent 自带 Range 支持
	fs := http.FileServer(http.Dir(filepath.Dir(srcRoot)))
	srv := httptest.NewServer(fs)
	defer srv.Close()

	dst := t.TempDir()
	cl, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl.AddWebSeeds([]string{srv.URL})
	cl.Start()
	defer cl.Close()
	waitDone(t, cl)

	for _, f := range meta.Files {
		want, err := os.ReadFile(filepath.Join(srcRoot, filepath.Join(f.Path...)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(cl.FinalPath(), filepath.Join(f.Path...)))
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != string(got) {
			t.Fatalf("文件 %v 内容不符", f.Path)
		}
	}
	if cl.Stats().Downloaded == 0 {
		t.Fatal("应统计到经 webseed 的下载量")
	}
}

// TestUPnPSOAP:假网关验证 SOAP 报文与外部 IP 解析。
func TestUPnPSOAP(t *testing.T) {
	var gotAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 16<<10)
		n, _ := r.Body.Read(body)
		reqBody := string(body[:n])
		switch {
		case strings.Contains(reqBody, "GetExternalIPAddress"):
			fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>203.0.113.7</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
		case strings.Contains(reqBody, "AddPortMapping"), strings.Contains(reqBody, "DeletePortMapping"):
			gotAction = reqBody
			w.WriteHeader(200)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	gw := &UPnP{
		ctrlURL:     srv.URL,
		serviceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
		hc:          &http.Client{Timeout: 5 * time.Second},
		localIP:     "192.168.1.10",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ip, err := gw.getExternalIP(ctx)
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("外部 IP 解析: %q err=%v", ip, err)
	}
	if err := gw.AddPortMapping(ctx, 6881, "tcp", 6881, "ZDownloader <测试>"); err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if !strings.Contains(gotAction, "<NewExternalPort>6881</NewExternalPort>") ||
		!strings.Contains(gotAction, "<NewProtocol>TCP</NewProtocol>") ||
		!strings.Contains(gotAction, "<NewInternalClient>192.168.1.10</NewInternalClient>") ||
		!strings.Contains(gotAction, "ZDownloader &lt;测试&gt;") {
		t.Fatalf("SOAP 报文不符: %s", gotAction)
	}
	if err := gw.DeletePortMapping(ctx, 6881, "TCP"); err != nil {
		t.Fatalf("DeletePortMapping: %v", err)
	}
}

// TestLSDPacketParse:构造 BEP-14 广播报文,验证解析与 peer 分发。
func TestLSDPacketParse(t *testing.T) {
	ih1, ih2 := make20(0x11), make20(0x22)
	msg := fmt.Sprintf("BT-SEARCH * HTTP/1.1\r\nHost: 239.192.152.143:6771\r\nPort: 34567\r\nInfohash: %x\r\nInfohash: %x\r\n\r\n", ih1, ih2)

	got := map[string]int{}
	l := &LSD{port: 11111, onPeer: func(ih [20]byte, addr string) {
		got[addr+"/"+fmt.Sprintf("%x", ih)]++
	}}
	l.handlePacket([]byte(msg), &net.UDPAddr{IP: net.IPv4(192, 168, 1, 55), Port: 6771})
	if len(got) != 2 {
		t.Fatalf("应分发 2 个 (infohash, peer) 对, got %v", got)
	}
	for k := range got {
		if !strings.HasPrefix(k, "192.168.1.55:34567/") {
			t.Fatalf("peer 地址不符: %s", k)
		}
	}
}

// TestFastHandshakeFlags:握手 fast 位往返。
func TestFastHandshakeFlags(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		_ = WriteHandshake(c1, Handshake{InfoHash: testIH, PeerID: [20]byte{'a'}}, true, true, true)
	}()
	hs, err := ReadHandshake(c2)
	if err != nil {
		t.Fatal(err)
	}
	if !hs.SupportsFast() || !hs.SupportsDHT() || !hs.SupportsExt() {
		t.Fatalf("fast/dht/ext 能力位应全置位: %v", hs.Reserved)
	}
}

// TestWebSeedSegments:piece→文件区段映射(多文件/跨文件/单文件)。
func TestWebSeedSegments(t *testing.T) {
	meta, err := ParseTorrent(manualTorrent(t, map[string]any{
		"name":   "种子",
		"pieces": string(bytes.Repeat([]byte{2}, 40)),
		"files": []any{
			map[string]any{"path": []any{"a.bin"}, "length": int64(16384)},
			map[string]any{"path": []any{"sub", "b.txt"}, "length": int64(16384)},
		},
		"piece length": int64(16384),
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{meta: meta}
	// piece 0 完整落在 a.bin
	segs, total, ok := c.pieceFileSegments("http://x/y", 0)
	if !ok || len(segs) != 1 || total != 16384 ||
		!strings.HasSuffix(segs[0].url, "/a.bin") || segs[0].start != 0 || segs[0].end != 16384 {
		t.Fatalf("piece0 区段不符: %+v %d %v", segs, total, ok)
	}
	// piece 1 跨 a.bin 尾部与 b.txt 头部?16384 边界对齐——piece1 恰是 b.txt
	segs, total, ok = c.pieceFileSegments("http://x/y", 1)
	if !ok || len(segs) != 1 || !strings.Contains(segs[0].url, "/sub/b.txt") {
		t.Fatalf("piece1 区段不符: %+v %d %v", segs, total, ok)
	}
	meta2 := &MetaInfo{
		Name: "种子", PieceLength: 128, TotalLength: 300, Multi: true,
		Pieces: bytes.Repeat([]byte{2}, 60),
		Files: []FileInfo{
			{Path: []string{"a"}, Length: 100, Offset: 0},
			{Path: []string{"b"}, Length: 200, Offset: 100},
		},
	}
	c2 := &Client{meta: meta2}
	segs, total, ok = c2.pieceFileSegments("http://x", 0) // [0,128):a 的 [0,100) + b 的 [0,28)
	if !ok || len(segs) != 2 || total != 128 {
		t.Fatalf("跨界 piece 区段不符: %+v %d %v", segs, total, ok)
	}
	if segs[0].start != 0 || segs[0].end != 100 || segs[1].start != 0 || segs[1].end != 28 {
		t.Fatalf("跨界区段偏移不符: %+v %+v", segs[0], segs[1])
	}
	if !strings.HasSuffix(segs[0].url, "/a") || !strings.HasSuffix(segs[1].url, "/b") {
		t.Fatalf("跨界区段 URL 不符: %s %s", segs[0].url, segs[1].url)
	}
}
