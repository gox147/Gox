package stdlib

import (
	"strconv"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// promiseCtor 是 %Promise% 构造器 —— 规范 PromiseResolve(C, x) 的 C。
//
// 为什么需要它: 步骤 1.b 要判 `SameValue(xConstructor, C)`, 而 C 是内建的
// %Promise%, 不是从某个对象上现取的。此前没有这个包级引用, PromiseResolve
// 的"取 x.constructor"那一步干脆没实现 (rj9MwH)。
var promiseCtor object.Value

// promiseProtoThen 是 %Promise.prototype%.then 的内建方法值 (B1 rwjnvX):
// promiseAttach 的 *Promise 快路径用它判断 "then 是否被覆写" —— 覆写后必须
// 走通用 Invoke (invoke-then 族: 覆写后调用计数 0 ≠ 3)。
var promiseProtoThen object.Value

// promiseCapability 是 NewPromiseCapability 的产物: 内建 %Promise% 快路径
// 的真实 Promise + resolve/reject 函数; 用户构造器路径则是对注册进去的
// resolve/reject 的包装调用。
type promiseCapability struct {
	promise *object.Promise // 内建路径: 真实 Promise (用户路径为 nil)
	obj     object.Value    // 用户路径: Construct(C, «executor») 的产物
	resolve object.Value
	reject  object.Value
	// resolveVal/rejectVal 是 executor 记录的原始值 (可能为 undefined),
	// 用于 GetCapabilitiesExecutor 的"已赋值"判定。
	resolveVal object.Value
	rejectVal  object.Value
}

// isUndefinedVal 报告 v 是否为 undefined (含 nil)。
func isUndefinedVal(v object.Value) bool {
	if v == nil {
		return true
	}
	_, ok := v.(*object.Undefined)
	return ok
}

// result 返回 capability 携带的 promise 对象 (内建路径 *Promise; 用户路径
// 构造产物)。
func (c *promiseCapability) result() object.Value {
	if c.promise != nil {
		return c.promise
	}
	return c.obj
}

// callResolve/callReject 统一 settle 出口: 内建路径直调内部 Resolve/Reject
// (零桥接开销), 用户路径经 CallFunction 调注册的 JS 函数, 异常上抛。
func (c *promiseCapability) callResolve(v object.Value) object.Value {
	if c.promise != nil {
		c.promise.Resolve(v)
		return nil
	}
	res := object.CallFunction(c.resolve, nil, v)
	if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
		return ev
	}
	if e, isErr := res.(*object.Error); isErr && e != nil {
		return e
	}
	return nil
}

func (c *promiseCapability) callReject(v object.Value) object.Value {
	if c.promise != nil {
		c.promise.Reject(v)
		return nil
	}
	res := object.CallFunction(c.reject, nil, v)
	if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
		return ev
	}
	if e, isErr := res.(*object.Error); isErr && e != nil {
		return e
	}
	return nil
}

// isBuiltinPromiseCtor 判断 this 是否就是内建 %Promise% (快路径条件)。
func isBuiltinPromiseCtor(v object.Value) bool {
	return promiseCtor != nil && v == promiseCtor
}

// newPromiseCapability 实现 NewPromiseCapability (27.2.1.8) 的能力校验 ——
// T7 的根因收口 (capability-executor-not-callable / ctx-non-ctor /
// ctx-non-object 族):
//   - this 不可构造 → TypeError;
//   - 内建 %Promise%: 快路径, 直接造内部 Promise 与 resolve/reject 闭包;
//   - 用户构造器: 以 GetCapabilitiesExecutor 构造 —— executor 记录传入的
//     resolve/reject (重复调用 → TypeError); 构造完成后 resolve/reject
//     缺一或不可调用 → TypeError。
func newPromiseCapability(thisVal object.Value) (*promiseCapability, object.Value) {
	if !object.IsConstructor(thisVal) {
		return nil, object.NewErrorWithName("TypeError", "Promise: constructor is not a constructor")
	}
	if isBuiltinPromiseCtor(thisVal) {
		p := object.NewPromise()
		resolve := object.NewBuiltin("resolve", func(args ...object.Value) object.Value {
			p.Resolve(argAt(args, 0))
			return object.UndefinedSingleton
		})
		resolve.SetFunctionLength(1)
		reject := object.NewBuiltin("reject", func(args ...object.Value) object.Value {
			p.Reject(argAt(args, 0))
			return object.UndefinedSingleton
		})
		reject.SetFunctionLength(1)
		return &promiseCapability{promise: p, resolve: resolve, reject: reject}, nil
	}
	cap := &promiseCapability{}
	// GetCapabilitiesExecutor (27.2.1.5.1): length 2、不可构造; 第二次调用
	// 前先检查 [[Resolve]]/[[Reject]] 是否已为非 undefined 值 (检查先于
	// 赋值) —— executor() 空参调用的语义是"置 undefined", 不算已赋值
	// (capability-executor-called-twice 的 fn1/fn2 分支)。
	executor := object.NewBuiltin("", func(args ...object.Value) object.Value {
		r, j := argAt(args, 0), argAt(args, 1)
		if !isUndefinedVal(cap.resolveVal) || !isUndefinedVal(cap.rejectVal) {
			return object.NewErrorWithName("TypeError", "Promise capability executor already called")
		}
		cap.resolveVal, cap.rejectVal = r, j
		return object.UndefinedSingleton
	})
	executor.SetFunctionLength(2)
	executor.NotConstructable = true
	// 规范 27.5.1.5: promise = ? Construct(ctor, «executor») —— 构造调用
	// 异常完成一律原样传播 (无 reject-and-return 分支)。
	constructed := object.ConstructFunction(thisVal, executor)
	if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
		return nil, ev
	}
	if e, isErr := constructed.(*object.Error); isErr && e != nil {
		return nil, e
	}
	// Construct 之后 [[Resolve]]/[[Reject]] 必须 callable (含 undefined →
	// TypeError: executor 未被调用或以空参调用都属此列)。
	if !object.IsCallable(cap.resolveVal) || !object.IsCallable(cap.rejectVal) {
		return nil, object.NewErrorWithName("TypeError", "Promise capability resolve/reject must be callable")
	}
	cap.resolve = cap.resolveVal
	cap.reject = cap.rejectVal
	cap.obj = constructed
	return cap, nil
}

