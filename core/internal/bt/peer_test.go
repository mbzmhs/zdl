package bt

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	want := Handshake{InfoHash: testIH, PeerID: PeerID()}
	// 对端:读→校验→回一个不带能力位的握手
	errCh := make(chan error, 2)
	go func() {
		if err := WriteHandshake(c1, want, true, true, true); err != nil {
			errCh <- err
			return
		}
		hs, err := ReadHandshake(c1)
		if err != nil {
			errCh <- err
			return
		}
		if hs.InfoHash != want.InfoHash || hs.PeerID == ([20]byte{}) {
			t.Errorf("对端握手解析异常: %+v", hs)
			errCh <- nil
			return
		}
		if !hs.SupportsExt() || !hs.SupportsDHT() {
			t.Error("发送端能力位丢失")
		}
		errCh <- WriteHandshake(c1, Handshake{InfoHash: testIH, PeerID: [20]byte{'x'}}, false, false, false)
	}()
	hs, err := ReadHandshake(c2)
	if err != nil {
		t.Fatal(err)
	}
	if hs.InfoHash != want.InfoHash || hs.PeerID != want.PeerID {
		t.Fatalf("hs=%+v want=%+v", hs, want)
	}
	if !hs.SupportsExt() || !hs.SupportsDHT() {
		t.Fatal("接收端能力位丢失")
	}
	// 主端写一个带能力位的握手,再读对端回的无能力位握手
	if err := WriteHandshake(c2, Handshake{InfoHash: testIH, PeerID: [20]byte{'y'}}, true, true, true); err != nil {
		t.Fatal(err)
	}
	hs, err = ReadHandshake(c2)
	if err != nil {
		t.Fatal(err)
	}
	if hs.SupportsExt() || hs.SupportsDHT() {
		t.Fatal("未置位的 reserved 应解析为不支持")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestReadHandshakeRejectsGarbage(t *testing.T) {
	if _, err := ReadHandshake(bytes.NewReader([]byte{20, 0, 0, 0})); err == nil {
		t.Fatal("协议名长度 20 应报错")
	}
	bad := append([]byte{19}, []byte("NotTorrent protocol")...)
	if _, err := ReadHandshake(bytes.NewReader(bad)); err == nil {
		t.Fatal("错误协议名应报错")
	}
}

func TestMsgFraming(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		WriteMsg(c1, msgHave, BuildHave(7))
		WriteKeepalive(c1)
		WriteMsg(c1, msgRequest, BuildRequest(3, 16384, 16384))
		c1.Close()
	}()

	m, err := ReadMsg(c2)
	if err != nil || m.ID != msgHave {
		t.Fatalf("have: %v %+v", err, m)
	}
	m, err = ReadMsg(c2)
	if err != nil || m.ID != MsgKeepalive {
		t.Fatalf("keepalive: %v %+v", err, m)
	}
	m, err = ReadMsg(c2)
	if err != nil || m.ID != msgRequest {
		t.Fatalf("request: %v %+v", err, m)
	}
	idx, begin, length, err := ParseRequest(m.Payload)
	if err != nil || idx != 3 || begin != 16384 || length != 16384 {
		t.Fatalf("request 解析: %v %d %d %d", err, idx, begin, length)
	}
	// 对端关闭 → EOF
	if _, err := ReadMsg(c2); err != io.EOF {
		t.Fatalf("应返回 EOF: %v", err)
	}
}

func TestBitfieldRoundTrip(t *testing.T) {
	have := make([]bool, 37)
	for i := range have {
		have[i] = i%3 == 0
	}
	payload := bitfieldBytes(have)
	if len(payload) != (37+7)/8 {
		t.Fatalf("payload 长度 %d", len(payload))
	}
	got, err := parseBitfield(payload, 37)
	if err != nil {
		t.Fatal(err)
	}
	for i := range have {
		if got[i] != have[i] {
			t.Fatalf("bit %d 不符", i)
		}
	}
	if _, err := parseBitfield(payload, 41); err == nil {
		t.Fatal("长度不符应报错")
	}
}

func TestPieceParse(t *testing.T) {
	data := []byte("0123456789")
	payload := append([]byte{0, 0, 0, 5, 0, 0, 0, 42}, data...)
	idx, begin, got, err := ParsePiece(payload)
	if err != nil || idx != 5 || begin != 42 || !bytes.Equal(got, data) {
		t.Fatalf("%v %d %d %q", err, idx, begin, got)
	}
	if _, _, _, err := ParsePiece([]byte{1, 2, 3}); err == nil {
		t.Fatal("过短 piece 应报错")
	}
}

