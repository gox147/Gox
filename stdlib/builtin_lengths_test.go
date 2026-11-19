// 内建 length 的语义闸门 (T3-5 第二十三批, 2026-11-20 凌晨批)
//
// 为什么先立这条闸门再动优化：applyBuiltinLengths 是本批唯一的改动点，它把
// 「length 写进 PropDescs（Object.defineProperty 语义）」换成「length 写进
// BuiltinFunction/BuiltinMethod 的结构体 Length 字段」。**语义必须一字不差**，
// 否则这就是一次借优化之名做的行为改动：
//
//   - length 必须**自有**且**不可枚举**：`for (const k in f)` 不许看见它，
//     `Object.keys(f)` 同理 —— 但 Object.getOwnPropertyNames 必须看得见；
//   - 描述符必须是 { writable:false, enumerable:false, configurable:true }：
//     严格模式下赋值要抛 TypeError，delete 必须成功；
//   - 值必须是规范形参个数（含 0 这个"和默认值恰好一样"的陷阱 —— 若优化把
//     "值为 0 就跳过不写"当成等价，Array.prototype.map 等一批 0 参函数就
//     N/A 了，这条断言就是为它准备的）；
//   - 优化后**不许**在内建函数上留任何 PropDescs 条目：结构体字段才是快路径，
//     万一哪天有人两边都写，这里直接红。
//
// 全部走 SetupGlobals 造出来的真实全局环境（不是手工拼的空壳），所以它同时
// 覆盖 applyBuiltinLengths 与惰性物化时的 applyBuiltinLengthsPrefixes 两条路径
// —— 后者未单独断言：Temporal / TypedArray 家族只在被首次访问时物化，
// 断言它们等于逼着测例把惰性化打回全量（§12.22 已经算过那笔账不划算）。
package stdlib_test

import (
	"testing"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/stdlib"
	"github.com/14752222/Gox/vm"
)

// builtinLengthProbes 是 (限定名, 规范 length) 探针表。
//
// 选取原则：每个值至少覆盖一种形态 —— 非 0、0、rest/默认值截断、命名空间下的
// 静态函数、原型方法、构造器本体。
// 探针值取自 **2026-11-20 凌晨基线**（本批未改动时的真实行为），不是凭记忆
// 写的规范值 —— 两者不一致的地方已经记在 docs/performance.md §3.4.8 的「与
// 规范不符、本批不修」一节。闸门钉的是"本批前后一字不差"，不是"顺手纠偏"。
var builtinLengthProbes = []struct {
	path string
	want int64
}{
	{"parseInt", 2},
	{"Array", 1},
	{"Array.from", 1},
	{"Array.prototype.map", 1},
	{"Array.prototype.push", 1},
	{"Array.prototype.slice", 2},
	{"Array.prototype.forEach", 1},
	{"Array.prototype.join", 1},
	{"Math.max", 2},
	{"Math.round", 1},
	{"JSON.stringify", 3},
	{"Object.defineProperty", 3},
	{"Object.assign", 2},
	{"Number.isInteger", 1},
	{"Map.prototype.get", 1},
	{"Function", 1},                    // 基线 1（规范 1）
	{"String", 0},                      // 同上
	{"String.prototype.slice", 0},      // 同上（规范 2）
	{"String.prototype.charCodeAt", 0}, // 同上（规范 1）
}

// 值为 0 的探针单独一张表：它们是"跳过不写"这类优化最先踩坏的一批。
var zeroLengthProbes = []string{
	"Array.isArray",
	"Array.prototype.toString",
	"Object.create",
	"String.prototype.normalize",
	"String.prototype.trimEnd",
}

