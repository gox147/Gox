package gfx

import (
	"sync/atomic"
	"testing"
)

// ===== 闸门: 每帧布局开头解析显示密度时不许做往返查询 =====
//
// ## 为什么要有这条闸门
//
// `Layout` 的第一件事是 `refreshDensity()` —— 它要回答"本帧的 dp 换算系数是多少",
// 而答案取决于**窗口落在哪块屏上** (同一窗口从 1x 屏挪到 2x 屏, 下一帧就得按新
// 系数排)。这条语义是对的: 系数必须每帧刷, 不能在建树时一次性写进 props。
//
// 但实现方式曾经是"每帧现场向后端问一次": `DisplayOf(surface)` 在 X11 后端里是
// **两次同步往返** (xproto.TranslateCoordinates + randr.GetMonitors), 再到内核侧
// `findDisplay` 里用 `Displays()` 再枚举一遍 (**第三次往返**)。三次往返都在渲染
// 线程上, 单屏上每帧白白付 0.3~0.5ms —— 桌面 60fps 的帧预算 16.7ms 里有 2~3%
// 花在"确认我没被挪到别的屏上"这件事上, 而实际上窗口没动。
//
// 正确的形态是**事件驱动**: 后端在"窗口真的换了屏"时通知内核 (X11 的
// ConfigureNotify / RandR screen change, win32 的 WM_DISPLAYCHANGE,
// cocoa 的 didChangeScreen), 内核据此作废缓存; 布局路径只读缓存。
// 于是每帧成本从"三次往返"降到"一次指针比较"。
//
// ## 这条闸门守的是什么
//
// 用假后端数调用次数, 不数毫秒 —— 毫秒在 CI runner 上是噪声 (§12.11 已经
// 因为同一个理由把阈值从 1.33 降到 1.15); 而"每帧问了几次后端"是确定量,
// 退回去就必然红。
//
// 为什么单独写一个假工厂而不复用 scaledFactory: 那个的可调用次数由用例自己
// 数在字段上, 而这里要在 `Layout` 内部也能计数 (Layout 是包内函数, 拿得到
// 接口)。用 atomic 是因为将来若有并行布局路径, 计数器不能有数据竞争。

// densityProbeFactory 是会数调用次数的 displayProvider 假工厂。
type densityProbeFactory struct {
	s      *fakeSurface
	scale  float64
	calls  atomic.Int64 // DisplayOf 被调用的次数
	dispss atomic.Int64 // Displays 被调用的次数
}

func (f *densityProbeFactory) Create(cfg WindowConfig) (Surface, error) { return f.s, nil }

func (f *densityProbeFactory) Displays() []Display {
	f.dispss.Add(1)
	return []Display{{
		ID: "probe-0", Name: "probe", W: 1920, H: 1080,
		WorkW: 1920, WorkH: 1080, Scale: f.scale, Primary: true,
	}}
}

func (f *densityProbeFactory) DisplayOf(s Surface) (string, bool) {
	f.calls.Add(1)
	if s == f.s {
		return "probe-0", true
	}
	return "", false
}

// withProbeFactory 装好假工厂 + 假 app, 用完彻底还原 (密度是包级状态)。
func withProbeFactory(t testing.TB, scale float64) *densityProbeFactory {
	t.Helper()
	fake := newFakeSurface()
	f := &densityProbeFactory{s: fake, scale: scale}
	SetDefaultFactory(f)
	a := &app{surface: fake, dirtyNodes: map[*GuiNode]struct{}{}}
	registerApp(a)
	ResetDensityForTest()
	t.Cleanup(func() {
		unregisterApp(a)
		appMu.Lock()
		if activeApp == a {
			activeApp = nil
		}
		appMu.Unlock()
		SetDefaultFactory(nil)
		ResetDensityForTest()
	})
	return f
}

