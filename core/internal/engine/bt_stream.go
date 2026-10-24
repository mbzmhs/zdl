// 边下边播(BT 流媒体):引擎侧编排。
// 数据面在 bt.Client/picker(优先级)与 storage(读取),本文件负责:
// 文件解析与校验、元数据 pin/播放窗口计算、播放头游标策略、连接登记与
// 空闲回收、状态快照。HTTP 语义(Range/206/flush)在 server 层。
package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zdownloader/internal/bt"
)

const (
	streamPinBytes   = 1 << 20        // 文件头/尾元数据 pin 覆盖字节数(容器头、moov/cues)
	streamAheadBytes = 32 << 20       // 播放窗口目标缓冲字节
	streamAheadMin   = 4              // 窗口宽度下限(piece 数)
	streamAheadMax   = 64             // 窗口宽度上限
	streamIdle       = 5 * time.Minute // 无流请求自动还原优先级(防陈旧游标拖慢总进度)
)

var (
	// ErrStreamNotWanted 文件不在当前下载选择内(前端引导扩选)。
	ErrStreamNotWanted = errors.New("文件不在下载选择内,请先在文件页勾选")
	// ErrStreamV2Deferred v2 磁力缺 piece layers:整文件延迟校验,piece
	 // 永不"落定",流会无限等待,直接拒绝。
	ErrStreamV2Deferred = errors.New("该文件需整体校验,暂不支持边下边播")
	// ErrStreamNoSession 任务没有运行中的会话(暂停/错误)。
	ErrStreamNoSession = errors.New("会话未运行,无法边下边播")
	// ErrStreamStale 迟到的旧代次开流请求(快速切集后被放弃的播放器资源
	// 重试/迟达):接受它会把整个流会话切回旧文件——位图/游标/pin 全部
	// 跟着横跳。带旧代次的请求直接拒绝;不带代次(外部播放器)不受限。
	ErrStreamStale = errors.New("旧播放会话的请求")
)

// btMediaExts 可边下边播的媒体扩展名(默认文件挑选与前端播放入口共用口径)。
var btMediaExts = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true, ".avi": true, ".mov": true,
	".ts": true, ".m4v": true, ".flv": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

// taskStream 任务的边下边播运行时会话(不持久化)。游标策略:最新打开的
// 连接在 Range 到达时可移动游标(seek 即时响应;探测连接读的都是已 pin
// 的头尾 piece,即使短暂抢游标也无害);任何连接实际吐出数据后游标跟随
// 服务位置推进(窗口贴着真实播放头)。
type taskStream struct {
	file     string    // 种子内相对路径
	fi       int       // 文件下标
	size     int64     // 文件长度
	off      int64     // 文件在种子内的全局起始偏移
	pieceLen int64
	first    int       // 文件覆盖的首 piece
	last     int       // 文件覆盖的末 piece
	opened   time.Time
	lastAt   time.Time  // 最近一次流请求(空闲回收依据)
	seqGen   int64      // 连接序号发生器(仅分配;打开 ≠ 拥有游标权)
	lastSeq  int64      // 游标权属主连接(0 = 无属主;仅 Pos(streaming) 或
	                   // 吐数接管可赋值——探测连接、幽灵连接均不可)
	conns    map[int64]*streamConn // 打开中的流连接(含各自服务区间)
	// pin 事实源(开流默认 1MB 头/尾;容器探测精化、预热合并后经
	// streamApplyLocked 下发 picker;切文件即随会话整体作废)
	pinHead []int
	pinTail []int
	tailOn  bool
	warm    map[string][]int // 预热文件 → 并入 pin 的 piece 集(剧集下一集)
	bitrate float64          // 容器探测出的平均码率 B/s(0 = 未知;窗口自适应与前端提示)
	metaOn  bool             // 容器探测已启动(同文件重开不重复探测)
	// done 完成态直读模式(任务已完成/做种结束,会话已拆):不做任何
	// picker 优先级,全部请求直读最终文件——边看边下途中任务完成转做种、
	// 做种达标停止时,正在播放的流重连后走此模式无缝续播(旧实现返回
	// "已完成"409,播放断流且无法重开);path 为最终文件绝对路径。
	done bool
	path string
	// metaEnd 元数据区结束偏移=可播内容起点(探测得出:faststart=moov 结束,
	// moov 在尾=mdat 起点,MKV=首个 Cluster;0=未知)。前端播放头做
	// "时间→字节"内插的锚点:头部 moov 占几 MB 但播放时间为零,不锚定会让
	// 红线系统性落后真实位置一个 moov 的量(实测 2.09MB 头 moov 文件)。
	metaEnd int64
	// seek 容器索引解析出的"时间(秒)→字节"采样表(stts/stsc/stco 或
	// Cues;探测后台填充)。前端播放头据此精确换算——与播放器同源索引,
	// 取代均摊/内插等估算。
	seek []seekPoint
}

