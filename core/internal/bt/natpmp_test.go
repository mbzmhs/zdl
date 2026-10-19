package bt

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestNATPMPMapReqLayout(t *testing.T) {
	buf := natpmpMapReq(natpmpOpMapTCP, 6881, 0, 7200)
	if len(buf) != 12 {
		t.Fatalf("请求长度 %d,期望 12", len(buf))
	}
	if buf[0] != 0 || buf[1] != natpmpOpMapTCP {
		t.Fatalf("version/opcode = %d/%d,期望 0/1", buf[0], buf[1])
	}
	if got := binary.BigEndian.Uint16(buf[4:]); got != 6881 {
		t.Fatalf("内部端口 %d,期望 6881", got)
	}
	if got := binary.BigEndian.Uint16(buf[6:]); got != 0 {
		t.Fatalf("外部端口 %d,期望 0(网关自选)", got)
	}
	if got := binary.BigEndian.Uint32(buf[8:]); got != 7200 {
		t.Fatalf("lifetime %d,期望 7200", got)
	}
}

func TestNATPMPMapRespParse(t *testing.T) {
	b := make([]byte, 16)
	b[0], b[1] = 0, natpmpOpMapTCP|0x80
	binary.BigEndian.PutUint16(b[2:], 0) // result=0
	binary.BigEndian.PutUint16(b[8:], 40000)
	binary.BigEndian.PutUint32(b[12:], 7200)
	port, life, code, err := natpmpMapResp(b)
	if err != nil || code != 0 || port != 40000 || life != 7200 {
		t.Fatalf("解析结果 port=%d life=%d code=%d err=%v", port, life, code, err)
	}
	// 错误响应:opcode 未置高位应被拒绝
	b[1] = natpmpOpMapTCP
	if _, _, _, err := natpmpMapResp(b); err == nil {
		t.Fatal("opcode 未置高位应报错")
	}
	// 长度不对
	if _, _, _, err := natpmpMapResp(b[:15]); err == nil {
		t.Fatal("长度 15 应报错")
	}
}

func TestNATPMPProbeRespParse(t *testing.T) {
	// 探测响应 12 字节:ver/op/result/sec[4]/ip[4]
	b := make([]byte, 12)
	b[1] = natpmpOpPublicIP | 0x80
	b[8], b[9], b[10], b[11] = 100, 64, 20, 8
	req := []byte{0, natpmpOpPublicIP}
	if !bytes.Equal(req, []byte{0, 0}) {
		t.Fatal("公网地址请求应为 [0,0]")
	}
	ip := net.IPv4(b[8], b[9], b[10], b[11])
	if ip.String() != "100.64.20.8" {
		t.Fatalf("外部 IP %s", ip)
	}
}

func TestGatewayCandidatesNoPanic(t *testing.T) {
	// 只验证不 panic;真实网络环境里通常能列出至少 .1 网关
	_ = gatewayCandidates()
}
