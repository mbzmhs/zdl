// Package logbuf 提供一个线程安全的日志环形缓冲,供 UI 侧边栏查看系统日志。
package logbuf

import (
	"strings"
	"sync"
)

// Buffer 保存最近 max 行日志,超出即丢弃最旧行(计入 Dropped)。
// 作为 io.Writer 挂进 log 输出链,按行切分(尾行不完整则暂存拼接到下次)。
type Buffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	dropped int
	partial string // 尾部不完整行暂存
}

func New(max int) *Buffer {
	if max < 10 {
		max = 10
	}
	return &Buffer{max: max}
}

// Default 进程级共享缓冲(挂进 log 输出链;server 直接读取)。
// 环形大小照顾 uTP 包级调试(每连接每包一行):200 行只够 6 秒历史,
// 排查"连接在数据停"类问题时根本看不全,放大到 5 万行(约 10MB 内存)。
var def = New(50000)

func Default() *Buffer { return def }

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.partial + string(p)
	b.partial = ""
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			if s != "" {
				b.partial = s
			}
			break
		}
		line := s[:idx]
		s = s[idx+1:]
		if line == "" {
			continue
		}
		b.lines = append(b.lines, line)
		if len(b.lines) > b.max {
			b.lines = b.lines[1:]
			b.dropped++
		}
	}
	return len(p), nil
}

// Tail 返回最近 n 行(时间正序)。
func (b *Buffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// Dropped 返回累计丢弃的旧行数。
func (b *Buffer) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}
