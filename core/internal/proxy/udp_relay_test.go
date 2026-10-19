package proxy

import (
	"encoding/binary"
	"net"
	"net/url"
	"testing"
	"time"
)

// TestSOCKS5UDPRelay 用迷你 SOCKS5 服务器(支持 UDP ASSOCIATE)验证中继收发。
func TestSOCKS5UDPRelay(t *testing.T) {
	// UDP 目标服务器:收到 "ping" 回 "pong"
	updLn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer updLn.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := updLn.ReadFrom(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "ping" {
				updLn.WriteTo([]byte("pong"), addr)
			}
		}
	}()
	target := updLn.LocalAddr().(*net.UDPAddr)

	// 代理控制通道
	ctrl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	// 代理的中继 UDP socket
	relayUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relayUDP.Close()
	go func() {
		conn, err := ctrl.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// 方法协商
		head := make([]byte, 2)
		if _, err := readFullTest(conn, head); err != nil {
			return
		}
		m := make([]byte, head[1])
		readFullTest(conn, m)
		conn.Write([]byte{0x05, 0x00})
		// 请求:ATYP ADDR PORT
		ver := make([]byte, 4)
		readFullTest(conn, ver)
		var alen int
		switch ver[3] {
		case 0x01:
			alen = 4
		case 0x03:
			b := make([]byte, 1)
			readFullTest(conn, b)
			alen = int(b[0])
		case 0x04:
			alen = 16
		}
		rest := make([]byte, alen+2)
		readFullTest(conn, rest)
		// 应答 BND = 代理中继 socket 地址
		ra := relayUDP.LocalAddr().(*net.UDPAddr)
		resp := []byte{0x05, 0x00, 0x00, 0x01, ra.IP[0], ra.IP[1], ra.IP[2], ra.IP[3], 0, 0}
		binary.BigEndian.PutUint16(resp[8:10], uint16(ra.Port))
		conn.Write(resp)
		// 中继循环:客户端数据剥头转发;目标回包加头发回客户端
		var clientAddr net.Addr
		buf := make([]byte, 2048)
		for {
			n, from, err := relayUDP.ReadFrom(buf)
			if err != nil {
				return
			}
			if ta, ok := from.(*net.UDPAddr); ok && ta.Port == target.Port {
				// 目标服务器的回包:加头后发回客户端
				if clientAddr == nil {
					continue
				}
				hdr := []byte{0, 0, 0, 0x01, target.IP[0], target.IP[1], target.IP[2], target.IP[3], 0, 0}
				binary.BigEndian.PutUint16(hdr[8:10], uint16(target.Port))
				relayUDP.WriteTo(append(hdr, buf[:n]...), clientAddr)
				continue
			}
			if n < 10 {
				continue
			}
			atyp := buf[3]
			if atyp != 0x01 {
				continue
			}
			port := binary.BigEndian.Uint16(buf[8:10])
			taddr := &net.UDPAddr{IP: net.IP(buf[4:8]), Port: int(port)}
			clientAddr = from
			relayUDP.WriteTo(buf[10:n], taddr)
		}
	}()

	u, _ := url.Parse("socks5://" + ctrl.Addr().String())
	pc, err := SOCKS5UDPRelay(u)
	if err != nil {
		t.Fatalf("建立中继失败: %v", err)
	}
	defer pc.Close()

	if _, err := pc.WriteTo([]byte("ping"), target); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	buf := make([]byte, 256)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, addr, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(buf[:n]) != "pong" {
		t.Fatalf("回包不符: %q", buf[:n])
	}
	if ua, ok := addr.(*net.UDPAddr); !ok || !ua.IP.Equal(target.IP) || ua.Port != target.Port {
		t.Fatalf("回包地址不符: %v", addr)
	}
}

func readFullTest(conn net.Conn, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := conn.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
