package gfx

// ===== 默认字体预取 (T3-5 第十六批, 2026-11-19 夜批) =====
//
// 背景 (见 docs/performance.md §12.16): 首帧的 `Layout` 里有 1.78ms 是
// **首次用字体**的一次性开销 (目录扫描 0.66 + 选默认字体 1.12)。这块工作要读的
// 只有字体目录与 sfnt 表目录, 与建窗 (X11 的 connect / CreateWindow / CreateGC /
// 键盘映射 / Map / Sync) 没有任何数据依赖 —— 却被排在建窗**之后**, 白等 1.78ms。
//
// 本文件把它放到后台 goroutine, 与"模块装载 + 建树 + 建窗"并行:
//
//	import gx/gfx ──┬─ 预取开始 (本文件)
//	                └─ 模块装载 → 建树 → Mount → 建窗 → redraw → Layout ─→ 首次文本测量
//	                                                                        ↑ 这里等它
//
// 起跑线选在 `gx/gfx` 模块首次查找 (render.go): 那是"这台进程要 GUI 了"的
// 第一个确定信号, 离首次文本测量还有 1.2~1.5ms 可以盖。A/B 实测起跑线决定收益
// (Mount 处起只省 0.23ms, 模块处起省 0.70~0.95ms) —— 所以这里不是随手挑的位置。
//
// 四条纪律:
//  1. **默认关闭**, 由 CLI 边界 (cmd/gox) 显式开启 —— 与 devmode 同一条理由:
//     库不该因为被链接进某个进程就改变行为。gfx 单测大量 resetFontCaches,
//     后台加载会与它们抢字体状态 (fd / 缓存), 默认关闭才能保证测试进程干净。
//  2. **没人要就不做**: `gox version` / `gox create` 不 import `gx/gfx`, 一次
//     扫描都不付; 不开窗的纯计算脚本若 import 了 `gx/gfx`, 也只是后台 CPU
//     (不挡它的主线程)。与 §12.6 的"惰性"口径不冲突。
//  3. **失败静默**: 预取不报错也不改变选字体结果 —— 它调的就是 loadBaseFont,
//     与首帧路径同一个函数、同一把锁、同一份缓存; 失败时首帧仍会重试, 并照常
//     决定是降级 (静态候选 / 首个可解析面) 还是报错。
//  4. **幂等**: 每进程最多一个 goroutine; 重复触发 (模块查找 + Mount 兜底) 无害。

import (
	"sync"
	"sync/atomic"
)

// fontPrefetchOn 由 CLI 边界打开 (EnableFontPrefetch)。默认 false。
var fontPrefetchOn atomic.Bool

// fontPrefetchMu / started: 每个进程只起一个预热 goroutine。
// 不用 sync.Once 是因为测试需要把"已启动"复位 (Once 不能按值还原,
// 见 fontCandsRan 的注释), 而这里的状态只有"起过没起过"一件事。
var (
	fontPrefetchMu      sync.Mutex
	fontPrefetchStarted bool
)

// EnableFontPrefetch 打开"要用 GUI 时后台预热默认字体"。
//
// 为什么不放在进程启动: 那会让 `gox version` / `gox create` 这类子命令白付
// 一次字体加载, 也让"只跑计算、从不开窗"的脚本破功。起跑线放在
// `gx/gfx` 模块查找处, 恰好等于"这个脚本要 GUI"。
func EnableFontPrefetch() { fontPrefetchOn.Store(true) }

// prefetchBaseFont 在后台加载默认字体 (幂等, 每进程最多一个 goroutine)。
//
// 调用点两处 (都在 render.go): `gx/gfx` 模块工厂 (主起点) 与 Mount (兜底,
// 给不走 JS import 的宿主)。两处都幂等, 先到的那个赢。
func prefetchBaseFont() {
	if !fontPrefetchOn.Load() {
		return
	}
	fontPrefetchMu.Lock()
	if fontPrefetchStarted {
		fontPrefetchMu.Unlock()
		return
	}
	fontPrefetchStarted = true
	fontPrefetchMu.Unlock()

	go func() {
		// 返回值与错误都丢弃: 成功了首帧直接命中 baseFont; 失败了首帧照旧
		// 走 loadBaseFont 完整路径 (含错误上报与静态候选兜底)。
		_, _ = loadBaseFont()
	}()
}
