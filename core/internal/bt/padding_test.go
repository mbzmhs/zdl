package bt

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestIsPaddingPath 填充文件识别:BitComet 命名与 BEP-47 .pad 目录两种形态。
func TestIsPaddingPath(t *testing.T) {
	yes := [][]string{
		{"____padding_file_0___lzd"},
		{"a", "____padding_file_12___"},
		{".pad", "262144"},
		{"dir", ".pad", "16384"},
	}
	no := [][]string{
		{"big1.bin"},
		{"padding_file_0"},
		{"_padding_file_0"}, // 单下划线开头,不足 __
		{"a", ".padfile", "x"},
		{"a", "padding", "1"},
	}
	for _, p := range yes {
		if !IsPaddingPath(p) {
			t.Fatalf("%v 应识别为填充文件", p)
		}
	}
	for _, p := range no {
		if IsPaddingPath(p) {
			t.Fatalf("%v 不应识别为填充文件", p)
		}
	}
}

// TestStoragePaddingVirtualized 填充文件虚拟化全链路:BitComet 形态的填充
// 文件(全零对齐占位)绝不落盘——块写入丢弃、piece 校验读回按零合成、
// 上传读取补零,真实文件数据完好;旧版本遗留的真实填充文件在打开时清除。
func TestStoragePaddingVirtualized(t *testing.T) {
	// 制种:真实文件 + BitComet 命名的填充文件(全零)+ 真实文件
	// (文件表按字母序,padding 排最前,正好覆盖"首文件即填充"的布局)
	src := t.TempDir()
	fileA := make([]byte, 2<<20)
	fileB := make([]byte, 2<<20)
	pad := make([]byte, 300<<10) // 全零
	if _, err := rand.Read(fileA); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(fileB); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"big1.bin", fileA}, {"big2.bin", fileB}, {"____padding_file_0___lzd", pad}} {
		if err := os.WriteFile(filepath.Join(src, f.name), f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, meta, err := BuildTorrentFromDisk(src, "", "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	padIdx := -1
	srcs := make([][]byte, len(meta.Files))
	for i, f := range meta.Files {
		name := f.Path[len(f.Path)-1]
		if IsPaddingPath(f.Path) {
			padIdx = i
			srcs[i] = pad // 填充文件:内容全零
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		srcs[i] = b
	}
	if padIdx < 0 {
		t.Fatal("种子应含填充文件")
	}
	srcAt := func(off int64) byte { // 种子字节流全局偏移 → 源字节(填充为零)
		for i, f := range meta.Files {
			if off < f.Offset || off >= f.Offset+f.Length {
				continue
			}
			if IsPaddingPath(f.Path) {
				return 0
			}
			return srcs[i][off-f.Offset]
		}
		panic("偏移越界")
	}

	dest := t.TempDir()
	st, err := OpenStorage(meta, dest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	padPath := filepath.Join(dest, meta.Name, meta.Files[padIdx].Path[len(meta.Files[padIdx].Path)-1])
	if _, err := os.Stat(padPath); !os.IsNotExist(err) {
		t.Fatal("填充文件不应被创建")
	}

	// 模拟完整下载:按块写入(填充区段也照常投递——对端发来的就是零),
	// 逐 piece 校验。校验读回时填充段按零合成,必须与制种哈希一致。
	for p := 0; p < meta.PieceCount(); p++ {
		plen := meta.PieceLen(p)
		off := meta.PieceOffset(p)
		for b := int64(0); b < plen; b += BlockSize {
			n := min(BlockSize, plen-b)
			chunk := make([]byte, n)
			for i := int64(0); i < n; i++ {
				chunk[i] = srcAt(off + b + i)
			}
			if err := st.WriteBlock(p, uint32(b), chunk); err != nil {
				t.Fatal(err)
			}
		}
		if !st.VerifyPiece(p) {
			t.Fatalf("piece %d 校验应通过(填充段按零合成)", p)
		}
	}
	if _, err := os.Stat(padPath); !os.IsNotExist(err) {
		t.Fatal("下载完成后填充文件仍不应存在")
	}

	// 真实文件内容逐字节完好
	for i, f := range meta.Files {
		if IsPaddingPath(f.Path) {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dest, meta.Name, filepath.Join(f.Path...)))
		if err != nil || !bytes.Equal(got, srcs[i]) {
			t.Fatalf("%s 数据不符(err=%v)", f.Path, err)
		}
	}

	// 做种读取跨界区段(strict):填充尾段(零)+ 真实头段,绝不能报
	// "文件未落盘";填充在前,正好覆盖 pad→big1 边界
	padF := meta.Files[padIdx]
	spanOff := padF.Offset + padF.Length - 100<<10
	sp := make([]byte, 200<<10)
	if err := st.ReadForUpload(spanOff, sp); err != nil {
		t.Fatalf("跨界读取填充区不应报错: %v", err)
	}
	for i := int64(0); i < int64(len(sp)); i++ {
		if want := srcAt(spanOff + i); sp[i] != want {
			t.Fatalf("跨界读取内容不符 @%d: got %d want %d", i, sp[i], want)
		}
	}

	// 旧版本遗留的真实填充文件(正式名与 .!zd 形态)在打开时清除
	if err := os.WriteFile(padPath, pad, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(padPath+IncompleteSuffix, pad, 0o644); err != nil {
		t.Fatal(err)
	}
	have := make([]bool, meta.PieceCount())
	for i := range have {
		have[i] = true // 生产重开语义:位图全持有,已完成文件不再补后缀
	}
	st2, err := OpenStorage(meta, dest, nil, have)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st2.Close)
	if _, err := os.Stat(padPath); !os.IsNotExist(err) {
		t.Fatal("遗留的正式名填充文件应被清除")
	}
	if _, err := os.Stat(padPath + IncompleteSuffix); !os.IsNotExist(err) {
		t.Fatal("遗留的 .!zd 填充文件应被清除")
	}
}
