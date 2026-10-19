package engine

// HLS 引擎:解析 m3u8(主清单/媒体清单)、并行下载分片、AES-128 解密、
// 顺序合并为单个 TS 文件。所有网络能力(代理/限速/重试/自定义头)复用 Manager。

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/util"
)

type hlsVariant struct {
	URI       string
	Bandwidth int64
}

type hlsKey struct {
	Method string
	URI    string
	IV     []byte
}

type hlsMedia struct {
	Segments      []string
	InitSegment   string
	Key           *hlsKey
	MediaSequence int
	IsVOD         bool
}

// parseMaster 解析主清单,返回全部变体。
func parseMaster(text string, base *url.URL) ([]hlsVariant, error) {
	var variants []hlsVariant
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			bw := int64(0)
			for _, attr := range strings.Split(line[len("#EXT-X-STREAM-INF:"):], ",") {
				kv := strings.SplitN(attr, "=", 2)
				if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "BANDWIDTH") {
					bw, _ = strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
				}
			}
			// 清单 URI 是下一个非注释行
			for j := i + 1; j < len(lines); j++ {
				uri := strings.TrimSpace(lines[j])
				if uri == "" || strings.HasPrefix(uri, "#") {
					continue
				}
				variants = append(variants, hlsVariant{URI: resolveURI(base, uri), Bandwidth: bw})
				break
			}
		}
	}
	if len(variants) == 0 {
		return nil, errors.New("主清单中没有变体")
	}
	return variants, nil
}

// parseMedia 解析媒体清单。
func parseMedia(text string, base *url.URL) (*hlsMedia, error) {
	m := &hlsMedia{}
	lines := strings.Split(text, "\n")
	var curKey *hlsKey
	seenEndList := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			k, err := parseKey(line[len("#EXT-X-KEY:"):], base)
			if err != nil {
				return nil, err
			}
			curKey = k
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			for _, attr := range strings.Split(line[len("#EXT-X-MAP:"):], ",") {
				kv := strings.SplitN(attr, "=", 2)
				if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "URI") {
					m.InitSegment = resolveURI(base, unquote(kv[1]))
				}
			}
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			n, _ := strconv.Atoi(strings.TrimSpace(line[len("#EXT-X-MEDIA-SEQUENCE:"):]))
			m.MediaSequence = n
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			seenEndList = true
		case strings.HasPrefix(line, "#EXTINF:"):
			// 分片 URI 是下一个非注释行
			for j := i + 1; j < len(lines); j++ {
				uri := strings.TrimSpace(lines[j])
				if uri == "" || strings.HasPrefix(uri, "#") {
					continue
				}
				m.Segments = append(m.Segments, resolveURI(base, uri))
				break
			}
		}
	}
	if !seenEndList {
		return nil, errors.New("不支持直播流(无 EXT-X-ENDLIST),目前仅支持点播 VOD")
	}
	if len(m.Segments) == 0 {
		return nil, errors.New("媒体清单中没有分片")
	}
	m.IsVOD = true
	m.Key = curKey
	return m, nil
}

func parseKey(s string, base *url.URL) (*hlsKey, error) {
	k := &hlsKey{Method: "NONE"}
	for _, attr := range strings.Split(s, ",") {
		kv := strings.SplitN(attr, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		switch key {
		case "METHOD":
			k.Method = unquote(val)
		case "URI":
			k.URI = resolveURI(base, unquote(val))
		case "IV":
			iv, err := parseIV(unquote(val))
			if err != nil {
				return nil, fmt.Errorf("IV 解析失败: %w", err)
			}
			k.IV = iv
		}
	}
	switch k.Method {
	case "NONE", "":
		return nil, nil
	case "AES-128":
		return k, nil
	default:
		return nil, fmt.Errorf("不支持的加密方式: %s", k.Method)
	}
}

func parseIV(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if s == "" {
		return nil, errors.New("IV 为空")
	}
	raw, err := hexDecode(s)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16 {
		return nil, errors.New("IV 超过 16 字节")
	}
	iv := make([]byte, 16)
	copy(iv[16-len(raw):], raw)
	return iv, nil
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("十六进制长度为奇数")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		b, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out[i] = byte(b)
	}
	return out, nil
}

func unquote(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"`)
}

func resolveURI(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if base != nil {
		return base.ResolveReference(u).String()
	}
	return u.String()
}

// ---------- 运行 ----------

const hlsMaxAttempts = 4

