package bt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Storage 管理一个 BT 任务的落盘。
// 就地落盘布局:数据从第一块起就写在最终路径上,用户可以边下载边使用
// 已完成的文件,路径自始至终不变。未完成的文件带 .!zd 后缀标记;其覆盖的
// piece 全部校验通过后立即去掉后缀恢复正式文件名(逐文件完成,不必等整
// 个种子)。旧版临时形态(<名>.zdlbt 目录 / <名>.zdl.part)在打开时自动迁移。
// IncompleteSuffix 未完成文件的后缀标记(就地布局:完成的文件恢复正式名)。
const IncompleteSuffix = ".!zd"

// Storage 负责按全局偏移读写 piece 数据。多文件种子数据目录 = <下载目录>/<名>/,
// 单文件 = <下载目录>/<名>[.!zd]。
type Storage struct {
	meta     *MetaInfo
	dir      string // 下载目录
	partPath string // 兼容字段:与 final 相同(就地布局没有独立临时形态)
	final    string // 最终路径(单文件为文件路径,多文件为数据目录)
	fhs      []*os.File
	fhMu     sync.RWMutex // 保护 fhs:逐文件完成改名会关/开句柄,读写路径持读锁
	ro       bool         // 只读形态(做种服务):缺文件按错误处理,不惰性创建
	scratch  []byte       // 校验用缓冲(vmu 保护)
	vmu      sync.Mutex

	// 内存攒块:块先进内存,piece 凑齐后在内存里过 SHA-1,通过才一次性
	// 落盘——省掉"每块 16KB 随机写 + 整件读回"两趟 I/O。总缓冲超限或
	// 磁盘写路径复杂化时整体溢写(此后该 piece 的块直写磁盘,行为同旧)。
	pbMu    sync.Mutex
	pbufs   map[int]*pieceBuf
	pbBytes int64
	pbCap   int64

	// 逐文件完成跟踪:piece 校验通过即计数,某文件的全部 piece 齐了就
	// 摘掉 .!zd 后缀(见 MarkPiecesVerified)。记账按 (文件,piece) 二维
	// 进行:边界 piece 被多个文件共享,某文件先完成时其 piece 的全局标记
	// 不能挡住别的文件对同一 piece 的计数(实测 bin.dat 因此永远差 1)。
	fxMu       sync.Mutex
	fileFirst  []int    // 每文件起始 piece(下标换算用)
	fileLeft   []int    // 每文件剩余未校验 piece 数
	fileGot    [][]bool // 每文件已计数 piece(相对 first 的下标)
	fileDone   []bool   // 每文件是否已恢复正式文件名
	pieceFiles [][]int  // piece → 覆盖的文件下标(v1 边界 piece 为 2 个)

	// pad 填充文件掩码(与 Files 对齐):填充文件内容保证全零,仅作 piece
	// 对齐占位——绝不落盘(写丢弃、读按零、不建句柄),开任务时顺手清掉
	// 旧版本遗留的真实文件(见 removePaddingLeftovers)。
	pad []bool
}

const pieceBufCap = 64 << 20 // 攒块总预算(约等于 60 peer × 1MB 在途的到达速率)

type pieceBuf struct {
	buf   []byte
	have  []bool // 按块下标
	nHave int
	spill bool // 已整片溢写到磁盘,后续块直写
}

func newPieceBuf(plen int64, blocks int) *pieceBuf {
	return &pieceBuf{buf: make([]byte, plen), have: make([]bool, blocks)}
}

