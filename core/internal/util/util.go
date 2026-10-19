// Package util 提供通用工具:文件名清洗、原子写、ID 生成、打开浏览器。
package util

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// RandomID 返回 16 位十六进制随机 ID。
func RandomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// windowsReserved 是 Windows 保留设备名,不能作为文件名。
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeFilename 清洗 Windows 非法字符与保留名,返回安全的文件名。
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(b.String())
	// 去掉结尾的点(Windows 不允许)
	for strings.HasSuffix(name, ".") {
		name = strings.TrimSuffix(name, ".")
	}
	// 保留名检查(不含扩展名部分)
	base := strings.ToUpper(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[base] {
		name = "_" + name
	}
	// 限长,尽量保留扩展名
	const maxName = 180
	if len(name) > maxName {
		ext := filepath.Ext(name)
		if len(ext) < maxName {
			name = name[:maxName-len(ext)] + ext
		} else {
			name = name[:maxName]
		}
	}
	if name == "" {
		name = "download"
	}
	return name
}

// FilenameFromURL 从 URL 路径末段提取文件名。
func FilenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		return ""
	}
	if dec, err := url.PathUnescape(base); err == nil {
		base = dec
	}
	return SanitizeFilename(base)
}

// FilenameFromContentDisposition 解析 Content-Disposition 头(含 RFC 5987 filename*)。
func FilenameFromContentDisposition(v string) string {
	if v == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(v); err == nil {
		if fn := params["filename"]; fn != "" {
			return SanitizeFilename(fn)
		}
	}
	// 兜底:宽松提取
	for _, key := range []string{"filename*=", "filename="} {
		if i := strings.Index(strings.ToLower(v), key); i >= 0 {
			part := v[i+len(key):]
			if j := strings.IndexByte(part, ';'); j >= 0 {
				part = part[:j]
			}
			part = strings.Trim(strings.TrimSpace(part), `"'`)
			if part != "" {
				return SanitizeFilename(part)
			}
		}
	}
	return ""
}

// UniquePath 当目标文件已存在时追加 " (n)" 序号,返回不冲突的完整路径。
func UniquePath(dir, name string) string {
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); os.IsNotExist(err) {
		return full
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// UniqueName 返回一个既不在 taken(同目录已被占用的文件名,如批量里的在途兄弟
// 任务——它们此刻磁盘上还没有文件)也不在磁盘上的文件名,冲突时按 " (1)"
// " (2)" … 在扩展名前追加序号顺延。name 为空原样返回空。
func UniqueName(dir, name string, taken []string) string {
	if name == "" {
		return ""
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		used := false
		for _, s := range taken {
			if s == cand {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, cand)); os.IsNotExist(err) {
			return cand
		}
	}
}

// jsonWriteLocks 按目标路径串行化并发写(进程内):rename 对同一目标的
// 并发替换在 Windows 上高频拒绝,串行化后重试只兜跨进程竞争(杀软扫描等)。
var jsonWriteLocks sync.Map

// WriteJSONAtomic 先写临时文件再 rename,保证崩溃时状态文件完整。
// 临时文件用 CreateTemp 生成唯一名:同一状态文件存在多个并发写方(周期
// 落盘/事件落盘),固定或同刻时间戳的 tmp 名会让写方互相截断/抢 rename,
// 落盘残缺 JSON——下次启动解析失败即静默丢任务。
func WriteJSONAtomic(path string, v any) error {
	l, _ := jsonWriteLocks.LoadOrStore(path, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if attempt >= 3 {
			_ = os.Remove(tmp)
			return err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
}

// ReadJSON 读取 JSON 文件到 v,文件不存在时不报错。
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, v)
}

// OpenBrowser 用系统默认浏览器打开 URL。
func OpenBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

// OpenFile 用系统默认关联程序打开本地文件。
func OpenFile(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// OpenFileLocation 在文件管理器中定位文件。
func OpenFileLocation(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,", path)
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

// OpenDir 在文件管理器中打开目录(不选中文件)。下载中/暂停等状态
// 的任务文件尚未落定,只能给用户开目录。
func OpenDir(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}
