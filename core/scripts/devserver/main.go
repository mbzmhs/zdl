// devserver 是开发/演示用静态文件服务器,支持 Range(基于 http.ServeContent)。
// 用法: go run ./scripts/devserver [目录] [端口]
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	dir := "."
	port := "9800"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if len(os.Args) > 2 {
		port = os.Args[2]
	}
	abs, _ := filepath.Abs(dir)
	http.Handle("/", http.FileServer(http.Dir(abs)))
	log.Printf("serving %s at http://127.0.0.1:%s", abs, port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, nil))
}
