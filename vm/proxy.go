package vm

import (
	"fmt"

	"github.com/14752222/Gox/object"
)

// typeErrorThrow 构造**可被 try/catch 捕获**的 TypeError 抛出信号。
// 之前 proxy 路径的合成错误用裸 fmt.Errorf 直接 return —— 那是 Go error
// 而非 JS 异常, 会绕过 handleThrow 逃出 try/catch (vm error 直达顶层)。
// 包装成 *ThrowError 后, 调用点的 rethrowBridgeError 会走 handleThrow
// 正常匹配 try 处理器。
func typeErrorThrow(format string, a ...any) error {
	return &ThrowError{Value: object.NewErrorWithName("TypeError", fmt.Sprintf(format, a...))}
}

// proxyTrap 按 trapName 分发代理 trap (规范 Proxy 内部方法共用的前段)。
// 返回 (result, handled, err):
//   - revoked proxy → TypeError (所有内部方法共享的校验, T5 r8dXes);
//   - trap 存在但不可调用 → TypeError "'%s' trap must be callable"
//     (GetMethod 语义; 此前 GetProxyTrap 把这种形态静默降级为"无 trap",
//     trap-is-not-callable 一族应抛的 TypeError 全部吞掉);
//   - trap 不存在 / undefined / null → (nil, false, nil): 调用方转发 target;
//   - trap 存在且可调用 → 调用 trap (this 绑 handler), 返回结果。
func (vm *VM) proxyTrap(proxy *object.Proxy, trapName string, args []object.Value) (object.Value, bool, error) {
	// 已撤销的代理: 所有操作抛 TypeError
	if proxy.IsRevoked {
		return nil, true, typeErrorThrow("Cannot perform '%s' on a proxy that has been revoked", trapName)
	}
	handler := proxy.Handler
	if handler == nil {
		return nil, false, nil
	}
	trap, present := object.GetProxyTrapMethod(handler, trapName)
	if !present {
		return nil, false, nil
	}
	if !object.IsCallable(trap) {
		return nil, true, typeErrorThrow("'%s' trap must be callable", trapName)
	}
	// 调用 trap，this 绑定为 handler
	result, err := vm.callFunction(trap, handler, args)
	if err != nil {
		return nil, true, err
	}
	return result, true, nil
}

// proxyForwardGet 无 get trap 时对 target 转发 [[Get]]。
// target 本身是 proxy 时必须递归走 trap 分发 (trap-is-undefined-target-is-proxy
// 族: 嵌套代理的每一层都要经过自己 handler 的 trap), 不能直接读 target 属性。
func (vm *VM) proxyForwardGet(target object.Value, key string, receiver object.Value) (object.Value, error) {
	if tp, ok := target.(*object.Proxy); ok {
		return vm.proxyGet(tp, key, receiver)
	}
	val, found := target.GetProperty(key)
	if !found || val == nil {
		return object.UndefinedSingleton, nil
	}
	return val, nil
}

// proxyGet 处理对代理的属性读取。
// receiver 是接收者 (通常就是代理本身)。

func (vm *VM) proxyGet(proxy *object.Proxy, key string, receiver object.Value) (object.Value, error) {
	res, handled, err := vm.proxyTrap(proxy, "get", []object.Value{proxy.Target, object.NewString(key), receiver})
	if err != nil {
		return nil, err
	}
	if !handled {
		// 无 trap: 转发到目标
		return vm.proxyForwardGet(proxy.Target, key, receiver)
	}
	if res == nil {
		return object.UndefinedSingleton, nil
	}
	return res, nil
}

// proxyGetSymbol 处理对代理的 **Symbol 键**属性读取。
// 规范 [[Get]] 的键是 property key —— Symbol 必须以原值传给 get trap,
// 不能 Inspect 字符串化 (否则 trap 内 `key === s` 永远 false,
// trap-is-null-target-is-proxy / get-with-symbol 族, T5 r8dXes)。
func (vm *VM) proxyGetSymbol(proxy *object.Proxy, sym *object.Symbol, receiver object.Value) (object.Value, error) {
	res, handled, err := vm.proxyTrap(proxy, "get", []object.Value{proxy.Target, sym, receiver})
	if err != nil {
		return nil, err
	}
	if !handled {
		// 无 trap: 转发到 target (target 是 proxy 时递归走其 get trap)
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxyGetSymbol(tp, sym, receiver)
		}
		desc, found := object.LookupSymbolPropertyDescriptorChain(proxy.Target, sym)
		if !found || desc.Value == nil {
			return object.UndefinedSingleton, nil
		}
		if acc, isAcc := desc.Value.(*object.Accessor); isAcc {
			if acc.Getter != nil && object.IsCallable(acc.Getter) {
				return object.CallFunction(acc.Getter, receiver), nil
			}
			return object.UndefinedSingleton, nil
		}
		return desc.Value, nil
	}
	if res == nil {
		return object.UndefinedSingleton, nil
	}
	return res, nil
}

