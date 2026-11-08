package gfx

// ===== 折叠屏 demo 的 30 秒门面 GIF 生成器（rwGXAD）=====
//
// 干什么：把 apps/foldable-demo 真的挂起来，用合成事件点四个场景的关键按钮，
// 逐帧取**真实上屏的像素**，拼成一份 30 秒 GIF + 四张静帧。
//
// 为什么不用"开真窗口录屏"：录屏带窗口装饰、桌面背景、DPI 缩放，且同一段
// 交互在 CI 上没法复现。离屏链路走的是同一条光栅化/布局代码，产物是稳定的，
// 也能在无头环境跑（与 gallery_shot_test.go 同一套设施）。
//
// 用法：
//   go test ./gfx -run TestFoldableShowcase -v            # 只跑交互断言（不写文件）
//   GOX_FOLDABLE_SHOTS=docs/foldable-demo go test ./gfx -run TestFoldableShowcase -v
//
// 断言的意义：GIF 只是产物，"同一段代码在四种形态下各自正确"才是要守的东西 ——
// 所以每一档都断言节点树里出现了该形态才有的文案（S1 栏数 / S2 两种 kind /
// S3 Tabletop 分屏 / S4 显示器列表）。

import (
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/vm"
)

type showcaseFrame struct {
	img   *image.RGBA
	delay time.Duration
}

// showcaseClips 是"点哪个按钮 → 取哪一帧"的剧本。时长加起来 ≈ 30s。
type showcaseClip struct {
	name  string
	click string // 点哪个按钮（空 = 不点，直接拍）
	hold  time.Duration
}

var showcaseClips = []showcaseClip{
	{"01-s1-wide", "", 3500 * time.Millisecond},
	{"02-s1-narrow", "窗口 520", 3500 * time.Millisecond},
	{"03-s1-wide-again", "窗口 900", 2000 * time.Millisecond},
	{"04-s2-half", "S2 折痕避让", 1200 * time.Millisecond},
	{"05-s2-half-hold", "", 3300 * time.Millisecond},
	{"06-s2-occlusion", "挖孔屏", 3500 * time.Millisecond},
	{"07-s3-flat", "S3 Tabletop", 3000 * time.Millisecond},
	{"08-s3-tabletop", "半折 · 横", 5000 * time.Millisecond},
	{"09-s3-play", "▶▶", 1500 * time.Millisecond},
	{"10-s4", "S4 跨屏", 3500 * time.Millisecond},
}

