//go:build gpu

package gpu_test

// ===== G-4 视觉闸门 · 26 组件 CPU vs GPU 双轨像素级 A/B =====
//
// 干什么: 把 Prism 核心组件的画廊脚本 (testdata/shots/*.js) 逐个挂两遍 ——
// 一遍 CPU 参照帧 (captureSurface), 一遍 GPU 合成帧 (gpu.Surface + Readback)
// —— 逐像素比对, 差异阈值 **0**。这对应 G-4 验收里的「26 组件快照像素级
// A/B」: GPU 改动不许让界面走样, 帧率再快也不行。
//
// 为什么阈值敢定 0: 文本/形状/抗锯齿全部出自同一条 CPU 光栅化链
// (gfx raster), GPU 呈现路径只做「帧缓冲纹理 → 合成 quad → (readback)」,
// 不碰任何绘制语义。同机同字体双跑, 差异只能来自 GPU 合成本身 ——
// 所以任何非零差异都是 GPU 轨的 bug, 不存在「可接受的近似」。
//
// 组件清单 (26 个, 与 docs/prism-design-system.md §3 S3~S7 对齐):
//   基础控件: button input search textarea checkbox radio switch slider
//   集合:     table list-item tree tabs pagination
//   浮层/反馈: menu select alert datepicker(收起态) avatar badge tag
//             separator empty label form rating upload
// 入表纪律:
//   - menu.js 脚本自带预展开下拉, tree.js 拍收起态, datepicker.js 拍收起态
//     —— 三者的取帧均不依赖交互事件, 双轨序列完全一致; 弹层点击类交互由
//     TestGPUPresentPixelExact 的悬停支路覆盖 (局部帧分支), 不在这里重复。
//   - **progress.js 不入表**: 其 indeterminate 色块是 1200ms 循环位移动画,
//     由 16ms 动画心跳按 wall-clock 推进 —— 双轨两次 mount 的取帧相位不可
//     对齐, 对照结果随跑随变 (2026-11-18 实测: 同机双跑出现 4~14 个 accent
//     色差异像素, 全因色块相位, 不是合成 bug)。动画驱动型脚本
//     (tooltip/dialog/drawer/spinner/skeleton/datav-*) 同理不入表 ——
//     这不是豁免, 是「A/B 必须可复现」的口径纪律。
//
// 跳过纪律 (G-4 验收「无硬件 runner 的跳过逻辑有测试钉住」):
//   - 只有 wgpu 连软件适配器都建不起来 (Available()==false) 才 skip;
//   - 软件适配器 (BackendKind()=="software") **必须照跑**: 它是 CI 的确定性
//     后端, 视觉闸门要的是像素一致, 不是帧率 —— 不许拿「没显卡」当跳过理由。
//     该语义由 TestGPUSnapshotABGate 钉住, 不是散落在测试体里的裸 if。
//
// 运行: go test -tags gpu ./gfx/gpu -run TestGPUSnapshotAB -v

import (
	"image"
	"image/color"
	"path/filepath"
	"testing"
	"time"

	"github.com/14752222/Gox/gfx"
	gpupkg "github.com/14752222/Gox/gfx/gpu"
	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/vm"
)

// snapshotABScripts 是入表的 26 个组件脚本名 (不含 .js, 与 testdata/shots 对齐)。
// 顺序按 Prism 文档 §3 的分组排, 保持与设计系统同一套阅读口径。
var snapshotABScripts = []string{
	// §3.1 基础控件 (S3/S4) —— progress 不入表: 其 indeterminate 变体是
	// 循环动画 (见文件头「入表纪律」), determinate 形态暂无双轨可复现的
	// 静态脚本, 待 gallery 侧拆出 determinate 用例后补入。
	"button", "input", "search", "textarea", "checkbox", "radio",
	"switch", "slider",
	// §3.2 集合组件 (S5)
	"table", "list-item", "tree", "tabs", "pagination",
	// §3.3 浮层组件 (S6) —— menu 预展开 / select·datepicker 收起态, 均静态可复现
	"menu", "select", "alert", "datepicker",
	// §3.4 反馈与展示 (S7)
	"avatar", "badge", "tag", "separator", "empty", "label", "form",
	"rating", "upload",
}

// snapshotABDir 是组件脚本目录 (相对 gfx/gpu 包; gpu 包在 gfx 外, 不能用
// gfx 包内的 galleryScriptDir 常量)。
const snapshotABDir = "../../testdata/shots"

