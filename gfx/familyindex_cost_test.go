package gfx

// 冷启动线 (T3-5) 第九批: 族索引建立的**并发化**。
//
// 两条断言, 各管一件事:
//
//  1. TestFamilyIndexBuildCost —— 成本闸门。族索引只在脚本第一次用
//     `fontFamily` 时建, 不在任何启动打点里, 变慢了没有哪条曲线会红
//     (§12.7 的遗留条目正是这么写的)。
//  2. TestFamilyIndexParallelMatchesSerial —— 并发化**没有改变索引内容**。
//     这条比成本更重要: "先到先得"的裁决一旦被 goroutine 调度左右, 同一台
//     机器两次启动可能选到不同字重的面, 渲染结果不可复现, 而且只在多字重
//     字体族上偶发 —— 这类 bug 靠人眼抓不到。
//
// 两条都只依赖"这台机器上有没有字体", 换机器不假红; 字体库太小则跳过。

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// buildCostRuns 是成本测量的轮数: 5 轮取最小值足够稳, 又不至于让 CI 多花
// 一秒。取最小值而不是中位数: 干扰 (GC/邻居负载) 只会让墙钟变大, 不会让它
// 变小 —— 同一份代码单独跑最小值 12.8ms, 混在大套件里单轮能到 197ms。
const buildCostRuns = 5

// familyBuildSpeedup 是"并发必须比顺序快多少倍"的下限。
//
// 用**比值**而不是绝对毫秒: 绝对阈值在 CI 上不可移植 (runner 2~8 核、负载
// 随邻居变), 而"并发化到底省了多少"是自校准的 —— 同一个进程、同一份页缓存、
// 同一批文件, 唯一变量就是 parallelFor 的 workers。
//
// 实测轨迹 (32 核, 同一台机器):
//
//	第九批 (逐文件整份解析): 顺序 41.3ms / 并发 13.8ms = 2.99x  → 下限 1.33x
//	第十一批 (逐文件只读 name/post 表头): 顺序 5.5ms / 并发 3.9ms = 1.30~1.57x
//
// 下限从 1.33 降到 1.15 是**跟着绝对成本走的**, 不是给回归留后门: 第十一批
// 把每个文件的成本砍掉 5.6 倍之后, 剩下的 5.5ms 里并发能拿到的只有调度重叠
// (读盘不再是瓶颈), 8 次取样落在 1.30~1.57 —— 1.33 正好卡在噪声带里, 会
// 假红 (实测 8 次中红 2 次)。1.15 仍然挡得住这条闸门真正要挡的两件事:
// "并发没接上"(=1.00) 与"新加的共享锁把并行串行化"(<1.15)。
const familyBuildSpeedup = 1.15

// measureFamilyIndexBuild 清空索引与字体文件缓存后重跑一次 build。
//
// 必须清缓存: build 的早退判据就是 ix.built, 不清等于量了个空函数 (这条
// 陷阱在第六批的探针里踩过)。
func measureFamilyIndexBuild(workers int) (time.Duration, int) {
	familyIdx.mu.Lock()
	familyIdx.built = false
	familyIdx.mu.Unlock()
	fontFileMu.Lock()
	fontFileCache = map[string]*fontFileEntry{}
	fontFileMu.Unlock()

	t0 := time.Now()
	familyIdx.buildWith(workers)
	d := time.Since(t0)

	familyIdx.mu.Lock()
	fams := len(familyIdx.files)
	familyIdx.mu.Unlock()
	return d, fams
}

// TestFamilyIndexBuildCost 是族索引建立成本的闸门 (判据见 familyBuildSpeedup)。
//
// 量两遍: 先 workers=1 (等价于并发化之前的逐文件顺序调用), 再 workers=0
// (自动并发)。两遍用**同一批文件、同一份页缓存**, 所以比值只反映并发本身。
//
// 为什么不用"绝对毫秒 ≤ N": 换台机器就假红 (第一版试过 25ms, 在 2 核 runner
// 上必红), 而放宽到"不会假红"的毫秒数又等于不设闸门。比值没有这个问题。
func TestFamilyIndexBuildCost(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过 (要遍历整机字体目录)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 fontScanDirs 只返回静态候选, 本探针无意义")
	}
	if raceDetectorEnabled {
		t.Skip("-race 下插桩让墙钟失真一个数量级, 比值断言无意义")
	}
	if runtime.GOMAXPROCS(0) < 4 {
		t.Skipf("只有 %d 个可用核, 并发收益无法与噪声区分", runtime.GOMAXPROCS(0))
	}
	measureFamilyIndexBuild(0) // 预热: 目录项与字体页进缓��, 只比解析

	var seqs, pars []time.Duration
	fams := 0
	for i := 0; i < buildCostRuns; i++ {
		d, n := measureFamilyIndexBuild(1) // 顺序
		seqs = append(seqs, d)
		fams = n
		d, n = measureFamilyIndexBuild(0) // 并发
		pars = append(pars, d)
		fams = n
	}
	seq := minDuration(seqs)
	par := minDuration(pars)
	speedup := float64(seq) / float64(par)
	t.Logf("族索引建立 (n=%d, 取最小值): 顺序=%.2fms 并发=%.2fms 加速=%.2fx 族数=%d GOMAXPROCS=%d",
		buildCostRuns, msOf(seq), msOf(par), speedup, fams, runtime.GOMAXPROCS(0))

	if fams < 50 {
		t.Skipf("本机只认出 %d 个族 (字体库太小/为空), 成本闸门没有意义", fams)
	}
	if seq < 5*time.Millisecond {
		t.Skipf("顺序建索引只用 %.1fms (字体库太小), 比值会被测量误差主导", msOf(seq))
	}
	if speedup < familyBuildSpeedup {
		t.Fatalf("族索引的并发只快了 %.2fx (顺序 %.1fms / 并发 %.1fms), 低于 %.2fx 的下限。\n"+
			"多半是 build 又退回逐文件顺序调用, 或者新加的共享状态把并行串行化了 "+
			"(见 docs/performance.md §12.8)。", speedup, msOf(seq), msOf(par), familyBuildSpeedup)
	}
}

