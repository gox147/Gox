//go:build linux

package x11

// 建连预热的闸门 (T3-5 第十八批, 见 prewarm.go)。
//
// 这些用例要真的连 X server, 没有 DISPLAY 就跳过 (与 x11_test.go 其余用例
// 同一条纪律: CI 无头环境下不造"假 X")。守的形态:
//   - 预热连接**只给第一个**取用者 (第二个窗口照旧自己连, 事件流不串);
//   - 建窗真的用的是预热那份连接 (不是"预热了但建窗又自己连一个");
//   - DISPLAY 变过就丢弃 (否则窗口会开在别的会话里);
//   - 连不上时返回 false, 不留半成品状态, 也不改变建窗的错误路径。

import (
	"os"
	"testing"
	"time"

	"github.com/14752222/Gox/gfx"
)

// ensureWarmOrSkip 保证本用例开始时**已经**有一条预热连接, 留在槽里交给
// 用例自己取。连不上就跳过。
//
// 为什么要重试: 上一个用例断开的连接, X server 侧要 ~10ms 才清理完, 紧接着
// 的 accept 会被拒 (docs/performance.md §12.12 记的是同一现象的另一面)。
// 这是**测试支架**的抖动, 不是预热失败 —— 真实首帧路径上此时还没有任何连接。
func ensureWarmOrSkip(t *testing.T) {
	t.Helper()
	if os.Getenv("DISPLAY") == "" {
		t.Skip("无 DISPLAY, 建连预热无对象")
	}
	for i := 0; i < 10; i++ {
		if Prewarm() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Skip("连不上当前 DISPLAY, 跳过")
}

// discardWarmForTest 丢弃尚未取用的预热连接。
func discardWarmForTest() {
	warmMu.Lock()
	c := warmConn
	warmConn, warmDisplay = nil, ""
	warmMu.Unlock()
	if c != nil {
		c.Close()
	}
}

func TestPrewarmConnTakenOnce(t *testing.T) {
	ensureWarmOrSkip(t)
	t.Cleanup(discardWarmForTest)

	warmMu.Lock()
	first := warmConn
	warmMu.Unlock()
	if first == nil {
		t.Fatalf("预热后槽里应有连接")
	}
	// 幂等: 再调一次不换连接。
	if !Prewarm() {
		t.Fatalf("第二次 Prewarm 应返回 true")
	}
	warmMu.Lock()
	second := warmConn
	warmMu.Unlock()
	if first != second {
		t.Fatalf("重复预热换掉了连接")
	}

	c := takeWarm()
	if c == nil {
		t.Fatalf("takeWarm 应取到预热连接")
	}
	c.Close()
	if again := takeWarm(); again != nil {
		again.Close()
		t.Fatalf("预热连接只该给一个取用者, 第二次仍取到了")
	}
}

// TestPrewarmConnUsedByCreate 建窗必须真的用预热那份连接。
func TestPrewarmConnUsedByCreate(t *testing.T) {
	ensureWarmOrSkip(t)
	t.Cleanup(discardWarmForTest)

	warmMu.Lock()
	warm := warmConn
	warmMu.Unlock()

	f := &factory{}
	surf, err := f.Create(gfx.WindowConfig{Title: "prewarm", Width: 120, Height: 80})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s, ok := surf.(*surface)
	if !ok {
		t.Fatalf("Create 返回的不是本后端的 surface: %T", surf)
	}
	if s.conn != warm {
		t.Fatalf("建窗没有用预热连接 (自己又连了一个)")
	}
	if left := takeWarm(); left != nil {
		left.Close()
		t.Fatalf("建窗之后预热槽应为空")
	}
	s.conn.Close()
}

// TestPrewarmDropsConnWhenDisplayChanged DISPLAY 变过就不能拿旧连接顶上。
func TestPrewarmDropsConnWhenDisplayChanged(t *testing.T) {
	ensureWarmOrSkip(t)
	t.Cleanup(discardWarmForTest)

	t.Setenv("DISPLAY", ":1977") // 与预热时的值不同即可, 不需要真有 server
	if c := takeWarm(); c != nil {
		c.Close()
		t.Fatalf("DISPLAY 变了仍取到了旧连接 —— 窗口会开在别的会话里")
	}
}

// TestPrewarmFailsWithoutServer 连不上时返回 false, 槽位保持干净。
func TestPrewarmFailsWithoutServer(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过 (可能要等 connect 失败)")
	}
	discardWarmForTest()
	t.Cleanup(discardWarmForTest)
	t.Setenv("DISPLAY", ":1977")
	if Prewarm() {
		t.Fatalf("没有 X server 时应返回 false")
	}
	warmMu.Lock()
	left := warmConn
	warmMu.Unlock()
	if left != nil {
		t.Fatalf("失败时不该留下连接")
	}
}
