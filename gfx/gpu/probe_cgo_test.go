//go:build gpu_cgo

package gpu

// 探针测试: cgo 轨 (G-1 三轨核销)。骨架阶段恒不可用 (Available=false /
// kind=none) —— 探针的作用是把「骨架阶段不声称可用」这条纪律钉进运行时,
// 后续 cgo 绑定补齐后此断言自然失败, 提示同步更新核销表。

import (
	"errors"
	"fmt"
	"testing"
)

func TestBackendTrackReport(t *testing.T) {
	if Available() {
		t.Fatal("cgo 轨骨架阶段 Available() 必须为 false; 若绑定已补齐, 请同步更新 scripts/gpu-track-audit.sh 的核销表")
	}
	if _, err := New(nil); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("cgo 轨骨架阶段 New 必须返回 ErrNotBuilt, got %v", err)
	}
	fmt.Printf("TRACK=cgo kind=%s backend=%s available=%v\n", BackendKind(), Backend(), Available())
}
