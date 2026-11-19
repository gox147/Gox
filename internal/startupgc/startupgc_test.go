package startupgc

// 闸门：这些用例守的是"抬高 → 还原"这条链上的每一个可能被改坏的形态。
// 它们都不看时间、不看机器，只断言 GC 目标与全局状态 —— 毫秒数在 CI 上不可判。

import (
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

// basePercent 把 GC 目标固定成 v，并在用例结束时还原成 100。
// 用真实的 debug.SetGCPercent：这套逻辑的价值全在"与运行时的真实交互"上，
// 换成假实现就只剩自证。
func basePercent(t *testing.T, v int) {
	t.Helper()
	debug.SetGCPercent(v)
	t.Cleanup(func() { debug.SetGCPercent(100) })
}

// currentPercent 读当前 GC 目标（-1 → -1，其余往返一次拿原值）。
func currentPercent() int {
	old := debug.SetGCPercent(-1)
	debug.SetGCPercent(old)
	return old
}

func resetEnv(t *testing.T, gogc string) {
	t.Helper()
	t.Setenv("GOGC", gogc)
}

func TestBoostRaisesPercent(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)
	g := Boost(0)
	defer g.Restore()
	if got := currentPercent(); got != BoostPercent {
		t.Fatalf("Boost 后 GC 目标 = %d, 想要 %d", got, BoostPercent)
	}
	if !Active() {
		t.Fatal("Boost 之后 Active() 应为 true")
	}
}

func TestRestorePutsBackPreviousValue(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)
	g := Boost(0)
	g.Restore()
	if got := currentPercent(); got != 100 {
		t.Fatalf("Restore 后 GC 目标 = %d, 想要 100", got)
	}
	if Active() {
		t.Fatal("Restore 之后 Active() 应为 false")
	}
	if g.Reason() != "manual" {
		t.Fatalf("Reason = %q, 想要 manual", g.Reason())
	}
}

// 还原的是"Boost 之前的值"，不是写死的 100 —— 否则会覆盖别的调优方。
func TestRestoreHonoursNonDefaultPrevious(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 400)
	g := Boost(0)
	if got := currentPercent(); got != BoostPercent {
		t.Fatalf("Boost 后 = %d", got)
	}
	g.Restore()
	if got := currentPercent(); got != 400 {
		t.Fatalf("Restore 后 = %d, 想要 400（Boost 前的原值）", got)
	}
}

// 用户设了 GOGC：一次都不碰（包括 GOGC=off）。
func TestBoostSkippedWhenGOGCEnvSet(t *testing.T) {
	for _, v := range []string{"50", "off", "100"} {
		resetEnv(t, v)
		basePercent(t, 100)
		g := Boost(0)
		if got := currentPercent(); got != 100 {
			t.Fatalf("GOGC=%q 时被抬成了 %d —— 用户的调优决定不该被改", v, got)
		}
		if Active() {
			t.Fatalf("GOGC=%q 时 Active() 应为 false", v)
		}
		g.Restore()
		if got := currentPercent(); got != 100 {
			t.Fatalf("空 Guard 的 Restore 改了状态: %d", got)
		}
	}
}

func TestRestoreIdempotentAndNilSafe(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)

	var calls atomic.Int32
	orig := setPercent
	setPercent = func(p int) int { calls.Add(1); return orig(p) }
	defer func() { setPercent = orig }()

	g := Boost(0)
	g.Restore()
	g.Restore()
	g.Restore()
	if n := calls.Load(); n != 2 {
		t.Fatalf("setPercent 调用 %d 次, 想要 2 次（抬一次 + 还一次）", n)
	}

	var nilGuard *Guard
	nilGuard.Restore() // 不许 panic
	if nilGuard.Reason() != "" {
		t.Fatal("nil Guard 的 Reason 应为空")
	}
}

func TestDoubleBoostDoesNotStack(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)
	g1 := Boost(0)
	g2 := Boost(0) // 第二个不该再抬一次
	if !Active() {
		t.Fatal("Active() 应为 true")
	}
	if got := currentPercent(); got != BoostPercent {
		t.Fatalf("重复 Boost 后 = %d", got)
	}
	g2.Restore() // 空 Guard：不许把窗口关掉
	if !Active() {
		t.Fatal("空 Guard 的 Restore 把窗口关了")
	}
	g1.Restore()
	if got := currentPercent(); got != 100 {
		t.Fatalf("最终 = %d", got)
	}
}

// deadline 兜底：新执行路径忘了 Restore 也不会让进程一直跑在 200%。
func TestDeadlineRestores(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)
	g := Boost(5 * time.Millisecond)
	defer g.Restore()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !Active() && currentPercent() == 100 {
			if g.Reason() != "deadline" {
				t.Fatalf("Reason = %q, 想要 deadline", g.Reason())
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("5ms 的 deadline 没还原：Active=%v percent=%d", Active(), currentPercent())
}

// 堆上限兜底：启动里灌数据（而不是建界面）时必须立刻收手。
// 用注入的 readHeap 假装活堆已超限，避免真的分配 64MB。
func TestHeapGuardRestores(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, 100)

	origRead := readHeap
	readHeap = func() uint64 { return HeapGuardBytes + 1 }
	defer func() { readHeap = origRead }()

	g := Boost(time.Minute) // 到点兜底设得很远，只能靠堆上限收
	defer g.Restore()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !Active() {
			if g.Reason() != "heap" {
				t.Fatalf("Reason = %q, 想要 heap", g.Reason())
			}
			if got := currentPercent(); got != 100 {
				t.Fatalf("堆上限还原后 = %d", got)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("堆超限 2s 内没还原")
}

// 已经在别处关掉 GC（prev<0）时不许"帮人打开"。
func TestBoostLeavesDisabledGCAlone(t *testing.T) {
	resetEnv(t, "")
	basePercent(t, -1)
	defer debug.SetGCPercent(100)
	g := Boost(0)
	if got := currentPercent(); got != -1 {
		t.Fatalf("GC 被打开了: %d", got)
	}
	if Active() || g.prev >= 0 {
		t.Fatal("prev<0 时不该留下状态")
	}
	g.Restore()
	if got := currentPercent(); got != -1 {
		t.Fatalf("Restore 后 = %d, 想要 -1", got)
	}
}
