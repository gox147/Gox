//go:build !gpu && !gpu_cgo

package gpu

// 探针测试: 三轨核销用 (G-1)。每条构建轨一个同名 TestBackendTrackReport,
// 由 scripts/gpu-track-audit.sh 以 -v 运行并抓取 TRACK= 行, 拼出
// 「三轨 × 有/无硬件 × 平台」的路径核销表。
//
// 本文件是默认轨 (nogpu 空壳) 的探针: 断言「没开 GPU = 什么都不做」这条
// 构建期性质在运行时成立 —— Available 恒 false, New 恒 ErrNotBuilt。

import (
	"errors"
	"fmt"
	"testing"
)

func TestBackendTrackReport(t *testing.T) {
	if Available() {
		t.Fatal("nogpu 轨 Available() 必须为 false —— 出现 true 说明三轨互斥被破坏")
	}
	if Backend() != "none" || BackendKind() != "none" {
		t.Fatalf("nogpu 轨 Backend/BackendKind 必须为 none, got %q / %q", Backend(), BackendKind())
	}
	s, err := New(nil)
	if !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("nogpu 轨 New 必须返回 ErrNotBuilt, got %v", err)
	}
	if s != nil {
		t.Fatal("nogpu 轨 New 不得返回非 nil Surface")
	}
	fmt.Printf("TRACK=nogpu kind=%s backend=%s available=%v new_err=ErrNotBuilt\n",
		BackendKind(), Backend(), Available())
}
