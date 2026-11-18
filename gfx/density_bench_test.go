package gfx

import "testing"

// ===== 基准: 每帧布局入口的密度解析成本 (T3-5 第十四批) =====
//
// 这条基准量的是一件事: `Layout` 的第一行 `refreshDensity()` 花了多少。
// 它与进程级首帧 A/B 互补 —— 首帧口径里它被 X 连接建立、字体、渲染初始化
// 稀释到看不见 (实测 ±0.3ms 落在 14ms 的噪声带里), 而它本身是**每帧**都在
// 付的成本, 帧率口径上占比并不小。
//
// 用假的 displayProvider 且让它**每次都真的做一点事** (不能是空函数, 否则
// 编译器会把整段优化掉, 测出来的"往返成本"是假的零)。真实后端在这里是
// 若干次 socket 往返, 数量级远大于这个假实现 —— 所以下面的绝对值是**下限**,
// 用来说明"缓存命中路径确实没有调用后端", 不用来代表 X11 的真实往返。
//
// 跑法: go test ./gfx/ -run XXX -bench BenchmarkLayoutDensity -benchmem

func benchDensityTree(n int) *GuiNode {
	root := mkNode("column", map[string]float64{"padding": 8})
	var build func(parent *GuiNode, depth int)
	build = func(parent *GuiNode, depth int) {
		if depth == 0 {
			return
		}
		for i := 0; i < 3; i++ {
			c := mkNode("view", map[string]float64{"width": float64(20 + i), "padding": 4})
			c.Parent = parent
			parent.Children = append(parent.Children, c)
			build(c, depth-1)
		}
	}
	build(root, n)
	return root
}

// benchLayoutDensity 是两条基准共用的被测体: 对同一棵 3 层宽的树反复 Layout,
// 只改变"密度缓存是否每轮作废"。
func benchLayoutDensity(b *testing.B, invalidateEachFrame bool) {
	f := withProbeFactory(b, 2)
	root := benchDensityTree(3)
	Layout(root, 600, 400) // 预热: 进入缓存命中态
	base := f.calls.Load()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if invalidateEachFrame {
			noteSurfaceDisplayMaybeChanged(f.s)
		}
		Layout(root, 600, 400)
	}
	b.StopTimer()
	perOp := float64(f.calls.Load()-base) / float64(b.N)
	b.ReportMetric(perOp, "backend-calls/op")
	if invalidateEachFrame && perOp < 1 {
		b.Fatalf("作废后仍没有调用后端 (%.2f 次/op): 对照失真", perOp)
	}
	if !invalidateEachFrame && perOp != 0 {
		b.Fatalf("窗口未换屏却问了后端 %.2f 次/帧: 缓存没生效", perOp)
	}
}

// BenchmarkLayoutDensityCached 是**改动后**的形态: 窗口没换屏 ⇒ 每帧零往返。
func BenchmarkLayoutDensityCached(b *testing.B) { benchLayoutDensity(b, false) }

// BenchmarkLayoutDensityPerFrameResolve 是**改动前**的形态: 每帧都要向后端问一次
// (旧实现就是这样, 只是没有假后端可用时那次调用落空)。两者的差 = 每帧省下的
// "一次后端解析"(X11 上是三次同步往返, 远大于这里的假实现)。
func BenchmarkLayoutDensityPerFrameResolve(b *testing.B) { benchLayoutDensity(b, true) }
