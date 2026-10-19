package bt

import (
	"testing"
	"time"
)

// TestEvictReason 淘汰判定:150s 双向零贡献、下载中被 choke 5 分钟零下行;
// 有任一方向流量、太年轻、做种期的 choke 都保留。
func TestEvictReason(t *testing.T) {
	cases := []struct {
		name      string
		p         evictPeer
		completed bool
		wantEvict bool
	}{
		{
			name:      "连接太年轻:保留",
			p:         evictPeer{age: 60 * time.Second},
			wantEvict: false,
		},
		{
			name:      "150s 双向零贡献:淘汰",
			p:         evictPeer{age: evictZeroDur + time.Second},
			wantEvict: true,
		},
		{
			name:      "有下行:保留",
			p:         evictPeer{age: 10 * time.Minute, down: 4096, choked: true},
			wantEvict: false,
		},
		{
			name:      "在吃我们的上传:保留(R1 不满足,R2 需被 choke)",
			p:         evictPeer{age: 4 * time.Minute, up: 8192},
			wantEvict: false,
		},
		{
			name:      "下载中被 choke 5 分钟且零下行:淘汰(在吃上传也淘汰)",
			p:         evictPeer{age: evictSnub, up: 1 << 20, choked: true},
			completed: false,
			wantEvict: true,
		},
		{
			name:      "被 choke 不足 5 分钟:保留",
			p:         evictPeer{age: evictSnub - time.Second, up: 1 << 20, choked: true},
			completed: false,
			wantEvict: false,
		},
		{
			name:      "做种期被 choke 无意义:保留(他还在下我们的)",
			p:         evictPeer{age: 10 * time.Minute, up: 1 << 20, choked: true},
			completed: true,
			wantEvict: false,
		},
		{
			name:      "零星下行也是贡献:保留",
			p:         evictPeer{age: evictSnub, down: 1},
			wantEvict: false,
		},
		{
			name:      "有需 piece 的种子被 choke 3 分钟:保留(等 unchoke 排队)",
			p:         evictPeer{age: 3 * time.Minute, choked: true, hasNeeded: true},
			completed: false,
			wantEvict: false,
		},
		{
			name:      "有需 piece 的种子零下行 10 分钟:淘汰",
			p:         evictPeer{age: evictNeededWindow + time.Second, choked: true, hasNeeded: true},
			completed: false,
			wantEvict: true,
		},
		{
			name:      "有需 piece 未被 choke 但 10 分钟零下行:淘汰(僵尸)",
			p:         evictPeer{age: evictNeededWindow + time.Second, hasNeeded: true},
			completed: false,
			wantEvict: true,
		},
		{
			name:      "有需 piece 的种子开始供数:保留",
			p:         evictPeer{age: 20 * time.Minute, down: 1 << 20, hasNeeded: true},
			completed: false,
			wantEvict: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := evictReason(tc.p, tc.completed)
			if got := reason != ""; got != tc.wantEvict {
				t.Fatalf("evict=%v want %v(reason=%q)", got, tc.wantEvict, reason)
			}
		})
	}
}

// TestShortEvictReason 短窗淘汰:无 bitfield 的连接绝对无法供数,60s 未通告
// 即清退;已通告 bitfield 但不含所需 piece 的 90s 清退;有任一方向流量的
// 交给常规规则;做种期不适用(避免误杀正在下载我们的 leecher)。
func TestShortEvictReason(t *testing.T) {
	cases := []struct {
		name      string
		p         evictPeer
		completed bool
		wantEvict bool
	}{
		{
			name:      "无 bitfield 未到 60s:保留",
			p:         evictPeer{age: evictNoBitfield - time.Second},
			wantEvict: false,
		},
		{
			name:      "无 bitfield 超 60s 零收发:淘汰",
			p:         evictPeer{age: evictNoBitfield + time.Second},
			wantEvict: true,
		},
		{
			name:      "无 bitfield 但我们在上传:保留",
			p:         evictPeer{age: evictNoBitfield + time.Second, up: 4096},
			wantEvict: false,
		},
		{
			name:      "无 bitfield 但有下行(不应发生):保留",
			p:         evictPeer{age: evictNoBitfield + time.Second, down: 4096},
			wantEvict: false,
		},
		{
			name:      "bitfield 无所需 piece 未到 90s:保留",
			p:         evictPeer{age: evictUselessBitfield - time.Second, bfKnown: true},
			wantEvict: false,
		},
		{
			name:      "bitfield 无所需 piece 超 90s:淘汰",
			p:         evictPeer{age: evictUselessBitfield + time.Second, bfKnown: true},
			wantEvict: true,
		},
		{
			name:      "bitfield 含所需 piece:保留(交给长窗)",
			p:         evictPeer{age: evictUselessBitfield + time.Second, bfKnown: true, hasNeeded: true},
			wantEvict: false,
		},
		{
			name:      "做种期不短窗淘汰:保留",
			p:         evictPeer{age: 10 * time.Minute},
			completed: true,
			wantEvict: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := shortEvictReason(tc.p, tc.completed)
			if got := reason != ""; got != tc.wantEvict {
				t.Fatalf("evict=%v want %v(reason=%q)", got, tc.wantEvict, reason)
			}
		})
	}
}
