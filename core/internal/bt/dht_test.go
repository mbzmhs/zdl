package bt

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startTestDHT 起一个绑定随机端口的测试 DHT 节点。
func startTestDHT(t *testing.T) *DHTServer {
	t.Helper()
	s, err := StartDHT(DHTConfig{})
	if err != nil {
		t.Fatalf("StartDHT: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// TestDHTKRPCRaw 用原始 UDP 报文验证 KRPC 应答形态:
// ping/get_peers/find_node/announce_peer/坏令牌/未知方法。
func TestDHTKRPCRaw(t *testing.T) {
	s := startTestDHT(t)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.Port()}

	cli, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	rpc := func(msg dict) dict {
		t.Helper()
		cli.WriteToUDP(MustEncode(msg), addr)
		buf := make([]byte, 8192)
		cli.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err := cli.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("KRPC 无应答: %v", err)
		}
		v, err := Decode(buf[:n])
		if err != nil {
			t.Fatalf("应答解码: %v", err)
		}
		d, ok := v.(dict)
		if !ok {
			t.Fatalf("应答不是 dict: %T", v)
		}
		return d
	}

	// ping
	d := rpc(dict{"t": "1", "y": "q", "q": "ping", "a": dict{"id": idStr(make20(1))}})
	if d["y"] != "r" {
		t.Fatalf("ping 应答 y=%v", d["y"])
	}
	r, _ := d["r"].(dict)
	if r == nil || len(r["id"].(string)) != 20 {
		t.Fatalf("ping 应答缺少 20 字节节点 id: %v", r)
	}

	// get_peers:返回 token + nodes,无 values
	d = rpc(dict{"t": "2", "y": "q", "q": "get_peers", "a": dict{
		"id": idStr(make20(2)), "info_hash": idStr(make20(0xAB)),
	}})
	r, _ = d["r"].(dict)
	if r == nil {
		t.Fatal("get_peers 应答缺少 r")
	}
	tok, _ := r["token"].(string)
	if tok == "" {
		t.Fatal("get_peers 应答缺少 token")
	}
	if _, has := r["values"]; has {
		t.Fatal("未宣告时不应有 values")
	}

	// announce_peer:正确令牌
	d = rpc(dict{"t": "3", "y": "q", "q": "announce_peer", "a": dict{
		"id": idStr(make20(2)), "info_hash": idStr(make20(0xAB)),
		"port": int64(34567), "token": tok,
	}})
	if d["y"] != "r" {
		t.Fatalf("announce_peer 应答 y=%v", d["y"])
	}

	// 再次 get_peers:values 应包含宣告的 peer
	d = rpc(dict{"t": "4", "y": "q", "q": "get_peers", "a": dict{
		"id": idStr(make20(3)), "info_hash": idStr(make20(0xAB)),
	}})
	r, _ = d["r"].(dict)
	vals, _ := r["values"].([]any)
	if len(vals) != 1 {
		t.Fatalf("宣告后 values 应有 1 项, got %v", r)
	}
	p := vals[0].(string)
	if len(p) != 6 {
		t.Fatalf("compact peer 应 6 字节, got %d", len(p))
	}
	port := uint16(p[4])<<8 | uint16(p[5])
	if port != 34567 {
		t.Fatalf("宣告端口不符: %d", port)
	}

	// 坏令牌宣告被拒(错误应答 y=e)
	d = rpc(dict{"t": "5", "y": "q", "q": "announce_peer", "a": dict{
		"id": idStr(make20(4)), "info_hash": idStr(make20(0xAB)),
		"port": int64(1), "token": "bogus",
	}})
	if d["y"] != "e" {
		t.Fatalf("坏令牌应返回错误, got y=%v", d["y"])
	}

	// 未知方法 → 204
	d = rpc(dict{"t": "6", "y": "q", "q": "no_such", "a": dict{"id": idStr(make20(5))}})
	if d["y"] != "e" {
		t.Fatalf("未知方法应返回错误, got y=%v", d["y"])
	}
}

// TestDHTLookupViaBootstrap 三节点链:路由节点 B,宣告方 S,查找方 L。
// S 经 B 宣告,L 的迭代查找应发现 S 的 peer 地址。
func TestDHTLookupViaBootstrap(t *testing.T) {
	b := startTestDHT(t) // 路由节点
	s := startTestDHT(t) // 宣告方
	l := startTestDHT(t) // 查找方

	ih := make20(0xCD)

	// 让 S、L 认识 B:直接插入路由表(生产路径为 bootstrapLoop)
	bAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: b.Port()}
	s.rt.insert(randomID(), bAddr)
	l.rt.insert(randomID(), bAddr)

	// S 宣告(端口随便指一个监听中的 TCP)
	tcpLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()
	announcePort := tcpLn.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n := s.Lookup(ctx, ih, announcePort, nil)
	_ = n

	// L 查找,应收到 S 的宣告地址
	var mu sync.Mutex
	var found []PeerAddr
	m := l.Lookup(ctx, ih, -1, func(p PeerAddr) {
		mu.Lock()
		found = append(found, p)
		mu.Unlock()
	})
	if m == 0 || len(found) == 0 {
		t.Fatalf("L 未发现 S 的 peer(found=%d)", m)
	}
	ok := false
	for _, p := range found {
		if p.Port == uint16(announcePort) {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("发现列表缺少宣告端口 %d: %v", announcePort, found)
	}
}

// TestDHTClientAnnounceLoop 验证 Client.dhtLoop:种子会话启动后即宣告,
// 宣告应落到路由节点(R)的记录里,供 get_peers 回应。
func TestDHTClientAnnounceLoop(t *testing.T) {
	s := startTestDHT(t) // 本机客户端共享的 DHT 节点
	r := startTestDHT(t) // 承接宣告的路由节点(生产环境为公网节点)
	s.rt.insert(randomID(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.Port()})

	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := NewSeedClient(meta, srcRoot, ClientConfig{DHT: s})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 轮询等待宣告记录出现(dhtLoop 首轮查找立即执行)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if peers := r.knownPeers(meta.InfoHash); len(peers) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("10s 内未见宣告记录")
}

// randomID 生成随机节点 id(测试用)。
func randomID() [20]byte {
	var id [20]byte
	for i := range id {
		id[i] = byte(i*7 + 3)
	}
	id[0] = 0x7A // 与 self 不同前缀,确保入桶
	return id
}

func make20(seed byte) [20]byte {
	var id [20]byte
	for i := range id {
		id[i] = seed
	}
	return id
}

// idStr 20 字节 id 转字符串(bencode 字节串)。
func idStr(b [20]byte) string { return string(b[:]) }

// TestDHTMarkFailedPurgesDeadNode 连续失败达上限的节点必须移出路由表:
// 否则死节点永远占着桶位,迭代查找每轮先问它们、两轮无收益即弃,
// 路由表再也长不大(卡 1-2 个节点的病灶)。
func TestDHTMarkFailedPurgesDeadNode(t *testing.T) {
	rt := newRoutingTable(make20(7))
	id := make20(10)
	rt.Insert(id, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 6881})
	if rt.Count() != 1 {
		t.Fatalf("插入后节点数应为 1,got %d", rt.Count())
	}
	rt.markFailed(id)
	rt.markFailed(id)
	if rt.Count() != 1 {
		t.Fatalf("未达失败上限(%d)不应移除,got %d", dhtMaxFails, rt.Count())
	}
	rt.markFailed(id)
	if rt.Count() != 0 {
		t.Fatalf("连续失败 %d 次应移出路由表,got %d", dhtMaxFails, rt.Count())
	}
	rt.markFailed(make20(11)) // 未知 id:不应 panic
}

