//go:build gpu && linux

package gpu

// Swapchain 端到端 X11 点亮测试 (A 路径硬前提 #2 的**接线级**验证)。
//
// ## 这块验证缺口从哪来
//
// G-1 三轨核销 (docs/gpu-track-audit.md) 一直有一条「swapchain 真机点亮
// 待做」——因为此前所有验证都停在两层: gate 语义用伪句柄钉 (fakeNativeSurface,
// 建链必失败走降级), 离屏像素用 Readback 钉 (TestGPUPresentPixelExact)。
// 中间那层——**真平台窗口 + 真 swapchain 建链 + Present 后像素真的落到
// 窗口上**——无头沙箱验不到, 被整体划进了「真机」。
//
// 实际上它不该整块划过去: wgpu 软件后端在 Linux 上原生支持 Xlib window
// target (hal/software/blit_linux.go, XPutImage 经 goffi dlopen libX11),
// 也就是说**有 X server 就能真点亮**, 只不过走的是软件适配器。本测试把这
// 层接缝在 Xvfb 下钉住: 真窗口 → gpu.New 建链 → Show → XGetImage 读回
// **窗口上**的像素逐点比对。
//
// ## 口径纪律 (与 gpu-track-audit.md 同源)
//
// 本测试证明的是**接线正确性** (建链不降级 + 像素逐点一致), 不是性能:
// 适配器是软件的, 性能口径仍归硬件闸门 (bench-present-4k.sh 硬件行),
// 本测试不产生任何可对外引用的帧率/耗时数字。真机三平台点亮 (win32/
// cocoa 侧, Metal/HWND 句柄形态) 仍属 G-3 硬件覆盖项 —— 本测试只把
// x11 这一行从「纯真机待办」里拆出来。
//
// ## 跳过语义 (G-4 同款纪律)
//
// 无 DISPLAY (无 X server) / 无窗口工厂 / 无适配器 → skip, 不是 pass:
// CI 无 X 环境时本测试不存在, 有 Xvfb 的 job 才算数。

import (
	"image"
	"image/color"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/14752222/Gox/gfx"
	_ "github.com/14752222/Gox/gfx/x11" // 注册默认窗口工厂 (init 副作用)
	"github.com/go-webgpu/goffi/ffi"
	"github.com/go-webgpu/goffi/types"
)

// xImageReadback 是 XGetImage 返回的 XImage 结构里测试关心的字段子集,
// 布局对齐 amd64 Linux (LP64) 的 Xlib.h _XImage —— 与 wgpu hal/software
// blit_linux.go 的 ximage 同源 (那边还要 funcs 子结构, 读回侧用不到)。
type xImageReadback struct {
	width          int32          // 0
	height         int32          // 4
	xoffset        int32          // 8
	format         int32          // 12
	data           unsafe.Pointer // 16 (char*) —— 与 uintptr 同布局 (LP64)
	byteOrder      int32          // 24
	bitmapUnit     int32          // 28
	bitmapBitOrder int32          // 32
	bitmapPad      int32          // 36
	depth          int32          // 40
	bytesPerLine   int32          // 44
	bitsPerPixel   int32          // 48
	_pad0          int32          // 52 (对齐 unsigned long)
	redMask        uint64         // 56 (unsigned long on LP64)
	greenMask      uint64         // 64
	blueMask       uint64         // 72
	obdata         uintptr        // 80
}

const (
	ximgZPixmap = 2 // ZPixmap (Xlib.h)
)

// x11TestConn 是测试自己的 Xlib 连接 (与 wgpu blit / xgb 的连接互相独立;
// X 资源 id 服务端全局, 跨客户端引用同一窗口合法)。lib 是 dlopen 库句柄
// (GetSymbol 用它, **不是** Display* —— 两者都是指针但语义无关)。
type x11TestConn struct {
	lib  unsafe.Pointer
	disp uintptr
}

