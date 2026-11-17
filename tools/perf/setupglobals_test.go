// 解释器初始化 (SetupGlobals) 的微基准 (T3-5 冷启动线)。
//
// 为什么单列一个只测 SetupGlobals 的基准：冷启动两条口径（进程级
// scripts/coldstart.sh、进程内 tools/perf TestColdStartToFirstFrame）都被
// GUI 装载与模块解析主导，解释器初始化本身只占其中一小段 —— 在那里做 A/B，
// 改动哪怕真有 10%，也会被 ~1ms 的噪声吃掉。要看「初始化惰性化」这类改动的
// 真实收益，必须把 SetupGlobals 单独拎出来测。
//
// 用法:
//
//	go test ./tools/perf/ -run XXX -bench SetupGlobals -benchtime 2000x -count=5
//
// 口径: 每轮新建一个完整的全局环境 (与进程真实启动时的构造量一致)，报
// ns/op、B/op、allocs/op。allocs/op 是冷启动线最敏感的指标 —— 初始化期间的
// 分配会直接抬高 GC 起点与首帧抖动。
package perf

import (
	"testing"

	"github.com/14752222/Gox/stdlib"
)

// BenchmarkSetupGlobals 只测「解释器初始化」：建全局环境 + 全部内建对象树，
// 不含脚本解析、求值、窗口创建。
func BenchmarkSetupGlobals(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if env := stdlib.SetupGlobals(); env == nil {
			b.Fatal("SetupGlobals 返回 nil")
		}
	}
}
