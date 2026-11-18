package gfx

import "sync"

// ===== 密度换算: 脚本写的逻辑单位 (dp/pt) → 内核的设备像素 =====
//
// ## 问题
//
// 内核的坐标约定是**设备像素** (后端 Size / 事件坐标都是它)。脚本写的
// `font={20}`、`padding={12}` 是按 dp/pt 直觉写的: 作者要的是"20 磅那么大",
// 不是"20 个物理像素"。3x 手机屏上 20 像素只有 6.7pt —— 实测整个 UI 挤成
// 一小块 (看板 rpr9zf §1)。
//
// ## 口径
//
//   - **脚本写出来的尺寸数字一律按逻辑单位解释**, 取值时乘窗口所在显示器的
//     `Display.Scale` 换成设备像素。
//   - **内核自己算出来的东西一律是设备像素**, 不参与换算: 屏幕/窗口尺寸、
//     图片自然尺寸、滚动偏移。
//   - **内置组件的度量常量是设计像素** (Prism S8 起的口径): 凡是排在 flex 里、
//     或者盒子里盛着字的量 —— 行高、字段行高、页码按钮边长、两个 picker 的网格
//     —— 取值处一律经 Space() 换算。**例外**是纯装饰的"发丝"量 (焦点环 2px、
//     table 的 1px 网格线、投影参数), 它们刻意留在物理像素。
//   - `Scale == 1` (桌面 x11/win32) 时换算恒等 —— 见 dpToPx 的快路径, 桌面
//     渲染结果与换算前**逐像素一致**, 既有 golden 与像素断言不会漂移。
//
// ## 为什么是"取值时换算"而不是"建树时改 props"
//
// 密度会变: 窗口被拖到另一块屏、用户在系统里改缩放、折叠屏切换主副屏。
// 写进 props 就变成一次性的, 而且会污染"脚本写的值" (脚本读回自己的 prop
// 时不该看到被内核乘过的数)。取值时换算则天然每帧新鲜。
//
// ## 为什么缓存而不是每次读 prop 都去问后端
//
// 解析一次要走 `displayProvider.DisplayOf(surface)` —— 那是平台 API
// (NSWindow / hwnd / ANativeWindow)。布局一帧要读上千次尺寸属性, 每次都
// 调平台 API 会直接吃掉帧预算。所以系数按帧缓存: Layout (每帧布局入口)
// 开头刷新一次, 本帧所有读数共用。
//
// ## 缓存何时作废: 事件驱动, 不是每帧重算 (T3-5 第十四批, 待接后端事件)
//
// 上一版把"按帧刷新"实现成了"每帧重新问后端"。X11 上那是每帧三次同步往返
// (TranslateCoordinates 12.3µs + QueryVersion 10.8µs + GetMonitors 13.5µs,
// 实测), 每帧 36.6µs 花在确认"窗口没被挪到别的屏上"。
//
// 现在: 系数与"它属于哪块 surface"一起缓存, 只在两种事件下作废 ——
//
//	① 窗口换了 (densitySurf != 当前 surface): 每次刷新只花一次指针比较;
//	② 后端报告"这块窗口可能换屏了" (noteSurfaceDisplayMaybeChanged):
//	   X11 的 ConfigureNotify / RandR screen change、win32 的 WM_DISPLAYCHANGE /
//	   WM_DPICHANGED、cocoa 的 NSWindowDidChangeScreen 都要调它。
//
// **为什么后端只需报"可能"**: 判断"真的换了"本身就要问后端 (那正是要省掉的
// 往返)。多作废一次只是多问一次后端; 漏报则会让布局永远按旧系数 —— 宁可多
// 作废, 不可漏报。

var (
	densityMu    sync.Mutex
	densityScale float64 // 本帧系数; <=0 表示还没算过
	densitySurf  Surface // 该系数对应的窗口 (窗口换了就作废)
	// densityValid 为 false 表示"缓存被事件作废, 下次 refreshDensity 必须重算"。
	densityValid bool
)

// activeSurface 返回"当前窗口"的 Surface (无窗口 → nil)。
func activeSurface() Surface {
	if a := currentApp(); a != nil {
		return a.surface
	}
	return nil
}

// resolveDensity 现场解析密度系数 (无窗口 / 后端查不到 / Scale<=1 → 1)。
//
// 与 defaultFontSize 的前身同一条判定链: 查不到屏必须**安静地落 1**, 而不是
// 报 warning —— 纯节点单测、无头宿主、以及"后端没实现 displayProvider"的
// 平台都走这条路径, 出声只会制造噪音。
func resolveDensity() float64 {
	s := activeSurface()
	if s == nil {
		return 1
	}
	if dp, ok := defaultFactory.(displayProvider); ok {
		if id, hit := dp.DisplayOf(s); hit && id != "" {
			if d, found := findDisplay(id); found && d.Scale > 1 {
				return d.Scale
			}
		}
	}
	return 1
}