// TestFoldableShowcase 交互回放 + 取帧 + 断言。
func TestFoldableShowcase(t *testing.T) {
	object.GlobalScheduler().ClearAll()
	t.Cleanup(func() { object.GlobalScheduler().ClearAll() })
	ResetDisplaysForTest()
	t.Cleanup(ResetDisplaysForTest)

	fake := newFakeSurface()
	SetDefaultFactory(&galleryFactory{fake})
	t.Cleanup(func() { SetDefaultFactory(nil) })

	entry := filepath.Join("..", "apps", "foldable-demo", "src", "main.js")
	v, err := vm.EvalFileVM(entry)
	if err != nil {
		t.Fatalf("挂载 foldable-demo 失败: %v", err)
	}
	a := currentApp()
	if a == nil {
		t.Fatalf("脚本没有挂上窗口")
	}

	// 事件按"一轮一个"投递（fakeSurface 的容量与 WaitEvents 语义所限，
	// 见 gallery_shot_test.go 的同款说明）。
	var steps []func()
	idle := func(n int) {
		for i := 0; i < n; i++ {
			steps = append(steps, func() {})
		}
	}
	click := func(label string) {
		steps = append(steps, func() {
			n := buttonByText(a.root, label)
			if n == nil {
				t.Fatalf("找不到按钮 %q", label)
			}
			if n.Box.W <= 0 || n.Box.H <= 0 {
				t.Fatalf("按钮 %q 没有布局尺寸 (%v)", label, n.Box)
			}
			x, y := centerOf(n)
			fake.push(Event{Kind: EventMouseDown, X: x, Y: y})
		})
		steps = append(steps, func() {
			n := buttonByText(a.root, label)
			x, y := centerOf(n)
			fake.push(Event{Kind: EventMouseUp, X: x, Y: y})
		})
	}

	var frames []showcaseFrame
	capture := func(name string) {
		steps = append(steps, func() {
			img := shotsImage(fake)
			if img == nil {
				t.Fatalf("没拿到帧（渲染链路没走到 Show）")
			}
			frames = append(frames, showcaseFrame{img: img, delay: clipHold(len(frames))})
			assertClipState(t, a.root, name, len(frames)-1)
		})
	}

	// 起手：让 setTimeout(0) 里的 bindWindow 跑完 + 布局稳定。
	idle(6)
	for _, c := range showcaseClips {
		if c.click != "" {
			click(c.click)
			idle(10)
		}
		idle(4)
		capture(c.name)
	}

	// 收尾：关窗让事件循环退出。
	steps = append(steps, func() { fake.push(Event{Kind: EventClose}) })

	round := 0
	pump := func(maxWait time.Duration) bool {
		if round < len(steps) {
			steps[round]()
			round++
			// 假 Surface 在 maxWait<=0 时会睡 10s（真后端是"无限期等事件"），
			// 而离屏回放没有真实时钟要走 —— 统一钳到 20ms，跑得快且语义不变。
			if maxWait > 20*time.Millisecond || maxWait <= 0 {
				maxWait = 20 * time.Millisecond
			}
			return Pump(maxWait)
		}
		return false
	}
	if err := v.RunTimersWithPump(pump); err != nil {
		t.Fatalf("RunTimersWithPump: %v", err)
	}
	if len(frames) != len(showcaseClips) {
		t.Fatalf("帧数 %d 与剧本 %d 不符", len(frames), len(showcaseClips))
	}
	for i, f := range frames {
		st := galleryInkStats(f.img)
		if st.Colors < galleryMinColors || st.Ink < galleryMinInk {
			t.Fatalf("%s: 画面近乎空白 (%s)", showcaseClips[i].name, st)
		}
	}

	out := os.Getenv("GOX_FOLDABLE_SHOTS")
	if out == "" {
		return
	}
	writeShowcaseAssets(t, out, frames)
}

// clipHold 取第 i 帧的展示时长（剧本里写死，保证总长 ≈ 30s）。
func clipHold(i int) time.Duration {
	if i < len(showcaseClips) {
		return showcaseClips[i].hold
	}
	return time.Second
}

// assertClipState 断言该帧对应的形态真的生效了 —— 不是"图看着像"，而是
// 节点树里出现了只有这个形态才会有的文案。
func assertClipState(t *testing.T, root *GuiNode, name string, i int) {
	t.Helper()
	want := map[string]string{
		"01-s1-wide":       "当前 2 栏",
		"02-s1-narrow":     "当前 1 栏",
		"03-s1-wide-again": "当前 2 栏",
		"04-s2-half":       "division 1 段",
		"05-s2-half-hold":  "division 1 段",
		"06-s2-occlusion":  "occlusion 1 处",
		"07-s3-flat":       "当前不是 Tabletop",
		"08-s3-tabletop":   "上段：内容 / 画面",
		"09-s3-play":       "画面 02 / 03",
		"10-s4":            "显示器",
	}[name]
	if want == "" {
		return
	}
	if findText(root, want) == nil {
		t.Fatalf("%s（第 %d 帧）: 节点树里找不到 %q —— 形态/布局没按预期切过去\n实际文案: %v",
			name, i, want, dumpTexts(root))
	}
}

