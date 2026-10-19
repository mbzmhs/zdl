package bt

import (
	"bytes"
	"testing"

	"zdownloader/internal/limiter"
)

// TestSnapshotBlocksFlushesFirst 回归:块级位图持久化必须保证"位图里的块
// 都在盘上"。旧实现把仅存在内存攒块缓冲里的块也写进位图且不落盘,强杀/
// 崩溃后缓冲丢失、位图照信,重启恢复出"账面有、盘上是零"的幻影块——
// piece 以零块凑数反复校验失败(实测同一 piece 换批 peer 仍失败,下载
// 死循环、正常 peer 被误封)。修复 = 快照前整片溢写落盘。
func TestSnapshotBlocksFlushesFirst(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "http://t/announce", "", 65536) // 64K piece = 4 块
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(meta, t.TempDir(), ClientConfig{GlobalDown: limiter.New(0)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	st := c.store.Load()
	if st == nil {
		t.Fatal("store 未初始化")
	}

	// 向 piece 0 写 3 个块(进内存攒块缓冲,未溢写)并登记到 picker
	const piece = 0
	blk := bytes.Repeat([]byte{0xAB}, BlockSize)
	for b := 0; b < 3; b++ {
		if err := st.WriteBlock(piece, uint32(b*BlockSize), blk); err != nil {
			t.Fatal(err)
		}
		if isNew, _ := c.picker.RecordBlock(piece, uint32(b*BlockSize)); !isNew {
			t.Fatalf("块 %d 登记被拒", b)
		}
	}

	// 快照:必须先把缓冲溢写落盘,再导出位图(不变量:账里的块都在盘上)
	snap := c.SnapshotBlocks()
	stride := (int(meta.PieceLength+BlockSize-1)/BlockSize + 7) / 8
	seg := snap[piece*stride : (piece+1)*stride]
	for b := 0; b < 3; b++ {
		if seg[b/8]&(1<<(7-uint(b%8))) == 0 {
			t.Fatalf("块 %d 已落盘,应入账", b)
		}
	}
	if seg[3/8]&(1<<(7-uint(3%8))) != 0 {
		t.Fatal("未交付的块不应入账")
	}
	// 盘上校验:快照后数据必须真的在磁盘(读回与写入内容一致)
	got := make([]byte, 3*BlockSize)
	if err := st.readFileRange(0, got, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat(blk, 3)) {
		t.Fatal("入账的块未落盘(盘上数据与账目不符)")
	}
}
