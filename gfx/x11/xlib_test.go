//go:build linux

package x11

import "testing"

// TestXlibDisplaySilentDegrade 钉住 dlopen 方案的降级语义 (A 路径硬前提
// #3): 无论有无 X 环境, xlibDisplay 都不 panic; 打不开 Xlib 或连不上
// X server 就老实返回 0, GPU 侧 gate 会因此拒绝建 swapchain —— 这正是
// 设计行为 (安静退 CPU blit), 不是缺陷。真机点亮属于 G-3 三平台验证,
// 无头沙箱/CI 验不到真上屏, 也不许拿兜底口径冒充。
func TestXlibDisplaySilentDegrade(t *testing.T) {
	d := xlibDisplay()
	if d == 0 {
		t.Log("Xlib 不可用 (无 libX11 或连不上 X server), 返回 0 —— 降级路径正确")
		return
	}
	t.Logf("Xlib Display* = %#x (有 X 环境)", d)
	// once 语义: 二次调用必须返回同一条连接 (进程级共享)。
	if d2 := xlibDisplay(); d2 != d {
		t.Fatalf("xlibDisplay 两次调用返回不同句柄: %#x vs %#x", d, d2)
	}
}

// TestXlibLibraryOverride 钉住库名覆盖入口: GOX_XLIB_LIBRARY 必须生效,
// 这是特殊发行版/调试场景的逃生门 (不设则回默认 soname)。
func TestXlibLibraryOverride(t *testing.T) {
	t.Setenv("GOX_XLIB_LIBRARY", "libX11.so.notexist")
	if got := xlibLibrary(); got != "libX11.so.notexist" {
		t.Fatalf("GOX_XLIB_LIBRARY 未生效: got %q", got)
	}
	t.Setenv("GOX_XLIB_LIBRARY", "")
	if got := xlibLibrary(); got != "libX11.so.6" {
		t.Fatalf("默认 soname 不对: got %q", got)
	}
}
