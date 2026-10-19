package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"zdownloader/internal/logbuf"
)

// 桌面版(main.go)与 headless 版(main_headless.go)共享的启动辅助。

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// setupLogging 把日志写入数据目录(GUI 子系统构建下没有控制台);
// 若 stderr 可用(控制台/管道)则同时输出。超过 8MB 轮转一次。
func setupLogging(dataDir string) {
	logPath := filepath.Join(dataDir, "zdownloader.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if st, e := f.Stat(); e == nil && st.Size() > 8<<20 {
		f.Close()
		_ = os.Rename(logPath, logPath+".old")
		f, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
	}
	var w io.Writer = f
	if _, e := os.Stderr.Stat(); e == nil {
		w = io.MultiWriter(f, os.Stderr)
	}
	// 挂接环形缓冲,供 UI「系统日志」侧栏读取
	w = io.MultiWriter(w, logbuf.Default())
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags)
}
