// Package hub 是极简的发布/订阅广播器,供 SSE 推送使用。
package hub

import "sync"

type Hub struct {
	mu  sync.RWMutex
	chs map[chan []byte]struct{}
}

func New() *Hub {
	return &Hub{chs: make(map[chan []byte]struct{})}
}

// Subscribe 返回订阅通道与取消函数;慢消费者会被跳过(消息丢弃而非阻塞)。
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.chs[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.chs, ch)
		h.mu.Unlock()
	}
	return ch, cancel
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.chs {
		select {
		case ch <- msg:
		default: // 消费者堆积则丢弃本帧,下一帧会带全量快照
		}
	}
}

// CloseAll 关闭全部订阅通道(SSE 处理器的接收随即以 ok=false 结束、连接收尾),
// 供退出流程使用:http.Server.Shutdown 会等长连接空闲,不先掐断 SSE 它就要
// 干等满超时。可重复调用;CloseAll 后 Broadcast 为空转。
func (h *Hub) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.chs {
		close(ch)
	}
	h.chs = make(map[chan []byte]struct{})
}
