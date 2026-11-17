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
