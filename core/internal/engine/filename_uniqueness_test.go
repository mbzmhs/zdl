package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClaimFilenameUniqueness 同目录同名的批量任务必须拿到互不冲突的最终
// 文件名(第 1 个原名,其后顺延 (1)(2)…),且要跳过磁盘上已存在的文件。
// 这是"批量填同一文件名 → 写同一个 .zdl.part 互相覆盖"回归的守门:
// claimFilename 是唯一名来源,part 路径由它拼出,名字不重则 part 不重。
func TestClaimFilenameUniqueness(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{tasks: map[string]*Task{}}
	for _, id := range []string{"a", "b", "c"} {
		m.tasks[id] = &Task{Dir: dir}
	}
	got := map[string]string{}
	for _, id := range []string{"a", "b", "c"} {
		got[id] = m.claimFilename(dir, "report.pdf", id)
	}
	want := map[string]string{"a": "report.pdf", "b": "report (1).pdf", "c": "report (2).pdf"}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s = %q, want %q (all=%v)", id, got[id], w, got)
		}
	}

	// 磁盘已有 report (2).pdf → 新任务应顺延到 (3)(report.pdf/(1) 被在途任务占用)。
	if err := os.WriteFile(filepath.Join(dir, "report (2).pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.tasks["d"] = &Task{Dir: dir}
	if d := m.claimFilename(dir, "report.pdf", "d"); d != "report (3).pdf" {
		t.Fatalf("已有 report (2).pdf 时应顺延到 (3), got %q", d)
	}

	// 不同目录互不影响:另一目录的同名任务仍拿原名。
	other := t.TempDir()
	m.tasks["e"] = &Task{Dir: other}
	if e := m.claimFilename(other, "report.pdf", "e"); e != "report.pdf" {
		t.Fatalf("不同目录不应互相判重, got %q", e)
	}
}
