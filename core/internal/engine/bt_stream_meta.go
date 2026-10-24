// 边下边播元数据探测:把默认 1MB 头/尾 pin 精化为容器元数据的精确区间,
// 并解出时长折算平均码率(窗口自适应 + 前端低速提示)。
//   MP4 faststart(ftyp→moov→mdat):头部 pin 精确到 moov 结束,尾部 pin 解除;
//   MP4 moov 在尾(ftyp→mdat→moov):尾部 pin 精确到 moov 起点到文件尾——
//     moov 大于 1MB 时默认 pin 覆盖不全,播放器拿不到完整索引(本探测的主要动因);
//   MKV:头部 pin 精确到首个 Cluster,尾部 pin 精确到 Cues 元素起。
// 解析器为纯函数(可单测);编排为开流时启动的一次性后台探测,会话失配
// (换文件/停止/会话结束)即自行退出,不阻塞任何请求路径。
package engine

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"strings"
	"time"

	"zdownloader/internal/bt"
)

const (
	streamProbeHeadMP4 = 64 << 10       // MP4 头部走查读取量(ftyp/free→moov|mdat 头足够)
	streamProbeDurRead = 4 << 10        // moov 前缀读取量(mvhd 惯例是 moov 首子 atom)
	streamProbeTailMax = 16 << 20       // MKV 尾部 Cues 扫描增长上限
	streamProbeTimeout = 2 * time.Minute // 探测总限时(无种/慢种不悬挂)
	streamAdaptSecs    = 30             // 目标缓冲秒数:窗口字节 = 码率 × 此值
	streamAdaptMinByte = 4 << 20        // 自适应窗口字节下限(过窄无安全边际)
	streamAdaptMaxByte = 64 << 20       // 上限(与初始窗口一致)
)

// ---------- MP4 ----------

// mp4HeadResult MP4 文件头顶层 atom 走查结论(偏移均为文件内字节)。
type mp4HeadResult struct {
	moovFront bool  // faststart:moov 在 mdat 之前
	moovOff   int64 // moov 起点未知时 -1
	moovEnd   int64 // moov 结束偏移(atom 头声明的完整尺寸,可在缓冲之外)
	mdatEnd   int64 // 首个 mdat 结束偏移;其后(可能有 free)即 moov 所在区
	headEnd   int64 // 头部元数据区结束:moov 在头 = moovEnd,否则 = mdat 起点
}

// mp4WalkHead 走查文件头顶层 atom 序列。ok=false:缓冲内走不到 moov/mdat
// (畸形文件或头前有超大 free),调用方维持默认 pin。
func mp4WalkHead(head []byte, size int64) (mp4HeadResult, bool) {
	var r mp4HeadResult
	r.moovOff = -1
	var off int64
	for off+8 <= int64(len(head)) && off < size {
		asize := int64(binary.BigEndian.Uint32(head[off : off+4]))
		typ := string(head[off+4 : off+8])
		hdrLen := int64(8)
		if asize == 1 { // 64 位扩展长度
			if off+16 > int64(len(head)) {
				return r, false
			}
			asize = int64(binary.BigEndian.Uint64(head[off+8 : off+16]))
			hdrLen = 16
		}
		if asize < hdrLen || off+asize > size {
			return r, false // 非法尺寸(size==0 = mdat 到 EOF,其后无 moov 可探)
		}
		switch typ {
		case "moov":
			if r.moovOff < 0 {
				r.moovOff, r.moovEnd = off, off+asize
			}
		case "mdat":
			r.mdatEnd = off + asize
			if r.moovOff >= 0 {
				r.moovFront = true
				r.headEnd = r.moovEnd
			} else {
				r.headEnd = off // mdat 体开始 = 头部元数据区结束
			}
			return r, true
		}
		off += asize
	}
	if r.moovOff >= 0 { // 头区只有 ftyp→moov(无 mdat,罕见):按 faststart 处理
		r.moovFront = true
		r.headEnd = r.moovEnd
		return r, true
	}
	return r, false
}