// open 打开 Xlib 显示连接; 失败返回 false (调用方 skip)。
func (c *x11TestConn) open() bool {
	h, err := ffi.LoadLibrary("libX11.so.6")
	if err != nil || h == nil {
		return false
	}
	c.lib = h
	var name unsafe.Pointer // NULL → 读 DISPLAY
	openD := bindX11Fn(h, "XOpenDisplay", types.PointerTypeDescriptor,
		[]*types.TypeDescriptor{types.PointerTypeDescriptor})
	if openD == nil {
		return false
	}
	_, err = ffi.CallFunction(&openD.cif, openD.fn,
		unsafe.Pointer(&c.disp), []unsafe.Pointer{unsafe.Pointer(&name)})
	return err == nil && c.disp != 0
}

// bindX11Fn 与 gfx/x11 的 xlibBind 同款 (测试包不能 import 未导出符号, 照抄)。
func bindX11Fn(h unsafe.Pointer, name string, ret *types.TypeDescriptor,
	args []*types.TypeDescriptor) *struct {
	cif types.CallInterface
	fn  unsafe.Pointer
} {
	sym, err := ffi.GetSymbol(h, name)
	if err != nil || sym == nil {
		return nil
	}
	var cif types.CallInterface
	if err := ffi.PrepareCallInterface(&cif, types.DefaultCall, ret, args); err != nil {
		return nil
	}
	return &struct {
		cif types.CallInterface
		fn  unsafe.Pointer
	}{cif: cif, fn: sym}
}

// windowPixels 用 XGetImage 把窗口 (0,0,w,h) 的像素读回成 RGBA。
// 软件后端 blit 落到窗口的是 BGRA (RGBA→BGRA swizzle + ZPixmap), 这里
// 按 XImage 自带的 masks 解码, 不硬编码字节序。
func windowPixels(lib unsafe.Pointer, disp, win uintptr, w, h int) (*image.RGBA, bool) {
	// 注意: GetSymbol 的第一个参数是 **dlopen 库句柄**, 不是 Display*。
	getImage := bindX11Fn(lib, "XGetImage", types.PointerTypeDescriptor,
		[]*types.TypeDescriptor{
			types.PointerTypeDescriptor, // Display*
			types.PointerTypeDescriptor, // Drawable —— 64 位整型实参按 wgpu blit_linux 同款走 8 字节存储
			types.SInt32TypeDescriptor,  // src_x
			types.SInt32TypeDescriptor,  // src_y
			types.UInt32TypeDescriptor,  // width
			types.UInt32TypeDescriptor,  // height
			types.PointerTypeDescriptor, // plane_mask (unsigned long)
			types.SInt32TypeDescriptor,  // format
		})
	if getImage == nil {
		return nil, false
	}
	var ximg unsafe.Pointer
	d := uintptr(win)
	pl := uintptr(0xFFFFFFFF)
	sx, sy := int32(0), int32(0)
	uw, uh := uint32(w), uint32(h)
	fmt := int32(ximgZPixmap)
	args := []unsafe.Pointer{
		unsafe.Pointer(&disp), unsafe.Pointer(&d),
		unsafe.Pointer(&sx), unsafe.Pointer(&sy),
		unsafe.Pointer(&uw), unsafe.Pointer(&uh),
		unsafe.Pointer(&pl), unsafe.Pointer(&fmt),
	}
	if _, err := ffi.CallFunction(&getImage.cif, getImage.fn,
		unsafe.Pointer(&ximg), args); err != nil || ximg == nil {
		return nil, false
	}
	xi := (*xImageReadback)(ximg)
	if xi.data == nil || xi.bytesPerLine <= 0 {
		return nil, false
	}
	out := image.NewRGBA(image.Rect(0, 0, int(xi.width), int(xi.height)))
	bpp := int(xi.bitsPerPixel) / 8
	if bpp < 3 {
		return nil, false
	}
	line := int(xi.bytesPerLine)
	shifts := func(m uint64) (uint8, uint8) { // (掩码有效位宽, 尾零数)
		s := uint8(0)
		for m != 0 && m&1 == 0 {
			m >>= 1
			s++
		}
		width := uint8(0)
		for m != 0 {
			m >>= 1
			width++
		}
		return width, s
	}
	rw, rs := shifts(xi.redMask)
	gw, gs := shifts(xi.greenMask)
	bw, bs := shifts(xi.blueMask)
	scale := func(v uint64, bits uint8) uint8 {
		if bits == 0 {
			return 0
		}
		max := (uint64(1) << bits) - 1
		return uint8(v * 0xFF / max)
	}
	for y := 0; y < int(xi.height); y++ {
		row := (*[1 << 24]byte)(unsafe.Add(xi.data, uintptr(y*line)))[:uw*uint32(bpp)]
		for x := 0; x < int(xi.width); x++ {
			var px uint64
			for i := 0; i < bpp; i++ {
				px |= uint64(row[x*bpp+i]) << (8 * i) // little-endian LSBFirst
			}
			out.SetRGBA(x, y, color.RGBA{
				R: scale((px&xi.redMask)>>rs, rw),
				G: scale((px&xi.greenMask)>>gs, gw),
				B: scale((px&xi.blueMask)>>bs, bw),
				A: 0xFF,
			})
		}
	}
	return out, true
}

