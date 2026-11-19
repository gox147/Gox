//go:build gpu

package gfx

// G-2 探针 · 第三问: GPU 光栅化与 CPU 光栅化的视觉一致性 —— 矩形纯色阶段
// （G-3 第一阶段）的像素级 A/B。
//
// 探针结论驱动的设计选择: Prism 全部组件走**圆角 + roundCov SDF 覆盖率
// 抗锯齿**（decor.go fillRoundRect: 中段 FillRect 直写 + 角带逐像素
// blendPixel），普查显示角带散点占稳定态单帧调用数 99%+。因此 GPU 第一
// 阶段如果只做"纯色 quad"，26 组件快照必然在圆角上翻车 —— 必须**在
// fragment shader 里复刻 roundCov 的同一个盒形 SDF 公式**。
//
// 本探针把这件事最小化实现一遍（每 quad 携带 rect/color/rad 顶点属性,
// **单 draw call 合批**），与 CPU fillRoundRect 逐像素对拍, 回答:
//   1. SDF 公式在 f32 下与 CPU f64 的收敛度（逐通道最大差 / 精确率）;
//   2. 合批（一次 Draw 提交全部 quad）是否改变结果;
//   3. 边缘 case（rad=0 / 1px 线 / 半透明 / 越界矩形）是否同口径。
//
// 口径: 无 GPU 硬件沙箱上跑的是 wgpu **software 后端** —— 它对"像素正确性"
// 是可信口径（软件光栅化实现同一 WebGPU 规范语义, 合批/混合/SDF 的正确性
// 结论可迁移), 但**性能数字不进闸门**; 硬件复测待 gpu-hardware runner。
//
// 运行: go test -tags gpu ./gfx/ -run TestProbeGPURoundRectPixelAB -v

import (
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"os"
	"testing"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	_ "github.com/gogpu/wgpu/hal/allbackends"
)

// probeRect 是对拍用例: CPU 侧 fillRoundRect 与 GPU 侧 SDF quad 同参数。
type probeRect struct {
	r   Rect
	rad int
	c   color.RGBA
}

const (
	probeW, probeH = 320, 240
	probeBG        = 0xf1f5f9 // slate-100, 与 Prism 常用底色同族
)

// probeCases 覆盖: 大圆角/全圆角(rad=H/2)/小圆角/rad=0 纯 quad/1px 线/
// 半透明/越界矩形。
func probeCases() []probeRect {
	return []probeRect{
		{Rect{20, 20, 280, 60}, 12, color.RGBA{0x3b, 0x82, 0xf6, 0xff}},
		{Rect{20, 100, 130, 50}, 25, color.RGBA{0x16, 0xa3, 0x4a, 0xff}},
		{Rect{170, 100, 130, 50}, 8, color.RGBA{0xf5, 0x9e, 0x0b, 0xff}},
		{Rect{20, 170, 40, 40}, 0, color.RGBA{0xef, 0x44, 0x44, 0xff}},    // rad=0: 纯 quad
		{Rect{80, 170, 1, 40}, 0, color.RGBA{0x0f, 0x17, 0x2a, 0xff}},     // 1px 竖线
		{Rect{100, 170, 60, 1}, 0, color.RGBA{0x0f, 0x17, 0x2a, 0xff}},    // 1px 横线
		{Rect{200, 170, 100, 50}, 40, color.RGBA{0x8b, 0x5c, 0xf6, 0xff}}, // rad>H/2: 钳位
		{Rect{80, 220, 120, 40}, 10, color.RGBA{0x64, 0x74, 0x8b, 0xc8}},  // 半透明
		{Rect{290, 200, 60, 60}, 10, color.RGBA{0x0e, 0xa5, 0xe9, 0xff}},  // 右/下越界
	}
}

const probeWGSL = `
struct VOut {
	@builtin(position) pos: vec4f,
	@location(0) rect: vec4f,
	@location(1) color: vec4f,
	@location(2) rad: f32,
}

@vertex
fn vs(@location(0) xy: vec2f, @location(1) rect: vec4f,
      @location(2) color: vec4f, @location(3) rad: f32) -> VOut {
	var o: VOut;
	o.pos = vec4f(xy, 0.0, 1.0);
	o.rect = rect;
	o.color = color;
	o.rad = rad;
	return o;
}

// fs 与 decor.go roundCov 同一个盒形 SDF 公式（像素中心 = pos.xy, y 向下
// 与 image 坐标同向）。覆盖率 = clamp(0.5 - d, 0, 1)。
@fragment
fn fs(v: VOut) -> @location(0) vec4f {
	let halfW = v.rect.z / 2.0;
	let halfH = v.rect.w / 2.0;
	let c = v.rect.xy + vec2f(halfW, halfH);
	let q = abs(v.pos.xy - c) - (vec2f(halfW, halfH) - vec2f(v.rad));
	let qo = max(q, vec2f(0.0));
	let m = max(q.x, q.y);
	let d = length(qo) + min(m, 0.0) - v.rad;
	let cov = clamp(0.5 - d, 0.0, 1.0);
	let a = v.color.a * cov;
	// 预乘输出 + src*One + dst*(1-srcAlpha) = CPU 的 src-over 口径
	return vec4f(v.color.rgb * a, a);
}
`