// snapshotABGate 是 26 组件套件的跳过判据 —— **唯一**的 skip 决策点,
// 被 TestGPUSnapshotABGate 钉住:
//   - available=false: wgpu 连软件适配器都建不起来, 环境没有可用的 GPU 轨,
//     skip (理由返回给日志, 不静默);
//   - available=true: 无论 hardware 还是 software 都跑。software 是确定性
//     后端, 视觉闸门跑在它上面正是 CI 无显卡也能验像素一致的原因。
func snapshotABGate(available bool, kind string) string {
	if !available {
		return "wgpu 适配器不可用 (连软件适配器都建不起来), GPU 轨不存在 —— 视觉闸门无从跑起"
	}
	_ = kind // 软件适配器不许跳过: 见文件头「跳过纪律」。kind 参与签名是为了
	// 让钉住测试能表达「software 也要跑」这条语义, 防止将来有人把它改成
	// 「非 hardware 就 skip」而测试毫无感觉。
	return ""
}

// TestGPUSnapshotABGate 钉住跳过逻辑 (G-4 验收: 跳过要有测试钉住, 不靠猜):
// 无适配器 → skip; 软件适配器 → 照跑 (不许拿没显卡当理由); 硬件适配器 → 跑。
func TestGPUSnapshotABGate(t *testing.T) {
	if r := snapshotABGate(false, "software"); r == "" {
		t.Fatal("无适配器时 gate 必须给出 skip 理由 —— 空理由等于永远跑, 环境坏了只会莫名红")
	}
	if r := snapshotABGate(true, "software"); r != "" {
		t.Fatalf("软件适配器不许跳过 (它是 CI 的确定性后端, 视觉闸门靠它跑), gate 却说: %s", r)
	}
	if r := snapshotABGate(true, "hardware"); r != "" {
		t.Fatalf("硬件适配器必须跑, gate 却说: %s", r)
	}
}

// snapshotFactory 与 factory 同型, 但按 <window> 声明的尺寸开面 ——
// 26 个脚本各有各的画幅 (360x120 的 avatar 到 420x420 的 table), 固定
// 400x300 会把大组件裁掉一半, 比对口径就缩水了 (与 gfx 包内 galleryFactory
// 同一手法的 gpu_test 侧复刻: gpu 包在 gfx 外, 用不了它的测试替身)。
type snapshotFactory struct {
	inner *captureSurface // 尺寸落在捕获面上 (两条路径共用同一尺寸逻辑)
	s     gfx.Surface     // 真正交给 SetDefaultFactory 的面 (GPU 轨是 gpu.Surface)
}

func (f *snapshotFactory) Create(cfg gfx.WindowConfig) (gfx.Surface, error) {
	if cfg.Width > 0 && cfg.Height > 0 {
		f.inner.w, f.inner.h = cfg.Width, cfg.Height
	}
	return f.s, nil
}

// snapshotABInk 是「这帧真的画了东西」的判据 (galleryInkStats 的 gpu_test
// 侧复刻): 颜色种类 + 非底色像素。没有它, 「CPU 和 GPU 都画成空盒子」会
// 以 0 差异的成绩通过比对 —— 两张白纸当然相等。
type snapshotABInk struct {
	Colors int
	Ink    int
}

func snapshotABInkStats(img *image.RGBA) snapshotABInk {
	b := img.Bounds()
	bg := img.RGBAAt(b.Min.X, b.Min.Y)
	seen := map[color.RGBA]struct{}{}
	ink := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			seen[c] = struct{}{}
			if c != bg {
				ink++
			}
		}
	}
	return snapshotABInk{Colors: len(seen), Ink: ink}
}

// snapshotABMinColors 返回某脚本的颜色数下限 (与 gfx 包画廊判据同源):
// 入表脚本画面里都有文字/图标或色块, 8 种以下是「画成空盒子」的症状。
func snapshotABMinColors(string) int { return 8 }

