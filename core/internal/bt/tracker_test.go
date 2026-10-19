package bt

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testIH = [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

func testReq() AnnounceReq {
	return AnnounceReq{
		InfoHash: testIH, PeerID: [20]byte{'-', 'Z', 'D', '1', '0', '0', '0', '-'},
		Port: 6881, Left: 1000, Event: EventStarted, Key: 42,
	}
}

func TestHTTPAnnounceCompact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("info_hash") != string(testIH[:]) {
			t.Errorf("info_hash 不符: %x", q.Get("info_hash"))
		}
		if q.Get("event") != "started" || q.Get("port") != "6881" || q.Get("compact") != "1" {
			t.Errorf("参数不符: %v", q)
		}
		if q.Get("left") != "1000" {
			t.Errorf("left 不符")
		}
		// compact:1.2.3.4:1234 与 5.6.7.8:5678
		w.Write([]byte("d8:completei7e10:incompletei3e8:intervali1800e5:peers12:" +
			"\x01\x02\x03\x04\x04\xd2\x05\x06\x07\x08\x16\x2ee"))
	}))
	defer srv.Close()

	resp, err := HTTPAnnounce(context.Background(), srv.Client(), srv.URL, testReq(), "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 1800 || resp.Complete != 7 || resp.Incomplete != 3 {
		t.Fatalf("resp=%+v", resp)
	}
	if len(resp.Peers) != 2 || resp.Peers[0].String() != "1.2.3.4:1234" || resp.Peers[1].String() != "5.6.7.8:5678" {
		t.Fatalf("peers=%v", resp.Peers)
	}
}

func TestHTTPAnnounceDictPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d8:intervali900e5:peersld2:ip9:127.0.0.14:porti7777eeee"))
	}))
	defer srv.Close()
	resp, err := HTTPAnnounce(context.Background(), srv.Client(), srv.URL, testReq(), "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 900 || len(resp.Peers) != 1 || resp.Peers[0].String() != "127.0.0.1:7777" {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestHTTPAnnounceFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d14:failure reason12:no such infoe"))
	}))
	defer srv.Close()
	_, err := HTTPAnnounce(context.Background(), srv.Client(), srv.URL, testReq(), "")
	if err == nil || !strings.Contains(err.Error(), "no such info") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPAnnounceBadScheme(t *testing.T) {
	if _, err := HTTPAnnounce(context.Background(), http.DefaultClient, "ftp://x/a", testReq(), ""); err == nil {
		t.Fatal("应拒绝非 http tracker")
	}
}

// fakeUDPTracker 是最小 BEP-15 服务器:connect + announce。
func fakeUDPTracker(t *testing.T) (addr string, announceCh chan AnnounceReq) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	announceCh = make(chan AnnounceReq, 4)
	var connID int64 = 0x1122334455667788
	go func() {
		buf := make([]byte, 4096)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := buf[:n]
			if len(pkt) == 16 && binary.BigEndian.Uint64(pkt[0:8]) == udpMagic {
				out := make([]byte, 16)
				binary.BigEndian.PutUint32(out[0:4], 0)
				copy(out[4:8], pkt[12:16]) // txid
				binary.BigEndian.PutUint64(out[8:16], uint64(connID))
				conn.WriteToUDP(out, remote)
				continue
			}
			if len(pkt) == 98 {
				var req AnnounceReq
				copy(req.InfoHash[:], pkt[16:36])
				copy(req.PeerID[:], pkt[36:56])
				req.Downloaded = int64(binary.BigEndian.Uint64(pkt[56:64]))
				req.Left = int64(binary.BigEndian.Uint64(pkt[64:72]))
				req.Uploaded = int64(binary.BigEndian.Uint64(pkt[72:80]))
				req.Event = AnnounceEvent(binary.BigEndian.Uint32(pkt[80:84]))
				req.Port = binary.BigEndian.Uint16(pkt[96:98])
				announceCh <- req
				out := make([]byte, 20+12)
				binary.BigEndian.PutUint32(out[0:4], 1)
				copy(out[4:8], pkt[12:16])
				binary.BigEndian.PutUint32(out[8:12], 600)
				binary.BigEndian.PutUint32(out[12:16], 2)
				binary.BigEndian.PutUint32(out[16:20], 9)
				copy(out[20:26], []byte{10, 0, 0, 1, 0x30, 0x39})    // 10.0.0.1:12345
				copy(out[26:32], []byte{192, 168, 0, 9, 0x1F, 0x90}) // 192.168.0.9:8080
				conn.WriteToUDP(out, remote)
			}
		}
	}()
	return conn.LocalAddr().String(), announceCh
}

func TestUDPAnnounce(t *testing.T) {
	addr, ch := fakeUDPTracker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := UDPAnnounce(ctx, "udp://"+addr, testReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 600 || resp.Incomplete != 2 || resp.Complete != 9 {
		t.Fatalf("resp=%+v", resp)
	}
	sortPeers(resp.Peers)
	if len(resp.Peers) != 2 || resp.Peers[0].String() != "10.0.0.1:12345" || resp.Peers[1].String() != "192.168.0.9:8080" {
		t.Fatalf("peers=%v", resp.Peers)
	}
	select {
	case got := <-ch:
		if got.InfoHash != testIH || got.Event != EventStarted || got.Port != 6881 || got.Left != 1000 {
			t.Fatalf("tracker 收到的参数不符: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tracker 未收到 announce")
	}
}

func TestUDPAnnounceError(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			_, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			out := make([]byte, 8+20)
			binary.BigEndian.PutUint32(out[0:4], 3)
			copy(out[4:8], buf[12:16])
			copy(out[8:], "unregistered torrent")
			conn.WriteToUDP(out[:8+20], remote)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = UDPAnnounce(ctx, fmt.Sprintf("udp://%s", conn.LocalAddr()), testReq())
	if err == nil || !strings.Contains(err.Error(), "unregistered torrent") {
		t.Fatalf("err=%v", err)
	}
}

func TestUDPAnnounceBadURL(t *testing.T) {
	if _, err := UDPAnnounce(context.Background(), "udp://127.0.0.1", testReq()); err == nil {
		t.Fatal("缺端口应报错")
	}
	if _, err := UDPAnnounce(context.Background(), "http://127.0.0.1:6969/a", testReq()); err == nil {
		t.Fatal("应拒绝非 udp tracker")
	}
}
