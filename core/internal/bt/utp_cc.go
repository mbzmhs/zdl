package bt

import "math"

// uTP 拥塞控制:LEDBAT(RFC 6817)。时延历史与窗口公式按规范
// .zcode/notes/utp-spec.md §6/§7 移植(libutp DelayHist 逐行对应)。
// 所有时延单位为微秒,时间戳 32 位回绕用无符号运算。

const (
	utpTargetDelay   = 100000    // 目标单向排队时延 100ms(µs)
	utpGainPerRTT    = 3000.0    // 每 RTT 窗口最大增长(字节)
	utpMinCwnd       = 1436.0    // 最小拥塞窗口 = 1 MSS(libtorrent 口径)
	utpBaseHistSlots = 13        // base_delay 槽数(每分钟轮换一槽)
	utpCurHistSize   = 3         // 当前时延取最近 3 样本最小
	utpBaseRotateMs  = 60 * 1000 // 每 60s 轮换一槽
	utpInitRTO       = 1000      // 初始重传超时(ms,BEP-29)
	utpSynRTO        = 3000      // SYN 重传超时(ms,两库一致)
	utpMinRTO        = 500       // 超时下限(ms)
	utpMaxRTO        = 60000     // 连续退避封顶(ms)
)

// u32Before 报告环上 lhs 是否先于 rhs(compare_less_wrap:两方向取短,
// 距离相同视为不先)。用于 delayBase 比较与 32 位时戳回绕。
func u32Before(lhs, rhs uint32) bool {
	distUp := rhs - lhs   // lhs → rhs 顺时针
	distDown := lhs - rhs // rhs → lhs 顺时针
	return distUp < distDown
}

// delayHist 是 LEDBAT 时延历史:
//   - base:最近 13 个"每分钟一槽"样本的滑动最小(跨约 13 分钟,过滤排队尖峰);
//   - cur: 最近 3 个样本减 base 的最小值 = 当前排队时延。
type delayHist struct {
	baseHist [utpBaseHistSlots]uint32
	baseIdx  int
	base     uint32
	baseAtMs uint64
	init     bool

	cur    [utpCurHistSize]uint32
	curIdx int
	curN   int // 已写入槽位数(未满时只扫已填槽,防初始 0 污染 min)
}

// addSample 记录一个时延样本(µs)。nowMs 为单调毫秒时钟。
func (h *delayHist) addSample(sample uint32, nowMs uint64) {
	if !h.init {
		for i := range h.baseHist {
			h.baseHist[i] = sample
		}
		h.base = sample
		h.baseAtMs = nowMs
		h.init = true
	}
	if u32Before(sample, h.baseHist[h.baseIdx]) {
		h.baseHist[h.baseIdx] = sample
	}
	if u32Before(sample, h.base) {
		h.base = sample
	}
	h.cur[h.curIdx] = sample - h.base // 回绕安全的减法
	h.curIdx = (h.curIdx + 1) % utpCurHistSize
	if h.curN < utpCurHistSize {
		h.curN++
	}

	if nowMs-h.baseAtMs > utpBaseRotateMs {
		h.baseAtMs = nowMs
		h.baseIdx = (h.baseIdx + 1) % utpBaseHistSlots
		h.baseHist[h.baseIdx] = sample
		// 槽内旧样本自动过期:全槽重扫最小值
		m := h.baseHist[0]
		for _, v := range h.baseHist[1:] {
			if u32Before(v, m) {
				m = v
			}
		}
		h.base = m
	}
}

// value 返回当前排队时延(最近 3 样本相对 base 的最小值);无样本返回 0。
func (h *delayHist) value() uint32 {
	if h.curN == 0 {
		return 0
	}
	m := h.cur[0]
	n := h.curN
	if n > utpCurHistSize {
		n = utpCurHistSize
	}
	for _, v := range h.cur[1:n] {
		if v < m {
			m = v
		}
	}
	return m
}

// utpRTT 是 RFC 6298 风格 RTT 估计(Karn:只对只发送过一次的包取样)。
type utpRTT struct {
	srtt   float64 // 平滑 RTT(µs)
	rttvar float64 // 偏差(µs)
	valid  bool
}

func (r *utpRTT) sample(sampleUs float64) {
	if !r.valid {
		r.srtt = sampleUs
		r.rttvar = sampleUs / 2
		r.valid = true
		return
	}
	delta := r.srtt - sampleUs
	if delta < 0 {
		delta = -delta
	}
	r.rttvar += (delta - r.rttvar) / 4
	r.srtt += (sampleUs - r.srtt) / 8
}

