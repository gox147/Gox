//go:build gpu

package gpu

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/14752222/Gox/gfx"
	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	_ "github.com/gogpu/wgpu/hal/allbackends" // 注册 Vulkan/DX12/Metal/GLES/Software 后端
)

// 包级单例: 全部窗口共享一个 GPU device (多窗口 / 多屏场景下驱动对象只建一份,
// 每窗口一个 surface/swapchain —— 与 gogpu 应用框架同一模型)。
// Instance 与 Adapter 一并持有: swapchain 建链 (CreateSurfaceUnsafe) 要用
// Instance, 能力查询 (GetSurfaceCapabilities) 要用 Adapter —— 回收它们
// 会导致窗口化路径整个瘸掉, 且 Go 侧无警告, 只能真机上查。
var (
	devOnce     sync.Once
	devInstance *wgpu.Instance
	devAdapter  *wgpu.Adapter
	devInst     *wgpu.Device
	devErr      error
	devInfo     gputypes.AdapterInfo // 建 device 时那张适配器的信息 (Backend() 用)
)

// Device 返回进程级共享 GPU device; 不可用时 (无适配器/驱动) 返回 error,
// 调用方应退回原平台后端, 行为降级而非失败。
func Device() (*wgpu.Device, error) {
	devOnce.Do(func() {
		inst, err := wgpu.CreateInstance(nil)
		if err != nil {
			devErr = fmt.Errorf("wgpu instance: %w", err)
			return
		}
		ad, err := inst.RequestAdapter(nil)
		if err != nil {
			// 硬件适配器不可得 → 软件渲染器 (无 GPU 机器的降级路径)
			ad, err = inst.RequestAdapter(&wgpu.RequestAdapterOptions{ForceFallbackAdapter: true})
			if err != nil {
				devErr = fmt.Errorf("wgpu adapter: %w", err)
				return
			}
		}
		d, err := ad.RequestDevice(nil)
		if err != nil {
			devErr = fmt.Errorf("wgpu device: %w", err)
			return
		}
		devInstance, devAdapter = inst, ad
		devInst = d
		devInfo = ad.Info()
	})
	return devInst, devErr
}

// Available 报告 GPU 呈现路径是否可用 (设备能建起来即可, 软件后端也算可用)。
func Available() bool {
	_, err := Device()
	return err == nil
}

// Backend 报告当前实际拿到的后端 (调试面板 / canIUse 式能力探测用):
// "wgpu-vulkan-hw" / "wgpu-metal-hw" / "wgpu-software" / "none"。
// 软件适配器 (DeviceTypeCPU: SwiftShader/llvmpipe/wgpu 纯 Go 软件 HAL)
// 统一报 "wgpu-software" —— 它的 API 后端字段是 Empty, 报出来只会误导。
func Backend() string {
	_, err := Device()
	if err != nil {
		return "none"
	}
	if devInfo.DeviceType == gputypes.DeviceTypeCPU {
		return "wgpu-software"
	}
	return fmt.Sprintf("wgpu-%s-hw", devInfo.Backend.String())
}

// BackendKind 报告适配器种类: "hardware" / "software" / "none"。
//
// 呈现包装策略靠它分流: 无 GPU 硬件时软件后端做合成**比 CPU blit 慢**
// (4K 实测约 5x, 见 scripts/bench-present-4k.sh —— 纯 Go 软光栅每 Draw
// 还全量重建管线/深度缓冲, 100MB/帧分配), 所以生产路径只在 hardware 时
// 包装; software 是 CI/无头验证的确定性后端, 由调用方显式选择。
func BackendKind() string {
	_, err := Device()
	if err != nil {
		return "none"
	}
	if devInfo.DeviceType == gputypes.DeviceTypeCPU {
		return "software"
	}
	return "hardware"
}

