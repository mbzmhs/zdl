package bt

import (
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"zdownloader/internal/util"
)

const (
	// piece 长度上下限:下限防畸形,上限对齐 v2 生态最大值(64MiB)。
	minPieceLength = 1 << 9
	maxPieceLength = 64 << 20
	// maxPathSeg 路径段长度上限(Windows 单段 255)。
	maxPathSeg = 255
	// v2BlockSize BEP-52 块级 merkle 的固定块长(16KiB)。
	v2BlockSize = 16 << 10
)

// FileInfo 是种子内单个文件的声明。
type FileInfo struct {
	Path   []string // 相对路径段(多文件);单文件为 [Name]
	Length int64
	Offset int64 // 在种子字节流中的全局偏移(piece 边界跨文件)
}

// MetaInfo 是解析后的 .torrent 元数据。infohash 基于原始 info dict 字节计算。
type MetaInfo struct {
	Announce     string
	AnnounceList [][]string
	Comment      string
	CreatedBy    string

	InfoHash    [20]byte
	Name        string
	PieceLength int64
	Pieces      []byte // 原始 20 字节哈希串
	Private     bool
	Multi       bool // 多文件(目录)种子

	Length      int64 // 单文件长度(多文件为 0)
	Files       []FileInfo
	TotalLength int64

	// BEP-52 v2-only 种子(file tree + SHA-256 merkle):piece 逐文件对齐、
	// 不跨文件,长度/偏移/归属/期望叶哈希逐 piece 建表。
	// hybrid 种子(同时含 v1 pieces)走 v1 路径,这些字段为空。
	V2          bool
	pieceFile   []int32
	pieceOff    []int64
	pieceLenTab []int32
	filePieces  []int32
	fileRoots   [][32]byte

	// 层哈希状态。多 piece 文件的逐 piece 期望哈希来自顶层 piece layers,
	// 但 ut_metadata(磁力)只传 info dict、不传 layers:此时 layerKnown=false,
	// 下载期由整文件块级 merkle 校验反推后再回填,故需加锁。
	lmu         sync.RWMutex
	pieceLeaf   [][32]byte
	layerKnown  []bool
	layersDirty bool

	rawInfo []byte // 原始 info dict 字节(供 ut_metadata 做种应答)
}

// RawInfo 返回原始 info dict 字节(只读,勿修改)。
func (m *MetaInfo) RawInfo() []byte { return m.rawInfo }

// NamedCopy 返回仅 Name 不同的副本,供存储/落盘路径改名使用(不能直接浅拷贝
// 结构体:内含层哈希读写锁)。除 Name 外的元数据(含 piece 表与层哈希底层
// 数组)与源共享且只读;副本不参与下载期层反推,锁独立。
func (m *MetaInfo) NamedCopy(name string) *MetaInfo {
	c := &MetaInfo{
		Announce:     m.Announce,
		AnnounceList: m.AnnounceList,
		Comment:      m.Comment,
		CreatedBy:    m.CreatedBy,
		InfoHash:     m.InfoHash,
		Name:         name,
		PieceLength:  m.PieceLength,
		Pieces:       m.Pieces,
		Private:      m.Private,
		Multi:        m.Multi,
		Length:       m.Length,
		Files:        m.Files,
		TotalLength:  m.TotalLength,
		V2:           m.V2,
		pieceFile:    m.pieceFile,
		pieceOff:     m.pieceOff,
		pieceLenTab:  m.pieceLenTab,
		filePieces:   m.filePieces,
		fileRoots:    m.fileRoots,
		rawInfo:      m.rawInfo,
	}
	m.lmu.RLock()
	c.pieceLeaf = m.pieceLeaf
	c.layerKnown = m.layerKnown
	m.lmu.RUnlock()
	return c
}

// PieceCount 返回 piece 总数。
func (m *MetaInfo) PieceCount() int {
	if m.V2 {
		return len(m.pieceLenTab)
	}
	return len(m.Pieces) / 20
}

// PieceLen 返回第 idx 个 piece 的实际字节数(v2 中各文件末 piece 逐表查询)。
func (m *MetaInfo) PieceLen(idx int) int64 {
	if idx < 0 || idx >= m.PieceCount() {
		return 0
	}
	if m.V2 {
		return int64(m.pieceLenTab[idx])
	}
	if end := int64(idx+1) * m.PieceLength; end > m.TotalLength {
		return m.TotalLength - int64(idx)*m.PieceLength
	}
	return m.PieceLength
}

