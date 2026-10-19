//go:build darwin

package tray

import "testing"

// asl 转义覆盖:普通串、双引号、反斜杠、换行。
func TestASLEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", `"hello"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"line1\nline2", `"line1\nline2"`},
		{"tab\there", `"tab\there"`},
	}
	for _, c := range cases {
		if got := asl(c.in); got != c.want {
			t.Fatalf("asl(%q) = %s, 期望 %s", c.in, got, c.want)
		}
	}
}
