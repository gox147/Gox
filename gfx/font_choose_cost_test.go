package gfx

// 「选默认字体」的读盘纪律闸门 (T3-5 第十批)。
//
// 被守的形态: chooseBaseFont 逐个候选整份解析时, 排在正体前面的那个 19MB
// CJK 集合字体 (Linux 上恒为 NotoSansCJK-Bold.ttc) 会被完整读进堆, 只为得出
// "它是粗体, 跳过" 这个结论 —— 每次启动白读 19MB (~6ms)。本批先探针 (只读
// 表目录 + name 表, ~88KB) 再决定要不要整份读。
//
// 为什么用"读盘文件数 / 读盘字节"而不是耗时: 耗时依赖页缓存与机器负载, 在
// CI 上不稳定; 而"为了挑字面读了几份文件的全部字节"是确定性的结构事实。
//
// 跑法:
//
//	go test ./gfx/ -run 'TestChooseBaseFont|TestProbeFileFaceRank' -v -count=1

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/font"
)

// fontProbeReadHook / fontFullReadHook 之外的一层计数: 本用例只关心
// "整份解析 (parseFontFile) 被调用了多少次、读了多少字节", 因此用
// fontReadHook 数文件、用被读文件的大小累加字节。
type chooseReadRecorder struct {
	dir   string
	files map[string]int
	bytes int64
}

func newChooseReadRecorder(dir string) *chooseReadRecorder {
	r := &chooseReadRecorder{dir: dir, files: map[string]int{}}
	fontReadHook = func(path string) {
		if filepath.Dir(path) != dir {
			return
		}
		fontFileMu.Lock() // 与缓存写入共用同一把锁, 计数天然串行
		r.files[path]++
		if st, err := os.Stat(path); err == nil {
			r.bytes += st.Size()
		}
		fontFileMu.Unlock()
	}
	return r
}

func (r *chooseReadRecorder) stop() { fontReadHook = nil }

// resetFontStateForTest 把字体子系统恢复到"本次启动刚开始"的状态。
func resetFontStateForTest(t *testing.T) {
	t.Helper()
	fontMu.Lock()
	baseFont = nil
	faceBySize = map[int]font.Face{}
	fontMu.Unlock()
	scannedFonts = nil
	resetFontCaches()
	t.Cleanup(func() {
		fontMu.Lock()
		baseFont = nil
		faceBySize = map[int]font.Face{}
		fontMu.Unlock()
		scannedFonts = nil
		resetFontCaches()
		fontReadHook = nil
	})
}

// systemFontOfRank 找系统里第一个被评为 rank 档位的字体 (rank==0 为正体,
// rank>0 为粗体/斜体等非正体)。找不到就跳过 —— 该机器没有可用的对照样本。
func systemFontOfRank(t *testing.T, wantZero bool) string {
	t.Helper()
	initFontCandidates()
	for _, p := range fontCandidates {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if _, err := parseFontFile(p); err != nil {
			continue
		}
		rank, ok := probeFileFaceRank(p)
		if !ok {
			continue
		}
		if (rank == 0) == wantZero {
			return p
		}
	}
	if wantZero {
		t.Skip("当前机器没有正体系统字体, 跳过")
	}
	t.Skip("当前机器只有正体系统字体 (没有粗体/斜体样本), 跳过")
	return ""
}