// TestGPUSnapshotAB26 逐组件跑 CPU vs GPU 像素级 A/B。
func TestGPUSnapshotAB26(t *testing.T) {
	if r := snapshotABGate(gpupkg.Available(), gpupkg.BackendKind()); r != "" {
		t.Skip(r)
	}

	for _, name := range snapshotABScripts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(snapshotABDir, name+".js")
			load := func() (*vm.VM, error) { return vm.EvalFileVM(path) }

			// ---- 路径 A: CPU 参照帧 ----
			// 空转轮次与画廊同源 (gallerySettleRounds=2): mount 首帧之外再给
			// 挂载副作用 (signal → effect → 标脏 → 重绘) 两轮落地机会, 取到的
			// 是稳定态而不是半成品。两条路径轮次逐一相同, 差异只可能来自合成。
			cpuCap := newCaptureSurface(400, 300)
			runAndSettleFactory(t, load, cpuCap, cpuCap)
			cpuFrame := cpuCap.frame()
			if cpuFrame == nil {
				t.Fatal("CPU 路径一帧都没上屏")
			}

			// ---- 路径 B: GPU 合成帧 (软件适配器 = CI 确定性后端) ----
			gpuInner := newCaptureSurface(400, 300)
			gs, err := gpupkg.New(gpuInner)
			if err != nil {
				t.Fatalf("gpu.Surface: %v", err)
			}
			runAndSettleFactory(t, load, gs, gpuInner)
			gpuFrame, err := gs.Readback()
			if err != nil {
				t.Fatalf("GPU readback: %v", err)
			}

			// ---- 非空白守卫 (先于比对: 两张白纸的 0 差异没有意义) ----
			cpuInk := snapshotABInkStats(cpuFrame)
			minColors := snapshotABMinColors(name)
			if cpuInk.Colors < minColors || cpuInk.Ink < 400 {
				t.Fatalf("CPU 参照帧近乎空白 (%d 色 / %d 墨, 阈值 %d 色 / 400 墨) —— "+
					"脚本没画出东西, 比对形同虚设", cpuInk.Colors, cpuInk.Ink, minColors)
			}
			if gpuFrame.Bounds() != cpuFrame.Bounds() {
				t.Fatalf("尺寸不一致: CPU %v vs GPU %v", cpuFrame.Bounds(), gpuFrame.Bounds())
			}

			// ---- 逐像素比对, 阈值 0 ----
			diff := 0
			firstDiff := image.Point{-1, -1}
			b := cpuFrame.Bounds()
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					if cpuFrame.RGBAAt(x, y) != gpuFrame.RGBAAt(x, y) {
						diff++
						if firstDiff.X < 0 {
							firstDiff = image.Point{X: x, Y: y}
						}
					}
				}
			}
			st := gs.Stats()
			t.Logf("%s: %dx%d, CPU %d 色 / %d 墨 | GPU 帧统计 整帧 %d / 局部 %d | 差异像素 %d (首个 %v)",
				name, b.Dx(), b.Dy(), cpuInk.Colors, cpuInk.Ink, st.FullFrames, st.RegionFrames, diff, firstDiff)
			if diff != 0 {
				t.Fatalf("GPU 呈现路径与 CPU 帧缓冲不一致 (%d 像素, 首个差异 %v) —— "+
					"合成 quad 只该搬运像素, 任何非零差异都是 GPU 轨的 bug", diff, firstDiff)
			}
		})
	}
}

// runAndSettleFactory 用 snapshotFactory 按脚本 <window> 声明尺寸开面并驱动
// 到稳定态。CPU 与 GPU 两条路径都走它 —— 双轨必须用同一套尺寸/轮次, 否则
// 比对口径先就不成立。注释里说的「CPU 路径直接用 runAndSettle」不成立:
// runAndSettle 用的是固定 400x300 的 factory, 快照脚本的画幅各不相同,
// 固定尺寸会把大组件裁掉一半 (见 snapshotFactory)。
func runAndSettleFactory(t *testing.T, load func() (*vm.VM, error), s gfx.Surface, inner *captureSurface) {
	t.Helper()
	object.GlobalScheduler().ClearAll()
	t.Cleanup(func() { object.GlobalScheduler().ClearAll() })

	gfx.SetDefaultFactory(&snapshotFactory{inner: inner, s: s})
	t.Cleanup(func() { gfx.SetDefaultFactory(nil) })

	v, err := load()
	if err != nil {
		t.Fatalf("EvalFileVM: %v", err)
	}

	round := 0
	pump := func(maxWait time.Duration) bool {
		switch round {
		case 0, 1: // 两轮空转, 与 CPU 路径轮次逐一相同
			inner.push(gfx.Event{Kind: gfx.EventMouseLeave})
		case 2:
			inner.push(gfx.Event{Kind: gfx.EventClose})
		}
		round++
		return gfx.Pump(maxWait)
	}
	if err := v.RunTimersWithPump(pump); err != nil {
		t.Fatalf("RunTimersWithPump: %v", err)
	}
}
