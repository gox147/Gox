// Package abi_pin_fixture 只为一个目的存在: 让 tools/perf 的形态闸门能对着
// 一份**稳定的**、与解释器主循环同形的源码, 观察 Go 对"常量下标切片寻址"的
// 代码生成。它不进任何生产路径, 也不该被别的包 import。
//
// 改动这里的函数形状 = 让 tools/perf/TestLocalsAddressingShape 失效; 改之前
// 先读那份测试的开头注释 (它记着这一轮为什么把形态钉住)。
package abi_pin_fixture

import "unsafe"

// Value 与 object.Value 的最小同形替身: interface{} 在 amd64 上同样是 16 字节
// (itab/type + data), 常量下标寻址的代码生成与真实 Value 一致。
type Value = interface{}

// Frame 与 vm.Frame 的寻址相关字段同序 (Instructions 之外的 Locals 位置)。
type Frame struct {
	Locals []Value
}

// LocalsDirect 是 vm 主循环里 `frame.Locals[slot]` (slot 为编译期常量) 的同形
// 代码: noinline 防止被优化成一团, //go:noinline 也让 nm 能找到符号。
//
//go:noinline
func LocalsDirect(f *Frame, v Value) Value {
	f.Locals[1] = v
	return f.Locals[2]
}

// PtrIndirect 是**反例形态**: 与上面的局部寻址候选 (LocalPtrs 指针表) 同形,
// 用来证明闸门拦得住。
//
//go:noinline
func PtrIndirect(f *PtrFrame, v Value) Value {
	*(*Value)(f.Ptrs[1]) = v
	return *(*Value)(f.Ptrs[2])
}

// PtrIndirectLoop 是反例的**可变下标**形态 (主循环里 operand 是运行期值, 不是
// 常量)。常量下标那版在源码层等价, 但生成物差别很大: 常量下标会被折成
// `mov 0xNN(%data),%reg`, 而可变下标必须走 `lea ...(..)(..*0x10)` 的地址算术。
// 两种都算"指针表", 闸门都要认得。
//
//go:noinline
func PtrIndirectLoop(f *PtrFrame, slot int, v Value) Value {
	*(*Value)(f.Ptrs[slot]) = v
	return *(*Value)(f.Ptrs[slot+1])
}

// LocalsLoop 是同形的"直接切片 + 可变下标"对照组: 它同样要算地址, 但**没有**
// 指针表那一层间接, 所以不该被闸门拦下 —— 这一对样本的存在是为了证明闸门判的
// 是"多一层间接", 而不是"算没算地址"。
//
//go:noinline
func LocalsLoop(f *Frame, slot int, v Value) Value {
	f.Locals[slot] = v
	return f.Locals[slot+1]
}

type PtrFrame struct {
	Ptrs []unsafe.Pointer
}
