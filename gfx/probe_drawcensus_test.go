//go:build gpu

package gfx

// G-2 探针 · 第二问: FillRect/Draw 的调用形态能否有效合批（离线, 不依赖
// GPU 硬件）。
//
// 方法: 挂载真实 UI 脚本, 拿到已布局的渲染树 (a.root), 手动驱动一次
// Draw(img, root), 用 census.go 的普查钩子逐原语计数。比"整个截图过程
// 混算"干净: 这里的数字是**稳定态单帧**的口径, 正是 GPU 侧每帧要接管的
// 调用量。
//
// 回答四件事 (对应 docs/gpu-raster-probe.md):
//   1. 单帧原语总数: GPU 每帧要提交的 quad 数量级;
//   2. 颜色离散度与"同色相邻"比例: 不换管线/不换绑定能连续提交多少;
//   3. 微矩形占比 (≤2px): 边框/分隔线这类, quad 化没问题, 但要防 scissor
//      粒度过细把 draw call 放大;
//   4. AA 散点 (blendPixel) 占比: 覆盖率抗锯齿是 CPU 逐像素的, GPU 化要么
//      改 coverage shader 要么留在 CPU —— 占比决定它是不是第一阶段就绕不开。
//
// 运行: go test -tags gpu ./gfx/ -run TestProbeDrawCensus -v
//
// 口径: 无 GPU 硬件沙箱 (Linux x86_64); 数字描述的是**调用形态**, 与
// GPU 硬件无关, 可进结论文档; 性能数字另测。

import (
	"fmt"
	"image"
	"image/color"
	"testing"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/vm"
)

type censusAgg struct {
	calls    int
	tiny     int // w<=2 || h<=2 (边框笔画/分隔线/AA散点)
	alpha    int // src-over 半透明 (A<255) —— GPU 混合语义要对齐的那部分
	areaPx   int // Σ w*h (请求面积, 未按 clip 收窄)
	colors   map[string]int
	kinds    map[string]int // 按 kind 的调用数
	kindArea map[string]int // 按 kind 的请求面积
	lastCol  string         // 仅 fillrect 流参与同色段分析 (GPU 矩形批处理口径)
	sameRun  int
	maxRun   int // 最长同色连续段 —— "不换绑定连续提交"的上界信号
	runCalls int // 同色连续段切换次数 = 理论最小 draw call 数 (按颜色)
}

func (a *censusAgg) hook(kind string, w, h int, c color.RGBA, alphaBlend bool) {
	a.calls++
	a.kinds[kind]++
	a.kindArea[kind] += w * h
	if w <= 2 || h <= 2 {
		a.tiny++
	}
	if alphaBlend {
		a.alpha++
	}
	a.areaPx += w * h
	key := fmt.Sprintf("%02x%02x%02x%02x", c.R, c.G, c.B, c.A)
	a.colors[key]++
	if kind != "fillrect" {
		return // 同色段分析只看矩形流
	}
	if key == a.lastCol {
		a.sameRun++
	} else {
		if a.sameRun > a.maxRun {
			a.maxRun = a.sameRun
		}
		a.runCalls++
		a.sameRun = 1
		a.lastCol = key
	}
}

// censusOneFrame 挂载 src 并对稳定态做单帧普查。winW/winH 是窗口尺寸,
// 必须与脚本里 <window> 一致 (img 是同尺寸的独立帧缓冲)。
func censusOneFrame(t *testing.T, name, src string, winW, winH int) censusAgg {
	t.Helper()
	object.GlobalScheduler().ClearAll()
	t.Cleanup(func() { object.GlobalScheduler().ClearAll() })

	SetDefaultFactory(fakeFactoryForCensus{w: winW, h: winH})
	t.Cleanup(func() { SetDefaultFactory(nil) })

	v, err := vm.EvalVM(src)
	if err != nil {
		t.Fatalf("%s: EvalVM: %v", name, err)
	}
	a := currentApp()
	if a == nil || a.root == nil {
		t.Fatalf("%s: 脚本没有挂上窗口", name)
	}

	img := image.NewRGBA(image.Rect(0, 0, winW, winH))
	var agg censusAgg
	agg.colors = map[string]int{}
	agg.kinds = map[string]int{}
	agg.kindArea = map[string]int{}
	prev := drawCensusHook
	drawCensusHook = agg.hook
	defer func() { drawCensusHook = prev }()

	Draw(img, a.root) // 稳定态单帧, 全帧口径
	_ = v
	if agg.maxRun < agg.sameRun {
		agg.maxRun = agg.sameRun
	}
	return agg
}