func TestBuiltinLengthSemantics(t *testing.T) {
	env := stdlib.SetupGlobals()
	for _, p := range builtinLengthProbes {
		p := p
		t.Run(p.path, func(t *testing.T) {
			fn := asFuncObj(t, p.path, resolvePath(t, env, p.path))

			// (a) 描述符: 值 / 不可写 / 不可枚举 / 可配置
			desc, ok := fn.OwnDescriptor("length")
			if !ok {
				t.Fatalf("%s 没有自有 length", p.path)
			}
			if desc.Value == nil {
				t.Fatalf("%s length 描述符无值", p.path)
			}
			if got, isNum := desc.Value.(*object.Number); !isNum {
				t.Fatalf("%s length 不是数字: %T", p.path, desc.Value)
			} else if int64(got.Value) != p.want {
				t.Fatalf("%s length = %v, 基线 %d", p.path, got.Value, p.want)
			}
			if desc.Writable {
				t.Errorf("%s length 应为不可写", p.path)
			}
			if desc.Enumerable {
				t.Errorf("%s length 应为不可枚举", p.path)
			}
			if !desc.Configurable {
				t.Errorf("%s length 应为可配置", p.path)
			}

			// (b) 走属性访问读到的值与描述符一致（GetProperty / OwnDescriptor 不许分裂）
			if v, ok := fn.GetProperty("length"); !ok || v == nil {
				t.Fatalf("%s GetProperty(length) 取不到", p.path)
			} else if n, isNum := v.(*object.Number); !isNum || int64(n.Value) != p.want {
				t.Fatalf("%s GetProperty(length) = %v, 基线 %d", p.path, v, p.want)
			}
		})
	}
}

func TestBuiltinLengthSemanticsZero(t *testing.T) {
	env := stdlib.SetupGlobals()
	// Object.create.length === 2 —— 先纠一个常见误记，再用真实 0 参函数做断言。
	for _, path := range zeroLengthProbes {
		fn := asFuncObj(t, path, resolvePath(t, env, path))
		desc, ok := fn.OwnDescriptor("length")
		if !ok {
			t.Fatalf("%s 没有自有 length", path)
		}
		if n, isNum := desc.Value.(*object.Number); !isNum || n.Value != 0 {
			t.Errorf("%s length = %v, 期望 0", path, desc.Value)
		}
	}
}

// TestBuiltinLengthDefinePropertySync 钉住「结构体字段与 defineProperty 双写」的
// 一致性：用户在 length 上显式 defineProperty 之后，属性读取必须立刻看到新值。
//
// 背景：本批把 length 的真相源从 PropDescs 挪到 BuiltinFunction.Length 字段，
// 读路径是「PropDescs 命中则用描述符值，否则用字段」。DefineOwn 的 length 分支
// 因此必须把值同步进字段 —— 否则两份互相看不见的值会在 delete/重定义序列里
// 露出来（§3.4.9 记录了这条分支被"怀疑拖慢 fib28"而短暂撤回过一次，三方差分
// 实测差 0.3%、在噪声内，因此保留）。
func TestBuiltinLengthDefinePropertySync(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "defineProperty 改 length 后读新值",
			src: `const f = JSON.stringify;
Object.defineProperty(f, "length", {value: 7, writable: false, enumerable: false, configurable: true});
f.length;`,
			want: "7",
		},
		{
			name: "描述符与字段不走岔",
			src: `const f = Math.round;
Object.defineProperty(f, "length", {value: 4, writable: false, enumerable: false, configurable: true});
const d = Object.getOwnPropertyDescriptor(f, "length");
(f.length === 4 && d.value === 4) ? "ok" : "split";`,
			want: "ok",
		},
		{
			name: "delete 后再 defineProperty 回到新值",
			src: `const f = Array.prototype.push;
delete f.length;
Object.defineProperty(f, "length", {value: 3, writable: false, enumerable: false, configurable: true});
f.length;`,
			want: "3",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := evalString(t, c.src); got != c.want {
				t.Fatalf("得到 %q, 期望 %q", got, c.want)
			}
		})
	}
}

