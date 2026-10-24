package bt

// 块粒度流播的存储层 API:内存攒块的块级到货位图与直读。
// 覆盖:乱序到货位图、直读字节比对、未到货块拒绝、校验取走缓冲后的
// 回退(piece 级盘读)。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func streamBlockSource(t *testing.T) (root string, content []byte, meta *MetaInfo) {
	t.Helper()
	root = t.TempDir()
	content = make([]byte, 256<<10) // 4 片 × 64KB,每片 4 块
	for i := range content {
		content[i] = byte(i*13 + i/997)
	}
	if err := os.WriteFile(filepath.Join(root, "v.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(root, "", "", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	return root, content, meta
}

func TestStorageStreamBlock(t *testing.T) {
	_, content, meta := streamBlockSource(t)
	st, err := OpenStorage(meta, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 未写入:块状态不可知
	if _, ok := st.StreamBlockState(0); ok {
		t.Fatal("未开始的 piece 不应有块级状态")
	}
	// 乱序写入块 0 与块 2(piece 0)
	for _, b := range []int{0, 2} {
		if err := st.WriteBlock(0, uint32(b*BlockSize), content[b*BlockSize:(b+1)*BlockSize]); err != nil {
			t.Fatal(err)
		}
	}
	have, ok := st.StreamBlockState(0)
	if !ok || len(have) != 4 || !have[0] || have[1] || !have[2] || have[3] {
		t.Fatalf("块位图应为 [1,0,1,0],得到 %v ok=%v", have, ok)
	}
	// 直读已到货块:字节一致
	dst := make([]byte, BlockSize)
	if !st.StreamBlockRead(0, 0, dst) || !bytes.Equal(dst, content[:BlockSize]) {
		t.Fatal("块 0 直读数据不符")
	}
	if !st.StreamBlockRead(0, 2*BlockSize, dst) || !bytes.Equal(dst, content[2*BlockSize:3*BlockSize]) {
		t.Fatal("块 2 直读数据不符")
	}
	// 块内偏移直读(不跨块)
	if !st.StreamBlockRead(0, 100, dst[:2000]) || !bytes.Equal(dst[:2000], content[100:2100]) {
		t.Fatal("块内偏移直读不符")
	}
	// 未到货块:拒绝
	if st.StreamBlockRead(0, BlockSize, dst) {
		t.Fatal("未到货块不应可直读")
	}
	// 补齐块 1、3 → 校验通过 → 缓冲被取走:块级状态消失,盘读可得全片
	for _, b := range []int{1, 3} {
		if err := st.WriteBlock(0, uint32(b*BlockSize), content[b*BlockSize:(b+1)*BlockSize]); err != nil {
			t.Fatal(err)
		}
	}
	if !st.VerifyPiece(0) {
		t.Fatal("piece 0 校验应通过")
	}
	if _, ok := st.StreamBlockState(0); ok {
		t.Fatal("校验取走缓冲后不应再有块级状态")
	}
	if st.StreamBlockRead(0, 0, dst) {
		t.Fatal("缓冲取走后直读应失败(调用方转盘读)")
	}
	full := make([]byte, 64<<10)
	if err := st.readFileRange(0, full, false); err != nil || !bytes.Equal(full, content[:64<<10]) {
		t.Fatal("盘读全片数据不符")
	}
}