// PieceOffset 返回第 idx 个 piece 在种子字节流中的起始偏移。
// v1 均匀网格按乘法;v2 逐文件对齐,查表。
func (m *MetaInfo) PieceOffset(idx int) int64 {
	if idx < 0 || idx >= m.PieceCount() {
		return 0
	}
	if m.V2 {
		return m.pieceOff[idx]
	}
	return int64(idx) * m.PieceLength
}

// FilePieceRange 返回第 fi 个文件占据的 piece 区间 [first,last](含端点)。
// 零长文件不占 piece,返回空区间(last < first)。
func (m *MetaInfo) FilePieceRange(fi int) (first, last int) {
	if fi < 0 || fi >= len(m.Files) {
		return 0, -1
	}
	if m.V2 {
		first := 0
		for i := 0; i < fi; i++ {
			first += int(m.filePieces[i])
		}
		n := int(m.filePieces[fi])
		if n == 0 {
			return first, first - 1
		}
		return first, first + n - 1
	}
	f := m.Files[fi]
	if f.Length == 0 {
		return 0, -1
	}
	first = int(f.Offset / m.PieceLength)
	last = int((f.Offset + f.Length - 1) / m.PieceLength)
	if last < first {
		last = first
	}
	return first, last
}

// PieceFile 返回第 idx 个 piece 所属的文件序号(v2 查表;未覆盖返回 -1)。
func (m *MetaInfo) PieceFile(idx int) int {
	if m.V2 && idx >= 0 && idx < len(m.pieceFile) {
		return int(m.pieceFile[idx])
	}
	return -1
}

// PieceHash 返回第 idx 个 piece 的期望 SHA-1(仅 v1;v2 用 VerifyPieceData)。
func (m *MetaInfo) PieceHash(idx int) [20]byte {
	var h [20]byte
	copy(h[:], m.Pieces[idx*20:(idx+1)*20])
	return h
}

// fileLeafCount 返回某文件 16K 块叶数补零到 2 的幂的结果(单 piece 小文件
// 的 piece 节点须裁剪到这个叶数)。
func (m *MetaInfo) fileLeafCount(fi int) int {
	n := int((m.Files[fi].Length + v2BlockSize - 1) / v2BlockSize)
	ml := 1
	for ml < n {
		ml <<= 1
	}
	return ml
}

// VerifyPieceData 校验整片 piece 数据(data 长度须等于 PieceLen(idx))。
// v1 为 SHA-1 直接比对;v2 按 BEP-52:16KiB 块级 merkle——叶 = SHA-256(块),
// 末块可短、超出文件末的叶置零,piece 的哈希是其 pieceBlocks 个块叶的子树根,
// 期望值来自 piece layers(单 piece 文件即 pieces root 本身)。
// 该文件的层哈希未知(磁力缺 layers 且尚未反推)时返回 false:调用方应先走
// 整文件 merkle 校验(见 LayerKnown/SetFileLayer)。
func (m *MetaInfo) VerifyPieceData(idx int, data []byte) bool {
	if idx < 0 || idx >= m.PieceCount() {
		return false
	}
	if !m.V2 {
		return sha1.Sum(data) == m.PieceHash(idx)
	}
	// 文件块叶数补零到 2 的幂后可能不足一个 piece 窗口(单 piece 小文件),
	// piece 节点须裁剪到文件树的叶数
	fi := m.PieceFile(idx)
	if fi < 0 {
		return false
	}
	m.lmu.RLock()
	want := m.pieceLeaf[idx]
	known := fi < len(m.layerKnown) && m.layerKnown[fi]
	m.lmu.RUnlock()
	if !known {
		return false
	}
	return m.PieceNodeHash(idx, data) == want
}

// PieceNodeHash 计算一片数据在其文件 16K 块级 merkle 树中的节点哈希。
func (m *MetaInfo) PieceNodeHash(idx int, data []byte) [32]byte {
	fi := m.PieceFile(idx)
	if fi < 0 {
		return zero32
	}
	return v2PieceNode(data, int(m.PieceLength/v2BlockSize), m.fileLeafCount(fi))
}

