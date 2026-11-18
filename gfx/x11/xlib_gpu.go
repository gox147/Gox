//go:build linux && gpu && (amd64 || arm64)

// Xlib Display* 的 goffi dlopen 方案 —— gfx/x11/xlib.go (purego 版) 在
// **GPU 轨**下的替身 (2026-11-19, G-1 三轨核销交叉编译矩阵修复)。
//
// ## 为什么 GPU 轨必须换掉 purego
//
// `-tags gpu` 拉进来的 go-webgpu/goffi 与 ebitengine/purego 各自带了一份
// fakecgo (都定义 `_cgo_init` 等符号), 同时链进同一个二进制是链接期硬错误:
//
//	CGO_ENABLED=0 → link: duplicated definition of symbol _cgo_init,
//	                from goffi/internal/fakecgo and purego/internal/fakecgo
//
// 这正是 gfx/softmedia/pulse_disabled.go 早已踩过并写进文档的坑 —— 而
// 2026-11-18 落地的 xlib.go (purego 版) 恰恰违反了这条纪律: 它是 linux 下
// `-tags gpu` 构建中唯一的 purego 入口, 导致真零 cgo (CGO_ENABLED=0) 的
// 交叉编译矩阵在 linux/amd64+arm64 上炸链接 (windows 因 purego 不走
// fakecgo 幸免; darwin 的冲突源在 gfx/cocoa 的 purego, 另案处理)。
//
// 解法: GPU 轨下用 goffi 的 LoadLibrary/GetSymbol/CallFunction —— 与
// go-webgpu 同源 fakecgo, 天然不冲突。非 GPU 轨维持 purego 版不动
// (默认构建零行为变化)。
//
// 降级语义与 purego 版逐字一致: dlopen/XOpenDisplay 任一失败安静返回 0
// 且不再重试 (sync.Once), GPU 侧 gate 拒绝建链、窗口照常 CPU blit。
package x11

import (
	"os"
	"sync"
	"unsafe"

	"github.com/go-webgpu/goffi/ffi"
	"github.com/go-webgpu/goffi/types"
)

var (
	xlibOnce sync.Once
	xlibDisp uintptr // 成功打开的 Xlib Display*, 0 = 不可用
)

// xlibLibrary 返回要 dlopen 的 Xlib 库名, 默认 libX11.so.6 (Debian/
// Fedora/Arch 通用 soname); GOX_XLIB_LIBRARY 可覆盖 (调试/特殊发行版)。
// 与 purego 版共享同一环境变量口径。
func xlibLibrary() string {
	if n := os.Getenv("GOX_XLIB_LIBRARY"); n != "" {
		return n
	}
	return "libX11.so.6"
}

// xlibCall 绑定一个无参/单指针参的 libX11 函数。失败返回 nil, 调用方安静降级。
type xlibFn struct {
	cif types.CallInterface
	fn  unsafe.Pointer
}

func xlibBind(h unsafe.Pointer, name string, ret *types.TypeDescriptor, args []*types.TypeDescriptor) *xlibFn {
	sym, err := ffi.GetSymbol(h, name)
	if err != nil || sym == nil {
		return nil
	}
	var cif types.CallInterface
	if err := ffi.PrepareCallInterface(&cif, types.DefaultCall, ret, args); err != nil {
		return nil
	}
	return &xlibFn{cif: cif, fn: sym}
}

// xlibDisplay 进程级懒打开一条 Xlib 连接, 返回 Display*; 失败返回 0
// (调用方按 NativeWindowProvider 约定安静退回 CPU blit)。非热路径:
// 每进程只真正执行一次, 之后都是读缓存。
func xlibDisplay() uintptr {
	xlibOnce.Do(func() {
		h, err := ffi.LoadLibrary(xlibLibrary())
		if err != nil || h == nil {
			return // 没有 libX11 (如无 X 的容器/精简发行版), 安静降级
		}
		xInitThreads := xlibBind(h, "XInitThreads", types.IntTypeDescriptor, nil)
		xOpenDisplay := xlibBind(h, "XOpenDisplay", types.PointerTypeDescriptor,
			[]*types.TypeDescriptor{types.PointerTypeDescriptor})
		if xOpenDisplay == nil {
			return
		}
		// XInitThreads 只需成功调一次; 返回值非 0 即成功 (Status),
		// 失败也继续 —— 顶多是 Display* 的多线程使用有风险, 不该因此
		// 把整条 GPU 轨掐掉 (诚实口径: 这是已知取舍)。
		if xInitThreads != nil {
			var st int32
			_, _ = ffi.CallFunction(&xInitThreads.cif, xInitThreads.fn,
				unsafe.Pointer(&st), nil)
		}
		var name unsafe.Pointer // NULL → 读 DISPLAY, 与 xgb 同源
		var disp uintptr
		_, err = ffi.CallFunction(&xOpenDisplay.cif, xOpenDisplay.fn,
			unsafe.Pointer(&disp), []unsafe.Pointer{unsafe.Pointer(&name)})
		if err != nil || disp == 0 {
			// 无 X server / DISPLAY 指错了地方: 句柄用不上, 卸掉
			// (FreeLibrary 失败不影响降级路径, 忽略错误)。
			_ = ffi.FreeLibrary(h)
			return
		}
		xlibDisp = disp
	})
	return xlibDisp
}
