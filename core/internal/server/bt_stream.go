// 边下边播 HTTP 端点:Range 语义 + 逐 piece 边落定边发出。
// 数据与优先级在 engine/bt(见 bt_stream.go 编排),本文件只负责 HTTP:
// 206/Content-Range/Content-Length 头、piece 对齐的服务循环、等待上限。
package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zdownloader/internal/bt"
)

const (
	// streamWaitPiece 单个数据块(≤1 个 piece)等待落定的上限。播放器读
	// 超时普遍在 30s 量级(Kodi/FFmpeg),宁断流触发播放器重试也别硬等。
	streamWaitPiece = 25 * time.Second
	// streamWriteChunk 单次向播放器写出的最大字节(piece 内切片;piece
	// 可达 16MB,切片给出 flush 粒度,断连时也能及时退出)。
	streamWriteChunk = 512 << 10
	// streamReadBuf 读取缓冲上限(单 piece 粒度等待,buff 大于 piece 无益)。
	streamReadBuf = 4 << 20
)

// parseRangeHeader 解析单区间 Range 头,返回 [start,end](闭区间)。
// hasRange 报告请求是否携带有效 Range;ok=false 表示无法满足(起点越界,
// 调用方应回 416)。多区间只服务第一段;后缀形式 bytes=-N 支持(播放器
// 探测文件尾拿时长/索引的常见姿势);语法非法视为无 Range(整文件)。
func parseRangeHeader(hdr string, size int64) (start, end int64, hasRange, ok bool) {
	if size <= 0 {
		return 0, -1, false, false
	}
	hdr = strings.TrimSpace(hdr)
	if len(hdr) < 6 || !strings.EqualFold(hdr[:6], "bytes=") {
		return 0, size - 1, false, true
	}
	spec := strings.TrimSpace(hdr[6:])
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, size - 1, false, true
	}
	lhs, rhs := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if lhs == "" {
		// bytes=-N:末 N 字节;N≥size 按 RFC 视为整文件
		n, err := strconv.ParseInt(rhs, 10, 64)
		if err != nil || n <= 0 {
			return 0, size - 1, false, true
		}
		if n >= size {
			return 0, size - 1, true, true
		}
		return size - n, size - 1, true, true
	}
	s, err := strconv.ParseInt(lhs, 10, 64)
	if err != nil || s < 0 {
		return 0, size - 1, false, true
	}
	if s >= size {
		return 0, 0, false, false
	}
	e := size - 1
	if rhs != "" {
		if v, err := strconv.ParseInt(rhs, 10, 64); err == nil && v >= s && v < e {
			e = v
		}
	}
	return s, e, true, true
}

// handleBTStream 边下边播端点。播放器(内嵌 <video> 或 VLC/Kodi 等)按
// Range 拉流:引擎把区间起点换算为播放头(seek 即重排优先级),随后按
// piece 对齐逐块等待落定、读盘、flush——绝不把未落定区域用零填充发出。
func (s *Server) handleBTStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := s.mgr.BTStreamOpen(id, r.URL.Query().Get("file"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	start, end, hasRange, ok := parseRangeHeader(r.Header.Get("Range"), info.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, "区间越界")
		return
	}
	seq, err := s.mgr.BTStreamConnOpen(id)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	defer s.mgr.BTStreamConnClose(id, seq)
	// 最新连接拥有游标权:本请求的起点即当前播放头(seek 即时重排窗口)。
	// 仅开放式 Range(到文件尾)/无 Range 视为播放语义;有界小 Range 是
	// 探测/moov 回读,不抢游标(否则浏览器回读文件头会把主播放流饿死在头部)
	s.mgr.BTStreamPos(id, seq, start, end >= info.Size-1)

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Cache-Control", "no-store")
	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, min(info.PieceLen, streamReadBuf))
	bbuf := make([]byte, bt.BlockSize) // 块级直读缓冲
	for off := start; off <= end; {
		// 快路径:off 所在 piece 已落定 → 盘读整段(piece 对齐、受 chunk
		// 上限约束)。已落定 piece 的数据必在盘上(校验即落盘)。
		if s.mgr.BTStreamPieceDone(id, off) {
			n := end - off + 1
			if pieceLeft := info.PieceLen - off%info.PieceLen; pieceLeft < n {
				n = pieceLeft
			}
			n = min(n, int64(len(buf)), streamWriteChunk)
			wctx, cancel := context.WithTimeout(r.Context(), streamWaitPiece)
			werr := s.mgr.BTStreamWait(wctx, id, off, n)
			cancel()
			if werr != nil {
				return
			}
			if rerr := s.mgr.BTStreamRead(id, off, buf[:n]); rerr != nil {
				return
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			s.mgr.BTStreamServed(id, seq, off+n)
			off += n
			continue
		}
		// 块级路径:piece 未落定时逐块等待、内存攒块直读(16KB 粒度)——
		// 首字节不必等整片 SHA-1,大 piece 种子首载大幅提前。溢写 piece
		// 块状态不可知,WaitBlock 只能等整片,行为退回 piece 粒度。
		pieceLeft := info.PieceLen - off%info.PieceLen
		sliceEnd := min(end+1, off+pieceLeft)
		for off < sliceEnd {
			local := (info.FileOffset + off) % info.PieceLen
			n := min(int64(bt.BlockSize)-local%bt.BlockSize, sliceEnd-off)
			wctx, cancel := context.WithTimeout(r.Context(), streamWaitPiece)
			pieceDone, arrived, werr := s.mgr.BTStreamWaitBlock(wctx, id, off)
			cancel()
			if werr != nil {
				return // 超时/断开/会话结束:结束响应,播放器重试
			}
			if pieceDone {
				break // 整片恰好落定:跳回快路径盘读本片剩余
			}
			if !arrived {
				return
			}
			if !s.mgr.BTStreamBlockRead(id, off, bbuf[:n]) {
				// 缓冲恰被校验取走:重查整片,落定走盘读、否则重等本块
				if s.mgr.BTStreamPieceDone(id, off) {
					break
				}
				continue
			}
			if _, werr := w.Write(bbuf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			s.mgr.BTStreamServed(id, seq, off+n)
			off += n
		}
	}
}

// handleBTStreamStatus 流播状态(前端缓冲可视化:已落定位图 base64)。
func (s *Server) handleBTStreamStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.mgr.BTStreamStatus(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleBTStreamStop 停止边下边播,还原纯下载优先级。
func (s *Server) handleBTStreamStop(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.BTStreamStop(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