// openSeedingStorage 以只读方式打开最终路径的数据(做种服务用)。
// finalPath 为单文件文件路径或多文件数据目录。文件选择任务里被排除的
// 文件按 .!zd 后缀回退打开;从未落盘的未选文件留空句柄(ro 形态不创建),
// 只有当某 piece 真正读到它时才报错——需求内的 piece 不会碰到这种情况。
func openSeedingStorage(meta *MetaInfo, finalPath string) (*Storage, error) {
	s := &Storage{
		meta:     meta,
		dir:      filepath.Dir(finalPath),
		partPath: finalPath,
		final:    finalPath,
		ro:       true,
		fhs:      make([]*os.File, len(meta.Files)),
	}
	s.pad = meta.paddingMask()
	n := len(meta.Files)
	if n == 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		if s.pad[i] {
			continue // 填充文件虚拟化:读取按零,不碰磁盘
		}
		var path string
		if meta.Multi && len(meta.Files) > 0 {
			path = filepath.Join(finalPath, filepath.Join(meta.Files[i].Path...))
		} else {
			path = finalPath
		}
		fh, err := os.Open(path)
		if err != nil {
			fh, err = os.Open(path + IncompleteSuffix)
		}
		if err != nil {
			if _, serr := os.Stat(path); os.IsNotExist(serr) {
				continue // 未选文件从未落盘:留给惰性错误处理
			}
			s.Close()
			return nil, err
		}
		s.fhs[i] = fh
	}
	return s, nil
}