// setupPromise 设置 Promise 构造器。
func setupPromise(env *runtime.Environment) {
	promiseProto := setupPromiseProto()
	object.SetPromiseProto(promiseProto)
	setToStringTag(promiseProto, "Promise")

	promiseFn := object.NewBuiltin("Promise", func(args ...object.Value) object.Value {
		// 规范: Promise 构造器必须有 [[Construct]] ——
		// Promise(fn) / Promise.call(p, fn) 一律 TypeError (undefined-newtarget)。
		if !object.IsConstructCall() {
			return object.NewErrorWithName("TypeError", "Promise constructor cannot be invoked without 'new'")
		}
		if len(args) < 1 {
			return object.NewErrorWithName("TypeError", "Promise resolver undefined is not a function")
		}
		executor := args[0]
		if !object.IsCallable(executor) {
			return object.NewErrorWithName("TypeError", "Promise resolver is not a function")
		}

		p := object.NewPromise()

		// 创建 resolve 和 reject 函数 —— 规范 CreateResolvingFunctions:
		// 二者共享 alreadyResolved 标志, 首次调用生效, 其后一切调用
		// (含 executor throw 触发的 reject) 一律忽略
		// (exception-after-resolve-in-executor 族)。
		alreadyResolved := false
		resolve := object.NewBuiltin("resolve", func(args ...object.Value) object.Value {
			if !alreadyResolved {
				alreadyResolved = true
				val := object.Value(object.UndefinedSingleton)
				if len(args) > 0 {
					val = args[0]
				}
				p.Resolve(val)
			}
			return object.UndefinedSingleton
		})

		reject := object.NewBuiltin("reject", func(args ...object.Value) object.Value {
			if !alreadyResolved {
				alreadyResolved = true
				reason := object.Value(object.UndefinedSingleton)
				if len(args) > 0 {
					reason = args[0]
				}
				p.Reject(reason)
			}
			return object.UndefinedSingleton
		})

		// 调用 executor(resolve, reject)
		// 规范: executor 抛出的异常应让 Promise reject。
		// 旧实现完全忽略执行结果，executor 出错会让 Promise 永久 pending。
		//
		// 这里同时检查两个信号:
		// 1. TakeCallbackError —— 回调桥记录的 JS 异常 (涵盖 throw 任意值);
		// 2. 返回的 *Error       —— executor 返回/抛出的 Error 对象。
		ret := object.CallFunction(executor, nil, resolve, reject)
		if cbErr := object.TakeCallbackError(); cbErr != nil {
			// executor 异常: 仅当尚未 resolve/reject 过才拒绝 (alreadyResolved)
			if !alreadyResolved {
				alreadyResolved = true
				if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
					p.Reject(unwrapThrown(thrown))
				} else {
					p.Reject(object.NewErrorWithName("Error", cbErr.Error()))
				}
			} else {
				object.TakeCallbackErrorValue() // 丢弃, 防桥残留
			}
		} else if e, isErr := ret.(*object.Error); isErr && !alreadyResolved {
			alreadyResolved = true
			p.Reject(e)
		}

		return p
	})
	// RequiresNew (T7): 仅 `new Promise(...)` 合法 —— VM 经 OP_NEW 进入
	// 构造窗口, 构造器体用 IsConstructCall() 区分 `Promise(fn)` 普通调用
	// (undefined-newtarget 族 → TypeError)。
	promiseFn.RequiresNew = true

	// Promise.resolve(value) —— PromiseResolve(C, x): capability 校验 (T7)
	promiseFn.SetProperty("resolve", object.NewBuiltinMethod("resolve", func(this object.Value, args ...object.Value) object.Value {
		if this == nil {
			return object.NewErrorWithName("TypeError", "Promise.resolve: this cannot be null or undefined")
		}
		if _, isUndef := this.(*object.Undefined); isUndef {
			return object.NewErrorWithName("TypeError", "Promise.resolve: this cannot be null or undefined")
		}
		if _, isNull := this.(*object.Null); isNull {
			return object.NewErrorWithName("TypeError", "Promise.resolve: this cannot be null or undefined")
		}
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		x := argAt(args, 0)
		// C === %Promise% 且 x 是原生 Promise: 直接返回 (规范步骤 3)。
		if isBuiltinPromiseCtor(this) {
			if p, ok := x.(*object.Promise); ok {
				return p
			}
		}
		if errVal := cap.callResolve(x); errVal != nil {
			return errVal
		}
		return cap.result()
	}))

	// Promise.reject(reason) —— NewPromiseCapability(C) + capability reject (T7)
	promiseFn.SetProperty("reject", object.NewBuiltinMethod("reject", func(this object.Value, args ...object.Value) object.Value {
		if this == nil {
			return object.NewErrorWithName("TypeError", "Promise.reject: this cannot be null or undefined")
		}
		if _, isUndef := this.(*object.Undefined); isUndef {
			return object.NewErrorWithName("TypeError", "Promise.reject: this cannot be null or undefined")
		}
		if _, isNull := this.(*object.Null); isNull {
			return object.NewErrorWithName("TypeError", "Promise.reject: this cannot be null or undefined")
		}
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		if errVal := cap.callReject(argAt(args, 0)); errVal != nil {
			return errVal
		}
		return cap.result()
	}))

	// Promise.all(iterable)
	promiseFn.SetProperty("all", object.NewBuiltinMethod("all", func(this object.Value, args ...object.Value) object.Value {
		// 规范 27.2.4.1: NewPromiseCapability → GetPromiseResolve → GetIterator
		// —— 顺序不可颠倒。
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		promiseResolve, errVal := getPromiseResolve(this)
		if errVal != nil {
			// GetPromiseResolve 异常 → IfAbruptRejectPromise (异步 reject)。
			if ev := cap.callReject(errVal); ev != nil {
				return ev
			}
			return cap.result()
		}
		// 交错迭代 + 逐元素 resolve element (B1 rwjnvX): reject 侧是共享的
		// capability [[Reject]], resolve element 每元素一个 ([[AlreadyCalled]]
		// 守卫 + values List 增长语义)。
		return promiseAggregate(aggAll, cap, promiseResolve, this, argAt(args, 0))
	}))

	// Promise.race(iterable): 第一个 settled 的结果决定胜负
	promiseFn.SetProperty("race", object.NewBuiltinMethod("race", func(this object.Value, args ...object.Value) object.Value {
		// capability 校验 (T7) → GetPromiseResolve → GetIterator。
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		promiseResolve, errVal := getPromiseResolve(this)
		if errVal != nil {
			if ev := cap.callReject(errVal); ev != nil {
				return ev
			}
			return cap.result()
		}
		// 规范 PerformPromiseRace: 两侧都是 capability 的 [[Resolve]]/[[Reject]]
		// 本体 —— 每元素共享同一函数对象 (same-resolve-function /
		// same-reject-function 族), settle 幂等由 Promise 内部 alreadyResolved
		// 语义保证, 不需要本地 settled 标志 (B1 rwjnvX)。
		return promiseAggregate(aggRace, cap, promiseResolve, this, argAt(args, 0))
	}))

	// Promise.allSettled(iterable): 等待全部 settle，永不 reject (除参数非法)
	promiseFn.SetProperty("allSettled", object.NewBuiltinMethod("allSettled", func(this object.Value, args ...object.Value) object.Value {
		// capability 校验 (T7) → GetPromiseResolve → GetIterator。
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		promiseResolve, errVal := getPromiseResolve(this)
		if errVal != nil {
			if ev := cap.callReject(errVal); ev != nil {
				return ev
			}
			return cap.result()
		}
		// 规范 PerformPromiseAllSettled: 两侧 element function 都是每元素一个
		// (各自带 [[AlreadyCalled]] 守卫 + index), reject 不共享 (与 all/any
		// 不同 —— allSettled 必须等每个元素各自的结算记录, B1 rwjnvX)。
		return promiseAggregate(aggAllSettled, cap, promiseResolve, this, argAt(args, 0))
	}))

	// Promise.any(iterable): 第一个 fulfilled 获胜；全部 rejected 时
	// 以 AggregateError 拒绝 (ES2021)
	promiseFn.SetProperty("any", object.NewBuiltinMethod("any", func(this object.Value, args ...object.Value) object.Value {
		// capability 校验 (T7) → GetPromiseResolve → GetIterator。
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		promiseResolve, errVal := getPromiseResolve(this)
		if errVal != nil {
			if ev := cap.callReject(errVal); ev != nil {
				return ev
			}
			return cap.result()
		}
		// 规范 PerformPromiseAny: resolve 侧共享 capability [[Resolve]],
		// reject 侧是每元素的 error element ([[AlreadyCalled]] + errors List)。
		return promiseAggregate(aggAny, cap, promiseResolve, this, argAt(args, 0))
	}))

	// Promise.withResolvers() (ES2024): 返回 {promise, resolve, reject}
	promiseFn.SetProperty("withResolvers", object.NewBuiltinMethod("withResolvers", func(this object.Value, args ...object.Value) object.Value {
		// capability 校验 (T7): receiver 不是构造器 → TypeError
		// (withResolvers/ctx-non-ctor / ctx-non-object)。
		cap, errVal := newPromiseCapability(this)
		if errVal != nil {
			return errVal
		}
		var settled bool
		var resolveFn, rejectFn object.Value
		if cap.promise != nil {
			// 内建快路径: 原闭包形态 (保留 settled-once 语义)。
			p := cap.promise
			resolveFn = object.NewBuiltin("resolve", func(rargs ...object.Value) object.Value {
				if !settled {
					settled = true
					p.Resolve(argAt(rargs, 0))
				}
				return object.UndefinedSingleton
			})
			rejectFn = object.NewBuiltin("reject", func(rargs ...object.Value) object.Value {
				if !settled {
					settled = true
					p.Reject(argAt(rargs, 0))
				}
				return object.UndefinedSingleton
			})
		} else {
			// 用户构造器路径: 用注册进 capability 的 resolve/reject。
			resolveFn = cap.resolve
			rejectFn = cap.reject
		}
		result := object.NewObject()
		result.SetProperty("promise", cap.result())
		result.SetProperty("resolve", resolveFn)
		result.SetProperty("reject", rejectFn)
		return result
	}))

	// Promise.try(fn, ...args) (ES2025): 同步调用 fn，异常转为 rejection
	promiseFn.SetProperty("try", object.NewBuiltin("try", func(args ...object.Value) object.Value {
		if len(args) == 0 || !object.IsCallable(args[0]) {
			return object.NewErrorWithName("TypeError", "Promise.try: fn must be a function")
		}
		fn := args[0]
		callArgs := []object.Value{}
		if len(args) > 1 {
			callArgs = args[1:]
		}
		p := object.NewPromise()
		ret := object.CallFunction(fn, object.UndefinedSingleton, callArgs...)
		if cbErr := object.TakeCallbackError(); cbErr != nil {
			// 规范: throw 的原始值 (如字符串 "boom") 直接作为 rejection reason
			if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
				p.Reject(unwrapThrown(thrown))
			} else {
				p.Reject(object.NewErrorWithName("Error", cbErr.Error()))
			}
		} else if e, isErr := ret.(*object.Error); isErr && e != nil {
			p.Reject(e)
		} else {
			p.Resolve(ret)
		}
		return p
	}))

	// Promise.allKeyed(obj) / Promise.allSettledKeyed(obj) (ES2026 Iterator
	// helpers 同期的 Promise 补充): 参数是普通对象而非可迭代对象, 键为对象
	// 的自有可枚举属性 (字符串键按插入序 + Symbol 键)。
	promiseFn.SetProperty("allKeyed", object.NewBuiltinMethod("allKeyed", func(this object.Value, args ...object.Value) object.Value {
		// 规范: this 必须是构造器 (GetPromiseResolve 前置校验), 否则同步 TypeError
		if !object.IsConstructor(this) {
			return object.NewErrorWithName("TypeError", "Promise.allKeyed called on non-constructor")
		}
		return promiseKeyed(this, args, false)
	}))
	promiseFn.SetProperty("allSettledKeyed", object.NewBuiltinMethod("allSettledKeyed", func(this object.Value, args ...object.Value) object.Value {
		if !object.IsConstructor(this) {
			return object.NewErrorWithName("TypeError", "Promise.allSettledKeyed called on non-constructor")
		}
		return promiseKeyed(this, args, true)
	}))

	setCtorPrototype(promiseFn, promiseProto)
	promiseProto.SetBuiltinProperty("constructor", promiseFn)
	promiseCtor = promiseFn

	env.Declare("Promise", promiseFn, false)
}

