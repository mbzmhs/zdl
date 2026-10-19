package engine

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/util"
)

const (
	minMultiSize = 1 << 20  // 小于 1MB 不分段
	readBufSize  = 64 << 10 // 单次写盘粒度
	maxAttempts  = 6
)

// fatalError 表示重试无意义的错误(4xx、写盘失败等)。
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

func isFatal(err error) bool {
	var fe *fatalError
	return errors.As(err, &fe)
}

// probeInfo 汇总一次探测得到的资源元数据。
type probeInfo struct {
	size    int64
	ranges  bool
	etag    string
	lastMod string
	cd      string
	ctype   string
	final   string
}

// runHTTP 探测 + 选择多线程/单线程路径。
func (m *Manager) runHTTP(ctx context.Context, t *Task) error {
	client := &http.Client{Transport: m.transportFor(t)}
	if err := m.probeHTTP(ctx, t, client); err != nil {
		return err
	}

	t.mu.Lock()
	multi := t.AcceptRanges && t.Size > minMultiSize && t.Connections > 1
	var pool *segPool
	if multi {
		if len(t.Segs) > 0 {
			pool = newChunkPoolFrom(t.Segs) // 续传
		} else {
			pool = newChunkPool(t.Size, t.Connections)
		}
		t.pool = pool
		t.Segs = pool.Snapshot()
	}
	conns := t.Connections
	if conns < 1 {
		conns = 1
	}
	limit := t.SpeedLimit
	t.mu.Unlock()
	spec := m.specOf(t) // 必须在释放 t.mu 之后调用(内部会加锁)
	m.persist(t)

	// 每任务限速句柄:挂到任务上,运行中可经 Update 热更新
	tLim := newTaskLimiter(0)
	if limit > 0 {
		tLim.set(limit)
	} else if m.cfg.TaskLimit > 0 {
		tLim.set(m.cfg.TaskLimit)
	}
	t.mu.Lock()
	t.limits = tLim
	t.mu.Unlock()

	if !multi {
		return m.runSingle(ctx, t, client, spec, tLim)
	}
	return m.runMulti(ctx, t, client, spec, pool, conns, tLim)
}