func (m *Manager) runHLS(ctx context.Context, t *Task) error {
	client := &http.Client{Transport: m.transportFor(t)}
	spec := m.specOf(t)

	// HLS 不走 HTTP 探测:文件名 = 清单 URL 去掉 .m3u8 后缀 + .ts
	t.mu.Lock()
	if t.Filename == "" {
		name := "stream"
		if u, err := url.Parse(spec.url); err == nil {
			base := path.Base(u.Path)
			if lower := strings.ToLower(base); strings.HasSuffix(lower, ".m3u8") {
				base = base[:len(base)-5]
			}
			if base != "" && base != "/" && base != "." {
				name = base
			}
		}
		// 判重同上:同目录同名清单/在途任务会写同一个 .zdl.part。
		if cand := m.claimFilename(t.Dir, util.SanitizeFilename(name)+".ts", t.ID); cand != "" {
			t.Filename = cand
		}
	}
	conns := t.Connections
	t.mu.Unlock()
	if conns < 1 {
		conns = 1
	}

	// 拉取并解析清单(支持 主清单 → 媒体清单 一层跳转)
	playlistURL := spec.url
	var media *hlsMedia
	for level := 0; level < 3; level++ {
		body, err := m.hlsFetchAll(ctx, client, spec, playlistURL)
		if err != nil {
			return fmt.Errorf("获取清单失败: %w", err)
		}
		text := string(body)
		base, _ := url.Parse(playlistURL)
		if strings.Contains(text, "#EXT-X-STREAM-INF") && !strings.Contains(text, "#EXTINF") {
			variants, err := parseMaster(text, base)
			if err != nil {
				return err
			}
			best := variants[0]
			for _, v := range variants {
				if v.Bandwidth > best.Bandwidth {
					best = v
				}
			}
			playlistURL = best.URI
			continue
		}
		media, err = parseMedia(text, base)
		if err != nil {
			return err
		}
		break
	}
	if media == nil {
		return errors.New("清单解析失败")
	}

	t.mu.Lock()
	t.SegTotal = len(media.Segments)
	t.Encrypted = media.Key != nil
	// 恢复:t.Segs 中记录已完成分片(Start=索引)
	done := make([]bool, len(media.Segments))
	for _, s := range t.Segs {
		if s.Done == 1 && s.Start >= 0 && s.Start < int64(len(done)) {
			done[s.Start] = true
		}
	}
	t.mu.Unlock()

	// 解密密钥
	var key []byte
	if media.Key != nil {
		body, err := m.hlsFetchAll(ctx, client, spec, media.Key.URI)
		if err != nil {
			return fmt.Errorf("获取解密密钥失败: %w", err)
		}
		key = body
		if len(key) != 16 {
			return fmt.Errorf("密钥长度异常: %d 字节", len(key))
		}
	}

	// 工作目录:分片临时文件
	t.mu.Lock()
	workDir := t.finalPath() + ".zdlwork"
	t.mu.Unlock()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return &fatalError{"无法创建分片工作目录: " + err.Error()}
	}

	// 并行下载分片
	t.mu.Lock()
	if t.Size < 0 {
		t.Size = 0 // HLS 总大小 = init 段 + 各分片解密后累计
	}
	t.mu.Unlock()

	errCh := make(chan error, 1)
	var once sync.Once
	fail := func(err error) { once.Do(func() { errCh <- err }) }

	t.mu.Lock()
	limit := t.SpeedLimit
	t.mu.Unlock()
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

	next := make(chan int, len(media.Segments))
	for i := range media.Segments {
		if !done[i] {
			next <- i
		}
	}
	close(next)

	var wg sync.WaitGroup
	cancel := m.cancelOf(t)
	for w := 0; w < min(conns, len(media.Segments)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range next {
				if ctx.Err() != nil {
					return
				}
				segPath := filepath.Join(workDir, fmt.Sprintf("seg-%05d.bin", idx))
				if _, err := os.Stat(segPath); err == nil { // 上次已下完但未记录
					m.hlsSegDone(t, idx)
					continue
				}
				err := m.hlsDownloadSegment(ctx, t, client, spec, tLim, media, key, idx, segPath)
				if err == nil {
					m.hlsSegDone(t, idx)
					continue
				}
				if ctx.Err() != nil {
					return
				}
				fail(err)
				if cancel != nil {
					cancel()
				}
				return
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 合并(init 段 → 0..n)
	return m.hlsConcat(ctx, t, client, spec, media, workDir)
}

func (m *Manager) hlsSegDone(t *Task, idx int) {
	t.mu.Lock()
	t.SegDone++
	t.Segs = append(t.Segs, Segment{Start: int64(idx), Done: 1})
	t.mu.Unlock()
}

// hlsDownloadSegment 下载单个分片到 segPath(已解密)。
func (m *Manager) hlsDownloadSegment(ctx context.Context, t *Task, client *http.Client, spec reqSpec, tLim *taskLimiter, media *hlsMedia, key []byte, idx int, segPath string) error {
	var lastErr error
	for attempt := 0; attempt < hlsMaxAttempts; attempt++ {
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
		err := m.hlsSegmentOnce(ctx, t, client, spec, tLim, media, key, idx, segPath)
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

func (m *Manager) hlsSegmentOnce(ctx context.Context, t *Task, client *http.Client, spec reqSpec, tLim *taskLimiter, media *hlsMedia, key []byte, idx int, segPath string) error {
	segURL := media.Segments[idx]
	req, err := m.newRequest(ctx, "GET", segURL, spec)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &fatalError{fmt.Sprintf("分片 %d 下载失败: HTTP %d", idx, resp.StatusCode)}
	}

	// 分块读取,走限速器并实时更新进度
	var data bytes.Buffer
	readN := m.readSizeFor(tLim)
	buf := make([]byte, readN)
	for {
		if err := m.waitLimits(ctx, tLim, int64(readN)); err != nil {
			return err
		}
		n, rerr := resp.Body.Read(buf)
		m.refundLimits(tLim, int64(readN)-int64(n))
		if n > 0 {
			data.Write(buf[:n])
			t.mu.Lock()
			t.Downloaded += int64(n)
			t.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr // 网络错误,重试
		}
	}
	body := data.Bytes()

	// 解密(AES-128 CBC + PKCS7,每个分片独立加密)
	if key != nil {
		if len(body) == 0 || len(body)%aes.BlockSize != 0 {
			return fmt.Errorf("分片 %d 密文长度不是 16 的倍数", idx)
		}
		iv := media.Key.IV
		if iv == nil {
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(media.MediaSequence+idx))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return &fatalError{"AES 初始化失败: " + err.Error()}
		}
		mode := cipher.NewCBCDecrypter(block, iv)
		mode.CryptBlocks(body, body)
		body, err = pkcs7Unpad(body, aes.BlockSize)
		if err != nil {
			return &fatalError{fmt.Sprintf("分片 %d 解密失败: %v", idx, err)}
		}
	}

	tmp := segPath + ".part"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return &fatalError{"写入分片失败: " + err.Error()}
	}
	if err := os.Rename(tmp, segPath); err != nil {
		return &fatalError{"分片落盘失败: " + err.Error()}
	}
	return nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("空数据")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, errors.New("PKCS7 填充非法")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS7 填充非法")
		}
	}
	return data[:len(data)-pad], nil
}