// LayerKnown 报告某文件的逐 piece 期望哈希是否已知(v2;非 v2 恒真)。
func (m *MetaInfo) LayerKnown(fi int) bool {
	if !m.V2 {
		return true
	}
	if fi < 0 || fi >= len(m.layerKnown) {
		return true
	}
	m.lmu.RLock()
	defer m.lmu.RUnlock()
	return m.layerKnown[fi]
}

// FileRoot 返回某文件的 pieces root(v2;非 v2 为零值)。
func (m *MetaInfo) FileRoot(fi int) [32]byte {
	if !m.V2 || fi < 0 || fi >= len(m.fileRoots) {
		return zero32
	}
	return m.fileRoots[fi]
}

// SetFileLayer 回填某文件的逐 piece 期望哈希。缺 layers 的 v2 磁力在整文件
// 块级 merkle 校验通过后调用,此后该文件可逐片校验、上传与做种。
func (m *MetaInfo) SetFileLayer(fi int, nodes [][32]byte) {
	if !m.V2 || fi < 0 || fi >= len(m.filePieces) {
		return
	}
	first, last := m.FilePieceRange(fi)
	n := last - first + 1
	if n <= 0 {
		return
	}
	m.lmu.Lock()
	for k := 0; k < n && k < len(nodes); k++ {
		if first+k < len(m.pieceLeaf) {
			m.pieceLeaf[first+k] = nodes[k]
		}
	}
	if fi < len(m.layerKnown) {
		m.layerKnown[fi] = true
	}
	m.layersDirty = true
	m.lmu.Unlock()
}

// LayersDirty 报告是否有运行期反推出、尚未写回归档种子的 layer。
func (m *MetaInfo) LayersDirty() bool {
	m.lmu.RLock()
	defer m.lmu.RUnlock()
	return m.layersDirty
}

// ClearLayersDirty 在归档重写成功后调用。
func (m *MetaInfo) ClearLayersDirty() {
	m.lmu.Lock()
	m.layersDirty = false
	m.lmu.Unlock()
}

// v2PieceNode 计算一片数据在 16K 块级 merkle 树中的节点哈希。
// BEP-52:叶 = SHA-256(16KiB 块),末块可短于 16KiB(不补零),文件末之后
// 的块叶置零;节点 = 两两 SHA-256 上卷。mLeaf 为文件块叶补零后的总数:
// 不足一个 piece 窗口的文件(单 piece 小文件)节点只覆盖到 mLeaf。
func v2PieceNode(data []byte, pieceBlocks, mLeaf int) [32]byte {
	n := (len(data) + v2BlockSize - 1) / v2BlockSize
	count := pieceBlocks
	if mLeaf < count {
		count = mLeaf
	}
	nodes := make([][32]byte, count)
	for i := 0; i < count; i++ {
		if i < n {
			end := min((i+1)*v2BlockSize, len(data))
			nodes[i] = sha256.Sum256(data[i*v2BlockSize : end])
		} else {
			nodes[i] = zero32
		}
	}
	for len(nodes) > 1 {
		next := make([][32]byte, len(nodes)/2)
		for i := range next {
			var buf [64]byte
			copy(buf[:32], nodes[2*i][:])
			copy(buf[32:], nodes[2*i+1][:])
			next[i] = sha256.Sum256(buf[:])
		}
		nodes = next
	}
	return nodes[0]
}

var zero32 [32]byte

// v2RootFromPieces 由 piece 层哈希重构 16K 块级树的根(BEP-52 解析期对拍:
// layers 与 pieces root 不一致即拒绝)。piece 级节点数 = 2^ceil(log2(块数))
// / 每 piece 块数,完全超出文件末的节点由纯零块构成,规范规定不写入
// piece layers,此处补齐后上卷。
func v2RootFromPieces(entries [][32]byte, pieceBlocks, nBlocks int) [32]byte {
	m := 1
	for m < nBlocks {
		m <<= 1
	}
	level := m / pieceBlocks
	if level < 1 {
		level = 1
	}
	arr := make([][32]byte, level)
	copy(arr, entries)
	zb := make([][32]byte, pieceBlocks)
	for i := range zb {
		zb[i] = zero32
	}
	tail := v2PieceNodeFromLeaves(zb)
	for i := len(entries); i < level; i++ {
		arr[i] = tail
	}
	for len(arr) > 1 {
		next := make([][32]byte, len(arr)/2)
		for i := range next {
			var buf [64]byte
			copy(buf[:32], arr[2*i][:])
			copy(buf[32:], arr[2*i+1][:])
			next[i] = sha256.Sum256(buf[:])
		}
		arr = next
	}
	return arr[0]
}