// Surface 实现 gfx.Surface: 事件/尺寸/等待委托内层平台 Surface,
// Show/ShowRegions 走 GPU 合成。内层为 nil 时是纯离屏模式 (测试/无头)。
//
// 线程契约与 gfx 一致: 全部方法只在渲染线程 (LockOSThread 的那个) 调用,
// 内部不加锁 —— wgpu device 本身不是线程安全的, 这与 Gox 单线程 GUI 模型匹配。
type Surface struct {
	inner gfx.Surface // 可为 nil (离屏)

	dev      *wgpu.Device
	pipeline *wgpu.RenderPipeline
	bindLyt  *wgpu.BindGroupLayout
	vbuf     *wgpu.Buffer
	sampler  *wgpu.Sampler

	// swapchain 备用管线要复用的共享资源 (buildPipeline 建好后留存):
	shaderModule  *wgpu.ShaderModule
	layoutForSwap *wgpu.PipelineLayout
	swapPipeline  *wgpu.RenderPipeline // 非 RGBA8 swapchain 格式的管线 (按需)

	// fbTex 是 CPU 帧缓冲的 GPU 镜像 (持久, 尺寸随窗口变化重建)。
	fbTex    *wgpu.Texture
	fbView   *wgpu.TextureView
	bindGrp  *wgpu.BindGroup
	fbW, fbH int

	// target 是合成目标: 离屏模式 (测试/无头) 的渲染终点; 窗口化模式的
	// Readback/兜底纹理 (真渲染终点是 swapchain 纹理, 见 wgpuSurf)。
	target *wgpu.Texture
	// targetView 是 target 的视图 (随 target 重建; 每帧建视图再 Release
	// 是白付一笔驱动调用, 4K 下尤其冤 —— 缓存之)。
	targetView *wgpu.TextureView

	// ── 窗口化 swapchain (A 路径硬前提 #2, 2026-11-17 接线) ──
	// wgpuSurf 非 nil = 窗口化真上屏已就绪: 渲染终点是 swapchain 纹理,
	// ShowRegions 收尾 Present 到原生窗口 (不再只有离屏合成 + readback)。
	// swapBroken = 运行期失效开关: 任何 acquire/render/present 错误都会
	// 置位并永久退回「内层 CPU blit」—— swapchain 疯掉不该拖死窗口,
	// 这与 New 失败时不包装是同一条降级语义, 只是时机晚一拍。
	wgpuSurf     *wgpu.Surface
	swapFormat   gputypes.TextureFormat // swapchain 实际格式 (可能非 RGBA8, 如 BGRA8)
	swapW, swapH int
	swapBroken   bool

	// uploadScratch 是子纹理上传的复用缓冲 (按最大脏区增长)。wgpu 的
	// WriteTexture 语义保证调用返回时数据已被拷走, 所以下一帧可以直接
	// 覆写 —— 每帧 make 33MB 再让 GC 追, 是白给应用 CPU 加税。
	uploadScratch []byte

	// 帧统计 (给 gx/dev 的调试面板用: "GPU 呈现路径到底走了哪条路")。
	frames        int // 呈现帧数
	fullFrames    int // 整帧上传的次数
	regionFrames  int // 局部 (脏矩形) 上传的帧数
	uploadedDrawn int // 累计合成的矩形数

	width, height int
}

// Stats 是 GPU 呈现路径的帧统计 —— 用来回答「这帧到底走了全帧还是脏矩形」。
type Stats struct {
	Frames        int // 呈现帧数
	FullFrames    int // 整帧次数
	RegionFrames  int // 局部帧次数
	UploadedDrawn int // 累计合成矩形数
}

// 编译期断言: Surface 确实是 gfx.Surface。
var _ gfx.Surface = (*Surface)(nil)

// New 建 GPU 呈现 Surface。inner 为内层平台 Surface (提供事件与窗口;
// 可为 nil = 纯离屏)。返回 error 时 GPU 路径不可用, 调用方应继续用 inner。
func New(inner gfx.Surface) (*Surface, error) {
	dev, err := Device()
	if err != nil {
		return nil, err
	}
	s := &Surface{inner: inner, dev: dev}
	if inner != nil {
		s.width, s.height = inner.Size()
	} else {
		s.width, s.height = 400, 300
	}
	if err := s.buildPipeline(); err != nil {
		return nil, err
	}
	if err := s.ensureTargets(s.width, s.height); err != nil {
		return nil, err
	}
	// 窗口化: 尝试建 swapchain 真上屏 (A 路径硬前提 #2)。失败**不是错误**:
	// Surface 照常返回 (离屏模式, 测试/无头照用), 只是窗口拿不到像素 ——
	// 上层 (cmd/gox 包装点) 用 PresentsToWindow() 判断要不要真包装,
	// 真窗口没 swapchain 就退回 CPU blit, 行为降级而非失败。
	_ = s.initSwapchain()
	return s, nil
}