// mp4MoovSeconds 解析 moov 前缀里的 mvhd 时长(version 0/1)。moov 只需覆盖
// mvhd 头(惯例是 moov 首子 atom,前 4KB 足够)。
func mp4MoovSeconds(moov []byte) (float64, bool) {
	if len(moov) < 8 {
		return 0, false
	}
	limit := int64(binary.BigEndian.Uint32(moov[0:4])) // moov 自身尺寸
	if limit < 8 || limit > int64(len(moov)) {
		limit = int64(len(moov)) // 前缀缓冲:以缓冲为界继续找 mvhd
	}
	var off int64 = 8
	for off+8 <= limit {
		asize := int64(binary.BigEndian.Uint32(moov[off : off+4]))
		typ := string(moov[off+4 : off+8])
		hdrLen := int64(8)
		if asize == 1 {
			if off+16 > limit {
				return 0, false
			}
			asize = int64(binary.BigEndian.Uint64(moov[off+8 : off+16]))
			hdrLen = 16
		}
		if asize < hdrLen {
			return 0, false
		}
		if typ == "mvhd" {
			body := moov[off+hdrLen:]
			if len(body) < 4 {
				return 0, false
			}
			if body[0] == 1 { // v1:ctime(8) mtime(8) timescale(4) duration(8)
				if len(body) < 32 {
					return 0, false
				}
				ts := binary.BigEndian.Uint32(body[20:24])
				du := binary.BigEndian.Uint64(body[24:32])
				if ts == 0 || du == 0 {
					return 0, false
				}
				return float64(du) / float64(ts), true
			}
			// v0:ctime(4) mtime(4) timescale(4) duration(4)
			if len(body) < 20 {
				return 0, false
			}
			ts := binary.BigEndian.Uint32(body[12:16])
			du := binary.BigEndian.Uint32(body[16:20])
			if ts == 0 || du == 0 {
				return 0, false
			}
			return float64(du) / float64(ts), true
		}
		off += asize
	}
	return 0, false
}

// ---------- MKV / WebM ----------

// mkvVint 读 EBML vint(尺寸语义:清除标记位)。unknown=全 1(未知尺寸)。
func mkvVint(b []byte) (val uint64, n int, unknown, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, false
	}
	n = 1
	mask := byte(0x80)
	for mask > 0 && b[0]&mask == 0 {
		mask >>= 1
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false, false
	}
	v := uint64(b[0] & ^mask)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	// 全 1(每个字节 0xFF)= 未知尺寸
	for i := 0; i < n; i++ {
		if b[i] != 0xFF {
			return v, n, false, true
		}
	}
	return 0, n, true, true
}