// probeGPUDevice 与 gfx/gpu.Device 同构的最小 device 建立（gfx 包内测试
// 不能 import gfx/gpu —— 它反过来 import gfx, 会成环）。返回 (device, 口径)。
//
// 【2026-11-20 段错误根因与门控】本函数在**共享的 gfx 测试进程内**执行
// wgpu.CreateInstance + RequestAdapter —— RequestAdapter 会按 allbackends
// 注册顺序逐后端枚举, 在装有 libvulkan.so.1 的机器上会经 goffi(FFI)
// dlopen **真 Vulkan loader** 并枚举适配器（如 lavapipe）, loader 自建
// 原生线程。实测组合后果: 全量 `go test -tags gpu ./gfx/` 在探针之后
// ~150 个用例处延迟 SIGSEGV, 崩溃栈 gdb 取证为 **cgo trampoline
// (runtime/cgo crosscall1) 执行堆地址**(无 Go 运行时栈, GOTRACEBACK=crash
// 亦无输出), -race 零 DATA RACE 报告; 对照实验: 无 -tags gpu 全量 PASS、
// 前缀剔除本探针 PASS、GOMAXPROCS=2 仍崩（非调度规模问题）。
// ⇒ 探针存在价值在「硬件复测/专项运行」, 不在全量跑批的进程卫生。改为
// 显式 opt-in: GOX_GPU_PROBE=1 才运行; gpu-hardware runner 的 job 里
// 显式带上该变量。
func probeGPUDevice(t *testing.T) (*wgpu.Device, string) {
	t.Helper()
	inst, err := wgpu.CreateInstance(nil)
	if err != nil {
		t.Skipf("wgpu instance: %v", err)
	}
	ad, err := inst.RequestAdapter(nil)
	if err != nil {
		ad, err = inst.RequestAdapter(&wgpu.RequestAdapterOptions{ForceFallbackAdapter: true})
		if err != nil {
			t.Skipf("无可用适配器: %v", err)
		}
	}
	info := ad.Info()
	if info.DeviceType == gputypes.DeviceTypeCPU {
		// 2026-11-17 探针实测: software 后端**不能**作 B 路径正确性参照 ——
		// 它不是忠实的光栅化器, 而是特化路径 + SPIR-V 解释器的组合:
		//   a. 全屏 quad 走 tryFullscreenBlit memcpy 快路径 (无纹理绑定时输出垃圾);
		//   b. FS 无 @location 输入 → 根本不执行 shader, resolveFragmentColor
		//      兜底 (uniform 或白 {1,1,1,1});
		//   c. FS 有 varying → 解释器执行, 但顶点属性搬运在本探针布局下结果
		//      错误 (纯红 varying 实测输出 {0,1,152})。
		// 现有 TestGPUPresentPixelExact 恰好落在「快路径 + 简单纹理采样」这个
		// 被特化的交集里, 所以它绿 —— A 路径 (上屏合成) 在 software 后端的
		// 像素 A/B 仍可信; B 路径 (任意片元逻辑) 不行。
		// ⇒ 本探针在 software 适配器上跳过, 硬件复测由 gpu-hardware runner 承接。
		t.Skip("software 适配器 (兜底口径): 片元执行路径被特化/解释, SDF 像素 A/B 不可信, 待硬件复测")
	}
	kind := "hardware"
	dev, err := ad.RequestDevice(nil)
	if err != nil {
		t.Skipf("wgpu device: %v", err)
	}
	return dev, kind
}

// buildProbeVerts 把全部用例打包成**一个**顶点缓冲（合批口径: 一次 Draw
// 提交所有 quad）。顶点: xy(NDC) + rect(px) + color(unorm8x4) + rad。
func buildProbeVerts(cases []probeRect) ([]byte, uint32) {
	const stride = 44 // (2+4+1)*4 + 4
	buf := make([]byte, 0, 6*stride*len(cases))
	addF32 := func(v float32) {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
	}
	for _, pc := range cases {
		r := pc.r
		l := 2*float32(r.X)/probeW - 1
		rt := 2*float32(r.X+r.W)/probeW - 1
		tp := 1 - 2*float32(r.Y)/probeH
		bm := 1 - 2*float32(r.Y+r.H)/probeH
		rect := [4]float32{float32(r.X), float32(r.Y), float32(r.W), float32(r.H)}
		quad := [6][2]float32{{l, tp}, {rt, tp}, {l, bm}, {l, bm}, {rt, tp}, {rt, bm}}
		for _, xy := range quad {
			addF32(xy[0])
			addF32(xy[1])
			for _, v := range rect {
				addF32(v)
			}
			buf = append(buf, pc.c.R, pc.c.G, pc.c.B, pc.c.A)
			addF32(minF(float32(pc.rad), minF(float32(r.W)/2, float32(r.H)/2)))
		}
	}
	return buf, uint32(len(cases) * 6)
}