// TestLayoutDensityNoBackendRoundTrip 是核心断言: **连续两次** Layout, 后端只能被
// 问一次 (第一次拿不到缓存, 允许问; 之后每帧都必须走缓存)。
//
// 这条用例在修之前必然是红的 (每次 Layout 都问) —— 它就是"先建闸门再优化"里那道闸门。
func TestLayoutDensityNoBackendRoundTrip(t *testing.T) {
	f := withProbeFactory(t, 2)

	root := mkNode("column", map[string]float64{"width": 400, "height": 300})
	root.Tag = "column"

	Layout(root, 400, 300)
	first := f.calls.Load()
	if first == 0 {
		t.Fatalf("首次 Layout 应当解析一次密度 (缓存为空), 实际后端调用 0 次")
	}
	if first > 1 {
		t.Fatalf("首次 Layout 解析密度不许重复问后端: got %d 次", first)
	}

	// 第二~第 N 帧: 窗口没换, 一个往返都不该有。
	for i := 2; i <= 6; i++ {
		Layout(root, 400, 300)
	}
	if got := f.calls.Load(); got != first {
		t.Fatalf("窗口未换屏时后续每帧都在问后端: DisplayOf 调用 %d 次 (首帧 %d 次)", got, first)
	}
	if got := f.dispss.Load(); got > 1 {
		t.Fatalf("Displays 枚举只该在解析时发生一次 (findDisplay 不许每帧重列), got %d", got)
	}
}

// TestLayoutDensityFollowsWindowMove 守"事件驱动"的语义面: 窗口换屏后, 下一帧
// 必须用**新的**系数 —— 省往返不许把"每帧刷"的语义也省掉。
//
// 这条在修之前是绿的 (旧实现天生每帧问), 修之后必须仍然绿 —— 它是闸门的另一半:
// 光把查询省掉而忘了在换屏时作废缓存, 就成了"跨屏后布局永远按旧系数"的真 bug。
func TestLayoutDensityFollowsWindowMove(t *testing.T) {
	f := withProbeFactory(t, 1)

	root := mkNode("column", map[string]float64{"width": 400, "height": 300})
	root.Tag = "column"
	Layout(root, 400, 300)
	if got := displayScale(); got != 1 {
		t.Fatalf("初始屏 scale=1, got %v", got)
	}

	// 窗口被挪到 2x 屏: 后端把同一块 surface 报到另一块屏上。
	f.scale = 2
	noteSurfaceDisplayMaybeChanged(f.s)

	Layout(root, 400, 300)
	if got := displayScale(); got != 2 {
		t.Fatalf("窗口换到 2x 屏后下一帧应当按 2.0 布局, got %v", got)
	}

	// 再挪回 1x: 同样必须跟上 (作废是每次事件都发生, 不是一次性)。
	f.scale = 1
	noteSurfaceDisplayMaybeChanged(f.s)
	Layout(root, 400, 300)
	if got := displayScale(); got != 1 {
		t.Fatalf("窗口挪回 1x 屏后应当按 1.0 布局, got %v", got)
	}
}

// TestLayoutDensityNotifyOtherSurfaceKeepsCache 守"事件粒度": 别的窗口换屏不该让
// 当前窗口的缓存失效 —— 多窗口应用里窗口 A 拖到 2x 屏, 不该让窗口 B 每动一次
// 就多问一次后端。通知带 surface 参数就是为了这个。
func TestLayoutDensityNotifyOtherSurfaceKeepsCache(t *testing.T) {
	f := withProbeFactory(t, 2)
	root := mkNode("column", map[string]float64{"width": 400, "height": 300})
	root.Tag = "column"
	Layout(root, 400, 300)
	base := f.calls.Load()

	other := newFakeSurface()
	noteSurfaceDisplayMaybeChanged(other)
	Layout(root, 400, 300)

	if got := f.calls.Load(); got != base {
		t.Fatalf("别的窗口换屏不应当让当前窗口重问后端: got %d 次 (基线 %d 次)", got, base)
	}
}

// TestDisplayScaleUncachedPathStillResolves 守"无缓存读数"路径: 没走过 Layout 的
// 读数 (挂载前的测量、纯逻辑单测) 仍要现场解析 —— 不许为了省往返而让这些
// 场景拿到 0 或恒 1。
func TestDisplayScaleUncachedPathStillResolves(t *testing.T) {
	withProbeFactory(t, 3)
	if got := displayScale(); got != 3 {
		t.Fatalf("无缓存时的读数必须现场解析, got %v", got)
	}
}
