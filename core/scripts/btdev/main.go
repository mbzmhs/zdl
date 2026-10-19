// btdev 是 BT 调试工具:内置最小 HTTP tracker + 做种器一体。
// 用于本地端到端验证:对指定目录做种,并输出可供下载端使用的 .torrent。
// 用法: go run ./scripts/btdev -dir <源目录> [-http 127.0.0.1:9801] [-torrent out.torrent]
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"

	"zdownloader/internal/bt"
)

func main() {
	dir := flag.String("dir", "", "做种的源目录或文件")
	httpAddr := flag.String("http", "127.0.0.1:9801", "内置 tracker 监听地址")
	torrentOut := flag.String("torrent", "", "输出的 .torrent 路径(默认 <dir>.torrent)")
	upKB := flag.Int("up", 0, "做种上传限速 KB/s(0 不限;联调「下载中」状态时用来拖住下载端)")
	flag.Parse()
	if *dir == "" {
		log.Fatal("缺少 -dir")
	}

	announce := "http://" + *httpAddr + "/announce"
	raw, meta, err := bt.BuildTorrentFromDisk(*dir, announce, "", 0)
	if err != nil {
		log.Fatal(err)
	}
	out := *torrentOut
	if out == "" {
		out = *dir + ".torrent"
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		log.Fatal(err)
	}

	// 最小 tracker:记录每个 info_hash 的 (ip:port),应答全部 peer
	var mu sync.Mutex
	swarm := map[string]map[string]bool{}
	http.HandleFunc("/announce", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ih := fmt.Sprintf("%x", []byte(q.Get("info_hash")))
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		key := net.JoinHostPort(ip, q.Get("port"))
		mu.Lock()
		if swarm[ih] == nil {
			swarm[ih] = map[string]bool{}
		}
		swarm[ih][key] = true
		var peers []byte
		for k := range swarm[ih] {
			h, p, _ := net.SplitHostPort(k)
			ip4 := net.ParseIP(h).To4()
			if ip4 == nil {
				continue
			}
			pn, _ := strconv.Atoi(p)
			peers = append(peers, ip4...)
			peers = append(peers, byte(pn>>8), byte(pn))
		}
		mu.Unlock()
		w.Write(bt.MustEncode(map[string]any{
			"interval":   int64(2),
			"peers":      string(peers),
			"complete":   int64(1),
			"incomplete": int64(0),
		}))
	})
	go http.ListenAndServe(*httpAddr, nil)

	seed, err := bt.NewSeedClient(meta, *dir, bt.ClientConfig{MaxPeers: 32, UpLimit: int64(*upKB) << 10})
	if err != nil {
		log.Fatal(err)
	}
	seed.AddTrackers([]string{announce})
	seed.Start()
	defer seed.Close()

	fmt.Printf("做种中: %s\n种子文件: %s\ntracker: %s\ninfohash: %x\n监听: %s\n",
		meta.Name, out, announce, meta.InfoHash, seed.ListenAddr())
	select {}
}
