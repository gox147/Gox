//go:build linux

// Package x11 是 gfx 的 Linux X11 窗口后端。
//
// 基于 jezek/xgb (纯 Go 的 X11 协议实现, 无 cgo): 直接建窗口、PutImage
// 上屏 (ZPixmap 24 位深度, 小端字节序下每像素 4 字节 = B,G,R,X)、
// 按钮/按键/关闭事件转 gfx.Event。等待采用独立读事件 goroutine + 定时
// select, WaitEvents 可限时。关闭按钮经 WM_PROTOCOLS/WM_DELETE_WINDOW。
// xgb 的请求多为 unchecked (返回 Cookie 不返回错误), 协议错误经
// WaitForEvent 以 Event/XError 形式回来。
//
// TODO(P2-7): 无 IME 支持 —— 输入法要走 XIM 协议 (或现代方案 ibus/fcitx 的
// DBus 接口), 两者都超出"零 cgo、零新依赖"的约束范围。因此 Linux 下
// `<input>` / `<textarea>` 只能直接输入键盘能打出的字符 (BMP), 中文候选词
// 输入不可用。Windows 后端已实现 (见 gfx/ime.go 与 win32 的 WM_IME_* 分支)。
package x11

import (
	"encoding/binary"
	"fmt"
	"image"
	"sync"
	"time"
	"unsafe"

	"github.com/14752222/Gox/gfx"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

func init() {
	gfx.SetDefaultFactory(&factory{})
}

type factory struct{}

// clampI16 把坐标钳进 int16 (xproto.CreateWindow 的 X/Y 是 int16)。
// 多屏拼接的坐标偶尔会超出, 回绕会让窗口出现在屏幕另一头 (且不报错)。
func clampI16(v int) int {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

func (f *factory) Create(cfg gfx.WindowConfig) (gfx.Surface, error) {
	return newSurface(cfg)
}

type surface struct {
	conn *xgb.Conn
	win  xproto.Window
	// root 是本屏的根窗口 (多屏 X11 里通常只有一个根, 所有屏拼在它上面)。
	// 窗口位置要经 TranslateCoordinates 换算到根坐标 —— WM 会把顶层窗口
	// reparent 到一个 frame 窗口, GetGeometry 拿到的是相对 frame 的位置。
	root xproto.Window
	gc   xproto.Gcontext

	wmProtocols xproto.Atom
	wmDelete    xproto.Atom

	events chan gfx.Event
	evch   chan xgb.Event // 读事件 goroutine → WaitEvents
	errch  chan error

	// keyNames: 键码 → 键名 (启动时拉取键盘映射构建)
	keyNames map[xproto.Keycode]string

	mu     sync.Mutex
	closed bool

	// pixMu / pixBuf: 上屏转换的复用缓冲 (T3-5 第二十六批)。只由 ShowRegions
	// 在 pixMu 下使用; 缓冲只增不减, 峰值 = 最大的行带 (128KB) 或单带尺寸。
	pixMu  sync.Mutex
	pixBuf []byte

	w, h int
	// x, y 是最近一次 ConfigureNotify 报的**父窗口坐标系**位置 (移动检测用;
	// 对外报的屏幕坐标由 Bounds 现算, 见 window.go)。
	x, y int
}

func newSurface(cfg gfx.WindowConfig) (gfx.Surface, error) {
	// 预热连接优先 (T3-5 第十八批, 见 prewarm.go): 建连若撞上字体后台预取,
	// 握手会从 0.18ms 涨到 1.2~1.7ms —— 提前在无争用时连好就没这一段。
	conn := takeWarm()
	if conn == nil {
		var err error
		conn, err = xgb.NewConn() // 连接 $DISPLAY
		if err != nil {
			return nil, fmt.Errorf("connect X server: %w (DISPLAY set?)", err)
		}
	}
	gfx.PhaseTrace("x11.conn_ready")
	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)

	w, h := cfg.Width, cfg.Height
	if w <= 0 {
		w = 400
	}
	if h <= 0 {
		h = 300
	}

	win, err := xproto.NewWindowId(conn)
	if err != nil {
		conn.Close() // 预热连接: 到此为止没人接手, 别把 fd 挂到进程退出
		return nil, err
	}
	// 事件: 结构变化/按键(按下+抬起)/按钮(按下+抬起)/指针移动/离开窗口。
	// 曝光不需要 (gfx 自管重绘)。注意 P1-1 新增的指针与键盘事件在纯 Go
	// 单测里覆盖不到, 需 Linux 实机验证 (Windows 路径已完整可用)。
	const eventMask = xproto.EventMaskStructureNotify |
		xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease |
		xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
		xproto.EventMaskPointerMotion | xproto.EventMaskLeaveWindow
	// 位置: WindowConfig.X/Y/Display 显式指定时按目标屏放置 (ResolveWindowPlacement
	// 把"工作区相对 + 设备像素"换算成根坐标里的绝对像素); 否则 (0,0) 交给 WM。
	// X11 用 int16 传坐标 —— 超出范围的值钳一下, 免得回绕到屏幕另一头。
	//
	// 合流备注 (2026-10-02): §四 那版按 cfg.HasPos 直接吃绝对像素 (并裸转
	// int16, 跨屏的大负坐标会回绕)。统一保留 ResolveWindowPlacement 这版:
	// 它同时覆盖多屏目标屏选择与 int16 钳位。
	winX, winY := 0, 0
	if px, py, ok := gfx.ResolveWindowPlacement(cfg); ok {
		winX, winY = clampI16(px), clampI16(py)
	}
	xproto.CreateWindow(conn, screen.RootDepth, win, screen.Root,
		int16(winX), int16(winY), uint16(w), uint16(h), 1,
		xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwEventMask, []uint32{eventMask})

	gfx.PhaseTrace("x11.window_sent")
	gc, err := xproto.NewGcontextId(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	xproto.CreateGC(conn, gc, xproto.Drawable(win), 0, nil)

	s := &surface{
		conn: conn, win: win, root: screen.Root, gc: gc,
		events: make(chan gfx.Event, 256),
		evch:   make(chan xgb.Event, 256),
		errch:  make(chan error, 1),
		w:      w, h: h,
	}

	// 标题 (WM_NAME)
	xproto.ChangeProperty(conn, xproto.PropModeReplace, win, xproto.AtomWmName,
		xproto.AtomString, 8, uint32(len(cfg.Title)), []byte(cfg.Title))

	// WM_DELETE_WINDOW: 点关闭按钮发 ClientMessage 而不是硬断连接
	s.wmProtocols = internAtom(conn, "WM_PROTOCOLS")
	s.wmDelete = internAtom(conn, "WM_DELETE_WINDOW")
	if s.wmProtocols != 0 && s.wmDelete != 0 {
		var delBuf [4]byte
		binary.LittleEndian.PutUint32(delBuf[:], uint32(s.wmDelete))
		xproto.ChangeProperty(conn, xproto.PropModeReplace, win, s.wmProtocols,
			xproto.AtomAtom, 32, 1, delBuf[:])
	}

	gfx.PhaseTrace("x11.props_done")
	s.loadKeymap(setup)
	gfx.PhaseTrace("x11.keymap_done")

	// RandR: 为了能收到"显示器拓扑变了"的事件 (根窗口的 ScreenChangeNotify)。
	// 必须在 MapWindow 之前选事件, 且与 display.go 的 ensureRandr 共用同一张
	// 已初始化表 (不重复发 QueryExtension)。
	if ensureRandr(conn) {
		randr.SelectInputChecked(conn, setup.DefaultScreen(conn).Root, randr.NotifyMaskScreenChange).Check()
	}

	gfx.PhaseTrace("x11.randr_done")
	xproto.MapWindow(conn, win)
	conn.Sync() // 冲刷建窗请求并确认连接可用
	gfx.PhaseTrace("x11.mapped")

	// 新窗口播种密度缓存 (见 gfx/density.go 的 noteSurfaceCreated): 首帧之前
	// 就把"我落在哪块屏上"问清楚一次, 之后只在换屏事件里重问。
	gfx.NoteSurfaceCreated(s)

	// 独立读事件 goroutine: xgb 的 WaitForEvent 阻塞, 借通道转成可 select
	go func() {
		for {
			ev, err := conn.WaitForEvent()
			if err != nil {
				select {
				case s.errch <- err:
				default:
				}
				return
			}
			if ev == nil {
				select {
				case s.errch <- fmt.Errorf("x11 connection closed"):
				default:
				}
				return
			}
			s.evch <- ev
		}
	}()
	return s, nil
}

// loadKeymap 启动时一次性拉取键码→键符映射, 构建键码→键名表。
func (s *surface) loadKeymap(setup *xproto.SetupInfo) {
	s.keyNames = map[xproto.Keycode]string{}
	count := byte(setup.MaxKeycode - setup.MinKeycode + 1)
	if count == 0 {
		return
	}
	reply, err := xproto.GetKeyboardMapping(s.conn, setup.MinKeycode, count).Reply()
	if err != nil {
		return
	}
	symsPer := int(reply.KeysymsPerKeycode)
	if symsPer == 0 {
		return
	}
	for i := 0; i < int(count); i++ {
		ks := uint32(reply.Keysyms[i*symsPer]) // 首键符 (未修饰)
		if name := KeysymName(ks); name != "" {
			s.keyNames[setup.MinKeycode+xproto.Keycode(i)] = name
		}
	}
}

// KeysymName 键符 → 键名 (与 win32 后端同一套命名)。
// 可打印 ASCII 直接取字符; 功能键按 X11 标准键符区间映射。
func KeysymName(ks uint32) string {
	switch {
	case ks >= 0x20 && ks <= 0x7E:
		return string(rune(ks))
	case ks >= 0x01000100 && ks <= 0x0100ffff: // Latin-1 补充起的 Unicode 区
		return string(rune(ks - 0x01000000))
	}
	switch ks {
	case 0xFF08:
		return "Backspace"
	case 0xFF09:
		return "Tab"
	case 0xFF0D:
		return "Enter"
	case 0xFF1B:
		return "Escape"
	case 0xFF50:
		return "Home"
	case 0xFF51:
		return "ArrowLeft"
	case 0xFF52:
		return "ArrowUp"
	case 0xFF53:
		return "ArrowRight"
	case 0xFF54:
		return "ArrowDown"
	case 0xFF55:
		return "PageUp"
	case 0xFF56:
		return "PageDown"
	case 0xFF57:
		return "End"
	case 0xFFFF:
		return "Delete"
	}
	if ks >= 0xFFBE && ks <= 0xFFC9 { // F1..F12
		return fmt.Sprintf("F%d", ks-0xFFBE+1)
	}
	return ""
}

func internAtom(conn *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return r.Atom
}

func (s *surface) Events() <-chan gfx.Event { return s.events }

// SetTitle 实现 gfx 的可选 windowController 接口: 改 WM_NAME。
// (X11 未实机验证 —— 与本文件其余部分同一状态, 见 agent_doc/gui-component-status.md §1.6; agent_doc/ 已迁往项目共享资产盘, 仓库不留副本)
func (s *surface) SetTitle(title string) {
	xproto.ChangeProperty(s.conn, xproto.PropModeReplace, s.win, xproto.AtomWmName,
		xproto.AtomString, 8, uint32(len(title)), []byte(title))
}

// ResizeClient 实现 gfx 的可选 windowController 接口: ConfigureWindow 改
// 窗口尺寸 (X11 无客户区/外框之分)。ConfigureNotify 随之到来 → EventResize。
func (s *surface) ResizeClient(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	xproto.ConfigureWindow(s.conn, s.win,
		uint16(xproto.ConfigWindowWidth|xproto.ConfigWindowHeight),
		[]uint32{uint32(w), uint32(h)})
}

// MoveTo 实现 gfx 的可选 windowMover 接口 (M4): 把窗口左上角移到虚拟桌面
// 绝对坐标 (x,y)。X11 里也就是相对**根窗口**的坐标 —— ConfigureWindow 的
// x/y 正是这个口径, gfx 层已把"工作区相对"换算好。
func (s *surface) MoveTo(x, y int) error {
	xproto.ConfigureWindow(s.conn, s.win,
		uint16(xproto.ConfigWindowX|xproto.ConfigWindowY),
		[]uint32{uint32(int32(x)), uint32(int32(y))})
	return nil
}

// WindowBounds 实现 gfx 的可选 windowBoundsProvider 接口 (M4): 经
// TranslateCoordinates 拿窗口相对**根窗口**的位置 (跨过 WM 的 frame), 再加
// 客户区尺寸。
func (s *surface) WindowBounds() (x, y, w, h int, ok bool) {
	reply, err := xproto.TranslateCoordinates(s.conn, s.win, s.root, 0, 0).Reply()
	if err != nil {
		return 0, 0, 0, 0, false
	}
	cw, ch := s.Size()
	return int(reply.DstX), int(reply.DstY), cw, ch, true
}

// Size 返回当前窗口尺寸 (ConfigureNotify 维护)。
func (s *surface) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

// Show 整帧上屏。
func (s *surface) Show(img *image.RGBA) { s.ShowRegions(img, nil) }

// ShowRegions 逐区域 PutImage 上屏 (rects 空 = 整帧)。
func (s *surface) ShowRegions(img *image.RGBA, rects []image.Rectangle) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if len(rects) == 0 {
		rects = []image.Rectangle{img.Bounds()}
	}
	for _, r := range rects {
		r = r.Intersect(img.Bounds())
		if r.Empty() {
			continue
		}
		// PutImage 的请求长度字段是 16 位 (4 字节单位, 上限 256KB 总长)。
		// 一个 >256KB 的区域 (500x300 RGBA 就有 600KB) 单次发送会被 X
		// 服务器回 BadLength 并**掐断整条连接** —— 症状是窗口打开一两秒后
		// 静默消失、进程正常退出、stderr 全空 (S0 折叠屏 demo 在桌面首绘
		// 即崩的根因)。所以按行带切分, 每片数据远小于上限。
		const maxPutImageBytes = 128 * 1024 // 每请求图像数据上限 (含安全余量)
		stride := r.Dx() * 4
		rowsPerReq := maxPutImageBytes / stride
		if rowsPerReq < 1 {
			rowsPerReq = 1 // 单行就超限的极端窄高区域: 至少按行发
		}
		s.pixMu.Lock()
		for y := r.Min.Y; y < r.Max.Y; y += rowsPerReq {
			y2 := y + rowsPerReq
			if y2 > r.Max.Y {
				y2 = r.Max.Y
			}
			band := image.Rect(r.Min.X, y, r.Max.X, y2)
			// 复用本 surface 的转换缓冲 (T3-5 第二十六批): 320x200 每帧 256KB,
			// 4K 每帧 33MB —— 每次都 make 一遍等于每帧额外付一次"清零 + 交给 GC"。
			// 实测 320x200 的转换从 175µs 降到 45µs, 其中 33µs 是省掉的 make。
			// 安全前提: xproto.PutImage 会把 Data **拷贝**进请求缓冲再写 socket
			// (xproto.putImageRequest 里的 copy), 返回后本函数不再持有它。
			buf := s.scratchPix(band.Dx() * band.Dy() * 4)
			buf = rgbaToXZPixmapInto(img, band, buf)
			xproto.PutImage(s.conn, xproto.ImageFormatZPixmap, xproto.Drawable(s.win),
				s.gc, uint16(band.Dx()), uint16(band.Dy()), int16(band.Min.X), int16(band.Min.Y),
				0, 24, buf)
		}
		s.pixMu.Unlock()
	}
	gfx.PhaseTrace("x11.pixmap_done")
	s.conn.Sync() // 确保请求已送达 (xgb 无独立 flush 入口)
	gfx.PhaseTrace("x11.present_done")
}

// scratchPix 从 surface 上取一块长度 n 的复用缓冲 (只增不减)。
// 调用方必须持 s.pixMu —— 缓冲是共享的, 且 PutImage 返回前不能被下一带覆盖。
func (s *surface) scratchPix(n int) []byte {
	if cap(s.pixBuf) < n {
		s.pixBuf = make([]byte, n)
	}
	return s.pixBuf[:n]
}

// RGBAToXZPixmap 把 img 的 r 区域转为 X11 ZPixmap(24 深度, 32bpp)字节流:
// 小端客户端字节序下每像素 4 字节 = B,G,R,unused。与 win32 DIB 同构。
//
// 本函数自己分配返回缓冲 (导出 API, 调用方拥有返回值)。上屏热路径走
// rgbaToXZPixmapInto + surface 的复用缓冲, 不在这里每帧 make 一次。
func RGBAToXZPixmap(img *image.RGBA, r image.Rectangle) []byte {
	w := r.Dx()
	if w <= 0 || r.Dy() <= 0 {
		return nil
	}
	return rgbaToXZPixmapInto(img, r, make([]byte, w*r.Dy()*4))
}

// rgbaToXZPixmapInto 把 img 的 r 区域写进 dst (长度必须是 w*h*4), 返回 dst。
//
// 行首偏移按 **img.Bounds().Min** 归一 (T3-5 第二十六批): 改动前直接用
// r.Min.Y*Stride + r.Min.X*4, 隐含"图像 Bounds 起点是 (0,0)"。gfx 的帧缓冲
// 确实是 image.NewRGBA(Rect(0,0,w,h)), 所以线上从没踩到, 但这是导出函数,
// 传一张 Bounds 不带原点的图就会越界 panic (闸门测试第一版就撞上了)。
// 归一只让"本来就对的情况"保持一模一样, 不改变任何现网输出。
func rgbaToXZPixmapInto(img *image.RGBA, r image.Rectangle, dst []byte) []byte {
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return dst[:0]
	}
	baseX := r.Min.X - img.Rect.Min.X
	baseY := r.Min.Y - img.Rect.Min.Y
	if nativeLittleEndian {
		for y := 0; y < h; y++ {
			src := img.Pix[(baseY+y)*img.Stride+baseX*4:][:w*4]
			swizzleRowLE(src, dst[y*w*4:(y+1)*w*4])
		}
		return dst[:w*h*4]
	}
	// 大端主机: 保持逐字节实现, 慢但对 (ZPixmap 的字节布局是"客户端字节序",
	// 而下面的常量位移是按小端写的, 不能直接套)。
	for y := 0; y < h; y++ {
		src := img.Pix[(baseY+y)*img.Stride+baseX*4:][:w*4]
		row := dst[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			i := x * 4
			row[i] = src[i+2]   // B
			row[i+1] = src[i+1] // G
			row[i+2] = src[i]   // R
			row[i+3] = 0        // unused
		}
	}
	return dst[:w*h*4]
}