// promiseKeyed 实现 Promise.allKeyed / Promise.allSettledKeyed。
//
// 规范要点:
//   - 参数非对象 → 返回已 reject 的 Promise (TypeError), 不同步抛出;
//   - 键 = 自有可枚举属性: 字符串键 (OwnKeys + 可枚举过滤, 插入序) 与
//     Symbol 键 (SymbolKeys + 可枚举过滤);
//   - allKeyed 结果: 原型为 null 的对象, 每键一个 w/e/c 全 true 的数据属性,
//     键序 = 收集序 (与结算顺序无关);
//   - allSettledKeyed 结果: 普通对象 { fulfilled: {...}, rejected: {...} },
//     两个子对象原型为 %Object.prototype%, 同样按键序填充。
func promiseKeyed(this object.Value, args []object.Value, settled bool) object.Value {
	// capability 校验 (T7): this 必须是构造器 (PromiseCreateRequests 前置)。
	cap, errVal := newPromiseCapability(this)
	if errVal != nil {
		return errVal
	}

	// 参数必须是对象 (规范 ToObject 语义): 函数/数组/代理都算对象。
	// 非对象 → 异步 reject (不同步抛出)。
	if len(args) == 0 || !object.IsObjectValue(args[0]) {
		_ = cap.callReject(object.NewTypeError("Promise argument must be an object"))
		return cap.result()
	}

	type keyedEntry struct {
		strKey string // 字符串键 (symKey == nil 时有效)
		symKey *object.Symbol
		value  object.Value
	}
	var entries []keyedEntry

	switch obj := args[0].(type) {
	case *object.Object:
		// 字符串键: OwnKeys + 可枚举过滤
		for _, k := range obj.OwnKeys() {
			desc, okd := obj.OwnDescriptor(k)
			if !okd || !desc.Enumerable {
				continue
			}
			val, _ := obj.GetProperty(k)
			if thrown := callbackThrown(); thrown != nil {
				_ = cap.callReject(unwrapThrown(thrown))
				return cap.result()
			}
			entries = append(entries, keyedEntry{strKey: k, value: val})
		}
		// Symbol 键
		for _, sym := range obj.SymbolKeys() {
			desc, okd := obj.GetSymbolPropertyDescriptor(sym)
			if !okd || !desc.Enumerable {
				continue
			}
			val, _ := obj.GetSymbolProperty(sym)
			if thrown := callbackThrown(); thrown != nil {
				_ = cap.callReject(unwrapThrown(thrown))
				return cap.result()
			}
			entries = append(entries, keyedEntry{symKey: sym, value: val})
		}
	case *object.Array:
		// 数组: 索引即键 (length 不可枚举)
		for i, e := range obj.Elements {
			entries = append(entries, keyedEntry{strKey: strconv.Itoa(i), value: e})
		}
	default:
		// 其余对象类型 (函数/数组等) 走通用 OwnPropertyStore 接口:
		// *Closure 的 fn.key 等自定义可枚举属性也要被收集。
		store, hasStore := args[0].(object.OwnPropertyStore)
		getter, hasGet := args[0].(interface {
			GetProperty(string) (object.Value, bool)
		})
		if hasStore && hasGet {
			for _, k := range store.EnumerableOwnKeys() {
				val, _ := getter.GetProperty(k)
				if thrown := callbackThrown(); thrown != nil {
					_ = cap.callReject(unwrapThrown(thrown))
					return cap.result()
				}
				entries = append(entries, keyedEntry{strKey: k, value: val})
			}
		}
		// Proxy 的 ownKeys/getOwnPropertyDescriptor 陷阱暂未在 stdlib
		// 层暴露, 对应 trap 观察用例仍失败。
	}

	n := len(entries)
	if n == 0 {
		// 规范: 结果对象一律是 null 原型 (allSettledKeyed 亦然)
		_ = cap.callResolve(object.NewObjectWithProto(nil))
		return cap.result()
	}

	// GetPromiseResolve(ctor) (PerformPromiseAllKeyed): resolve 缺失/不可
	// 调用/读取异常 → 异步 reject (resolve-missing-reject-with-typeerror)。
	promiseResolve, errVal := getPromiseResolve(this)
	if errVal != nil {
		_ = cap.callReject(errVal)
		return cap.result()
	}
	values := make([]object.Value, n)
	statuses := make([]object.Value, n)
	remaining := n
	settledCount := 0

	finish := func() {
		if settled {
			// allSettledKeyed: 结果是 null 原型对象, 每键一个
			// {status: "fulfilled"|"rejected", value|reason} 数据属性。
			// (测试里的 "fulfilled"/"rejected" 只是恰好同名的输入键;
			// result.fulfilled === {status: "fulfilled", value: ...}。)
			out := object.NewObjectWithProto(nil)
			for i, e := range entries {
				if e.symKey != nil {
					out.SetSymbolProperty(e.symKey, statuses[i])
				} else {
					out.DefineOwnProperty(e.strKey, object.DataProperty(statuses[i]))
				}
			}
			_ = cap.callResolve(out)
		} else {
			out := object.NewObjectWithProto(nil)
			for i, e := range entries {
				if e.symKey != nil {
					out.SetSymbolProperty(e.symKey, values[i])
				} else {
					out.DefineOwnProperty(e.strKey, object.DataProperty(values[i]))
				}
			}
			_ = cap.callResolve(out)
		}
	}

	for i, e := range entries {
		idx := i
		// nextPromise = ? Call(promiseResolve, C, «value»); 调用异常 →
		// IfAbruptRejectPromise (capability-resolve-throws-reject)。
		nextPromise := object.CallFunction(promiseResolve, this, e.value)
		if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
			_ = cap.callReject(unwrapThrown(ev))
			return cap.result()
		}
		if eErr, isErr := nextPromise.(*object.Error); isErr && eErr != nil {
			_ = cap.callReject(unwrapThrown(eErr))
			return cap.result()
		}
		// Invoke(nextPromise, "then", «onFulfilled, onRejected») —— then
		// 缺失/不可调用/调用异常 → TypeError reject
		// (invoke-then-not-callable-reject 族)。
		// onRejected: allKeyed → 以原原因 reject capability;
		// allSettledKeyed → 记 {status:"rejected", reason} (rejected 输入
		// 同样要应答, 否则结果永久 pending)。
		onRejected := reactionBuiltin("__keyed_reject", func(rargs ...object.Value) object.Value {
			_ = cap.callReject(argAt(rargs, 0))
			remaining = -1 // 阻止后续 finish
			return object.UndefinedSingleton
		})
		if settled {
			onRejected = reactionBuiltin("__keyed_reject_status", func(rargs ...object.Value) object.Value {
				statuses[idx] = object.NewObjectWithProto(object.GetObjectPrototype())
				statuses[idx].(*object.Object).SetProperty("status", object.NewString("rejected"))
				statuses[idx].(*object.Object).SetProperty("reason", argAt(rargs, 0))
				settledCount++
				if settledCount == remaining {
					finish()
				}
				return object.UndefinedSingleton
			})
		}
		errVal := promiseAttach(nextPromise,
			reactionBuiltin("__keyed_resolve", func(fargs ...object.Value) object.Value {
				if settled {
					statuses[idx] = object.NewObjectWithProto(object.GetObjectPrototype())
					statuses[idx].(*object.Object).SetProperty("status", object.NewString("fulfilled"))
					statuses[idx].(*object.Object).SetProperty("value", argAt(fargs, 0))
				} else {
					values[idx] = argAt(fargs, 0)
				}
				settledCount++
				if settledCount == remaining {
					finish()
				}
				return object.UndefinedSingleton
			}),
			onRejected)
		if errVal != nil {
			// unwrap: promiseAttach 返回的包装 Error 带 Original
			// (invoke-then-error-reject / get-error-reject 的 reason 身份, B1)。
			_ = cap.callReject(unwrapThrown(errVal))
			return cap.result()
		}
	}

	return cap.result()
}

