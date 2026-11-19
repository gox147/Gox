// Package startupgc 在「进程启动 → 首帧已上屏」这段窗口里临时抬高 GC 目标，之后立刻还原。
//
// # 为什么需要它（问题不是"GC 慢"，而是"GC 恰好落在同步往返上"）
//
// 冷启动前 20ms 里，解释器初始化 + 字体解析 + 建树会把堆从 0 推到 4MB 左右，
// 正好越过 Go 运行时的首个 GC 目标。于是一次 GC 周期必然落在**建窗那一段**：
// 那里是不带 reply 的请求 + 几个同步往返（intern WM_PROTOCOLS / RandR
// SelectInputChecked / MapWindow + Sync），主 goroutine 一被拉去协助标记，
// 每个往返就贵 5~10 倍。
//
// 本机实测（32 核容器 / Xvfb 1024×768×24 / n=13 中位 / 见 docs/performance.md §12.19）：
//
//	GOGC=100（默认）: 首帧 18.1ms，建窗段 3.1~3.4ms，启动期 1 次 GC
//	GOGC=200        : 首帧 16.1ms，建窗段 1.5ms，   启动期 0 次 GC
//	GOGC=400/800/off: 首帧 16.1~16.3ms —— 与 200 完全同档
//
// 200 就到顶、再抬没用，说明收益只来自"**跳过首帧前那一次** GC"。所以本包
// 抬到 200 而不是关掉 GC：抬多了既不更快，还要多担内存。
//
// # 还原点为什么不是"求值返回那一刻"
//
// 实测过：求值返回时立刻还原（`debug.SetGCPercent` 降目标会马上安排一轮 GC），
// 那轮 GC 就落在"首帧已上屏 → 事件循环开始"之间 —— 用户已经看见窗口，但它
// **仍然落在首帧口径里**（探测轮询在 GC 之后才打印标记），于是优化在数字上
// 完全消失；而它对用户也没有额外好处（事件循环正要开始处理输入）。
//
// 现在的还原点是"首帧之后 + 一小段延迟"，另有堆上限兜底：首帧之前一次 GC 都
// 不发生，用户看到窗口的时刻提前；那轮 GC 挪到 100ms 后（那时用户已经在看
// 窗口，不在等启动）。**这是延迟收益，不是吞吐收益**：总 CPU 一点没少，
// 只是那 25ms 的 assist CPU 不再挡在建窗的同步往返上。
//
// # 四条纪律
//
//  1. **用户说了算**：进程环境里显式设了 GOGC（含 GOGC=off）就一次都不碰 ——
//     那是用户/发布方的调优决定，工具不该改。
//  2. **有借有还**：窗口结束（到点 / 堆超限 / 进入长驻循环）立刻还原。
//  3. **有兜底**：到点与堆上限两条独立触发；新加的执行路径忘了调终点也不会
//     让进程一直跑在 200%。没有兜底的"启动期抬 GC"是内存事故的经典写法。
//  4. **不猜堆**：堆水位用 runtime/metrics 读（无 STW），不用 ReadMemStats。
package startupgc

import (
	"os"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
)

// BoostPercent 是启动窗口内的 GC 目标百分比。
//
// 为什么是 200 而不是更大：见包注释的实测档位 —— 200 已跳过首帧前的唯一一次 GC，
// 400/800/off 拿到的是同一个数。再大只是把"最坏情况下的堆上浮"从 3× 变成 9×。
const BoostPercent = 200

// HeapGuardBytes 是窗口期的堆上限：超过就立刻还原（宁可放弃延迟收益）。
//
// 64MB：正常 GUI 首帧期的活堆是 3~5MB（本机那一次 GC 的读数是 3→4MB），
// 64MB 是"这不像启动、更像有人在启动里灌数据"的分界。按 200% 算，触到它时
// 活堆约 21MB，离内存事故还很远。
const HeapGuardBytes = 64 << 20

// 只为测试可替换：生产路径就是 runtime/debug、runtime/metrics 与 os 的直接调用。
var (
	setPercent = debug.SetGCPercent
	getenv     = os.Getenv
	readHeap   = readHeapAlloc
)

// readHeapAlloc 用 runtime/metrics 读活堆字节数（无 STW，微秒级）。
func readHeapAlloc() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindUint64 {
		return s[0].Value.Uint64()
	}
	return 0
}

// Guard 是一次"启动窗口"的句柄。零值/nil 都是安全可 Restore 的（等于空操作）。
type Guard struct {
	prev  int           // Boost 前的 GC 百分比；<0 表示本 Guard 没有改过任何东西
	once  sync.Once     //
	stop  chan struct{} // 关闭以停止兜底监视
	why   atomic.Value  // 还原原因（诊断用）：manual / deadline / heap
	guard uint64        // 堆上限（测试可调小）
}

// active 是当前生效的 Guard（全局最多一个）。
var active atomic.Pointer[Guard]

// Boost 抬高 GC 目标并返回句柄；调用方**必须**在启动窗口结束时 Restore
// （或依赖本包的到点/堆上限兜底）。after <= 0 表示不设到点兜底（只给测试用）。
//
// 下面三种情况返回一个"空 Guard"（Restore 是空操作，也不改任何全局状态）：
//   - 用户在环境里显式设了 GOGC；
//   - 已经有一个窗口开着（不叠加）；
//   - 当前 GC 已被别处关掉（prev < 0），不替别人做决定。
func Boost(after time.Duration) *Guard {
	g := &Guard{prev: -1, guard: HeapGuardBytes}
	if getenv("GOGC") != "" {
		return g
	}
	if active.Load() != nil {
		return g
	}
	prev := setPercent(BoostPercent)
	if prev < 0 {
		// 别处已经把 GC 关了：恢复成 -1 会"重新打开"它，那不是我们该干的事。
		setPercent(prev)
		return g
	}
	g.prev = prev
	active.Store(g)
	if after > 0 {
		g.stop = make(chan struct{})
		go g.watch(after)
	}
	return g
}

// watch 是到点 + 堆上限的兜底：两条独立触发，谁先到谁还原。
func (g *Guard) watch(after time.Duration) {
	deadline := time.NewTimer(after)
	defer deadline.Stop()
	// 5ms 一次：100ms 的窗口内最多 20 次 metrics 读取（每次微秒级）。
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-deadline.C:
			g.restoreWith("deadline")
			return
		case <-tick.C:
			if readHeap() > g.guard {
				g.restoreWith("heap")
				return
			}
		}
	}
}

// Restore 把 GC 目标还原成 Boost 之前的值（幂等；nil 接收者安全）。
//
// 还原动作本身会触发一次 GC（目标调低时运行时立即安排一轮）—— 这正是想要的:
// 那轮 GC 落在首帧之后，而不是挡在建窗的同步往返里。
func (g *Guard) Restore() { g.restoreWith("manual") }

func (g *Guard) restoreWith(reason string) {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.prev < 0 {
			return
		}
		g.why.Store(reason)
		if g.stop != nil {
			close(g.stop)
		}
		active.CompareAndSwap(g, nil)
		setPercent(g.prev)
	})
}

// Active 报告启动窗口是否还开着（有人 Boost 过且尚未 Restore）。
//
// 存在的意义是给上层写闸门：长驻循环开始前后它必须为 false。
func Active() bool { return active.Load() != nil }

// Reason 报告这次窗口是怎么结束的（manual / deadline / heap；未结束为空）。
func (g *Guard) Reason() string {
	if g == nil {
		return ""
	}
	if v, ok := g.why.Load().(string); ok {
		return v
	}
	return ""
}