// nativeLittleEndian 在包初始化时定一次, 供上屏路径选实现。
// 不用 unsafe 读内存的替代方案 (encoding/binary) 会把一次 load 变成边界检查 +
// 拼装, 在这条每帧几十万像素的循环里差 2~3 倍 —— 而这里要换的恰好只是字节序。
var nativeLittleEndian = func() bool {
	var x uint16 = 1
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// swizzleRowLE 把一行 RGBA (小端 uint32 = A<<24|B<<16|G<<8|R) 重排成
// BGRX (小端 uint32 = 0<<24|R<<16|G<<8|B), 一次处理 32 位。
//
// 为什么按字而不是按字节: 逐字节版每像素 4 次 load + 4 次 store + 3 次移位,
// 在 320x200 上是 175µs (1.46GB/s); 按字版每像素 1 次 load + 1 次 store +
// 3 次位运算, 同一台机器上约 4 倍。4K 全帧 (33MB) 从 ~22ms 降到 ~6ms 量级,
// 这是"多屏/高 DPI 下每帧都在付"的钱, 不只是冷启动那一次。
//
// 调用方保证 len(src) == len(dst) == 4*像素数 且首地址 4 字节对齐
// (image.RGBA 的 Pix 起点由 make 对齐, 行跨度是 4 的倍数, baseX*4 也是)。
func swizzleRowLE(src, dst []byte) {
	sw := unsafe.Slice((*uint32)(unsafe.Pointer(&src[0])), len(src)/4)
	dw := unsafe.Slice((*uint32)(unsafe.Pointer(&dst[0])), len(dst)/4)
	for i, v := range sw {
		// v 的字节: [R][G][B][A] → 目标字节 [B][G][R][0]
		dw[i] = (v>>16)&0x000000FF | v&0x0000FF00 | (v&0x000000FF)<<16
	}
}

// WaitEvents 等待 X 事件至多 maxWait (<=0 无限期), 翻译为 gfx.Event 投递。
// 连接断开/关闭返回 false。
func (s *surface) WaitEvents(maxWait time.Duration) bool {
	return s.waitEvents(maxWait, nil)
}

// WaitEventsWake 实现 gfx 的 waker 可选接口: 与 WaitEvents 同义, 但额外
// 监听 wake (gfx.Post 的跨线程唤醒信号), 读到值时立即返回 —— 否则单窗口
// 空闲时泵会带着无限期预算睡死, 跨线程 Post 的任务要等到恰好有 X 事件才
// 被执行。X11 这边直接把它作为一个 select 分支即可 (wake 为 nil 时该分支
// 自动禁用, 正是普通 WaitEvents 的语义)。
func (s *surface) WaitEventsWake(maxWait time.Duration, wake <-chan struct{}) bool {
	return s.waitEvents(maxWait, wake)
}

// waitEvents 是 WaitEvents / WaitEventsWake 的公共实现。
func (s *surface) waitEvents(maxWait time.Duration, wake <-chan struct{}) bool {
	deadline := time.Time{}
	if maxWait > 0 {
		deadline = time.Now().Add(maxWait)
	}
	for {
		var timer <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return true // 超时: 交回事件循环 (可能有定时器到期)
			}
			timer = time.After(d)
		}
		select {
		case <-s.errch:
			s.markClosed()
			return false
		case ev := <-s.evch:
			if !s.translate(ev) {
				return false
			}
		case <-wake:
			// 跨线程 Post 唤醒: 立即交回 Pump, 本轮 DrainTasks 执行任务。
			return true
		case <-timer:
			return true
		}
	}
}

