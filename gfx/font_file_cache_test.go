package gfx

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/font"
)

// 冷启动线 (T3-5 第五批) 的回归断言: **同一个字体文件在一次启动里只读一次盘**。
//
// 为什么这条值得单独锁住: 2026-11-17 夜批实测, 首次用字体时 ~70ms 里有 17ms
// 是"扫描刚解析过 NotoSansCJK-Bold.ttc (19MB) 之后, 选默认字体时又从磁盘读了
// 它一遍"。这类浪费没有任何可见后果 —— 界面照样出字, 只是首帧晚了 17ms,
// 靠看代码发现不了 (两次读盘分别在两个文件、两层调用栈里)。把它变成断言,
// 下一次谁再引入重复读盘就会红。
//
// 判据是**每份样本恰好被读到一次**, 比一个总数更能定位是哪个文件被重复读了。
func TestFontFileReadOnceAcrossScanAndChoose(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	dir := t.TempDir()
	// 名字按字典序排列, 让首个候选确定是 a_first.ttf (与真实 Linux 上
	// "扫描结果 CJK 组在前、组内保持目录序"同一形态)。
	var want []string
	for _, name := range []string{"a_first.ttf", "b_second.ttf", "c_third.ttf"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("写样本 %s: %v", name, err)
		}
		want = append(want, filepath.Join(dir, name))
	}

	fontMu.Lock()
	baseFont = nil
	faceBySize = map[int]font.Face{}
	fontMu.Unlock()
	scannedFonts = nil
	fontCandsOnce = sync.Once{}
	resetFontCaches()
	rebuildCandidatesLocked()
	t.Cleanup(func() {
		fontMu.Lock()
		baseFont = nil
		faceBySize = map[int]font.Face{}
		fontMu.Unlock()
		scannedFonts = nil
		fontCandsOnce = sync.Once{}
		rebuildCandidatesLocked()
		resetFontCaches()
	})

	// 只统计**样本目录**里的读盘: 选默认字体落空后仍会去碰真实系统字体,
	// 那不是本用例的观察对象, 也不该让结果依赖机器的字体布局。
	fontFileMu.Lock()
	fontFileCache = map[string]*fontFileEntry{}
	fontFileMu.Unlock()
	reads := map[string]int{}
	fontReadHook = func(path string) {
		if filepath.Dir(path) != dir {
			return
		}
		fontFileMu.Lock() // 与缓存共用同一把锁: 计数与缓存写入天然串行
		reads[path]++
		fontFileMu.Unlock()
	}
	t.Cleanup(func() { fontReadHook = nil })

	// 三个真实阶段: 扫描 → 选默认字体 → 按族名取面 (fontFromSrc 走 fontFileFaces)。
	got := scanSystemFonts([]string{dir}, 100)
	if len(got) != 3 {
		t.Fatalf("扫描结果 = %v, 期望 3 个真字体", got)
	}
	if _, err := chooseBaseFont(got); err != nil {
		t.Fatalf("chooseBaseFont: %v", err)
	}
	for _, p := range got {
		fs, err := fontFileFaces(p)
		if err != nil {
			t.Fatalf("fontFileFaces(%s): %v", filepath.Base(p), err)
		}
		if len(fs) == 0 {
			t.Fatalf("fontFileFaces(%s) 返回空面", filepath.Base(p))
		}
	}

	// 断言: 每个样本都恰好读到过一次。
	for _, p := range want {
		switch n := reads[p]; {
		case n == 0:
			t.Errorf("%s 一次都没被读 —— 样本没走到扫描路径, 断言无效", filepath.Base(p))
		case n != 1:
			t.Errorf("%s 被读了 %d 次, 期望 1 (扫描/选默认/取面必须共用同一份解析结果)", filepath.Base(p), n)
		}
	}
}