// timeoutMs 当前重传超时(µs→ms),无样本时用初始 1000ms。
func (r *utpRTT) timeoutMs() int64 {
	if !r.valid {
		return utpInitRTO
	}
	t := int64((r.srtt + 4*r.rttvar) / 1000)
	if t < utpMinRTO {
		t = utpMinRTO
	}
	return t
}

// utpCC 是单连接的拥塞控制器:维护 maxWindow(cwnd)与慢启动状态,
// 在收到有效 ack(ackedBytes>0)时按规范 §6.4A 更新。
type utpCC struct {
	maxWindow float64
	slowStart bool
	ssthresh  float64
	// lastMaxedOut:上次"打满窗口"的时刻——1s 内没打满不允许增长
	// (限速场景下防止时延样本虚低导致的假增长)。
	lastMaxedOutMs uint64
	saturated      bool
}

func newUtpCC(initial float64) *utpCC {
	return &utpCC{maxWindow: initial, slowStart: true}
}

// onAckacked 在 ackedBytes>0 的 ack 上调用。ourDelay 为当前排队时延
// (µs,已钳 minRTT);pktSize 为当前包尺寸;nowMs 单调毫秒。
// inFlight 为 ack 后仍在外的字节数,acked+inFlight 反映原在途规模。
func (c *utpCC) onAck(ourDelay uint32, acked, pktSize float64, nowMs uint64) {
	// 窗口因子:acked 与"原在途"的比例(ack 越接近打满窗口越接近 1)。
	inFlight := c.maxWindow
	if c.saturated {
		inFlight = acked + inFlight
	}
	windowFactor := math.Min(acked, inFlight) / math.Max(inFlight, acked)
	delayFactor := (float64(utpTargetDelay) - float64(ourDelay)) / float64(utpTargetDelay)
	gain := utpGainPerRTT * windowFactor * delayFactor

	if gain > 0 && nowMs-c.lastMaxedOutMs > 1000 {
		gain = 0 // 超过 1s 没打满窗口:限速或对端窗口受限,不增长
	}
	ledbat := c.maxWindow + gain
	if ledbat < utpMinCwnd {
		ledbat = utpMinCwnd
	}

	if c.slowStart {
		// 慢启动按确认字节增长(RFC 3465 字节计数):对端延迟 ack 时一次
		// 累计 ack 覆盖整窗,按"每 ack +1 包"增长会被 ack 批次大小钳成
		// 每 RTT 线性爬坡(实测异构对端 40ms 延迟 ack 下全程 ~3.7MB/s,
		// TCP 同机几百 MB/s);字节计数恢复每 RTT 翻倍的经典行为
		ss := c.maxWindow + acked
		switch {
		case c.ssthresh != 0 && ss > c.ssthresh:
			c.slowStart = false
			c.maxWindow = ledbat
		case float64(ourDelay) > float64(utpTargetDelay)*0.9:
			c.slowStart = false
			c.ssthresh = c.maxWindow
			c.maxWindow = ledbat
		default:
			if ss > ledbat {
				c.maxWindow = ss
			} else {
				c.maxWindow = ledbat
			}
		}
	} else {
		c.maxWindow = ledbat
	}
	if c.maxWindow < utpMinCwnd {
		c.maxWindow = utpMinCwnd
	}
	c.lastMaxedOutMs = nowMs
	c.saturated = false
}

// markSaturated 记录本端确实用满了窗口(发送侧每轮检查)。
func (c *utpCC) markSaturated(nowMs uint64) {
	c.lastMaxedOutMs = nowMs
	c.saturated = true
}

// onLoss 丢包折窗 ×0.5(下限 1 MSS),并退出慢启动。
func (c *utpCC) onLoss() {
	c.maxWindow *= 0.5
	if c.maxWindow < utpMinCwnd {
		c.maxWindow = utpMinCwnd
	}
	if c.slowStart {
		c.ssthresh = c.maxWindow
		c.slowStart = false
	}
}

// onTimeout 超时重置:有在途 → 窗口打回 1 包并重启慢启动;
// 空闲 → ×2/3 衰减(不重启)。返回新的 RTO 退避基数。
func (c *utpCC) onTimeout(inFlight float64) {
	if inFlight > 0 {
		c.maxWindow = utpMinCwnd
		c.slowStart = true
		c.ssthresh = 0
	} else {
		c.maxWindow = math.Max(c.maxWindow*2/3, utpMinCwnd)
	}
}