// proxyHasSymbol 处理对代理的 Symbol 键 `in` 检查 (has trap, 键传原值)。
func (vm *VM) proxyHasSymbol(proxy *object.Proxy, sym *object.Symbol) (bool, error) {
	res, handled, err := vm.proxyTrap(proxy, "has", []object.Value{proxy.Target, sym})
	if err != nil {
		return false, err
	}
	if !handled {
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxyHasSymbol(tp, sym)
		}
		if proxy.Target == nil {
			return false, nil
		}
		_, found := object.LookupSymbolPropertyDescriptorChain(proxy.Target, sym)
		return found, nil
	}
	return object.IsTruthyValue(res), nil
}

// proxySetSymbol 处理对代理的 Symbol 键属性写入 (set trap, 键传原值)。
func (vm *VM) proxySetSymbol(proxy *object.Proxy, sym *object.Symbol, val, receiver object.Value) error {
	res, handled, err := vm.proxyTrap(proxy, "set", []object.Value{proxy.Target, sym, val, receiver})
	if err != nil {
		return err
	}
	if !handled {
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxySetSymbol(tp, sym, val, receiver)
		}
		if o, ok := proxy.Target.(*object.Object); ok {
			o.SetSymbolProperty(sym, val)
		}
		return nil
	}
	_ = res
	return nil
}

// proxyDeleteSymbol 处理对代理的 Symbol 键 delete (deleteProperty trap, 键传原值)。
func (vm *VM) proxyDeleteSymbol(proxy *object.Proxy, sym *object.Symbol) (bool, error) {
	res, handled, err := vm.proxyTrap(proxy, "deleteProperty", []object.Value{proxy.Target, sym})
	if err != nil {
		return false, err
	}
	if !handled {
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxyDeleteSymbol(tp, sym)
		}
		if o, ok := proxy.Target.(*object.Object); ok {
			return o.DeleteSymbolProperty(sym), nil
		}
		return true, nil
	}
	return object.IsTruthyValue(res), nil
}

// proxyForwardSet 无 set trap 时对 target 转发 [[Set]]。
func (vm *VM) proxyForwardSet(target object.Value, key string, val, receiver object.Value) error {
	if tp, ok := target.(*object.Proxy); ok {
		return vm.proxySet(tp, key, val, receiver)
	}
	target.SetProperty(key, val)
	return nil
}

// proxySet 处理对代理的属性写入。

func (vm *VM) proxySet(proxy *object.Proxy, key string, val, receiver object.Value) error {
	res, handled, err := vm.proxyTrap(proxy, "set", []object.Value{proxy.Target, object.NewString(key), val, receiver})
	if err != nil {
		return err
	}
	if !handled {
		// 无 trap: 转发到目标
		return vm.proxyForwardSet(proxy.Target, key, val, receiver)
	}
	// 有 trap: 若 trap 返回 falsy，视为静默失败 (非严格模式)
	_ = res
	return nil
}

// proxyForwardHas 无 has trap 时对 target 转发 [[HasProperty]]。
func (vm *VM) proxyForwardHas(target object.Value, key string) (bool, error) {
	if tp, ok := target.(*object.Proxy); ok {
		return vm.proxyHas(tp, key)
	}
	_, found := target.GetProperty(key)
	return found, nil
}

// proxyHas 处理对代理的属性存在性检查 (in 操作符)。

func (vm *VM) proxyHas(proxy *object.Proxy, key string) (bool, error) {
	res, handled, err := vm.proxyTrap(proxy, "has", []object.Value{proxy.Target, object.NewString(key)})
	if err != nil {
		return false, err
	}
	if !handled {
		// 无 trap: 转发到目标
		return vm.proxyForwardHas(proxy.Target, key)
	}
	return object.IsTruthyValue(res), nil
}