// unwrapThrown 还原回调桥抛出的原始值: 规范要求 rejection reason 就是
// 用户 throw 的那个值 (Test262Error 实例等), 不能是包装后的通用 Error。
func unwrapThrown(err object.Value) object.Value {
	if e, ok := err.(*object.Error); ok && e.Original != nil {
		return e.Original
	}
	return err
}

// getPromiseResolve 实现 GetPromiseResolve(ctor): 取 ctor 的 "resolve" 方法。
// 读取异常或不可调用 → 返回 (nil, 错误值) —— 调用方按 IfAbruptRejectPromise
// 以该错误 reject capability。
func getPromiseResolve(ctor object.Value) (object.Value, object.Value) {
	getter, ok := ctor.(interface {
		GetProperty(string) (object.Value, bool)
	})
	if !ok {
		return nil, object.NewTypeError("Promise: this is not an object")
	}
	fn, _ := getter.GetProperty("resolve")
	if cbErr := object.TakeCallbackError(); cbErr != nil {
		// 规范 GetPromiseResolve: Get 异常原样传播 —— 抛出值身份必须保留
		// (Test262Error 实例等非 Error 包装值同样原样上抛,
		// invoke-resolve-get-error-reject 族)。
		if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
			return nil, unwrapThrown(thrown)
		}
		return nil, object.NewErrorWithName("Error", cbErr.Error())
	}
	if !object.IsCallable(fn) {
		return nil, object.NewTypeError("Promise: 'resolve' is not a function")
	}
	return fn, nil
}

