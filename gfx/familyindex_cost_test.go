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

// buildCostRuns 是成本测量的轮数: 5 轮取中位足够稳, 又不至于让 CI 多花一秒。
const buildCostRuns = 5

// familyBuildLimit 是族索引建立成本的上限。
//
// 实测 (本机 EPYC 9754 / 32 vCPU / 330 文件 148MB / 342 面, n=5 中位):
//
//	顺序实现   41~45ms
//	并发实现   13~15ms
//
// 取 25ms 卡在两者中间: 拦的是"退回单线程"这类结构性回退, 不拦几毫秒浮动。
//
// 判据取**最小值**而不是中位数: 并发建的墙钟在同一进程里会被 GC / 邻居负载
// 抬高 (实测同一份代码, 单独跑中位 13.7ms, 混在大套件里中位能到 103ms、
// 单轮最大 197ms), 而干扰只会让它**变大**, 不会让它变小 —— 于是"最小值"
// 是这条路上唯一稳定的判据。顺序实现的 min 同样是 41~45ms, 两者不会混淆。
const familyBuildLimit = 25 * time.Millisecond

// measureFamilyIndexBuild 清空索引与字体文件缓存后重跑一次 build。
//
// 必须清缓存: build 的早退判据就是 ix.built, 不清等于量了个空函数 (这条
// 陷阱在第六批的探针里踩过)。
func measureFamilyIndexBuild() (time.Duration, int) {
	familyIdx.mu.Lock()
	familyIdx.built = false
	familyIdx.mu.Unlock()
	fontFileMu.Lock()
	fontFileCache = map[string]*fontFileEntry{}
	fontFileMu.Unlock()

	t0 := time.Now()
	familyIdx.build()
	d := time.Since(t0)

	familyIdx.mu.Lock()
	fams := len(familyIdx.files)
	familyIdx.mu.Unlock()
	return d, fams
}

// TestFamilyIndexBuildCost 是族索引建立成本的闸门 (判据见 familyBuildLimit)。
//
// 判据用墙钟中位数: 这一批的成本是**逐面解析** (每面一遍 sfnt 的 name/post
// 解析, 纯 CPU), 与字体库内容无关, 所以墙钟可比; 而"读了几次盘"在这条路上
// 恒等于"文件数", 换实现也不会变, 当不了判据。
func TestFamilyIndexBuildCost(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过 (要遍历整机字体目录)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 fontScanDirs 只返回静态候选, 本探针无意义")
	}
	if raceDetectorEnabled {
		t.Skip("-race 下插桩本身就让墙钟慢一个数量级 (14ms → 117ms), 成本断言无意义")
	}
	measureFamilyIndexBuild() // 预热: 目录项进页缓存, 只比解析

	xs := make([]time.Duration, 0, buildCostRuns)
	fams := 0
	for i := 0; i < buildCostRuns; i++ {
		d, n := measureFamilyIndexBuild()
		xs = append(xs, d)
		fams = n
	}
	med := medianDuration(xs)
	t.Logf("族索引建立 (n=%d): 中位=%.2fms min=%.2fms max=%.2fms 族数=%d GOMAXPROCS=%d",
		buildCostRuns, msOf(med), msOf(minDuration(xs)), msOf(maxDuration(xs)), fams, runtime.GOMAXPROCS(0))

	if fams < 50 {
		t.Skipf("本机只认出 %d 个族 (字体库太小/为空), 成本闸门没有意义", fams)
	}
	best := minDuration(xs)
	if best > familyBuildLimit {
		t.Fatalf("族索引建立最快一轮 %.1fms 仍超过 %v 上限 (中位 %.1fms) —— 首次用 fontFamily 会白等这一笔。\n"+
			"先看 build 是不是又退回逐文件顺序调用 (第九批改成了有界并发: 顺序 41~45ms → 并发 13~15ms,"+
			"见 docs/performance.md §12.8)。", msOf(best), familyBuildLimit, msOf(med))
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
	measureFamilyIndexBuild()
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
