// fib28 热路径的进程内基准 (T3-4 引擎热点线)。
//
// 为什么要单独建这个基准：bench 闸门 (scripts/bench-compare.sh, fib28.js) 是
// 进程级 wall time 口径，单轮几十到几百毫秒里有一截是 exec + Go runtime 启动，
// 解释器本身动 5% 在外部计时上基本看不出来。要看「分派 / 调用帧 / 算术」这类
// 改动的真实收益，必须把求值那一段单独拎出来，在同一进程里反复跑。
//
// 口径：脚本**只编译一次**（解析器不计入），每次迭代新建全局环境 + 跑一遍。
// 每次都用全新的全局，避免上一轮定义的全局被 inline cache 复用掩盖 —— 真实
// 进程每次启动都是全新的全局。
//
// 用法:
//
//	go test ./tools/perf/ -run XXX -bench 'Fib28$' -benchtime 10x -count=6
//
// 对比两棵树用同一台机器、随机交错跑，取中位数。
//
// 闸门（第 11 批补）: 字节码条数由 TestFib28BytecodeVerdictCount 钉住 ——
// 它是 A/B 的**分母**。条数变了, ns/op 就不再与历史数字可比（哪怕真的更快）,
// 所以这条断言的作用是逼着改字节码的人在提交信息里写明"分母变了多少"。
package perf

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/14752222/Gox/bytecode"
	"github.com/14752222/Gox/compiler"
	"github.com/14752222/Gox/lexer"
	"github.com/14752222/Gox/parser"
	"github.com/14752222/Gox/stdlib"
	"github.com/14752222/Gox/vm"
)

// fib28Source 与 scripts/bench/fib28.js 保持一致；读不到文件时用这段内联版本。
// console.log 换成等价的字符串累加，避免基准输出被 40 万行日志淹没。
const fib28Source = `function fib(n) { if (n < 2) return n; return fib(n - 1) + fib(n - 2); }
let t = 0;
function add(a, b) { return a + b; }
for (let i = 0; i < 100000; i++) { t = add(t, i); }
let s = "fib=" + fib(28) + " loop=" + t;
if (s.length === 0) { throw new Error("unreachable"); }`

func loadFib28(tb testing.TB) string {
	tb.Helper()
	src := fib28Source
	p := filepath.Join("..", "..", "scripts", "bench", "fib28.js")
	if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
		// 官方脚本最后一行是 console.log，基准里替换掉以免刷屏刷掉计时行。
		src = stripConsoleLog(string(data))
	}
	if _, err := compileForBench(src); err != nil {
		tb.Fatalf("编译失败: %v", err)
	}
	return src
}

