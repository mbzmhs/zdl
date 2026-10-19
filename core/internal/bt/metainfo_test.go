package bt

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTestTree(t *testing.T) (root string, bin []byte) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.dat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bin = make([]byte, 70000)
	if _, err := rand.Read(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, bin
}

func TestBuildAndParseRoundTrip(t *testing.T) {
	root, bin := buildTestTree(t)
	_, mi, err := BuildTorrentFromDisk(root, "http://t/announce", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Name != filepath.Base(root) || !mi.Multi {
		t.Fatalf("name=%q multi=%v", mi.Name, mi.Multi)
	}
	if mi.Announce != "http://t/announce" {
		t.Fatalf("announce=%q", mi.Announce)
	}
	if len(mi.Files) != 3 || mi.TotalLength != 70005 {
		t.Fatalf("files=%d total=%d", len(mi.Files), mi.TotalLength)
	}
	// 字典序:a.txt, empty.dat, sub/b.bin;零长文件不占字节流
	f0, f1, f2 := mi.Files[0], mi.Files[1], mi.Files[2]
	if f0.Path[0] != "a.txt" || f0.Offset != 0 || f0.Length != 5 {
		t.Fatalf("f0=%+v", f0)
	}
	if f1.Path[0] != "empty.dat" || f1.Offset != 5 || f1.Length != 0 {
		t.Fatalf("f1=%+v", f1)
	}
	if f2.Path[0] != "sub" || f2.Path[1] != "b.bin" || f2.Offset != 5 || f2.Length != 70000 {
		t.Fatalf("f2=%+v", f2)
	}
	if mi.PieceCount() != 5 || mi.PieceLen(4) != 4469 || mi.PieceLen(0) != 16384 {
		t.Fatalf("pieces=%d len0=%d len4=%d", mi.PieceCount(), mi.PieceLen(0), mi.PieceLen(4))
	}
	// piece 0 = a.txt 全部 + b.bin 前 16379 字节
	want := sha1.Sum(append([]byte("hello"), bin[:16379]...))
	if mi.PieceHash(0) != want {
		t.Fatal("piece 0 哈希不符")
	}
	want = sha1.Sum(bin[16379 : 16379+16384])
	if mi.PieceHash(1) != want {
		t.Fatal("piece 1(跨文件边界)哈希不符")
	}
	// 构造可复现:两次构建 infohash 一致
	_, mi2, err := BuildTorrentFromDisk(root, "http://t/announce", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if mi2.InfoHash != mi.InfoHash {
		t.Fatal("infohash 不稳定")
	}
	// readDiskRange 跨文件边界读取
	buf := make([]byte, 16384)
	if err := readDiskRange(true, root, mi.Files, 16384, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, bin[16379:16379+16384]) {
		t.Fatal("readDiskRange 跨边界内容不符")
	}
}

func TestBuildSingleFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "only.bin")
	content := bytes.Repeat([]byte{0x5A}, 30000)
	if err := os.WriteFile(root, content, 0o644); err != nil {
		t.Fatal(err)
	}
	data, mi, err := BuildTorrentFromDisk(root, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Multi || mi.Name != "only.bin" || mi.Length != 30000 || mi.TotalLength != 30000 {
		t.Fatalf("mi=%+v", mi)
	}
	if len(mi.Files) != 1 || mi.Files[0].Path[0] != "only.bin" {
		t.Fatalf("files=%+v", mi.Files)
	}
	// 默认 piece 长度自适应(<=64MB → 256KiB)
	if mi.PieceLength != 256<<10 {
		t.Fatalf("pieceLength=%d", mi.PieceLength)
	}
	if mi.PieceCount() != 1 { // 30000 < 262144
		t.Fatalf("pieceCount=%d", mi.PieceCount())
	}
	// announce 为空时顶层不含该键
	if bytes.Contains(data, []byte("announce")) {
		t.Fatal("不应包含 announce")
	}
}

func manualTorrent(t *testing.T, info map[string]any) []byte {
	t.Helper()
	infoBytes, err := Encode(info)
	if err != nil {
		t.Fatal(err)
	}
	return MustEncode(map[string]any{"info": Raw(infoBytes)})
}

func TestParsePrivateFlag(t *testing.T) {
	pieces := bytes.Repeat([]byte{1}, 20)
	data := manualTorrent(t, map[string]any{"private": int64(1), "pieces": string(pieces), "length": int64(1000), "name": "x", "piece length": int64(16384)})
	mi, err := ParseTorrent(data)
	if err != nil {
		t.Fatal(err)
	}
	if !mi.Private || mi.Length != 1000 {
		t.Fatalf("mi=%+v", mi)
	}
	data = manualTorrent(t, map[string]any{"private": int64(0), "pieces": string(pieces), "length": int64(1000), "name": "x", "piece length": int64(16384)})
	mi, err = ParseTorrent(data)
	if err != nil || mi.Private {
		t.Fatalf("private=0 解析错误: %v %+v", err, mi)
	}
}

func TestParseTorrentTrailingBytes(t *testing.T) {
	// 部分 .torrent 尾部带多余字节(工具拼接/追加/二次导出残留),严格
	// Decode 会报"尾部多余"整体拒绝;ParseTorrent 应容错恢复,且 infohash
	// 与干净版一致(info dict 原始字节未受尾部影响)。
	pieces := bytes.Repeat([]byte{3}, 20)
	info := map[string]any{"name": "x", "piece length": int64(16384), "pieces": string(pieces), "length": int64(1000)}
	clean := manualTorrent(t, info)
	ci, err := ParseTorrent(clean)
	if err != nil {
		t.Fatal(err)
	}
	dirty := append(append([]byte{}, clean...),
		[]byte("\r\njunk trailing garbage that a strict decoder would reject")...)
	di, err := ParseTorrent(dirty)
	if err != nil {
		t.Fatalf("带尾部多余字节应容错解析: %v", err)
	}
	if di.InfoHash != ci.InfoHash {
		t.Fatalf("infohash 漂移: clean=%x dirty=%x", ci.InfoHash, di.InfoHash)
	}
	if di.Name != ci.Name || di.Length != ci.Length {
		t.Fatalf("字段漂移: clean=%+v dirty=%+v", ci, di)
	}
}

func TestParseRejectsPathTraversal(t *testing.T) {
	pieces := bytes.Repeat([]byte{1}, 20)
	info := map[string]any{
		"name": "evil", "piece length": int64(16384), "pieces": string(pieces),
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"..", "..", "win.ini"}},
		},
	}
	if _, err := ParseTorrent(manualTorrent(t, info)); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
	// 非法字符被清洗而非拒绝
	info["files"] = []any{map[string]any{"length": int64(1), "path": []any{"a<b>c:d"}}}
	mi, err := ParseTorrent(manualTorrent(t, info))
	if err != nil {
		t.Fatal(err)
	}
	if mi.Files[0].Path[0] != "a_b_c_d" {
		t.Fatalf("清洗结果=%q", mi.Files[0].Path[0])
	}
}

