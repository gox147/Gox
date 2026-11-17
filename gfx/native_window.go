package gfx

// NativeWindowTarget 是平台窗口的**原生句柄包**, 供 GPU 呈现后端
// (gfx/gpu) 建 swapchain 用 —— 这是 A 路径「上屏合成默认化」的硬前提
// 之一 (docs/gpu-track-audit.md「A 路径硬前提缺口」第 1 条)。
//
// ## 为什么走可选接口而不是扩 Surface 契约
//
// 与 nativeVideoHost / softmedia 同一套纪律: 可选能力走可选接口。
// Surface 的五个方法 (Show/ShowRegions/Size/WaitEvents/Events) 是所有
// 实现方 (含测试 fake) 都必须满足的最小契约, 原生句柄只有三个桌面平台
// 后端给得出, 塞进主契约只会逼每个实现方写空实现。呈现包装器
// (SetPresentWrapper) 拿到 inner 后用类型断言探测:
//
//	if p, ok := inner.(NativeWindowProvider); ok {
//	    tgt, ok := p.NativeWindowTarget()
//	}
//
// ## 平台覆盖现状 (2026-11-17, 诚实口径)
//
//   - win32: ✅ HWND + HINSTANCE, wgpu 侧 SurfaceTargetFromWindowsHWND
//     直接可用 (gfx/win32)。
//   - x11: ⚠️ 只报 Window id, Display 恒 0 —— xgb 是纯 Go 协议实现,
//     拿不到 Xlib Display*, 而 wgpu 的 X11 surface 要求 Xlib 句柄
//     (SurfaceTargetFromXlibWindow)。在引入 Xlib Display* (dlopen 方案
//     待评估) 之前, GPU 侧对 x11 目标必须拒绝 —— 见 gpu 包 swapchain
//     接线处的 gate。
//   - cocoa: ❌ 未实现。wgpu 要 CAMetalLayer*, 需要先给 NSView
//     setWantsLayer+layer 并取层指针, 纯 Go objc 调用可行但未验证,
//     留给三平台验证阶段一并做 (G-3 窗口化分阶段)。
//   - android/ios/harmony: 未接入 (移动端本来就没接 SetPresentWrapper)。
type NativeWindowTarget struct {
	// Kind 标识窗口系统: "win32" / "x11" / "wayland" / "cocoa"。
	// GPU 后端按它决定喂给 wgpu 哪个 SurfaceTargetFrom* 构造器。
	Kind string
	// Display 是窗口系统连接句柄: win32 为 HINSTANCE, x11 为 Xlib
	// Display* (当前后端给不出, 恒 0), wayland 为 wl_display*。
	// cocoa 不用 (Metal 层从 View 自取)。
	Display uintptr
	// Window 是窗口/表面句柄: win32 为 HWND, x11 为 X Window id,
	// wayland 为 wl_surface*, cocoa 为 CAMetalLayer*。
	Window uintptr
}

// NativeWindowProvider 是「能交出原生窗口句柄」的可选能力接口。
// 实现方语义约定:
//   - 返回 false 表示当前窗口交不出可用句柄 (已销毁 / 平台不支持),
//     调用方安静退回 CPU blit, 不得重试到成功为止;
//   - 句柄必须在窗口生命周期内稳定 —— GPU 后端持有它建 surface,
//     窗口销毁后句柄失效的责任边界在实现方 (销毁时置 false 并停发帧)。
//   - 非热路径: 每窗口只在 GPU 包装时调用一次。
type NativeWindowProvider interface {
	NativeWindowTarget() (NativeWindowTarget, bool)
}