// streamConn 单条流连接的服务区间记账(文件内字节,供播放头时间→字节内插
// 换算:浏览器的 buffered 时间区间与该连接吐出的字节区间一一对应)。
type streamConn struct {
	start  int64     // Range 起点到达时的服务起点
	end    int64     // 已服务边界(每吐一块推进)
	probe  bool      // 探测连接(有界小 Range:moov/ftyp 重读等)。不拥有游标、
	                // 不进活务服务区间;真正吐数据且主人离开后可接管(兼容只发
	                // 有界请求的播放器)
	lastAt time.Time // 最近一次吐数(识别浏览器前向预读连接:属主活跃吐数中时,远处的新开放连接是预读而非拖动)
}

// activeConnLocked 返回游标权所属连接的服务区间记录(当前属主;属主已离开
// 则现存最新非探测连接)。探测连接不在此列——浏览器回读文件头的小 Range
// 会抢走"最新"名分,把游标饿死在头部。须持所属 Task 的 mu。
// streamIsReadaheadLocked 判定新到达的开放式 Range 是否为浏览器前向预读
// 而非真实拖动:预读发生在主连接活跃吐数时(播放中持续有数据流),
// 拖动时浏览器会先放弃旧连接(属主停滞/消失)。误当拖动会把游标/
// 窗口/锚点带到预读区域(实测红线蹩空白区后又跳回)。须持 t.mu。
func streamIsReadaheadLocked(st *taskStream, fileOff int64) bool {
	owner, live := st.conns[st.lastSeq]
	if !live || owner.probe {
		return false
	}
	if time.Since(owner.lastAt) > 3*time.Second {
		return false // 属主停滞:更像拖动后的新播放连接
	}
	// 远超属主服务前沿的开放请求 = 预读。距离阈值
	// min(8MB, 文件/4):小跳转(<阈值)即使属主活跃也按拖动处理(保拖动体验)
	far := int64(8 << 20)
	if q := st.size / 4; q < far {
		far = q
	}
	return fileOff > owner.end+far
}

func (st *taskStream) activeConnLocked() *streamConn {
	if rec, live := st.conns[st.lastSeq]; live && !rec.probe {
		return rec
	}
	newest := int64(0)
	var out *streamConn
	for s, rec := range st.conns {
		if rec.probe || s <= newest {
			continue
		}
		newest, out = s, rec
	}
	return out
}