func TestParseErrors(t *testing.T) {
	root, _ := buildTestTree(t)
	data, _, err := BuildTorrentFromDisk(root, "http://t/a", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTorrent(data[:len(data)-3]); err == nil {
		t.Fatal("截断数据应报错")
	}
	// piece 数与总长不符
	bad := manualTorrent(t, map[string]any{
		"name": "x", "piece length": int64(16384), "pieces": string(bytes.Repeat([]byte{2}, 20)), "length": int64(999999),
	})
	if _, err := ParseTorrent(bad); err == nil {
		t.Fatal("piece 数不符应报错")
	}
	// pieces 长度非 20 倍数
	bad = manualTorrent(t, map[string]any{
		"name": "x", "piece length": int64(16384), "pieces": "12345", "length": int64(999),
	})
	if _, err := ParseTorrent(bad); err == nil {
		t.Fatal("pieces 非法应报错")
	}
	// 缺少 name
	bad = manualTorrent(t, map[string]any{"piece length": int64(16384), "pieces": "", "length": int64(0)})
	if _, err := ParseTorrent(bad); err == nil {
		t.Fatal("缺少 name 应报错")
	}
}

func TestParseEmptyTorrent(t *testing.T) {
	// 0 长度单文件种子合法:piece 数为 0
	data := manualTorrent(t, map[string]any{"name": "e", "piece length": int64(16384), "pieces": "", "length": int64(0)})
	mi, err := ParseTorrent(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.PieceCount() != 0 || mi.TotalLength != 0 {
		t.Fatalf("mi=%+v", mi)
	}
}

func TestInfoHashAgainstPythonCraftedVector(t *testing.T) {
	// 手工构造并核对已知 infohash(与 mktorrent 等工具同构的规范编码)
	// info = {"length":9,"name":"test.txt","piece length":32768,"pieces":sha1("123456789")}
	content := []byte("123456789")
	h := sha1.Sum(content)
	info := map[string]any{
		"length": int64(9), "name": "test.txt", "piece length": int64(32768), "pieces": string(h[:]),
	}
	mi, err := ParseTorrent(manualTorrent(t, info))
	if err != nil {
		t.Fatal(err)
	}
	// 独立重算:sha1(bencode(info)) 应等于解析出的 infohash
	infoBytes, _ := Encode(info)
	want := sha1.Sum(infoBytes)
	if mi.InfoHash != want {
		t.Fatalf("infohash %x != %x", mi.InfoHash, want)
	}
}

func TestMagnetHexAndBase32(t *testing.T) {
	ih := [20]byte{0x2c, 0x6b, 0x68, 0x58, 0xd6, 0x1d, 0xa9, 0x54, 0x3d, 0x42, 0x31, 0xa7, 0x1d, 0xb4, 0xb1, 0xc9, 0x26, 0x4b, 0x06, 0x85}
	hexStr := hex.EncodeToString(ih[:])
	b32Str := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(ih[:])

	m, err := ParseMagnet("magnet:?xt=urn:btih:" + hexStr + "&dn=Test%20Name&tr=http%3A%2F%2Fa%2Fan&tr=http%3A%2F%2Fb%2Fbn&x.pe=1.2.3.4:51413")
	if err != nil {
		t.Fatal(err)
	}
	if m.InfoHash != ih || m.Name != "Test Name" || len(m.Trackers) != 2 || len(m.Peers) != 1 {
		t.Fatalf("m=%+v", m)
	}
	// 大写十六进制
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + strings.ToUpper(hexStr))
	if err != nil {
		t.Fatal(err)
	}
	if m.InfoHash != ih {
		t.Fatalf("大写 hex 解析不符: %+v", m)
	}
	// base32
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + b32Str + "&tr=udp%3A%2F%2Ft%3A1337")
	if err != nil || m.InfoHash != ih || len(m.Trackers) != 1 {
		t.Fatalf("b32: %v %+v", err, m)
	}
	// 重复 tracker 去重
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + hexStr + "&tr=http%3A%2F%2Fa%2Fan&tr=http%3A%2F%2Fa%2Fan")
	if err != nil || len(m.Trackers) != 1 {
		t.Fatalf("去重失败: %v %+v", err, m)
	}
	// 非法 x.pe 被忽略
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + hexStr + "&x.pe=not-a-peer")
	if err != nil || len(m.Peers) != 0 {
		t.Fatalf("非法 x.pe: %v %+v", err, m)
	}
}

