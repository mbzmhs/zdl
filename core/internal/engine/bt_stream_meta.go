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
	headEnd   int64   // 首个 Cluster 起点(头元数据区结束;0 = 未探得)
	durSec    float64 // Segment>Info 时长(>0 = 解析到)
	segDataOff int64  // Segment 数据区起点(头后;Cues 的 ClusterPosition 相对它)
	timeScale float64 // Info>TimecodeScale(纳秒/单位;默认 1e6)
}

// mkvWalkHead 走查 EBML 头 → Segment → 其一级子元素,直到首个 Cluster。
// headEnd 超出 head 缓冲(头元数据大于 1MB)时放弃(返回 0,维持默认 pin)。
func mkvWalkHead(head []byte, size int64) mkvHeadResult {
	var r mkvHeadResult
	r.timeScale = 1e6
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
	r.segDataOff = off // Cues 的 ClusterPosition 相对 Segment 数据区起点
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
			r.durSec, r.timeScale = mkvInfoFields(head[body : body+int64(sz)])
		}
		off = body + int64(sz)
	}
	return r
}

// mkvInfoFields 解析 Segment>Info 的时长与 TimecodeScale(纳秒/单位,
// 默认 1e6)。
func mkvInfoFields(info []byte) (durSec, scale float64) {
	scale = 1e6
	var dur float64 = -1.0
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
		return 0, scale
	}
	return dur * scale / 1e9, scale
}

// mkvBuildSeekPoints 解析 Cues 元素(CuePoint: CueTime + CueTrackPositions>
// CueClusterPosition)构建时间→字节表。segDataOff 为 Segment 数据区起点
//(ClusterPosition 相对它),scale 为 TimecodeScale。失败返回 nil。
func mkvBuildSeekPoints(cuesBody []byte, segDataOff int64, scale float64) []seekPoint {
	var pts []seekPoint
	off := 0
	for off+2 <= len(cuesBody) {
		id, n, ok := mkvElementID(cuesBody[off:])
		if !ok {
			break
		}
		so := off + n
		if so >= len(cuesBody) {
			break
		}
		sz, sn, unk, ok := mkvVint(cuesBody[so:])
		if !ok || unk {
			break
		}
		body, bend := so+sn, so+sn+int(sz)
		if bend > len(cuesBody) {
			break
		}
		if id == 0xBB { // CuePoint
			var t uint64
			var pos int64 = -1
			co := body
			for co+2 <= bend {
				cid, cn, ok2 := mkvElementID(cuesBody[co:])
				if !ok2 {
					break
				}
				cso := co + cn
				if cso >= bend {
					break
				}
				csz, csn, cunk, ok3 := mkvVint(cuesBody[cso:])
				if !ok3 || cunk {
					break
				}
				cbody, cbend := cso+csn, cso+csn+int(csz)
				if cbend > bend {
					break
				}
				switch cid {
				case 0xB3: // CueTime
					var v uint64
					for _, c := range cuesBody[cbody:cbend] {
						v = v<<8 | uint64(c)
					}
					t = v
				case 0xE7: // CueTrackPositions
					po := cbody
					for po+2 <= cbend {
						pid, pn, ok4 := mkvElementID(cuesBody[po:])
						if !ok4 {
							break
						}
						pso := po + pn
						if pso >= cbend {
							break
						}
						psz, psn, punk, ok5 := mkvVint(cuesBody[pso:])
						if !ok5 || punk {
							break
						}
						pbody, pbend := pso+psn, pso+psn+int(psz)
						if pbend > cbend {
							break
						}
						if pid == 0xF1 { // CueClusterPosition
							var v uint64
							for _, c := range cuesBody[pbody:pbend] {
								v = v<<8 | uint64(c)
							}
							pos = segDataOff + int64(v)
						}
						po = pbend
					}
				}
				co = cbend
			}
			if pos >= 0 && scale > 0 {
				pts = append(pts, seekPoint{float64(t) * scale / 1e9, pos})
			}
		}
		off = bend
	}
	if len(pts) == 0 {
		return nil
	}
	return downsampleSeekPoints(pts, 400)
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

// ---------- 播放头精确定位:容器索引时间→字节表 ----------
// 播放器 seek/播放位置换算用的就是容器自带索引(MP4:stts/stsc/stco,
// MKV:Cues)。探测已定位并 pin 了这份索引,顺势解析成 (秒,字节) 采样表
// 下发前端,红线按 currentTime 二分查找——与播放器同源的精确换算,
// 取代一切估算(均摊/内插在 VBR 与"推送-解析速率解耦"下原理性失准)。

// seekPoint 播放头定位采样点(秒 → 文件内字节偏移)。
type seekPoint struct {
	t   float64
	off int64
}

// mp4Child 在 atom 容器(body 字节)中找指定类型的子 atom,返回其 body
// 区间 [off, off+size)。ok=false:未找到/越界。
func mp4Child(body []byte, typ string) ([]byte, bool) {
	var off int64
	for off+8 <= int64(len(body)) {
		asize := int64(binary.BigEndian.Uint32(body[off : off+4]))
		t := string(body[off+4 : off+8])
		hdr := int64(8)
		if asize == 1 {
			if off+16 > int64(len(body)) {
				return nil, false
			}
			asize = int64(binary.BigEndian.Uint64(body[off+8 : off+16]))
			hdr = 16
		}
		if asize < hdr || off+asize > int64(len(body)) {
			return nil, false
		}
		if t == typ {
			return body[off+hdr : off+asize], true
		}
		off += asize
	}
	return nil, false
}

func mp4U32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off : off+4]) }

