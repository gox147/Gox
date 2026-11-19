//go:build linux

package x11

// ===== 建连预热 (T3-5 第十八批, 见 gfx/display_prewarm.go 的说明) =====
//
// 这里只做一件事: 把 `xgb.NewConn` (dial + Xauthority + Setup 握手) 提前跑完,
// 把连接留给**下一个** newSurface 用。窗口的建立、事件、上屏一概不动 ——
// 预热只是把"连接"这一个动作挪到更早、还没有并发 I/O 的时刻。
//
// 为什么 warmConn 只给一个 surface: 一个 xgb.Conn 上挂着一个窗口的事件流
// (newSurface 里的读事件 goroutine 独占 WaitForEvent)。两个窗口共用一个连接
// 会互相抢事件, 那是另一个量级的改动; 这里保持"一窗一连", 预热只省第一个。

import (
	"os"
	"sync"

	"github.com/jezek/xgb"
)

var (
	warmMu   sync.Mutex
	warmConn *xgb.Conn
	// warmDisplay 是预热时用的 DISPLAY 值。取用时比对: 进程里改了 DISPLAY
	// (测试与多会话宿主会这么做) 就不能拿旧连接顶上, 否则窗口开在别人的屏上。
	warmDisplay string
)

// Prewarm 提前完成连接与 Setup 握手。已预热过直接返回 true (幂等)。
// 连不上 (无 DISPLAY / 无 X server) 返回 false, 不留下半成品状态。
func Prewarm() bool {
	warmMu.Lock()
	defer warmMu.Unlock()
	if warmConn != nil {
		return warmDisplay == os.Getenv("DISPLAY")
	}
	conn, err := xgb.NewConn()
	if err != nil {
		return false
	}
	warmConn = conn
	warmDisplay = os.Getenv("DISPLAY")
	return true
}

// takeWarm 取走预热连接 (只给第一个取用者)。没有可用的就返回 nil,
// 调用方按老路径自己连。DISPLAY 变过时丢弃旧连接 —— 那一份已经指向
// 另一个显示服务器, 拿它建窗等于把窗口开到别的会话里。
func takeWarm() *xgb.Conn {
	warmMu.Lock()
	defer warmMu.Unlock()
	conn := warmConn
	if conn == nil {
		return nil
	}
	warmConn = nil
	if warmDisplay != os.Getenv("DISPLAY") {
		conn.Close()
		return nil
	}
	return conn
}

// PrewarmDisplay 实现 gfx 的 displayPrewarmer 可选能力。
func (f *factory) PrewarmDisplay() bool { return Prewarm() }