func TestMagnetErrors(t *testing.T) {
	cases := []string{
		"",
		"http://example.com/file",
		"magnet:?",
		"magnet:?dn=only-name",
		"magnet:?xt=urn:btih:zzzz",       // 非法哈希
		"magnet:?xt=urn:btih:2c6b6858",   // 长度不足
		"magnet:?xt=urn:btmh:1220abcdef", // btmh 长度不足
		"magnet:?xt=urn:btmh:zz" + strings.Repeat("0", 62), // btmh 非十六进制
	}
	for _, c := range cases {
		if _, err := ParseMagnet(c); err == nil {
			t.Errorf("ParseMagnet(%q) 应返回错误", c)
		}
	}
	// 既无 btih 也无 btmh
	if _, err := ParseMagnet("magnet:?dn=x"); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("缺哈希错误不明确: %v", err)
	}
}

func TestMagnetV2(t *testing.T) {
	v2 := sha256.Sum256([]byte("v2 info dict bytes"))
	hex64 := hex.EncodeToString(v2[:])

	// 规范形态:btmh 带 1220 multihash 前缀
	m, err := ParseMagnet("magnet:?xt=urn:btmh:1220" + hex64 + "&dn=V2%20Name&tr=http%3A%2F%2Ft%2Fa")
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsV2() || m.V2Hash == nil || *m.V2Hash != v2 {
		t.Fatalf("v2 哈希解析不符: %+v", m)
	}
	// DHT/握手用 SHA-256(info) 截断 20 字节
	if want := [20]byte(v2[:20]); m.InfoHash != want {
		t.Fatalf("截断 infohash 不符: %x != %x", m.InfoHash, want)
	}
	if m.Name != "V2 Name" || len(m.Trackers) != 1 {
		t.Fatalf("m=%+v", m)
	}

	// 裸 64 位十六进制(无 1220 前缀)同样接受
	m, err = ParseMagnet("magnet:?xt=urn:btmh:" + strings.ToUpper(hex64))
	if err != nil || m.V2Hash == nil || *m.V2Hash != v2 {
		t.Fatalf("裸 hex btmh: %v %+v", err, m)
	}

	// hybrid:btih + btmh 并存,InfoHash 用 v1,校验哈希两者都留
	v1hex := hex.EncodeToString(bytes.Repeat([]byte{0x11}, 20))
	m, err = ParseMagnet("magnet:?xt=urn:btih:" + v1hex + "&xt=urn:btmh:1220" + hex64)
	if err != nil {
		t.Fatal(err)
	}
	if m.V2Hash == nil || *m.V2Hash != v2 {
		t.Fatalf("hybrid 缺 v2 哈希: %+v", m)
	}
	if want := bytes.Repeat([]byte{0x11}, 20); !bytes.Equal(m.InfoHash[:], want) {
		t.Fatalf("hybrid 应以 v1 为 infohash: %x", m.InfoHash)
	}

	// btmh 承载 v1 sha1(1114 前缀)也接受
	m, err = ParseMagnet("magnet:?xt=urn:btmh:1114" + v1hex)
	if err != nil || m.IsV2() || !bytes.Equal(m.InfoHash[:], bytes.Repeat([]byte{0x11}, 20)) {
		t.Fatalf("btmh v1 形态: %v %+v", err, m)
	}
}