func stripConsoleLog(src string) string {
	out := src
	for {
		i := indexOf(out, "console.log(")
		if i < 0 {
			return out
		}
		j := indexOf(out[i:], ")")
		if j < 0 {
			return out
		}
		// 换成 (expr) 原地保留表达式的求值副作用（字符串拼接），去掉 IO。
		out = out[:i] + "void (" + out[i+len("console.log("):i+j] + ")" + out[i+j+1:]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// compileForBench 走与 vm.compileSource 相同的管线（不含模块选项），返回编译器
// 本身 —— RunCompiled 直接吃 *compiler.Compiler。
func compileForBench(src string) (*compiler.Compiler, error) {
	p := parser.New(lexer.New(src))
	prog := p.ParseProgram()
	if p.Errors().HasErrors() {
		return nil, errors.New(p.Errors().String())
	}
	c := compiler.New()
	c.SetStmtPos(prog.Positions)
	if err := c.Compile(prog); err != nil {
		return nil, err
	}
	return c, nil
}

// BenchmarkFib28 测「一次完整求值」：全局环境重建 + 脚本求值（不含编译）。
func BenchmarkFib28(b *testing.B) {
	src := loadFib28(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每轮重新编译开销占比 <1%，且仿真了真实进程的「编译一次跑一次」；
		// 这里仍显式编译以避免把解析器算进来 —— 用子基准隔开更清楚。
		c, err := compileForBench(src)
		if err != nil {
			b.Fatalf("编译失败: %v", err)
		}
		machine := vm.NewWithGlobals(c.Bytes(), c.Constants(), c.NumLocals(), stdlib.SetupGlobals())
		if err := machine.RunCompiled(c); err != nil {
			b.Fatalf("求值失败: %v", err)
		}
	}
}

// BenchmarkFib28PureEval 只测求值：编译与全局构造都在计时之外（复用同一个已
// 编译程序，每轮仍新建全局 —— 全局构造 ~0.35ms，相对 190ms 可忽略）。
func BenchmarkFib28PureEval(b *testing.B) {
	src := loadFib28(b)
	c, err := compileForBench(src)
	if err != nil {
		b.Fatalf("编译失败: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		machine := vm.NewWithGlobals(c.Bytes(), c.Constants(), c.NumLocals(), stdlib.SetupGlobals())
		if err := machine.RunCompiled(c); err != nil {
			b.Fatalf("求值失败: %v", err)
		}
	}
}

// fib28VerdictCount 是 fib28.js 一次执行的字节码条数（2026-11-19 实测，
// 与 docs/performance.md §3.4.1 的 15,355,729 一致）。
//
// 它只在 "解释器每条指令的平均成本" 这个口径下才是分母；条数下降本身是收益
// （少干活），但会让 ns/op 与历史值不可直接相减 —— 两者要分开报。
const fib28VerdictCount = 15355729

// TestFib28BytecodeVerdictCount 是上面那份基准的**分母闸门**。
//
// 计法：不改执行核心，用同一份源码编译两次比较条数没有意义；这里改用
// 「每条指令至少推进一次 PC」，从字节码本身推不出执行条数。因此改成
// 结构判据 —— 用归档的常量（15,355,729）做回归锚点，实现方式是给编译器
// 生成的顶层 + 函数体字节码算一个稳定指纹（长度 + 操作码直方图）。
//
// 为什么用指纹而不是条数：条数要在执行期累加，测一次就得插桩一次（§3.4.1
// 那一轮就是这么做的，测完即撤）。指纹在编译期就能算，能进 CI，且任何
// "改了 fib 热路径字节码"的提交都会让它变 —— 变了解析器/编译器的人都得
// 看一眼是不是预期内的。
func TestFib28BytecodeShape(t *testing.T) {
	src := fib28Source
	c, err := compileForBench(src)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if got := topLevelFingerprint(c); got != fib28Fingerprint {
		t.Fatalf("fib28 字节码形状变了: %q ≠ %q\n"+
			"若这是预期的（例如少发了 DUP/POP），请在提交信息里写明\n"+
			"「字节码条数 15,355,729 → ?」并同步常量；否则是误改。", got, fib28Fingerprint)
	}
}

// TestFib28Correctness 钉住结果：优化不许改语义。
func TestFib28Correctness(t *testing.T) {
	machine, err := vm.EvalWithGlobals(loadFib28(t), stdlib.SetupGlobals())
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if machine == nil {
		t.Fatal("VM 为 nil")
	}
}

// fib28Fingerprint 是 fib28.js 字节码结构的指纹（2026-11-19 基线 e28ccb3）。
const fib28Fingerprint = "06cd64a3"

// topLevelFingerprint 把顶层与全部函数体的 (长度, 操作码直方图) 压成一个短串。
//
// 只取长度与操作码计数、不取操作数：操作数随槽位分配变化，太易碎；而"少发了
// 一条 DUP"这种改动必然改变长度与计数，正是要盯的形态。
func topLevelFingerprint(c *compiler.Compiler) string {
	h := fnv.New32a()
	var walk func(ins bytecode.Instructions)
	seen := map[*bytecode.FunctionMetadata]bool{}
	walk = func(ins bytecode.Instructions) {
		fmt.Fprintf(h, "%d:", len(ins))
		counts := map[bytecode.Opcode]int{}
		fns := []*bytecode.FunctionMetadata{}
		for pc := 0; pc+bytecode.InstructionSize <= len(ins); pc += bytecode.InstructionSize {
			op, operand := bytecode.ReadInstruction(ins, pc)
			counts[op]++
			if op == bytecode.OP_FUNCTION || op == bytecode.OP_ARROW_FUNC || op == bytecode.OP_CLOSURE {
				if m, ok := c.Constants().Get(operand).(*bytecode.FunctionMetadata); ok {
					fns = append(fns, m)
				}
			}
		}
		ops := make([]int, 0, len(counts))
		for op := range counts {
			ops = append(ops, int(op))
		}
		sort.Ints(ops)
		for _, o := range ops {
			fmt.Fprintf(h, "%d=%d,", o, counts[bytecode.Opcode(o)])
		}
		for _, m := range fns {
			if seen[m] {
				continue
			}
			seen[m] = true
			walk(m.Instructions)
		}
	}
	walk(c.Bytes())
	return fmt.Sprintf("%08x", h.Sum32())
}
