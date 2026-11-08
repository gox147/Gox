//go:build gpu

package gpu

// 探针测试: wgpu 轨 (G-1 三轨核销)。真实报告本进程拿到的适配器:
// 有硬件 = "wgpu-<backend>-hw" + kind=hardware; 无硬件 = wgpu-software +
// kind=software (兜底口径, 数字不进闸门)。由 scripts/gpu-track-audit.sh 抓取。

import (
	"fmt"
	"testing"
)

func TestBackendTrackReport(t *testing.T) {
	av := Available()
	kind, backend := BackendKind(), Backend()
	fmt.Printf("TRACK=wgpu kind=%s backend=%s available=%v\n", kind, backend, av)
	if kind == "software" {
		fmt.Println("TRACK=wgpu note=fallback-no-hardware 兜底口径: 无 GPU 硬件, 数字不进闸门、不对外")
	}
}