func v2PieceNodeFromLeaves(leaves [][32]byte) [32]byte {
	nodes := make([][32]byte, len(leaves))
	copy(nodes, leaves)
	for len(nodes) > 1 {
		next := make([][32]byte, len(nodes)/2)
		for i := range next {
			var buf [64]byte
			copy(buf[:32], nodes[2*i][:])
			copy(buf[32:], nodes[2*i+1][:])
			next[i] = sha256.Sum256(buf[:])
		}
		nodes = next
	}
	return nodes[0]
}

// ParseTorrent 解析 .torrent 字节。所有字符串路径段都经 Windows 清洗,
// 拒绝 "."、".." 与清洗后为空的段,杜绝路径穿越。
func ParseTorrent(data []byte) (*MetaInfo, error) {
	root, err := Decode(data)
	if err != nil {
		// 容错:部分 .torrent 文件尾部带多余字节(工具拼接/追加、二次导出
		// 残留等),严格 Decode 会报"尾部多余 N 字节"而整体拒收。回退到
		// DecodePrefix 只解析首部完整值、忽略尾随数据,救回可解析的种子;
		// 尾部不影响 info dict 原始字节,故 infohash 不变。首部本身损坏时
		// 两次都失败,返回原始(更具体)错误。
		if pr, _, perr := DecodePrefix(data); perr == nil {
			root = pr
		} else {
			return nil, fmt.Errorf("种子 bencode 解析失败: %w", err)
		}
	}
	rm, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("种子顶层不是 dict")
	}
	start, end, err := InfoDictSpan(data)
	if err != nil {
		return nil, err
	}
	info, ok := rm["info"].(map[string]any)
	if !ok {
		return nil, errors.New("种子缺少 info 字段")
	}
	// v2-only 判定:有 file tree 且无有效 v1 pieces。hybrid(两者都有)走
	// v1 路径——piece 网格与校验完全兼容,v1 群即可互操作。
	piecesStr, hasPieces := info["pieces"].(string)
	_, hasTree := info["file tree"].(map[string]any)
	isV2 := hasTree && (!hasPieces || len(piecesStr)%20 != 0)
	mi := &MetaInfo{}
	if isV2 {
		h := sha256.Sum256(data[start:end])
		copy(mi.InfoHash[:], h[:20]) // BEP-52:infohash = SHA-256(info) 截断 20 字节
	} else {
		mi.InfoHash = sha1.Sum(data[start:end])
	}
	mi.rawInfo = data[start:end]
	mi.Announce, _ = rm["announce"].(string)
	if tier, ok := rm["announce-list"].([]any); ok {
		for _, t := range tier {
			l, ok := t.([]any)
			if !ok {
				continue
			}
			var urls []string
			for _, u := range l {
				if s, ok := u.(string); ok && s != "" {
					urls = append(urls, s)
				}
			}
			if len(urls) > 0 {
				mi.AnnounceList = append(mi.AnnounceList, urls)
			}
		}
	}
	mi.Comment, _ = rm["comment"].(string)
	mi.CreatedBy, _ = rm["created by"].(string)

	name, ok := info["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, errors.New("种子缺少名称")
	}
	mi.Name = util.SanitizeFilename(name)
	pl, ok := info["piece length"].(int64)
	if !ok || pl < minPieceLength || pl > maxPieceLength {
		return nil, fmt.Errorf("种子 piece 长度非法: %v", pl)
	}
	mi.PieceLength = pl

	switch {
	case hasPieces && len(piecesStr)%20 == 0:
		// v1 / hybrid
		mi.Pieces = []byte(piecesStr)
		mi.Private = info["private"] == int64(1)
		if fl, ok := info["files"].([]any); ok {
			mi.Multi = true
			var off int64
			for _, fe := range fl {
				fd, ok := fe.(map[string]any)
				if !ok {
					return nil, errors.New("种子 files 条目非法")
				}
				length, _ := fd["length"].(int64)
				if length < 0 {
					return nil, errors.New("种子文件长度非法")
				}
				pl, ok := fd["path"].([]any)
				if !ok || len(pl) == 0 {
					return nil, errors.New("种子文件路径非法")
				}
				segs := make([]string, 0, len(pl))
				for _, p := range pl {
					s, ok := p.(string)
					if !ok {
						return nil, errors.New("种子文件路径非法")
					}
					if s = sanitizeSeg(s); s == "" {
						return nil, fmt.Errorf("种子包含非法路径段 %q", s)
					}
					segs = append(segs, s)
				}
				mi.Files = append(mi.Files, FileInfo{Path: segs, Length: length, Offset: off})
				off += length
			}
			if len(mi.Files) == 0 {
				return nil, errors.New("种子没有文件")
			}
		} else {
			length, ok := info["length"].(int64)
			if !ok || length < 0 {
				return nil, errors.New("种子缺少文件长度")
			}
			mi.Length = length
			mi.Files = []FileInfo{{Path: []string{mi.Name}, Length: length, Offset: 0}}
		}
		for _, f := range mi.Files {
			mi.TotalLength += f.Length
		}
		want := (mi.TotalLength + mi.PieceLength - 1) / mi.PieceLength
		if int64(len(mi.Pieces)/20) != want {
			return nil, fmt.Errorf("piece 数与文件总长不符: %d != %d", len(mi.Pieces)/20, want)
		}
	case isV2:
		if err := mi.parseV2FileTree(info["file tree"].(map[string]any), rm["piece layers"]); err != nil {
			return nil, err
		}
		mi.Private = info["private"] == int64(1)
	default:
		return nil, errors.New("种子 pieces 字段非法")
	}
	return mi, nil
}

