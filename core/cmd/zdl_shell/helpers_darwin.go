//go:build darwin

// 常驻组件的启动辅助(zdl_ui 两形态共用的启动辅助与锁文件互斥判定中,
// 本文件只含日志与诊断——互斥/派生在 mutex_darwin.go;与 helpers_windows.go
// 同构;待与引擎侧收敛为共享 internal/boot 包)。
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"zdownloader/internal/logbuf"
)

// setupLogging 数据目录轮转日志 + 环形缓冲双写。引擎与窗口/托盘进程并发追加
// 同一文件(O_APPEND 原子追加),各形态 log.SetPrefix 区分来源。
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
	log.SetOutput(io.MultiWriter(f, logbuf.Default()))
	log.SetFlags(log.LstdFlags)
}

// startDiagnostics 诊断端点(仅 127.0.0.1):pprof + 每分钟内存快照日志
func startDiagnostics(port int) {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", pprof.Handler("index"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("诊断端点: http://127.0.0.1:%d/debug/pprof/", port)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("诊断端点退出: %v", err)
		}
	}()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		var ms runtime.MemStats
		for range t.C {
			runtime.ReadMemStats(&ms)
			log.Printf("[mem] HeapAlloc=%dMB HeapSys=%dMB Sys=%dMB NumGC=%d Goroutines=%d",
				ms.HeapAlloc>>20, ms.HeapSys>>20, ms.Sys>>20, ms.NumGC, runtime.NumGoroutine())
		}
	}()
}
