package util

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWriteJSONAtomicConcurrent 并发写同一状态文件:临时名固定时两个写方
// 互相截断撕裂,rename 出残缺 JSON——下次启动解析失败即静默丢任务。
// 唯一临时名下无论并发多少次,落盘内容都必须是完整合法的 JSON。
func TestWriteJSONAtomicConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				v := map[string]any{"g": g, "i": i, "pad": make([]byte, 512)}
				if err := WriteJSONAtomic(path, v); err != nil {
					t.Errorf("写入失败: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("最终内容不是合法 JSON: %v\n%s", err, b[:min(len(b), 200)])
	}
	// 崩溃无关的正常路径不应遗留临时文件
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" && e.Name() != filepath.Base(path) {
			t.Fatalf("遗留临时文件: %s", e.Name())
		}
	}
}

// TestUniqueName 序号顺延判重:无冲突原名;磁盘已有则 (1);taken 命中(在途
// 兄弟任务)则顺延跳过;无扩展名同样顺延;空名原样返回。
func TestUniqueName(t *testing.T) {
	dir := t.TempDir()
	if got := UniqueName(dir, "x.txt", nil); got != "x.txt" {
		t.Fatalf("无冲突应原名, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := UniqueName(dir, "x.txt", nil); got != "x (1).txt" {
		t.Fatalf("磁盘冲突应 (1), got %q", got)
	}
	if got := UniqueName(dir, "x.txt", []string{"x (1).txt"}); got != "x (2).txt" {
		t.Fatalf("taken 命中应顺延到 (2), got %q", got)
	}
	if got := UniqueName(dir, "noext", []string{"noext"}); got != "noext (1)" {
		t.Fatalf("无扩展名也应顺延, got %q", got)
	}
	if got := UniqueName(dir, "", nil); got != "" {
		t.Fatalf("空名应原样, got %q", got)
	}
}