func TestExtHandshakeRoundTrip(t *testing.T) {
	payload := BuildExtHandshake(12345, 6881, "")
	e, err := ParseExtHandshake(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !e.SupportsMetadata() || !e.SupportsPEX() {
		t.Fatal("m 字典声明丢失")
	}
	if e.PeerExtID("ut_metadata") != LocalMetadataID || e.MetadataSize != 12345 || e.ListenPort != 6881 {
		t.Fatalf("e=%+v", e)
	}
	if e.PeerExtID("ut_unknown") != 0 {
		t.Fatal("未知扩展应返回 0")
	}
	// 解析对端发来的握手(m 的 ID 是对端视角,可能与我们的不同)
	other := MustEncode(map[string]any{
		"m":             map[string]any{"ut_metadata": int64(7), "lt_dht": int64(0)},
		"metadata_size": int64(999),
		"v":             "qBittorrent 5.0",
		"reqq":          int64(500),
	})
	e, err = ParseExtHandshake(other)
	if err != nil {
		t.Fatal(err)
	}
	if e.PeerExtID("ut_metadata") != 7 || e.SupportsPEX() || e.MetadataSize != 999 || e.Version != "qBittorrent 5.0" || e.ReqQ != 500 {
		t.Fatalf("e=%+v", e)
	}
}

func TestMetadataMsg(t *testing.T) {
	// 请求/拒绝
	for _, tc := range []struct {
		mt  int64
		typ int64
	}{{metaReq, metaReq}, {metaReject, metaReject}} {
		m, err := ParseMetadataMsg(BuildMetadataMsg(tc.mt, 3))
		if err != nil || m.Type != tc.typ || m.Piece != 3 || m.Data != nil {
			t.Fatalf("%v %+v", err, m)
		}
	}
	// 数据:dict + 原始字节
	raw := []byte("d4:name3:abce")
	m, err := ParseMetadataMsg(BuildMetadataData(2, 13, raw))
	if err != nil || m.Type != metaData || m.Piece != 2 || m.TotalSize != 13 || !bytes.Equal(m.Data, raw) {
		t.Fatalf("%v %+v", err, m)
	}
	// 解析后重编码的 Data 拼接应还原出完整元数据(由 client 层拼接校验,这里验证单分片保真)
	if _, err := Decode(m.Data); err != nil {
		t.Fatalf("Data 应是完整 bencode: %v", err)
	}
}

func TestPEXRoundTrip(t *testing.T) {
	added := []PeerAddr{
		{IP: net.IPv4(10, 0, 0, 1), Port: 12345},
		{IP: net.IPv4(10, 0, 0, 2), Port: 6881},
	}
	flags := []byte{0x01, 0x02}
	dropped := []PeerAddr{{IP: net.IPv4(10, 0, 0, 3), Port: 9999}}
	payload := BuildPEX(added, flags, dropped)
	m, err := ParsePEX(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Added) != 2 || m.Added[0].String() != "10.0.0.1:12345" || m.Added[1].String() != "10.0.0.2:6881" {
		t.Fatalf("added=%v", m.Added)
	}
	if len(m.AddedF) != 2 || m.AddedF[0] != 0x01 {
		t.Fatalf("flags=%v", m.AddedF)
	}
	if len(m.Dropped) != 1 || m.Dropped[0].String() != "10.0.0.3:9999" {
		t.Fatalf("dropped=%v", m.Dropped)
	}
	// IPv6 地址被 compactPeersString 跳过,不应产生错误输出
	payload = BuildPEX([]PeerAddr{{IP: net.IPv6loopback, Port: 1}}, nil, nil)
	m, _ = ParsePEX(payload)
	if len(m.Added) != 0 {
		t.Fatalf("IPv6 不应进入 v4 紧凑地址: %v", m.Added)
	}
}

// TestWireMessageIDs 用字面量锁死线上消息 ID:BEP-6 三个 fast extension
// 常量曾整体错位(HaveAll/HaveNone/Reject 轮转一格),qBittorrent 的
// HaveAll 被当 Reject 丢弃、我方 HaveAll 对端解析成 HaveNone——同一张
// 错表在对称的回环测试里互洽,只有跨客户端互操作才会暴露。
func TestWireMessageIDs(t *testing.T) {
	if msgChoke != 0 || msgUnchoke != 1 || msgInterested != 2 || msgNotInterested != 3 ||
		msgHave != 4 || msgBitfield != 5 || msgRequest != 6 || msgPiece != 7 || msgCancel != 8 {
		t.Fatal("BEP-3 基础消息 ID 错误")
	}
	if msgExtended != 20 {
		t.Fatal("BEP-10 extended 应为 20")
	}
	if msgSuggest != 0x0D || msgHaveAll != 0x0E || msgHaveNone != 0x0F ||
		msgRejectReq != 0x10 || msgAllowedFast != 0x11 {
		t.Fatalf("BEP-6 fast extension 消息 ID 错位: suggest=%d haveAll=%d haveNone=%d reject=%d allowedFast=%d",
			msgSuggest, msgHaveAll, msgHaveNone, msgRejectReq, msgAllowedFast)
	}
}