// refreshDensity 刷新本帧的密度系数。每帧布局的入口 (Layout) 调一次。
//
// 命中条件: 缓存有效 **且** 属于当前窗口 —— 此时一个往返都不发。
func refreshDensity() {
	cur := activeSurface()
	densityMu.Lock()
	if densityValid && densitySurf == cur {
		densityMu.Unlock()
		return
	}
	densityMu.Unlock()

	// 解析要调后端, 不在持锁时做 (后端回调里可能再读密度 ⇒ 自我死锁)。
	s := resolveDensity()

	densityMu.Lock()
	densityScale, densitySurf, densityValid = s, cur, true
	densityMu.Unlock()
}

// noteSurfaceDisplayMaybeChanged 由后端在"这块窗口的位置/所在显示器可能变了"
// 时调用 (窗口移动/缩放、显示器插拔、分辨率或缩放变更、RandR screen change)。
//
// 语义是**保守作废**: 传 nil 作废全部 (拓扑变了, 所有窗口都可能换屏);
// 传具体 surface 只作废属于它的缓存 (别的窗口换屏不该让本窗口重问后端)。
// 拖拽期间同名事件可能每帧来一次 —— 作废只在"下一次 Layout"才产生一次解析,
// 不会累积。
// noteSurfaceCreated 由后端在**新窗口建好之后**调用 (X11 MapWindow 之后、
// win32 CreateWindowEx 之后、移动端宿主 attach 之后)。
//
// 它只做一件事: 让这个窗口的密度缓存"立即播种"一次。不播种的话, 第一次
// Layout 仍会现场解析 (那是对的), 但**之后**每一次"作废"都要等到下一次
// Layout 才解析 —— 播种之后, 作废与重算都发生在事件到达的那一帧, 布局路径
// 更干净。更实际的理由: 窗口建好到第一次 Layout 之间若有别的地方读密度
// (挂载测量), 播种能保证它与首帧用的是同一个值。
func noteSurfaceCreated(s Surface) {
	noteSurfaceDisplayMaybeChanged(s)
}

// NoteSurfaceCreated 是 noteSurfaceCreated 的**导出**形式, 供 gfx 之外的
// 平台后端 (gfx/x11、gfx/win32、gfx/cocoa 与移动宿主绑定层) 在建窗成功后调用。
// 后端包不能调包内名字, 而这条纪律必须在后端落地 —— 少了它首帧仍然正确
// (第一次 Layout 会现场解析), 只是作废后的第一次重算会被推迟到下一次布局。
func NoteSurfaceCreated(s Surface) { noteSurfaceCreated(s) }

func noteSurfaceDisplayMaybeChanged(s Surface) {
	densityMu.Lock()
	if s == nil || densitySurf == nil || densitySurf == s {
		densityValid = false
		densityScale = 0
	}
	densityMu.Unlock()
}

// displayScale 返回本帧的密度系数 (恒 >= 1)。
//
// 两条路径:
//   - 缓存命中 (本帧 Layout 已刷过、窗口没换、没被事件作废): 直接返回;
//   - 没走过 Layout 的读数 (挂载前的测量、纯逻辑单测、事件回调里): 现场解析,
//     **不写缓存** —— 写了会让"下一帧的第一次读数"用到上一帧的窗口。
func displayScale() float64 {
	cur := activeSurface()
	densityMu.Lock()
	s, surf, ok := densityScale, densitySurf, densityValid
	densityMu.Unlock()
	if ok && s > 0 && surf == cur {
		return s
	}
	return resolveDensity()
}

// ResetDensityForTest 清掉密度缓存 (测试用: 用例之间不许串味)。
func ResetDensityForTest() {
	densityMu.Lock()
	densityScale = 0
	densitySurf = nil
	densityValid = false
	densityMu.Unlock()
}

// dpToPx 把脚本写的逻辑尺寸换算成设备像素 (四舍五入)。
//
// Scale<=1 时走 `int(v)` 的恒等快路径 —— 注意不能统一用 roundHalf: 既有
// 代码在 Scale=1 时是**截断** (`int(10.7) == 10`), 改成四舍五入会让桌面
// 1x 的渲染结果漂移, 而"桌面 1x 逐像素不变"是本次改动的硬约束。
func dpToPx(v float64) int {
	s := displayScale()
	if s <= 1 {
		return int(v)
	}
	return roundHalf(v * s)
}
