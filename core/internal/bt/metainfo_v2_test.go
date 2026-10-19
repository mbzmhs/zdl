package bt

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"testing"
)

// v2BuildOpts 控制合成种子的形态。
type v2BuildOpts struct {
	// withLayers=false 模拟磁力经 ut_metadata 只拿到 info dict(BEP-52 的
	// piece layers 在顶层、不在 info 内),解析后该文件层哈希未知。
	withLayers bool
	// corruptLayer 篡改某个多 piece 文件的 layer 内容(验证解析期对拍拒绝)。
	corruptLayer bool
}

// buildV2Torrent 构造 BEP-52 v2-only 种子字节(带完整 piece layers)。files 为
// 相对路径(斜杠)→内容;piece 长度固定 16KiB(块级树的叶级=piece 层)。按规范:
// 叶 = sha256(16K 块),末块可短,叶数补零到 2^n,piece layers 即块叶层
// (超出文件末的省略)。
func buildV2Torrent(t *testing.T, files map[string][]byte, extraTop map[string]any) []byte {
	t.Helper()
	return buildV2TorrentOpts(t, files, extraTop, v2BuildOpts{withLayers: true})
}

func buildV2TorrentOpts(t *testing.T, files map[string][]byte, extraTop map[string]any, o v2BuildOpts) []byte {
	t.Helper()
	const pl = 1 << 14
	type fv struct {
		path  []string
		data  []byte
		root  string
		layer string
	}
	var fs []fv
	for p, d := range files {
		fs = append(fs, fv{path: splitSlash(p), data: d})
	}
	// 规范顺序:bencode 字典序(逐段字节序)
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && lessPath(fs[j].path, fs[j-1].path); j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}

	tree := map[string]any{}
	layers := map[string]any{}
	for i := range fs {
		d := fs[i].data
		nb := (len(d) + v2BlockSize - 1) / v2BlockSize
		if nb == 0 {
			nb = 1 // 零长文件也占一个(空)叶位置,root 不参与 piece
		}
		leaves := make([][32]byte, nb)
		lb := make([]byte, 0, nb*32)
		for j := 0; j < (len(d)+v2BlockSize-1)/v2BlockSize; j++ {
			end := min((j+1)*v2BlockSize, len(d))
			leaves[j] = sha256.Sum256(d[j*v2BlockSize : end])
			lb = append(lb, leaves[j][:]...)
		}
		root := v2BlockRoot(leaves)
		fs[i].root = string(root[:])
		if len(d) > pl {
			layers[fs[i].root] = string(lb)
		}
		// 挂到 file tree
		cur := tree
		for _, seg := range fs[i].path[:len(fs[i].path)-1] {
			next, ok := cur[seg].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[seg] = next
			}
			cur = next
		}
		last := fs[i].path[len(fs[i].path)-1]
		cur[last] = map[string]any{"length": int64(len(d)), "pieces root": fs[i].root}
	}

	info := map[string]any{
		"name":         "v2test",
		"piece length": int64(pl),
		"meta version": int64(2),
		"file tree":    tree,
	}
	if o.corruptLayer {
		for k, v := range layers {
			b := []byte(v.(string))
			b[0] ^= 0xFF
			layers[k] = string(b)
			break
		}
	}
	top := map[string]any{"info": info, "announce": "http://t/announce"}
	if o.withLayers {
		top["piece layers"] = layers
	}
	for k, v := range extraTop {
		top[k] = v
	}
	data, err := Encode(top)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// v2BlockRoot 16K 块叶补零到 2^n 后上卷的 merkle 根。
func v2BlockRoot(leaves [][32]byte) [32]byte {
	m := 1
	for m < len(leaves) {
		m <<= 1
	}
	cur := make([][32]byte, m)
	copy(cur, leaves)
	for i := len(leaves); i < m; i++ {
		cur[i] = zero32
	}
	for len(cur) > 1 {
		next := make([][32]byte, len(cur)/2)
		for i := range next {
			var buf [64]byte
			copy(buf[:32], cur[2*i][:])
			copy(buf[32:], cur[2*i+1][:])
			next[i] = sha256.Sum256(buf[:])
		}
		cur = next
	}
	return cur[0]
}

func splitSlash(p string) []string {
	var out []string
	for _, s := range bytes.Split([]byte(p), []byte("/")) {
		out = append(out, string(s))
	}
	return out
}