// PresentsToWindow 报告本 Surface 是否真的能把像素呈到原生窗口
// (swapchain 就绪)。呈现包装点必须以它为准决定要不要包装真平台窗口:
// false 时包装等于把窗口变黑 (GPU 轨只到离屏纹理为止)。
// 离屏模式 (inner==nil, 测试/无头) 恒 false, 但那不是包装场景。
func (s *Surface) PresentsToWindow() bool { return s.wgpuSurf != nil }

// initSwapchain 从内层平台 Surface 拿原生句柄建 wgpu surface 并配置
// swapchain。三道 gate, 任何一道不过都返回 error (调用方退回 CPU blit):
//
//  1. inner 实现 gfx.NativeWindowProvider (可选接口, 不扩 Surface 契约);
//  2. 目标 kind 是 wgpu 支持的窗口系统;
//  3. 平台后端交得出 wgpu 需要的句柄形态 (x11 目前交不出 Xlib Display*)。
//
// 口径纪律: 本函数只决定「能不能上屏」, 不做任何性能声明 —— 是否比
// CPU blit 快要靠硬件口径基准 (bench-present-4k.sh, 不接受兜底口径)。
func (s *Surface) initSwapchain() error {
	if devInstance == nil || devAdapter == nil {
		return fmt.Errorf("gpu: instance/adapter 未就绪")
	}
	p, ok := s.inner.(gfx.NativeWindowProvider)
	if !ok {
		return fmt.Errorf("inner %T 未实现 NativeWindowProvider", s.inner)
	}
	tgt, ok := p.NativeWindowTarget()
	if !ok {
		return fmt.Errorf("inner 交不出原生窗口句柄")
	}
	var st wgpu.SurfaceTargetUnsafe
	switch tgt.Kind {
	case "win32":
		st = wgpu.SurfaceTargetFromWindowsHWND(tgt.Display, tgt.Window)
	case "x11":
		// Display* 由 gfx/x11 的 dlopen 方案供给 (xlib.go, 2026-11-18
		// 落地); dlopen/XOpenDisplay 失败时仍是 0 —— 这道 gate 保留:
		// 0 = 安静退 CPU blit, 行为与无 GPU 轨完全一致。
		if tgt.Display == 0 {
			return fmt.Errorf("x11: 无 Xlib Display*, wgpu 建不了 surface")
		}
		st = wgpu.SurfaceTargetFromXlibWindow(tgt.Display, tgt.Window)
	default:
		// cocoa / wayland / android: 未实现或未接入, 见 gfx/native_window.go
		return fmt.Errorf("平台 %q 暂无 swapchain 接线", tgt.Kind)
	}
	surf, err := devInstance.CreateSurfaceUnsafe(st)
	if err != nil {
		return fmt.Errorf("create surface: %w", err)
	}
	caps := devAdapter.GetSurfaceCapabilities(surf)
	if len(caps.Formats) == 0 {
		return fmt.Errorf("surface 无可用格式")
	}
	// 优先 RGBA8Unorm (与离屏管线同格式, 免建第二条管线); 拿不到就
	// 用适配器首选格式, 渲染时走 pipelineFor 建对应格式的管线。
	format := caps.Formats[0]
	for _, f := range caps.Formats {
		if f == gputypes.TextureFormatRGBA8Unorm {
			format = f
			break
		}
	}
	w, h := s.inner.Size()
	if err := surf.Configure(s.dev, &wgpu.SurfaceConfiguration{
		Width: uint32(w), Height: uint32(h),
		Format:      format,
		Usage:       gputypes.TextureUsageRenderAttachment,
		PresentMode: gputypes.PresentModeFifo, // Fifo 全平台保证支持, VSync 防撕裂
	}); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	s.wgpuSurf = surf
	s.swapFormat = format
	s.swapW, s.swapH = w, h
	return nil
}

