// 分配预算闸门 (T3-4 第九批, 2026-11-20 凌晨批)
//
// 为什么要有这条闸门：fib28 的每指令成本 (12.9ns) 里有一块是**装箱**——
// 每次数字结果都要 `&Number{...}` 一次，实测 40.3 万次/跑（平均每 38 条指令
// 一次分配）。这类退化**不会让任何现有闸门变红**：
//
//   - check-bench.py 盯的是 fib28 的**毫秒数**，而一次额外分配只值 1~2ns，
//     少 10% 分配在 130ms 的墙钟里完全看不见（噪声底就有 ±1%，见下）；
//   - check-coldstart.py 盯的是首帧，与热循环无关；
//   - test262 只看语义，分配多少一条都不管。
//
// 于是"某个后续提交把快路径退化成通用路径"（例如给 addValues 加一层
// ToPrimitive，或在 NewNumber 前多一次断言失败）就会静默发生，等有人几年后
// 想优化时才发现基线已经浮了。本文件把**分配次数**钉住。
//
// 为什么用分配次数而不是毫秒：分配次数是**确定性的**（同一份代码在同一台
// 机器上逐轮差 ≤3 次，见下面的实测），跨机器也基本一致——它数的是"发生了
// 多少次 make/&T"，不是"花了多久"。所以它在 2 核 CI runner 上不会假红，
// 也不需要 --update-baseline 维护。毫秒级回归仍由 check-bench.py 负责。
//
// 阈值来历（2026-11-20 本机 AMD EPYC 9K65 / go1.26.2 实测，逐轮样本见
// bench-results/perf-ab-*.json）：
//
//	Fib28PureEval  403,267 / 403,268 / 403,269  → 预算 408,000 (+1.2% 余量)
//	SetupGlobals     2,102 / 2,102 / 2,103      → 预算 2,200  (+4.7% 余量)
//
// T3-5 第二十三批 (2026-11-20) 把 SetupGlobals 的实测压到 **1,570**：内建函数的
// length 不再走 DefineOwn 写 PropDescs（每个内建函数省掉一张双槽 map），
// 实测 1,570/1,570/1,588。**预算刻意不跟着下调** —— 这是一条防退化的地板，
// 不是一把"贴着实测再收紧"的尺子；把预算收到 1,600 只会让下一个人的
// 编译器升级假红。它现在仍有 40% 余量，那是因为 533 次是一个结构性台阶，
// 不是噪声。
//
// 余量给的是"编译器换版本时 map/切片实现的分配口径微调"，不是给回归留后门：
// 真要加一次装箱，403k 这个量级上至少 +5k，一定撞线。
package perf

import (
	"runtime"
	"testing"

	"github.com/14752222/Gox/stdlib"
	"github.com/14752222/Gox/vm"
)

const (
	// fib28AllocBudget 是一次完整求值 (不含编译) 的分配次数上限。
	fib28AllocBudget = 408000
	// setupGlobalsAllocBudget 是一次解释器初始化 (建全局环境 + 全部内建对象树)
	// 的分配次数上限。冷启动线对它敏感：初始化期间的分配直接抬高 GC 起点。
	setupGlobalsAllocBudget = 2200
)

// countAllocs 跑 fn n 次，返回**平均每次**的堆分配次数。
//
// 用 runtime.MemStats.Mallocs 的差值而不是 testing.AllocsPerRun：后者会
// 先跑一轮预热再跑 n 轮，对 fib28 这种 130ms 一轮的负载等于白跑两倍时间；
// 而 Mallocs 是单调计数器，前后相减即可，语义更直接。
func countAllocs(n int, fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		fn()
	}
	runtime.ReadMemStats(&after)
	return (after.Mallocs - before.Mallocs) / uint64(n)
}

// TestFib28AllocBudget 钉住热循环的分配量。
//
// 这条闸门挡的典型退化（都真实发生过或差点发生）：
//   - 把 Number+Number 快路径删掉，退回 ToPrimitive 通用路径（每次 +2 断言，
//     命中失败还会多分配）；
//   - 给 `frame.Locals` 的 clear 改成逐次 make；
//   - 关掉 arguments 惰性化 / 常量池复用（每调用 +1 分配 × 83 万次）。
func TestFib28AllocBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过 (一轮求值 ~130ms, 只为量分配数)")
	}
	if raceDetectorEnabled {
		t.Skip("-race 下分配次数会整体漂移, 预算判据无意义")
	}
	src := loadFib28(t)
	c, err := compileForBench(src)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	got := countAllocs(3, func() {
		machine := vm.NewWithGlobals(c.Bytes(), c.Constants(), c.NumLocals(), stdlib.SetupGlobals())
		if err := machine.RunCompiled(c); err != nil {
			t.Fatalf("求值失败: %v", err)
		}
	})
	t.Logf("fib28 一次求值分配 %d 次 (预算 %d)", got, fib28AllocBudget)
	if got > fib28AllocBudget {
		t.Fatalf("fib28 分配量回归: %d > 预算 %d (+%.1f%%) —— "+
			"要么是真退化了, 要么是**有意的**新分配: 后者必须同时调预算并在 docs/performance.md §3.5 写清"+
			"这笔分配换来了什么。别直接调数字了事。",
			got, fib28AllocBudget, float64(got-fib28AllocBudget)/float64(fib28AllocBudget)*100)
	}
}

// TestSetupGlobalsAllocBudget 钉住解释器初始化的分配量 (冷启动线)。
//
// 这条与 TestFib28AllocBudget 是**两条不同的账**：热循环的分配在求值期间，
// 初始化的分配在首帧之前。后者变多 ⇒ GC 起点抬高 ⇒ 首帧那条 GC 更早触发，
// 而首帧口径里多一次 GC 就是 1~2ms（§12.19 记过这笔账）。
func TestSetupGlobalsAllocBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过")
	}
	if raceDetectorEnabled {
		t.Skip("-race 下分配次数会整体漂移, 预算判据无意义")
	}
	got := countAllocs(20, func() {
		if env := stdlib.SetupGlobals(); env == nil {
			t.Fatal("SetupGlobals 返回 nil")
		}
	})
	t.Logf("SetupGlobals 每次分配 %d 次 (预算 %d)", got, setupGlobalsAllocBudget)
	if got > setupGlobalsAllocBudget {
		t.Fatalf("解释器初始化分配量回归: %d > 预算 %d —— "+
			"启动路径上多出来的分配会直接抬高首帧前的 GC 起点 (docs/performance.md §12.5/§12.19)。"+
			"若是有意的, 调预算并写明原因。", got, setupGlobalsAllocBudget)
	}
}
