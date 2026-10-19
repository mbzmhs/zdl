package bt

import (
	"os"
	"testing"
)

// TestMain 全包封闭化:默认引导节点全是公网域名,单测一律置空——
// 每个节点启动的引导循环不再发公网 DNS 与 UDP(门禁慢且不稳的主因,
// 离线/代理环境下还会拖出数秒级超时)。需要 DHT 拓扑的用例用本地
// 节点互插路由表完成引导。
func TestMain(m *testing.M) {
	DHTBootstrapNodes = nil
	os.Exit(m.Run())
}