// 编译期断言: surface 满足 gfx 的 waker 可选能力 (签名一变先在这里报错)。
var _ interface {
	WaitEventsWake(time.Duration, <-chan struct{}) bool
} = (*surface)(nil)

// markClosed 关闭标记 (连接已不可用)。
func (s *surface) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// translate 把 X 事件翻译为 gfx.Event; 返回 false 表示应关闭。
//
// 注意事件类型全是**值**而不是指针: jezek/xgb 的 xproto.*EventNew 返回的是
// 值类型 (return v, 不是 &v), 写成 `case *xproto.ButtonPressEvent` 时 type
// switch 永远不匹配 —— 编译期完全合法, 运行期表现为"这段分支是死代码",
// 窗口画出来了却对鼠标/键盘毫无反应, 没有任何报错。这正是 X11 后端在桌面
// 上"能看不能点"的根因, 改动时不要给这些 case 加 `*`。
func (s *surface) translate(ev xgb.Event) bool {
	if p, ok := ev.(*xproto.ButtonPressEvent); ok && p != nil {
		ev = *p // 指针传入也归一成值, 兼容调用方不小心取址
	}
	switch e := ev.(type) {
	case randr.ScreenChangeNotifyEvent:
		// 显示器拓扑变了 (插拔 / 改分辨率 / RandR 重配): 屏表与"窗口落在哪块屏"
		// 都可能变。两者都经 Post 转投 —— 它们要跑脚本回调, 不能在事件读协程里做。
		// NotifyDisplaysChanged 内部作废密度缓存 (见 density.go), 随后的
		// PostWindowDisplayCheck 把窗口的新归属重新播种。
		gfx.Post(func() { gfx.NotifyDisplaysChanged() })
		gfx.PostWindowDisplayCheck(s)
	case xproto.ClientMessageEvent:
		// WM_DELETE_WINDOW → 关闭
		if e.Type == s.wmProtocols && len(e.Data.Data32) > 0 &&
			xproto.Atom(e.Data.Data32[0]) == s.wmDelete {
			s.trySend(gfx.Event{Kind: gfx.EventClose})
		}
	case xproto.ButtonPressEvent:
		// 滚轮的 Shift 状态在事件 state 位域里 (与键盘事件同一套解析),
		// Scroll 容器据此做 Shift+滚轮横向滚动 (rSkhXA)。
		_, wheelShift, _ := xModifiers(e.State)
		switch e.Detail {
		case 1: // 左键
			s.trySend(gfx.Event{Kind: gfx.EventMouseDown, X: int(e.EventX), Y: int(e.EventY)})
		case 4: // 滚轮上
			s.trySend(gfx.Event{Kind: gfx.EventMouseWheel, X: int(e.EventX), Y: int(e.EventY), DeltaY: wheelDelta, Shift: wheelShift})
		case 5: // 滚轮下
			s.trySend(gfx.Event{Kind: gfx.EventMouseWheel, X: int(e.EventX), Y: int(e.EventY), DeltaY: -wheelDelta, Shift: wheelShift})
		}
	case xproto.ButtonReleaseEvent:
		switch e.Detail {
		case 1:
			s.trySend(gfx.Event{Kind: gfx.EventMouseUp, X: int(e.EventX), Y: int(e.EventY)})
		case 3: // 右键
			s.trySend(gfx.Event{Kind: gfx.EventMouseRightUp, X: int(e.EventX), Y: int(e.EventY)})
		}
	case xproto.MotionNotifyEvent:
		s.trySend(gfx.Event{Kind: gfx.EventMouseMove, X: int(e.EventX), Y: int(e.EventY)})
	case xproto.LeaveNotifyEvent:
		s.trySend(gfx.Event{Kind: gfx.EventMouseLeave})
	case xproto.KeyPressEvent:
		if name, ok := s.keyNames[e.Detail]; ok {
			ctrl, shift, alt := xModifiers(e.State)
			s.trySend(gfx.Event{Kind: gfx.EventKeyDown, Key: name, Ctrl: ctrl, Shift: shift, Alt: alt})
		}
	case xproto.KeyReleaseEvent:
		if name, ok := s.keyNames[e.Detail]; ok {
			ctrl, shift, alt := xModifiers(e.State)
			s.trySend(gfx.Event{Kind: gfx.EventKeyUp, Key: name, Ctrl: ctrl, Shift: shift, Alt: alt})
		}
	case xproto.ConfigureNotifyEvent:
		s.mu.Lock()
		resized := int(e.Width) != s.w || int(e.Height) != s.h
		moved := int(e.X) != s.x || int(e.Y) != s.y
		s.w, s.h = int(e.Width), int(e.Height)
		s.x, s.y = int(e.X), int(e.Y)
		s.mu.Unlock()
		if resized {
			s.trySend(gfx.Event{Kind: gfx.EventResize, W: int(e.Width), H: int(e.Height)})
			// M4: 尺寸变了 → 让 gx/viewport 的断点/尺寸类订阅者重算。
			gfx.Post(func() { gfx.NotifyViewportChanged() })
		}
		// 移动 (§四 窗口/系统缺口): ConfigureNotify 的 X/Y 是**相对父窗口**
		// 的坐标, 而 gfx 的 EventMove 口径是屏幕 (根窗口) 坐标。有 WM 重定
		// 父 (reparenting) 时两者差着标题栏/边框, 直接转发是错的 —— 所以
		// 这里做一次 TranslateCoordinates 换算。
		//
		// 往返是安全的: xgb 的回复由它自己的读协程按序号分发, 本函数虽在
		// VM 线程上跑 (事件由读协程经通道递过来), 也不会与事件读抢 socket。
		if moved {
			root := xproto.Setup(s.conn).DefaultScreen(s.conn).Root
			if rep, err := xproto.TranslateCoordinates(s.conn, s.win, root, 0, 0).Reply(); err == nil && rep != nil {
				s.trySend(gfx.Event{Kind: gfx.EventMove, X: int(rep.DstX), Y: int(rep.DstY)})
			}
		}
		// M4: 位置也可能变了 (用户拖动 / ConfigureWindow) → 让内核比对是否换屏。
		//
		// 合流备注 (2026-10-02): 上面 §四 的 EventMove 与这句刻意并存 —— 两者
		// 服务于不同消费者, 不是重复: EventMove 是**给脚本**的"窗口动了"通知
		// (屏幕绝对坐标); PostWindowDisplayCheck 是**内核自己**判断"跨屏了没"
		// (决定该窗口归属哪个 display, 直接影响 position()/bounds() 报的
		// displayId 与 HiDPI 缩放)。删掉任何一个都会丢一条链路。
		gfx.PostWindowDisplayCheck(s)
	}
	return true
}