// TestFamilyIndexParallelMatchesSerial 钉住"并发化不改变索引内容"。
//
// 做法: 先用 build() 的并发路径建一份, 再用 indexFileLocked 逐文件顺序建
// 一份 (并发化之前的调用形态), 两份逐项比对。同一份输入的解析结果是确定的,
// 于是差异只可能来自"谁先写进 map" —— 正是要拦的东西。
func TestFamilyIndexParallelMatchesSerial(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过 (要遍历整机字体目录)")
	}
	measureFamilyIndexBuild(0)
	parallelDump := dumpFamilyIndex()

	serial := &familyIndex{}
	serial.mu.Lock()
	serial.built = true
	serial.files = map[string]map[styleAxis]faceSrc{}
	serial.alt = map[string]string{}
	serial.mono = map[string]bool{}
	serial.styleOf = map[string][]styleAxis{}
	for _, p := range familyCandidatePaths() {
		serial.indexFileLocked(p)
	}
	serial.mu.Unlock()

	serialDump := dumpIndex(serial)
	if parallelDump == "" {
		t.Skip("本机没有可解析的字体族, 断言无意义")
	}
	if parallelDump != serialDump {
		t.Fatalf("并发建索引与顺序建索引的结果不一致 (并发化改变了'先到先得'的裁决)。\n"+
			"差异行数=%d %s", diffLines(parallelDump, serialDump), firstDiff(parallelDump, serialDump))
	}
}

// dumpFamilyIndex 导出一份**与插入顺序无关**的索引快照 (排序后的文本)。
func dumpFamilyIndex() string {
	familyIdx.mu.Lock()
	defer familyIdx.mu.Unlock()
	return dumpIndex(familyIdx)
}

// dumpIndex 是 dumpFamilyIndex 的实现, 便于对任一份索引取快照。
func dumpIndex(ix *familyIndex) string {
	var b strings.Builder
	keys := make([]string, 0, len(ix.files))
	for k := range ix.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		axes := make([]string, 0, len(ix.files[k]))
		for ax, src := range ix.files[k] {
			axes = append(axes, fmt.Sprintf("%v=%s#%d", ax, src.path, src.index))
		}
		sort.Strings(axes)
		fmt.Fprintf(&b, "fam %q mono=%v axes=%v\n", k, ix.mono[k], axes)
	}
	alts := make([]string, 0, len(ix.alt))
	for a := range ix.alt {
		alts = append(alts, a+"→"+ix.alt[a])
	}
	sort.Strings(alts)
	for _, a := range alts {
		fmt.Fprintf(&b, "alt %s\n", a)
	}
	return b.String()
}

// diffLines 数 b 里有多少行是 a 里没有的 (只看行集合, 不求最短编辑距离 ——
// 这条断言的输出只要够定位问题)。
func diffLines(a, b string) int {
	as := strings.Split(a, "\n")
	bs := strings.Split(b, "\n")
	have := make(map[string]int, len(as))
	for _, l := range as {
		have[l]++
	}
	n := 0
	for _, l := range bs {
		if have[l] > 0 {
			have[l]--
			continue
		}
		n++
	}
	return n
}

func firstDiff(a, b string) string {
	as := strings.Split(a, "\n")
	bs := strings.Split(b, "\n")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return fmt.Sprintf("第一处差异在第 %d 行:\n  并发: %s\n  顺序: %s", i+1, as[i], bs[i])
		}
	}
	return ""
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func minDuration(xs []time.Duration) time.Duration {
	m := xs[0]
	for _, v := range xs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxDuration(xs []time.Duration) time.Duration {
	m := xs[0]
	for _, v := range xs[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func medianDuration(xs []time.Duration) time.Duration {
	c := append([]time.Duration(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	n := len(c)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// BenchmarkFamilyIndexBuild 量"第一次用 fontFamily"要付多少 (清缓存后重建)。
//
// T3-5 第十一批把逐文件解析换成 name/post 表头探针, 这条基准留作该改动的
// 长期盯梢: 族索引不在任何启动打点里, 它退化了没有曲线会红。
func BenchmarkFamilyIndexBuild(b *testing.B) {
	if testing.Short() {
		b.Skip("-short 下跳过 (要遍历整机字体目录)")
	}
	for i := 0; i < b.N; i++ {
		measureFamilyIndexBuild(0)
	}
}