// TestDHTNeedsHeal 健康门:薄表(<dhtThinNodes)或失联(超过 dhtHealQuiet
// 没有出站应答)才触发自愈;表足额且最近有应答时不打扰。
func TestDHTNeedsHeal(t *testing.T) {
	s := &DHTServer{rt: newRoutingTable(make20(7))}
	s.lastGood.Store(time.Now().UnixNano())
	if !s.needsHeal() {
		t.Fatal("空表即使刚有应答也应触发自愈")
	}
	for i := byte(10); i < 10+dhtThinNodes; i++ {
		s.rt.Insert(make20(i), &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000 + int(i)})
	}
	if s.needsHeal() {
		t.Fatal("表足额且刚收到应答,不应自愈")
	}
	s.lastGood.Store(time.Now().Add(-dhtHealQuiet - time.Minute).UnixNano())
	if !s.needsHeal() {
		t.Fatal("超过失联阈值无出站应答,应触发自愈(防整表僵尸)")
	}
	// 薄表:节点不足时即便网络正常也触发
	s.lastGood.Store(time.Now().UnixNano())
	s.rt.markFailed(make20(10))
	s.rt.markFailed(make20(10))
	s.rt.markFailed(make20(10)) // 移除 1 个,剩 3 < dhtThinNodes
	if !s.needsHeal() {
		t.Fatal("薄表应触发自愈")
	}
}