// mp4BuildSeekPoints 解析完整 moov 字节,构建视频轨(无则首轨)的
// 时间→字节表,精确到 chunk 粒度;按字节步长降采样(进度条只需 piece 级
// 精度)。失败返回 nil(调用方回退估算)。
func mp4BuildSeekPoints(moov []byte) []seekPoint {
	// 收集全部 trak(游标逐个取,≤16 防畸形)
	var traks [][]byte
	for cur := moov; len(traks) <= 16; {
		body, ok := mp4Child(cur, "trak")
		if !ok {
			break
		}
		traks = append(traks, body)
		cur = cur[afterChild(cur, "trak"):]
	}
	var stblBody []byte
	var timescale uint32
	for _, tk := range traks {
		mdia, ok := mp4Child(tk, "mdia")
		if !ok {
			continue
		}
		mdhd, ok2 := mp4Child(mdia, "mdhd")
		if !ok2 || len(mdhd) < 20 {
			continue
		}
		ts := mp4U32(mdhd, 12) // v0:ctime mtime timescale(12) duration(16)
		if mdhd[0] == 1 {
			if len(mdhd) < 32 {
				continue
			}
			ts = mp4U32(mdhd, 20)
		}
		if ts == 0 {
			continue
		}
		hdlr, okH := mp4Child(mdia, "hdlr")
		isVideo := okH && len(hdlr) >= 12 && string(hdlr[8:12]) == "vide"
		minf, okM := mp4Child(mdia, "minf")
		if !okM {
			continue
		}
		stbl, okS := mp4Child(minf, "stbl")
		if !okS {
			continue
		}
		if stblBody == nil { // 暂存首轨作候选(可能无视频轨)
			stblBody, timescale = stbl, ts
		}
		if isVideo { // 视频轨优先,命中即定
			stblBody, timescale = stbl, ts
			break
		}
	}
	if stblBody == nil || timescale == 0 {
		return nil
	}
	stts, ok := mp4Child(stblBody, "stts")
	stsc, ok2 := mp4Child(stblBody, "stsc")
	stco, ok3 := mp4Child(stblBody, "stco")
	co64 := false
	if !ok3 {
		stco, ok3 = mp4Child(stblBody, "co64")
		co64 = true
	}
	if !ok || !ok2 || !ok3 || len(stts) < 8 || len(stsc) < 8 || len(stco) < 4 {
		return nil
	}
	// stts:(count, delta) 序列
	nT := int(mp4U32(stts, 4))
	type td struct{ n, d uint32 }
	tts := make([]td, 0, nT)
	p := 8
	for i := 0; i < nT && p+8 <= len(stts); i++ {
		tts = append(tts, td{mp4U32(stts, p), mp4U32(stts, p+4)})
		p += 8
	}
	// stsc:(firstChunk, samplesPerChunk, sdi) 序列(1 基)
	nC := int(mp4U32(stsc, 4))
	type sc struct{ first, n uint32 }
	scs := make([]sc, 0, nC)
	p = 8
	for i := 0; i < nC && p+12 <= len(stsc); i++ {
		scs = append(scs, sc{mp4U32(stsc, p), mp4U32(stsc, p+4)})
		p += 12
	}
	// stco:chunk 文件偏移
	nO := int(mp4U32(stco, 4))
	offs := make([]int64, 0, nO)
	if co64 {
		p = 8
		for i := 0; i < nO && p+8 <= len(stco); i++ {
			offs = append(offs, int64(binary.BigEndian.Uint64(stco[p:p+8])))
			p += 8
		}
	} else {
		p = 8
		for i := 0; i < nO && p+4 <= len(stco); i++ {
			offs = append(offs, int64(mp4U32(stco, p)))
			p += 4
		}
	}
	if len(tts) == 0 || len(scs) == 0 || len(offs) == 0 {
		return nil
	}
	// 逐 chunk 发点:chunk 首样本时间 = 已累计时间;随后跳过本 chunk 样本
	var pts []seekPoint
	var timeU int64
	ti, left := 0, tts[0].n
	si := 0 // stsc 游标
	for ci := 0; ci < len(offs); ci++ {
		for si+1 < len(scs) && uint32(ci+1) >= scs[si+1].first {
			si++
		}
		n := scs[si].n
		pts = append(pts, seekPoint{float64(timeU) / float64(timescale), offs[ci]})
		for k := uint32(0); k < n; k++ {
			if left == 0 {
				ti++
				if ti >= len(tts) {
					break
				}
				left = tts[ti].n
			}
			timeU += int64(tts[ti].d)
			left--
		}
		if ti >= len(tts) && left == 0 && ci+1 < len(offs) {
			// stts 提前耗尽(畸形):后续 chunk 时间不再可信,截断
			break
		}
	}
	return downsampleSeekPoints(pts, 400)
}