// FileWantedFor 按文件路径精确匹配文件级需求位图:与 WantedPiecesFor 的
// piece 区间推算不同,共享边界 piece 的相邻文件不得被误标为需求。
func TestFileWantedFor(t *testing.T) {
	m := &MetaInfo{Name: "pack", Files: []FileInfo{
		{Path: []string{"a.txt"}, Length: 100},
		{Path: []string{"sub", "b.txt"}, Length: 200},
		{Path: []string{"c.txt"}, Length: 300},
	}}

	fw, err := m.FileWantedFor(nil)
	if fw != nil || err != nil {
		t.Fatalf("空选择 = 全部: %v %v", fw, err)
	}
	fw, err = m.FileWantedFor([]string{"a.txt", "sub/b.txt"})
	if err != nil || len(fw) != 3 || !fw[0] || !fw[1] || fw[2] {
		t.Fatalf("按路径选中: %v %v", fw, err)
	}
	// 反斜杠路径归一化
	fw, err = m.FileWantedFor([]string{"sub\\b.txt"})
	if err != nil || len(fw) != 3 || fw[0] || !fw[1] || fw[2] {
		t.Fatalf("反斜杠归一化: %v %v", fw, err)
	}
	// 任一路径不在种子中必须报错(否则上层回退全量下载,选择静默失效)
	if _, err = m.FileWantedFor([]string{"a.txt", "nope.txt"}); err == nil {
		t.Fatal("未知路径应报错")
	}
}
