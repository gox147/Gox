package vm

import (
	"fmt"

	"github.com/14752222/Gox/bytecode"
	"github.com/14752222/Gox/object"
)

// callClosureFromStack 是 OP_CALL 的快路径（T3-4 第十二批）:
// 实参此刻就在**操作数栈上**（栈顶 = argN，往下 argN-1 … arg1），调用方没有
// 先把它们弹进复用缓冲再传进来。
//
// 与 callClosure 的关系: 帧装配（常量池、this/new.target、捕获变量、形参放置、
// arguments、自引用）逐条对齐，唯一差别是"实参从哪里读"。之所以不抽公共函数
// 而写第二份装箱代码: 公共函数要么接受 []object.Value（那正是要省掉的那次
// 搬运），要么接受一个抽象的取值接口（每次读参数一次间接调用 + 接口装箱，
// 在 fib 这种一参数函数上比重排还贵）。这份代码的每一步都能在 callClosure
// 里找到对应行，改动时**必须两边同时改** —— 我在下面用行号注释标了对应关系。
//
// 前置条件（由调用点保证）:
//   - closure.Fn != nil，且不是生成器函数、形态参数前导段；
//   - 栈上至少 numArgs 个实参，且它们连续位于栈顶；
//   - 没有待处理的直接 eval 标记（pendingDirectEval == false）。
//
// 后置条件: 实参已从操作数栈弹出，新帧已压栈（执行循环会立刻跑它）。
func (vm *VM) callClosureFromStack(closure *object.Closure, numArgs int) error {
	fn := closure.Fn
	if fn == nil {
		return fmt.Errorf("VM: closure has no function")
	}
	if vm.frameIdx >= MaxFrames-1 {
		return &jsThrow{Name: "RangeError", Message: "Maximum call stack size exceeded"}
	}
	stackLen := vm.stack.Len()
	if numArgs > stackLen {
		// 只可能在字节码损坏时发生；宁可明确报错，也不要越界读。
		return fmt.Errorf("VM: OP_CALL operand %d exceeds operand stack %d", numArgs, stackLen)
	}

	// ---- 以下至"形参放置"之前与 callClosure 逐条相同 ----
	constants := vm.constants
	if fn.Constants != nil && !sameConstantSlice(vm.constants, fn.Constants) {
		constants = &bytecode.ConstantPool{Constants: fn.Constants}
	}
	frame := vm.acquireFrame(fn.Instructions, constants, fn.NumLocals)
	frame.Closure = closure
	frame.This = vm.resolveFrameThis(closure)
	if closure.IsArrow {
		frame.NewTarget = closure.NewTarget
	} else if vm.hasPendingNewTarget {
		frame.NewTarget = vm.pendingNewTarget
	} else {
		frame.NewTarget = closure.NewTarget
	}
	if frame.NewTarget == nil {
		frame.NewTarget = object.UndefinedSingleton
	}
	vm.hasPendingNewTarget = false
	vm.pendingNewTarget = nil
	// 栈高扣掉实参: 返回时截断到"调用前的栈高"，与 callClosure 在
	// 「实参已弹出」之后取 Len() 的结果一致。
	frame.StackBase = stackLen - numArgs

	if len(closure.CapturedLocals) > 0 {
		copy(frame.Locals, closure.CapturedLocals)
	}
	frame.SharedCells = closure.CapturedLocals

	// ---- 形参放置: 语义同 callClosure，只是实参读栈 ----
	baseSlot := fn.BaseSlot
	hasRest := len(fn.Parameters) > 0 && fn.Parameters[len(fn.Parameters)-1].Rest
	restParamIndex := len(fn.Parameters) - 1

	// 实参在栈上的绝对下标: 第 i 个实参 = 栈高 - numArgs + i
	argAt := func(i int) object.Value { return vm.stack.PeekAt(numArgs - 1 - i) }

	if hasRest {
		numRegular := restParamIndex
		for i := 0; i < numRegular; i++ {
			slot := baseSlot + i
			if slot < len(frame.Locals) {
				if i < numArgs {
					frame.Locals[slot] = argAt(i)
				} else {
					frame.Locals[slot] = object.UndefinedSingleton
				}
			}
		}
		var restElements []object.Value
		if numArgs > numRegular {
			restElements = make([]object.Value, numArgs-numRegular)
			for i := range restElements {
				restElements[i] = argAt(numRegular + i)
			}
		}
		restSlot := baseSlot + restParamIndex
		if restSlot < len(frame.Locals) {
			frame.Locals[restSlot] = object.NewArray(restElements)
		}
	} else {
		placeCount := numArgs
		if placeCount > fn.NumParameters {
			placeCount = fn.NumParameters
		}
		for i := 0; i < placeCount; i++ {
			slot := baseSlot + i
			if slot < len(frame.Locals) {
				frame.Locals[slot] = argAt(i)
			}
		}
		for i := placeCount; i < fn.NumParameters; i++ {
			slot := baseSlot + i
			if slot < len(frame.Locals) {
				frame.Locals[slot] = object.UndefinedSingleton
			}
		}
	}

	// arguments 对象: 同 callClosure（SkipArguments 时跳过）。
	if !fn.SkipArguments && fn.ArgumentsSlot >= 0 && fn.ArgumentsSlot < len(frame.Locals) {
		argsCopy := make([]object.Value, numArgs)
		for i := 0; i < numArgs; i++ {
			argsCopy[i] = argAt(i)
		}
		frame.Locals[fn.ArgumentsSlot] = object.NewArray(argsCopy)
	}

	// 实参读完了，才把栈截到调用前的高度（截断会清槽，先清就取不到值）。
	vm.stack.Truncate(frame.StackBase)

	// 命名函数表达式的自引用: 同 callClosure。
	if fn.SelfSlot >= 0 && fn.SelfSlot < len(frame.Locals) {
		frame.Locals[fn.SelfSlot] = closure
	}

	vm.pushFrame(frame)
	return nil
}
