package main

// 启动窗口 (T3-5 第十九批): 在"进程开始跑 JS"到"首帧已上屏之后"之间把 GC 目标
// 抬到 200%，之后还原 —— 机制、实测见 internal/startupgc 与 docs/performance.md §12.19。
//
// 起点选在"这台进程要执行用户代码"那一刻 (runFile / startREPL)，而不是 main 的
// 第一行：`gox version` / `gox help` 一次 JS 都不跑，抬了只会让它们在退出时
// 白发一次 GC。终点不由这里精确决定 —— 见下面 startupGCWindow 的说明。
//
// 兜底三层: ①窗口自己的到点/堆上限 (startupgc.Guard.watch)；②main 的 defer
// 覆盖"命令跑到一半返回"的其余分支；③长驻循环入口 (startREPL) 显式还原。

import (
	"time"

	"github.com/14752222/Gox/internal/startupgc"
)

// startupGCWindow 是启动窗口的时长：首帧之后的一小段。
//
// 为什么不是"求值返回即还原"（那是最直觉的写法，实测过）：求值返回时降目标会
// **立刻**安排一轮 GC，它就落在"首帧已上屏 → 事件循环开始"之间 —— 用户看见了
// 窗口，但首帧口径里这轮 GC 仍被算进去（探测轮询在它之后才打印标记），优化在
// 数字上完全消失，也没有让任何真实等待变短。
//
// 100ms 的依据: 本机首帧 17~19ms、慢机 100ms 量级，所以 100ms 之后用户几乎
// 一定已经在看窗口；同时它足够短，不至于让"启动里灌数据"的应用在 200% 的目标
// 下攒出大堆（另有 64MB 堆上限兜底，见 startupgc.HeapGuardBytes）。
const startupGCWindow = 100 * time.Millisecond

// startupGuard 是当前窗口的句柄 (nil 安全)。
var startupGuard *startupgc.Guard

// startupWindowObserver 仅供测试断言"runFile 自己开过窗口"（nil 时零成本）。
//
// 为什么需要它：窗口的开关都在 runFile 内部，测试从外面只能看到"关着"这个
// 终态 —— 那与"根本没开过"无法区分，于是删掉 begin 也测不出来。
var startupWindowObserver func(open bool)

// beginStartupWindow 打开启动窗口。
//
// 重入必须**保留第一个句柄**而不是拿新的覆盖：`startupgc.Boost` 在窗口已开时
// 返回的是空 Guard（不叠加），若这里无条件赋值，第二次 begin 就会把真正的
// 句柄挤掉，之后的 end 变成空操作 —— 窗口一直开着。这是本批唯一一个
// "看起来完全正常、只有内存曲线变了"的坑，所以显式挡住。
func beginStartupWindow() {
	if startupgc.Active() {
		return
	}
	startupGuard = startupgc.Boost(startupGCWindow)
	if f := startupWindowObserver; f != nil {
		f(true)
	}
}

// endStartupWindow 立刻关闭启动窗口并还原 GC 目标 (幂等，nil 安全)。
//
// 正常路径不调它 —— 正常路径靠窗口自己的到点兜底，这样那次 GC 落在首帧之后。
// 它只用于"这里马上要进长驻循环，不该再等"的场合 (REPL) 与 main 的兜底 defer。
func endStartupWindow() {
	if startupGuard == nil {
		return
	}
	startupGuard.Restore()
	if f := startupWindowObserver; f != nil {
		f(false)
	}
}
