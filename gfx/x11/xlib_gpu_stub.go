//go:build linux && gpu && !(amd64 || arm64)

// GPU 轨 xlib 的架构占位: goffi 的 dlopen 只覆盖 linux amd64/arm64
// (见 goffi ffi/dl_unix.go build 约束)。其余架构上 gpu 轨本就未必能建
// (wgpu/goffi 同样受限), 这里诚实交 0 —— GPU 侧 gate 拒绝建链, 窗口
// 照常 CPU blit, 与「打不开 Xlib」的降级语义完全一致。
package x11

import "os"

var (
	xlibOnce sync.Once
	xlibDisp uintptr // 恒 0: 该架构 goffi dlopen 不可用
)

// xlibLibrary 与主轨共享环境变量口径 (测试引用)。
func xlibLibrary() string {
	if n := os.Getenv("GOX_XLIB_LIBRARY"); n != "" {
		return n
	}
	return "libX11.so.6"
}

func xlibDisplay() uintptr { return 0 }
