//go:build darwin

// cocoa 的 GPU 呈现接线 —— A 路径「三平台验证」的最后一个桌面平台
// (gfx.NativeWindowProvider, 2026-11-19 落地)。
//
// ## 为什么必须两段式 (2026-11-18 设计约束, docs/gpu-track-audit.md)
//
// win32/x11 的 CPU blit 只读窗口句柄, GPU 接管与否不影响 CPU 路径;
// cocoa 不是 —— CPU blit 上屏走 CALayer.contents = NSImage。若在
// NativeWindowTarget() 里直接把 view 的层换成 CAMetalLayer, GPU 建链
// 一旦失败回退 CPU, contents 路径在 CAMetalLayer 上不渲染 → 黑窗。
//
// 所以拆成两段, 中间由 GPU 后端 (backend_wgpu.go) 决定提交:
//
//   - 第 1 段 NativeWindowTarget(): 只**创建** CAMetalLayer (不碰 view),
//     存在 surface 里, 原样交出句柄。建链失败时 view 从未被触碰 ——
//     CPU blit 路径零副作用。
//   - 第 2 段 CommitNativeWindow(ok): GPU 后端在 CreateSurfaceUnsafe +
//     Configure 全部成功后才调 ok=true 切层 (setLayer + setWantsLayer);
//     ok=false (建链失败 / swapchain 运行期失效) 时恢复原 backing layer
//     —— CPU blit (contents=NSImage) 立即可用, 窗口不黑。
//
// ## 诚实口径
//
// 本文件在无 macOS 真机的沙箱只能保证「编译通过 + 降级语义被 gfx/gpu
// 的 gate 测试钉住」; CAMetalLayer 真的能点亮 swapchain 属于 G-3 三平台
// 真机验证的硬件覆盖项, 未做之前不得声称 cocoa 平台 GPU 上屏可用。
package cocoa

import (
	"github.com/14752222/Gox/gfx"
	"github.com/ebitengine/purego/objc"
)

// metal.go 专用 selector (不得与 cocoa.go 已注册的重名 —— 同名 selector
// 注册两次会编译不过)。
var (
	selSetLayer = objc.RegisterName("setLayer:")
)

// 编译期断言: surface 满足 gfx 的原生窗口句柄 + 两段式提交可选能力。
var (
	_ gfx.NativeWindowProvider = (*surface)(nil)
	_ gfx.NativeWindowCommit   = (*surface)(nil)
)

// NativeWindowTarget 交出 CAMetalLayer 句柄 (gfx.NativeWindowProvider)。
//
// 这是两段式的**第 1 段**: 只创建层, 不切换 view 的层 —— 交给 wgpu 建
// Metal surface 用的指针在提交前对窗口完全无感。view 还没建好 / 原
// backing layer 拿不到 (切层失败后无法回滚) 时拒绝交出, GPU 侧安静退
// CPU blit。每次调用都可能新建一层 (GPU 后端每窗口只调一次, 见接口
// 契约); 上一段留下的未提交层由 CommitNativeWindow(false) 回收。
func (s *surface) NativeWindowTarget() (gfx.NativeWindowTarget, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.win == 0 || s.view == 0 || s.layer == 0 {
		// layer == 0 也拒绝: 两段式的回滚语义依赖「原层可恢复」,
		// 没有原层的窗口不许切层 (宁可不用 GPU, 不冒黑窗风险)。
		return gfx.NativeWindowTarget{}, false
	}
	caml := objc.ID(objc.GetClass("CAMetalLayer")).Send(selAlloc).Send(selInit)
	if caml == 0 {
		return gfx.NativeWindowTarget{}, false
	}
	// HiDPI: drawableSize 缺省按 bounds × contentsScale 推, 与 CPU blit
	// 同一套 scale 口径; 建链成功后 wgpu 按窗口尺寸重配。
	caml.Send(selSetContentsScale, s.scale)
	s.metalLayer = caml
	return gfx.NativeWindowTarget{
		Kind: "cocoa",
		// Display 不用 (Metal 层从 View 自取, 见 NativeWindowTarget
		// 结构体注释); Window 携带 CAMetalLayer*。
		Window: uintptr(caml),
	}, true
}

// CommitNativeWindow 两段式的**第 2 段** (gfx.NativeWindowCommit)。
//
//   - ok=true: GPU 建链成功, 正式切层。view 变成 layer-hosting
//     (显式 setLayer + setWantsLayer), 后续像素全部由 CAMetalLayer 呈现。
//     语义与 win32/x11 的降级不同: 这里**不承诺可逆**, 切层后的窗口
//     不再走 contents=NSImage —— GPU 后端承诺运行期失效时先调 ok=false
//     再落回 CPU blit。
//   - ok=false: 两种情形都收敛到「CPU blit 可用」: (a) 第 1 段建了层
//     但建链失败 —— 从未切过 view, 扔掉未提交层即可 (引用置 0, 单个
//     CALayer 的泄漏以窗口建链次数为上界, 实践上每窗口一次);
//     (b) swapchain 运行期失效 —— 恢复原 backing layer, contents 路径
//     立即恢复。幂等: 未提交过时 false 是 no-op, 重复 false 安全。
func (s *surface) CommitNativeWindow(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		if s.metalLayer == 0 || s.metalCommitted || s.view == 0 {
			return
		}
		// 切层顺序: setLayer 先给层、setWantsLayer 后宣告 hosting ——
		// AppKit 文档对 layer-hosting view 的要求 (顺序反了会被当成
		// layer-backed 重建层, 显式层被丢弃)。
		s.view.Send(selSetLayer, s.metalLayer)
		s.view.Send(selSetWantsLayer, true)
		s.metalCommitted = true
		return
	}
	if !s.metalCommitted {
		// 未提交过: 只是回收第 1 段的未用层。
		s.metalLayer = 0
		return
	}
	// 运行期失效: 恢复原 backing layer, CPU blit (contents=NSImage)
	// 的载体回来了, 窗口不黑。CAMetalLayer 由 wgpu surface 侧的失效
	// 语义接管, 这里不再持有。
	s.view.Send(selSetLayer, s.layer)
	s.view.Send(selSetWantsLayer, true)
	s.metalCommitted = false
	s.metalLayer = 0
}
