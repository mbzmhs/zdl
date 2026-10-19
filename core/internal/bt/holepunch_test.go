package bt

import "testing"

// 扩展握手必须声明 ut_holepunch,否则对端(如 BitComet)界面不会标出 UDP
// 打孔能力。此测试锁定该通告不被打字错误改掉。
func TestExtHandshakeAdvertisesHolepunch(t *testing.T) {
	raw := BuildExtHandshake(1234, 40985, "BitComet 1.99")
	e, err := ParseExtHandshake(raw)
	if err != nil {
		t.Fatalf("解析扩展握手: %v", err)
	}
	if got := e.PeerExtID("ut_holepunch"); got != LocalHolepunchID {
		t.Fatalf("ut_holepunch 扩展 ID=%d,期望 %d", got, LocalHolepunchID)
	}
	if !e.SupportsHolepunch() {
		t.Fatal("SupportsHolepunch 为 false,通告丢失")
	}
	if !e.SupportsMetadata() || !e.SupportsPEX() {
		t.Fatal("原有扩展声明被破坏")
	}
}

func TestParseHolepunchRendezvous(t *testing.T) {
	payload := MustEncode(map[string]any{"msg_type": int64(hpRendezvous), "ip": "1.2.3.4", "port": int64(6881)})
	m, err := ParseHolepunch(payload)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if m.MsgType != hpRendezvous || m.IP != "1.2.3.4" || m.Port != 6881 {
		t.Fatalf("解析结果错误: %+v", m)
	}
	if _, err := ParseHolepunch([]byte("not-a-dict")); err == nil {
		t.Fatal("非 dict 载荷应报错")
	}
}