// pipelineFor 返回适配给定颜色格式的呈现管线 (按需建, 缓存于 swapPipeline)。
// swapchain 格式与离屏 RGBA8 不一致时 (常见: 窗口系统只给 BGRA8) 必须用
// 匹配的管线, 否则验证层报格式不兼容。
func (s *Surface) pipelineFor(format gputypes.TextureFormat) (*wgpu.RenderPipeline, error) {
	if format == gputypes.TextureFormatRGBA8Unorm {
		return s.pipeline, nil
	}
	if s.swapPipeline != nil {
		return s.swapPipeline, nil
	}
	// 复用顶点缓冲与布局, 只换 Fragment 目标格式重建。
	pl, err := s.dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
		Label:  "gpu-present-pipeline-swap",
		Layout: s.layoutForSwap,
		Vertex: wgpu.VertexState{
			Module: s.shaderModule, EntryPoint: "vs_main",
			Buffers: []gputypes.VertexBufferLayout{{
				ArrayStride: 16, StepMode: gputypes.VertexStepModeVertex,
				Attributes: []gputypes.VertexAttribute{
					{Format: gputypes.VertexFormatFloat32x2, Offset: 0, ShaderLocation: 0},
					{Format: gputypes.VertexFormatFloat32x2, Offset: 8, ShaderLocation: 1},
				},
			}},
		},
		Fragment: &wgpu.FragmentState{
			Module: s.shaderModule, EntryPoint: "fs_main",
			Targets: []gputypes.ColorTargetState{{
				Format: format, WriteMask: gputypes.ColorWriteMaskAll,
			}},
		},
		Primitive: gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList},
	})
	if err != nil {
		return nil, err
	}
	s.swapPipeline = pl
	return pl, nil
}

// buildPipeline 建「全屏 textured quad」管线 (呈现路径唯一需要的绘制)。
func (s *Surface) buildPipeline() error {
	type V struct{ x, y, u, v float32 }
	verts := []V{
		{-1, 1, 0, 0}, {1, 1, 1, 0}, {-1, -1, 0, 1},
		{-1, -1, 0, 1}, {1, 1, 1, 0}, {1, -1, 1, 1},
	}
	buf := make([]byte, len(verts)*16)
	for i, vt := range verts {
		putF32(buf[i*16:], vt.x, vt.y, vt.u, vt.v)
	}
	vbuf, err := s.dev.CreateBuffer(&wgpu.BufferDescriptor{
		Label: "gpu-present-quad", Size: uint64(len(buf)),
		Usage: gputypes.BufferUsageVertex | gputypes.BufferUsageCopyDst,
	})
	if err != nil {
		return err
	}
	if err := s.dev.Queue().WriteBuffer(vbuf, 0, buf); err != nil {
		return err
	}
	s.vbuf = vbuf

	shader, err := s.dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{
		Label: "gpu-present", WGSL: presentWGSL,
	})
	if err != nil {
		return err
	}
	s.shaderModule = shader

	bgl, err := s.dev.CreateBindGroupLayout(&wgpu.BindGroupLayoutDescriptor{
		Entries: []gputypes.BindGroupLayoutEntry{
			{Binding: 0, Visibility: gputypes.ShaderStageFragment,
				Texture: &gputypes.TextureBindingLayout{SampleType: gputypes.TextureSampleTypeFloat}},
			{Binding: 1, Visibility: gputypes.ShaderStageFragment,
				Sampler: &gputypes.SamplerBindingLayout{Type: gputypes.SamplerBindingTypeFiltering}},
		},
	})
	if err != nil {
		return err
	}
	s.bindLyt = bgl

	layout, err := s.dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{
		Label: "gpu-present-layout", BindGroupLayouts: []*wgpu.BindGroupLayout{bgl},
	})
	if err != nil {
		return err
	}
	s.layoutForSwap = layout
	pl, err := s.dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
		Label:  "gpu-present-pipeline",
		Layout: layout,
		Vertex: wgpu.VertexState{
			Module: shader, EntryPoint: "vs_main",
			Buffers: []gputypes.VertexBufferLayout{{
				ArrayStride: 16, StepMode: gputypes.VertexStepModeVertex,
				Attributes: []gputypes.VertexAttribute{
					{Format: gputypes.VertexFormatFloat32x2, Offset: 0, ShaderLocation: 0},
					{Format: gputypes.VertexFormatFloat32x2, Offset: 8, ShaderLocation: 1},
				},
			}},
		},
		Fragment: &wgpu.FragmentState{
			Module: shader, EntryPoint: "fs_main",
			Targets: []gputypes.ColorTargetState{{
				Format: gputypes.TextureFormatRGBA8Unorm, WriteMask: gputypes.ColorWriteMaskAll,
			}},
		},
		Primitive: gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList},
	})
	if err != nil {
		return err
	}
	s.pipeline = pl

	sampler, err := s.dev.CreateSampler(&wgpu.SamplerDescriptor{
		MagFilter: gputypes.FilterModeNearest, MinFilter: gputypes.FilterModeNearest,
		AddressModeU: gputypes.AddressModeClampToEdge, AddressModeV: gputypes.AddressModeClampToEdge,
	})
	if err != nil {
		return err
	}
	s.sampler = sampler
	return nil
}