// BTStreamInfo 边下边播会话信息。
type BTStreamInfo struct {
	File        string `json:"file"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	PieceLen    int64  `json:"pieceLen"`
	FirstPiece  int    `json:"firstPiece"`
	LastPiece   int    `json:"lastPiece"`
	// FileOffset 文件在种子内的全局起始偏移(服务循环做 piece 内块定位)。
	FileOffset int64 `json:"fileOffset,omitempty"`
}

// BTStreamStatus 流播状态(前端缓冲可视化/播放面板)。
type BTStreamStatus struct {
	Streaming   bool   `json:"streaming"`
	File        string `json:"file,omitempty"`
	Size        int64  `json:"size"`
	PieceLen    int64  `json:"pieceLen"`
	FirstPiece  int    `json:"firstPiece"`
	LastPiece   int    `json:"lastPiece"`
	Pieces      string `json:"pieces,omitempty"` // 已落定位图(base64,MSB 在前,仅文件区间)
	ContentType string `json:"contentType,omitempty"`
	// 以下供播放面板分块图定位引擎优先级(piece 均为全局下标;-1 = 未知/无)。
	// 注意:不可加 omitempty——头部 pin 恒自 piece 0 起(单文件种子),
	// 零值会被 JSON 丢弃,前端拿不到字段就永远画不出头部 pin(实测教训)
	FileOffset   int64 `json:"fileOffset,omitempty"`   // 文件在种子内的全局起始偏移(byte→piece 换算)
	CursorPiece  int   `json:"cursorPiece"`            // 下载游标(窗口起点)
	Ahead        int   `json:"ahead"`                  // 窗口宽度(piece 数)
	Depth        int   `json:"depth"`                  // 顺序深度:前沿实际可并行认领的 piece 数
	PinHeadFirst int   `json:"pinHeadFirst"`           // 头部元数据 pin 区间
	PinHeadLast  int   `json:"pinHeadLast"`
	PinTailFirst int   `json:"pinTailFirst"`           // 尾部 pin(faststart 确认后不再下发)
	PinTailLast  int   `json:"pinTailLast"`
	// ServeStart/ServeEnd 活跃连接的服务字节区间(文件内;end > start 才有效):
	// 前端把 video.buffered 的时间区间与之对齐,做播放头时间→字节的内插换算
	//(替代全片恒定码率均摊,VBR 下前者准一个量级)。不可省略:serveStart=0
	//(从头播)同样有效。
	ServeStart int64 `json:"serveStart"`
	ServeEnd   int64 `json:"serveEnd"`
	// Bitrate 容器探测的平均码率 B/s(0 = 未知);Speed 任务当前下载速度 B/s。
	// 前端据 Speed < Bitrate 提示"速度低于码率,可能反复缓冲"。
	Bitrate float64 `json:"bitrate,omitempty"`
	Speed   float64 `json:"speed,omitempty"`
	// Warming 剧集预热已触发(下一集头/尾 piece 已并入最高优先级);
	// WarmReady 预热的 piece 已全部落定(元数据在盘,切集秒开)。
	Warming   bool `json:"warming,omitempty"`
	WarmReady bool `json:"warmReady,omitempty"`
	// MetaEnd 元数据区结束偏移=可播内容起点(容器探测;0=未知)。播放头
	// 时间→字节内插以此为锚:头部 moov/EBML 区占用字节但播放时间为零。
	MetaEnd int64 `json:"metaEnd,omitempty"`
	// Seek 容器索引"时间(秒)→字节"采样表(stts/stsc/stco 或 Cues;探测
	// 后台解析,≤400 点)。前端播放头据此二分查找精确换算,优先于一切估算。
	Seek [][]float64 `json:"seek,omitempty"`
	// Conns/OwnerSeq 连接记账明细(诊断:定位游标/服务区间/红线错位环节)。
	Conns    []BTStreamConnView `json:"conns,omitempty"`
	OwnerSeq int64              `json:"ownerSeq,omitempty"`
}

// BTStreamConnView 单条流连接的服务区间快照(诊断用)。
type BTStreamConnView struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Probe bool  `json:"probe,omitempty"`
}

// btNormPath 归一路径分隔符(\ → /,与 WantedPiecesFor 的口径一致)。
func btNormPath(p string) string { return strings.Join(strings.Split(p, "\\"), "/") }

// piecesWithin 过滤出 [lo,hi] 区间内的 piece 下标(不修改入参切片——
// StreamState 返回的是 picker 内部切片的引用)。
func piecesWithin(ps []int, lo, hi int) []int {
	var out []int
	for _, p := range ps {
		if p >= lo && p <= hi {
			out = append(out, p)
		}
	}
	return out
}

// streamContentType 按扩展名给 Content-Type(未知给 octet-stream,本地
// 播放器按内容探测照样能播)。
func streamContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".mpg", ".mpeg":
		return "video/mpeg"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".ogg", ".oga":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

// btStreamMeta 从任务归档种子解析元数据(与 TaskDetail 未运行路径同源)。
func btStreamMeta(t *Task) (*bt.MetaInfo, error) {
	t.mu.Lock()
	tpath := t.TorrentPath
	t.mu.Unlock()
	if tpath == "" {
		return nil, errors.New("种子元数据不可用")
	}
	raw, err := os.ReadFile(tpath)
	if err != nil {
		return nil, errors.New("种子归档读取失败")
	}
	meta, err := bt.ParseTorrent(raw)
	if err != nil {
		return nil, errors.New("种子归档解析失败")
	}
	return meta, nil
}

// btFinalFilePath 已完成任务的最终文件绝对路径(与做种恢复同口径:
// t.Dir/<t.Filename> 为单文件本身/多文件种子根目录,任务完成时 Filename
// 已是最终落盘名)。文件缺失/尺寸不符视为数据异常,拒绝开流。
func btFinalFilePath(t *Task, meta *bt.MetaInfo, fi int) (string, error) {
	t.mu.Lock()
	dir, name := t.Dir, t.Filename
	t.mu.Unlock()
	if name == "" {
		name = meta.Name
	}
	p := filepath.Join(dir, name)
	if meta.Multi {
		p = filepath.Join(p, filepath.Join(meta.Files[fi].Path...))
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() != meta.Files[fi].Length {
		return "", errors.New("已完成文件不在盘上或状态异常,无法播放")
	}
	return p, nil
}

// readFinalRange 完成态直读:按需开关句柄读最终文件一段(512KB 级分片
// 下每秒一两次 open,开销可忽略;无生命周期管理,天然免疫改名/删除竞态)。
func readFinalRange(path string, off int64, buf []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, rerr := f.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if rerr != nil {
		return rerr
	}
	return io.EOF // 短读:请求区间越过文件尾(防御,正常不可达)
}

// BTStreamOpen 开启(或切换)任务的边下边播。file 为种子内相对路径;
// 空 = 单文件种子取唯一文件,多文件优先取最大的媒体文件(无媒体扩展名
// 则取最大文件)。幂等:重复打开同一文件只刷新活跃时间,换文件则整体
// 重算 pin/窗口。epoch 为调用方(前端)的单调代次:0 = 不参与代次判定
// (外部播放器);携带且小于任务当前代次的请求视为迟到旧请求,拒绝
// (ErrStreamStale)——防快速切集后被放弃的播放器资源把会话拽回旧文件。
func (m *Manager) BTStreamOpen(id, file string, epoch int64) (*BTStreamInfo, error) {
	t := m.get(id)
	if t == nil {
		return nil, ErrNotFound
	}
	t.mu.Lock()
	kind, status := t.Kind, t.Status
	wantedFiles := t.WantedFiles
	prev := t.stream
	// 代次门:快速切集后被放弃的播放器资源(重试/迟到的旧连接)携带旧
	// 代次,放行会把会话切回旧文件——位图/游标/pin 随之横跳。旧代次一律
	// 拒绝;0(外部播放器/测试)不参与判定,保持自由切换语义
	if epoch > 0 && epoch < t.streamEpoch {
		t.mu.Unlock()
		return nil, ErrStreamStale
	}
	if epoch > t.streamEpoch {
		t.streamEpoch = epoch
	}
	t.mu.Unlock()
	if kind != KindBT {
		return nil, errors.New("仅 BT 任务支持边下边播")
	}
	// 完成态放行(直读最终文件):边看边下途中任务完成转做种、做种达标
	// 停止,正在播放的流重连后经 done 模式无缝续播;暂停/错误等仍拒绝
	if status != StatusRunning && status != StatusSeeding && status != StatusCompleted {
		return nil, ErrStreamNoSession
	}
	done := status == StatusCompleted
	meta, err := btStreamMeta(t)
	if err != nil {
		return nil, err
	}
	fi := -1
	if file != "" {
		want := btNormPath(file)
		for i := range meta.Files {
			if strings.Join(meta.Files[i].Path, "/") == want {
				fi = i
				break
			}
		}
		if fi < 0 {
			return nil, fmt.Errorf("种子中不存在文件: %s", file)
		}
	} else if len(meta.Files) > 0 {
		best, bestSize := -1, int64(-1)
		anyMedia, mediaSize := -1, int64(-1)
		for i := range meta.Files {
			sz := meta.Files[i].Length
			if sz > bestSize {
				best, bestSize = i, sz
			}
			if btMediaExts[strings.ToLower(filepath.Ext(strings.Join(meta.Files[i].Path, "/")))] && sz > mediaSize {
				anyMedia, mediaSize = i, sz
			}
		}
		fi = best
		if anyMedia >= 0 {
			fi = anyMedia
		}
	}
	if fi < 0 || len(meta.Files) == 0 {
		return nil, errors.New("种子文件列表不可用")
	}
	fp := strings.Join(meta.Files[fi].Path, "/")
	if len(wantedFiles) > 0 {
		ok := false
		for _, w := range wantedFiles {
			if btNormPath(w) == fp {
				ok = true
				break
			}
		}
		if !ok {
			return nil, ErrStreamNotWanted
		}
	}
	if !meta.LayerKnown(fi) {
		return nil, ErrStreamV2Deferred
	}
	t.mu.Lock()
	cl := t.btcl
	t.mu.Unlock()
	// 完成态不需要会话(直读最终文件);运行/做种态必须有活会话
	if !done && cl == nil {
		return nil, ErrStreamNoSession
	}
	var path string
	if done {
		if path, err = btFinalFilePath(t, meta, fi); err != nil {
			return nil, err
		}
	}

	first, last := meta.FilePieceRange(fi)
	pinN := int(streamPinBytes / meta.PieceLength)
	if pinN < 1 {
		pinN = 1
	}
	pinHead := make([]int, 0, pinN)
	for p := first; p <= last && p < first+pinN; p++ {
		pinHead = append(pinHead, p)
	}
	pinTail := make([]int, 0, pinN)
	for p := last; p >= first && p > last-pinN; p-- {
		pinTail = append([]int{p}, pinTail...)
	}
	ahead := int((streamAheadBytes + meta.PieceLength - 1) / meta.PieceLength)
	if ahead < streamAheadMin {
		ahead = streamAheadMin
	}
	if ahead > streamAheadMax {
		ahead = streamAheadMax
	}

	size := meta.Files[fi].Length
	now := time.Now()
	t.mu.Lock()
	if prev != nil && prev.file == fp && prev.done == done {
		// 同文件同模式重复打开(播放器重连/新开探测连接):只刷新活跃时间
		prev.lastAt = now
	} else {
		if !done && !cl.Completed() {
			cl.SetStreaming(pinHead, pinTail, ahead)
		}
		t.stream = &taskStream{
			file: fp, fi: fi, size: size, off: meta.Files[fi].Offset,
			pieceLen: meta.PieceLength, first: first, last: last,
			opened: now, lastAt: now, conns: map[int64]*streamConn{},
			pinHead: pinHead, pinTail: pinTail, tailOn: true,
			done: done, path: path,
		}
	}
	st := t.stream
	probe := st != nil && !st.done && !st.metaOn && cl != nil && !cl.Completed()
	if probe {
		st.metaOn = true
	}
	t.mu.Unlock()
	if probe {
		// 容器元数据探测(一次性后台):精化头/尾 pin、faststart 解除尾部、
		// 时长→码率→窗口自适应。会话失配(换文件/停止/结束)自行退出。
		go m.btStreamProbe(t, st, cl)
	}
	return &BTStreamInfo{
		File:        st.file,
		Size:        st.size,
		ContentType: streamContentType(st.file),
		PieceLen:    st.pieceLen,
		FirstPiece:  st.first,
		LastPiece:   st.last,
		FileOffset:  st.off,
	}, nil
}

// BTStreamConnOpen 登记一条流连接,返回其序号(最新连接拥有 Range 游标权)。
func (m *Manager) BTStreamConnOpen(id string) (int64, error) {
	t := m.get(id)
	if t == nil {
		return 0, ErrNotFound
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream == nil {
		return 0, ErrStreamNoSession
	}
	t.stream.seqGen++
	seq := t.stream.seqGen
	t.stream.conns[seq] = &streamConn{}
	t.stream.lastAt = time.Now()
	return seq, nil
}

// BTStreamConnClose 注销流连接(播放器断开/seek 换连接)。
func (m *Manager) BTStreamConnClose(id string, seq int64) {
	t := m.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream != nil {
		delete(t.stream.conns, seq)
	}
}

// BTStreamPos Range 请求到达。streaming 区分请求语义:开放式 Range
// (bytes=X- 到文件尾,播放/seek)接管游标权并拨游标;有界小 Range(探测、
// moov/ftyp 回读)只记自己的服务区间,不抢游标——否则浏览器回读文件头的
// 小请求会成为"最新连接",把主播放流饿死在头部位置。
func (m *Manager) BTStreamPos(id string, seq, fileOff int64, streaming bool) {
	t := m.get(id)
	if t == nil {
		return
	}
	if fileOff < 0 {
		fileOff = 0
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	owns := false
	if st != nil {
		st.lastAt = time.Now()
		if rec := st.conns[seq]; rec != nil {
			rec.start, rec.end = fileOff, fileOff // 该连接的服务区间自 Range 起点重新记账
			rec.probe = !streaming || streamIsReadaheadLocked(st, fileOff)
			if streaming && !rec.probe {
				st.lastSeq = seq // 播放语义连接接管游标权(探测/预读连接无此资格)
				owns = true
			}
		}
	}
	t.mu.Unlock()
	if !owns || cl == nil {
		return // 探测/预读连接不拨游标
	}
	cl.SetStreamCursorOff(st.off + fileOff)
}

// BTStreamServed 每向播放器吐出一块数据即推进游标(窗口贴真实播放位置)。
// 连接门控:仅最新连接的吐数才推进游标——拖动后旧连接在被播放器放弃之前
// 仍在吐数据,其位置可能在新播放头之外,无条件推进会拖着游标跑(实测回拖
// 时游标跑到播放头前面)。最新连接已离开(如秒开即关的探测连接)时,游标权
// 移交现存最新的连接,避免冻结。
func (m *Manager) BTStreamServed(id string, seq, fileOff int64) {
	t := m.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	if st != nil {
		st.lastAt = time.Now()
		if rec := st.conns[seq]; rec != nil && fileOff > rec.end {
			rec.end = fileOff // 本连接服务边界推进(逐连接记账,不受门控影响)
			rec.lastAt = time.Now()
		}
		if seq != st.lastSeq {
			if rec0, live := st.conns[st.lastSeq]; live && !rec0.probe {
				t.mu.Unlock()
				return // 属主(播放连接)在场:本连接的吐数不代表播放头
			}
			// 属主缺位:游标权移交现存最新的非探测连接;本连接非其人则让路。
			// 已关闭的幽灵连接(记账已删)绝不接管——迟到吐数会把游标拽回旧路径
			newest := int64(0)
			for s, rec := range st.conns {
				if !rec.probe && s > newest {
					newest = s
				}
			}
			st.lastSeq = newest
			if newest != seq {
				if newest != 0 {
					t.mu.Unlock()
					return
				}
				// 无任何非探测候选:允许正在吐数且仍在场的连接接管
				//(纯有界 Range 播放器的每段请求都按探测记账,靠此路径推进)
				if _, live := st.conns[seq]; !live {
					t.mu.Unlock()
					return
				}
				st.lastSeq = seq
			}
		}
	}
	t.mu.Unlock()
	if st == nil || cl == nil {
		return
	}
	cl.SetStreamCursorOff(st.off + fileOff)
}

// BTStreamWait 等待 [fileOff, fileOff+n)(文件内偏移)覆盖的 piece 全部
// 落定。先取信号再查状态,落定与唤醒之间无丢事件窗口;2s 兜底轮询。
func (m *Manager) BTStreamWait(ctx context.Context, id string, fileOff, n int64) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || (!st.done && cl == nil) {
		return ErrStreamNoSession
	}
	if st.done {
		return nil // 完成态直读:全部数据已在盘,无需等待
	}
	off := st.off + fileOff
	for {
		sig := cl.StreamWaitSignal()
		if cl.StreamPieceDone(off, n) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sig:
		case <-time.After(2 * time.Second):
		}
	}
}

// BTStreamPieceDone 立即检查 fileOff 所在 piece 是否已落定(盘可读)。
func (m *Manager) BTStreamPieceDone(id string, fileOff int64) bool {
	t := m.get(id)
	if t == nil {
		return false
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || (!st.done && cl == nil) {
		return false
	}
	if st.done {
		return true // 完成态:整文件在盘恒可读
	}
	return cl.StreamPieceDone(st.off+fileOff, 1)
}

// BTStreamWaitBlock 块粒度流播等待:阻塞至 fileOff 所在块可用——整片
// 落定(pieceDone=true,走盘读)或该块已内存到货(arrived=true,走攒块
// 直读,不必等整片 SHA-1)。溢写/未开始的 piece 块状态不可知,只能等
// 整片(退回 piece 粒度,行为同旧版)。先取信号再查状态,无丢事件窗口。
func (m *Manager) BTStreamWaitBlock(ctx context.Context, id string, fileOff int64) (pieceDone, arrived bool, err error) {
	t := m.get(id)
	if t == nil {
		return false, false, ErrNotFound
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || (!st.done && cl == nil) {
		return false, false, ErrStreamNoSession
	}
	if st.done {
		return true, false, nil // 完成态直读:恒走盘读快路径
	}
	g := st.off + fileOff
	for {
		sig := cl.StreamWaitSignal()
		if cl.StreamPieceDone(g, 1) {
			return true, false, nil
		}
		if cl.StreamBlockArrived(g) {
			return false, true, nil
		}
		select {
		case <-ctx.Done():
			return false, false, ctx.Err()
		case <-sig:
		case <-time.After(2 * time.Second):
		}
	}
}

// BTStreamBlockRead 内存攒块直读:拷贝 fileOff 起 len(buf) 字节(调用方
// 保证块已到货且不跨块)。false=缓冲恰被校验取走等,调用方重查 piece
// 落定后改走盘读。完成态不适用(恒走盘读)。
func (m *Manager) BTStreamBlockRead(id string, fileOff int64, buf []byte) bool {
	t := m.get(id)
	if t == nil {
		return false
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || st.done || cl == nil {
		return false
	}
	return cl.StreamBlockRead(st.off+fileOff, buf)
}

// BTStreamRead 读取文件内偏移的一段数据到 buf(前置 BTStreamWait 通过;
// 覆盖 piece 已全部落定,数据必在盘上)。完成态直读最终文件。
func (m *Manager) BTStreamRead(id string, fileOff int64, buf []byte) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || (!st.done && cl == nil) {
		return ErrStreamNoSession
	}
	if st.done {
		return readFinalRange(st.path, fileOff, buf)
	}
	return cl.StreamReadOff(st.off+fileOff, buf)
}

// BTStreamStatus 当前流播状态(含文件区间的已落定位图,base64 打包)。
func (m *Manager) BTStreamStatus(id string) (*BTStreamStatus, error) {
	t := m.get(id)
	if t == nil {
		return nil, ErrNotFound
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	kind := t.Kind
	speed, bitrate, metaEnd := float64(t.speed), 0.0, int64(0)
	var serve *streamConn
	var warmSets map[string][]int
	var conns []BTStreamConnView
	var seekPts []seekPoint
	owner := int64(0)
	if st != nil {
		serve = st.activeConnLocked() // conns/lastSeq 受 t.mu 保护,锁内取
		bitrate = st.bitrate
		metaEnd = st.metaEnd
		warmSets = st.warm
		seekPts = st.seek
		owner = st.lastSeq
		for seq, c := range st.conns {
			conns = append(conns, BTStreamConnView{Start: c.start, End: c.end, Probe: c.probe})
			_ = seq
		}
		sort.Slice(conns, func(i, j int) bool { return conns[i].Start < conns[j].Start })
	}
	t.mu.Unlock()
	if kind != KindBT {
		return nil, errors.New("仅 BT 任务支持边下边播")
	}
	out := &BTStreamStatus{}
	if st == nil {
		return out, nil
	}
	out.Streaming = true
	out.File = st.file
	out.Size = st.size
	out.PieceLen = st.pieceLen
	out.FirstPiece = st.first
	out.LastPiece = st.last
	out.ContentType = streamContentType(st.file)
	out.CursorPiece = -1
	out.PinHeadFirst, out.PinHeadLast = -1, -1
	out.PinTailFirst, out.PinTailLast = -1, -1
	if st.off > 0 {
		out.FileOffset = st.off
	}
	out.Bitrate = bitrate
	out.Speed = speed
	out.MetaEnd = metaEnd
	if len(seekPts) > 0 {
		tab := make([][]float64, len(seekPts))
		for i, p := range seekPts {
			tab[i] = []float64{p.t, float64(p.off)}
		}
		out.Seek = tab
	}
	out.Conns = conns
	out.OwnerSeq = owner
	if st.done {
		// 完成态:全量位图(整文件在盘),无窗口/pin/游标可言
		n := st.last - st.first + 1
		packed := make([]byte, (n+7)/8)
		for i := range packed {
			packed[i] = 0xFF
		}
		if r := uint(n % 8); r != 0 {
			packed[len(packed)-1] &= byte(0xFF << (8 - r))
		}
		out.Pieces = base64.StdEncoding.EncodeToString(packed)
	} else if cl != nil {
		bits := cl.HaveBits()
		n := st.last - st.first + 1
		packed := make([]byte, (n+7)/8)
		for i := 0; i < n; i++ {
			if st.first+i < len(bits) && bits[st.first+i] {
				packed[i/8] |= 1 << (7 - uint(i%8))
			}
		}
		out.Pieces = base64.StdEncoding.EncodeToString(packed)
		// 引擎优先级位置(分块图:窗口带/前沿深度/pin 区间/下载游标)。
		// pin 展示口径:只报当前文件区间内的 piece——预热下一集的 pin 在本
		// 文件区间外,按"首~尾"夹逼画带会把整个进度条涂黄;跨文件的共享
		// 边界 piece 仍在区间内,如实显示
		if cur, ahead, depth, ph, pt, tailOn, ok := cl.StreamState(); ok {
			out.CursorPiece = cur
			out.Ahead = ahead
			out.Depth = depth
			ph = piecesWithin(ph, st.first, st.last)
			pt = piecesWithin(pt, st.first, st.last)
			if len(ph) > 0 {
				out.PinHeadFirst, out.PinHeadLast = ph[0], ph[len(ph)-1]
			}
			if tailOn && len(pt) > 0 {
				out.PinTailFirst, out.PinTailLast = pt[0], pt[len(pt)-1]
			}
		}
	}
	// 活跃连接服务区间(播放头时间→字节内插换算)。刚 seek(end==start)
	// 的零长度区间也下发:前端 span=0 时红线直接贴 seek 点,优于均摊回退。
	// 完成态同样下发(done 会话无 cl,但记账在 conns 上)
	if serve != nil && serve.end >= serve.start && (serve.start != 0 || serve.end != 0) {
		out.ServeStart, out.ServeEnd = serve.start, serve.end
	}
	// 剧集预热状态:已触发 + 预热的 piece 是否全部落定(黄点=下载中,
	// 绿点=元数据已在盘,切集秒开)
	if len(warmSets) > 0 {
		out.Warming = true
		if cl != nil {
			ready := true
		warm:
			for _, ps := range warmSets {
				for _, p := range ps {
					if !cl.StreamPieceDone(int64(p)*st.pieceLen, 1) {
						ready = false
						break warm
					}
				}
			}
			out.WarmReady = ready
		}
	}
	return out, nil
}

// BTStreamStop 停止边下边播并还原下载优先级。幂等。
func (m *Manager) BTStreamStop(id string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	cl := t.btcl
	t.stream = nil
	t.mu.Unlock()
	if cl != nil {
		cl.ClearStreaming()
	}
	return nil
}

// BTStreamWarm 剧集预热:把另一文件(通常下一集)的头/尾元数据 piece 并入
// pin 常驻最高优先级,切集时索引已在盘、秒开。不切换播放会话、不移动游标。
// 幂等:同一文件重复预热只记账一次。
func (m *Manager) BTStreamWarm(id, file string) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	if file == "" {
		return errors.New("缺少文件参数")
	}
	t.mu.Lock()
	st := t.stream
	cl := t.btcl
	wantedFiles := t.WantedFiles
	t.mu.Unlock()
	if st == nil || (!st.done && cl == nil) {
		return ErrStreamNoSession
	}
	if st.done {
		return nil // 完成态:数据全在盘,无优先级可言,预热为无操作
	}
	meta, err := btStreamMeta(t)
	if err != nil {
		return err
	}
	want := btNormPath(file)
	fi := -1
	for i := range meta.Files {
		if strings.Join(meta.Files[i].Path, "/") == want {
			fi = i
			break
		}
	}
	if fi < 0 {
		return fmt.Errorf("种子中不存在文件: %s", file)
	}
	if len(wantedFiles) > 0 {
		ok := false
		for _, w := range wantedFiles {
			if btNormPath(w) == want {
				ok = true
				break
			}
		}
		if !ok {
			return ErrStreamNotWanted
		}
	}
	if !meta.LayerKnown(fi) {
		return ErrStreamV2Deferred
	}
	first, last := meta.FilePieceRange(fi)
	pinN := int(streamPinBytes / meta.PieceLength)
	if pinN < 1 {
		pinN = 1
	}
	pieces := make([]int, 0, pinN*2)
	for p := first; p <= last && p < first+pinN; p++ {
		pieces = append(pieces, p)
	}
	for p := last; p >= first && p > last-pinN; p-- {
		pieces = append(pieces, p)
	}
	t.mu.Lock()
	st = t.stream // 期间可能被换会话:重取复核
	cl = t.btcl
	if st == nil || cl == nil {
		t.mu.Unlock()
		return ErrStreamNoSession
	}
	if st.warm == nil {
		st.warm = map[string][]int{}
	}
	if _, done := st.warm[want]; done {
		t.mu.Unlock()
		return nil
	}
	st.warm[want] = pieces
	streamApplyLocked(t, st, cl, nil, nil, nil, 0)
	t.mu.Unlock()
	return nil
}

// btStreamIdleReclaim 无流请求持续 streamIdle 后自动还原优先级:
// 防止"看了一半关掉播放器",陈旧游标让下载长期卡在中段。btPump 周期调用。
func (m *Manager) btStreamIdleReclaim(t *Task, cl *bt.Client) {
	t.mu.Lock()
	if t.stream == nil || time.Since(t.stream.lastAt) <= streamIdle {
		t.mu.Unlock()
		return
	}
	t.stream = nil
	t.mu.Unlock()
	if cl != nil {
		cl.ClearStreaming()
	}
}
