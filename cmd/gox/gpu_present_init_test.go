//go:build gpu

// GPU 呈现接入层的降级语义测试钉 (G-3 验收项「降级路径有测试钉住」)。
//
// 覆盖三段:
//  1. planGPUPresent 纯判定表 —— unavailable 安静退回 / 软件适配器不包装 /
//     显式 GOX_GPU=software 强制包装;
//  2. installGPUPresent 实机行为 —— 沙箱无硬件时 wgpu 回软件适配器,
//     必须判 SoftNotForced 而不是包装 (慢 5 倍口径); 显式强制时判 Wrap;
//  3. gpuPresentWrapper 窗口化降级 —— 句柄给不出时返回 (nil,false),
//     绝不让窗口黑掉 (与 swapchain_gate_test 的 gate 探针同一口径)。
//
// 真上屏 (PresentsToWindow()==true) 需 GPU 硬件 + 真平台窗口, 沙箱验不到,
// 属 G-1 硬件口径覆盖项 —— 本文件**不**拿软件适配器数字冒充硬件口径。
package main

import (
	"image"
	"testing"
	"time"

	"github.com/14752222/Gox/gfx"
	"github.com/14752222/Gox/gfx/gpu"
)

// pinSurface 是最简 gfx.Surface (同 swapchain_gate_test 的 nopSurface)。
type pinSurface struct{}

func (pinSurface) Show(*image.RGBA)                           {}
func (pinSurface) ShowRegions(*image.RGBA, []image.Rectangle) {}
func (pinSurface) Size() (int, int)                           { return 64, 48 }
func (pinSurface) WaitEvents(time.Duration) bool              { return false }
func (pinSurface) Events() <-chan gfx.Event                   { return nil }

func TestPlanGPUPresent(t *testing.T) {
	cases := []struct {
		name      string
		available bool
		kind      string
		forced    bool
		want      gpuPresentPlan
	}{
		{"适配器建不起来 → 安静退回", false, "none", false, gpuPlanUnavailable},
		{"软件适配器未强制 → 不包装 (慢 5 倍口径)", true, "software", false, gpuPlanSoftNotForced},
		{"软件适配器 + GOX_GPU=software → 强制包装", true, "software", true, gpuPlanWrap},
		{"硬件适配器 → 包装", true, "hardware", false, gpuPlanWrap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planGPUPresent(tc.available, tc.kind, tc.forced)
			if got != tc.want {
				t.Fatalf("planGPUPresent(%v,%q,%v) = %v, want %v",
					tc.available, tc.kind, tc.forced, got, tc.want)
			}
		})
	}
}

// TestInstallGPUPresent_实机判定: 沙箱/无硬件环境下 wgpu 回软件适配器,
// 必须判 SoftNotForced; 显式 GOX_GPU=software 后必须判 Wrap。
// 无适配器环境 (Available()==false) 退路不在实机断言里 —— 由纯判定表钉住。
func TestInstallGPUPresent(t *testing.T) {
	if !gpu.Available() {
		t.Skip("GPU 呈现路径不可用 (无适配器), 实机判定由纯判定表钉住")
	}
	t.Setenv("GOX_GPU", "")
	gfx.SetPresentWrapper(nil) // 复位链, init 装过的也清掉
	t.Cleanup(func() { gfx.SetPresentWrapper(nil) })

	if got := installGPUPresent(); got != gpuPlanSoftNotForced {
		t.Fatalf("无硬件沙箱 BackendKind()=%q, 期望 SoftNotForced, got %v", gpu.BackendKind(), got)
	}

	t.Setenv("GOX_GPU", "software")
	if got := installGPUPresent(); got != gpuPlanWrap {
		t.Fatalf("GOX_GPU=software 强制后期望 Wrap, got %v", got)
	}
}

// TestGPUPresentWrapper_句柄不可用降级: 包装器拿到交不出原生句柄的 inner
// 时返回 (nil,false) —— 窗口走 CPU blit, 不黑窗。真上屏属 G-1 硬件口径。
func TestGPUPresentWrapper_HandleUnavailable(t *testing.T) {
	if !gpu.Available() {
		t.Skip("GPU 呈现路径不可用 (无适配器)")
	}
	t.Setenv("GOX_GPU", "software")

	s, ok := gpuPresentWrapper(pinSurface{})
	if ok {
		t.Fatalf("无原生句柄的 inner 包装成功 —— 兜底口径混入窗口化口径")
	}
	if s != nil {
		t.Fatalf("降级时应返回 nil surface, got %T", s)
	}
}