// mkvElementID 读元素 ID(保留标记位的原始字节序值)。
func mkvElementID(b []byte) (id uint64, n int, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = 1
	mask := byte(0x80)
	for mask > 0 && b[0]&mask == 0 {
		mask >>= 1
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	v := uint64(b[0])
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, true
}

// Matroska 元素 ID(Segment 一级)。
const (
	mkvIDEbmlHeader = 0x1A45DFA3
	mkvIDSegment    = 0x18538067
	mkvIDSeekHead   = 0x114D9B74
	mkvIDInfo       = 0x1549A966
	mkvIDTracks     = 0x1654AE6B
	mkvIDCluster    = 0x1F43B675
	mkvIDCues       = 0x1C53BB6C
	mkvIDTimeScale  = 0x2AD7B1
	mkvIDDuration   = 0x4489
)

// mkvHeadResult MKV 头部走查结论。
type mkvHeadResult struct {
	headEnd int64   // 首个 Cluster 起点(头元数据区结束;0 = 未探得)
	durSec  float64 // Segment>Info 时长(>0 = 解析到)
}

// mkvWalkHead 走查 EBML 头 → Segment → 其一级子元素,直到首个 Cluster。
// headEnd 超出 head 缓冲(头元数据大于 1MB)时放弃(返回 0,维持默认 pin)。
func mkvWalkHead(head []byte, size int64) mkvHeadResult {
	var r mkvHeadResult
	// EBML 头(可含 Void 等,直接按元素跳过)
	off := int64(0)
	id, n, ok := mkvElementID(head[off:])
	if !ok || id != mkvIDEbmlHeader {
		return r
	}
	off += int64(n)
	sz, sn, unk, ok := mkvVint(head[off:])
	if !ok || unk {
		return r
	}
	off += int64(sn) + int64(sz)
	if off < 0 || off > int64(len(head)) {
		return r // 尺寸越界(损坏/伪造):防止跳出缓冲
	}
	// Segment 容器(尺寸未知 = 流式封装,仍可走子元素)
	id, n, ok = mkvElementID(head[off:])
	if !ok || id != mkvIDSegment {
		return r
	}
	off += int64(n)
	if off > int64(len(head)) {
		return r
	}
	segEnd := size
	sz, sn, unk, ok = mkvVint(head[off:])
	if !ok {
		return r
	}
	off += int64(sn)
	if !unk {
		if end := off + int64(sz); end < segEnd {
			segEnd = end
		}
	}
	// Segment 一级子元素:SeekHead/Info/Tracks… 直到 Cluster
	for off < segEnd && off < size {
		if off < 0 || off+1 > int64(len(head)) {
			return r // 头缓冲之外:无法继续走
		}
		id, n, ok = mkvElementID(head[off:])
		if !ok {
			return r
		}
		so := off + int64(n)
		if so > int64(len(head)) {
			return r
		}
		sz, sn, unk, ok = mkvVint(head[so:])
		if !ok {
			return r
		}
		body := so + int64(sn)
		if unk || sz < 0 || body+int64(sz) > size {
			return r // Cluster 未知尺寸等:头走查止步
		}
		if id == mkvIDCluster {
			r.headEnd = off
			return r
		}
		if id == mkvIDInfo && body+int64(sz) <= int64(len(head)) {
			r.durSec = mkvInfoSeconds(head[body : body+int64(sz)])
		}
		off = body + int64(sz)
	}
	return r
}

// mkvInfoSeconds 解析 Segment>Info 的时长:Duration(浮点)× TimecodeScale
// (纳秒/单位,默认 1e6)。
func mkvInfoSeconds(info []byte) float64 {
	scale := float64(1e6)
	dur := -1.0
	off := 0
	for off+2 <= len(info) {
		id, n, ok := mkvElementID(info[off:])
		if !ok {
			break
		}
		so := off + n
		if so >= len(info) {
			break
		}
		sz, sn, unk, ok := mkvVint(info[so:])
		if !ok || unk {
			break
		}
		body, bend := so+sn, so+sn+int(sz)
		if bend > len(info) {
			break
		}
		switch id {
		case mkvIDTimeScale:
			var v uint64
			for _, c := range info[body:bend] {
				v = v<<8 | uint64(c)
			}
			if v > 0 {
				scale = float64(v)
			}
		case mkvIDDuration:
			switch bend - body {
			case 4:
				dur = float64(math.Float32frombits(binary.BigEndian.Uint32(info[body:])))
			case 8:
				dur = math.Float64frombits(binary.BigEndian.Uint64(info[body:bend]))
			}
		}
		off = bend
	}
	if dur <= 0 {
		return 0
	}
	return dur * scale / 1e9
}

// mkvCuesOffset 在尾部缓冲(覆盖文件内 [tailStart, fileSize))反向扫描 Cues
// 元素头(ID 4 字节 + 尺寸 vint),尺寸与"延伸到文件尾附近"吻合才算命中——
// 视频负载里随机出现 4 字节 ID 模式的假阳性由尺寸校验排除(Cues 之后通常
// 只剩 Tags 等小元素,留 512KB 余量)。返回 Cues 起点文件内偏移;-1 = 未找到。
func mkvCuesOffset(tail []byte, tailStart, fileSize int64) int64 {
	for i := len(tail) - 4; i >= 0; i-- {
		if tail[i] != 0x1C || tail[i+1] != 0x53 || tail[i+2] != 0xBB || tail[i+3] != 0x6C {
			continue
		}
		sz, sn, unk, ok := mkvVint(tail[i+4:])
		if !ok || unk || sz == 0 {
			continue
		}
		end := tailStart + int64(i) + 4 + int64(sn) + int64(sz)
		if end > fileSize || end < fileSize-512<<10 {
			continue
		}
		return tailStart + int64(i)
	}
	return -1
}

// ---------- 引擎编排 ----------

// btStreamProbe 开流时启动的一次性后台探测(每会话一次)。按扩展名分派;
// 任何失败(等待超时/解析不出)都只是维持默认 pin,静默退出。
func (m *Manager) btStreamProbe(t *Task, st *taskStream, cl *bt.Client) {
	switch strings.ToLower(filepath.Ext(st.file)) {
	case ".mp4", ".m4v", ".mov":
		m.btStreamProbeMP4(t, st, cl)
	case ".mkv", ".webm":
		m.btStreamProbeMKV(t, st, cl)
	}
}

// streamAlive 探测会话是否仍是当前会话(换文件/停止/会话结束即失配)。
func (m *Manager) streamAlive(t *Task, st *taskStream) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stream == st && t.btcl != nil
}