// dumpTexts 列出当前树里所有文本（断言失败时的取证）。
func dumpTexts(root *GuiNode) []string {
	var out []string
	for _, n := range findAll(root, "#text") {
		if s := strings.TrimSpace(n.Text); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// findText 在子树里找文本节点（contains 匹配，动态拼接的文案也能命中）。
func findText(root *GuiNode, sub string) *GuiNode {
	return findFirstWhere(root, func(n *GuiNode) bool {
		return n.Tag == "#text" && strings.Contains(n.Text, sub)
	})
}

// buttonByText 找到"子树里含指定文本"的按钮。
func buttonByText(root *GuiNode, label string) *GuiNode {
	for _, b := range findAll(root, "button") {
		if findText(b, label) != nil {
			return b
		}
	}
	return nil
}

// writeShowcaseAssets 写 4 张静帧 + 1 份 GIF。
func writeShowcaseAssets(t *testing.T, dir string, frames []showcaseFrame) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	stills := map[int]string{
		1: "s1-narrow-single.png",
		5: "s2-occlusion-avoid.png",
		7: "s3-tabletop.png",
		9: "s4-cross-display.png",
	}
	for i, name := range stills {
		if i >= len(frames) {
			continue
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("建文件: %v", err)
		}
		if err := png.Encode(f, frames[i].img); err != nil {
			f.Close()
			t.Fatalf("写 PNG: %v", err)
		}
		f.Close()
		t.Logf("静帧: %s", filepath.Join(dir, name))
	}

	gifPath := filepath.Join(dir, "foldable-30s.gif")
	f, err := os.Create(gifPath)
	if err != nil {
		t.Fatalf("建 GIF: %v", err)
	}
	defer f.Close()
	if err := encodeShowcaseGIF(f, frames); err != nil {
		t.Fatalf("写 GIF: %v", err)
	}
	st, _ := f.Stat()
	t.Logf("GIF: %s (%d KB)", gifPath, st.Size()/1024)
}

// encodeShowcaseGIF 用"全片共享调色板"编码：UI 是大块纯色，只要把出现最多的
// 颜色收进调色板，GIF 就几乎无损；共享调色板还能让相邻帧的差分压缩生效。
func encodeShowcaseGIF(w *os.File, frames []showcaseFrame) error {
	if len(frames) == 0 {
		return nil
	}
	pal := showcasePalette(frames)
	out := &gif.GIF{LoopCount: 0}
	for _, fr := range frames {
		p := image.NewPaletted(fr.img.Bounds(), pal)
		// 逐像素映射到最近色（调色板覆盖了绝大多数纯色，命中即为精确值）。
		idx := map[color.RGBA]uint8{}
		b := fr.img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := fr.img.RGBAAt(x, y)
				v, ok := idx[c]
				if !ok {
					v = nearestPaletteIndex(pal, c)
					idx[c] = v
				}
				p.Pix[(y-b.Min.Y)*p.Stride+(x-b.Min.X)] = v
			}
		}
		d := int(fr.delay / (10 * time.Millisecond))
		if d < 2 {
			d = 2
		}
		out.Image = append(out.Image, p)
		out.Delay = append(out.Delay, d)
	}
	return gif.EncodeAll(w, out)
}

// showcasePalette 收集全片出现最多的 255 种颜色（不足则用灰阶补齐）。
func showcasePalette(frames []showcaseFrame) color.Palette {
	count := map[color.RGBA]int{}
	for _, fr := range frames {
		b := fr.img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y += 2 {
			for x := b.Min.X; x < b.Max.X; x += 2 {
				count[fr.img.RGBAAt(x, y)]++
			}
		}
	}
	type kv struct {
		c color.RGBA
		n int
	}
	list := make([]kv, 0, len(count))
	for c, n := range count {
		list = append(list, kv{c, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	pal := make(color.Palette, 0, 256)
	for _, e := range list {
		if len(pal) >= 255 {
			break
		}
		pal = append(pal, e.c)
	}
	for len(pal) < 256 {
		g := uint8(len(pal))
		pal = append(pal, color.RGBA{g, g, g, 255})
	}
	return pal
}

// nearestPaletteIndex 线性找最近色（256 项 × 像素数，UI 图规模可接受；
// 调用方还会按颜色去重，实际只算一次/色）。
func nearestPaletteIndex(pal color.Palette, c color.RGBA) uint8 {
	best, bestD := 0, 1<<30
	for i, p := range pal {
		r, g, b, _ := p.RGBA()
		dr := int(r>>8) - int(c.R)
		dg := int(g>>8) - int(c.G)
		db := int(b>>8) - int(c.B)
		d := dr*dr + dg*dg + db*db
		if d < bestD {
			bestD, best = d, i
			if d == 0 {
				break
			}
		}
	}
	return uint8(best)
}
