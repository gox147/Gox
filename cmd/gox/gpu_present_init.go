//go:build gpu

// GPU 呈现后端的接入点 (阶段 1, 接缝 A)。
//
// 整个文件只有 -tags gpu 才编译: 默认构建连 gfx/gpu 都不 import,
// 「GUI 没开 GPU = PC 零 cgo」由 build tag 保证, 不靠运行时判断。
//
// ## 包装策略: 只包装硬件适配器
//
// 无 GPU 硬件时 wgpu 会拿软件渲染器 (纯 Go 软件 HAL / SwiftShader /
// llvmpipe)。实测 (scripts/bench-present-4k.sh, 4K 全帧): 软件后端做
// 合成的应用 CPU 约 168ms/帧, 而 x11 PutImage 只要 32ms/帧 —— **慢 5 倍**,
// 因为纯 Go 软光栅把合成接了过去, 且每 Draw 全量重建管线/深度缓冲
// (约 100MB/帧分配)。所以生产路径默认不包装软件适配器; GOX_GPU=software
// 可强制打开 (CI 像素级验证 / 无头调试用)。
//
// 为什么读环境变量的代码在 cmd 而不是库里: 见 initDevMode 同款纪律 ——
// 库不该因为「被链接进某个进程」就改变行为, 配置由 CLI 边界显式传入。

package main

import (
	"fmt"
	"os"

	"github.com/14752222/Gox/gfx"
	"github.com/14752222/Gox/gfx/gpu"
)

// gpuPresentPlan 是接入层的判定结果 —— 抽成纯函数是为了可测 (G-3 验收:
// 降级路径必须有测试钉住, init 里的不可测判定等于裸奔)。
type gpuPresentPlan int

const (
	// gpuPlanUnavailable: 适配器建不起来 (Available()==false) → 安静退回,
	// 窗口照常走平台 CPU blit —— GPU 是加速器, 不是依赖。
	gpuPlanUnavailable gpuPresentPlan = iota
	// gpuPlanSoftNotForced: 适配器是软件渲染且未显式 GOX_GPU=software →
	// 不包装 (文件头: 软件后端慢 5 倍), 只打一行 stderr 提示。
	gpuPlanSoftNotForced
	// gpuPlanWrap: 安装呈现包装器; 单个窗口是否真走 GPU 由包装器内动态
	// 判定 (swapchain 就绪才真上屏, 见 gpuPresentWrapper)。
	gpuPlanWrap
)

// planGPUPresent 是 init 判定的纯函数形态: available = gpu.Available(),
// kind = gpu.BackendKind(), forced = GOX_GPU=software。
func planGPUPresent(available bool, kind string, forced bool) gpuPresentPlan {
	if !available {
		return gpuPlanUnavailable
	}
	if kind == "software" && !forced {
		return gpuPlanSoftNotForced
	}
	return gpuPlanWrap
}

// gpuPresentWrapper 是安装进 gfx 的呈现包装器 (行为与 2026-11-17 版完全一致)。
// 返回 ok=false 表示该窗口降级 CPU blit, 绝不让窗口黑掉。
func gpuPresentWrapper(inner gfx.Surface) (gfx.Surface, bool) {
	s, err := gpu.New(inner)
	if err != nil || !s.PresentsToWindow() {
		if err != nil {
			fmt.Fprintf(os.Stderr, "gox: GPU 呈现不可用, 退回 CPU blit: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr,
				"gox: GPU 呈现未启用 —— 原生句柄不可用 (平台 %T 暂无 swapchain 接线), 退回 CPU blit\n", inner)
		}
		return nil, false
	}
	return s, true
}

// installGPUPresent 是 init 的可测主体; 返回判定结果供测试断言。
func installGPUPresent() gpuPresentPlan {
	plan := planGPUPresent(gpu.Available(), gpu.BackendKind(), os.Getenv("GOX_GPU") == "software")
	switch plan {
	case gpuPlanUnavailable:
		return plan
	case gpuPlanSoftNotForced:
		fmt.Fprintf(os.Stderr,
			"gox: GPU 呈现未启用 —— 适配器为软件渲染 (%s), 实测比 CPU blit 慢; "+
				"设 GOX_GPU=software 可强制启用\n", gpu.Backend())
		return plan
	}
	gfx.SetPresentWrapper(gpuPresentWrapper)
	return plan
}

func init() {
	installGPUPresent()
}
