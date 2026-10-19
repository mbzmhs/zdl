package engine

import (
	"sort"
	"sync"
)

// segPool 是分块任务队列:预切成大量小块,worker 按序认领,
// 快的 worker 自然多做,天然消除静态切分的长尾;无共享可变段,避免窃取竞态。
type segPool struct {
	mu       sync.Mutex
	pending  []Segment          // 待下载(可能带 Done,即续传的半截块)
	inflight map[int64]*Segment // 进行中,key=Start
	minChunk int64
}

const (
	minChunkSize  = 1 << 20 // 1MB,低于此不再细块
	maxChunkSize  = 16 << 20
	chunksPerConn = 8 // 每连接预切块数,决定粒度与长尾上界
)

// newChunkPool 全新切分。
func newChunkPool(size int64, conns int) *segPool {
	if conns < 1 {
		conns = 1
	}
	chunk := size / int64(conns*chunksPerConn)
	if chunk < minChunkSize {
		chunk = minChunkSize
	}
	if chunk > maxChunkSize {
		chunk = maxChunkSize
	}
	p := &segPool{inflight: map[int64]*Segment{}, minChunk: minChunkSize}
	start := int64(0)
	for start < size {
		end := start + chunk - 1
		if end > size-1 {
			end = size - 1
		}
		p.pending = append(p.pending, Segment{Start: start, End: end})
		start = end + 1
	}
	return p
}

// newChunkPoolFrom 从持久化快照恢复:已完成的块丢弃,半截块(带 Done)重新入队。
func newChunkPoolFrom(segs []Segment) *segPool {
	p := &segPool{inflight: map[int64]*Segment{}, minChunk: minChunkSize}
	for _, s := range segs {
		if s.Remaining() > 0 {
			p.pending = append(p.pending, s)
		}
	}
	sort.Slice(p.pending, func(i, j int) bool { return p.pending[i].Start < p.pending[j].Start })
	return p
}

// Acquire 认领下一块;队列空返回 false。
func (p *segPool) Acquire() (Segment, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return Segment{}, false
	}
	seg := p.pending[0]
	p.pending = p.pending[1:]
	s := seg
	p.inflight[seg.Start] = &s
	return seg, true
}

// Progress worker 回报进行中块的写入进度。
func (p *segPool) Progress(seg Segment) {
	p.mu.Lock()
	if s := p.inflight[seg.Start]; s != nil && seg.Done > s.Done {
		s.Done = seg.Done
	}
	p.mu.Unlock()
}

// Complete 标记块完成。
func (p *segPool) Complete(seg Segment) {
	p.mu.Lock()
	delete(p.inflight, seg.Start)
	p.mu.Unlock()
}

// Snapshot 返回全部剩余块(含进行中的进度),供持久化与统计。
func (p *segPool) Snapshot() []Segment {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Segment, 0, len(p.pending)+len(p.inflight))
	out = append(out, p.pending...)
	for _, s := range p.inflight {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Remaining 统计剩余字节数。
func (p *segPool) Remaining() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for _, s := range p.pending {
		n += s.Remaining()
	}
	for _, s := range p.inflight {
		n += s.Remaining()
	}
	return n
}

// DoneBytes 已完成字节(用于快照持久化)。
func (p *segPool) DoneBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int64
	for _, s := range p.pending {
		n += s.Done
	}
	for _, s := range p.inflight {
		n += s.Done
	}
	return n
}