// promiseAttach 在 nextPromise 上挂一对反应处理函数: *Promise 且 then 未被
// 覆写时直挂内部回调 (零桥接); 其余走 Invoke(nextPromise, "then", «...»),
// 读取或调用异常 → 返回错误值 (IfAbruptRejectPromise)。
// 快路径前提 (B1 rwjnvX): (1) 回调都是内建函数; (2) p 的 "then" 属性查回
// 的仍是 %Promise.prototype%.then 本体 —— 覆写后必须走通用路径
// (invoke-then 族: 覆写后调用计数 0 ≠ 3)。"then" 只读一次: 访问器 getter
// 只允许被调一次 (invoke-resolve-get-once 族), 读回的覆写值直接 Invoke,
// 不重复做属性查找。
func promiseAttach(nextPromise, onF, onR object.Value) object.Value {
	if p, ok := nextPromise.(*object.Promise); ok {
		of, ofOk := onF.(*object.BuiltinFunction)
		or, orOk := onR.(*object.BuiltinFunction)
		if ofOk && orOk && promiseProtoThen != nil {
			tv, _ := p.GetProperty("then")
			if cbErr := object.TakeCallbackError(); cbErr != nil {
				if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
					if e, isErr := thrown.(*object.Error); isErr && e != nil {
						return e
					}
					// 非 Error 抛值 (Test262Error 实例等): 裸包装必须带
					// Original, 否则 rejection reason 身份丢失
					// (invoke-then-get-error-reject 族, B1 rwjnvX)。
					w := object.NewErrorWithName("Error", cbErr.Error())
					w.Original = thrown
					return w
				}
				return object.NewErrorWithName("Error", cbErr.Error())
			}
			if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
				if e, isErr := ev.(*object.Error); isErr && e != nil {
					return e
				}
			}
			if tv == promiseProtoThen {
				p.OnFulfilled(of)
				p.OnRejected(or)
				return nil
			}
			// then 被覆写: 用已读到的值直接 Invoke (不再二次查属性)。
			if tv != nil && object.IsCallable(tv) {
				res := object.CallFunction(tv, nextPromise, onF, onR)
				if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
					if e, isErr := ev.(*object.Error); isErr && e != nil {
						return e
					}
					return object.NewErrorWithName("Error", "then call failed")
				}
				if e, isErr := res.(*object.Error); isErr && e != nil {
					return e
				}
				return nil
			}
			return object.NewTypeError("Promise: resolution has no callable 'then'")
		}
		// 回调非内建 (用户构造器路径): 落通用路径。
	}
	getter, hasGet := nextPromise.(interface {
		GetProperty(string) (object.Value, bool)
	})
	if !hasGet {
		return object.NewTypeError("Promise: resolution is not an object")
	}
	thenFn, _ := getter.GetProperty("then")
	if cbErr := object.TakeCallbackError(); cbErr != nil {
		if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
			if e, isErr := thrown.(*object.Error); isErr && e != nil {
				return e
			}
			// 非 Error 抛值: 裸包装带 Original, 保住 rejection reason 身份
			// (B1 rwjnvX, 与快路径同口径)。
			w := object.NewErrorWithName("Error", cbErr.Error())
			w.Original = thrown
			return w
		}
		return object.NewErrorWithName("Error", cbErr.Error())
	}
	if !object.IsCallable(thenFn) {
		// invoke-then-not-callable-* 族: Invoke 不可调用的 then → TypeError
		return object.NewTypeError("Promise: resolution has no callable 'then'")
	}
	res := object.CallFunction(thenFn, nextPromise, onF, onR)
	if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
		if e, isErr := ev.(*object.Error); isErr && e != nil {
			return e
		}
		w := object.NewErrorWithName("Error", "then call failed")
		w.Original = unwrapThrown(ev)
		return w
	}
	if e, isErr := res.(*object.Error); isErr && e != nil {
		return e
	}
	return nil
}

// ── 聚合族交错驱动 (B1 rwjnvX) ─────────────────────────────────────────────
//
// 根因清单 (对应子单 rwjnvX 描述, 全部在本驱动内收口):
//  1. 旧实现先 promiseCollectElements 把迭代器**排干**再逐元素 resolve ——
//     规范是「每 next 一个元素立即 resolve 并挂反应」的交错循环
//     (PerformPromiseAll/AllSettled/Any/Race)。排干在 done 永不成立的迭代器
//     上死循环 (20 条超时), resolve/then 抛错后也不会 IteratorClose。
//  2. resolve element 缺 [[AlreadyCalled]] 守卫 —— 同一元素二次结算会改写
//     results / 提前 remaining 归零 (call-resolve-element-after-return /
//     resolve-from-same-thenable 族)。
//  3. reject element 用了每元素独立闭包 —— 规范里 all/any 的 reject 侧与
//     race 两侧都是共享的 capability [[Reject]]/[[Resolve]]
//     (same-reject-function / same-resolve-function 族)。
//  4. element function 缺 length=1 / name="" (resolve-element-function-* /
//     reject-element-function-* 族, reactionBuiltin 统一补齐)。
//
// ⚠ mode: aggAll / aggAllSettled / aggAny / aggRace。

type aggMode int

const (
	aggAll aggMode = iota
	aggAllSettled
	aggAny
	aggRace
)

// promiseAggregateReject 统一出口: IfAbruptRejectPromise —— 以 reason 异步
// reject capability 并返回其 promise; reject 自身异常原样上抛。
func promiseAggregateReject(cap *promiseCapability, reason object.Value) object.Value {
	if ev := cap.callReject(reason); ev != nil {
		return ev
	}
	return cap.result()
}

