package bt

import (
	"os"
	"testing"
	"time"
)

// TestStorageCloseRacesWriteBlock 回归:暂停路径 Storage.Close 持锁把 pbufs
// 置 nil,而在途连接的最后一块仍会并发进入 WriteBlock——旧实现在锁外预检查
// `s.pbufs != nil` 通过后,锁内对已置 nil 的 map 赋值,panic "assignment to
// entry in nil map"(实测暂停瞬间的崩溃栈即此)。修复后 Close 之后到达的块
// 自动转直写磁盘,不允许 panic;反复迭代并以 -race 校验无数据竞争。
func TestStorageCloseRacesWriteBlock(t *testing.T) {
	srcRoot, _ := buildTestTree(t)
	_, meta, err := BuildTorrentFromDisk(srcRoot, "http://t/announce", "", 16384)
	if err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 120; iter++ {
		dst := t.TempDir()
		st, err := OpenStorage(meta, dst, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, BlockSize)
			for piece := 0; piece < meta.PieceCount(); piece++ {
				_ = st.WriteBlock(piece, 0, buf)
			}
		}()
		// 每轮错开 Close 相对写块的时机:覆盖"检查前/检查后/写盘中"
		time.Sleep(time.Duration(iter%7) * time.Millisecond)
		st.Close()
		<-done
		st.Close() // 懒重开的直写句柄也要关掉,否则 TempDir 清理在 Windows 上失败
		_ = os.RemoveAll(dst)
	}
}
