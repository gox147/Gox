package gfx

import (
	"os"
	"testing"

	"golang.org/x/image/math/fixed"
)

// TestBaseFaceLoadReadsOnlyNeededTables 守 T3-5 第十二批的形态: 把字体文件
// 变成"能栅格化出字形"的面, 只该按需读表, 不该把整份字节读进堆。
//
// 为什么用字节数而不是毫秒: 页缓存会让"整份读 19MB"在 CI 上也只要几毫秒,
// 毫秒断言在 runner 上必是噪声; 而"从盘上要了多少字节"是确定量 —— 与
// TestProbeReadsOnlyNameAndPost 同一口径。负向形态很干脆: 一旦退回
// os.ReadFile, 读盘量立刻等于文件大小。
func TestBaseFaceLoadReadsOnlyNeededTables(t *testing.T) {
	path := systemFontOfRank(t, true) // 正体面样本, 该机器没有会 Skip
	resetFontStateForTest(t)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	ent, err := loadFontFileEntry(path)
	if err != nil {
		t.Fatalf("loadFontFileEntry(%s): %v", path, err)
	}
	if ent.reg == nil {
		t.Fatalf("%s: 解析不出正体面", path)
	}

	// 真栅格化二十来个字形 (首帧的典型负载: 三行短文本), 让"取字形"这段
	// 按需读盘也进到统计里 —— 只测建面不测取字, 等于没测热路径。
	face, err := newFaceFrom(ent.reg, 14)
	if err != nil {
		t.Fatalf("newFaceFrom: %v", err)
	}
	for _, r := range "coldstart first-frame probe gox 0123456789" {
		if _, _, _, _, ok := face.Glyph(fixed.P(0, 0), r); !ok {
			t.Logf("字形 %q 缺失 (不影响字节断言)", r)
		}
	}

	got := ent.readBytes.Load()
	t.Logf("%s: 文件 %d 字节, 建面+42 字形读盘 %d 字节 (%.4f%%)",
		st.Name(), st.Size(), got, float64(got)/float64(st.Size())*100)

	// 上限取"文件大小的 5%"与 1MB 的较小者: 本机 CJK 集合字体 19MB → 950KB。
	// 实测值远低于此 (见日志), 留出的余量是给不同 x/image 版本的表读取策略
	// (有的会把 hmtx/loca 整表读进来) —— 但任何"整份读"都必然越界。
	limit := st.Size() / 20
	if limit > 1<<20 {
		limit = 1 << 20
	}
	if got > limit {
		t.Fatalf("建面读盘 %d 字节 > 上限 %d (文件 %d 字节) —— 退回了整份读盘?",
			got, limit, st.Size())
	}
}