// promiseIteratorClose 实现 IteratorClose (7.4.7): 取 iterator.return,
// 可调用则以 iterator 为 this 调用。规范: close 期间 (读取 return、调用)
// 的任何异常都不覆盖调用方的原始 abrupt completion —— 这里全部吞掉。
// return 属性存在但不可调用按 GetMethod 语义视作异常, 同样吞掉。
func promiseIteratorClose(iterator object.Value) {
	if iterator == nil || !object.IsObjectValue(iterator) {
		return
	}
	retFn, _ := iterator.GetProperty("return")
	if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
		return
	}
	if retFn == nil || !object.IsCallable(retFn) {
		return
	}
	_ = object.CallFunction(retFn, iterator)
	object.TakeThrownValue()
}

// promiseAggregate 是 all/allSettled/any/race 的共享交错驱动。
// GetIterator → 逐元素 next → 立即 resolve → 按模式挂反应;
// 任一 abrupt (next/done/value/resolve/then) 一律 IteratorClose +
// IfAbruptRejectPromise。返回值恒为 capability 的 promise。
func promiseAggregate(mode aggMode, cap *promiseCapability, promiseResolve, ctor, iterable object.Value) object.Value {
	// GetIterator (7.4.1): 数组与字符串原始值走内建快路径 (原生迭代器无
	// return 语义, 无需 close); 其余走通用 @@iterator —— 沿原型链查符号属性
	// 并展开访问器 getter, 使 String/Boolean 等原始值经原型获得可迭代性
	// (iter-arg-is-string-* 族)。
	var elems []object.Value
	var iterator, nextFn object.Value
	if arr, ok := iterable.(*object.Array); ok {
		elems = arr.Elements
	} else if s, ok := iterable.(*object.String); ok {
		for _, r := range s.Value {
			elems = append(elems, object.NewString(string(r)))
		}
	} else {
		sym := object.GetGlobalSymbol("Symbol.iterator")
		var iterFn object.Value
		if sym != nil {
			if desc, found := object.LookupSymbolPropertyDescriptorChain(iterable, sym); found {
				v := desc.Value
				if acc, isAcc := v.(*object.Accessor); isAcc {
					if acc.Getter == nil {
						v = object.UndefinedSingleton
					} else {
						v = object.CallFunction(acc.Getter, iterable)
						if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
							return promiseAggregateReject(cap, unwrapThrown(ev))
						}
					}
				}
				iterFn = v
			}
		}
		if iterFn == nil || iterFn == object.UndefinedSingleton || iterFn == object.NullSingleton {
			return promiseAggregateReject(cap, object.NewTypeError("Promise: argument is not iterable"))
		}
		if !object.IsCallable(iterFn) {
			return promiseAggregateReject(cap, object.NewTypeError("Promise: [Symbol.iterator] is not a function"))
		}
		iterator = object.CallFunction(iterFn, iterable)
		if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
			return promiseAggregateReject(cap, unwrapThrown(ev))
		}
		if e, isErr := iterator.(*object.Error); isErr && e != nil {
			return promiseAggregateReject(cap, unwrapThrown(e))
		}
		if !object.IsObjectValue(iterator) {
			return promiseAggregateReject(cap, object.NewTypeError("Promise: iterator is not an object"))
		}
		nextFn, _ = iterator.GetProperty("next")
		if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
			return promiseAggregateReject(cap, unwrapThrown(ev))
		}
		if nextFn == nil || !object.IsCallable(nextFn) {
			return promiseAggregateReject(cap, object.NewTypeError("Promise: iterator has no callable next"))
		}
	}

	// all/allSettled: 结果 List (规范 values 的增长语义: index 不足处补
	// undefined); any: errors List。remaining 初值 1 = 「迭代正常完成」这一
	// 份 (规范 remainingElementCount), 元素结算各扣 1, 归零方执行终局 settle。
	values := []object.Value{}
	errors := []object.Value{}
	remaining := 1

	// growList: values/errors List 增长到 index+1 (不足处补 undefined)。
	growList := func(list []object.Value, index int) []object.Value {
		for len(list) <= index {
			list = append(list, object.UndefinedSingleton)
		}
		return list
	}
	// finalResolve: 终局 resolve (values 数组), 异常 → IfAbruptRejectPromise。
	finalResolve := func() {
		if ev := cap.callResolve(object.NewArray(values)); ev != nil {
			_ = cap.callReject(unwrapThrown(ev))
		}
	}
	// finalAnyReject: 全部失败 → AggregateError, 异常 → IfAbruptRejectPromise。
	finalAnyReject := func() {
		if ev := cap.callReject(newAggregateError(errors, "All promises were rejected")); ev != nil {
			_ = cap.callReject(unwrapThrown(ev))
		}
	}

	// makeAllResolveElement: Promise.all Resolve Element Functions ——
	// [[AlreadyCalled]] 守卫 + values[index] 写入 + remaining 归零终局。
	makeAllResolveElement := func(index int) object.Value {
		alreadyCalled := false
		return reactionBuiltin("", func(args ...object.Value) object.Value {
			if alreadyCalled {
				return object.UndefinedSingleton
			}
			alreadyCalled = true
			values = growList(values, index)
			values[index] = argAt(args, 0)
			remaining--
			if remaining == 0 {
				finalResolve()
			}
			return object.UndefinedSingleton
		})
	}
	// makeSettledFulfillElement / makeSettledRejectElement: allSettled 的
	// 两个 element function —— 规范里同一元素的一对函数**共享**同一个
	// [[AlreadyCalled]] 记录 (先结算者生效, 另一侧被忽略)。
	makeSettledElements := func(index int) (object.Value, object.Value) {
		alreadyCalled := false
		markSettled := func(statusObj object.Value) {
			values = growList(values, index)
			values[index] = statusObj
			remaining--
			if remaining == 0 {
				finalResolve()
			}
		}
		onF := reactionBuiltin("", func(args ...object.Value) object.Value {
			if alreadyCalled {
				return object.UndefinedSingleton
			}
			alreadyCalled = true
			statusObj := object.NewObject()
			statusObj.SetProperty("status", object.NewString("fulfilled"))
			statusObj.SetProperty("value", argAt(args, 0))
			markSettled(statusObj)
			return object.UndefinedSingleton
		})
		onR := reactionBuiltin("", func(args ...object.Value) object.Value {
			if alreadyCalled {
				return object.UndefinedSingleton
			}
			alreadyCalled = true
			statusObj := object.NewObject()
			statusObj.SetProperty("status", object.NewString("rejected"))
			statusObj.SetProperty("reason", argAt(args, 0))
			markSettled(statusObj)
			return object.UndefinedSingleton
		})
		return onF, onR
	}
	// makeAnyErrorElement: Promise.any Reject Element Functions ——
	// errors[index] 写入 + remaining 归零 → AggregateError。
	makeAnyErrorElement := func(index int) object.Value {
		alreadyCalled := false
		return reactionBuiltin("", func(args ...object.Value) object.Value {
			if alreadyCalled {
				return object.UndefinedSingleton
			}
			alreadyCalled = true
			errors = growList(errors, index)
			errors[index] = argAt(args, 0)
			remaining--
			if remaining == 0 {
				finalAnyReject()
			}
			return object.UndefinedSingleton
		})
	}

	// 交错主循环。
	//
	// close 语义 (对齐规范 Promise.all/allSettled/any/race 调用方步骤):
	// IteratorStepValue 内部的异常 (next()/读取 done/value) 会把
	// iteratorRecord.[[Done]] 置 true —— 调用方据此**跳过** IteratorClose
	// (iter-step-err-no-close / iter-next-val-err-no-close 族断言 returnCount
	// === 0); 而 promiseResolve 调用或 Invoke(nextPromise,"then") 异常时
	// [[Done]] 仍为 false —— 调用方执行 IteratorClose 后 IfAbruptRejectPromise
	// (resolve-throws-iterator-return-* / invoke-then-error-close 族)。
	elem := object.Value(object.UndefinedSingleton)
	idx := 0
	iterDone := false
	closeUnlessDone := func() {
		if !iterDone && iterator != nil {
			promiseIteratorClose(iterator)
		}
	}
	for {
		if nextFn == nil {
			// 内建快路径 (数组/字符串): 无迭代器对象, 取尽即完, 无 close。
			if idx >= len(elems) {
				break
			}
			elem = elems[idx]
		} else {
			result := object.CallFunction(nextFn, iterator)
			if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
				iterDone = true
				return promiseAggregateReject(cap, unwrapThrown(ev))
			}
			if e, isErr := result.(*object.Error); isErr && e != nil {
				iterDone = true
				return promiseAggregateReject(cap, unwrapThrown(e))
			}
			if !object.IsObjectValue(result) {
				iterDone = true
				return promiseAggregateReject(cap, object.NewTypeError("Promise: iterator result is not an object"))
			}
			doneVal, _ := result.GetProperty("done")
			if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
				iterDone = true
				return promiseAggregateReject(cap, unwrapThrown(ev))
			}
			if doneVal != nil && object.IsTruthyValue(doneVal) {
				break
			}
			elem, _ = result.GetProperty("value")
			if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
				iterDone = true
				return promiseAggregateReject(cap, unwrapThrown(ev))
			}
		}

		// nextPromise = Call(promiseResolve, ctor, «value») —— receiver 是
		// 构造器 ctor (规范 PerformPromiseAll: Call(promiseResolve, ctor, «next»);
		// invoke-resolve.js: assert.sameValue(this, Promise))。
		nextPromise := object.CallFunction(promiseResolve, ctor, elem)
		if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
			closeUnlessDone()
			return promiseAggregateReject(cap, unwrapThrown(ev))
		}
		if e, isErr := nextPromise.(*object.Error); isErr && e != nil {
			closeUnlessDone()
			return promiseAggregateReject(cap, unwrapThrown(e))
		}

		// 规范: 每挂一个元素 remainingElementsCount +1 (在 Invoke "then" 前)。
		remaining++

		// 按模式挂反应 (reject/resolve 侧共享或每元素, 见根因清单 2/3)。
		var onF, onR object.Value
		switch mode {
		case aggAll:
			onF = makeAllResolveElement(idx)
			onR = cap.reject
		case aggAllSettled:
			onF, onR = makeSettledElements(idx)
		case aggAny:
			onF = cap.resolve
			onR = makeAnyErrorElement(idx)
		case aggRace:
			onF = cap.resolve
			onR = cap.reject
		}
		if errVal := promiseAttach(nextPromise, onF, onR); errVal != nil {
			closeUnlessDone()
			return promiseAggregateReject(cap, unwrapThrown(errVal))
		}
		idx++
	}

	// 迭代正常完成: remaining 扣 1, 归零方执行终局 settle
	// (空迭代器 → 直接终局)。
	remaining--
	if remaining == 0 {
		switch mode {
		case aggAll, aggAllSettled:
			finalResolve()
		case aggAny:
			finalAnyReject()
		}
	}
	return cap.result()
}