// parseV2FileTree 解析 BEP-52 v2-only 种子:file tree 展开为文件表,piece
// 逐文件对齐建表(不跨文件,长度/偏移/归属逐 piece 查表)。多 piece 文件必须
// 携带顶层 piece layers,且对其做 merkle 根对拍——layers 与 pieces root 不
// 一致即拒绝,防喂错层数据后静默校验全败。
func (m *MetaInfo) parseV2FileTree(tree map[string]any, layersAny any) error {
	type ftFile struct {
		segs   []string
		length int64
		root   string
	}
	var leaves []ftFile
	var walk func(d map[string]any, prefix []string) error
	walk = func(d map[string]any, prefix []string) error {
		// 键按字节序展开:即种子内文件的规范顺序(字节流顺序随之确定)
		for _, k := range sortedKeys(d) {
			sub, isDict := d[k].(map[string]any)
			if !isDict {
				return fmt.Errorf("v2 file tree 存在非法节点 %q", k)
			}
			if _, isFile := sub["length"]; isFile {
				ln, ok := sub["length"].(int64)
				if !ok || ln < 0 {
					return errors.New("v2 种子文件长度非法")
				}
				pr, _ := sub["pieces root"].(string)
				if ln > 0 && len(pr) != 32 {
					return fmt.Errorf("v2 种子文件 %q pieces root 非法", k)
				}
				// BitComet 系制种端把文件表示为「目录 → 空名文件」
				// (099.png/{""}:…),空名叶折叠进父路径才是真实文件路径
				segs := make([]string, 0, len(prefix)+1)
				segs = append(segs, prefix...)
				if k != "" {
					s := sanitizeSeg(k)
					if s == "" {
						return fmt.Errorf("v2 种子包含非法路径段 %q", k)
					}
					segs = append(segs, s)
				} else if len(prefix) == 0 {
					return errors.New("v2 种子包含根级空名文件")
				}
				leaves = append(leaves, ftFile{segs: segs, length: ln, root: pr})
			} else {
				if err := walk(sub, append(prefix, k)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(tree, nil); err != nil {
		return err
	}
	if len(leaves) == 0 {
		return errors.New("v2 种子没有文件")
	}
	m.V2 = true
	if len(leaves) == 1 && len(leaves[0].segs) == 1 {
		// 单文件 v2:数据即文件本身,布局与 v1 单文件一致
		m.Multi = false
		m.Length = leaves[0].length
		m.Files = []FileInfo{{Path: []string{m.Name}, Length: leaves[0].length}}
	} else {
		m.Multi = true
		var off int64
		for _, f := range leaves {
			m.Files = append(m.Files, FileInfo{Path: f.segs, Length: f.length, Offset: off})
			off += f.length
		}
	}
	layers, _ := layersAny.(map[string]any)
	pl := m.PieceLength
	if pl < v2BlockSize || pl%v2BlockSize != 0 {
		// BEP-52:piece 长度须为 16KiB 的 2 的幂;块级树的 piece 级节点
		// 数计算依赖整除
		return errors.New("v2 种子 piece 长度须为 16KiB 的整数幂")
	}
	pieceBlocks := int(pl / v2BlockSize)
	m.pieceLeaf = make([][32]byte, 0, 1024)
	m.layerKnown = make([]bool, len(m.Files))
	for fi, f := range m.Files {
		n := int((f.Length + pl - 1) / pl)
		m.filePieces = append(m.filePieces, int32(n))
		var root [32]byte
		copy(root[:], leaves[fi].root)
		m.fileRoots = append(m.fileRoots, root)
		if f.Length == 0 {
			m.layerKnown[fi] = true // 零长文件不占 piece,无层可言
			continue
		}
		base := len(m.pieceLenTab)
		for j := 0; j < n; j++ {
			l := pl
			if rem := f.Length - int64(j)*pl; rem < pl {
				l = rem
			}
			m.pieceFile = append(m.pieceFile, int32(fi))
			m.pieceOff = append(m.pieceOff, f.Offset+int64(j)*pl)
			m.pieceLenTab = append(m.pieceLenTab, int32(l))
			m.pieceLeaf = append(m.pieceLeaf, root) // n==1 时 root 即叶;多 piece 下方覆盖
		}
		if n == 1 {
			m.layerKnown[fi] = true // 单 piece:root 即叶,无需 layers
			continue
		}
		// 多 piece 文件:期望逐 piece 哈希取自顶层 piece layers。磁力经
		// ut_metadata 只拿到 info dict(规范规定 layers 不在 info 内),此时
		// layers 缺失是正常形态——标记层未知,下载期由整文件 merkle 反推。
		// layers 在场则严格校验(长度、与 pieces root 对拍),防喂错层数据。
		layerStr, ok := layers[leaves[fi].root].(string)
		if !ok {
			for j := 0; j < n; j++ {
				m.pieceLeaf[base+j] = zero32
			}
			continue
		}
		if len(layerStr) != n*32 {
			return fmt.Errorf("v2 种子文件 %q piece layers 长度不符", strings.Join(f.Path, "/"))
		}
		entries := make([][32]byte, n)
		for j := 0; j < n; j++ {
			copy(entries[j][:], layerStr[j*32:(j+1)*32])
			m.pieceLeaf[base+j] = entries[j]
		}
		// BEP-52:16K 块级树,由 piece 层重构根与 pieces root 对拍
		nBlocks := int((f.Length + v2BlockSize - 1) / v2BlockSize)
		if v2RootFromPieces(entries, pieceBlocks, nBlocks) != root {
			return fmt.Errorf("v2 种子文件 %q piece layers 与 pieces root 不符", strings.Join(f.Path, "/"))
		}
		m.layerKnown[fi] = true
	}
	for _, f := range m.Files {
		m.TotalLength += f.Length
	}
	return nil
}

// sortedKeys 按字节序返回 dict 键(bencode 规范序,file tree 展开即种子内
// 文件的规范顺序)。
func sortedKeys(d map[string]any) []string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sanitizeSeg 清洗单个路径段:拒绝 "."/"..",非法字符与保留名交给 util,清洗后为空即拒绝。
func sanitizeSeg(s string) string {
	s = strings.TrimSpace(s)
	if s == "." || s == ".." {
		return ""
	}
	if s == "" || len(s) > maxPathSeg {
		return ""
	}
	return util.SanitizeFilename(s)
}

// BuildTorrentBytes 从已解析的 MetaInfo 重建 .torrent 字节(磁力任务元数据归档用)。
// announce 为空时顶层不含该键;announce-list 原样保留;createdBy 为
// "created by" 元数据(对外可见,空则用默认伪装名)。v2 种子必须回填顶层
// "piece layers"(不在 info 内),否则归档后再解析会因缺 layers 被拒。
func BuildTorrentBytes(m *MetaInfo, announce, createdBy string) ([]byte, error) {
	if len(m.rawInfo) == 0 {
		return nil, errors.New("缺少原始 info dict")
	}
	top := map[string]any{
		"info":       Raw(m.rawInfo),
		"created by": createdByFallback(createdBy),
	}
	if layers := m.buildPieceLayers(); layers != nil {
		top["piece layers"] = layers
	}
	if announce != "" {
		top["announce"] = announce
	}
	if len(m.AnnounceList) > 0 {
		var tiers []any
		for _, tier := range m.AnnounceList {
			var urls []any
			for _, u := range tier {
				urls = append(urls, u)
			}
			tiers = append(tiers, urls)
		}
		top["announce-list"] = tiers
	}
	return Encode(top)
}

// buildPieceLayers 由已解析的 piece 表重建 BEP-52 顶层 "piece layers":
// 每个多 piece 文件一项,键为 pieces root,值为该文件逐 piece 的 16K 块级
// 子树根哈希串。单 piece 文件不设(其 root 即叶,规范不写)。非 v2 返回 nil。
func (m *MetaInfo) buildPieceLayers() map[string]any {
	if !m.V2 || len(m.filePieces) == 0 {
		return nil
	}
	m.lmu.RLock()
	defer m.lmu.RUnlock()
	layers := map[string]any{}
	base := 0
	for fi, n := range m.filePieces {
		if n > 1 && fi < len(m.layerKnown) && m.layerKnown[fi] {
			b := make([]byte, 0, int(n)*32)
			for j := 0; j < int(n) && base+j < len(m.pieceLeaf); j++ {
				b = append(b, m.pieceLeaf[base+j][:]...)
			}
			if len(b) == int(n)*32 {
				layers[string(m.fileRoots[fi][:])] = string(b)
			}
		}
		base += int(n)
	}
	if len(layers) == 0 {
		return nil
	}
	return layers
}

// BuildTorrentFromDisk 从磁盘文件或目录构造 .torrent,用于自我对拍测试与本地做种。
// pieceLength<=0 时按总大小自适应。返回种子字节与重新解析(自检)后的 MetaInfo。
func BuildTorrentFromDisk(root, announce, createdBy string, pieceLength int64) ([]byte, *MetaInfo, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if pieceLength <= 0 {
		pieceLength = pickPieceLength(0) // 先取默认;统计出总长后在下方修正
	}
	var files []FileInfo
	var total int64
	if st.IsDir() {
		var rels []string
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				rel, rerr := filepath.Rel(root, p)
				if rerr != nil {
					return rerr
				}
				rels = append(rels, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		sort.Strings(rels)
		for _, rel := range rels {
			fi, serr := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if serr != nil {
				return nil, nil, serr
			}
			files = append(files, FileInfo{Path: strings.Split(rel, "/"), Length: fi.Size(), Offset: total})
			total += fi.Size()
		}
		if len(files) == 0 {
			return nil, nil, errors.New("目录为空,无法生成种子")
		}
	} else {
		total = st.Size()
		files = []FileInfo{{Path: []string{st.Name()}, Length: total, Offset: 0}}
	}
	if total == 0 {
		return nil, nil, errors.New("总大小为 0,无法生成种子")
	}
	if pieceLength <= 0 {
		pieceLength = pickPieceLength(total)
	}

	hashes, err := hashDiskPieces(st.IsDir(), root, files, total, pieceLength)
	if err != nil {
		return nil, nil, err
	}
	info := map[string]any{
		"name":         st.Name(),
		"piece length": pieceLength,
		"pieces":       hashes,
	}
	if st.IsDir() {
		var fany []any
		for _, f := range files {
			pany := make([]any, 0, len(f.Path))
			for _, s := range f.Path {
				pany = append(pany, s)
			}
			fany = append(fany, map[string]any{"length": f.Length, "path": pany})
		}
		info["files"] = fany
	} else {
		info["length"] = total
	}
	infoBytes, err := Encode(info)
	if err != nil {
		return nil, nil, err
	}
	top := map[string]any{"info": Raw(infoBytes), "created by": createdByFallback(createdBy)}
	if announce != "" {
		top["announce"] = announce
	}
	data, err := Encode(top)
	if err != nil {
		return nil, nil, err
	}
	mi, err := ParseTorrent(data)
	if err != nil {
		return nil, nil, err
	}
	return data, mi, nil
}

// pickPieceLength 按总大小选 piece 长度,piece 数控制在 1k~16k 的常见区间。
func pickPieceLength(total int64) int64 {
	switch {
	case total <= 64<<20:
		return 256 << 10
	case total <= 1<<30:
		return 1 << 20
	case total <= 8<<30:
		return 2 << 20
	default:
		return 4 << 20
	}
}

// hashDiskPieces 从磁盘读取全部文件内容并计算逐 piece SHA-1。
// base 为多文件种子的数据目录,或单文件种子的文件本身。
func hashDiskPieces(multi bool, base string, files []FileInfo, total, pieceLength int64) ([]byte, error) {
	out := make([]byte, 0, int((total+pieceLength-1)/pieceLength)*20)
	buf := make([]byte, pieceLength)
	for start := int64(0); start < total; start += pieceLength {
		end := min(start+pieceLength, total)
		if err := readDiskRange(multi, base, files, start, buf[:end-start]); err != nil {
			return nil, err
		}
		h := sha1.Sum(buf[:end-start])
		out = append(out, h[:]...)
	}
	return out, nil
}

// readDiskRange 按文件偏移表读取种子字节流 [off, off+len(p)) 的内容,
// piece 跨文件边界时自动拼接。零长文件不占字节流,自动跳过。
func readDiskRange(multi bool, base string, files []FileInfo, off int64, p []byte) error {
	i := sort.Search(len(files), func(i int) bool {
		return files[i].Offset+files[i].Length > off
	})
	pos := 0
	for i < len(files) && pos < len(p) {
		f := files[i]
		i++
		if f.Length == 0 || off >= f.Offset+f.Length {
			continue
		}
		inFile := off - f.Offset
		n := min(int64(len(p)-pos), f.Length-inFile)
		var path string
		if multi {
			path = filepath.Join(base, filepath.Join(f.Path...))
		} else {
			path = base // 单文件种子:数据根就是文件本身
		}
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = fh.ReadAt(p[pos:pos+int(n)], inFile)
		fh.Close()
		if err != nil {
			return err
		}
		pos += int(n)
		off += n
	}
	if pos < len(p) {
		return errors.New("磁盘数据不足(文件可能被改动)")
	}
	return nil
}

// WantedPiecesFor 依据所选文件(BEP-19 相对路径,以 / 连接)推算 piece 需求位图。
// 跨越选中文件边界的 piece 会被整块下载(piece 是校验与传输的最小单位)。
// files 为空或没有任何匹配时返回 nil(= 全部下载)。
func (m *MetaInfo) WantedPiecesFor(files []string) []bool {
	if len(files) == 0 {
		return nil
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[strings.Join(strings.Split(f, "\\"), "/")] = true
	}
	wanted := make([]bool, m.PieceCount())
	any := false
	for i, f := range m.Files {
		if !set[strings.Join(f.Path, "/")] {
			continue
		}
		any = true
		first, last := m.FilePieceRange(i)
		for pi := first; pi <= last && pi < len(wanted); pi++ {
			wanted[pi] = true
		}
	}
	if !any {
		return nil
	}
	return wanted
}

// FileWantedFor 依据所选文件推算文件级需求位图(按 meta.Files 序号精确
// 匹配路径)。与 WantedPiecesFor 的差异:文件选择下载时相邻文件共享边界
// piece,按 piece 区间相交推算会把未选的邻居一并标成需求——进度展示、
// 预分配都会失真。files 为空返回 (nil, nil)(= 全部);任一路径在种子中
// 不存在则报错(调用方决定拒绝还是回退)。路径统一接受 \ 或 / 分隔。
func (m *MetaInfo) FileWantedFor(files []string) ([]bool, error) {
	if len(files) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[strings.Join(strings.Split(f, "\\"), "/")] = true
	}
	out := make([]bool, len(m.Files))
	matched := make(map[string]bool, len(set))
	for i, f := range m.Files {
		p := strings.Join(f.Path, "/")
		if set[p] {
			out[i] = true
			matched[p] = true
		}
	}
	for p := range set {
		if !matched[p] {
			return nil, fmt.Errorf("种子中不存在所选文件: %s", p)
		}
	}
	return out, nil
}

// WantedBytes 返回位图标记的 piece 总字节数(wanted 为 nil 时全部)。
func (m *MetaInfo) WantedBytes(wanted []bool) int64 {
	if wanted == nil {
		return m.TotalLength
	}
	var total int64
	for i, w := range wanted {
		if w {
			total += m.PieceLen(i)
		}
	}
	return total
}

// createdByFallback 归一化 "created by":空值回退默认伪装名。
func createdByFallback(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return "BitComet 1.99"
}
