package bt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildTwoFileTree 两个各占多个 piece 的文件,供部分选择测试。
// 手动临时目录(非 t.TempDir):Windows 下测试结束即刻清理时,客户端
// 句柄可能尚未完全释放导致 RemoveAll 失败误报 FAIL。
func buildTwoFileTree(t *testing.T) (root, f1 string) {
	t.Helper()
	root, err := os.MkdirTemp("", "zdpart-src")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f1 = filepath.Join(root, "f1.bin")
	b1 := make([]byte, 100_000)
	b2 := make([]byte, 90_000)
	for i := range b1 {
		b1[i] = byte(i*7 + 1)
	}
	for i := range b2 {
		b2[i] = byte(i*5 + 3)
	}
	if err := os.WriteFile(f1, b1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f2.bin"), b2, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, f1
}

// TestPartialSeedServesSelected 回归:只选部分文件的下载任务完成后做种,
// 对端(全量下载者)必须能从它取走【全部选中文件】的内容,而不是极小一部分。
// 根因(bte bt.79):跨界 piece 属于未选邻居的边界字节曾被禁止落盘,做种
// 服务该 piece 时读不到数据只能丢请求,对端永远凑不齐选中文件的末 piece。
func TestPartialSeedServesSelected(t *testing.T) {
	root, f1 := buildTwoFileTree(t)
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	want := meta.WantedPiecesFor([]string{"f1.bin"})
	fw, err := meta.FileWantedFor([]string{"f1.bin"})
	if err != nil {
		t.Fatal(err)
	}
	pdst, err := os.MkdirTemp("", "zdpart-partial")
	if err != nil {
		t.Fatal(err)
	}
	ldst, err := os.MkdirTemp("", "zdpart-leech")
	if err != nil {
		t.Fatal(err)
	}
	var partial, leech *Client
	t.Cleanup(func() {
		partial.Close()
		leech.Close()
		os.RemoveAll(pdst)
		os.RemoveAll(ldst)
	})

	// 全量源:供 partial 先把 f1 下载齐
	seed, err := NewSeedClient(meta, root, ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	partial, err = NewClient(meta, pdst, ClientConfig{MaxPeers: 8, WantedPieces: want, FileWanted: fw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	partial.Start()
	if err := partial.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, partial)
	seed.Close() // 移除全量源:之后 leech 只能从 partial 拿数据

	// 机制断言:跨界 piece 中属于未选邻居 f2 的头部字节已随下载落盘
	// (f2.bin.!zd 只含真实跨界字节),做种服务才有数据可读
	var edge []byte
	edge, err = os.ReadFile(filepath.Join(pdst, meta.Name, "f2.bin"+IncompleteSuffix))
	if err != nil || len(edge) == 0 {
		t.Fatalf("未选邻居的边界字节未落盘: err=%v len=%d", err, len(edge))
	}

	leech, err = NewClient(meta, ldst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	if err := leech.AddPeerAddr(partial.ListenAddr()); err != nil {
		t.Fatal(err)
	}

	src, err := os.ReadFile(f1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ldst, meta.Name)
	deadline := time.Now().Add(30 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got, err = os.ReadFile(filepath.Join(dir, "f1.bin"))
		if err != nil {
			got, err = os.ReadFile(filepath.Join(dir, "f1.bin"+IncompleteSuffix))
		}
		if err == nil && len(got) == len(src) && bytes.Equal(src, got) {
			if up := partial.Stats().Uploaded; up < int64(len(src)) {
				t.Fatalf("partial 上传量异常: %d 字节", up)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("30s 内 leech 未取齐 f1: got %d/%d 字节 (err=%v, partial 上传 %d 字节)",
		len(got), len(src), err, partial.Stats().Uploaded)
}