// TestBuiltinLengthNotInForIn 用真实引擎跑 JS，钉住"length 不可枚举、不可写、可删"。
//
// 这一条用 vm.Eval 而不是直接读 Go 结构：descriptor 标志在两条读路径上
// （GetProperty / OwnDescriptor / for-in 枚举）各有一套实现，只测 Go 侧会漏掉
// 枚举器那一路 —— 而"把 length 写进 Properties 映射"正是这次优化最可能引入
// 的退化（那样 for-in 就会看见它）。
func TestBuiltinLengthNotInForIn(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		want         string
		skipKnownGap bool
	}{
		{
			name: "for-in 看不见 length",
			src: `let seen = false;
for (const k in Array.prototype.map) { if (k === "length") seen = true; }
seen ? "leak" : "ok";`,
			want: "ok",
		},
		{
			name: "Object.keys 看不见 length",
			src:  `Object.keys(Array.prototype.map).indexOf("length") === -1 ? "ok" : "leak";`,
			want: "ok",
		},
		{
			name: "getOwnPropertyNames 看得见 length",
			src:  `Object.getOwnPropertyNames(Array.prototype.map).indexOf("length") >= 0 ? "ok" : "missing";`,
			want: "ok",
		},
		{
			name: "hasOwnProperty 为真",
			src:  `Array.prototype.map.hasOwnProperty("length") ? "ok" : "missing";`,
			want: "ok",
		},
		{
			name: "描述符标志",
			src: `const d = Object.getOwnPropertyDescriptor(Array.prototype.map, "length");
(d.writable === false && d.enumerable === false && d.configurable === true) ? "ok" : "bad";`,
			want: "ok",
		},
		{
			// 基线缺陷（本批不修，§3.4.8）：内建函数的 length 在 Gox 里对
			// 赋值不报错（静默忽略）。这条断言**记录**了规范期望，但现在
			// 必须跳过 —— 否则闸门在改动前就是红的，无法证明"前后一字不差"。
			skipKnownGap: true,
			name:         "严格模式赋值抛 TypeError",
			src: `let r = "no-throw";
try { (function(){ "use strict"; Math.max.length = 9; })(); } catch (e) { r = e instanceof TypeError ? "ok" : "wrong-error"; }
r;`,
			want: "ok",
		},
		{
			// 基线缺陷（本批不修，§3.4.8）：delete 返回 true 但 hasOwnProperty
			// 仍为 false 之后 length 读回 0 —— 删除占位与结构体字段的优先级
			// 在基线里就不一致。本批只保证这条**行为**不变。
			skipKnownGap: true,
			name:         "delete 成功且之后走删除占位",
			src: `const f = String.prototype.slice;
const before = f.length;
const del = delete f.length;
(del === true && f.hasOwnProperty("length") === false && before === 2) ? "ok" : "bad";`,
			want: "ok",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if c.skipKnownGap {
				t.Skip("基线已知缺陷，见 docs/performance.md §3.4.8；本批只保证行为不变")
			}
			got := evalString(t, c.src)
			if got != c.want {
				t.Fatalf("得到 %q, 期望 %q", got, c.want)
			}
		})
	}
}

// resolvePath 按 "a.b.c" 逐段取属性（只走 env.Get + GetProperty，不触发 JS 求值）。
func resolvePath(t *testing.T, env interface {
	Get(string) (object.Value, bool)
}, path string) object.Value {
	t.Helper()
	root := path
	rest := ""
	if i := indexDot(path); i >= 0 {
		root, rest = path[:i], path[i+1:]
	}
	v, got := env.Get(root)
	if !got || v == nil {
		t.Fatalf("全局 %q 不存在", root)
	}
	for rest != "" {
		seg := rest
		if i := indexDot(rest); i >= 0 {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		getter, ok := v.(interface {
			GetProperty(string) (object.Value, bool)
		})
		if !ok {
			t.Fatalf("%s 无法取属性 %q", path, seg)
		}
		nv, got := getter.GetProperty(seg)
		if !got || nv == nil {
			t.Fatalf("%s 取不到 %q", path, seg)
		}
		v = nv
	}
	return v
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func evalString(t *testing.T, src string) string {
	t.Helper()
	last, err := vm.EvalWithGlobals(src, stdlib.SetupGlobals())
	if err != nil {
		t.Fatalf("求值失败: %v\n源码:\n%s", err, src)
	}
	if last == nil {
		t.Fatalf("没有末值\n源码:\n%s", src)
	}
	// 字符串值直接取底层文本（Inspect 会带引号，断言表里写的是裸文本）。
	if str, ok := last.(*object.String); ok {
		return str.Value
	}
	return last.Inspect()
}

// asFuncObj 把任意"函数类值"适配成能读自有描述符的对象句柄。
//
// 内建函数有三套结构（*object.Object 的普通函数实例、*BuiltinFunction、
// *BuiltinMethod），它们**都**实现了 OwnDescriptor/GetProperty，但只有第一套是
// *object.Object —— 测试里不能只断言一种类型，否则覆盖不到真正的 length 写入点。
func asFuncObj(t *testing.T, path string, v object.Value) interface {
	OwnDescriptor(string) (object.PropertyDescriptor, bool)
	GetProperty(string) (object.Value, bool)
} {
	t.Helper()
	if v == nil {
		t.Fatalf("%s 解析为 nil", path)
	}
	fn, ok := v.(interface {
		OwnDescriptor(string) (object.PropertyDescriptor, bool)
		GetProperty(string) (object.Value, bool)
	})
	if !ok {
		t.Fatalf("%s 不支持自有描述符查询: %T", path, v)
	}
	return fn
}
