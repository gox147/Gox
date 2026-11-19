package gfx

// ===== 显示后端建连预热 (T3-5 第十八批, 2026-11-19 夜批) =====
//
// 背景 (见 docs/performance.md §12.18): 首帧链路上"连上显示服务器并完成握手"
// 这一步单独跑只要 0.18ms, 但**在字体后台预取正在跑的时候会变成 1.2~1.7ms** ——
// 同一台机器、同一个二进制, 只改字体预取的起跑时间就能复现 (延迟 2ms 起 →
// 0.18ms; 立刻起 → 1.4ms)。§12.16 那批把字体预取挂在了 `gx/gfx` 模块查找处,
// 于是这笔 1.2ms 的惩罚恰好落在紧随其后的建窗握手上, 吃掉了一半收益。
//
// 本批的顺序调整: 在同一个"要 GUI 了"的信号上**先建连、再起字体预取**。
// 建连时还没有并发 I/O, 0.18ms 干干净净地过去; 之后字体那 1.78ms 才起来,
// 与建窗请求 / 建树 / 首帧布局并行。A/B: 进程级首帧 10.47ms → 9.77ms
// (−0.71ms, −6.7%, 15/15 轮更快)。
//
// 三条纪律 (与 font_prefetch.go 对齐):
//  1. **默认关闭**: 库不因为"被链接进某个进程"就去连显示服务器, 由 CLI 边界显式打开。
//  2. **没人要就不做**: 只在 `gx/gfx` 模块查找 (脚本确实要用 GUI) 与 Mount 兜底处调用;
//     `gox version` / 纯计算脚本一次都不连。
//  3. **失败静默**: 连不上 (无 DISPLAY / 无 X server) 就返回 false, 建窗时照旧
//     自己连并照旧报错 —— 预热不改变任何错误路径。
//
// 为什么不做成"后台 goroutine 里连": 建连本身要对 X server 做一次握手往返,
// 与字体解析争用时正是被拖慢的那一段; 放后台等于把惩罚又请回来 (实测后台连
// 与字体预取重叠时仍是 1.2ms 量级)。所以这里刻意让它在主线程上跑完再起预取。

import (
	"sync/atomic"
)

// displayPrewarmOn 由 CLI 边界打开 (EnableDisplayPrewarm)。默认 false。
var displayPrewarmOn atomic.Bool

// EnableDisplayPrewarm 打开"要用 GUI 时先建显示连接"。
func EnableDisplayPrewarm() { displayPrewarmOn.Store(true) }

// displayPrewarmer 是后端可选能力: 提前建连 (不建窗、不上屏)。
// 与 displayProvider 同一套纪律 —— 不扩 Surface 接口, 后端认领才生效。
type displayPrewarmer interface {
	PrewarmDisplay() bool
}

// PrewarmDisplay 让当前显示后端提前建立连接。
// 后端不支持 / 没有显示服务器时返回 false (调用方无需区分这两种情况)。
func PrewarmDisplay() bool {
	if defaultFactory == nil {
		return false
	}
	p, ok := defaultFactory.(displayPrewarmer)
	if !ok {
		return false
	}
	return p.PrewarmDisplay()
}

// prewarmDisplayForGUI 是首帧路径上的调用点: 开关关着就什么都不做。
// 幂等: 已经预热过就直接返回 (后端自己保证不重复建连)。
func prewarmDisplayForGUI() {
	if !displayPrewarmOn.Load() {
		return
	}
	PrewarmDisplay()
}