type fakeFactoryForCensus struct{ w, h int }

func (f fakeFactoryForCensus) Create(cfg WindowConfig) (Surface, error) {
	s := newFakeSurface()
	s.w, s.h = f.w, f.h
	return s, nil
}

func reportCensus(t *testing.T, name string, agg censusAgg, winW, winH int) {
	t.Helper()
	screen := winW * winH
	t.Logf("%s: 原语 %d (按 kind: fillrect=%d glyph=%d aapixel=%d), 请求面积 %.2f×全帧 (过绘因子)",
		name, agg.calls, agg.kinds["fillrect"], agg.kinds["glyph"], agg.kinds["aapixel"],
		float64(agg.areaPx)/float64(screen))
	t.Logf("%s: 微矩形 %d (%.1f%%), 半透明 %d (%.1f%%); fillrect 面积 %.3f×全帧, aapixel 散点 %d px (%.2f×全帧)",
		name, agg.tiny, pctOf(agg.tiny, agg.calls), agg.alpha, pctOf(agg.alpha, agg.calls),
		float64(agg.kindArea["fillrect"])/float64(screen), agg.kindArea["aapixel"],
		float64(agg.kindArea["aapixel"])/float64(screen))
	t.Logf("%s: 颜色种类 %d, fillrect 同色段切换 %d (理论最小 draw call), 最长同色段 %d",
		name, len(agg.colors), agg.runCalls, agg.maxRun)
}

func pctOf(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func TestProbeDrawCensus(t *testing.T) {
	if _, err := fontFace(16); err != nil {
		t.Skip("无可用字体, 绘制普查探针跳过 (文本口径缺失)")
	}

	const w, h = 480, 360

	// 场景 1: 表单页 —— Prism 组件全景的浓缩 (按钮/输入/勾选/滑杆/标签/文本)。
	form := `
import { h, render } from "gx/gfx";
render(
  <window title="census-form" width={480} height={360}>
    <column gap={10} style={{padding: 14}}>
      <row gap={8}>
        <button>保存</button>
        <button kind="primary">提交</button>
        <button kind="danger" disabled={true}>删除</button>
      </row>
      <input placeholder="请输入名称" value="元构光年" />
      <row gap={8}>
        <checkbox checked={true}>记住我</checkbox>
        <switch checked={true} />
      </row>
      <slider value={60} />
      <progress value={72} />
      <row gap={6}>
        <tag kind="success">运行中</tag>
        <tag kind="warning">待审核</tag>
        <tag kind="danger">已熔断</tag>
      </row>
      <text>软件光栅化 → GPU 合成: 中文 CJK 字形是文本阶段的第一个坑</text>
    </column>
  </window>
);
`

	// 场景 2: 数据面板 —— 列表/表格型 UI, 边框分隔线密集 (微矩形大户)。
	dash := `
import { h, render } from "gx/gfx";
render(
  <window title="census-dash" width={480} height={360}>
    <column gap={0} style={{padding: 0}}>
      <row style={{padding: 10, background: "#1e293b"}}>
        <text style={{color: "#f8fafc", bold: true}}>集群概览</text>
      </row>
      {Array.from({length: 8}, (_, i) => (
        <row gap={8} style={{padding: 8, border: true}}>
          <text width={120}>节点 node-{i}</text>
          <progress value={40 + i * 7} />
          <tag kind={i % 3 == 0 ? "success" : i % 3 == 1 ? "warning" : "danger"}>
            {["正常", "降级", "熔断"][i % 3]}
          </tag>
        </row>
      ))}
    </column>
  </window>
);
`

	for _, sc := range []struct {
		name string
		src  string
	}{{"表单页", form}, {"数据面板", dash}} {
		agg := censusOneFrame(t, sc.name, sc.src, w, h)
		reportCensus(t, sc.name, agg, w, h)
	}
}