// hlsConcat 顺序合并分片为最终 TS 文件。
func (m *Manager) hlsConcat(ctx context.Context, t *Task, client *http.Client, spec reqSpec, media *hlsMedia, workDir string) error {
	partPath := t.partPath()
	out, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return &fatalError{"无法创建输出文件: " + err.Error()}
	}

	writeAll := func(data []byte) error {
		_, err := out.Write(data)
		return err
	}

	// init 段(fMP4 场景)
	if media.InitSegment != "" {
		init, err := m.hlsFetchAll(ctx, client, spec, media.InitSegment)
		if err != nil {
			out.Close()
			return fmt.Errorf("获取 init 分段失败: %w", err)
		}
		if err := writeAll(init); err != nil {
			out.Close()
			return err
		}
		t.mu.Lock()
		t.Size += int64(len(init))
		t.mu.Unlock()
	}

	var total int64
	for i, _ := range media.Segments {
		if err := ctx.Err(); err != nil {
			out.Close()
			return err
		}
		data, err := os.ReadFile(filepath.Join(workDir, fmt.Sprintf("seg-%05d.bin", i)))
		if err != nil {
			out.Close()
			return &fatalError{"读取分片失败: " + err.Error()}
		}
		if err := writeAll(data); err != nil {
			out.Close()
			return err
		}
		total += int64(len(data))
	}
	if err := out.Close(); err != nil {
		return err
	}

	t.mu.Lock()
	t.Size = t.Size + total
	t.mu.Unlock()

	// 安全规则:绝不覆盖已存在的文件——冲突时自动改名 (1)(2)…
	final := t.finalPath()
	if _, err := os.Stat(final); err == nil {
		alt := util.UniquePath(t.Dir, t.Filename)
		t.mu.Lock()
		t.Filename = filepath.Base(alt)
		t.mu.Unlock()
		final = alt
	}
	if err := os.Rename(partPath, final); err != nil {
		return &fatalError{"落盘失败: " + err.Error()}
	}
	_ = os.RemoveAll(workDir)

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

// hlsFetchAll 下载整个响应体(清单/密钥/init 段)。
func (m *Manager) hlsFetchAll(ctx context.Context, client *http.Client, spec reqSpec, rawURL string) ([]byte, error) {
	req, err := m.newRequest(ctx, "GET", rawURL, spec)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