// streamWaitPieces 等待文件内 [rel, rel+n) 覆盖的 piece 全部落定。
// 会话失配/超时返回 false。
func (m *Manager) streamWaitPieces(t *Task, st *taskStream, cl *bt.Client, rel, n int64, deadline time.Time) bool {
	off := st.off + rel
	for {
		sig := cl.StreamWaitSignal()
		if cl.StreamPieceDone(off, n) {
			return true
		}
		if time.Now().After(deadline) || !m.streamAlive(t, st) {
			return false
		}
		select {
		case <-sig:
		case <-time.After(2 * time.Second):
		}
	}
}

// streamReadAt 读取文件内 [rel, rel+len(buf))(前置:覆盖 piece 已落定)。
func streamReadAt(st *taskStream, cl *bt.Client, rel int64, buf []byte) error {
	return cl.StreamReadOff(st.off+rel, buf)
}

// streamPiecesOf 文件内字节区间 [rel, rel+n) 覆盖的 piece 下标(全局口径)。
func (st *taskStream) piecesOf(rel, n int64) []int {
	first := int((st.off + rel) / st.pieceLen)
	last := int((st.off + rel + n - 1) / st.pieceLen)
	out := make([]int, 0, last-first+1)
	for p := first; p <= last; p++ {
		out = append(out, p)
	}
	return out
}

// streamApplyLocked 应用探测/预热结论(须持 t.mu):更新 st 的 pin 事实源并
// 把合并结果(当前文件 pin + 预热集)连同自适应窗口下发 picker。head/tail
// 传 nil 表示该项不变;durSec>0 记录码率并重算窗口。会话失配则整体跳过。
func streamApplyLocked(t *Task, st *taskStream, cl *bt.Client, head, tail []int, tailOn *bool, durSec float64) {
	if t.stream != st {
		return
	}
	if head != nil {
		st.pinHead = head
	}
	if tail != nil {
		st.pinTail = tail
	}
	if tailOn != nil {
		st.tailOn = *tailOn
	}
	if durSec > 0 {
		st.bitrate = float64(st.size) / durSec
	}
	if cl == nil {
		return
	}
	mergedHead := append([]int(nil), st.pinHead...)
	for _, ps := range st.warm {
		mergedHead = append(mergedHead, ps...)
	}
	cl.RefineStreamPins(mergedHead, st.pinTail, st.tailOn)
	if st.bitrate > 0 {
		cl.SetStreamAhead(streamAheadFor(st.bitrate, st.pieceLen))
	}
}

// streamAheadFor 按平均码率折算自适应窗口宽度:目标 streamAdaptSecs 秒的
// 内容供给量,字节钳 [4MB, 64MB],piece 数钳沿用静态下限/上限。
func streamAheadFor(bitrate float64, pieceLen int64) int {
	nb := int64(bitrate * streamAdaptSecs)
	if nb < streamAdaptMinByte {
		nb = streamAdaptMinByte
	}
	if nb > streamAdaptMaxByte {
		nb = streamAdaptMaxByte
	}
	n := int((nb + pieceLen - 1) / pieceLen)
	if n < streamAheadMin {
		n = streamAheadMin
	}
	if n > streamAheadMax {
		n = streamAheadMax
	}
	return n
}

