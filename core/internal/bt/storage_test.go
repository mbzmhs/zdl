package bt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestStorageFullFlow 用源目录做种数据,经 Storage 逐块写入到另一个目录,
// 全部校验通过后 Finalize,比对新目录内容与源目录逐字节一致。
func TestStorageFullFlow(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	data, meta, err := BuildTorrentFromDisk(srcRoot, "http://t/announce", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	_ = data

	dst := t.TempDir()
	st, err := OpenStorage(meta, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 逐 piece:从源目录读原始内容,按 16KiB 块写入
	for piece := 0; piece < meta.PieceCount(); piece++ {
		begin := 0
		for int64(begin) < meta.PieceLen(piece) {
			length := int64(BlockSize)
			if rest := meta.PieceLen(piece) - int64(begin); rest < length {
				length = rest
			}
			buf := make([]byte, length)
			if err := readDiskRange(meta.Multi, srcRoot, meta.Files, int64(piece)*meta.PieceLength+int64(begin), buf); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteBlock(piece, uint32(begin), buf); err != nil {
				t.Fatal(err)
			}
			begin += int(length)
		}
		if !st.VerifyPiece(piece) {
			t.Fatalf("piece %d 校验失败", piece)
		}
	}
	// 未写满的 piece 应校验失败(预分配的零块 ≠ 期望哈希)
	final, err := st.Finalize()
	if err != nil {
		t.Fatal(err)
	}

	// 校验落定后的文件内容
	if meta.Multi {
		if filepath.Base(final) != meta.Name || filepath.Ext(final) != "" {
			t.Fatalf("最终目录: %q", final)
		}
		if _, err := os.Stat(filepath.Join(final, "empty.dat")); err != nil {
			t.Fatal("零长文件应存在")
		}
		for _, f := range meta.Files {
			want, err := os.ReadFile(filepath.Join(srcRoot, filepath.Join(f.Path...)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(final, filepath.Join(f.Path...)))
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(got) {
				t.Fatalf("文件 %v 内容不符", f.Path)
			}
		}
		// 就地布局:旧临时形态已成历史,不应残留 .zdlbt 目录或 .!zd 标记
		if _, err := os.Stat(filepath.Join(dst, meta.Name+".zdlbt")); !os.IsNotExist(err) {
			t.Fatal("旧临时目录不应存在")
		}
		if HasIncompleteMark(final) {
			t.Fatal("全部完成后不应残留 .!zd 标记")
		}
	} else {
		got, err := os.ReadFile(final)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(srcRoot)
		if string(want) != string(got) {
			t.Fatal("单文件内容不符")
		}
	}
}

// TestStorageInPlaceFileCompletion 就地布局核心约定:未完成文件带 .!zd
// 后缀;某文件的全部 piece 校验通过后立即恢复正式名(边下边用),其余
// 文件保持标记。
func TestStorageInPlaceFileCompletion(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Multi || len(meta.Files) < 2 {
		t.Fatalf("需多文件种子,实际 Multi=%v files=%d", meta.Multi, len(meta.Files))
	}
	dst := t.TempDir()
	st, err := OpenStorage(meta, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 全新任务:所有非零长文件都应带 .!zd 标记
	for _, f := range meta.Files {
		if f.Length == 0 {
			continue
		}
		marked := filepath.Join(dst, meta.Name, filepath.Join(f.Path...)) + IncompleteSuffix
		if _, err := os.Stat(marked); err != nil {
			t.Fatalf("未完成文件应带标记: %v", f.Path)
		}
	}

	// 只写并校验第一个文件的 piece:该文件恢复正式名,其余仍带标记
	first, last := meta.FilePieceRange(0)
	for piece := first; piece <= last; piece++ {
		for begin := 0; int64(begin) < meta.PieceLen(piece); begin += BlockSize {
			length := int64(BlockSize)
			if rest := meta.PieceLen(piece) - int64(begin); rest < length {
				length = rest
			}
			buf := make([]byte, length)
			if err := readDiskRange(meta.Multi, srcRoot, meta.Files, int64(piece)*meta.PieceLength+int64(begin), buf); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteBlock(piece, uint32(begin), buf); err != nil {
				t.Fatal(err)
			}
		}
		if !st.VerifyPiece(piece) {
			t.Fatalf("piece %d 校验失败", piece)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, meta.Name, filepath.Join(meta.Files[0].Path...))); err != nil {
		t.Fatal("完成的文件应去掉 .!zd 后缀")
	}
	for _, f := range meta.Files[1:] {
		if f.Length == 0 {
			continue
		}
		marked := filepath.Join(dst, meta.Name, filepath.Join(f.Path...)) + IncompleteSuffix
		if _, err := os.Stat(marked); err != nil {
			t.Fatalf("未完成文件应保持标记: %v", f.Path)
		}
	}
}

// TestStorageLegacyMigration 旧版临时形态自动迁移为就地布局:
// 多文件 <名>.zdlbt/ → <名>/(未完成文件补 .!zd),单文件 .zdl.part → <名>.!zd。
func TestStorageLegacyMigration(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	// 伪造旧多文件临时目录:数据裸名放在 <名>.zdlbt/ 里
	legacy := filepath.Join(dst, meta.Name+".zdlbt")
	for _, f := range meta.Files {
		p := filepath.Join(legacy, filepath.Join(f.Path...))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		want := make([]byte, f.Length)
		if f.Length > 0 {
			if err := readDiskRange(true, srcRoot, meta.Files, f.Offset, want); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(p, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := OpenStorage(meta, dst, nil, nil) // have=nil:全部视为未完成
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("旧临时目录应已迁移")
	}
	if _, err := os.Stat(filepath.Join(dst, meta.Name)); err != nil {
		t.Fatal("就地数据目录应存在")
	}
	if !HasIncompleteMark(filepath.Join(dst, meta.Name)) {
		t.Fatal("位图全空时文件应带 .!zd 标记")
	}
}

// TestStorageLegacyMigrationSingle 单文件旧临时件迁移:数据保持可校验。
func TestStorageLegacyMigrationSingle(t *testing.T) {
	src := filepath.Join(t.TempDir(), "only.bin")
	content := []byte("legacy part data")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(src, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	old := filepath.Join(dst, meta.Name+".zdl.part")
	if err := os.WriteFile(old, content, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStorage(meta, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("旧 .zdl.part 应已迁移")
	}
	if _, err := os.Stat(filepath.Join(dst, meta.Name) + IncompleteSuffix); err != nil {
		t.Fatal("迁移后应为 <名>.!zd 形态")
	}
	if !st.VerifyPiece(0) {
		t.Fatal("迁移后的数据应保持可校验")
	}
}

func TestStoragePartialVerifyFails(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStorage(meta, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// 什么都没写:piece0 应校验失败
	if st.VerifyPiece(0) {
		t.Fatal("空数据不应通过校验")
	}
	// 写入正确数据后应通过
	buf := make([]byte, meta.PieceLen(0))
	if err := readDiskRange(meta.Multi, srcRoot, meta.Files, 0, buf); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBlock(0, 0, buf); err != nil {
		t.Fatal(err)
	}
	if !st.VerifyPiece(0) {
		t.Fatal("正确数据应通过校验")
	}
}

// TestStoragePieceBufSpill 攒块缓冲超限时整片溢写、后续块直写磁盘,
// 最终数据与校验仍须正确;校验失败的缓冲被丢弃后可重下恢复。
func TestStoragePieceBufSpill(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	st, err := OpenStorage(meta, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.pbMu.Lock()
	st.pbCap = 1 << 20 // 强制溢写:1MB 预算 < 总量
	st.pbMu.Unlock()

	writePieceFromSrc := func(piece int) error {
		for begin := 0; int64(begin) < meta.PieceLen(piece); begin += BlockSize {
			length := int64(BlockSize)
			if rest := meta.PieceLen(piece) - int64(begin); rest < length {
				length = rest
			}
			buf := make([]byte, length)
			if err := readDiskRange(meta.Multi, srcRoot, meta.Files, int64(piece)*meta.PieceLength+int64(begin), buf); err != nil {
				return err
			}
			if err := st.WriteBlock(piece, uint32(begin), buf); err != nil {
				return err
			}
		}
		return nil
	}

	if meta.PieceCount() < 2 {
		t.Fatalf("测试种子至少需要 2 个 piece,实际 %d", meta.PieceCount())
	}
	// piece 0:先写坏数据制造校验失败,再写正确数据恢复
	for begin := 0; int64(begin) < meta.PieceLen(0); begin += BlockSize {
		length := int64(BlockSize)
		if rest := meta.PieceLen(0) - int64(begin); rest < length {
			length = rest
		}
		bad := bytes.Repeat([]byte{0xA5}, int(length))
		if err := st.WriteBlock(0, uint32(begin), bad); err != nil {
			t.Fatal(err)
		}
	}
	if st.VerifyPiece(0) {
		t.Fatal("坏数据不应通过校验")
	}
	if err := writePieceFromSrc(0); err != nil {
		t.Fatal(err)
	}
	if !st.VerifyPiece(0) {
		t.Fatal("重写正确数据后应通过校验")
	}
	// 其余 piece:正常写入(触发溢写路径)
	for piece := 1; piece < meta.PieceCount(); piece++ {
		if err := writePieceFromSrc(piece); err != nil {
			t.Fatal(err)
		}
		if !st.VerifyPiece(piece) {
			t.Fatalf("piece %d 溢写路径校验失败", piece)
		}
	}
	if _, err := st.Finalize(); err != nil {
		t.Fatal(err)
	}
	// 最终落盘内容须与源一致(多文件种子最终形态是目录)
	if meta.Multi {
		for _, f := range meta.Files {
			gotB, err := os.ReadFile(filepath.Join(dst, meta.Name, filepath.Join(f.Path...)))
			if err != nil {
				t.Fatal(err)
			}
			wantB := make([]byte, f.Length)
			if f.Length > 0 {
				// 文件内容经全局偏移从源树读取
				if err := readDiskRange(true, srcRoot, meta.Files, f.Offset, wantB); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(gotB, wantB) {
				t.Fatalf("文件 %v 内容与源不一致", f.Path)
			}
		}
		return
	}
	got, err := os.ReadFile(filepath.Join(dst, meta.Name))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, meta.TotalLength)
	if err := readDiskRange(false, srcRoot, meta.Files, 0, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("落盘内容与源不一致: got %d bytes want %d bytes", len(got), len(want))
	}
}

// TestStorageUnwantedEdgeBytes 未选文件不预分配(打开存储本身不创建);
// 跨界 piece 的边界字节以 .!zd 形态随写落盘、内容与源一致——做种服务
// 该 piece 时必须能原样读出,否则对端永远凑不齐选中文件的末 piece(见
// TestPartialSeedServesSelected)。扫描式读(改选重启的恢复路径)仍绝不
// 创建文件:未落盘区域按零填充。
func TestStorageUnwantedEdgeBytes(t *testing.T) {
	// 三文件树:选 a.txt 写 piece0(跨界 b.bin 头部);c.bin 全程不写
	srcRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b1 := make([]byte, 70_000)
	c1 := make([]byte, 30_000)
	for i := range b1 {
		b1[i] = byte(i*7 + 1)
	}
	for i := range c1 {
		c1[i] = byte(i*5 + 3)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "sub", "b.bin"), b1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "sub", "c.bin"), c1, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Multi || len(meta.Files) < 3 {
		t.Fatalf("需三文件种子")
	}
	dst := t.TempDir()
	fw := make([]bool, len(meta.Files))
	fw[0] = true // 只选 a.txt
	st, err := OpenStorage(meta, dst, fw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bin := filepath.Join(dst, meta.Name, "sub", "b.bin")
	cin := filepath.Join(dst, meta.Name, "sub", "c.bin")

	// 打开存储不创建未选文件(不预分配)
	if _, err := os.Stat(bin + IncompleteSuffix); !os.IsNotExist(err) {
		t.Fatal("打开存储不应创建未选文件")
	}

	// 写入跨界 piece0(覆盖 b.bin 头部):校验用内存数据,应通过;
	// b.bin 的边界字节以 .!zd 形态落盘,内容与源逐字节一致
	for begin := 0; int64(begin) < meta.PieceLen(0); begin += BlockSize {
		length := int64(BlockSize)
		if rest := meta.PieceLen(0) - int64(begin); rest < length {
			length = rest
		}
		buf := make([]byte, length)
		if err := readDiskRange(meta.Multi, srcRoot, meta.Files, int64(begin), buf); err != nil {
			t.Fatal(err)
		}
		if err := st.WriteBlock(0, uint32(begin), buf); err != nil {
			t.Fatal(err)
		}
	}
	if !st.VerifyPiece(0) {
		t.Fatal("piece0 校验失败")
	}
	edge, err := os.ReadFile(bin + IncompleteSuffix)
	if err != nil {
		t.Fatalf("跨界字节应随写落盘(.!zd): %v", err)
	}
	if len(edge) == 0 {
		t.Fatal("落盘的边界字节不应为空")
	}
	wantHead := make([]byte, len(edge))
	if err := readDiskRange(meta.Multi, srcRoot, meta.Files, 5, wantHead); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(edge, wantHead) {
		t.Fatal("落盘的边界字节应与源内容一致")
	}
	// 做种服务路径:边界 piece 现在能原样读出(数据完整,不丢请求)
	got := make([]byte, 16)
	if err := st.ReadForUpload(5, got); err != nil {
		t.Fatalf("边界字节落盘后做种读取应成功: %v", err)
	}
	if !bytes.Equal(got, wantHead[:16]) {
		t.Fatal("做种读出的边界字节应与源一致")
	}

	// 扫描式读(恢复路径)对从未落盘的 c.bin 区间按零填充且不创建文件
	buf := make([]byte, 64)
	if err := st.readFileRange(meta.PieceOffset(5)+100, buf, false); err != nil {
		t.Fatalf("非严格读应按零填充: %v", err)
	}
	for _, b := range buf {
		if b != 0 {
			t.Fatal("未落盘区域应读到零")
		}
	}
	if _, err := os.Stat(cin + IncompleteSuffix); !os.IsNotExist(err) {
		t.Fatal("扫描读不应创建未选文件")
	}

	// 严格读(上传服务路径):未落盘按错误,绝不发零块
	if err := st.readFileRange(meta.PieceOffset(5)+100, make([]byte, 16), true); err == nil {
		t.Fatal("严格读未落盘文件应报错")
	}
}

// TestOpenStorageCleansZeroByteUnselected 旧版本遗留的 0 字节未选 .!zd
// 残留在重新打开时自愈删除;带真实边界字节的保留(扩选可复用)。
func TestOpenStorageCleansZeroByteUnselected(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "", "", 16384)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	fw := make([]bool, len(meta.Files))
	fw[0] = true // 只选 a.txt;b.bin 未选
	bin := filepath.Join(dst, meta.Name, "sub", "b.bin")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+IncompleteSuffix, nil, 0o644); err != nil { // 0 字节残留
		t.Fatal(err)
	}
	st, err := OpenStorage(meta, dst, fw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(bin + IncompleteSuffix); !os.IsNotExist(err) {
		t.Fatal("0 字节未选残留应被清理")
	}

	// 非零残留(真实边界数据)保留
	if err := os.WriteFile(bin+IncompleteSuffix, []byte("real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStorage(meta, dst, fw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := os.Stat(bin + IncompleteSuffix); err != nil {
		t.Fatal("非零未选文件应保留")
	}
}