// TestSwapchainX11WindowE2E —— x11 swapchain 真点亮 + 窗口像素逐点验证。
func TestSwapchainX11WindowE2E(t *testing.T) {
	// 进程隔离闸门 (2026-11-19 实测钉住的硬约束): 本测试与其它 gfx/gpu
	// 测试**同进程运行**时, 无论走到哪一段 (哪怕只 goffi LoadLibrary
	// libX11 + 一次失败的 XOpenDisplay), 后续测试必触发 wgpu-software
	// 段错误 (crash 在 cgo/FFI 层, 无 Go 栈; 单独进程连跑 5 次全绿)。
	// 属上游 goffi/wgpu 交互缺陷, 本仓不可修 —— CI 与本地都用独立进程
	// 专跑本测试 (见 gpu-present.yml e2e step), 其余套件进程里恒 skip。
	if os.Getenv("GOX_GPU_X11_E2E") != "1" {
		t.Skip("专用进程测试: 需 GOX_GPU_X11_E2E=1 (独立进程, 不与其它测试同跑)")
	}
	if os.Getenv("DISPLAY") == "" {
		t.Skip("无 X server (DISPLAY 未设) —— 需 Xvfb 环境 (xvfb-run) 或真机")
	}
	if !Available() {
		t.Skip("GPU 呈现路径不可用 (无适配器)")
	}
	// X server 可达性是**环境** (DISPLAY 可能指向死掉的服务器), 探测不到
	// 就 skip; 但可达之后建窗失败是**后端回归**, 必须 Fatal —— 否则 x11
	// 轨建窗逻辑坏了会静默 skip, 闸门失明。
	conn := &x11TestConn{}
	if !conn.open() {
		t.Skip("X server 不可达 / 无 libX11 —— 需 Xvfb (xvfb-run) 或真机")
	}
	f := gfx.DefaultFactory()
	if f == nil {
		t.Skip("无窗口工厂 (gfx/x11 未注册)")
	}

	const wW, wH = 256, 192
	inner, err := f.Create(gfx.WindowConfig{Title: "gox-gpu-swapchain-e2e", Width: wW, Height: wH})
	if err != nil {
		t.Fatalf("建真窗口失败 (X server 可达): %v", err)
	}

	// 1. 建链: 真平台窗口 + 软件适配器 (Xvfb) 应成功, 不许静默降级。
	s, err := New(inner)
	if err != nil {
		t.Fatalf("gpu.New 失败: %v", err)
	}
	if !s.PresentsToWindow() {
		t.Fatalf("swapchain 未建链 (PresentsToWindow=false) —— x11 接线断了: " +
			"软件适配器支持 Xlib window target, 有 X server 就不该降级")
	}

	// 2. 一帧四象限纯色 (四象限纯色帧, 象限中心采样, 避开任何边缘效应)。
	w, h := inner.Size()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	midX, midY := w/2, h/2
	quads := []struct {
		rect  image.Rectangle
		color color.RGBA
	}{
		{image.Rect(0, 0, midX, midY), color.RGBA{R: 0xFF, A: 0xFF}},                   // 左上 红
		{image.Rect(midX, 0, w, midY), color.RGBA{G: 0xFF, A: 0xFF}},                   // 右上 绿
		{image.Rect(0, midY, midX, h), color.RGBA{B: 0xFF, A: 0xFF}},                   // 左下 蓝
		{image.Rect(midX, midY, w, h), color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}}, // 右下 白
	}
	for _, q := range quads {
		fillRect(img, q.rect, q.color)
	}

	// 3. GPU 合成 + Present 到原生窗口。
	s.Show(img)

	// 4. 从窗口读回像素逐点比对 (软件后端 Present 同步 XPutImage; 保守起见
	//    留一小段重试窗, 防异步时序 —— 全绿则一次即过)。连接已建 (第 0 步)。
	// 从平台 inner 拿真窗口句柄 (与建链用的同一个): 走 NativeWindowProvider。
	type nwp interface {
		NativeWindowTarget() (gfx.NativeWindowTarget, bool)
	}
	prov, ok := inner.(nwp)
	if !ok {
		t.Fatalf("平台 surface 未实现 NativeWindowProvider —— 接线回归")
	}
	tgt, ok := prov.NativeWindowTarget()
	if !ok || tgt.Display == 0 || tgt.Window == 0 {
		t.Fatalf("平台交不出句柄: tgt=%+v ok=%v", tgt, ok)
	}

	points := []struct {
		pt   image.Point
		want color.RGBA
		name string
	}{
		{image.Pt(midX/2, midY/2), color.RGBA{R: 0xFF, A: 0xFF}, "左上/红"},
		{image.Pt(midX+midX/2, midY/2), color.RGBA{G: 0xFF, A: 0xFF}, "右上/绿"},
		{image.Pt(midX/2, midY+midY/2), color.RGBA{B: 0xFF, A: 0xFF}, "左下/蓝"},
		{image.Pt(midX+midX/2, midY+midY/2), color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}, "右下/白"},
	}
	var got *image.RGBA
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ok bool
		got, ok = windowPixels(conn.lib, tgt.Display, tgt.Window, w, h)
		if ok && snapshotMatches(got, quads) {
			break // 像素已落窗且比对通过
		}
		if time.Now().After(deadline) {
			if got != nil {
				for _, p := range points {
					t.Logf("采样 @%s (%d,%d): 窗口=%v 期望=%v", p.name, p.pt.X, p.pt.Y,
						got.RGBAAt(p.pt.X, p.pt.Y), p.want)
				}
			}
			t.Fatalf("窗口像素读回/比对超时 (2s): ok=%v, got=%v", ok, got != nil)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range points {
		c := got.RGBAAt(p.pt.X, p.pt.Y)
		if c != p.want {
			t.Fatalf("窗口像素不一致 @%s (%d,%d): 窗口上=%v 期望=%v (整帧见失败输出)",
				p.name, p.pt.X, p.pt.Y, c, p.want)
		}
	}
	// 抽查全帧: 四象限边界内侧逐行扫 32 个点, 防止「采样点恰好对、整帧错」。
	for _, q := range quads {
		cx, cy := (q.rect.Min.X+q.rect.Max.X)/2, (q.rect.Min.Y+q.rect.Max.Y)/2
		for _, off := range []int{-32, -16, 0, 16, 32} {
			x, y := cx+off, cy
			if !image.Pt(x, y).In(q.rect) {
				continue
			}
			if c := got.RGBAAt(x, y); c != q.color {
				t.Fatalf("整帧抽查不一致 @(%d,%d) [象限中心 (%d,%d)]: 窗口上=%v 期望=%v",
					x, y, cx, cy, c, q.color)
			}
		}
	}

	// 5. 呈现后仍在窗口化模式 (不许这一帧触发降级)。
	if !s.PresentsToWindow() {
		t.Fatalf("Show 后 PresentsToWindow=false —— present 过程发生 swapchain 降级")
	}
}

// fillRect 纯色填充 (不引 image/draw 的遮罩语义, 颜色逐字节确定)。
func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

// snapshotMatches 全帧比对: 四象限逐像素相等 (软件路径无抗锯齿/亚像素,
// 纯色上传 → 全帧逐点一致是唯一正确口径)。
func snapshotMatches(got *image.RGBA, quads []struct {
	rect  image.Rectangle
	color color.RGBA
}) bool {
	for _, q := range quads {
		for y := q.rect.Min.Y; y < q.rect.Max.Y; y++ {
			for x := q.rect.Min.X; x < q.rect.Max.X; x++ {
				if got.RGBAAt(x, y) != q.color {
					return false
				}
			}
		}
	}
	return true
}