// mp4ChildExcept/afterChild 辅助(收集多 trak 用):afterChild 返回容器中
// 指定类型首个子 atom 结束位置的字节游标。
func afterChild(body []byte, typ string) int {
	var off int64
	for off+8 <= int64(len(body)) {
		asize := int64(binary.BigEndian.Uint32(body[off : off+4]))
		t := string(body[off+4 : off+8])
		hdr := int64(8)
		if asize == 1 {
			if off+16 > int64(len(body)) {
				return len(body)
			}
			asize = int64(binary.BigEndian.Uint64(body[off+8 : off+16]))
			hdr = 16
		}
		if asize < hdr || off+asize > int64(len(body)) {
			return len(body)
		}
		if t == typ {
			return int(off + asize)
		}
		off += asize
	}
	return len(body)
}

// downsampleSeekPoints 按字节步长降采样并强制时间/偏移单调递增。
func downsampleSeekPoints(pts []seekPoint, maxN int) []seekPoint {
	if len(pts) <= maxN || len(pts) == 0 {
		return pts
	}
	step := float64(pts[len(pts)-1].off-pts[0].off) / float64(maxN)
	out := []seekPoint{pts[0]}
	var nextOff = float64(pts[0].off) + step
	for _, p := range pts[1:] {
		if float64(p.off) >= nextOff {
			out = append(out, p)
			nextOff = float64(p.off) + step
		}
	}
	// 末点兜底(步长取整可能漏掉)
	if out[len(out)-1].off != pts[len(pts)-1].off {
		out = append(out, pts[len(pts)-1])
	}
	return out
}

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
// 文件尾,头部缩到 mdat 起点);读 moov 前缀解 mvhd 时长 → 码率自适应;
// 最后整读 moov 解 stts/stsc/stco 构建"时间→字节"精确播放头定位表。
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
	// 内容起点(播放头内插锚点):faststart=moov 结束、moov 在尾=mdat 起点
	if r.headEnd > 0 {
		t.mu.Lock()
		if t.stream == st {
			st.metaEnd = r.headEnd
		}
		t.mu.Unlock()
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
		m.probeMP4SeekTable(t, st, cl, r.moovOff, r.moovEnd-r.moovOff, deadline)
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
	// 时长 + 定位表:moov 起点处可能夹 free/skip 小 atom,逐个跳到 moov
	moOff, moSize := m.probeMP4FindMoov(t, st, cl, r.mdatEnd, deadline)
	if dur, ok := mp4MoovSeconds(buf); ok {
		apply(nil, nil, nil, dur)
	}
	if moSize > 0 {
		m.probeMP4SeekTable(t, st, cl, moOff, moSize, deadline)
	}
}

