package gfx

// T3-5 第十一批: 判面/族索引改走"极小 sfnt 读取器"(gfx/font_probe.go) 的闸门。
//
// 三条断言, 各守一件事:
//
//  1. TestProbeReadsOnlyNameAndPost —— 成本形态。探针读一个 19MB 的 CJK 集合
//     字体不能超过几百 KB: 一旦有人在探针里加回整份解析, 毫秒断言在 CI 上看
//     不出来 (页缓存让整份读也显得便宜), 字节数看得出来。
//  2. TestProbeNamesMatchFullParse —— 口径等价。名字/轴/等宽标记必须与整份
//     解析逐面一致, 否则族索引会静默把字重登记错 (加粗退化成合成, 或默认
//     字体选成粗体)。
//  3. BenchmarkProbeFileFaceRank —— 给下一批留一个可复跑的耗时数字; 不进闸门
//     (绝对毫秒在 CI 上不可移植)。
//
// 三条都只依赖"这台机器上有没有字体": 没有就跳过, 换机器不假红。

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// probeTestFonts 返回本机可用的候选 (按文件大小降序, 集合字体排在前面 ——
// "10 个面"的代价只有集合字体才看得出来)。
func probeTestFonts(t *testing.T) []string {
	t.Helper()
	initFontCandidates()
	seen := map[string]bool{}
	var out []string
	for _, p := range fontCandidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		si, _ := os.Stat(out[i])
		sj, _ := os.Stat(out[j])
		return si.Size() > sj.Size()
	})
	if len(out) == 0 {
		t.Skip("当前机器没有系统字体, 跳过")
	}
	return out
}

// TestProbeReadsOnlyNameAndPost 断言探针的读盘量。
//
// 阈值 512KB: 实测 10 面 CJK 集合字体 ~80KB, 单面西文 ~10KB。留 6 倍余量是
// 为了容纳"name 表特别大"的怪字体, 但离 19MB 的整份读还有 37 倍 —— 足够把
// "退回 col.Font(i)" 这种改动挡住。
func TestProbeReadsOnlyNameAndPost(t *testing.T) {
	fonts := probeTestFonts(t)
	p := fonts[0]
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	var got int64
	var calls int
	fontProbeBytesHook = func(_ string, n int) { calls++; got += int64(n) }
	t.Cleanup(func() { fontProbeBytesHook = nil })

	if _, ok := probeFileFaceRank(p); !ok {
		t.Fatalf("%s: 探针判为不可用", filepath.Base(p))
	}
	if calls != 1 {
		t.Fatalf("探针钩子触发 %d 次, 期望 1 次", calls)
	}
	if got > 512<<10 {
		t.Errorf("%s: 探针读了 %d 字节, 超过 512KB 上限 (整份文件 %d 字节; 判面只需 name/post 表头)",
			filepath.Base(p), got, st.Size())
	}
	if got == 0 {
		t.Errorf("%s: 探针读了 0 字节, 钩子没接上", filepath.Base(p))
	}
	t.Logf("探针 %s: 读 %d 字节 / 文件 %d 字节 (%.4f%%)",
		filepath.Base(p), got, st.Size(), float64(got)*100/float64(st.Size()))
}

