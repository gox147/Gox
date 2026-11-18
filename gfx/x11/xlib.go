//go:build linux && !gpu

// Xlib Display* 的 dlopen 方案 —— A 路径「GPU 上屏合成」三平台验证的
// 硬前提之一 (2026-11-18 落地, docs/gpu-track-audit.md「硬前提缺口」)。
//
// ## 问题
//
// wgpu 的 X11 surface 走 vkCreateXlibSurfaceKHR, 必须要 Xlib 的
// Display*; 而 gfx/x11 基于 jezek/xgb —— 纯 Go 的 X11 协议实现, 只有
// 自己的 socket 连接, 拿不出 Xlib 句柄。传统解法是 cgo 链接 libX11,
// 但那会破坏「零 cgo、单机交叉编译」的发行根基 (G-1 三轨核销的结论)。
//
// ## 方案
//
// 用 purego (已在 go.mod, gfx/cocoa 同款依赖) 运行时 dlopen
// libX11.so.6 → XOpenDisplay(NULL), 纯 Go、零 cgo。要点:
//
//   - XOpenDisplay(NULL) 与 xgb.NewConn 同源 (都读 DISPLAY 环境变量),
//     两条连接打开的是同一 X server。X Window id 是 server 侧资源,
//     任何连到同一 server 的连接都可引用 —— 这正是 wgpu 需要
//     「Display* + Window id」组合能渲染到 xgb 建的窗口的原因。
//   - 这条 Xlib 连接只交给 wgpu/Vulkan 建 VkSurface 用: 不读事件、
//     不画东西, 与 xgb 连接互不干扰。进程内所有窗口共享一条,
//     从不 XCloseDisplay (进程退出由 OS/X server 回收)。
//   - 进程启动早期就调 XInitThreads: Vulkan loader / wgpu 内部可能
//     从非调用线程触碰 Display*, 不初始化多线程支持是未定义行为。
//   - 失败语义遵循 NativeWindowProvider 接口约定: dlopen 或
//     XOpenDisplay 任一失败, 安静返回 0 且**不再重试** (sync.Once)。
//     GPU 侧 (backend_wgpu.go) 看到 Display==0 拒绝建 swapchain,
//     窗口照常走 CPU blit —— 行为与没有 GPU 轨时完全一致。
//
// ## 诚实口径
//
// 本方案在无头沙箱只能验证「打不开就老实降级」; Xlib Display* 真的
// 能让 swapchain 点亮, 属于 G-3 三平台真机验证的硬件覆盖项, 未做之前
// 不得声称 x11 平台 GPU 上屏可用。
package x11

import (
	"os"
	"sync"

	"github.com/ebitengine/purego"
)

var (
	xlibOnce sync.Once
	xlibDisp uintptr // 成功打开的 Xlib Display*, 0 = 不可用
)

// xlibLibrary 返回要 dlopen 的 Xlib 库名, 默认 libX11.so.6 (Debian/
// Fedora/Arch 通用 soname); GOX_XLIB_LIBRARY 可覆盖 (调试/特殊发行版)。
func xlibLibrary() string {
	if n := os.Getenv("GOX_XLIB_LIBRARY"); n != "" {
		return n
	}
	return "libX11.so.6"
}

// xlibDisplay 进程级懒打开一条 Xlib 连接, 返回 Display*; 失败返回 0
// (调用方按 NativeWindowProvider 约定安静退回 CPU blit)。非热路径:
// 每进程只真正执行一次, 之后都是读缓存。
func xlibDisplay() uintptr {
	xlibOnce.Do(func() {
		h, err := purego.Dlopen(xlibLibrary(), purego.RTLD_LAZY|purego.RTLD_LOCAL)
		if err != nil {
			return // 没有 libX11 (如无 X 的容器/精简发行版), 安静降级
		}
		var xInitThreads func() int
		var xOpenDisplay func(name *byte) uintptr
		purego.RegisterLibFunc(&xInitThreads, h, "XInitThreads")
		purego.RegisterLibFunc(&xOpenDisplay, h, "XOpenDisplay")
		// XInitThreads 只需成功调一次; 返回值非 0 即成功 (Status),
		// 失败也继续 —— 顶多是 Display* 的多线程使用有风险, 不该因此
		// 把整条 GPU 轨掐掉 (诚实口径: 这是已知取舍)。
		_ = xInitThreads()
		disp := xOpenDisplay(nil) // NULL → 读 DISPLAY, 与 xgb 同源
		if disp == 0 {
			// 无 X server / DISPLAY 指错了地方: 句柄用不上, 卸掉
			// (Dlclose 失败不影响降级路径, 忽略错误)。
			_ = purego.Dlclose(h)
			return
		}
		xlibDisp = disp
	})
	return xlibDisp
}