// probeHTTP 探测资源;续传时校验资源未变,变化则重置进度。
func (m *Manager) probeHTTP(ctx context.Context, t *Task, client *http.Client) error {
	spec := m.specOf(t)
	info, err := m.probeOnce(ctx, "HEAD", "", spec, client)
	if err != nil {
		info, err = m.probeOnce(ctx, "GET", "bytes=0-0", spec, client)
		if err != nil {
			return fmt.Errorf("无法访问资源: %w", err)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 续传一致性:资源变了就从头再来
	if len(t.Segs) > 0 || t.Downloaded > 0 {
		changed := false
		if t.ETag != "" && info.etag != "" && t.ETag != info.etag {
			changed = true
		}
		if t.LastModified != "" && info.lastMod != "" && t.LastModified != info.lastMod {
			changed = true
		}
		if t.Size > 0 && info.size > 0 && t.Size != info.size {
			changed = true
		}
		if changed {
			t.Segs = nil
			t.pool = nil
			t.Downloaded = 0
		}
	}

	t.Size = info.size
	t.AcceptRanges = info.ranges
	t.ETag = info.etag
	t.LastModified = info.lastMod
	t.FinalURL = info.final

	// 文件名在首次探测时确定:按 CD/URL 推导,并对同目录在途任务+磁盘判重,
	// 避免多条同名任务写同一个 .zdl.part 互相覆盖(完成时再兜底一次)。
	if t.Filename == "" {
		if cand := m.claimFilename(t.Dir, nameFromInfo(info), t.ID); cand != "" {
			t.Filename = cand
		}
	}
	return nil
}

// nameFromInfo 按优先级推导文件名:Content-Disposition → URL 路径 → MIME 类型猜测。
func nameFromInfo(info *probeInfo) string {
	name := util.FilenameFromContentDisposition(info.cd)
	if name == "" {
		name = util.FilenameFromURL(info.final)
	}
	if name == "" {
		name = "download"
		if exts, _ := mime.ExtensionsByType(info.ctype); len(exts) > 0 {
			name += exts[0]
		}
	} else if filepath.Ext(name) == "" && info.ctype != "" {
		if exts, _ := mime.ExtensionsByType(info.ctype); len(exts) > 0 {
			name += exts[0]
		}
	}
	return filepath.Base(name)
}

// ProbeResult 是 /api/probe 的响应体:链接预解析结果。
type ProbeResult struct {
	OK           bool   `json:"ok"`
	Filename     string `json:"filename,omitempty"`
	Size         int64  `json:"size"` // -1 表示未知
	AcceptRanges bool   `json:"acceptRanges"`
	ContentType  string `json:"contentType,omitempty"`
	FinalURL     string `json:"finalUrl,omitempty"`
	HLS          bool   `json:"hls"`
	Error        string `json:"error,omitempty"`
}

// Probe 预解析下载链接(合法性/文件名/大小/断点续传),不创建任务。
// HEAD 失败自动降级 GET bytes=0-0,与任务创建时的探测逻辑一致。
func (m *Manager) Probe(ctx context.Context, rawURL, referer string, headers map[string]string, proxyOverride string) *ProbeResult {
	res := &ProbeResult{Size: -1}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		res.Error = "链接格式不正确,仅支持 http/https"
		return res
	}
	spec := reqSpec{url: u.String(), ua: m.cfg.UserAgent, referer: referer, headers: headers}
	client := &http.Client{Transport: m.probeTransport(proxyOverride)}
	defer client.CloseIdleConnections()

	info, err := m.probeOnce(ctx, "HEAD", "", spec, client)
	if err != nil {
		info, err = m.probeOnce(ctx, "GET", "bytes=0-0", spec, client)
		if err != nil {
			res.Error = "无法访问资源: " + err.Error()
			return res
		}
	}
	res.OK = true
	res.Filename = nameFromInfo(info)
	res.ContentType = info.ctype
	res.FinalURL = info.final
	lower := strings.ToLower(info.final) + " " + strings.ToLower(info.ctype)
	if strings.Contains(lower, "mpegurl") || strings.Contains(lower, ".m3u8") {
		res.HLS = true // 清单长度不代表媒体大小
		return res
	}
	res.Size = info.size
	res.AcceptRanges = info.ranges
	return res
}

func (m *Manager) probeOnce(ctx context.Context, method, rangeHdr string, spec reqSpec, client *http.Client) (*probeInfo, error) {
	req, err := m.newRequest(ctx, method, spec.url, spec)
	if err != nil {
		return nil, err
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // 帮助连接复用

	info := &probeInfo{size: -1}
	switch method {
	case "HEAD":
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > 0 {
			info.size = resp.ContentLength
		}
		info.ranges = strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
	default: // GET bytes=0-0
		switch resp.StatusCode {
		case http.StatusPartialContent:
			info.ranges = true
			if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
				info.size = total
			}
		case http.StatusOK:
			info.ranges = false
			if resp.ContentLength > 0 {
				info.size = resp.ContentLength
			}
		default:
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	info.etag = resp.Header.Get("ETag")
	info.lastMod = resp.Header.Get("Last-Modified")
	info.cd = resp.Header.Get("Content-Disposition")
	info.ctype = resp.Header.Get("Content-Type")
	if resp.Request != nil && resp.Request.URL != nil {
		info.final = resp.Request.URL.String()
	}
	return info, nil
}

func parseContentRangeTotal(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 || i == len(v)-1 {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	if err != nil || total <= 0 {
		return 0, false
	}
	return total, true
}

// ---------- 多线程分块下载 ----------

func (m *Manager) runMulti(ctx context.Context, t *Task, client *http.Client, spec reqSpec, pool *segPool, conns int, tLim *taskLimiter) error {
	partPath := t.partPath()
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return &fatalError{"无法创建临时文件: " + err.Error()}
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	if err := f.Truncate(t.Size); err != nil { // 稀疏预分配
		return &fatalError{"预分配文件失败: " + err.Error()}
	}

	var wg sync.WaitGroup
	firstErr := make(chan error, 1)
	var once sync.Once
	cancel := m.cancelOf(t)
	fail := func(err error) {
		once.Do(func() {
			firstErr <- err
			if cancel != nil {
				cancel()
			}
		})
	}

	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				seg, ok := pool.Acquire()
				if !ok {
					return
				}
				if err := m.downloadSeg(ctx, t, client, f, spec, tLim, pool, &seg); err != nil {
					fail(err)
					return
				}
				pool.Complete(seg)
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-firstErr:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if rem := pool.Remaining(); rem > 0 {
		return fmt.Errorf("下载不完整,缺少 %d 字节", rem)
	}

	closed = true
	f.Close()
	return m.finishDownload(t)
}

func (m *Manager) downloadSeg(ctx context.Context, t *Task, client *http.Client, f *os.File, spec reqSpec, tLim *taskLimiter, pool *segPool, seg *Segment) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffFor(attempt)):
			}
		}
		err := m.segOnce(ctx, t, client, f, spec, tLim, pool, seg)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isFatal(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (m *Manager) segOnce(ctx context.Context, t *Task, client *http.Client, f *os.File, spec reqSpec, tLim *taskLimiter, pool *segPool, seg *Segment) error {
	start := seg.Start + seg.Done
	req, err := m.newRequest(ctx, "GET", spec.url, spec)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, seg.End))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return &fatalError{"服务器返回 416,分段越界(资源可能已变化)"}
	}
	if resp.StatusCode != http.StatusPartialContent {
		// 请求带了 If-Range 却拿到 200:资源在下载途中被替换,
		// 继续写会拼接新旧内容,立即中止(重试时探测会发现变化并整体重下)
		if spec.ifRange != "" && resp.StatusCode == http.StatusOK {
			return &fatalError{"资源在下载过程中被更改,已中止续传(重试该任务将自动重新下载)"}
		}
		return &fatalError{fmt.Sprintf("多线程下载被服务器拒绝(HTTP %d),可尝试降低线程数", resp.StatusCode)}
	}

	readN := m.readSizeFor(tLim)
	buf := make([]byte, readN)
	for {
		// 先购令牌再读取:杜绝先读后付带来的突发超速;短读部分退款
		if err := m.waitLimits(ctx, tLim, int64(readN)); err != nil {
			return err
		}
		n, rerr := resp.Body.Read(buf)
		m.refundLimits(tLim, int64(readN)-int64(n))
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], seg.Start+seg.Done); werr != nil {
				return &fatalError{"写入文件失败: " + werr.Error()}
			}
			seg.Done += int64(n)
			pool.Progress(*seg)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr // 网络错误,可重试
		}
	}
	if seg.Done < seg.Len() {
		return fmt.Errorf("连接提前结束,还差 %d 字节", seg.Len()-seg.Done)
	}
	return nil
}

