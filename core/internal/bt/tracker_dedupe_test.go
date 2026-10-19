package bt

import (
	"testing"
	"time"
)

// tracker 去重:同 host:port(不同协议 scheme)视为重复,保留最近成功的一个。
func TestTrackerDedupe(t *testing.T) {
	http1 := "http://1.2.3.4:8000/announce"
	udp1 := "udp://1.2.3.4:8000/announce"
	udp2 := "udp://5.6.7.8:6969/announce"
	c := &Client{
		trackers: []string{http1, udp1, udp2},
		trStates: map[string]*trackerState{},
		stopCh:   make(chan struct{}),
	}
	close(c.stopCh) // 测试中不发 stopped 通告
	now := time.Now()
	// http 版一小时前成功,udp 版刚刚成功:应保留 udp 删除 http
	c.trStates[http1] = &trackerState{url: http1, lastOK: now.Add(-time.Hour)}
	c.trStates[udp1] = &trackerState{url: udp1, lastOK: now}

	removed := c.DedupeTrackers()
	if len(removed) != 1 || removed[0] != http1 {
		t.Fatalf("应删除 %q,实际删除 %v", http1, removed)
	}
	c.trkMu.Lock()
	got := append([]string(nil), c.trackers...)
	c.trkMu.Unlock()
	if len(got) != 2 || got[0] != udp1 || got[1] != udp2 {
		t.Fatalf("去重后应剩 %v,实际 %v", []string{udp1, udp2}, got)
	}
}

// TrackerKey 等价键:http/https 补默认端口,不同 host 不等价。
func TestTrackerKey(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"http://1.2.3.4:8000/announce", "udp://1.2.3.4:8000/announce", true},
		{"http://example.com/announce", "http://example.com:80/announce", true},
		{"https://example.com/announce", "http://example.com:80/announce", false},
		{"udp://example.com:6969/announce", "udp://example.com:7000/announce", false},
		{"udp://1.1.1.1:80/announce", "udp://2.2.2.2:80/announce", false},
	}
	for _, c := range cases {
		if got := TrackerKey(c.a) == TrackerKey(c.b); got != c.same {
			t.Fatalf("TrackerKey(%q)==TrackerKey(%q) = %v, 期望 %v", c.a, c.b, got, c.same)
		}
	}
}