// TestDHTPersistRoundTrip 落盘→回灌:节点/本机 id 恢复一致;
// 查询失败中的节点不落盘;损坏文件忽略不 panic。
func TestDHTPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dht.json")
	self := make20(0x5A)
	s := &DHTServer{cfg: DHTConfig{PersistFile: path}, id: self, rt: newRoutingTable(self)}

	live := make20(10)
	s.rt.Insert(live, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 6881})
	dying := make20(11)
	s.rt.Insert(dying, &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 6882})
	s.rt.markFailed(dying) // 失败中的节点不落盘
	s.saveRouting()

	p := readDHTPersist(path)
	if p == nil {
		t.Fatal("落盘文件不可读")
	}
	if p.Self != hex.EncodeToString(self[:]) {
		t.Fatalf("本机 id 未落盘: %s", p.Self)
	}
	if len(p.Nodes) != 1 {
		t.Fatalf("查询失败节点不应落盘,got %d 个节点", len(p.Nodes))
	}

	// 回灌:新实例恢复节点与地址
	s2 := &DHTServer{cfg: DHTConfig{PersistFile: path}, id: self, rt: newRoutingTable(self)}
	s2.loadRouting()
	if s2.rt.Count() != 1 {
		t.Fatalf("回灌节点数应为 1,got %d", s2.rt.Count())
	}
	got := s2.rt.closest(live, 1)
	if len(got) == 0 || got[0].addr.Port != 6881 || !got[0].addr.IP.Equal(net.IPv4(1, 2, 3, 4)) {
		t.Fatalf("回灌地址错误: %+v", got)
	}

	// 损坏文件:忽略不 panic,不灌入
	if err := os.WriteFile(path, []byte("{不是json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if readDHTPersist(path) != nil {
		t.Fatal("损坏文件应返回 nil")
	}
	s3 := &DHTServer{cfg: DHTConfig{PersistFile: path}, id: self, rt: newRoutingTable(self)}
	s3.loadRouting()
	if s3.rt.Count() != 0 {
		t.Fatalf("损坏文件不应灌入节点,got %d", s3.rt.Count())
	}

	// 文件不存在:静默 nil
	if readDHTPersist(filepath.Join(t.TempDir(), "nope.json")) != nil {
		t.Fatal("缺失文件应返回 nil")
	}
}

// TestDHTRestoresNodeIDFromPersist 重启沿用持久化的本机节点 id(稳定身份)。
func TestDHTRestoresNodeIDFromPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dht.json")
	self := make20(0x77)
	b, err := json.Marshal(dhtPersist{Self: hex.EncodeToString(self[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := StartDHT(DHTConfig{PersistFile: path})
	if err != nil {
		t.Fatalf("StartDHT: %v", err)
	}
	defer s.Stop()
	if s.id != self {
		t.Fatalf("本机 id 未恢复: got %x want %x", s.id, self)
	}
}