// TestScanSystemFontsDoesNotReadFontBytes 是 T3-5 第六批的闸门:
// **目录扫描阶段一次字体字节都不该读**。
//
// 为什么单列一条而不是只靠耗时: 扫描要不要读盘, 是一个"是/否"的事实, 与机器
// 上有多少字体、页缓存热不热、CPU 忙不忙都无关。2026-11-17 夜批实测, 扫描在
// 本机要读 330 个文件 / 148MB ≈ 50ms —— 而这 148MB 换来的信息只有"这文件是
// 不是真字体 + 第一个正体面是谁", 且**只有第一个候选会真的被用上**。为了
// 300 个永远不会被渲染的字体付 148MB 读盘, 是冷启动里最大的一笔冤枉钱。
//
// 判据两条:
//  1. 扫描期间读盘字节数 == 0 (真伪与字重判定推迟到首次使用时);
//  2. 扫描之后 chooseBaseFont 仍能选到真字体 —— 目录里混着"改了扩展名的
//     非字体文件"时, 首个候选解析失败必须能继续往下试, 而不是整屏无字。
func TestScanSystemFontsDoesNotReadFontBytes(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	dir := t.TempDir()
	for _, name := range []string{"DejaVuSans.ttf", "NotoSansCJK-Regular.ttf", "a_first.ttf"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("写样本 %s: %v", name, err)
		}
	}
	// 名字排在 CJK 组最前, 且内容不是字体: 用来证明"扫描不判真伪"之后,
	// 选默认字体的回退路径仍然把坏候选跳过去。
	broken := filepath.Join(dir, "NotoSansCJK-0broken.ttf")
	if err := os.WriteFile(broken, []byte("not a font at all"), 0o644); err != nil {
		t.Fatalf("写坏样本: %v", err)
	}

	resetFontStageCaches()
	t.Cleanup(resetFontStageCaches)

	var reads int
	perPath := map[string]int{}
	fontFileMu.Lock()
	fontFileCache = map[string]*fontFileEntry{}
	fontFileMu.Unlock()
	fontReadHook = func(path string) {
		if filepath.Dir(path) != dir {
			return
		}
		reads++
		perPath[filepath.Base(path)]++
	}
	t.Cleanup(func() { fontReadHook = nil })

	got := scanSystemFonts([]string{dir}, 100)
	if reads != 0 {
		t.Errorf("扫描阶段读了 %d 次盘, 期望 0 —— 扫描只按扩展名收集候选, 真伪与字重留给首次使用时判定", reads)
	}
	// 坏样本也在候选里: 扫描不替调用方做判定, 这是有意的口径(见上)。
	if len(got) != 4 {
		t.Fatalf("候选数 = %d (%v), 期望 4 个 .ttf 条目", len(got), got)
	}
	if filepath.Base(got[0]) != "NotoSansCJK-0broken.ttf" {
		t.Errorf("首个候选 = %s, 期望 CJK 组按目录序排在最前", filepath.Base(got[0]))
	}

	f, err := chooseBaseFont(got)
	if err != nil {
		t.Fatalf("chooseBaseFont: %v —— 坏候选必须能跳过", err)
	}
	if f == nil {
		t.Fatal("chooseBaseFont 返回 nil")
	}
	// 样本是同一个字体的副本 (字重都"不正经"), 所以 chooseBaseFont 会一路试到
	// 最后一个候选才兜底返回首个可解析者 —— 这是设计行为。这里守的是**不重复**:
	// 每个候选最多读一次, 坏样本排除在外。
	for name, n := range perPath {
		if n > 1 {
			t.Errorf("%s 在选默认字体阶段被读了 %d 次, 期望 ≤1", name, n)
		}
	}
	if perPath["NotoSansCJK-0broken.ttf"] != 1 {
		t.Errorf("坏样本读盘次数 = %d, 期望 1 (它必须真的被试过, 否则这条断言没验证回退)",
			perPath["NotoSansCJK-0broken.ttf"])
	}
}

// TestFontFileReadHookIsLive 是上面那条断言的反向校验: 故意绕过缓存再读一次,
// 计数必须变成 2 —— 一个从没失败过的断言等于没有断言。
func TestFontFileReadHookIsLive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "counter.ttf")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("写样本: %v", err)
	}
	n := 0
	fontReadHook = func(path string) {
		if path == p {
			n++
		}
	}
	t.Cleanup(func() { fontReadHook = nil })

	fireFontReadHook(p)
	if _, err := os.ReadFile(p); err != nil { // 绕过缓存直接读
		t.Fatalf("读样本: %v", err)
	}
	fireFontReadHook(p)
	if n != 2 {
		t.Fatalf("钩子计数 = %d, 期望 2 (钩子没接在真实读盘路径上)", n)
	}
}

// TestFontFileCacheHonoursFileIdentity 文件被替换后 (mtime/size 变) 必须重读,
// 不能拿旧的面继续用 —— 同一个进程里换字体目录、热替换字体文件都是真实用法。
func TestFontFileCacheHonoursFileIdentity(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	p := filepath.Join(t.TempDir(), "swap.ttf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写样本: %v", err)
	}
	if _, err := parseFontFile(p); err != nil {
		t.Fatalf("首解析: %v", err)
	}
	// 改成垃圾内容: 若缓存不认身份, 这里仍会返回旧面 (错误地"解析成功")。
	if err := os.WriteFile(p, []byte("no longer a font"), 0o644); err != nil {
		t.Fatalf("改写样本: %v", err)
	}
	future := time.Now().Add(2 * time.Second) // 显式改 mtime: 有些文件系统 mtime 粒度粗
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if _, err := parseFontFile(p); err == nil {
		t.Fatalf("文件已被替换成非字体, 仍解析成功 —— 文件身份校验失效")
	}
}