// probeMP4FindMoov 在 moov 区域起点处跳过 free/skip/wide 小 atom 定位
// moov(atom 起点与完整尺寸)。找不到返回 (0,-1)。
func (m *Manager) probeMP4FindMoov(t *Task, st *taskStream, cl *bt.Client, at int64, deadline time.Time) (int64, int64) {
	for i := 0; i < 8 && at+16 <= st.size; i++ {
		hdr := make([]byte, 16)
		if !m.streamWaitPieces(t, st, cl, at, 16, deadline) ||
			streamReadAt(st, cl, at, hdr) != nil {
			return 0, -1
		}
		asize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		hdrLen := int64(8)
		if asize == 1 {
			asize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hdrLen = 16
		}
		if asize < hdrLen || at+asize > st.size {
			return 0, -1
		}
		if typ == "moov" {
			return at, asize
		}
		if typ != "free" && typ != "skip" && typ != "wide" {
			return 0, -1
		}
		at += asize
	}
	return 0, -1
}

// probeMP4SeekTable 整读 moov(等待其 pin 的 piece 落定)并构建/落账
// 时间→字节表。失败静默(红线回退锚点/均摊)。
func (m *Manager) probeMP4SeekTable(t *Task, st *taskStream, cl *bt.Client, off, size int64, deadline time.Time) {
	const maxMoov = 32 << 20
	if off < 0 || size < 8 || size > maxMoov {
		return
	}
	if !m.streamWaitPieces(t, st, cl, off, size, deadline) {
		return
	}
	mo := make([]byte, size)
	if streamReadAt(st, cl, off, mo) != nil {
		return
	}
	body := 8
	if mp4U32(mo, 0) == 1 { // 64 位长度头
		body = 16
	}
	if int64(body) >= size {
		return
	}
	pts := mp4BuildSeekPoints(mo[body:])
	if len(pts) == 0 {
		return
	}
	t.mu.Lock()
	if t.stream == st {
		st.seek = pts
	}
	t.mu.Unlock()
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
	var segDataOff int64
	timeScale := 1e6
	if n >= 16 && m.streamWaitPieces(t, st, cl, 0, n, deadline) {
		head := make([]byte, n)
		if streamReadAt(st, cl, 0, head) == nil {
			r := mkvWalkHead(head, st.size)
			dur = r.durSec
			segDataOff, timeScale = r.segDataOff, r.timeScale
			t.mu.Lock()
			if t.stream == st {
				if r.headEnd > 0 && r.headEnd <= st.size {
					st.metaEnd = r.headEnd // 内容起点=首个 Cluster
					streamApplyLocked(t, st, cl, st.piecesOf(0, r.headEnd), nil, nil, dur)
				} else if dur > 0 {
					streamApplyLocked(t, st, cl, nil, nil, nil, dur)
				}
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
			// Cues → 时间→字节表(播放头精确定位;与播放器同源)
			if idx := off - tailStart; idx >= 0 && idx+4 < int64(len(tail)) {
				if _, idn, ok1 := mkvElementID(tail[idx:]); ok1 {
					if sz, sn, unk, ok2 := mkvVint(tail[idx+int64(idn):]); ok2 && !unk {
						b := idx + int64(idn+sn)
						if e := b + int64(sz); e >= b && e <= int64(len(tail)) {
							if pts := mkvBuildSeekPoints(tail[b:e], segDataOff, timeScale); len(pts) > 0 {
								t.mu.Lock()
								if t.stream == st {
									st.seek = pts
								}
								t.mu.Unlock()
							}
						}
					}
				}
			}
			return
		}
	}
}
