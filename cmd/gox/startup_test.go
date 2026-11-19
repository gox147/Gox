package main

// 闸门：启动窗口 (T3-5 第十九批) 的接线。
//
// 这一批的收益是"首帧前那一次 GC 被挪到首帧之后"，毫秒数在 CI 上不可判 ——
// 所以闸门守的是**形态**：
//  ① 执行用户代码的路径上，窗口必须真的开过（删掉 begin 就红：优化等于没接线）；
//  ② 求值返回时**不许**立刻还原（那样那轮 GC 会重新落回"首帧→探测标记"之间，
//     优化在数字上消失）；还原必须靠窗口自己的到点兜底；
//  ③ 用户显式设了 GOGC 时，一根手指都不许碰；
//  ④ begin 重入不许把句柄覆盖掉（那会让窗口永远关不上）。

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/14752222/Gox/internal/startupgc"
)

// currentPercent 读当前 GC 目标（用一次 -1 往返拿原值再还原，不改最终设置）。
func currentPercent(t *testing.T) int {
	t.Helper()
	old := debug.SetGCPercent(-1)
	debug.SetGCPercent(old)
	return old
}

// TestRunFileOpensStartupWindow 是这一批最关键的一条。
//
// 反向验证（都实际跑过）：
//   - 删掉 runFile 里的 beginStartupWindow() → "没有打开启动窗口"红；
//   - 在 runFile 里求值之后加一句 endStartupWindow() → "求值返回时就关了"红。
func TestRunFileOpensStartupWindow(t *testing.T) {
	t.Setenv("GOGC", "") // 空 = 视为未设（口径见 internal/startupgc）
	debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(100) })

	dir := t.TempDir()
	entry := filepath.Join(dir, "entry.js")
	if err := os.WriteFile(entry, []byte("void 0;\n"), 0o644); err != nil {
		t.Fatalf("写入口: %v", err)
	}

	var opened bool
	startupWindowObserver = func(open bool) { opened = opened || open }
	t.Cleanup(func() {
		startupWindowObserver = nil
		endStartupWindow() // 别把窗口留给后面的用例
	})

	runFile(entry)

	if !opened {
		t.Fatal("runFile 没有打开启动窗口 —— beginStartupWindow() 被删了？（等于本批优化没接线）")
	}
	if !startupgc.Active() {
		t.Fatal("求值返回时窗口就关了 —— 那轮 GC 会重新落进首帧口径里，本批收益归零")
	}
	if got := currentPercent(t); got != startupgc.BoostPercent {
		t.Fatalf("求值返回后 GC 目标 = %d, 想要 %d", got, startupgc.BoostPercent)
	}

	// 兜底必须真的会收手（不依赖任何人的手工调用）。
	limit := time.Now().Add(3 * time.Second)
	for time.Now().Before(limit) {
		if !startupgc.Active() {
			if got := currentPercent(t); got != 100 {
				t.Fatalf("兜底还原后 GC 目标 = %d", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("窗口 3s 内没有自己关掉 —— 到点/堆上限兜底失效了")
}

// TestStartupWindowHonoursUserGOGC 守"用户说了算"：显式 GOGC 时不许抬。
func TestStartupWindowHonoursUserGOGC(t *testing.T) {
	t.Setenv("GOGC", "50")
	debug.SetGCPercent(50)
	t.Cleanup(func() { debug.SetGCPercent(100) })

	beginStartupWindow()
	defer endStartupWindow()

	if startupgc.Active() {
		t.Fatal("设了 GOGC 还开了启动窗口")
	}
	if got := currentPercent(t); got != 50 {
		t.Fatalf("设了 GOGC=50 时 GC 目标 = %d", got)
	}
}

// TestBeginStartupWindowIsReentrant 守"重入不换句柄"。
//
// 形态: begin 被调用两次（例如以后有人把 begin 也加到 main 里，或 runFile 被
// 别的入口包了一层），第一次的句柄必须还在 —— 否则 end 还的是空 Guard，
// 窗口永远开着。删掉 startup.go 里的 Active() 判断，这条立刻红。
func TestBeginStartupWindowIsReentrant(t *testing.T) {
	t.Setenv("GOGC", "")
	debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(100) })

	beginStartupWindow()
	beginStartupWindow()
	endStartupWindow()
	if startupgc.Active() {
		t.Fatal("两次 begin 一次 end 之后窗口还开着 —— 句柄被覆盖了")
	}
	if got := currentPercent(t); got != 100 {
		t.Fatalf("重入一次 begin 之后 GC 目标 = %d", got)
	}
}