// ensureTargets 按窗口尺寸 (重) 建帧缓冲纹理镜像与合成目标。
// 对应 x11 后端在 resize 时重建 GC/窗口尺寸的同类动作。
func (s *Surface) ensureTargets(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("gpu: 非正尺寸 %dx%d", w, h)
	}
	if s.fbTex != nil && s.fbW == w && s.fbH == h {
		return nil
	}
	// 帧缓冲镜像: CopyDst (CPU 上传) + TextureBinding (着色器采样)
	fb, err := s.dev.CreateTexture(&wgpu.TextureDescriptor{
		Label: "gpu-framebuffer", Dimension: gputypes.TextureDimension2D,
		Size:          wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		MipLevelCount: 1, SampleCount: 1,
		Format: gputypes.TextureFormatRGBA8Unorm,
		Usage:  gputypes.TextureUsageTextureBinding | gputypes.TextureUsageCopyDst,
	})
	if err != nil {
		return err
	}
	view, err := s.dev.CreateTextureView(fb, nil)
	if err != nil {
		return err
	}
	bindGrp, err := s.dev.CreateBindGroup(&wgpu.BindGroupDescriptor{
		Label: "gpu-present-bind", Layout: s.bindLyt,
		Entries: []wgpu.BindGroupEntry{
			{Binding: 0, TextureView: view},
			{Binding: 1, Sampler: s.sampler},
		},
	})
	if err != nil {
		return err
	}
	// 合成目标: 窗口化时由 swapchain 提供; 离屏 (测试/无头) 用这张纹理。
	tgt, err := s.dev.CreateTexture(&wgpu.TextureDescriptor{
		Label: "gpu-present-target", Dimension: gputypes.TextureDimension2D,
		Size:          wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		MipLevelCount: 1, SampleCount: 1,
		Format: gputypes.TextureFormatRGBA8Unorm,
		Usage:  gputypes.TextureUsageRenderAttachment | gputypes.TextureUsageCopySrc,
	})
	if err != nil {
		return err
	}
	// targetView 先建成功再交接线 (CreateTextureView 失败时不能留下
	// "target 已换 / view 还是旧的" 的不一致状态, 否则下一帧会拿旧视图
	// 采样新尺寸的帧缓冲 —— 静默错帧, 比跳过一帧难查得多)。
	tgtView, err := s.dev.CreateTextureView(tgt, nil)
	if err != nil {
		return err
	}
	if s.targetView != nil {
		s.targetView.Release() // 重建 (resize) 时释放旧视图
	}
	s.fbTex, s.fbView, s.bindGrp, s.target, s.targetView = fb, view, bindGrp, tgt, tgtView
	s.fbW, s.fbH = w, h
	return nil
}

// Show 整帧上屏 (gfx.Surface)。
func (s *Surface) Show(img *image.RGBA) { s.ShowRegions(img, nil) }