func lessPath(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func v2TestFiles() map[string][]byte {
	return map[string][]byte{
		"b.txt":         bytes.Repeat([]byte{0xAB}, 50000),       // 4 pieces(末 256B)
		"a/inner/c.bin": bytes.Repeat([]byte{0xCD}, 2*16384+100), // 3 pieces
		"empty.txt":     nil,                                     // 零长:不占 piece
		"one.txt":       []byte("tiny"),                          // 单 piece:root 即叶,无 layers
	}
}

func TestParseV2Torrent(t *testing.T) {
	files := v2TestFiles()
	mi, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !mi.V2 {
		t.Fatal("应识别为 v2 种子")
	}
	// 含嵌套路径(a/inner/c.bin)必须判为多文件目录
	if !mi.Multi {
		t.Fatal("含嵌套路径的 v2 种子应为多文件")
	}
	// 文件表:规范序 a/inner/c.bin, b.txt, empty.txt, one.txt
	wantPaths := []string{"a/inner/c.bin", "b.txt", "empty.txt", "one.txt"}
	if len(mi.Files) != len(wantPaths) {
		t.Fatalf("文件数 %d != %d", len(mi.Files), len(wantPaths))
	}
	for i, w := range wantPaths {
		got := joinSlash(mi.Files[i].Path)
		if got != w {
			t.Fatalf("文件序 %d: %s != %s", i, got, w)
		}
	}
	// piece 计数:4+3+0+1 = 8
	if mi.PieceCount() != 8 {
		t.Fatalf("piece 数 %d != 8", mi.PieceCount())
	}
	// 偏移表:逐 piece 长度和 == 总长;偏移与文件 Offset 一致
	var sum int64
	for i := 0; i < mi.PieceCount(); i++ {
		sum += mi.PieceLen(i)
	}
	if sum != mi.TotalLength {
		t.Fatalf("piece 长度和 %d != 总长 %d", sum, mi.TotalLength)
	}
	if mi.PieceLen(6) != 50000-3*16384 {
		t.Fatalf("b.txt 末 piece 长度 %d 应为 %d", mi.PieceLen(6), 50000-3*16384)
	}
	// 零长文件不占 piece:区间为空(last < first)
	first, last := mi.FilePieceRange(2)
	if last >= first {
		t.Fatalf("零长文件应为空 piece 区间, got [%d,%d]", first, last)
	}
}

func joinSlash(p []string) string {
	s := ""
	for i, x := range p {
		if i > 0 {
			s += "/"
		}
		s += x
	}
	return s
}

func TestV2VerifyPieces(t *testing.T) {
	files := v2TestFiles()
	mi, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	const pl = 1 << 14
	for _, rel := range []string{"b.txt", "a/inner/c.bin", "one.txt"} {
		data := files[rel]
		fi := -1
		for i, f := range mi.Files {
			if joinSlash(f.Path) == rel {
				fi = i
			}
		}
		if fi < 0 {
			t.Fatalf("文件 %s 不在表中", rel)
		}
		first, last := mi.FilePieceRange(fi)
		_ = last
		for j := 0; j*pl < len(data); j++ {
			end := min((j+1)*pl, len(data))
			piece := data[j*pl : end]
			if !mi.VerifyPieceData(first+j, piece) {
				t.Fatalf("%s piece %d 校验应通过", rel, j)
			}
			// 篡改一字节必须失败
			bad := append([]byte{}, piece...)
			bad[0] ^= 0xFF
			if mi.VerifyPieceData(first+j, bad) {
				t.Fatalf("%s piece %d 篡改后不应通过", rel, j)
			}
		}
	}
}

func TestV2WantedPiecesAndProgress(t *testing.T) {
	files := v2TestFiles()
	mi, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	// 只选 b.txt:规范序 1(a/inner/c.bin 3 piece → 0-2),b.txt 占 3..6
	wanted := mi.WantedPiecesFor([]string{"b.txt"})
	if wanted == nil {
		t.Fatal("选择结果不应为 nil")
	}
	for i, w := range wanted {
		if want := i >= 3 && i <= 6; w != want {
			t.Fatalf("piece %d wanted=%v 应为 %v", i, w, want)
		}
	}
	if got := mi.WantedBytes(wanted); got != 50000 {
		t.Fatalf("选中字节数 %d != 50000", got)
	}
	// 进度:piece3、piece4(b.txt 前两片)已校验 → b.txt 完成 32768,其余 0
	have := make([]bool, mi.PieceCount())
	have[3], have[4] = true, true
	prog := FileProgressOf(mi, have, nil)
	byPath := map[string]int64{}
	for _, p := range prog {
		byPath[joinSlash(p.Path)] = p.Done
	}
	if byPath["b.txt"] != 2*16384 {
		t.Fatalf("b.txt 进度 %d != 32768", byPath["b.txt"])
	}
	if byPath["one.txt"] != 0 || byPath["a/inner/c.bin"] != 0 {
		t.Fatalf("未完成文件进度应为 0: %v", byPath)
	}
}

func TestV2SingleFileTorrent(t *testing.T) {
	mi, err := ParseTorrent(buildV2Torrent(t, map[string][]byte{"only.bin": bytes.Repeat([]byte{7}, 40000)}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if mi.V2 {
		// 根级单段文件:按 v1 单文件布局(数据即文件本身)
	}
	if mi.Multi {
		t.Fatal("单文件 v2 不应为 Multi")
	}
	if len(mi.Files) != 1 || joinSlash(mi.Files[0].Path) != "v2test" {
		t.Fatalf("单文件布局错误: %v", mi.Files)
	}
	if mi.PieceCount() != 3 {
		t.Fatalf("piece 数 %d != 3", mi.PieceCount())
	}
	if !mi.VerifyPieceData(0, bytes.Repeat([]byte{7}, 16384)) {
		t.Fatal("piece0 校验应通过")
	}
	last := 40000 - 2*16384
	if !mi.VerifyPieceData(2, bytes.Repeat([]byte{7}, last)) {
		t.Fatal("末 piece 校验应通过")
	}
}

// TestV2TorrentWithoutLayers:模拟磁力经 ut_metadata 只拿到 info dict ——
// BEP-52 的顶层 piece layers 缺失时仍应能解析:单 piece 文件与零长文件层已知,
// 多 piece 文件层未知(逐片校验不可用,须下载期整文件反推)。
func TestV2TorrentWithoutLayers(t *testing.T) {
	files := v2TestFiles()
	mi, err := ParseTorrent(buildV2TorrentOpts(t, files, nil, v2BuildOpts{withLayers: false}))
	if err != nil {
		t.Fatalf("缺 piece layers 的 v2 种子应可解析: %v", err)
	}
	if !mi.V2 || mi.PieceCount() != 8 {
		t.Fatalf("元数据不符: v2=%v pieces=%d", mi.V2, mi.PieceCount())
	}
	// 规范序:a/inner/c.bin(3 片,未知), b.txt(4 片,未知), empty.txt(零长,已知), one.txt(单 piece,已知)
	if mi.LayerKnown(0) {
		t.Fatal("c.bin 多 piece 且无 layers,层应未知")
	}
	if mi.LayerKnown(1) {
		t.Fatal("b.txt 多 piece 且无 layers,层应未知")
	}
	if !mi.LayerKnown(2) || !mi.LayerKnown(3) {
		t.Fatal("零长/单 piece 文件的层应已知")
	}
	// 层未知时逐片校验必须明确失败(不可误判通过)
	if mi.VerifyPieceData(0, make([]byte, 16384)) {
		t.Fatal("层未知时逐片校验不应通过")
	}

	// 下载期反推:按块级节点哈希回填后,逐片校验恢复可用
	first, _ := mi.FilePieceRange(1) // b.txt:piece 3..6
	data := files["b.txt"]
	for j := 0; j*16384 < len(data); j++ {
		end := min((j+1)*16384, len(data))
		piece := data[j*16384 : end]
		mi.pieceLeaf[first+j] = mi.PieceNodeHash(first+j, piece) // 模拟 client 累计后 SetFileLayer
	}
	mi.layerKnown[1] = true
	for j := 0; j*16384 < len(data); j++ {
		end := min((j+1)*16384, len(data))
		if !mi.VerifyPieceData(first+j, data[j*16384:end]) {
			t.Fatalf("回填层后 b.txt piece %d 应通过", j)
		}
	}

	// 归档往返:反推的 layers 必须随 BuildTorrentBytes 写回,重新解析后层已知
	raw, err := BuildTorrentBytes(mi, "http://t/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	mi2, err := ParseTorrent(raw)
	if err != nil {
		t.Fatalf("归档种子再解析失败: %v", err)
	}
	if !mi2.LayerKnown(1) {
		t.Fatal("归档往返后 b.txt 层应已知")
	}
	if mi2.pieceLeaf[first] != mi.pieceLeaf[first] {
		t.Fatal("归档往返后 piece 叶哈希不一致")
	}
	if mi2.InfoHash != mi.InfoHash || mi2.TotalLength != mi.TotalLength {
		t.Fatal("归档往返元数据不一致")
	}
	// 归档保留仍未反推文件的"层未知"状态(不得写入垃圾层)
	if mi2.LayerKnown(0) {
		t.Fatal("c.bin 层未反推,归档后不该谎称已知")
	}
}

func TestV2LayerRootMismatchRejected(t *testing.T) {
	if _, err := ParseTorrent(buildV2TorrentOpts(t, v2TestFiles(), nil, v2BuildOpts{withLayers: true, corruptLayer: true})); err == nil {
		t.Fatal("layers 与 pieces root 不符的种子必须被拒绝")
	}
	// 干净种子必须解析成功
	if _, err := ParseTorrent(buildV2Torrent(t, v2TestFiles(), nil)); err != nil {
		t.Fatal(err)
	}
}

// TestV2MetadataVerify 校验 metaHolder 的 v1/v2/hybrid 三种校验口径。
func TestV2MetadataVerify(t *testing.T) {
	info := ParseTorrentMustRawInfo(t, v2TestFiles())
	full := sha256.Sum256(info)
	var ih [20]byte
	copy(ih[:], full[:20])

	// v2:正确 SHA-256 通过
	h := &metaHolder{ih: ih, v2: &full, pieces: map[int][]byte{0: info}}
	if !h.complete(int64(len(info))) {
		t.Fatal("complete 应为真")
	}
	h.assemble()
	got := h.result()
	if got == nil || !got.V2 || got.PieceCount() != 8 {
		t.Fatalf("v2 元数据校验未通过: %+v", got)
	}

	// v2:错误哈希拒绝(分片被污染)
	bad := full
	bad[0] ^= 0xFF
	h2 := &metaHolder{ih: ih, v2: &bad, pieces: map[int][]byte{0: info}}
	h2.complete(int64(len(info)))
	h2.assemble()
	if h2.result() != nil {
		t.Fatal("错误 v2 哈希不应通过")
	}

	// hybrid:提供错误 v2 哈希但 SHA-1 命中 v1 infohash,应接受
	h3 := &metaHolder{ih: sha1.Sum(info), v2: &bad, pieces: map[int][]byte{0: info}}
	h3.complete(int64(len(info)))
	h3.assemble()
	if h3.result() == nil {
		t.Fatal("hybrid 下 SHA-1 命中应接受")
	}
}

// ParseTorrentMustRawInfo 取合成 v2 种子的原始 info dict 字节。
func ParseTorrentMustRawInfo(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	mi, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	return mi.RawInfo()
}

// TestFileProgRangesAndWanted 文件清单的分块序号与选中态(详情页列展示口径)。
func TestFileProgRangesAndWanted(t *testing.T) {
	files := v2TestFiles()
	mi, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	wanted := mi.WantedPiecesFor([]string{"b.txt"})
	prog := FileProgressOf(mi, make([]bool, mi.PieceCount()), fileWantedFor(mi, wanted))
	byPath := map[string]FileProg{}
	for _, p := range prog {
		byPath[joinSlash(p.Path)] = p
	}
	// 规范序:a/inner/c.bin(0-2), b.txt(3-6), empty.txt(空区间), one.txt(7)
	if p := byPath["b.txt"]; !p.Wanted || p.First != 3 || p.Last != 6 {
		t.Fatalf("b.txt 应选中且分块 3~6: %+v", p)
	}
	if p := byPath["a/inner/c.bin"]; p.Wanted || p.First != 0 || p.Last != 2 {
		t.Fatalf("a/inner/c.bin 未选且分块 0~2: %+v", p)
	}
	if p := byPath["empty.txt"]; p.Wanted || p.Last >= p.First {
		t.Fatalf("empty.txt 未选且应为空 piece 区间: %+v", p)
	}
	if p := byPath["one.txt"]; p.Wanted || p.First != 7 || p.Last != 7 {
		t.Fatalf("one.txt 未选且分块 7: %+v", p)
	}
	// 全部选择(fileWanted=nil):wanted 全真
	for _, p := range FileProgressOf(mi, nil, nil) {
		if !p.Wanted {
			t.Fatalf("%v 应为选中态", p.Path)
		}
	}
}
