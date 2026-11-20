//go:build linux

package x11

// ===== 上屏像素搬运的闸门 (T3-5 第二十六批, 2026-11-21 凌晨批) =====
//
// 被测函数 RGBAToXZPixmap 在**每一帧**的 ShowRegions 里跑一次 (每行带一次),
// 是 X11 后端唯一一处"逐像素"的循环。它的输出是直接过 socket 交给 X 服务器的
// 字节流 —— 错一个通道就是整屏偏色, 且**没有任何上层测试看得见** (图像比对测试
// 走的是 Draw 之后的 RGBA 缓冲, 不经过这条转换)。
//
// 所以这里用"旧实现当基准"的方式钉住: referenceSwizzle 是改动前那份逐字节
// 实现的原样拷贝, 新实现必须在**任意宽度 / 任意子矩形 / 任意行跨度**下与它
// 逐字节一致。这比"手写几个期望值"强: 手写的那几个探针只覆盖 1x1 与 2x2,
// 而按 32 位字重排的实现在"宽度为奇数 / 子矩形起点不在行首 / 行跨度带 padding"
// 这三类边界上最容易错位。
//
// 为什么同时留 BenchmarkRGBAToXZPixmap320x200: 首帧窗口就是 320x200 (见
// scripts/bench/coldstart_gui.js), 这条基准对应首帧上屏那一段的绝对成本;
// 4K 的数字由 gfx/present_bench_test.go 的端到端基准回答, 两者不重复。

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// referenceSwizzle 是改动前的实现 (逐字节), 作为等价性基准。
// 刻意保留"逐字节"写法而不是调用新实现 —— 否则闸门自己就先绿了。
func referenceSwizzle(img *image.RGBA, r image.Rectangle) []byte {
	w := r.Dx()
	buf := make([]byte, w*r.Dy()*4)
	for y := 0; y < r.Dy(); y++ {
		src := img.Pix[(r.Min.Y+y)*img.Stride:]
		dst := buf[y*w*4:]
		for x := 0; x < w; x++ {
			i := (r.Min.X + x) * 4
			j := x * 4
			dst[j] = src[i+2]   // B
			dst[j+1] = src[i+1] // G
			dst[j+2] = src[i]   // R
			dst[j+3] = 0        // unused
		}
	}
	return buf
}

// TestRGBAToXZPixmapMatchesReference 是主闸门: 随机图 × 各种形状。
//
// 覆盖矩阵:
//   - 宽度 1..16 (含 1/3/5 这类奇数, 按 32 位字重排最容易在尾部错位)
//   - 高度 1..4
//   - 整图 / 左上偏移子矩形 / 贴右下角子矩形 / 单像素
//   - 父图带"负起点"的 Bounds (image.Rect(7,9,...) 这种): 证明实现只用
//     img.Pix 与 r, 不假设 r.Min 是 (0,0)
func TestRGBAToXZPixmapMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20261121))

	// case 1: 整图, 宽度 1..16
	for w := 1; w <= 16; w++ {
		for h := 1; h <= 4; h++ {
			img := randomRGBA(rng, image.Rect(0, 0, w, h))
			got := RGBAToXZPixmap(img, img.Bounds())
			want := referenceSwizzle(img, img.Bounds())
			if !bytes.Equal(got, want) {
				t.Fatalf("%dx%d 整图不一致:\n got=%v\nwant=%v", w, h, got, want)
			}
		}
	}

	// case 2: 子矩形 (含奇数宽 / 偏移起点 / 贴右下角)
	img := randomRGBA(rng, image.Rect(0, 0, 17, 9))
	subs := []image.Rectangle{
		image.Rect(1, 1, 6, 5),   // 奇数宽 5, 偏移起点
		image.Rect(0, 0, 1, 1),   // 单像素
		image.Rect(16, 8, 17, 9), // 右下角单像素
		image.Rect(3, 2, 17, 9),  // 宽 14, 贴右下
		image.Rect(9, 0, 10, 9),  // 宽 1, 整列
	}
	for _, r := range subs {
		got := RGBAToXZPixmap(img, r)
		want := referenceSwizzle(img, r)
		if !bytes.Equal(got, want) {
			t.Fatalf("子矩形 %v 不一致:\n got=%v\nwant=%v", r, got, want)
		}
	}

	// case 3: 父图 Bounds 不在原点 —— 这里**故意不**和 referenceSwizzle 比:
	// 旧实现把 r.Min 当"从 0 开始的行列号", 对这种图直接越界 panic (本闸门第一版
	// 就是这么撞出来的)。新实现按 img.Bounds().Min 归一, 所以这一条用手算期望值。
	// 留痕见 docs/performance.md §12.27.4。
	img2 := image.NewRGBA(image.Rect(7, 9, 18, 15)) // 11x6
	for i := range img2.Pix {
		img2.Pix[i] = 0
	}
	// 左上角 (7,9) 写纯红, 右下角 (17,14) 写纯蓝
	img2.SetRGBA(7, 9, color.RGBA{R: 0xFF, A: 0xFF})
	img2.SetRGBA(17, 14, color.RGBA{B: 0xFF, A: 0xFF})
	got2 := RGBAToXZPixmap(img2, img2.Bounds())
	if len(got2) != 11*6*4 {
		t.Fatalf("非零原点整图长度 = %d, want %d", len(got2), 11*6*4)
	}
	if !bytes.Equal(got2[0:4], []byte{0, 0, 0xFF, 0}) {
		t.Fatalf("非零原点左上像素 = %v, want [0 0 255 0] (红→BGR)", got2[0:4])
	}
	last := (11*6 - 1) * 4
	if !bytes.Equal(got2[last:last+4], []byte{0xFF, 0, 0, 0}) {
		t.Fatalf("非零原点右下像素 = %v, want [255 0 0 0] (蓝→BGR)", got2[last:last+4])
	}
}