// ShowRegions 局部上屏 (gfx.Surface): rects 为空 = 全帧。
// 对应 x11 后端逐区域 PutImage 的结构, 这里是逐区域子纹理上传 + scissor 重绘。
func (s *Surface) ShowRegions(img *image.RGBA, rects []image.Rectangle) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w <= 0 || h <= 0 {
		return
	}
	if err := s.ensureTargets(w, h); err != nil {
		return // 尺寸异常时静默跳过本帧 (与平台后端对 closed 的处理同风格)
	}
	if len(rects) == 0 {
		rects = []image.Rectangle{img.Bounds()}
		s.fullFrames++
	} else {
		s.regionFrames++
	}
	s.frames++
	// 1. 帧缓冲像素 → GPU 纹理 (全帧整幅 / 局部帧逐区子上传)
	for _, r := range rects {
		r = r.Intersect(img.Bounds())
		if r.Empty() {
			continue
		}
		rowBytes := r.Dx() * 4
		need := rowBytes * r.Dy()
		if cap(s.uploadScratch) < need {
			s.uploadScratch = make([]byte, need) // 只增不减: 按最大脏区一次到位
		}
		sub := s.uploadScratch[:need]
		stride := img.Stride // image.RGBA 可能有对齐 padding
		for y := 0; y < r.Dy(); y++ {
			off := (r.Min.Y+y)*stride + r.Min.X*4
			copy(sub[y*rowBytes:], img.Pix[off:off+rowBytes])
		}
		if err := s.dev.Queue().WriteTexture(
			&wgpu.ImageCopyTexture{Texture: s.fbTex, Origin: wgpu.Origin3D{X: uint32(r.Min.X), Y: uint32(r.Min.Y)}},
			sub,
			&wgpu.ImageDataLayout{BytesPerRow: uint32(rowBytes), RowsPerImage: uint32(r.Dy())},
			&wgpu.Extent3D{Width: uint32(r.Dx()), Height: uint32(r.Dy()), DepthOrArrayLayers: 1},
		); err != nil {
			return
		}
	}
	// 2. 选渲染终点与呈现方式:
	//    - 窗口化且 swapchain 就绪 → acquire 交换链纹理, 收尾 Present 到
	//      原生窗口 (真上屏, A 路径硬前提 #2 的落地路径);
	//    - 否则 → 离屏 target 纹理 (测试 / 无头 / swapchain 失效降级)。
	renderView := s.targetView
	renderPl := s.pipeline
	present := func() {}
	if s.wgpuSurf != nil && !s.swapBroken {
		st, view, ok := s.acquireSwap(w, h)
		if !ok {
			// swapchain 运行期失效 (窗口销毁 / 驱动重置 / resize 竞态):
			// 永久置 broken 并把**这一帧**交回内层 CPU blit —— 降级而非
			// 失败, 窗口不许因为 GPU 疯掉而停更。
			s.swapBroken = true
			if s.inner != nil {
				s.inner.ShowRegions(img, rects)
			}
			return
		}
		defer view.Release()
		renderView = view
		pl, perr := s.pipelineFor(s.swapFormat)
		if perr != nil {
			s.swapBroken = true
			if s.inner != nil {
				s.inner.ShowRegions(img, rects)
			}
			return
		}
		renderPl = pl
		present = func() { _ = s.wgpuSurf.Present(st) }
	}
	// 合成: Load (保留目标旧内容) + 逐脏区 scissor 重绘 —— 只碰变化区域
	enc, err := s.dev.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: "gfx-frame"})
	if err != nil {
		return
	}
	pass, err := enc.BeginRenderPass(&wgpu.RenderPassDescriptor{
		ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: renderView, LoadOp: gputypes.LoadOpLoad, StoreOp: gputypes.StoreOpStore,
		}},
	})
	if err != nil {
		return
	}
	pass.SetPipeline(renderPl)
	pass.SetBindGroup(0, s.bindGrp, nil)
	pass.SetVertexBuffer(0, s.vbuf, 0)
	for _, r := range rects {
		r = r.Intersect(img.Bounds())
		if r.Empty() {
			continue
		}
		pass.SetScissorRect(gputypes.ScissorRect{
			X: uint32(r.Min.X), Y: uint32(r.Min.Y),
			Width: uint32(r.Dx()), Height: uint32(r.Dy()),
		})
		pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
		s.uploadedDrawn++
	}
	if err := pass.End(); err != nil {
		return
	}
	cmds, err := enc.Finish()
	if err != nil {
		return
	}
	_, _ = s.dev.Queue().Submit(cmds)
	// 窗口化路径: Present 交换链纹理上屏 (代替离屏 readback); 离屏路径
	// present 是空操作, 结果经 Readback() 验证。
	present()
}