// reactionBuiltin 创建反应元素函数 (resolve/reject element): 规范上它们
// 没有 [[Construct]] —— `new resolveFn` 必须 TypeError
// (resolve-element-function-nonconstructor / reject-element-* 族)。
// length 统一为 1、name 由调用方给 "" (规范 element function 是匿名函数,
// verifyProperty 的 length/name 描述符族, B1 rwjnvX)。
func reactionBuiltin(name string, fn func(args ...object.Value) object.Value) *object.BuiltinFunction {
	b := object.NewBuiltin(name, fn)
	b.NotConstructable = true
	b.SetFunctionLength(1)
	return b
}

// argAt 安全取第 i 个参数，越界时返回 undefined。
// Promise 回调接收的参数个数不受控，直接 args[0] 会 panic。
func argAt(args []object.Value, i int) object.Value {
	if i < len(args) && args[i] != nil {
		return args[i]
	}
	return object.UndefinedSingleton
}

// newAggregateError 构造 AggregateError (Promise.any 全部失败时使用)。
func newAggregateError(errors []object.Value, message string) object.Value {
	e := object.NewErrorWithName("AggregateError", message)
	e.SetProperty("errors", object.NewArray(errors))
	return e
}

func setupPromiseProto() *object.Object {
	p := object.NewObject()

	// then(onFulfilled, onRejected) —— 27.5.5.4: species 构造器 + 能力校验
	thenBuiltin := object.NewBuiltinMethod("then", func(this object.Value, args ...object.Value) object.Value {
		// 步骤 2: IsPromise(this) false → TypeError
		promise, ok := this.(*object.Promise)
		if !ok {
			return thisTypeError("Promise", "then", this)
		}
		var onFulfilled object.Value
		var onRejected object.Value
		if len(args) > 0 {
			onFulfilled = args[0]
		}
		if len(args) > 1 {
			onRejected = args[1]
		}
		// 步骤 3: C = SpeciesConstructor(promise, %Promise%)
		C := promiseCtor
		ctorVal, _ := promise.GetProperty("constructor")
		if cbErr := object.TakeCallbackError(); cbErr != nil {
			if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
				if e, isErr := thrown.(*object.Error); isErr && e != nil {
					return e
				}
			}
			return object.NewErrorWithName("Error", cbErr.Error())
		}
		// 规范 SpeciesConstructor: constructor 为 undefined → 用默认 %Promise%;
		// null 不是 undefined → Type 非 Object → TypeError (ctor-null 族)。
		if ctorVal != nil && !isUndefinedVal(ctorVal) {
			// 非 Object 的 constructor → TypeError (ctor-null 族)
			if !object.IsObjectValue(ctorVal) {
				return object.NewErrorWithName("TypeError", "Promise.prototype.then: constructor is not an object")
			}
			speciesVal := object.Value(nil)
			if sym := object.GetGlobalSymbol("Symbol.species"); sym != nil {
				// 沿原型链查 @@species; 内建构造器的 species 是 get 访问器,
				// 须调用 getter 取值 (与 TypedArraySpeciesCreate 同口径)。
				if desc, found := object.LookupSymbolPropertyDescriptorChain(ctorVal, sym); found {
					speciesVal = desc.Value
					if acc, isAcc := speciesVal.(*object.Accessor); isAcc {
						if acc.Getter == nil {
							speciesVal = object.UndefinedSingleton
						} else {
							speciesVal = object.CallFunction(acc.Getter, ctorVal)
							if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
								if e, isErr := ev.(*object.Error); isErr && e != nil {
									return e
								}
								return object.NewErrorWithName("Error", "species getter failed")
							}
						}
					}
				}
			}
			if speciesVal != nil && !isUndefinedVal(speciesVal) && speciesVal != object.NullSingleton {
				C = speciesVal
			} else {
				C = ctorVal
			}
		}
		if !isBuiltinPromiseCtor(C) {
			// 步骤 4: 用户构造器 → NewPromiseCapability(C) (executor 校验:
			// capability-executor-not-callable / called-twice 族); 步骤 5 反应
			// 经 capability settle。
			cap, errVal := newPromiseCapability(C)
			if errVal != nil {
				return errVal
			}
			hf := reactionBuiltin("__then_fulfilled", func(fargs ...object.Value) object.Value {
				if object.IsCallable(onFulfilled) {
					v := object.CallFunction(onFulfilled, object.UndefinedSingleton, argAt(fargs, 0))
					if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
						_ = cap.callReject(unwrapThrown(ev))
						return object.UndefinedSingleton
					}
					if e, isErr := v.(*object.Error); isErr && e != nil {
						_ = cap.callReject(e)
						return object.UndefinedSingleton
					}
					_ = cap.callResolve(v)
					return object.UndefinedSingleton
				}
				_ = cap.callResolve(argAt(fargs, 0))
				return object.UndefinedSingleton
			})
			hr := reactionBuiltin("__then_rejected", func(rargs ...object.Value) object.Value {
				if object.IsCallable(onRejected) {
					v := object.CallFunction(onRejected, object.UndefinedSingleton, argAt(rargs, 0))
					if ev := object.TakeThrownValue(); ev != object.UndefinedSingleton {
						_ = cap.callReject(unwrapThrown(ev))
						return object.UndefinedSingleton
					}
					if e, isErr := v.(*object.Error); isErr && e != nil {
						_ = cap.callReject(e)
						return object.UndefinedSingleton
					}
					_ = cap.callResolve(v)
					return object.UndefinedSingleton
				}
				_ = cap.callReject(argAt(rargs, 0))
				return object.UndefinedSingleton
			})
			promise.ThenCatch(hf, hr)
			return cap.result()
		}
		// 反应回调统一走微任务队列 (已结算也不同步执行), 实现在 object 层。
		return promise.ThenCatch(onFulfilled, onRejected)
	})
	p.SetBuiltinProperty("then", thenBuiltin)
	promiseProtoThen = thenBuiltin

	// catch(onRejected)
	p.SetBuiltinProperty("catch", object.NewBuiltinMethod("catch", func(this object.Value, args ...object.Value) object.Value {
		promise, ok := this.(*object.Promise)
		if !ok {
			return thisTypeError("Promise", "catch", this)
		}
		var onRejected object.Value
		if len(args) > 0 {
			onRejected = args[0]
		}
		return promise.Catch(onRejected)
	}))

	// finally(onFinally) (T7, ES2019 thenable 口径):
	//   1. RequireObjectCoercible(this);
	//   2. then = Get(this, "then"), 不可调用 → TypeError;
	//   3. 若 onFinally 不可调用, 直通包装器; 否则先调 onFinally 再透传结果;
	//   4. result = then.call(this, wrapperFulfilled, wrapperRejected)。
	p.SetBuiltinProperty("finally", object.NewBuiltinMethod("finally", func(this object.Value, args ...object.Value) object.Value {
		// RequireObjectCoercible
		if this == nil || this == object.UndefinedSingleton || this == object.NullSingleton {
			return object.NewTypeError("Promise.prototype.finally called on null or undefined")
		}
		var onFinally object.Value
		if len(args) > 0 {
			onFinally = args[0]
		}
		// Get(this, "then") — 支持用户覆写 (含普通对象 this)
		getter, hasGet := this.(interface {
			GetProperty(string) (object.Value, bool)
		})
		if !hasGet {
			return thisTypeError("Promise", "finally", this)
		}
		thenVal, _ := getter.GetProperty("then")
		if cbErr := object.TakeCallbackError(); cbErr != nil {
			// then getter 抛错 → 原样透传 (this-value-then-poisoned)
			if thrown := object.TakeCallbackErrorValue(); thrown != object.UndefinedSingleton {
				if e, isErr := thrown.(*object.Error); isErr {
					return e
				}
			}
			return object.NewErrorWithName("Error", cbErr.Error())
		}
		if !object.IsCallable(thenVal) {
			return object.NewTypeError("Promise.prototype.finally: 'then' is not a function")
		}
		// 直通包装器 (onFinally 不可调用): 值/原因原样透传
		passF := object.NewBuiltin("__finally_pass_f", func(fargs ...object.Value) object.Value {
			return argAt(fargs, 0)
		})
		passR := object.NewBuiltin("__finally_pass_r", func(fargs ...object.Value) object.Value {
			pr := object.NewPromise()
			pr.Reject(argAt(fargs, 0))
			return pr
		})
		if !object.IsCallable(onFinally) {
			return object.CallFunction(thenVal, this, passF, passR)
		}
		// onFinally 可调用: 包装器先执行 onFinally, 结果经 PromiseResolve 透传;
		// onFinally 抛错 → 该反应转为 rejection。
		finallyWrapper := func(mode int) object.Value {
			return object.NewBuiltin("__finally_wrapper", func(wargs ...object.Value) object.Value {
				v := argAt(wargs, 0)
				if object.IsCallable(onFinally) {
					ret := object.CallFunction(onFinally, object.UndefinedSingleton)
					if thrown := object.TakeThrownValue(); thrown != object.UndefinedSingleton {
						pr := object.NewPromise()
						// onFinally 抛错 → rejection reason 覆盖为抛出值
						pr.Reject(unwrapThrown(thrown))
						return pr
					}
					if ce, ok := ret.(*object.Error); ok && ce != nil {
						pr := object.NewPromise()
						pr.Reject(ce)
						return pr
					}
					// 规范 ThenFinally / ThenFinallyRejected: onFinally 的正常
					// 返回值不覆盖原 resolution value / rejection reason
					// (resolution-value-no-override 族)。
				}
				if mode == 1 {
					pr := object.NewPromise()
					pr.Resolve(v)
					return pr
				}
				pr := object.NewPromise()
				pr.Reject(v)
				return pr
			})
		}
		return object.CallFunction(thenVal, this, finallyWrapper(1), finallyWrapper(0))
	}))

	return p
}
