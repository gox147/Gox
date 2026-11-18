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
	fontCandsRan.Store(false)
	fontCandsScannedGen.Store(^uint64(0))
	resetFontCaches()
	rebuildCandidatesLocked()
	t.Cleanup(func() {
		fontMu.Lock()
		baseFont = nil
		faceBySize = map[int]font.Face{}
		fontMu.Unlock()
		scannedFonts = nil
		fontCandsOnce = sync.Once{}
		fontCandsRan.Store(false)
		fontCandsScannedGen.Store(^uint64(0))
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
	probes := map[string]int{}
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
	// 探针计数 (T3-5 第十批): 判面改成"只读表头"之后, 「坏候选被试过没有」
	// 只剩这个可观测点 —— 它既不该被静默忽略, 也不该被整份读进来。
	fontProbeHook = func(path string) {
		if filepath.Dir(path) != dir {
			return
		}
		probes[filepath.Base(path)]++
	}
	t.Cleanup(func() { fontReadHook = nil; fontProbeHook = nil })

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
	// 每个候选最多整份读一次 (T3-5 第十批起, 被探针否掉的候选是 0 次)。
	for name, n := range perPath {
		if n > 1 {
			t.Errorf("%s 在选默认字体阶段被整份读了 %d 次, 期望 ≤1", name, n)
		}
	}
	if probes["NotoSansCJK-0broken.ttf"] != 1 {
		t.Errorf("坏样本探针次数 = %d, 期望 1 (它必须真的被试过, 否则这条断言没验证回退)",
			probes["NotoSansCJK-0broken.ttf"])
	}
	if n := perPath["NotoSansCJK-0broken.ttf"]; n != 0 {
		t.Errorf("坏样本整份读盘 %d 次, 期望 0 —— 表头就足以判定它不是字体", n)
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

// useFontSampleDir 准备一个**不在 t.TempDir() 下**的样本目录, 并登记清理。
//
// 为什么不用 t.TempDir(): 缓存命中时会持有被缓存文件的 fd 直到
// resetFontCaches, 而 Windows 上"文件还被打开着"的目录删不掉 —— t.TempDir 的
// 自动清理会报 "The process cannot access the file because it is being used by
// another process", 于是**用例本身全过、却在 cleanup 阶段红掉**（2026-11-19 的
// Windows CI 就是这个形态, 断言一条都没出现）。这里的顺序是: 先 resetFontCaches
// 放掉 fd, 再删目录。
func useFontSampleDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gox-fontcache-")
	if err != nil {
		t.Fatalf("建样本目录: %v", err)
	}
	t.Cleanup(func() {
		resetFontCaches() // 先松开 fd（Windows 上不松开就删不掉）
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("清理样本目录（不致命, 在系统临时目录下）: %v", err)
		}
	})
	return dir
}

// TestFontFileCacheHonoursFileIdentity 文件被替换后 (mtime/size 变) 必须重读,
// 不能拿旧的面继续用 —— 同一个进程里换字体目录、热替换字体文件都是真实用法。
func TestFontFileCacheHonoursFileIdentity(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	p := filepath.Join(useFontSampleDir(t), "swap.ttf")
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
	if _, err := loadFontFileEntry(p); err == nil {
		t.Fatalf("文件已被替换成非字体, 仍解析成功 —— 文件身份校验失效")
	}
	// 再问一次: 上一次要把"坏文件"也钉成一次未命中, 不能反过来污染缓存。
	if _, err := loadFontFileEntry(p); err == nil {
		t.Fatalf("坏文件在第一次未命中之后又被缓存成功了 —— 缓存键没跟着内容走")
	}
}

// TestFontFileCacheRejectsSameSizeSameMtimeSwap 等长内容 + 原 mtime 的替换也必须
// 被认出来 —— (mtime, size) 不够, 必须验内容指纹。
//
// 为什么单独立一条: 2026-11-19 的 Windows CI 上 TestFontFileCacheHonoursFileIdentity
// 红的正是这个形态 —— os.WriteFile 覆盖写会**保留原 mtime**, 等长内容下
// (size, mtime) 完全没变, 缓存照样命中, 用例报"换成非字体仍解析成功"。那条
// 用例自己靠"把 mtime 往后推 2 秒"躲开了这个洞, 于是洞一直在。
func TestFontFileCacheRejectsSameSizeSameMtimeSwap(t *testing.T) {
	src := firstUsableFontPath(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	resetFontCandidateScan(true) // 需要持有 fontMu 的版本自己加锁
	t.Cleanup(func() { resetFontCandidateScan(true) })

	p := filepath.Join(useFontSampleDir(t), "same_size.ttf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写样本: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat 样本: %v", err)
	}
	if _, err := parseFontFile(p); err != nil {
		t.Fatalf("首解析: %v", err)
	}

	// 等长垃圾内容, 再把 mtime 按原值写回 (模拟 Windows 上"覆盖写保留 mtime")。
	junk := make([]byte, len(data))
	for i := range junk {
		junk[i] = 0x5a
	}
	if err := os.WriteFile(p, junk, 0o644); err != nil {
		t.Fatalf("改写样本: %v", err)
	}
	if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if now, err := os.Stat(p); err == nil {
		if now.Size() != st.Size() || !now.ModTime().Equal(st.ModTime()) {
			t.Skipf("本机无法构造等长 + 同 mtime 的样本 (size %d→%d, mtime %v→%v)",
				st.Size(), now.Size(), st.ModTime(), now.ModTime())
		}
	}
	if _, err := loadFontFileEntry(p); err == nil {
		t.Fatalf("等长内容替换 + 同 mtime 后仍解析成功 —— 缓存只靠 (size, mtime) 认身份, 内容指纹没生效")
	}
}

// TestFontFileCacheRejectsTailOnlySwap 首 4K 完全相同、只有后面某个字节不同
// 的替换也必须被认出来。
//
// 为什么还要这一条: 上面两条都是"首 4K 就不同"的形态, 只要指纹存了头部就能
// 过。2026-11-19 第二次 Windows 红揭示的真洞是: 指纹原先只存头部 4K 的**原字节**
// 而不是"片段 + 片段", 于是大小相同、头部相同、只在尾部差一个字节的文件
// 指纹照样相等 —— 契约写的是"内容变了就重读", 实际做不到。这条用例把契约钉死。
func TestFontFileCacheRejectsTailOnlySwap(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	if len(data) <= sigLen+16 {
		t.Skipf("样本只有 %d 字节, 造不出首 4K 之外的差异", len(data))
	}
	resetFontCandidateScan(true)
	t.Cleanup(func() { resetFontCandidateScan(true) })

	p := filepath.Join(useFontSampleDir(t), "tail_only.ttf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写样本: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat 样本: %v", err)
	}
	if _, err := loadFontFileEntry(p); err != nil {
		t.Fatalf("首解析: %v", err)
	}

	// 尾段翻一个字节, 长度与 mtime 全部保持原样。
	swapped := append([]byte(nil), data...)
	swapped[len(swapped)-1] ^= 0xff
	if err := os.WriteFile(p, swapped, 0o644); err != nil {
		t.Fatalf("改写样本: %v", err)
	}
	if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if now, err := os.Stat(p); err == nil && (now.Size() != st.Size() || !now.ModTime().Equal(st.ModTime())) {
		t.Skipf("本机无法构造等长 + 同 mtime 的样本 (size %d→%d, mtime %v→%v)",
			st.Size(), now.Size(), st.ModTime(), now.ModTime())
	}

	// 先确认这份内容真的变了而身份没变: 指纹必须不等。
	ent, err := loadFontFileEntry(p)
	if err != nil {
		t.Fatalf("重解析: %v", err)
	}
	want := takeFontSignature(swapped, int64(len(swapped)))
	if !ent.sigOK {
		t.Fatalf("重读后仍没有指纹 —— 身份校验退化成了纯 (size, mtime)")
	}
	if ent.sig != want {
		t.Fatalf("缓存返回的是旧 entry (指纹仍是替换前那份) —— 首 4K 相同、尾部不同的替换没被认出来")
	}
}

// TestFontFileCacheMissDoesNotUseStaleFingerprint 反向守: 内容没变时必须命中并
// 复用同一份 entry, 否则每次取面都整份重解析, 冷启动会退回去。
func TestFontFileCacheMissDoesNotUseStaleFingerprint(t *testing.T) {
	data, err := os.ReadFile(firstUsableFontPath(t))
	if err != nil {
		t.Fatalf("读取字体样本: %v", err)
	}
	resetFontCandidateScan(true)
	t.Cleanup(func() { resetFontCandidateScan(true) })

	p := filepath.Join(useFontSampleDir(t), "stable.ttf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写样本: %v", err)
	}
	first, err := loadFontFileEntry(p)
	if err != nil {
		t.Fatalf("首解析: %v", err)
	}
	again, err := loadFontFileEntry(p)
	if err != nil {
		t.Fatalf("二次解析: %v", err)
	}
	if first != again {
		t.Fatalf("内容没变却重建了 entry —— 指纹判定过严, 缓存形同虚设")
	}
}
