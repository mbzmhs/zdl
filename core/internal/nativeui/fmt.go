package nativeui

// fmt.js 的 Go 移植:展示格式化与任务派生字段(逐函数对齐,数值口径不改)。

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"zdownloader/internal/engine"
)

// urlUnescape 磁链 dn 参数解码(编码异常返回原文,=Vue decodeURIComponent try/catch)
func urlUnescape(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// fmtBytes 字节格式化(对齐 fmt.js:units B..TB;≥100 取整,≥10 一位小数,
// 其余两位;负数/空返回 —)
func fmtBytes(n int64) string {
	if n < 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, units[i])
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, units[i])
	default:
		return fmt.Sprintf("%.2f %s", v, units[i])
	}
}

// fmtSpeed 速度格式化(0/负 = —)
func fmtSpeed(n int64) string {
	if n <= 0 {
		return "—"
	}
	return fmtBytes(n) + "/s"
}

// fmtETA 预计剩余时间(对齐 fmt.js:≥30 天显示 ≥1月;时分秒两位补零)
func fmtETA(speed int64, remaining int64) string {
	if speed <= 0 || remaining <= 0 {
		return "--:--"
	}
	s := (remaining + speed - 1) / speed // ceil
	if s >= 86400*30 {
		return "≥1月"
	}
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}

// fmtTime 完成时间(yyyy-MM-dd HH:mm)
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// extOf 小写扩展名(无点;对齐 fmt.js extOf)
func extOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 || len(name)-i > 6 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// iconClass 文件类型分组(对齐 fmt.js iconClass:video/audio/img/doc/archive/other)
func iconClass(t engine.TaskView) string {
	if t.Kind == "hls" {
		return "hls"
	}
	switch extOf(t.Filename) {
	case "mp4", "mkv", "webm", "avi", "mov", "flv", "ts", "m4v", "wmv":
		return "video"
	case "mp3", "flac", "aac", "wav", "ogg", "m4a", "opus":
		return "audio"
	case "jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "ico":
		return "img"
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "epub", "md":
		return "doc"
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso":
		return "archive"
	}
	return "other"
}

// iconLabel 图标文字(HLS 显示 HLS,否则扩展名前 4 位大写)
func iconLabel(t engine.TaskView) string {
	if t.Kind == "hls" {
		return "HLS"
	}
	e := extOf(t.Filename)
	if e == "" {
		return "FILE"
	}
	if len(e) > 4 {
		e = e[:4]
	}
	return strings.ToUpper(e)
}

// MagnetFallbackName 磁力任务元数据就绪前的标题回退(=fmt.js:优先 dn 显示名
// 超长 40 截断,否则信息哈希)
func MagnetFallbackName(t engine.TaskView) string {
	if t.Kind != "bt" || t.Filename != "" || !strings.HasPrefix(strings.ToLower(t.URL), "magnet:") {
		return ""
	}
	i := strings.Index(t.URL, "dn=")
	if i >= 0 {
		dn := t.URL[i+3:]
		if j := strings.IndexByte(dn, '&'); j >= 0 {
			dn = dn[:j]
		}
		if dn != "" {
			name := urlUnescape(dn)
			for _, r := range []string{`\`, "/", ":", "*", "?", `"`, "<", ">", "|"} {
				name = strings.ReplaceAll(name, r, " ")
			}
			name = strings.TrimSpace(name)
			if name != "" {
				if len([]rune(name)) > 40 {
					name = string([]rune(name)[:40]) + "…"
				}
				return name
			}
		}
	}
	i = strings.Index(t.URL, "urn:btih:")
	if i >= 0 {
		h := t.URL[i+9:]
		if j := strings.IndexAny(h, "&"); j >= 0 {
			h = h[:j]
		}
		return strings.ToLower(h)
	}
	return ""
}

// StatusText 状态徽章文本(=fmt.js STATUS_TEXT)
func StatusText(s engine.Status) string {
	switch s {
	case engine.StatusWaiting:
		return "等待中"
	case engine.StatusRunning:
		return "下载中"
	case engine.StatusPaused:
		return "已暂停"
	case engine.StatusCompleted:
		return "已完成"
	case engine.StatusError:
		return "失败"
	case engine.StatusCanceled:
		return "已取消"
	case engine.StatusSeeding:
		return "做种中"
	case engine.StatusChoosing:
		return "待创建任务"
	}
	return string(s)
}