// TestChooseBaseFontReadsOnlyChosenFile 是本批的核心闸门:
// 候选表 = [粗体, 正体], 选出来的必须是正体, 且**整份读盘只有 1 个文件**。
//
// 改动前该断言红: 两个候选都会被整份读 (粗体为了被否掉, 正体为了被选中)。
func TestChooseBaseFontReadsOnlyChosenFile(t *testing.T) {
	boldPath := systemFontOfRank(t, false)
	plainPath := systemFontOfRank(t, true)
	boldData, err := os.ReadFile(boldPath)
	if err != nil {
		t.Fatalf("读取粗体样本: %v", err)
	}
	plainData, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("读取正体样本: %v", err)
	}

	dir := t.TempDir()
	// 文件名保序: 粗体在前 (与 Linux 上 NotoSansCJK-Bold 排在 Regular 之前同形)。
	bold := filepath.Join(dir, "a_bold.ttf")
	plain := filepath.Join(dir, "b_plain.ttf")
	if err := os.WriteFile(bold, boldData, 0o644); err != nil {
		t.Fatalf("写粗体样本: %v", err)
	}
	if err := os.WriteFile(plain, plainData, 0o644); err != nil {
		t.Fatalf("写正体样本: %v", err)
	}

	resetFontStateForTest(t)
	rec := newChooseReadRecorder(dir)
	defer rec.stop()

	f, err := chooseBaseFont([]string{bold, plain})
	if err != nil {
		t.Fatalf("chooseBaseFont: %v", err)
	}
	if got := baseFaceRank(f); got != 0 {
		t.Fatalf("选出的面 rank = %d, 期望 0 (正体优先)", got)
	}
	if n := rec.files[bold]; n != 0 {
		t.Errorf("粗体候选被整份读了 %d 次, 期望 0 (探针判面即可否掉它)", n)
	}
	if n := rec.files[plain]; n != 1 {
		t.Errorf("正体候选被整份读了 %d 次, 期望 1", n)
	}
	// 字节口径: 只该付一份文件的字节 (+ 探针读的表头, 不属于 fontReadHook 计数)。
	if rec.bytes > int64(len(plainData)) {
		t.Errorf("整份读盘 %d 字节 > 正体文件 %d 字节 —— 说明还多读了一份文件",
			rec.bytes, len(plainData))
	}
}

// TestChooseBaseFontFallbackStillReadsOneFile 守"整机只有非正体面"的降级路径:
// 兜底仍然有字可渲染, 且只读一份 (不是把每个候选都读完)。
func TestChooseBaseFontFallbackStillReadsOneFile(t *testing.T) {
	boldPath := systemFontOfRank(t, false)
	boldData, err := os.ReadFile(boldPath)
	if err != nil {
		t.Fatalf("读取粗体样本: %v", err)
	}
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a_bold.ttf", "b_bold.ttf", "c_bold.ttf"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, boldData, 0o644); err != nil {
			t.Fatalf("写样本 %s: %v", name, err)
		}
		paths = append(paths, p)
	}

	resetFontStateForTest(t)
	rec := newChooseReadRecorder(dir)
	defer rec.stop()

	f, err := chooseBaseFont(paths)
	if err != nil {
		t.Fatalf("chooseBaseFont: %v", err)
	}
	if f == nil {
		t.Fatal("兜底路径返回 nil 面 —— 无正体时也必须有字可渲染")
	}
	if total := rec.files[paths[0]] + rec.files[paths[1]] + rec.files[paths[2]]; total != 1 {
		t.Errorf("无正体兜底时整份读了 %d 个文件, 期望 1 (只读兜底那一个)", total)
	}
	if rec.files[paths[0]] != 1 {
		t.Errorf("兜底应取首个候选, 实际首个候选读盘 %d 次", rec.files[paths[0]])
	}
}

// TestProbeFileFaceRankMatchesFullParse 钉住"便宜探针"与"整份解析"同口径。
//
// 这条比字节断言重要: 探针判错档位 = 默认字体选成粗体 (整屏变粗, 且 fontWeight
// 看起来完全失效)。所以对每台机器上能拿到的样本, 两条路的 rank 必须相等。
func TestProbeFileFaceRankMatchesFullParse(t *testing.T) {
	initFontCandidates()
	checked := 0
	for _, p := range fontCandidates {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		f, err := parseFontFile(p)
		if err != nil {
			// 坏候选: 整份解析失败, 探针也不该说"可用"。
			if _, ok := probeFileFaceRank(p); ok {
				// 允许"能读表头但读不出面"的极端情形, 但记一条, 便于排查。
				t.Logf("注意: %s 整份解析失败但探针说可用", filepath.Base(p))
			}
			continue
		}
		probeRank, ok := probeFileFaceRank(p)
		if !ok {
			t.Errorf("%s: 探针判为不可用, 但整份解析成功", filepath.Base(p))
			continue
		}
		// 探针给的是文件里最好的一面; 整份解析给的是同一面 (regularFaceOf)。
		if want := baseFaceRank(f); probeRank != want {
			t.Errorf("%s: 探针 rank = %d, 整份解析 rank = %d (两条路必须同口径)",
				filepath.Base(p), probeRank, want)
		}
		checked++
		if checked >= 60 {
			break
		}
	}
	if checked == 0 {
		t.Skip("当前机器没有可解析的系统字体, 跳过")
	}
	t.Logf("探针/整份解析口径一致: 核对 %d 个字体文件", checked)
}
