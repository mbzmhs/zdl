package bt

import (
	"sync"
	"bytes"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildHubMeta 构造指定名字、内容随机的单文件种子(两个调用必然得到不同
// infohash,供多会话共享端口的路由测试用)。
func buildHubMeta(t *testing.T, name string) *MetaInfo {
	t.Helper()
	dir := t.TempDir()
	data := make([]byte, 64<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(dir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

// startHubPair 在一个 Hub 上挂两个不同 infohash 的会话,返回 hub 与会话。
func startHubPair(t *testing.T) (*Hub, *Client, *Client) {
	t.Helper()
	hub, err := StartHub(30000+int(time.Now().UnixNano()%20000), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Stop)
	var cs []*Client
	for i, meta := range []*MetaInfo{buildHubMeta(t, "hub-a.bin"), buildHubMeta(t, "hub-b.bin")} {
		c, err := NewClient(meta, t.TempDir(), ClientConfig{Hub: hub, MaxPeers: 8}, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Start()
		t.Cleanup(c.Close)
		if c.ListenPort() != hub.Port() {
			t.Fatalf("会话 %d 端口 %d != hub 端口 %d", i, c.ListenPort(), hub.Port())
		}
		cs = append(cs, c)
	}
	return hub, cs[0], cs[1]
}

// hubDialPlain 以明文 BT 握手连入 hub,返回服务端回写的握手(未匹配会话
// 时连接被静默关闭,读侧表现为错误)。
func hubDialPlain(t *testing.T, addr string, ih [20]byte, pid [20]byte) *Handshake {
	t.Helper()
	hs, conn := hubDialPlainKeep(t, addr, ih, pid)
	if conn != nil {
		conn.Close()
	}
	return hs
}

// hubDialPlainKeep 同 hubDialPlain,但不关闭连接:供需要保持 peer 在线
// (观察会话的 peers 表)的测试用,由调用方负责 Close。
func hubDialPlainKeep(t *testing.T, addr string, ih [20]byte, pid [20]byte) (*Handshake, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := WriteHandshake(conn, Handshake{InfoHash: ih, PeerID: pid}, true, true, true); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	hs, err := ReadHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, nil
	}
	return hs, conn
}

func TestHubRoutesPlaintextByInfoHash(t *testing.T) {
	hub, c1, c2 := startHubPair(t)
	addr := fmt.Sprintf("127.0.0.1:%d", hub.Port())
	var p1, p2, px [20]byte
	rand.Read(p1[:])
	rand.Read(p2[:])
	rand.Read(px[:])

	if hs := hubDialPlain(t, addr, c1.meta.InfoHash, p1); hs == nil {
		t.Fatal("会话1的明文入站未得到握手回应")
	} else if hs.InfoHash != c1.meta.InfoHash {
		t.Fatalf("回写握手 infohash 不符: %x", hs.InfoHash)
	}
	if hs := hubDialPlain(t, addr, c2.meta.InfoHash, p2); hs == nil {
		t.Fatal("会话2的明文入站未得到握手回应")
	} else if hs.InfoHash != c2.meta.InfoHash {
		t.Fatalf("回写握手 infohash 不符: %x", hs.InfoHash)
	}
	// 未知 infohash:静默断(读侧超时/EOF,不得回写)
	if hs := hubDialPlain(t, addr, px, px); hs != nil {
		t.Fatal("未知 infohash 不应得到握手回应")
	}
}

func TestHubRoutesMSEBySkey(t *testing.T) {
	hub, c1, c2 := startHubPair(t)
	addr := fmt.Sprintf("127.0.0.1:%d", hub.Port())
	dial := func(ih [20]byte) (net.Conn, error) {
		conn, err := net.DialTimeout("tcp4", addr, 3*time.Second)
		if err != nil {
			return nil, err
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		return conn, nil
	}
	for _, c := range []*Client{c1, c2} {
		conn, err := dial(c.meta.InfoHash)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := mseInitiate(conn, c.meta.InfoHash)
		if err != nil {
			conn.Close()
			t.Fatalf("skey %x 的 MSE 协商失败: %v", c.meta.InfoHash, err)
		}
		var pid [20]byte
		rand.Read(pid[:])
		if err := WriteHandshake(wire, Handshake{InfoHash: c.meta.InfoHash, PeerID: pid}, true, true, true); err != nil {
			t.Fatal(err)
		}
		hs, err := ReadHandshake(wire)
		wire.Close()
		if err != nil {
			t.Fatalf("MSE 入站未得到握手回应: %v", err)
		}
		if hs.InfoHash != c.meta.InfoHash {
			t.Fatalf("回写握手 infohash 不符: %x", hs.InfoHash)
		}
	}
	// 未注册 skey:步3 匹配失败,协商中断
	var unknown [20]byte
	rand.Read(unknown[:])
	conn, err := dial(unknown)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := mseInitiate(conn, unknown); err == nil {
		t.Fatal("未注册 skey 的 MSE 协商不应成功")
	}
}

func TestHubRoutesUTPByInfoHash(t *testing.T) {
	hub, c1, _ := startHubPair(t)
	seeder, err := specDial(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: hub.Port()})
	if err != nil {
		t.Fatalf("uTP 建连失败: %v", err)
	}
	defer seeder.close()
	deadline := time.Now().Add(5 * time.Second)
	_ = seeder.SetDeadline(deadline)
	var pid [20]byte
	rand.Read(pid[:])
	if err := WriteHandshake(seeder, Handshake{InfoHash: c1.meta.InfoHash, PeerID: pid}, true, true, true); err != nil {
		t.Fatal(err)
	}
	hs, err := ReadHandshake(seeder)
	if err != nil {
		t.Fatalf("uTP 入站未得到握手回应: %v", err)
	}
	if !bytes.Equal(hs.InfoHash[:], c1.meta.InfoHash[:]) {
		t.Fatalf("回写握手 infohash 不符: %x", hs.InfoHash)
	}
}

func TestHubStopClosesListener(t *testing.T) {
	hub, err := StartHub(30000+int(time.Now().UnixNano()%20000), nil)
	if err != nil {
		t.Fatal(err)
	}
	port := hub.Port()
	hub.Stop()
	// 关停后新入站必须失败(端口已释放或连接被立即关闭)
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err == nil {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, rerr := conn.Read(make([]byte, 1)); rerr == nil {
			conn.Close()
			t.Fatal("Stop 后入站仍可完成读取,监听未关闭")
		}
		conn.Close()
	}
}

// TestHubSameInfohashHandoff 同一种子开两个任务(同 infohash):入站落在
// 其中之一,把接到连接的那个会话关掉,幸存会话必须立即接手路由——旧实现
// 按 infohash 单槽登记,先注册会话退出后同种子其余会话的入站全断。
func TestHubSameInfohashHandoff(t *testing.T) {
	hub, err := StartHub(30000+int(time.Now().UnixNano()%20000), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	meta := buildHubMeta(t, "same.bin") // 两个会话共用同一份种子
	var cs []*Client
	for i := 0; i < 2; i++ {
		c, err := NewClient(meta, t.TempDir(), ClientConfig{Hub: hub, MaxPeers: 8}, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Start()
		defer c.Close()
		cs = append(cs, c)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", hub.Port())
	var pid [20]byte
	rand.Read(pid[:])

	// 第一次入站:必然被其中之一接住(任意会话可服务)。连接保持打开,
	// 否则 pc.run 读到 EOF 立即摘除 peer,轮询会扑空
	hs, conn := hubDialPlainKeep(t, addr, meta.InfoHash, pid)
	if hs == nil {
		t.Fatal("同 infohash 双会话:入站未得到握手回应")
	}
	var holder *Client
	deadline := time.Now().Add(3 * time.Second)
	for holder == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		for _, c := range cs {
			c.pMu.Lock()
			n := len(c.peers)
			c.pMu.Unlock()
			if n > 0 {
				holder = c
				break
			}
		}
	}
	conn.Close()
	if holder == nil {
		t.Fatal("未找到接住入站的会话")
	}

	// 复现用户场景:关掉接到连接的那个会话
	holder.Stop()

	// 幸存会话必须接手:再次入站必须成功(旧实现在此拿到 nil/被静默断)
	deadline = time.Now().Add(3 * time.Second)
	for {
		if hs := hubDialPlain(t, addr, meta.InfoHash, pid); hs != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("关闭持有会话后,幸存会话未接手入站路由")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestHubStopRacesAcceptDispatch 复现 bt.115 崩溃:双暂停令 hub.Stop 的
// wg.Wait 与 accept 循环 Accept 返回后的 wg.Add 并发 → WaitGroup misuse
// panic。压力循环 + -race 下应稳定暴露(修复后必须全绿)。
func TestHubStopRacesAcceptDispatch(t *testing.T) {
	for iter := 0; iter < 300; iter++ {
		hub, err := StartHub(20000+iter*100+int(time.Now().UnixNano()%50), nil)
		if err != nil {
			t.Fatal(err)
		}
		meta := buildHubMeta(t, "race.bin")
		c, err := NewClient(meta, t.TempDir(), ClientConfig{Hub: hub, MaxPeers: 8}, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Start()
		addr := fmt.Sprintf("127.0.0.1:%d", hub.Port())
		stop := make(chan struct{})
		var dials sync.WaitGroup
		for w := 0; w < 12; w++ {
			dials.Add(1)
			go func() {
				defer dials.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					conn, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
					if err != nil {
						continue
					}
					conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
					var pid [20]byte
					rand.Read(pid[:])
					WriteHandshake(conn, Handshake{InfoHash: meta.InfoHash, PeerID: pid}, true, true, true)
					conn.Read(make([]byte, 68))
					conn.Close()
				}
			}()
		}
		time.Sleep(15 * time.Millisecond)
		c.Stop()
		hub.Stop() // 若 Add 与 Wait 并发,此处 panic
		c.Close()  // 释放存储句柄,防 Windows TempDir 清理撞文件占用
		close(stop)
		dials.Wait()
	}
}