// btStreamProbeMP4:头 64KB 走查 atom 序列 → faststart(精化头 pin、解除尾
// pin)或 moov 在尾(mdat 尺寸定位 moov 起点,尾部 pin 精化到 moov 起点到
// 文件尾,头部缩到 mdat 起点);再读 moov 前缀解 mvhd 时长 → 码率自适应。
func (m *Manager) btStreamProbeMP4(t *Task, st *taskStream, cl *bt.Client) {
	deadline := time.Now().Add(streamProbeTimeout)
	n := int64(streamProbeHeadMP4)
	if n > st.size {
		n = st.size
	}
	if n < 16 || !m.streamWaitPieces(t, st, cl, 0, n, deadline) {
		return
	}
	head := make([]byte, n)
	if streamReadAt(st, cl, 0, head) != nil {
		return
	}
	r, ok := mp4WalkHead(head, st.size)
	if !ok {
		return
	}
	apply := func(head, tail []int, tailOn *bool, dur float64) {
		t.mu.Lock()
		streamApplyLocked(t, st, cl, head, tail, tailOn, dur)
		t.mu.Unlock()
	}
	if r.moovFront {
		// faststart:moov 已在头(其前缀即 head 缓冲内),时长直接解
		var dur float64
		if r.moovOff >= 0 && r.moovOff < int64(len(head)) {
			dur, _ = mp4MoovSeconds(head[r.moovOff:])
		}
		noTail := false
		apply(st.piecesOf(0, r.moovEnd), nil, &noTail, dur)
		return
	}
	if r.mdatEnd <= 0 || r.mdatEnd >= st.size {
		return // mdat 到 EOF:无 moov,默认 pin 不动
	}
	// moov 在尾:先下发精化 pin(含 moov 前缀的 piece——读取要等它们落定)
	apply(st.piecesOf(0, r.headEnd), st.piecesOf(r.mdatEnd, st.size-r.mdatEnd), nil, 0)
	n2 := int64(streamProbeDurRead)
	if rem := st.size - r.mdatEnd; n2 > rem {
		n2 = rem
	}
	if n2 < 8 || !m.streamWaitPieces(t, st, cl, r.mdatEnd, n2, deadline) {
		return
	}
	buf := make([]byte, n2)
	if streamReadAt(st, cl, r.mdatEnd, buf) != nil {
		return
	}
	if dur, ok := mp4MoovSeconds(buf); ok {
		apply(nil, nil, nil, dur)
	}
}

// btStreamProbeMKV:头 1MB 走查到首个 Cluster(精化头 pin)+ Info 时长;
// 尾部按 1→2→4→…→16MB 增长扫描 Cues 元素(先扩 pin 再等待,未命中翻倍
// 重试——moov 大于 1MB 的长视频 Cues 也能最终覆盖)。
func (m *Manager) btStreamProbeMKV(t *Task, st *taskStream, cl *bt.Client) {
	deadline := time.Now().Add(streamProbeTimeout)
	n := int64(streamPinBytes)
	if n > st.size {
		n = st.size
	}
	var dur float64
	if n >= 16 && m.streamWaitPieces(t, st, cl, 0, n, deadline) {
		head := make([]byte, n)
		if streamReadAt(st, cl, 0, head) == nil {
			r := mkvWalkHead(head, st.size)
			dur = r.durSec
			t.mu.Lock()
			if r.headEnd > 0 && r.headEnd <= st.size {
				streamApplyLocked(t, st, cl, st.piecesOf(0, r.headEnd), nil, nil, dur)
			} else if dur > 0 {
				streamApplyLocked(t, st, cl, nil, nil, nil, dur)
			}
			t.mu.Unlock()
		}
	}
	for win := int64(streamPinBytes); win <= streamProbeTailMax && win < st.size; win *= 2 {
		tailStart := st.size - win
		t.mu.Lock()
		streamApplyLocked(t, st, cl, nil, st.piecesOf(tailStart, win), nil, 0)
		t.mu.Unlock()
		if !m.streamWaitPieces(t, st, cl, tailStart, win, deadline) {
			return
		}
		tail := make([]byte, win)
		if streamReadAt(st, cl, tailStart, tail) != nil {
			return
		}
		if off := mkvCuesOffset(tail, tailStart, st.size); off >= 0 {
			t.mu.Lock()
			streamApplyLocked(t, st, cl, nil, st.piecesOf(off, st.size-off), nil, 0)
			t.mu.Unlock()
			return
		}
	}
}