// TestRGBAToXZPixmapDropsAlpha 钉住"第四字节恒为 0"这条独立约定:
// 源图的 alpha 不论是什么, 输出第 4 字节必须是 0 (X 24 位深度下 unused)。
// 如果哪天有人把 BGRX 改成 BGRA (深 32 位), 这条会立刻红 —— 那是另一套
// 视觉语义, 不该由一个"加速"提交顺手改掉。
func TestRGBAToXZPixmapDropsAlpha(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3, 1))
	img.SetRGBA(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 0xFF})
	img.SetRGBA(1, 0, color.RGBA{R: 4, G: 5, B: 6, A: 0x00})
	img.SetRGBA(2, 0, color.RGBA{R: 7, G: 8, B: 9, A: 0x80})
	data := RGBAToXZPixmap(img, img.Bounds())
	for i := 0; i < 3; i++ {
		if data[i*4+3] != 0 {
			t.Fatalf("第 %d 个像素的 unused 字节 = %d, want 0", i, data[i*4+3])
		}
	}
	if !bytes.Equal(data, []byte{3, 2, 1, 0, 6, 5, 4, 0, 9, 8, 7, 0}) {
		t.Fatalf("BGRA→BGRX 输出 = %v", data)
	}
}

// TestRGBAToXZPixmapEmpty 空区域: 不 panic, 返回 0 字节。
// (ShowRegions 会先 Intersect/Empty 过滤, 但这是导出函数, 边界自己守住。)
func TestRGBAToXZPixmapEmpty(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	// 注意: image.Rect 会把 x0>x1 归一成空矩形**之外**的正常矩形 (它会交换端点),
	// 所以"空"必须直接用 Rectangle 字面量表达, 否则这个测试自己就是假绿。
	empty := []image.Rectangle{
		{Min: image.Pt(2, 2), Max: image.Pt(2, 4)}, // 宽 0
		{Min: image.Pt(1, 1), Max: image.Pt(3, 1)}, // 高 0
	}
	for _, r := range empty {
		if got := RGBAToXZPixmap(img, r); len(got) != 0 {
			t.Fatalf("%v → %d 字节, want 0", r, len(got))
		}
	}
}

func randomRGBA(rng *rand.Rand, rect image.Rectangle) *image.RGBA {
	if rect.Dx() <= 0 || rect.Dy() <= 0 {
		rect = image.Rect(0, 0, 1, 1)
	}
	img := image.NewRGBA(rect)
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	return img
}

// BenchmarkRGBAToXZPixmap320x200 是首帧窗口尺寸下的纯转换成本。
func BenchmarkRGBAToXZPixmap320x200(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	img := randomRGBA(rng, image.Rect(0, 0, 320, 200))
	b.SetBytes(320 * 200 * 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(RGBAToXZPixmap(img, img.Bounds())) != 320*200*4 {
			b.Fatal("bad len")
		}
	}
}

// BenchmarkRGBAToXZPixmapInto320x200 是**上屏热路径真正走的那条**:
// 复用缓冲, 每帧不分配。与上面那条的差额就是"每帧 make 一次 + 交给 GC"的钱。
func BenchmarkRGBAToXZPixmapInto320x200(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	img := randomRGBA(rng, image.Rect(0, 0, 320, 200))
	buf := make([]byte, 320*200*4)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(rgbaToXZPixmapInto(img, img.Bounds(), buf)) != len(buf) {
			b.Fatal("bad len")
		}
	}
}