func minF(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

// renderProbeOnGPU 把全部用例在**单 draw call** 里提交并回读为 RGBA 帧
// (渲染 + 回读两段 encoder; 口径: 传入 dev 的适配器种类由调用方报告)。
func renderProbeOnGPU(t *testing.T, dev *wgpu.Device, cases []probeRect) *image.RGBA {
	t.Helper()
	shader, err := dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{Label: "probe-sdf", WGSL: probeWGSL})
	if err != nil {
		t.Fatalf("shader: %v", err)
	}
	vbufData, vertCount := buildProbeVerts(cases)
	vbuf, err := dev.CreateBuffer(&wgpu.BufferDescriptor{
		Label: "probe-quads", Size: uint64(len(vbufData)),
		Usage: gputypes.BufferUsageVertex | gputypes.BufferUsageCopyDst,
	})
	if err != nil {
		t.Fatalf("vbuf: %v", err)
	}
	if err := dev.Queue().WriteBuffer(vbuf, 0, vbufData); err != nil {
		t.Fatalf("write vbuf: %v", err)
	}
	tex, err := dev.CreateTexture(&wgpu.TextureDescriptor{
		Label: "probe-target", Dimension: gputypes.TextureDimension2D,
		Size:          wgpu.Extent3D{Width: probeW, Height: probeH, DepthOrArrayLayers: 1},
		MipLevelCount: 1, SampleCount: 1,
		Format: gputypes.TextureFormatRGBA8Unorm,
		Usage:  gputypes.TextureUsageRenderAttachment | gputypes.TextureUsageCopySrc,
	})
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	view, err := dev.CreateTextureView(tex, nil)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	layout, err := dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{
		Label: "probe-layout", BindGroupLayouts: nil, // 无绑定: SDF 参数全走顶点属性
	})
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	one := gputypes.BlendFactorOne
	omsa := gputypes.BlendFactorOneMinusSrcAlpha
	pl, err := dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
		Label:  "probe-sdf-pipeline",
		Layout: layout,
		Vertex: wgpu.VertexState{
			Module: shader, EntryPoint: "vs",
			Buffers: []gputypes.VertexBufferLayout{{
				ArrayStride: 44, StepMode: gputypes.VertexStepModeVertex,
				Attributes: []gputypes.VertexAttribute{
					{Format: gputypes.VertexFormatFloat32x2, Offset: 0, ShaderLocation: 0},
					{Format: gputypes.VertexFormatFloat32x4, Offset: 8, ShaderLocation: 1},
					{Format: gputypes.VertexFormatUnorm8x4, Offset: 24, ShaderLocation: 2},
					{Format: gputypes.VertexFormatFloat32, Offset: 28, ShaderLocation: 3},
				},
			}},
		},
		Fragment: &wgpu.FragmentState{
			Module: shader, EntryPoint: "fs",
			Targets: []gputypes.ColorTargetState{{
				Format: gputypes.TextureFormatRGBA8Unorm,
				Blend: &gputypes.BlendState{
					Color: gputypes.BlendComponent{SrcFactor: one, DstFactor: omsa},
					Alpha: gputypes.BlendComponent{SrcFactor: one, DstFactor: omsa},
				},
			}},
		},
		Primitive: gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList},
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	enc, err := dev.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: "probe-enc"})
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	pass, err := enc.BeginRenderPass(&wgpu.RenderPassDescriptor{
		ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: view, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
			ClearValue: gputypes.Color{
				R: float64(0xf1) / 255.0, G: float64(0xf5) / 255.0,
				B: float64(0xf9) / 255.0, A: 1,
			},
		}},
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	pass.SetPipeline(pl)
	pass.SetVertexBuffer(0, vbuf, 0)
	pass.Draw(gputypes.DrawArgs{VertexCount: vertCount, InstanceCount: 1})
	if err := pass.End(); err != nil {
		t.Fatalf("pass end: %v", err)
	}
	cmds, err := enc.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := dev.Queue().Submit(cmds); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return readbackTexture(t, dev, tex)
}

