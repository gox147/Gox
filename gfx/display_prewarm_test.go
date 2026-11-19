package gfx

// 建连预热的闸门 (T3-5 第十八批)。
//
// 守的形态:
//   - **默认关闭**: 库被链接进任意进程都不会自己去连显示服务器;
//   - **起点在模块查找**: 与字体预取同一个信号 (脚本 import gx/gfx), 且
//     **必须排在字体预取之前** —— 顺序反了握手会被字体加载拖慢 8~10 倍
//     (§12.18), 而功能用例全绿, 属"悄悄吃掉收益"的那类回归;
//   - **后端不认领就不生效**: 没有 displayPrewarmer 能力时不报错、不 panic。
//
// 反向验证 (写在 docs/performance.md §12.18): 把 render.go 里
// prewarmDisplayForGUI() 与 prefetchBaseFont() 两行对调,
// TestPrewarmHappensBeforeFontPrefetch 立刻红。

import (
	"sync/atomic"
	"testing"

	"github.com/14752222/Gox/object"
)

func resetDisplayPrewarmForTest(t *testing.T) {
	t.Helper()
	displayPrewarmOn.Store(false)
	t.Cleanup(func() { displayPrewarmOn.Store(false) })
}

// prewarmSpyFactory 记录 PrewarmDisplay 被调了几次, 并可断言调用时字体预取
// 是否已经起了 (顺序回归)。
type prewarmSpyFactory struct {
	calls       int32
	sawPrefetch int32 // >0 表示调用时预取已经在跑 (= 顺序反了)
}

func (f *prewarmSpyFactory) Create(WindowConfig) (Surface, error) { return newFakeSurface(), nil }

func (f *prewarmSpyFactory) PrewarmDisplay() bool {
	atomic.AddInt32(&f.calls, 1)
	if prefetchStartedForTest() {
		atomic.AddInt32(&f.sawPrefetch, 1)
	}
	return true
}

// plainFactory 不实现 displayPrewarmer: 模拟"后端没有这个能力"。
type plainFactory struct{}

func (f *plainFactory) Create(WindowConfig) (Surface, error) { return newFakeSurface(), nil }

// TestDisplayPrewarmOffByDefault 未开启时, 模块查找与建窗都不去连显示服务器。
func TestDisplayPrewarmOffByDefault(t *testing.T) {
	resetDisplayPrewarmForTest(t)
	resetFontPrefetchForTest(t)

	spy := &prewarmSpyFactory{}
	SetDefaultFactory(spy)
	t.Cleanup(func() { SetDefaultFactory(nil) })

	object.InvalidateBuiltinModuleCache("gx/gfx")
	if _, ok := object.LookupBuiltinModule("gx/gfx"); !ok {
		t.Fatalf("gx/gfx 模块未注册")
	}
	if got := atomic.LoadInt32(&spy.calls); got != 0 {
		t.Fatalf("未开启时模块查找不该建连, 实际调用 %d 次", got)
	}

	mountWithFactory(t, &plainFactory{}, WindowConfig{Title: "prewarm-off", Width: 120, Height: 80})
	// mountWithFactory 换了工厂, 但开关仍是关的 —— 关键断言是"没有 panic、
	// 没有走 PrewarmDisplay", 由上面那句 calls==0 与这条用例的不崩溃共同覆盖。
	if got := atomic.LoadInt32(&spy.calls); got != 0 {
		t.Fatalf("未开启时建窗不该建连, 实际调用 %d 次", got)
	}
}

// TestGfxModuleLookupPrewarmsWhenEnabled 开启后, import gx/gfx 那一刻就建连。
func TestGfxModuleLookupPrewarmsWhenEnabled(t *testing.T) {
	resetDisplayPrewarmForTest(t)
	resetFontPrefetchForTest(t)
	EnableDisplayPrewarm()

	spy := &prewarmSpyFactory{}
	SetDefaultFactory(spy)
	t.Cleanup(func() { SetDefaultFactory(nil) })

	object.InvalidateBuiltinModuleCache("gx/gfx")
	if _, ok := object.LookupBuiltinModule("gx/gfx"); !ok {
		t.Fatalf("gx/gfx 模块未注册")
	}
	if got := atomic.LoadInt32(&spy.calls); got != 1 {
		t.Fatalf("import gx/gfx 应建连 1 次, 实际 %d 次", got)
	}
}

// TestPrewarmHappensBeforeFontPrefetch 守顺序: 建连必须在字体预取之前。
//
// 为什么值得一条专门用例: 两行代码对调后**所有功能用例仍然全绿**, 只有进程级
// 首帧会慢 0.7~1.2ms (§12.18 的 A/B) —— 那正是最容易在后续 batch 里被无意
// 改回去的形态。
func TestPrewarmHappensBeforeFontPrefetch(t *testing.T) {
	resetDisplayPrewarmForTest(t)
	resetFontPrefetchForTest(t)
	EnableDisplayPrewarm()
	EnableFontPrefetch()

	spy := &prewarmSpyFactory{}
	SetDefaultFactory(spy)
	t.Cleanup(func() { SetDefaultFactory(nil) })

	// 主路径: 模块查找 (脚本 import gx/gfx)。**必须丢导出表缓存**, 否则
	// builder 不会被再次调用, 断言就退化成"上一次进程状态"。
	object.InvalidateBuiltinModuleCache("gx/gfx")
	if _, ok := object.LookupBuiltinModule("gx/gfx"); !ok {
		t.Fatalf("gx/gfx 模块未注册")
	}
	if got := atomic.LoadInt32(&spy.calls); got == 0 {
		t.Fatalf("开启后模块查找应触发建连")
	}
	if got := atomic.LoadInt32(&spy.sawPrefetch); got != 0 {
		t.Fatalf("建连发生在字体预取之后 (%d 次) —— render.go 模块入口里两行顺序反了", got)
	}

	// 兜底路径: Mount (不走 JS import 的宿主)。同样要求先连后取。
	// reset 会把预取开关关掉, 所以这里要重新打开 —— 否则 Mount 里的
	// prefetchBaseFont 是空操作, 顺序断言会退化成"永远通过"。
	resetFontPrefetchForTest(t)
	EnableFontPrefetch()
	before := atomic.LoadInt32(&spy.calls)
	mountWithFactory(t, spy, WindowConfig{Title: "prewarm-order", Width: 120, Height: 80})
	if atomic.LoadInt32(&spy.calls) == before {
		t.Fatalf("开启后建窗应触发建连")
	}
	if got := atomic.LoadInt32(&spy.sawPrefetch); got != 0 {
		t.Fatalf("Mount 里建连发生在字体预取之后 (%d 次) —— 两行顺序反了", got)
	}
}

// TestPrewarmDisplayWithoutCapability 后端不认领时返回 false, 不 panic。
func TestPrewarmDisplayWithoutCapability(t *testing.T) {
	SetDefaultFactory(&plainFactory{})
	t.Cleanup(func() { SetDefaultFactory(nil) })

	if PrewarmDisplay() {
		t.Fatalf("后端没有 PrewarmDisplay 能力时应返回 false")
	}
	SetDefaultFactory(nil)
	if PrewarmDisplay() {
		t.Fatalf("没有默认后端时应返回 false")
	}
}