// ---------- 单线程流式下载(不支持 Range 或小文件) ----------

func (m *Manager) runSingle(ctx context.Context, t *Task, client *http.Client, spec reqSpec, tLim *taskLimiter) error {
	partPath := t.partPath()

	// part 文件与已记录偏移对齐
	t.mu.Lock()
	offset := t.Downloaded
	t.mu.Unlock()
	if st, err := os.Stat(partPath); err == nil {
		if st.Size() != offset {
			offset = st.Size()
			t.mu.Lock()
			t.Downloaded = offset
			t.mu.Unlock()
		}
	} else {
		offset = 0
		t.mu.Lock()
		t.Downloaded = 0
		t.mu.Unlock()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts+2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffFor(attempt)):
			}
		}
		err := m.singleOnce(ctx, t, client, spec, tLim, partPath, &offset)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isFatal(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (m *Manager) singleOnce(ctx context.Context, t *Task, client *http.Client, spec reqSpec, tLim *taskLimiter, partPath string, offset *int64) error {
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return &fatalError{"无法创建临时文件: " + err.Error()}
	}
	defer f.Close()

	req, err := m.newRequest(ctx, "GET", spec.url, spec)
	if err != nil {
		return err
	}
	if *offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", *offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent: // 断点续传成功
	case http.StatusOK:
		if *offset > 0 { // 服务器忽略 Range:重下
			if err := f.Truncate(0); err != nil {
				return err
			}
			*offset = 0
			t.mu.Lock()
			t.Downloaded = 0
			t.mu.Unlock()
		}
		t.mu.Lock()
		if t.Size <= 0 && resp.ContentLength > 0 {
			t.Size = resp.ContentLength
		}
		t.mu.Unlock()
	default:
		return &fatalError{httpStatusMessage(resp.StatusCode)}
	}

	readN := m.readSizeFor(tLim)
	buf := make([]byte, readN)
	pos := *offset
	for {
		if err := m.waitLimits(ctx, tLim, int64(readN)); err != nil {
			return err
		}
		n, rerr := resp.Body.Read(buf)
		m.refundLimits(tLim, int64(readN)-int64(n))
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
				return &fatalError{"写入文件失败: " + werr.Error()}
			}
			pos += int64(n)
			t.mu.Lock()
			t.Downloaded = pos
			t.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	*offset = pos

	if err := f.Close(); err != nil { // Windows 下 rename 前必须关闭句柄
		return err
	}

	t.mu.Lock()
	if t.Size < 0 {
		t.Size = pos
	}
	t.mu.Unlock()
	return m.finishDownload(t)
}

