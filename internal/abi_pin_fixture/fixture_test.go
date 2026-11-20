package abi_pin_fixture

import (
	"testing"
	"unsafe"
)

// 这个包没有真实测试 —— 它只是 tools/perf 形态闸门的编译目标。这里放一条
// 最小自检, 免得 `go test ./...` 把它当成"没有测试的空包"。
func TestLocalsDirectRoundTrip(t *testing.T) {
	f := &Frame{Locals: make([]Value, 4)}
	LocalsDirect(f, "v")
	if f.Locals[1] != "v" {
		t.Fatalf("写入未生效: %v", f.Locals[1])
	}
}

// 反例形态的自检: 保证 PtrIndirect 不会被编译器当成死代码删掉 (否则形态闸门
// 的"负样本必红"这一半就失效了)。
func TestPtrIndirectRoundTrip(t *testing.T) {
	vals := make([]Value, 4)
	ptrs := make([]unsafe.Pointer, 4)
	for i := range vals {
		ptrs[i] = unsafe.Pointer(&vals[i])
	}
	f := &PtrFrame{Ptrs: ptrs}
	PtrIndirect(f, "w")
	if vals[1] != "w" {
		t.Fatalf("反例形态写入未生效: %v", vals[1])
	}
}

func TestPtrIndirectLoopRoundTrip(t *testing.T) {
	vals := make([]Value, 4)
	ptrs := make([]unsafe.Pointer, 4)
	for i := range vals {
		ptrs[i] = unsafe.Pointer(&vals[i])
	}
	f := &PtrFrame{Ptrs: ptrs}
	PtrIndirectLoop(f, 1, "w")
	if vals[1] != "w" {
		t.Fatalf("可变下标反例写入未生效: %v", vals[1])
	}
	lf := &Frame{Locals: make([]Value, 4)}
	LocalsLoop(lf, 1, "x")
	if lf.Locals[1] != "x" {
		t.Fatalf("对照组写入未生效: %v", lf.Locals[1])
	}
}
