package bt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitDone(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.WaitComplete(ctx); err != nil {
		t.Fatalf("下载未完成: %v", err)
	}
}

// TestSelfPlayMultiFile:做种端(源目录)→ 下载端,多文件种子全链路。
func TestSelfPlayMultiFile(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := NewSeedClient(meta, srcRoot, ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	dst := t.TempDir()
	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, leech)

	// 逐文件比对内容
	for _, f := range meta.Files {
		want, err := os.ReadFile(filepath.Join(srcRoot, filepath.Join(f.Path...)))
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
	if leech.FinalPath() != filepath.Join(dst, meta.Name) {
		t.Fatalf("最终路径: %q", leech.FinalPath())
	}
	if _, err := os.Stat(filepath.Join(dst, meta.Name+".zdlbt")); !os.IsNotExist(err) {
		t.Fatal("旧临时目录不应存在")
	}
	if seed.Stats().Uploaded == 0 {
		t.Fatal("做种端应统计到上传量")
	}
}

// TestSelfPlaySingleFileWithResume:单文件 2 piece,先写好 piece0 再续传。
func TestSelfPlaySingleFileWithResume(t *testing.T) {
	root := filepath.Join(t.TempDir(), "only.bin")
	content := make([]byte, 30000)
	for i := range content {
		content[i] = byte(i * 7)
	}
	if err := os.WriteFile(root, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PieceCount() != 2 {
		t.Fatalf("预期 2 piece: %d", meta.PieceCount())
	}

	seed, err := NewSeedClient(meta, root, ClientConfig{MaxPeers: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	// 预写 piece0,模拟上次中断
	dst := t.TempDir()
	st, err := OpenStorage(meta, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, meta.PieceLen(0))
	if err := readDiskRange(false, root, meta.Files, 0, buf); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBlock(0, 0, buf); err != nil {
		t.Fatal(err)
	}
	// 真实完成流程里 piece 校验通过才落盘、位图才会标记;此处对齐:
	// 未过校验的数据(仅在攒块缓冲里)不应被续传信任
	if !st.VerifyPiece(0) {
		t.Fatal("预写 piece0 校验失败")
	}
	st.Close()

	leech, err := NewClient(meta, dst, ClientConfig{MaxPeers: 4}, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()

	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, leech)

	got, err := os.ReadFile(leech.FinalPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatal("续传后内容不符")
	}
	// piece0 不应重复下载:下载端只应收到 piece1
	if leech.Stats().Downloaded > meta.PieceLen(1)*2 {
		t.Fatalf("下载量异常: %d", leech.Stats().Downloaded)
	}
}

// TestSelfPlayRateLimited:上传限速生效(10KB/s 上传 1 piece 约 1.6s)。
func TestSelfPlayRateLimited(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r.bin")
	content := make([]byte, 16384*4) // 4 piece
	for i := range content {
		content[i] = byte(i >> 4)
	}
	if err := os.WriteFile(root, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := NewSeedClient(meta, root, ClientConfig{MaxPeers: 4, UpLimit: 10 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	defer seed.Close()

	leech, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	waitDone(t, leech)
	elapsed := time.Since(start)
	if elapsed < 4*time.Second {
		t.Fatalf("限速未生效:4 piece×16KiB/10KB/s 应≥6s,实际 %v", elapsed)
	}
}