// acquireSwap 取一帧交换链纹理及其视图; 尺寸变了先重配 swapchain。
// 任何一步失败返回 ok=false —— 调用方置 swapBroken 退回 CPU blit。
func (s *Surface) acquireSwap(w, h int) (*wgpu.SurfaceTexture, *wgpu.TextureView, bool) {
	if s.swapW != w || s.swapH != h {
		s.wgpuSurf.Unconfigure()
		if err := s.wgpuSurf.Configure(s.dev, &wgpu.SurfaceConfiguration{
			Width: uint32(w), Height: uint32(h),
			Format:      s.swapFormat,
			Usage:       gputypes.TextureUsageRenderAttachment,
			PresentMode: gputypes.PresentModeFifo,
		}); err != nil {
			return nil, nil, false
		}
		s.swapW, s.swapH = w, h
	}
	st, _, err := s.wgpuSurf.GetCurrentTexture()
	if err != nil {
		return nil, nil, false
	}
	view, err := st.CreateView(nil)
	if err != nil {
		return nil, nil, false
	}
	return st, view, true
}

// Stats 读取帧统计快照 (gx/dev 调试面板 / 测试断言用)。
func (s *Surface) Stats() Stats {
	return Stats{
		Frames: s.frames, FullFrames: s.fullFrames,
		RegionFrames: s.regionFrames, UploadedDrawn: s.uploadedDrawn,
	}
}

// Readback 把合成结果读回 CPU —— 测试与无头验证用 (真实上屏路径不需要)。
func (s *Surface) Readback() (*image.RGBA, error) {
	n := s.fbW * s.fbH * 4
	buf, err := s.dev.CreateBuffer(&wgpu.BufferDescriptor{
		Label: "gpu-readback", Size: uint64(n),
		Usage: gputypes.BufferUsageCopyDst | gputypes.BufferUsageMapRead,
	})
	if err != nil {
		return nil, err
	}
	defer buf.Release()
	enc, err := s.dev.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{})
	if err != nil {
		return nil, err
	}
	enc.CopyTextureToBuffer(s.target, buf, []wgpu.BufferTextureCopy{{
		BufferLayout: wgpu.ImageDataLayout{BytesPerRow: uint32(s.fbW * 4), RowsPerImage: uint32(s.fbH)},
		Size:         wgpu.Extent3D{Width: uint32(s.fbW), Height: uint32(s.fbH), DepthOrArrayLayers: 1},
	}})
	cmds, err := enc.Finish()
	if err != nil {
		return nil, err
	}
	if _, err := s.dev.Queue().Submit(cmds); err != nil {
		return nil, err
	}
	pending, err := buf.MapAsync(wgpu.MapModeRead, 0, buf.Size())
	if err != nil {
		return nil, err
	}
	s.dev.Poll(wgpu.PollWait) // 软件后端映射 resolve 需显式驱动
	if err := pending.Wait(context.Background()); err != nil {
		return nil, err
	}
	mr, err := buf.MappedRange(0, buf.Size())
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, s.fbW, s.fbH))
	copy(img.Pix, mr.Bytes())
	_ = buf.Unmap()
	return img, nil
}

// Size 委托内层 (离屏模式返回建处时的尺寸)。
func (s *Surface) Size() (int, int) {
	if s.inner != nil {
		return s.inner.Size()
	}
	return s.width, s.height
}

// WaitEvents / Events 委托内层平台 Surface (GPU 化不改事件模型)。
// 离屏模式 (inner == nil): 无事件源, 返回 false = 事件源关闭。
func (s *Surface) WaitEvents(maxWait time.Duration) bool {
	if s.inner == nil {
		return false
	}
	return s.inner.WaitEvents(maxWait)
}

// Events 委托内层平台 Surface; 离屏模式返回 nil (永远不会有事件)。
func (s *Surface) Events() <-chan gfx.Event {
	if s.inner == nil {
		return nil
	}
	return s.inner.Events()
}
