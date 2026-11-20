package gfx

// ===== 首帧阶段探针 (T3-5 第二十六批, 2026-11-21 凌晨批) =====
//
// 背景: 冷启动的引擎侧已在 §12.22 收口 (惰性化上限 2.9%), 首帧 9ms 里剩下的
// 靶子全在 GUI 侧 (~5.96ms: 建窗 / 字体 / Layout / Draw / 上屏)。但此前**没有
// 任何手段知道这 5.96ms 花在哪一段** —— 只有"总数", 于是每次优化都只能靠猜,
// 而 §12.26.4 已证明本机 A/B 对 2% 量级的分辨力很有限。猜 + 低分辨力 = 白干。
//
// 本探针把首帧拆成可比较的段, 默认**完全关闭** (一个 atomic 读, 十万分之一毫秒):
//
//	GOX_STARTUP_PHASE=1 ./gox app.js
//	gox-phase   +1.149ms mount.enter
//	gox-phase   +1.155ms x11.conn_ready
//	...
//	gox-phase   +2.949ms frame.present_done
//
// 三条口径 (与 §12.22 / §12.26 的数字不可混用):
//  1. **相对时刻**: 基准取 gfx 包初始化的那一刻, 不是进程 exec。所以第一个
//     采样点 (main.enter) 之前的进程 exec + Go runtime + 133 个包 init 不在
//     这张表里 —— 那部分要用 `gox version` 的进程墙钟单独量 (§12.22.1)。
//  2. **段与段之间可能有别的东西**: 这里只打"某件事做完"的时刻, 不做区间归属。
//     两次采样之差 = 中间发生的全部工作 (含 goroutine 抢占), 这正是要看的。
//  3. **探针本身进测量**: 一次 fmt.Fprintf 到 stderr 约 3~10µs。要看 0.1ms 级
//     的段时, 先把 stderr 重定向到 /dev/null 再跑一遍确认量级。

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// startupPhaseBase 是相对时刻的零点: gfx 包初始化。
// 选包初始化而不是 main 的第一行, 是因为 `import gx/gfx` 的模块查找到
// 建窗之间那段 (脚本解析/建树) 也要能对齐到同一条时间轴。
var startupPhaseBase = time.Now()

// phaseWriter 是输出目标 (测试替换成 buffer; 生产是 stderr)。
var phaseWriter io.Writer = os.Stderr

// startupPhaseOn 由 CLI 边界打开 (EnableStartupPhase)。默认 false ——
// 与 devmode / 字体预取 / 显示预热同一条纪律: 库不自己读环境变量、不因为
// 被链接进某个进程就往 stderr 写东西。
var startupPhaseOn atomic.Bool

// EnableStartupPhase 打开阶段探针。幂等。
func EnableStartupPhase() { startupPhaseOn.Store(true) }

// StartupPhaseActive 报告探针是否开着 (测试与调用方判断用)。
func StartupPhaseActive() bool { return startupPhaseOn.Load() }

// PhaseTrace 打一个阶段点。名字用 "域.动作" 形式 (mount / frame / x11),
// 便于按域过滤。关闭时是一次 atomic 读 + 返回。
func PhaseTrace(name string) {
	if !startupPhaseOn.Load() {
		return
	}
	fmt.Fprintf(phaseWriter, "gox-phase %+9.3fms %s\n",
		float64(time.Since(startupPhaseBase).Microseconds())/1000, name)
}