// readbackTexture 把 RGBA8Unorm 纹理整幅读回为 *image.RGBA (探针/测试用)。
// render 使用与 copy 使用不能共一个 pass 链, 回读走独立 encoder —— 与
// gfx/gpu Surface.Readback 同构。
func readbackTexture(t *testing.T, dev *wgpu.Device, tex *wgpu.Texture) *image.RGBA {
	t.Helper()
	rbuf, err := dev.CreateBuffer(&wgpu.BufferDescriptor{
		Label: "probe-readback", Size: uint64(probeW * probeH * 4),
		Usage: gputypes.BufferUsageCopyDst | gputypes.BufferUsageMapRead,
	})
	if err != nil {
		t.Fatalf("readback buf: %v", err)
	}
	defer rbuf.Release()
	renc, err := dev.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: "probe-readback"})
	if err != nil {
		t.Fatalf("readback encoder: %v", err)
	}
	renc.CopyTextureToBuffer(tex, rbuf, []wgpu.BufferTextureCopy{{
		BufferLayout: wgpu.ImageDataLayout{BytesPerRow: probeW * 4, RowsPerImage: probeH},
		Size:         wgpu.Extent3D{Width: probeW, Height: probeH, DepthOrArrayLayers: 1},
	}})
	rcmds, err := renc.Finish()
	if err != nil {
		t.Fatalf("readback finish: %v", err)
	}
	if _, err := dev.Queue().Submit(rcmds); err != nil {
		t.Fatalf("readback submit: %v", err)
	}
	// 与 gogpu triangle-headless 示例同口径: Map(ctx) 同步映射 (内部完成
	// queue 同步), 不走 MapAsync+Poll —— 实测该组合在 software 后端拿到
	// 不完整数据 (2026-11-17 探针踩坑记录)。
	if err := rbuf.Map(t.Context(), wgpu.MapModeRead, 0, rbuf.Size()); err != nil {
		t.Fatalf("map: %v", err)
	}
	mr, err := rbuf.MappedRange(0, rbuf.Size())
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, probeW, probeH))
	copy(img.Pix, mr.Bytes())
	_ = rbuf.Unmap()
	return img
}

func TestProbeGPURoundRectPixelAB(t *testing.T) {
	// 显式 opt-in（见 probeGPUDevice 上方门控注释）: 全量跑批默认跳过,
	// 专项探针运行 GOX_GPU_PROBE=1 ./... 打开。
	if os.Getenv("GOX_GPU_PROBE") != "1" {
		t.Skip("GPU 光栅化探针默认不在全量跑批内运行 —— 探针经 goffi 枚举真 Vulkan loader, 会污染共享测试进程（2026-11-20 段错误根因, 见上方注释）; 专项运行: GOX_GPU_PROBE=1 go test -tags gpu ./gfx/ -run TestProbeGPURoundRectPixelAB")
	}
	// GPU device 建立与口径报告
	dev, kind := probeGPUDevice(t)

	// ---- CPU 参照帧: 与被测组件同一绘制原语 ----
	bg := color.RGBA{0xf1, 0xf5, 0xf9, 0xff}
	cpu := image.NewRGBA(image.Rect(0, 0, probeW, probeH))
	FillRect(cpu, Rect{0, 0, probeW, probeH}, bg)
	cases := probeCases()
	for _, pc := range cases {
		fillRoundRect(cpu, pc.r, pc.rad, pc.c)
	}

	// ---- GPU 帧: 单 draw call 合批提交全部 SDF quad ----
	gpuImg := renderProbeOnGPU(t, dev, cases)

	// ---- 逐像素对拍 ----
	exact, maxDiff, band := 0, 0, 0
	var firstDiff image.Point
	total := probeW * probeH
	for y := 0; y < probeH; y++ {
		for x := 0; x < probeW; x++ {
			c1, c2 := cpu.RGBAAt(x, y), gpuImg.RGBAAt(x, y)
			d := max(cdiff(c1.R, c2.R), cdiff(c1.G, c2.G))
			d = max(d, cdiff(c1.B, c2.B))
			if d == 0 {
				exact++
				continue
			}
			if d > maxDiff {
				maxDiff = d
				firstDiff = image.Point{X: x, Y: y}
			}
			if d <= 2 {
				band++
			}
		}
	}
	t.Logf("后端口径: %s; 合批: %d quad 单 draw call", kind, len(cases))
	t.Logf("CPU vs GPU(SDF): 精确一致 %d/%d (%.2f%%), |Δ|≤2 %d, 最大通道差 %d (首个 %v)",
		exact, total, float64(exact)/float64(total)*100, band, maxDiff, firstDiff)
	if maxDiff > 2 {
		t.Fatalf("SDF 覆盖率在 f32 下收敛不足: 最大通道差 %d > 2 (视觉门禁阈值), 首个 %v",
			maxDiff, firstDiff)
	}
}

func cdiff(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}
