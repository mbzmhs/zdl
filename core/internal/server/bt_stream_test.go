package server

import "testing"

// parseRangeHeader 表驱动:前缀/区间/后缀/多区间/非法语法/越界。
func TestParseRangeHeader(t *testing.T) {
	const size = 10000
	cases := []struct {
		name    string
		hdr     string
		start   int64
		end     int64
		has     bool
		ok      bool
	}{
		{"无 Range", "", 0, 9999, false, true},
		{"前缀", "bytes=0-", 0, 9999, true, true},
		{"区间", "bytes=100-199", 100, 199, true, true},
		{"end 越界截断", "bytes=9990-20000", 9990, 9999, true, true},
		{"后缀", "bytes=-500", 9500, 9999, true, true},
		{"后缀≥整文件", "bytes=-10000", 0, 9999, true, true},
		{"多区间取第一段", "bytes=0-1,5-9", 0, 1, true, true},
		{"空段忽略", "bytes=-", 0, 9999, false, true},
		{"语法非法按整文件", "bytes=abc", 0, 9999, false, true},
		{"非 bytes 单位忽略", "chunks=0-1", 0, 9999, false, true},
		{"起点越界 416", "bytes=10000-", 0, 0, false, false},
		{"负起点按整文件", "bytes=-5-10", 0, 9999, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, e, has, ok := parseRangeHeader(c.hdr, size)
			if s != c.start || e != c.end || has != c.has || ok != c.ok {
				t.Fatalf("parseRangeHeader(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
					c.hdr, s, e, has, ok, c.start, c.end, c.has, c.ok)
			}
		})
	}
	// 零长文件:一律不可满足
	if _, _, _, ok := parseRangeHeader("bytes=0-", 0); ok {
		t.Fatal("零长文件应不可满足")
	}
}
