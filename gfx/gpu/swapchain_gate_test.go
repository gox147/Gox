//go:build gpu

package gpu

// swapchain 接线探针 (A 路径硬前提 #2, 2026-11-17):
// 验证「窗口化包装点」的降级语义 —— 原生句柄给不出/给不对时,
// New 必须成功 (离屏模式照用) 且 PresentsToWindow()==false,
// 绝不能 panic 或把错误甩给调用方。真上屏 (PresentsToWindow==true)
// 只在有 GPU 硬件 + 真平台窗口的环境出现 (G-1 硬件口径覆盖项),
// 无硬件沙箱**验不到, 也不许拿兜底口径冒充**。

import (
	"image"
	"testing"
	"time"

	"github.com/14752222/Gox/gfx"
)

// fakeNativeSurface 是实现了 NativeWindowProvider 的假平台窗口,
// kind/句柄可注入 —— 探测 initSwapchain 的每道 gate。
type fakeNativeSurface struct {
	tgt gfx.NativeWindowTarget
	ok  bool
}

func (f *fakeNativeSurface) Show(img *image.RGBA) {}

func (f *fakeNativeSurface) ShowRegions(img *image.RGBA, rects []image.Rectangle) {}

func (f *fakeNativeSurface) Size() (int, int) { return 64, 48 }

func (f *fakeNativeSurface) WaitEvents(time.Duration) bool { return false }

func (f *fakeNativeSurface) Events() <-chan gfx.Event { return nil }

func (f *fakeNativeSurface) NativeWindowTarget() (gfx.NativeWindowTarget, bool) {
	return f.tgt, f.ok
}

// nopSurface 是最简 gfx.Surface (不实现 NativeWindowProvider)。
type nopSurface struct{}

func (nopSurface) Show(*image.RGBA)                           {}
func (nopSurface) ShowRegions(*image.RGBA, []image.Rectangle) {}
func (nopSurface) Size() (int, int)                           { return 64, 48 }
func (nopSurface) WaitEvents(time.Duration) bool              { return false }
func (nopSurface) Events() <-chan gfx.Event                   { return nil }

func TestSwapchainGate(t *testing.T) {
	if !Available() {
		t.Skip("GPU 呈现路径不可用 (无适配器)")
	}

	mk := func(tgt gfx.NativeWindowTarget, ok bool) *fakeNativeSurface {
		return &fakeNativeSurface{tgt: tgt, ok: ok}
	}
	cases := []struct {
		name  string
		inner gfx.Surface
	}{
		{"非提供方 inner", nopSurface{}},
		{"提供方但交不出句柄", mk(gfx.NativeWindowTarget{}, false)},
		{"x11 无 Xlib Display* (xgb 现状)", mk(gfx.NativeWindowTarget{Kind: "x11", Display: 0, Window: 0x1234}, true)},
		{"未知平台 kind", mk(gfx.NativeWindowTarget{Kind: "harmony", Display: 1, Window: 2}, true)},
		{"win32 伪句柄 (无效 HWND 必失败)", mk(gfx.NativeWindowTarget{Kind: "win32", Display: 0x400000, Window: 0xDEAD}, true)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.inner)
			if err != nil {
				t.Fatalf("New 失败: %v —— 句柄不可用应降级而非失败", err)
			}
			if s.PresentsToWindow() {
				t.Fatalf("伪句柄环境 PresentsToWindow()=true —— 兜底口径混入窗口化口径")
			}
			// 降级模式下渲染照常走离屏路径, 事件照常委托 inner。
			img := image.NewRGBA(image.Rect(0, 0, 64, 48))
			s.ShowRegions(img, nil)
			_ = s.WaitEvents(time.Millisecond)
			_ = s.Events()
			if w, _ := s.Size(); w != 64 {
				t.Fatalf("Size 未委托 inner: %d", w)
			}
		})
	}
}