// OpenStorage 打开(或创建)任务的就地落盘存储。have 为续传位图(nil = 全新
// 下载):决定各文件初始是否带 .!zd 后缀。fileWanted 为文件需求位图(nil = 全部):
// 不需要的文件只创建零长句柄、不做预分配(其数据本就不会被写入,只可能
// 因跨界 piece 获得少量头部字节)。旧版临时形态自动迁移为就地布局。
func OpenStorage(meta *MetaInfo, downloadDir string, fileWanted, have []bool) (*Storage, error) {
	s := &Storage{
		meta:  meta,
		dir:   downloadDir,
		pbufs: map[int]*pieceBuf{},
		pbCap: pieceBufCap,
	}
	if meta.Multi {
		s.partPath = filepath.Join(downloadDir, meta.Name)
		s.final = s.partPath
	} else {
		s.partPath = filepath.Join(downloadDir, meta.Name)
		s.final = s.partPath
	}
	s.pad = meta.paddingMask()
	s.migrateLegacy(downloadDir)
	// 填充文件历史残留:旧版本把它们当普通文件下载落盘(BEP-47/BitComet
	// 对齐占位,内容全零),虚拟化后读取不再经过磁盘,真实文件只会白占空间
	s.removePaddingLeftovers()
	// 逐文件完成跟踪:位图全持有的文件视为已完成(去掉后缀),其余带 .!zd
	s.initFileTracking(have)
	// 迁移遗留:就地布局下位图未持有的文件若裸名存在(旧 .zdlbt 时代数据),
	// 补上 .!zd 后缀,保证"未完成文件一律带标记"的展示约定
	if err := s.suffixIncompleteFiles(fileWanted); err != nil {
		return nil, err
	}
	// 自愈清理:旧版本曾因校验扫描/上传读路径的惰性创建,给未选文件凭空
	// 造出 0 字节 .!zd 残留(用户看到会误以为"要下载")。零字节即无数据,
	// 删除无损;带真实边界字节的保留(扩选时可复用)。
	if fileWanted != nil {
		for i := 0; i < s.fileCount(); i++ {
			if fileWanted[i] || s.fileLength(i) == 0 {
				continue // 选中文件与零长文件(做种需要它在位)不动
			}
			marked := s.dataPath(i) + IncompleteSuffix
			if fi, err := os.Stat(marked); err == nil && !fi.IsDir() && fi.Size() == 0 {
				_ = os.Remove(marked)
			}
		}
	}
	// 只创建并预分配【严格选中】的文件(零长选中文件也创建,做种与资源
	// 完整性需要它们存在)。未选文件不预分配、不撑尺寸:与选中文件共享
	// 边界 piece 的邻居,由跨界块的写入按需惰性创建(.!zd 形态,只长出
	// 真实的边界字节——做种服务该 piece 必须用到,见 writeRange)
	s.fhs = make([]*os.File, s.fileCount())
	for i := 0; i < s.fileCount(); i++ {
		if s.pad[i] {
			continue // 填充文件虚拟化:不建句柄、不预分配(读按零,写丢弃)
		}
		if fileWanted != nil && !fileWanted[i] {
			continue // 未选:句柄留空,读写路径按 fileOnDisk 分派
		}
		if err := s.ensureFile(i); err != nil {
			s.Close()
			return nil, err
		}
		if length := s.fileLength(i); length > 0 && !s.fileDone[i] {
			if err := s.fhs[i].Truncate(length); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// ensureFile 惰性打开(必要时创建)文件 i 的句柄。未选文件的 .!zd 形态
// 由跨界 piece 的边界块写入经此创建(见 writeRange)。须持 fhMu 写锁。
func (s *Storage) ensureFile(i int) error {
	if s.fhs[i] != nil {
		return nil
	}
	path := s.diskPath(i)
	if s.ro {
		if _, err := os.Stat(path); err != nil {
			return err // 只读形态(做种):未选文件从未落盘,读取即错误
		}
	}
	if s.meta.Multi {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	s.fhs[i] = fh
	return nil
}

// migrateLegacy 把旧版临时形态改名为就地布局:多文件 <名>.zdlbt/ → <名>/,
// 单文件 <名>.zdl.part → <名>.!zd(旧临时件视为未完成;数据若其实完整,
// 启动扫描逐 piece 校验通过后会自动摘掉后缀)。新旧形态并存时以新形态为准。
func (s *Storage) migrateLegacy(downloadDir string) {
	name := s.meta.Name
	if s.meta.Multi {
		old := filepath.Join(downloadDir, name+".zdlbt")
		if _, err := os.Stat(old); err != nil {
			return
		}
		if _, err := os.Stat(s.final); err == nil {
			return // 新目录已存在:旧目录留给用户处理,不覆盖
		}
		_ = os.Rename(old, s.final)
		return
	}
	old := filepath.Join(downloadDir, name+".zdl.part")
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(s.final + IncompleteSuffix); err == nil {
		return
	}
	_ = os.Rename(old, s.final+IncompleteSuffix)
}

// removePaddingLeftovers 删除填充文件的历史残留(正式名与 .!zd 形态都算)。
// 只删与种子内填充条目精确对应的路径,绝不波及用户数据;删除失败不阻断打开
// (下次打开重试)。
func (s *Storage) removePaddingLeftovers() {
	for i := range s.pad {
		if !s.pad[i] {
			continue
		}
		_ = os.Remove(s.dataPath(i))
		_ = os.Remove(s.dataPath(i) + IncompleteSuffix)
	}
}

// initFileTracking 建立 piece→文件映射与逐文件剩余计数。have 非空时,位图
// 全持有的文件直接记为已完成(后缀在 suffixIncompleteFiles 里处理)。
func (s *Storage) initFileTracking(have []bool) {
	n := s.meta.PieceCount()
	s.pieceFiles = make([][]int, n)
	s.fileDone = make([]bool, s.fileCount())
	s.fileLeft = make([]int, s.fileCount())
	s.fileFirst = make([]int, s.fileCount())
	s.fileGot = make([][]bool, s.fileCount())
	for i := 0; i < s.fileCount(); i++ {
		first, last := s.filePieceRange(i)
		if last < first { // 零长文件:无 piece,即视为完成
			s.fileDone[i] = true
			continue
		}
		s.fileFirst[i] = first
		s.fileLeft[i] = last - first + 1
		s.fileGot[i] = make([]bool, last-first+1)
		for p := first; p <= last; p++ {
			s.pieceFiles[p] = append(s.pieceFiles[p], i)
		}
		if have != nil {
			full := true
			for p := first; p <= last && full; p++ {
				if p >= len(have) || !have[p] {
					full = false
				}
			}
			if full {
				s.fileDone[i] = true
				s.fileLeft[i] = 0
			}
		}
	}
}

// suffixIncompleteFiles 给未完成但裸名存在的文件补 .!zd 后缀(旧数据迁移)。
// 已完成文件若误带后缀也顺手摘掉。
func (s *Storage) suffixIncompleteFiles(fileWanted []bool) error {
	for i := 0; i < s.fileCount(); i++ {
		bare := s.dataPath(i)
		marked := s.diskPath(i)
		if marked == bare {
			continue
		}
		_, bareExists := os.Stat(bare)
		_, markedExists := os.Stat(marked)
		if s.fileDone[i] && bareExists == nil {
			continue // 完成且裸名在位
		}
		if !s.fileDone[i] && markedExists == nil {
			continue // 未完成且已带标记
		}
		if bareExists != nil && markedExists != nil {
			continue // 两形态并存:留给用户处理,不覆盖
		}
		from, to := bare, marked
		if s.fileDone[i] {
			from, to = marked, bare
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("就地布局迁移改名失败: %w", err)
		}
	}
	return nil
}

// fileCount 单文件种子为 1,多文件为 len(meta.Files)。
func (s *Storage) fileCount() int {
	if s.meta.Multi && len(s.meta.Files) > 0 {
		return len(s.meta.Files)
	}
	return 1
}

// dataPath 文件 i 的正式路径(不带完成标记)。
func (s *Storage) dataPath(i int) string {
	if s.meta.Multi && len(s.meta.Files) > 0 {
		f := s.meta.Files[i]
		return filepath.Join(s.final, filepath.Join(f.Path...))
	}
	return s.final
}

// diskPath 文件 i 当前应在盘上的路径:未完成文件带 .!zd 后缀。
func (s *Storage) diskPath(i int) string {
	p := s.dataPath(i)
	if s.fileDone != nil && !s.fileDone[i] {
		return p + IncompleteSuffix
	}
	return p
}

// fileLength 文件 i 的长度(单文件为种子总长)。
func (s *Storage) fileLength(i int) int64 {
	if s.meta.Multi && len(s.meta.Files) > 0 {
		return s.meta.Files[i].Length
	}
	return s.meta.TotalLength
}

// filePieceRange 文件 i 覆盖的 piece 区间(单文件为全部 piece)。
func (s *Storage) filePieceRange(i int) (int, int) {
	if s.meta.Multi && len(s.meta.Files) > 0 {
		return s.meta.FilePieceRange(i)
	}
	return 0, s.meta.PieceCount() - 1
}

// PartPath 返回数据落盘形态路径(就地布局下与最终路径一致,保留兼容)。
func (s *Storage) PartPath() string { return s.partPath }

// WriteBlock 把一块数据写到全局偏移(跨文件边界自动切分)。
// 优先写入内存攒块缓冲;对应 piece 不在缓冲(超限/溢写/已关闭)时直写磁盘。
// 注意 pbufs 的 nil 判定必须在 pbMu 锁内:暂停路径 Close 持锁把 pbufs 置 nil,
// 在途连接的最后一块仍会并发进入本函数,锁外预检查通过后再 Lock 会对 nil
// map 赋值 panic(实测暂停瞬间的崩溃栈即 storage.go pbufs 赋值行)。
func (s *Storage) WriteBlock(piece int, begin uint32, data []byte) error {
	s.pbMu.Lock()
	if s.pbufs != nil {
		pb := s.pbufs[piece]
		if pb == nil && !s.overCap(int64(len(data))) {
			pb = newPieceBuf(s.meta.PieceLen(piece), s.nBlocksOf(piece))
			s.pbufs[piece] = pb
			s.pbBytes += int64(len(pb.buf))
		}
		if pb != nil && !pb.spill {
			copy(pb.buf[begin:], data)
			bi := int(begin) / BlockSize
			if bi < len(pb.have) && !pb.have[bi] {
				pb.have[bi] = true
				pb.nHave++
			}
			s.pbMu.Unlock()
			return nil
		}
	}
	s.pbMu.Unlock()
	off := s.meta.PieceOffset(piece) + int64(begin)
	return s.writeRange(off, data)
}

// overCap 报告再接纳一个 piece 缓冲是否会超出预算(须持 pbMu)。
// 超出时对已缓冲的 piece 整片溢写磁盘,把预算腾出来。
func (s *Storage) overCap(incoming int64) bool {
	if s.pbBytes+incoming <= s.pbCap {
		return false
	}
	// 从最早的缓冲开始整片溢写(写全片,缺失块后续到达时直写补齐)
	for p, pb := range s.pbufs {
		if pb.spill {
			continue
		}
		_ = s.writeRange(s.meta.PieceOffset(p), pb.buf)
		pb.spill = true
		s.pbBytes -= int64(len(pb.buf))
		if s.pbBytes+incoming <= s.pbCap {
			return false
		}
	}
	return true
}

// takePieceBuf 取走 piece 的内存缓冲(校验路径用),未在缓冲则返回 nil。
func (s *Storage) takePieceBuf(piece int) *pieceBuf {
	s.pbMu.Lock()
	defer s.pbMu.Unlock()
	if s.pbufs == nil {
		return nil
	}
	pb := s.pbufs[piece]
	delete(s.pbufs, piece)
	if pb != nil && !pb.spill {
		s.pbBytes -= int64(len(pb.buf))
		return pb
	}
	// 溢写过的缓冲放回去(数据主体在磁盘,块直写仍按 spill 走)
	return nil
}

// VerifyPiece 校验 SHA-1:piece 在内存缓冲里凑齐则直接内存校验、通过后
// 整片一次落盘;否则读回磁盘数据校验(旧路径)。多 peer 并发校验串行化。
// 校验通过会推进逐文件完成计数,集齐的文件立即摘掉 .!zd 后缀。
func (s *Storage) VerifyPiece(piece int) bool {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	ok := s.verifyPieceInner(piece)
	if ok {
		s.MarkPiecesVerified(piece)
	}
	return ok
}

func (s *Storage) verifyPieceInner(piece int) bool {
	if pb := s.takePieceBuf(piece); pb != nil {
		if pb.nHave == len(pb.have) && s.meta.VerifyPieceData(piece, pb.buf) {
			return s.writeRange(s.meta.PieceOffset(piece), pb.buf) == nil
		}
		// 缓冲不完整(理论不可达)或校验失败:丢弃缓冲走重下
		return false
	}
	plen := s.meta.PieceLen(piece)
	if int64(cap(s.scratch)) < plen {
		s.scratch = make([]byte, plen)
	}
	buf := s.scratch[:plen]
	if err := s.readFileRange(s.meta.PieceOffset(piece), buf, false); err != nil {
		return false
	}
	return s.meta.VerifyPieceData(piece, buf)
}

// MarkPiecesVerified 记账一个已校验 piece,并把覆盖它的文件里全部 piece
// 已齐的摘掉 .!zd 后缀(就地布局"边下边用"的关键一步)。按 (文件,piece)
// 二维幂等:同一 piece 对每个覆盖它的文件各计一次——边界 piece 被先完成
// 的文件"占用"全局标记后,仍要给共享它的其他文件记账。
func (s *Storage) MarkPiecesVerified(pieces ...int) {
	s.fxMu.Lock()
	defer s.fxMu.Unlock()
	for _, piece := range pieces {
		if piece < 0 || piece >= len(s.pieceFiles) {
			continue
		}
		for _, fi := range s.pieceFiles[piece] {
			if s.fileDone[fi] {
				continue
			}
			k := piece - s.fileFirst[fi]
			if k < 0 || k >= len(s.fileGot[fi]) || s.fileGot[fi][k] {
				continue
			}
			s.fileGot[fi][k] = true
			s.fileLeft[fi]--
			if s.fileLeft[fi] == 0 {
				s.fileDone[fi] = true
				s.renameToBare(fi)
			}
		}
	}
}

// renameToBare 把已完成文件从 .!zd 改回正式名。Windows 不允许改名被占用
// 的文件(Go 打开文件不带 FILE_SHARE_DELETE),先关句柄、改名、重开;
// 改名失败(例如文件被播放器占用)不视为错误,Finalize 收尾时会再补一次。
// 持 fhMu 写锁:与 readRange/writeRange(读锁)互斥,防止读写到已置空的句柄。
func (s *Storage) renameToBare(i int) {
	marked, bare := s.dataPath(i)+IncompleteSuffix, s.dataPath(i)
	if _, err := os.Stat(marked); err != nil {
		return // 本就无后缀(打开时按位图判定的完成文件)
	}
	s.fhMu.Lock()
	defer s.fhMu.Unlock()
	if s.fhs[i] != nil {
		s.fhs[i].Close()
		s.fhs[i] = nil
	}
	if err := os.Rename(marked, bare); err != nil {
		s.fileDone[i] = false // 留在未完成态,Finalize 时重试
		s.fhs[i], _ = os.OpenFile(marked, os.O_RDWR|os.O_CREATE, 0o644)
		return
	}
	s.fhs[i], _ = os.OpenFile(bare, os.O_RDWR, 0o644)
}

// CommitPieceNode 把已收齐的 piece 落盘并返回其 v2 块级 merkle 节点哈希。
// 用于磁力种子缺 piece layers 的整文件延迟校验:期望逐片哈希未知,先把数据
// 落定并累计各 piece 的节点哈希,文件全部到齐后由 client 重构根与 pieces root
// 对拍。
func (s *Storage) CommitPieceNode(piece int) ([32]byte, bool) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	if pb := s.takePieceBuf(piece); pb != nil {
		if pb.nHave != len(pb.have) {
			return zero32, false
		}
		if s.writeRange(s.meta.PieceOffset(piece), pb.buf) != nil {
			return zero32, false
		}
		return s.meta.PieceNodeHash(piece, pb.buf), true
	}
	plen := s.meta.PieceLen(piece)
	buf := make([]byte, plen)
	if err := s.readFileRange(s.meta.PieceOffset(piece), buf, false); err != nil {
		return zero32, false
	}
	return s.meta.PieceNodeHash(piece, buf), true
}

// ReadForUpload 为做种读取一段数据(serve peer 的 request)。严格模式:
// 未落盘的未选文件报错,调用方静默丢弃该请求(绝不把零块发给 peer)。
func (s *Storage) ReadForUpload(off int64, p []byte) error {
	return s.readFileRange(off, p, true)
}

// StreamBlockState 返回 piece 的块级到货位图(内存攒块,块粒度流播用)。
// buffered=false:不在内存缓冲(未开始/已溢写/已被校验取走/存储已关闭)——
// 溢写后的 piece 块级状态不可知(数据直写盘、位图不再维护),调用方回退
// piece 级等待;piece 已落定的数据在盘上,同样返回 false。
func (s *Storage) StreamBlockState(piece int) (have []bool, buffered bool) {
	s.pbMu.Lock()
	defer s.pbMu.Unlock()
	if s.pbufs == nil {
		return nil, false
	}
	pb := s.pbufs[piece]
	if pb == nil || pb.spill {
		return nil, false
	}
	return pb.have, true
}

// StreamBlockRead 从内存攒块缓冲拷贝一段已到货数据(localOff 为 piece 内
// 偏移,调用方保证所在块已到货且区间不跨块)。位图检查与拷贝同持 pbMu,
// 不会读到半块。缓冲被校验路径取走/溢写时返回 false,调用方应重查 piece
// 是否已落定改走盘读。
func (s *Storage) StreamBlockRead(piece, localOff int, dst []byte) bool {
	s.pbMu.Lock()
	defer s.pbMu.Unlock()
	if s.pbufs == nil {
		return false
	}
	pb := s.pbufs[piece]
	if pb == nil || pb.spill {
		return false
	}
	bi := localOff / BlockSize
	if bi >= len(pb.have) || !pb.have[bi] {
		return false
	}
	copy(dst, pb.buf[localOff:localOff+len(dst)])
	return true
}

// Finalize 在全部需求 piece 校验通过后调用:补摘残留的 .!zd 后缀(校验
// 通过瞬间改名失败——例如文件被播放器占用——的在这里重试)。就地布局
// 没有整体改名这一步,数据自始至终都在最终路径上。返回最终落定路径。
func (s *Storage) Finalize() (string, error) {
	s.Close()
	s.fxMu.Lock()
	defer s.fxMu.Unlock()
	for i := range s.fileDone {
		if s.fileDone[i] || s.fileLeft[i] > 0 {
			continue // 已恢复正式名,或文件选择排除(永远未完成,保留标记)
		}
		marked := s.dataPath(i) + IncompleteSuffix
		if _, err := os.Stat(marked); err == nil {
			if err := os.Rename(marked, s.dataPath(i)); err != nil {
				return "", fmt.Errorf("完成文件改名失败: %w", err)
			}
		}
		s.fileDone[i] = true
	}
	return s.final, nil
}

// FlushPieceBufs 把内存攒块缓冲整片溢写回磁盘(缓冲保留,后续块仍按 spill
// 直写补齐)。块级位图持久化前调用:保证入账的块确定都在盘上——否则强杀/
// 崩溃时缓冲数据丢失,而位图已把这些块标成"持有",重启后恢复出"账面有、
// 盘上是零"的幻影块,piece 以零块凑数反复校验失败(实测同一 piece 换批
// peer 仍失败,进度死循环)。
func (s *Storage) FlushPieceBufs() {
	s.pbMu.Lock()
	for p, pb := range s.pbufs {
		if pb.spill {
			continue
		}
		_ = s.writeRange(s.meta.PieceOffset(p), pb.buf)
		pb.spill = true
	}
	s.pbMu.Unlock()
}

// Close 释放全部文件句柄。优雅关闭前把攒块缓冲整片溢写回磁盘:恢复扫描
// 会重哈希,完整的 piece 直接找回;若直接丢弃缓冲,暂停/退出会把已收到
// 的数据全部吐回去重下(缺失块以零占位,后续块直写补齐)。
func (s *Storage) Close() {
	s.FlushPieceBufs()
	s.pbMu.Lock()
	s.pbufs = nil
	s.pbBytes = 0
	s.pbMu.Unlock()
	s.fhMu.Lock()
	for i, fh := range s.fhs {
		if fh != nil {
			fh.Close()
		}
		s.fhs[i] = nil
	}
	s.fhMu.Unlock()
}

// nBlocksOf piece 的块数。
func (s *Storage) nBlocksOf(piece int) int {
	return int((s.meta.PieceLen(piece) + BlockSize - 1) / BlockSize)
}

// fileIndexFor 返回包含全局偏移 off 的文件下标(跳过零长文件)。
func (s *Storage) fileIndexFor(off int64) int {
	files := s.meta.Files
	lo, hi := 0, len(files)
	for lo < hi {
		mid := (lo + hi) / 2
		if files[mid].Offset+files[mid].Length <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for lo < len(files) && files[lo].Length == 0 {
		lo++
	}
	if lo >= len(files) {
		return -1
	}
	return lo
}

// fileOnDisk 报告文件 i 是否已在盘上(任一形态)。用于读写路径对句柄为空
// 的文件分派:在盘上才打开跟随,不在盘上的未选文件读按零、写直接跳过。
func (s *Storage) fileOnDisk(i int) bool {
	bare := s.dataPath(i)
	if _, err := os.Stat(bare); err == nil {
		return true
	}
	_, err := os.Stat(bare + IncompleteSuffix)
	return err == nil
}

// readFileRange 读取一段全局偏移的数据,跨文件边界自动切分。strict 用于
// 上传服务:未落盘的未选文件按错误处理(宁可不给,绝不发零块);校验与
// 恢复扫描用非严格模式,未落盘区域按零填充——与旧的零预分配语义一致。
// 读路径【绝不创建文件】:否则改选重启的恢复扫描会经跨界 piece 给未选
// 邻居凭空造出 0 字节 .!zd 残留(实测 [BeanSub] 随身 s4 任务一次改选冒
// 出 4 个)。
// readFileRange 重试外层:文件完成瞬间 renameToBare 关旧句柄开新句柄是
// 合法瞬态,本函数取句柄后释放锁再 ReadAt,恰逢切换会得到一次 ErrClosed。
// 上传(ReadForUpload)/校验(verifyPieceInner)/流播三条读路径共用此处,
// 撞上即整体重读一次(重取新句柄);切换只在逐文件完成时发生一次,二次
// 撞上几乎不可达。
func (s *Storage) readFileRange(off int64, p []byte, strict bool) error {
	for attempt := 0; ; attempt++ {
		closed := false
		pos := 0
		for pos < len(p) {
			i := s.fileIndexFor(off + int64(pos))
			if i < 0 {
				return errors.New("偏移越界")
			}
			f := s.meta.Files[i]
			inFile := off + int64(pos) - f.Offset
			n := min(int64(len(p)-pos), f.Length-inFile)
			if n <= 0 {
				return errors.New("读取长度非法")
			}
			if s.pad != nil && s.pad[i] {
				// 填充文件:内容保证全零,直接补零返回(严格模式也不例外——
				// 做种服务跨界 piece 时就该发零,数据本身是种子哈希的一部分)
				clear(p[pos : pos+int(n)])
				pos += int(n)
				continue
			}
			s.fhMu.RLock()
			fh := s.fhs[i]
			s.fhMu.RUnlock()
			if fh == nil && !s.fileOnDisk(i) {
				if strict || s.ro {
					return fmt.Errorf("文件未落盘: %s", f.Path[len(f.Path)-1])
				}
				clear(p[pos : pos+int(n)]) // 从未写入:视为零(旧零预分配语义)
				pos += int(n)
				continue
			}
			if fh == nil {
				s.fhMu.Lock()
				if err := s.ensureFile(i); err != nil {
					s.fhMu.Unlock()
					return err
				}
				fh = s.fhs[i]
				s.fhMu.Unlock()
			}
			if _, err := fh.ReadAt(p[pos:pos+int(n)], inFile); err != nil {
				if errors.Is(err, fs.ErrClosed) && attempt == 0 {
					closed = true // 句柄切换瞬态:重取句柄整体重读
					break
				}
				return err
			}
			pos += int(n)
		}
		if !closed {
			return nil
		}
	}
}

func (s *Storage) writeRange(off int64, p []byte) error {
	pos := 0
	for pos < len(p) {
		i := s.fileIndexFor(off + int64(pos))
		if i < 0 {
			return errors.New("偏移越界")
		}
		f := s.meta.Files[i]
		inFile := off + int64(pos) - f.Offset
		n := min(int64(len(p)-pos), f.Length-inFile)
		if n <= 0 {
			return errors.New("写入长度非法")
		}
		if s.pad != nil && s.pad[i] {
			// 填充文件:对齐占位全零,网络来的块丢弃不落盘——piece 校验读
			// 回时按零合成,与制种端哈希一致;盘上绝不出现填充文件
			pos += int(n)
			continue
		}
		s.fhMu.RLock()
		fh := s.fhs[i]
		s.fhMu.RUnlock()
		if fh == nil {
			// 未选文件的跨界 piece 边界字节照常落盘(.!zd 形态、只有真实
			// 跨界字节):这些字节是 piece 校验的一部分,做种服务该 piece
			// 时必须原样发给对端,缺了它对端永远凑不齐选中文件的末 piece
			// (实测部分选择任务做种只传得出选中内容的零头)。0 字节残留
			// 由 OpenStorage 的自愈清理负责,带真实字节的保留(扩选可复用)
			s.fhMu.Lock()
			if err := s.ensureFile(i); err != nil {
				s.fhMu.Unlock()
				return err
			}
			fh = s.fhs[i]
			s.fhMu.Unlock()
		}
		if _, err := fh.WriteAt(p[pos:pos+int(n)], inFile); err != nil {
			return err
		}
		pos += int(n)
	}
	return nil
}

// HasIncompleteMark 报告 root(多文件数据目录)内是否含带 .!zd 后缀的
// 未完成文件。引擎用它区分"本任务的就地数据"与外来同名数据。
func HasIncompleteMark(root string) bool {
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return false
	}
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if found || err != nil {
			if found {
				return filepath.SkipAll
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, IncompleteSuffix) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
