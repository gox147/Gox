package gfx

// 默认字体预取的闸门 (T3-5 第十六批)。
//
// 这一组用例守的不是"快了多少"(那是 bench 的事), 而是**预取这件事的边界**:
//   - 默认关闭: 库被链接进任意进程都不会自作主张读字体;
//   - 开了才做, 且每进程只起一次;
//   - 预取的结果必须被首帧真正复用 (而不是"又解析一遍" —— 那等于没省);
//   - 没窗口就不该有预取 (与 §12.6 的惰性口径一致)。
//
// 反向验证 (写在 docs/performance.md §12.16): 把 prefetchBaseFont 的
// fontPrefetchOn 判断去掉, TestFontPrefetchOffByDefault 立刻红;
// 把 Mount 里的调用删掉, TestMountPrefetchesBaseFontWhenEnabled 超时红。

import (
	"testing"
	"time"

	"golang.org/x/image/font/opentype"

	"github.com/14752222/Gox/object"
)

// resetFontPrefetchForTest 把预取状态还原成"本进程刚开始"的样子。
func resetFontPrefetchForTest(t *testing.T) {
	t.Helper()
	fontPrefetchOn.Store(false)
	fontPrefetchMu.Lock()
	fontPrefetchStarted = false
	fontPrefetchMu.Unlock()
	resetFontStageCaches()
	t.Cleanup(func() {
		fontPrefetchOn.Store(false)
		fontPrefetchMu.Lock()
		fontPrefetchStarted = false
		fontPrefetchMu.Unlock()
		resetFontStageCaches()
	})
}

func prefetchStartedForTest() bool {
	fontPrefetchMu.Lock()
	defer fontPrefetchMu.Unlock()
	return fontPrefetchStarted
}

// TestFontPrefetchOffByDefault 库默认不预取: 建窗之后 baseFont 仍为空,
// 也没有后台预取被起过。
func TestFontPrefetchOffByDefault(t *testing.T) {
	resetFontPrefetchForTest(t)
	f := &oneSurfaceFactory{}
	mountWithFactory(t, f, WindowConfig{Title: "prefetch-off", Width: 120, Height: 80})

	if prefetchStartedForTest() {
		t.Fatalf("未开启时不该起预取 goroutine")
	}
	// 给假想中的后台加载一点时间: 真的是后台的话, 30ms 足够它把 baseFont 填上。
	time.Sleep(30 * time.Millisecond)
	fontMu.Lock()
	got := baseFont
	fontMu.Unlock()
	if got != nil {
		t.Fatalf("未开启时不该加载默认字体 (baseFont 已非空)")
	}
}

// TestMountPrefetchesBaseFontWhenEnabled 开启后建窗即预热, 且首帧用的就是
// 预取的那一份 (指针相同 = 没有二次解析)。
func TestMountPrefetchesBaseFontWhenEnabled(t *testing.T) {
	resetFontPrefetchForTest(t)
	EnableFontPrefetch()

	mountWithFactory(t, &oneSurfaceFactory{}, WindowConfig{Title: "prefetch-on", Width: 120, Height: 80})

	var prefetched *opentype.Font
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fontMu.Lock()
		prefetched = baseFont
		fontMu.Unlock()
		if prefetched != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if prefetched == nil {
		// 机器上没有任何可用系统字体时, "预取到东西"本来就无从谈起 ——
		// 这与 loadBaseFont 的失败口径一致, 不是本闸门要守的形态。
		if _, err := loadBaseFont(); err != nil {
			t.Skipf("本机没有可用系统字体, 预取闸门无对象: %v", err)
		}
		t.Fatalf("开启预取后 10s 内 baseFont 仍为空")
	}

	// 首帧路径 (loadBaseFont) 必须直接命中, 不是重新解析一遍。
	got, err := loadBaseFont()
	if err != nil {
		t.Fatalf("loadBaseFont: %v", err)
	}
	if got != prefetched {
		t.Fatalf("首帧没有复用预取结果 (指针不同: 预取=%p 首帧=%p)", prefetched, got)
	}
}

// TestPrefetchSingleShotPerProcess 第二个窗口不再起第二个预取, 也不重建字体。
func TestPrefetchSingleShotPerProcess(t *testing.T) {
	resetFontPrefetchForTest(t)
	EnableFontPrefetch()

	mountWithFactory(t, &oneSurfaceFactory{}, WindowConfig{Title: "a", Width: 100, Height: 60})
	waitBaseFont(t)
	if !prefetchStartedForTest() {
		t.Fatalf("首窗之后应已起过预取")
	}

	fontMu.Lock()
	first := baseFont
	fontMu.Unlock()
	mountWithFactory(t, &oneSurfaceFactory{}, WindowConfig{Title: "b", Width: 100, Height: 60})
	fontMu.Lock()
	second := baseFont
	fontMu.Unlock()
	if first != second {
		t.Fatalf("第二个窗口重建了字体 (不该发生)")
	}
}

// TestGfxModuleLookupStartsPrefetch 守"起跑线": 预取必须挂在 gx/gfx 模块
// 首次查找上, 而不是只挂在 Mount 上。
//
// 为什么这条重要: A/B 里 Mount-only 只省 0.23ms, 模块查找处起省 0.70~0.95ms ——
// 同一份字体加载, 起点晚 1ms 就只藏住三分之一。把调用挪回 Mount 不会让任何
// 功能用例变红, 只会悄悄吃掉 2/3 的收益, 所以这里直接断言起点。
func TestGfxModuleLookupStartsPrefetch(t *testing.T) {
	resetFontPrefetchForTest(t)
	EnableFontPrefetch()

	// 导出表是进程级缓存 (object.LookupBuiltinModule): 前面的用例早就 import
	// 过 gx/gfx, 不先丢弃缓存的话 builder 根本不会被再次调用, 这条断言就成了
	// "上一次进程状态"的断言。
	object.InvalidateBuiltinModuleCache("gx/gfx")
	if _, ok := object.LookupBuiltinModule("gx/gfx"); !ok {
		t.Fatalf("gx/gfx 模块未注册 (render.go 的 init 契约变了?)")
	}
	if !prefetchStartedForTest() {
		t.Fatalf("import gx/gfx 之后应已起预取 (起点被挪回 Mount 了?)")
	}
}

func waitBaseFont(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fontMu.Lock()
		ok := baseFont != nil
		fontMu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("10s 内 baseFont 未就绪")
}

// oneSurfaceFactory 是最小的 WindowFactory: 每次 Create 造一块新的假面
// (同一个假面挂两个窗口会撞 app 注册表)。
type oneSurfaceFactory struct{}

func (f *oneSurfaceFactory) Create(WindowConfig) (Surface, error) { return newFakeSurface(), nil }
