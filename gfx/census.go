package gfx

import "image/color"

// 绘制普查钩子 (G-2 光栅化可行性探针 / G-4 基准用)。
//
// 设计约束: **默认构建零行为变化、零可见开销**。
//   - drawCensusHook 默认 nil, censusDraw 是单 if 函数 (可内联), 没人安装
//     钩子时唯一代价是每次绘制原语多一次 nil 比对 —— 相对 FillRect 逐像素
//     循环可忽略。
//   - 它只**观测**, 不改绘制结果: 安装方 (探针测试 / 基准工具) 在测试进程里
//     赋值, 生产代码永远不装。
//
// 放这三个汇点的原因 (2026-11-17 探针结论的取样口):
//   - FillRect: "所有绘制最终都汇到 FillRect" (raster.go 注释自证) ——
//     矩形/边框/圆/粗线的合流点, 是 GPU 矩形批处理阶段要接管的调用量。
//   - blitGlyph: 文本绘制的字形级计数 —— GPU 文本阶段的图集查询次数口径。
//     合成粗体在 blitGlyph 内部压两遍 blitMask, 这里只记一次 (图集内存口径
//     按掩码算, 不按绘制次数算)。
//   - blendPixel: 覆盖率抗锯齿的逐像素散点 —— 这是"GPU 光栅化接管不了、
//     要么改成 coverage shader 要么留在 CPU"的那一类调用量, 探针要用它
//     回答"AA 散点占总绘制量的比例"。
//
// kind 取值: "fillrect" / "glyph" / "aapixel"。
// w/h 是请求尺寸 (FillRect 传入的原始 Rect, 未经 clip 收窄); aapixel 恒 1×1。
// alphaBlend 报告这次绘制是否走 src-over 半透明路径 (A<255) —— GPU 侧
// 混合语义对齐的难度信号。
var drawCensusHook func(kind string, w, h int, c color.RGBA, alphaBlend bool)

// censusDraw 是钩子的唯一触发点: nil 时被内联为空操作。
func censusDraw(kind string, w, h int, c color.RGBA, alphaBlend bool) {
	if drawCensusHook != nil {
		drawCensusHook(kind, w, h, c, alphaBlend)
	}
}
