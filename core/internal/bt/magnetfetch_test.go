package bt

import (
	"context"
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeV2Tree 把 v2 测试文件写到临时目录(多文件布局:相对路径逐段建目录)。
func writeV2Tree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for p, d := range files {
		fp := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, d, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestFetchMetadataV2FromSeedPeer:v2 种子的 ut_metadata 只传 info dict
// (BEP-52 的 piece layers 在顶层、不随元数据交换传输)。抓取侧须以
// SHA-256(info) 校验,并接受"层未知"的解析结果。
func TestFetchMetadataV2FromSeedPeer(t *testing.T) {
	files := v2TestFiles()
	root := writeV2Tree(t, files)
	seedMeta, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(seedMeta, root, ClientConfig{MaxPeers: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()
	host, portStr, err := net.SplitHostPort(seed.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	addr := PeerAddr{IP: net.ParseIP(host), Port: uint16(port)}

	full := sha256.Sum256(seedMeta.RawInfo())
	var ih [20]byte
	copy(ih[:], full[:20])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := FetchMetadataV2(ctx, ih, &full, PeerID(), nil, []PeerAddr{addr}, 0, nil, NetConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.V2 || got.PieceCount() != seedMeta.PieceCount() || got.TotalLength != seedMeta.TotalLength {
		t.Fatalf("元数据不符: %+v", got)
	}
	if got.LayerKnown(1) {
		t.Fatal("ut_metadata 不传顶层 layers,多 piece 文件层应未知")
	}
	// 拿错哈希(错误 v2)必须拒绝
	bad := full
	bad[0] ^= 0xFF
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := FetchMetadataV2(ctx2, ih, &bad, PeerID(), nil, []PeerAddr{addr}, 0, nil, NetConfig{}); err == nil {
		t.Fatal("错误 v2 哈希不应通过校验")
	}
}

// TestV2DownloadWithoutLayers 全链路:下载端的元数据缺 piece layers(磁力
// 形态),靠整文件 merkle 反推层哈希完成逐片校验并落定,同时验证反推层可
// 写回归档种子、重启后可直接逐片校验。
func TestV2DownloadWithoutLayers(t *testing.T) {
	files := v2TestFiles()
	root := writeV2Tree(t, files)
	seedMeta, err := ParseTorrent(buildV2Torrent(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(seedMeta, root, ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 下载端:模拟磁力拿到的 info-only 元数据(无 layers)
	leechMeta, err := ParseTorrent(buildV2TorrentOpts(t, files, nil, v2BuildOpts{withLayers: false}))
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	leech, err := NewClient(leechMeta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, leech)

	for _, f := range leechMeta.Files {
		want, err := os.ReadFile(filepath.Join(root, filepath.Join(f.Path...)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(leech.FinalPath(), filepath.Join(f.Path...)))
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != string(got) {
			t.Fatalf("文件 %v 内容不符", f.Path)
		}
	}
	// 多 piece 文件的层应由整文件校验反推出来,逐片校验此后可用
	for fi, f := range leechMeta.Files {
		if f.Length > leechMeta.PieceLength && !leechMeta.LayerKnown(fi) {
			t.Fatalf("文件 %v 的层应已被反推", f.Path)
		}
	}
	if !leechMeta.LayersDirty() {
		t.Fatal("反推层后应标记待写回归档")
	}
	// 写回归档 → 再解析:层已知,逐片校验直接可用(重启路径)
	raw, err := BuildTorrentBytes(leechMeta, "", "")
	if err != nil {
		t.Fatal(err)
	}
	reMeta, err := ParseTorrent(raw)
	if err != nil {
		t.Fatal(err)
	}
	have := leech.HaveBits()
	for i := range have {
		if !have[i] {
			continue
		}
		fi := reMeta.PieceFile(i)
		if fi < 0 {
			continue
		}
		if reMeta.Files[fi].Length > reMeta.PieceLength && !reMeta.LayerKnown(fi) {
			t.Fatalf("重启后 piece %d 所属文件层应已知", i)
		}
	}
}

func TestFetchMetadataFromSeedPeer(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(meta, srcRoot, ClientConfig{MaxPeers: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	host, portStr, err := net.SplitHostPort(seed.ListenAddr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	addr := PeerAddr{IP: net.ParseIP(host), Port: uint16(port)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := FetchMetadata(ctx, meta.InfoHash, PeerID(), nil, []PeerAddr{addr}, 0, nil, NetConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHash != meta.InfoHash {
		t.Fatal("infohash 不符")
	}
	if got.Name != meta.Name || got.TotalLength != meta.TotalLength || got.PieceCount() != meta.PieceCount() {
		t.Fatalf("got=%+v", got)
	}
	for i := 0; i < meta.PieceCount(); i++ {
		if got.PieceHash(i) != meta.PieceHash(i) {
			t.Fatalf("piece %d 哈希不符", i)
		}
	}
}