// finishDownload 校验、改名、落定状态。仅当任务仍处于 running 时生效。
// 安全规则:绝不覆盖磁盘上已存在的文件——冲突时自动改名 (1)(2)…。
func (m *Manager) finishDownload(t *Task) error {
	if err := verifyChecksum(t.partPath(), t.Checksum); err != nil {
		return err
	}
	part := t.partPath()
	final := t.finalPath()
	if _, err := os.Stat(final); err == nil {
		alt := util.UniquePath(t.Dir, t.Filename)
		t.mu.Lock()
		t.Filename = filepath.Base(alt)
		t.mu.Unlock()
		final = alt
	}
	// 注意用改名前捕获的 part 路径:partPath() 由 Filename 拼出,
	// 上面的冲突改名会同步改变它,导致指向不存在的临时文件。
	if err := os.Rename(part, final); err != nil {
		return &fatalError{"落盘失败: " + err.Error()}
	}
	t.mu.Lock()
	if t.Status == StatusRunning {
		t.Status = StatusCompleted
		t.CompletedAt = time.Now()
		t.Segs = nil
		t.pool = nil
		t.speed = 0
		t.Downloaded = t.Size
	}
	t.mu.Unlock()
	return nil
}

func httpStatusMessage(code int) string {
	switch {
	case code == http.StatusForbidden:
		return "服务器拒绝访问(403),可能需要 Referer 或 Cookie"
	case code == http.StatusNotFound:
		return "资源不存在(404)"
	case code == http.StatusUnauthorized:
		return "需要授权(401)"
	default:
		return fmt.Sprintf("服务器返回 HTTP %d", code)
	}
}

// ---------- 校验和 ----------

// verifyChecksum 按 "algo:hex" 校验文件;spec 为空跳过。
func verifyChecksum(path, spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return &fatalError{fmt.Sprintf("校验和格式非法: %q(应为 md5:xxx / sha1:xxx / sha256:xxx / crc32:xxx)", spec)}
	}
	algo := strings.ToLower(parts[0])
	want := strings.ToLower(strings.TrimSpace(parts[1]))

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var got string
	switch algo {
	case "crc32":
		h := crc32.NewIEEE()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		got = hex.EncodeToString(h.Sum(nil))
	case "md5", "sha1", "sha256":
		var h hash.Hash
		switch algo {
		case "md5":
			h = md5.New()
		case "sha1":
			h = sha1.New()
		default:
			h = sha256.New()
		}
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		got = hex.EncodeToString(h.Sum(nil))
	default:
		return &fatalError{fmt.Sprintf("不支持的校验算法: %s", algo)}
	}
	if got != want {
		return &fatalError{fmt.Sprintf("校验和不匹配: 期望 %s,实际 %s", want, got)}
	}
	return nil
}
