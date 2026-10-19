package bt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 复现:默认 256KiB piece(单 piece 多块)的自我的对拍是否卡死。
func TestSelfPlayLargePieces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "big.bin")
	content := make([]byte, 300000)
	for i := range content {
		content[i] = byte(i * 7)
	}
	if err := os.WriteFile(root, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 0)
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

	leech, err := NewClient(meta, t.TempDir(), ClientConfig{MaxPeers: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leech.Start()
	defer leech.Close()
	if err := leech.AddPeerAddr(seed.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := leech.WaitComplete(ctx); err != nil {
		t.Fatalf("未完成: %v (done=%d/%d)", err, leech.PiecesDone(), meta.PieceCount())
	}
}
