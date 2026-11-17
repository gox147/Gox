//go:build gpu

package gfx

// G-2 探针 · 第一问: CJK 字形图集的纹理内存与失效策略（离线测算, 不依赖
// GPU 硬件 —— 光栅化走的就是 CPU 侧 fontFace, 与 GPU 时代上传纹理的输入
// 完全同源）。
//
// 回答三件事:
//  1. 量级: 当前 fallback 链下 CJK 全块 (U+4E00..U+9FFF) + ASCII + CJK 标点
//     有多少可用字形 (mask 非空), 覆盖率多少;
//  2. 内存: 把这些字形按 1px 间隔装箱进 2048×2048 R8 图集页要几页几 MB,
//     UI 常用字号 (14/16/20/28) 各一遍;
//  3. 失效压力: 现有 glyphLRU 容量 1024 条 vs 上述量级的淘汰比 ——
//     "LRU 直接当图集缓存"是否成立。
//
// 运行: go test -tags gpu ./gfx/ -run TestProbeGlyphAtlasMemory -v
//
// 口径: 本探针在无 GPU 硬件沙箱 (Linux x86_64) 运行, 但**数字与硬件无关**
// —— 它测的是"GPU 文本阶段要喂进去的字形位图从哪来、有多大", 输入就是
// 现有 CPU 光栅化产物。结论进 docs/gpu-raster-probe.md。

import (
	"sort"
	"testing"
	"time"
)

// glyphBox 是装箱用的字形位图尺寸 (含 1px 双向 padding —— 图集里字形之间
// 必须 seep 隔离, 否则双线性采样会串色)。
type glyphBox struct {
	w, h int
}

// packShelf 贪心 shelf 装箱: 按 高度降序 排布, 行内从左到右, 行满换行,
// 页满换页。返回 (页数, 最后一页占用字节)。2048×2048 R8 = 4 MiB/页。
func packShelf(boxes []glyphBox, pageW, pageH int) (pages int, lastBytes int) {
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].h > boxes[j].h })
	pages, lastBytes = 1, 0
	x, y, rowH := 0, 0, 0
	for _, b := range boxes {
		if x+b.w > pageW {
			y += rowH
			x, rowH = 0, 0
		}
		if y+b.h > pageH {
			pages++
			x, y, rowH, lastBytes = 0, 0, 0, 0
		}
		x += b.w
		if b.h > rowH {
			rowH = b.h
		}
		lastBytes = (y + rowH) * pageW
	}
	return pages, lastBytes
}

func TestProbeGlyphAtlasMemory(t *testing.T) {
	if _, err := fontFace(16); err != nil {
		t.Skip("无可用字体, 字形图集探针跳过")
	}

	const (
		uroFirst, uroLast = 0x4E00, 0x9FFF // CJK 统一表意文字主块
		pageW, pageH      = 2048, 2048
	)

	// 1. 覆盖率普查: URO 全块 + ASCII 可打印 + CJK 标点/全角形式。
	covered := []rune{}
	uroTotal, uroCovered := 0, 0
	for r := rune(uroFirst); r <= rune(uroLast); r++ {
		uroTotal++
		if e, err := glyphFor(TextStyle{Size: 16}, r); err == nil && e.mask != nil {
			covered = append(covered, r)
			uroCovered++
		}
	}
	for r := rune(0x20); r <= rune(0x7E); r++ {
		if e, err := glyphFor(TextStyle{Size: 16}, r); err == nil && e.mask != nil {
			covered = append(covered, r)
		}
	}
	for r := rune(0x3000); r <= rune(0x303F); r++ { // CJK 标点
		if e, err := glyphFor(TextStyle{Size: 16}, r); err == nil && e.mask != nil {
			covered = append(covered, r)
		}
	}
	for r := rune(0xFF00); r <= rune(0xFFEF); r++ { // 全角形式
		if e, err := glyphFor(TextStyle{Size: 16}, r); err == nil && e.mask != nil {
			covered = append(covered, r)
		}
	}
	uroCov := float64(uroCovered) / float64(uroTotal) * 100
	t.Logf("fallback 链覆盖: URO 主块 %d/%d (%.1f%%) + ASCII/CJK标点/全角, 可用字形合计 %d",
		uroCovered, uroTotal, uroCov, len(covered))
	if uroCov < 50 {
		t.Skipf("当前环境字体 CJK 覆盖率 %.1f%% < 50%%, 测出来的图集量级不代表中文场景, 跳过", uroCov)
	}

	// 2. 图集内存测算: UI 常用字号逐个装箱。
	sizes := []int{14, 16, 20, 28}
	var sumPages int
	var sumMB float64
	var buildMS int64
	for _, size := range sizes {
		start := time.Now()
		boxes := make([]glyphBox, 0, len(covered))
		var maxW, maxH, totalBytes int
		for _, r := range covered {
			e, err := glyphFor(TextStyle{Size: size}, r)
			if err != nil || e.mask == nil {
				continue
			}
			b := e.mask.Bounds()
			// 1px padding: 图集装箱相邻字形必须隔离 (双线性采样串色)
			boxes = append(boxes, glyphBox{b.Dx() + 1, b.Dy() + 1})
			totalBytes += (b.Dx() + 1) * (b.Dy() + 1)
			if b.Dx() > maxW {
				maxW = b.Dx()
			}
			if b.Dy() > maxH {
				maxH = b.Dy()
			}
		}
		elapsed := time.Since(start).Milliseconds()
		if size == 16 {
			buildMS = elapsed
		}
		pages, _ := packShelf(boxes, pageW, pageH)
		mb := float64(totalBytes) / (1 << 20)
		sumPages += pages
		sumMB += mb
		t.Logf("size=%2d: 字形 %d 个, 位图合计 %.2f MiB (最大 %dx%d), 2048² R8 图集 %d 页, 光栅化耗时 %dms",
			size, len(boxes), mb, maxW, maxH, pages, elapsed)
	}

	// 3. 失效压力: glyphLRU 容量 1024 (font.go glyphLRU = newGlyphCache(1024))。
	const lruCap = 1024
	// 中文 UI 实际同时驻留: 一屏常见 ~200-600 个不同汉字 × 2-3 个字号。
	typicalLive := 500 * 2
	t.Logf("失效压力: 全块×4字号 = %d 条 (LRU 容量 %d, 超容 %.1f×); 中文一屏典型驻留 ~%d 条 (%.1f× 容量)",
		len(covered)*len(sizes), lruCap,
		float64(len(covered)*len(sizes))/float64(lruCap),
		typicalLive, float64(typicalLive)/float64(lruCap))

	t.Logf("汇总: 全字号图集合计 %d 页 (%.2f MiB 位图 / %.2f MiB 页分配), size=16 全块建图 %dms",
		sumPages, sumMB, float64(sumPages)*4, buildMS)

	// 参考基准: 500 常用字 × 2 字号的"典型驻留"图集。
	boxes := make([]glyphBox, 0, 500*2)
	for _, r := range covered[:minInt(500, len(covered))] {
		for _, size := range []int{14, 16} {
			e, err := glyphFor(TextStyle{Size: size}, r)
			if err != nil || e.mask == nil {
				continue
			}
			b := e.mask.Bounds()
			boxes = append(boxes, glyphBox{b.Dx() + 1, b.Dy() + 1})
		}
	}
	pages, _ := packShelf(boxes, pageW, pageH)
	t.Logf("典型驻留 (前 500 汉字 × 14/16 两档): %d 字形, %d 页 (%.2f MiB)", len(boxes), pages, float64(pages)*4)
}