// proxyForwardDelete 无 deleteProperty trap 时对 target 转发 [[Delete]]。
func (vm *VM) proxyForwardDelete(target object.Value, key string) (bool, error) {
	if tp, ok := target.(*object.Proxy); ok {
		return vm.proxyDelete(tp, key)
	}
	if o, ok := target.(*object.Object); ok {
		return o.DeleteProperty(key), nil
	}
	if arr, ok := target.(*object.Array); ok {
		return arr.DeleteOwn(key), nil
	}
	return true, nil
}

// proxyDelete 处理对代理的 delete 操作 (deleteProperty trap, T5 r8dXes)。
// 此前 OP_DELETE 对 proxy 无分支 —— trap 存在时既不分发也不校验可调用性。

func (vm *VM) proxyDelete(proxy *object.Proxy, key string) (bool, error) {
	res, handled, err := vm.proxyTrap(proxy, "deleteProperty", []object.Value{proxy.Target, object.NewString(key)})
	if err != nil {
		return false, err
	}
	if !handled {
		// 无 trap: 转发到目标
		return vm.proxyForwardDelete(proxy.Target, key)
	}
	return object.IsTruthyValue(res), nil
}

// proxyApply 处理对代理的调用 (apply trap)。

func (vm *VM) proxyApply(proxy *object.Proxy, thisArg object.Value, args []object.Value) (object.Value, error) {
	res, handled, err := vm.proxyTrap(proxy, "apply", []object.Value{proxy.Target, thisArg, object.NewArray(args)})
	if err != nil {
		return nil, err
	}
	if !handled {
		// 无 trap: 转发调用目标; target 是嵌套 proxy 时递归走其 apply trap
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxyApply(tp, thisArg, args)
		}
		if !object.IsCallable(proxy.Target) {
			return nil, typeErrorThrow("%s is not a function", proxy.Target.Inspect())
		}
		return vm.callFunction(proxy.Target, thisArg, args)
	}
	return res, nil
}

// proxyConstruct 处理对代理的 new 调用。
// 注意: OP_NEW 侧已先校验 IsConstructor(proxy) (target 可构造), 因此带
// construct trap 的 proxy 只要 target 不可构造就不会走到这里。
// newTarget 是规范 [[Construct]] 的第三参 (EvaluateNew: newTarget = 被构造的
// 代理本身; Reflect.construct 显式传入) —— 此前 trap 拿不到它,
// construct/call-parameters-new-target 族失败。

func (vm *VM) proxyConstruct(proxy *object.Proxy, args []object.Value, newTarget object.Value) (object.Value, error) {
	res, handled, err := vm.proxyTrap(proxy, "construct", []object.Value{proxy.Target, object.NewArray(args), newTarget})
	if err != nil {
		return nil, err
	}
	if !handled {
		// 无 trap: 以 new 方式调用目标; target 是嵌套 proxy 时递归
		// (newTarget 保持不变, 规范 [[Construct]] step 6.a)。
		if tp, ok := proxy.Target.(*object.Proxy); ok {
			return vm.proxyConstruct(tp, args, newTarget)
		}
		newObj := object.NewObject()
		// 规范 target.[[Construct]](args, newTarget) → OrdinaryCreateFrom-
		// Constructor(newTarget) → GetPrototypeFromConstructor:
		// Get(newTarget, "prototype") 对 proxy newTarget 须经其 get trap
		// (trap 可在读取中撤销代理); 原型非对象时 GetFunctionRealm(newTarget)
		// 对 revoked proxy 抛 TypeError (base-ctor-revoked-proxy 族, T5 r8dXes)。
		// 仅 proxy newTarget 走此路径, 其余 newTarget 保持既有口径。
		if ntp, isProxy := newTarget.(*object.Proxy); isProxy {
			proto, perr := vm.proxyGet(ntp, "prototype", newTarget)
			if perr != nil {
				return nil, perr
			}
			if object.IsObjectLike(proto) {
				newObj = object.NewObjectWithProto(proto)
			} else if ntp.IsRevoked || ntp.Target == nil {
				return nil, typeErrorThrow("Cannot construct a proxy that has been revoked")
			}
		}
		result, err := vm.callFunction(proxy.Target, newObj, args)
		if err != nil {
			return nil, err
		}
		// 构造语义: 若构造器返回对象则用之，否则用新对象
		if result != nil && object.IsObjectLike(result) {
			return result, nil
		}
		return newObj, nil
	}
	// 规范 Construct: trap 结果必须是对象, 否则 TypeError
	if res != nil && !object.IsObjectLike(res) {
		return nil, typeErrorThrow("proxy [[Construct]] must return an object")
	}
	return res, nil
}