// TestProbeNamesMatchFullParse 用整机字体核对"探针 vs 整份解析"的口径。
//
// 比 TestProbeFileFaceRankMatchesFullParse 更细: 那条只看"最好的面是几档",
// 这条逐个面比 family / typographic family / subfamily / 轴 / isFixedPitch ——
// 族索引登记面用的就是这五项。
func TestProbeNamesMatchFullParse(t *testing.T) {
	fonts := probeTestFonts(t)
	checked, limit := 0, 40
	var buf sfnt.Buffer
	for _, p := range fonts {
		if checked >= limit {
			break
		}
		raw, err := fontFileFaces(p)
		if err != nil {
			continue // 坏文件: 另一条断言 (TestProbeFileFaceRankMatchesFullParse) 管
		}
		// fontFileFaces 对"整份解析失败的面"留 nil 占位; 探针看不到 cmap 里
		// 的格式错误, 所以两边面数可能不同 —— 只比两边都认得的那些面, 按
		// 顺序对齐。整份解析能读而探针读不出 (或反之) 的情况交给 rank 那条
		// 断言管方向, 这里不做面数硬判。
		var want []*opentype.Font
		for _, f := range raw {
			if f != nil {
				want = append(want, f)
			}
		}
		got, err := probeFontFaces(p)
		if err != nil {
			t.Errorf("%s: 探针失败: %v", filepath.Base(p), err)
			continue
		}
		if len(got) != len(want) {
			continue // 面集合不同 (见上), 无可比对象
		}
		for i := range want {
			ft := want[i]
			sub := fontNameOf(ft, &buf, sfnt.NameIDSubfamily)
			if got[i].subfamily != sub {
				t.Errorf("%s 面%d: 子族名 探针=%q 整份=%q", filepath.Base(p), i, got[i].subfamily, sub)
			}
			if normalizeFamilyKey(got[i].family) != normalizeFamilyKey(fontNameOf(ft, &buf, sfnt.NameIDFamily)) {
				t.Errorf("%s 面%d: 族名不一致", filepath.Base(p), i)
			}
			if normalizeFamilyKey(got[i].typoFamily) != normalizeFamilyKey(fontNameOf(ft, &buf, sfnt.NameIDTypographicFamily)) {
				t.Errorf("%s 面%d: 排版族名不一致", filepath.Base(p), i)
			}
			if wantAxis := axisFromFont(ft, i, sub); got[i].subfamily == sub && axisFromSubfamily(got[i].subfamily, got[i].italicAngle) != wantAxis {
				t.Errorf("%s 面%d: 轴 探针=%+v 整份=%+v (angle=%v)",
					filepath.Base(p), i, axisFromSubfamily(got[i].subfamily, got[i].italicAngle), wantAxis, got[i].italicAngle)
			}
			mono := false
			if pt := ft.PostTable(); pt != nil {
				mono = pt.IsFixedPitch
			}
			if mono != got[i].fixedPitch {
				t.Errorf("%s 面%d: isFixedPitch 探针=%v 整份=%v", filepath.Base(p), i, got[i].fixedPitch, mono)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Skip("当前机器没有可解析的系统字体, 跳过")
	}
	t.Logf("探针/整份解析逐面一致: %d 个文件", checked)
}

// BenchmarkProbeFileFaceRank 量探针本身的成本 (选最大的那个字体文件:
// 10 面的 CJK 集合字体是探针最贵的情形)。
func BenchmarkProbeFileFaceRank(b *testing.B) {
	initFontCandidates()
	var best string
	var bestSize int64
	seen := map[string]bool{}
	for _, p := range fontCandidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil || st.Size() <= bestSize {
			continue
		}
		best, bestSize = p, st.Size()
	}
	if best == "" {
		b.Skip("当前机器没有系统字体")
	}
	b.SetBytes(bestSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := probeFileFaceRank(best); !ok {
			b.Fatalf("%s: 探针判为不可用", best)
		}
	}
}

// TestHasRequiredTables 钉住"哪些面在整份解析里算存在"。
//
// 这条是被 CI 逼出来的: 第一版把 loca+glyf 写成 TrueType 的唯一字形来源,
// ubuntu runner 上的 NotoColorEmoji.ttf (只有 CBDT/CBLC, glyf 为空) 就被探针
// 判成"不可用"、却被整份解析读了出来 —— 两条路口径当场分叉。字形来源有三种,
// 必须与 sfnt.parseGlyphData 的分支一一对应。
func TestHasRequiredTables(t *testing.T) {
	tr := func(tags ...string) map[string]bool {
		m := map[string]bool{}
		for _, s := range tags {
			m[s] = true
		}
		return m
	}
	base := []string{"head", "maxp", "cmap", "hhea", "hmtx", "post"}
	with := func(extra ...string) map[string]bool { return tr(append(append([]string{}, base...), extra...)...) }
	tt := []byte{0, 1, 0, 0} // 0x00010000
	otto := []byte("OTTO")

	cases := []struct {
		name string
		have map[string]bool
		ver  []byte
		want bool
	}{
		{"TrueType loca+glyf", with("loca", "glyf"), tt, true},
		{"彩色位图 CBLC/CBDT (glyf 为空)", with("CBLC", "CBDT"), tt, true},
		{"OTTO + CFF", with("CFF "), otto, true},
		{"OTTO + CFF2", with("CFF2"), otto, true},
		{"缺 post", tr("head", "maxp", "cmap", "hhea", "hmtx", "loca", "glyf"), tt, false},
		{"缺 cmap", tr("head", "maxp", "hhea", "hmtx", "post", "loca", "glyf"), tt, false},
		{"TrueType 但既无 loca/glyf 也无 CBLC", with(), tt, false},
		{"OTTO 但没有 CFF/CFF2", with("loca", "glyf"), otto, false},
		{"name 缺失不影响可用性", tr("head", "maxp", "cmap", "hhea", "hmtx", "post", "loca", "glyf"), tt, true},
	}
	for _, c := range cases {
		if got := hasRequiredTables(c.have, c.ver); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
