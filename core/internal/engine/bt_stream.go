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
	"os"
	"path/filepath"
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
	// ErrStreamNoSession 任务没有运行中的会话(暂停/错误/已完成)。
	ErrStreamNoSession = errors.New("会话未运行,无法边下边播")
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
}

// streamConn 单条流连接的服务区间记账(文件内字节,供播放头时间→字节内插
// 换算:浏览器的 buffered 时间区间与该连接吐出的字节区间一一对应)。
type streamConn struct {
	start int64 // Range 起点到达时的服务起点
	end   int64 // 已服务边界(每吐一块推进)
	probe bool  // 探测连接(有界小 Range:moov/ftyp 重读等)。不拥有游标、
	           // 不进活动服务区间;真正吐数据且主人离开后可接管(兼容只发
	           // 有界请求的播放器)
}

// activeConnLocked 返回游标权所属连接的服务区间记录(当前属主;属主已离开
// 则现存最新非探测连接)。探测连接不在此列——浏览器回读文件头的小 Range
// 会抢走"最新"名分,把游标饿死在头部。须持所属 Task 的 mu。
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
	// 以下供播放面板分块图定位引擎优先级(piece 均为全局下标;-1 = 未知/无)
	FileOffset   int64 `json:"fileOffset,omitempty"`   // 文件在种子内的全局起始偏移(byte→piece 换算)
	CursorPiece  int   `json:"cursorPiece"`            // 下载游标(窗口起点)
	Ahead        int   `json:"ahead"`                  // 窗口宽度(piece 数)
	Depth        int   `json:"depth"`                  // 顺序深度:前沿实际可并行认领的 piece 数
	PinHeadFirst int   `json:"pinHeadFirst,omitempty"` // 头部元数据 pin 区间
	PinHeadLast  int   `json:"pinHeadLast,omitempty"`
	PinTailFirst int   `json:"pinTailFirst,omitempty"` // 尾部 pin(faststart 确认后不再下发)
	PinTailLast  int   `json:"pinTailLast,omitempty"`
	// ServeStart/ServeEnd 活跃连接的服务字节区间(文件内;end > start 才有效):
	// 前端把 video.buffered 的时间区间与之对齐,做播放头时间→字节的内插换算
	//(替代全片恒定码率均摊,VBR 下前者准一个量级)。不可省略:serveStart=0
	//(从头播)同样有效。
	ServeStart int64 `json:"serveStart"`
	ServeEnd   int64 `json:"serveEnd"`
}

// btNormPath 归一路径分隔符(\ → /,与 WantedPiecesFor 的口径一致)。
func btNormPath(p string) string { return strings.Join(strings.Split(p, "\\"), "/") }

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

// BTStreamOpen 开启(或切换)任务的边下边播。file 为种子内相对路径;
// 空 = 单文件种子取唯一文件,多文件优先取最大的媒体文件(无媒体扩展名
// 则取最大文件)。幂等:重复打开同一文件只刷新活跃时间,换文件则整体
// 重算 pin/窗口。
func (m *Manager) BTStreamOpen(id, file string) (*BTStreamInfo, error) {
	t := m.get(id)
	if t == nil {
		return nil, ErrNotFound
	}
	t.mu.Lock()
	kind, status := t.Kind, t.Status
	wantedFiles := t.WantedFiles
	prev := t.stream
	t.mu.Unlock()
	if kind != KindBT {
		return nil, errors.New("仅 BT 任务支持边下边播")
	}
	if status != StatusRunning && status != StatusSeeding {
		if status == StatusCompleted {
			return nil, errors.New("任务已完成,请直接打开文件")
		}
		return nil, ErrStreamNoSession
	}
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
	if cl == nil {
		return nil, ErrStreamNoSession
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
	if prev != nil && prev.file == fp {
		// 同文件重复打开(播放器重连/新开探测连接):只刷新活跃时间
		prev.lastAt = now
	} else {
		if !cl.Completed() {
			cl.SetStreaming(pinHead, pinTail, ahead)
		}
		t.stream = &taskStream{
			file: fp, fi: fi, size: size, off: meta.Files[fi].Offset,
			pieceLen: meta.PieceLength, first: first, last: last,
			opened: now, lastAt: now, conns: map[int64]*streamConn{},
		}
	}
	st := t.stream
	t.mu.Unlock()
	return &BTStreamInfo{
		File:        st.file,
		Size:        st.size,
		ContentType: streamContentType(st.file),
		PieceLen:    st.pieceLen,
		FirstPiece:  st.first,
		LastPiece:   st.last,
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
	if st != nil {
		st.lastAt = time.Now()
		if rec := st.conns[seq]; rec != nil {
			rec.start, rec.end = fileOff, fileOff // 该连接的服务区间自 Range 起点重新记账
			rec.probe = !streaming
		}
		if streaming {
			st.lastSeq = seq // 播放语义连接接管游标权(探测连接无此资格)
		}
	}
	t.mu.Unlock()
	if st == nil || cl == nil || !streaming {
		return
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
	if st == nil || cl == nil {
		return ErrStreamNoSession
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

// BTStreamRead 读取文件内偏移的一段数据到 buf(前置 BTStreamWait 通过;
// 覆盖 piece 已全部落定,数据必在盘上)。
func (m *Manager) BTStreamRead(id string, fileOff int64, buf []byte) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	st, cl := t.stream, t.btcl
	t.mu.Unlock()
	if st == nil || cl == nil {
		return ErrStreamNoSession
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
	var serve *streamConn
	if st != nil {
		serve = st.activeConnLocked() // conns/lastSeq 受 t.mu 保护,锁内取
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
	if st.off > 0 {
		out.FileOffset = st.off
	}
	if cl != nil {
		bits := cl.HaveBits()
		n := st.last - st.first + 1
		packed := make([]byte, (n+7)/8)
		for i := 0; i < n; i++ {
			if st.first+i < len(bits) && bits[st.first+i] {
				packed[i/8] |= 1 << (7 - uint(i%8))
			}
		}
		out.Pieces = base64.StdEncoding.EncodeToString(packed)
		// 引擎优先级位置(分块图:窗口带/前沿深度/pin 区间/下载游标)
		if cur, ahead, depth, ph, pt, tailOn, ok := cl.StreamState(); ok {
			out.CursorPiece = cur
			out.Ahead = ahead
			out.Depth = depth
			if len(ph) > 0 {
				out.PinHeadFirst, out.PinHeadLast = ph[0], ph[len(ph)-1]
			}
			if tailOn && len(pt) > 0 {
				out.PinTailFirst, out.PinTailLast = pt[0], pt[len(pt)-1]
			}
		}
		// 活跃连接服务区间(播放头时间→字节内插换算)。刚 seek(end==start)
		// 的零长度区间也下发:前端 span=0 时红线直接贴 seek 点,优于均摊回退
		if serve != nil && serve.end >= serve.start && (serve.start != 0 || serve.end != 0) {
			out.ServeStart, out.ServeEnd = serve.start, serve.end
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
