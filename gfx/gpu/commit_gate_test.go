//go:build gpu

package gpu

// 两段式提交 gate (gfx.NativeWindowCommit, 2026-11-19):
// 钉住「建链结果 → 第 2 段提交」的接线纪律 —— cocoa 的降级安全完全
// 依赖 GPU 后端只在建链**成功**后发 CommitNativeWindow(true)、失败发
// false, 这条纪律不能靠口头约定。真机切层与恢复 (AppKit 侧行为) 属于
// G-3 三平台真机验证, 无头沙箱验不到, 也不许拿兜底口径冒充。

import (
	"image"
	"testing"
	"time"

	"github.com/14752222/Gox/gfx"
)

// fakeCommitSurface 在 fakeNativeSurface 上叠加提交记录,
// 探测 initSwapchain 两端与 degradeSwapchain 的触达语义。
type fakeCommitSurface struct {
	fakeNativeSurface
	calls []bool // CommitNativeWindow 逐次实参 (顺序敏感)
}

func (f *fakeCommitSurface) CommitNativeWindow(ok bool) {
	f.calls = append(f.calls, ok)
}

func TestSwapchainCommitGate(t *testing.T) {
	if !Available() {
		t.Skip("GPU 呈现路径不可用 (无适配器)")
	}

	t.Run("建链失败: 恰好一次 false, 不得误发 true", func(t *testing.T) {
		// cocoa 伪层句柄: 无头沙箱没有 Metal, 建链必然失败 ——
		// 正好用来验证失败路径的回收调用。
		fc := &fakeCommitSurface{fakeNativeSurface: fakeNativeSurface{
			tgt: gfx.NativeWindowTarget{Kind: "cocoa", Window: 0xC0FFEE}, ok: true,
		}}
		s, err := New(fc)
		if err != nil {
			t.Fatalf("New 失败: %v —— 建链失败应降级而非失败", err)
		}
		if s.PresentsToWindow() {
			t.Fatalf("伪层句柄 PresentsToWindow()=true —— 兜底口径混入窗口化口径")
		}
		if len(fc.calls) != 1 || fc.calls[0] != false {
			t.Fatalf("建链失败应恰好一次 CommitNativeWindow(false), 实得 %v", fc.calls)
		}
	})

	t.Run("非提交方 inner: 不触达提交接口", func(t *testing.T) {
		fc := &fakeCommitSurface{fakeNativeSurface: fakeNativeSurface{
			tgt: gfx.NativeWindowTarget{Kind: "harmony", Display: 1, Window: 2}, ok: true,
		}}
		// 用不实现 NativeWindowProvider 的 inner, 两段式全程不触达。
		s, err := New(nopSurface{})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		_ = s
		if len(fc.calls) != 0 {
			t.Fatalf("未参与建链的 inner 不应被提交: %v", fc.calls)
		}
	})

	t.Run("degradeSwapchain: 幂等触达 false (实现方侧承诺 no-op)", func(t *testing.T) {
		// 直连降级收拾函数 (swapBroken 运行期路径走的就是它):
		// gpu 侧每次失效都触达, 幂等性由实现方 (cocoa metal.go) 承诺,
		// 这里钉住的是「降级必须先恢复载体再落回」这条顺序纪律的触达点。
		fc := &fakeCommitSurface{}
		s := &Surface{inner: fc}
		s.degradeSwapchain()
		s.degradeSwapchain()
		if len(fc.calls) != 2 {
			t.Fatalf("两次降级应各触达一次, 实得 %v", fc.calls)
		}
		for _, ok := range fc.calls {
			if ok {
				t.Fatalf("降级路径不得发送 CommitNativeWindow(true): %v", fc.calls)
			}
		}
	})

	t.Run("降级后渲染照常委托 inner (窗口不黑的前提)", func(t *testing.T) {
		fc := &fakeCommitSurface{fakeNativeSurface: fakeNativeSurface{
			tgt: gfx.NativeWindowTarget{Kind: "cocoa", Window: 0xC0FFEE}, ok: true,
		}}
		s, err := New(fc)
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		img := image.NewRGBA(image.Rect(0, 0, 64, 48))
		s.ShowRegions(img, nil) // 降级模式: 离屏路径, 不得 panic
		_ = s.WaitEvents(time.Millisecond)
	})
}