// wheelDelta 与 Windows 的 WHEEL_DELTA 对齐 (一格 120), 让脚本不必区分平台。
const wheelDelta = 120

// xModifiers 从 X 事件 state 位域取修饰键。
// ModMask1 是传统 Alt 所在的修饰位 (与 win32 的 VK_MENU 对应)。
func xModifiers(state uint16) (ctrl, shift, alt bool) {
	return state&xproto.ModMaskControl != 0,
		state&xproto.ModMaskShift != 0,
		state&xproto.ModMask1 != 0
}

// trySend 非阻塞投递事件 (通道满则丢弃)。
func (s *surface) trySend(ev gfx.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// 编译期断言: surface 满足 gfx 的原生窗口句柄可选能力。
var _ gfx.NativeWindowProvider = (*surface)(nil)

// NativeWindowTarget 交出 X Window id + Xlib Display* (gfx.NativeWindowProvider)。
//
// Display* 来自 dlopen libX11 的 XOpenDisplay (见 xlib.go, purego 零 cgo,
// 2026-11-18 落地)。打不开 Xlib (无库 / 无 X server) 时 Display 恒 0,
// GPU 后端看到 0 会拒绝建 swapchain —— 窗口照常走 CPU blit, 行为与
// 没有这个接口时完全一致。真机点亮属于 G-3 三平台验证的硬件覆盖项。
func (s *surface) NativeWindowTarget() (gfx.NativeWindowTarget, bool) {
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win == 0 {
		return gfx.NativeWindowTarget{}, false
	}
	return gfx.NativeWindowTarget{
		Kind:    "x11",
		Display: xlibDisplay(), // dlopen Xlib (xlib.go); 0 = 不可用, GPU 侧 gate 拒绝建链
		Window:  uintptr(win),
	}, true
}
