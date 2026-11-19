package stdlib

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// setupObjectGlobal 创建 Object 构造器。
//
// 与 Array 同理，返回 *object.BuiltinFunction: Object 的 typeof 必须是
// "function"，且 Object() / new Object() 必须可调用。
func setupObjectGlobal() *object.BuiltinFunction {
	o := object.NewBuiltin("Object", func(args ...object.Value) object.Value {
		// Object() / Object(null) / Object(undefined) → 新的空对象
		if len(args) == 0 {
			return object.NewObject()
		}
		v := args[0]
		switch v.(type) {
		case *object.Null, *object.Undefined:
			return object.NewObject()
		case *object.Object, *object.Array, *object.Map, *object.Set,
			*object.RegExp, *object.Promise, *object.Error, *object.Proxy,
			*object.Closure, *object.CompiledFunction,
			*object.BuiltinFunction, *object.BuiltinMethod,
			*object.PrimitiveObject:
			// 已是对象: 原样返回
			return v
		}
		// 原始值: 规范 ToObject —— 返回对应的包装对象, 使其 instanceof
		// 对应对构造器为真 (Array.prototype.map 等泛型算法的第三个回调
		// 参数 obj === ToObject(this), test262 大量用例据此断言)。
		return toObjectValue(v)
	})
	// Object(x) 从不抛异常: 传入 Error 对象时它是"值"而非异常
	// (否则 Object(Error()) 会被 VM 当成 throw，call-error 用例失败)。
	o.ReturnIsValue = true
	o.SetProperty("name", object.NewString("Object"))

	// Object.keys(obj): 自有可枚举属性的键
	o.SetProperty("keys", object.NewBuiltin("keys", func(args ...object.Value) object.Value {
		keys, errVal := ownKeysArg(args, "Object.keys")
		if errVal != nil {
			return errVal
		}
		result := make([]object.Value, len(keys))
		for i, k := range keys {
			result[i] = object.NewString(k)
		}
		return object.NewArray(result)
	}))

	// Object.values(obj)
	o.SetProperty("values", object.NewBuiltin("values", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewArray([]object.Value{})
		}
		keys, errVal := ownKeys(args[0])
		if errVal != nil {
			return errVal
		}
		result := make([]object.Value, 0, len(keys))
		for _, k := range keys {
			v, _ := getOwnProperty(args[0], k)
			result = append(result, v)
		}
		return object.NewArray(result)
	}))

	// Object.entries(obj)
	// 旧实现漏掉了数组分支，导致 Object.entries([1,2]) 返回 []。
	o.SetProperty("entries", object.NewBuiltin("entries", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewArray([]object.Value{})
		}
		keys, errVal := ownKeys(args[0])
		if errVal != nil {
			return errVal
		}
		result := make([]object.Value, 0, len(keys))
		for _, k := range keys {
			v, _ := getOwnProperty(args[0], k)
			result = append(result, object.NewArray([]object.Value{
				object.NewString(k),
				v,
			}))
		}
		return object.NewArray(result)
	}))

	// Object.assign(target, ...sources)
	o.SetProperty("assign", object.NewBuiltin("assign", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewTypeError("Object.assign: target cannot be undefined")
		}
		// RequireObjectCoercible(target): null/undefined → TypeError
		// (19.1.2.1.1 Target-Null/Target-Undefined)。
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.assign called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.assign called on non-object")
		}
		target, ok := args[0].(*object.Object)
		if !ok {
			// 原始值 target: ToObject 后只有字符串包装带自有索引属性, 且
			// 全部不可写 —— Set(key, value, true) → TypeError
			// (assignment-to-readonly-property-of-target-*)。其余原始值包装
			// 无自有属性, 属性全部写进包装对象 (丢弃, 不抛)。
			if s, isStr := args[0].(*object.String); isStr && object.UTF16Len(s.Value) > 0 {
				for i := 1; i < len(args); i++ {
					src := args[i]
					if src == object.UndefinedSingleton || src == object.NullSingleton {
						continue
					}
					if store, ok2 := src.(object.OwnPropertyStore); ok2 && len(store.EnumerableOwnKeys()) > 0 {
						for _, k := range store.EnumerableOwnKeys() {
							if idx, err := strconv.Atoi(k); err == nil && idx >= 0 && idx < object.UTF16Len(s.Value) {
								return object.NewTypeError("Cannot assign to read only property '%s' of string", k)
							}
						}
					}
				}
			}
			return args[0]
		}
		for i := 1; i < len(args); i++ {
			// 规范 ToObject(source) 后复制可枚举自有属性; 函数类/数组等
			// 走 OwnPropertyStore 统一接口, 不再只认 *object.Object。
			src := args[i]
			if src == object.UndefinedSingleton || src == object.NullSingleton {
				continue
			}
			if store, ok2 := src.(object.OwnPropertyStore); ok2 {
				for _, k := range store.EnumerableOwnKeys() {
					// 规范 19.1.2.1: Get(from, nextKey) —— 必须经 GetProperty
					// 调用访问器 (getter 抛错要向上传播, 见
					// source-get-attr-error.js)。
					val, _ := src.(interface {
						GetProperty(string) (object.Value, bool)
					}).GetProperty(k)
					// Set(to, nextKey, propValue, true): 写失败 → TypeError
					// (target-set-not-writable / target-is-frozen-* /
					// target-is-non-extensible-* / target-is-sealed-* 族)。
					if errVal := setOrThrow(target, k, val); errVal != nil {
						return errVal
					}
				}
			}
		}
		return target
	}))

	// Object.freeze(obj): 不可扩展，且所有自有数据属性变为不可写、不可配置。
	// 经 object.FreezeProperties 统一处理 *Object 与 *Array —— 数组的索引
	// 是隐式数据属性, 需要补写显式描述符才能真的变成只读 (否则 freeze 后
	// arr.push(1) 仍写得进去)。
	// proxy: freeze 走 [[PreventExtensions]] trap; trap 报 false → TypeError
	// (freeze/throws-when-false)。
	o.SetProperty("freeze", object.NewBuiltin("freeze", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.UndefinedSingleton
		}
		if res, handled, errVal := proxyTrapDispatch(args[0], "preventExtensions"); errVal != nil {
			return errVal
		} else if handled {
			if !object.IsTruthyValue(res) {
				return object.NewTypeError("'preventExtensions' trap returned false")
			}
		}
		freezeTarget := args[0]
		if p, ok := args[0].(*object.Proxy); ok && p.Target != nil {
			freezeTarget = p.Target
		}
		object.FreezeProperties(freezeTarget, true)
		return args[0]
	}))

	// Object.isFrozen(obj): 不可扩展且所有自有属性均不可写、不可配置
	o.SetProperty("isFrozen", object.NewBuiltin("isFrozen", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(true)
		}
		if !object.IsExtensible(args[0]) {
			return object.NewBoolean(!object.HasWritableOrConfigurableOwn(args[0]))
		}
		return object.NewBoolean(false)
	}))

	// Object.seal(obj): 不可扩展，所有自有属性不可配置 (但保持可写)。
	// proxy: seal 走 [[PreventExtensions]] trap; trap 报 false → TypeError
	// (seal/throws-when-false)。
	o.SetProperty("seal", object.NewBuiltin("seal", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.UndefinedSingleton
		}
		if res, handled, errVal := proxyTrapDispatch(args[0], "preventExtensions"); errVal != nil {
			return errVal
		} else if handled {
			if !object.IsTruthyValue(res) {
				return object.NewTypeError("'preventExtensions' trap returned false")
			}
		}
		sealTarget := args[0]
		if p, ok := args[0].(*object.Proxy); ok && p.Target != nil {
			sealTarget = p.Target
		}
		object.FreezeProperties(sealTarget, false)
		return args[0]
	}))

	// Object.isSealed(obj): 不可扩展且所有自有属性均不可配置
	o.SetProperty("isSealed", object.NewBuiltin("isSealed", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(true)
		}
		if !object.IsExtensible(args[0]) {
			return object.NewBoolean(!object.HasConfigurableOwn(args[0]))
		}
		return object.NewBoolean(false)
	}))

	// Object.create(proto)
	o.SetProperty("create", object.NewBuiltin("create", func(args ...object.Value) object.Value {
		// 19.1.2.2 Object.create(O, P)
		if len(args) == 0 {
			return object.NewTypeError("Object.create: prototype argument is required")
		}
		var proto object.Value
		if _, isNull := args[0].(*object.Null); isNull {
			proto = object.NullSingleton
		} else if !object.IsObjectValue(args[0]) {
			// 规范: Type(O) 不是 Object 也不是 null → TypeError
			// (rFXXbq: Object/create "undefined is not an object" 族)。
			return object.NewTypeError("Object prototype may only be an Object or null: %s", toStr(args[0]))
		} else {
			proto = args[0]
		}
		obj := object.NewObjectWithProto(proto)
		// properties 参数: ToObject(P) 后对每个自有可枚举键取描述符并
		// CreateDataPropertyOrThrow (经 defineOneProperty, 与 defineProperties
		// 同一收口)。规范 ObjectDefineProperties step 2: props = ToObject(P) ——
		// null → TypeError (15.2.3.5-4-3); 非空字符串包装的每个码元都是
		// 非对象 descObj → ToPropertyDescriptor TypeError
		// (properties-arg-to-object-non-empty-string); 其余原始值包装无
		// 自有可枚举属性, 按空处理 (可观测行为一致)。
		if len(args) > 1 {
			if _, isUndef := args[1].(*object.Undefined); !isUndef {
				if _, isNull2 := args[1].(*object.Null); isNull2 {
					return object.NewTypeError("Object.create: properties argument cannot be null")
				}
				if s, isStr := args[1].(*object.String); isStr {
					if object.UTF16Len(s.Value) > 0 {
						return object.NewTypeError("Property description must be an object")
					}
					// 空字符串: 包装对象无可枚举自有属性 → 空描述符集。
				} else if store, ok := args[1].(object.OwnPropertyStore); ok {
					for _, key := range store.EnumerableOwnKeys() {
						descVal, found := args[1].GetProperty(key)
						if !found {
							continue
						}
						if errVal := defineOneProperty(obj, obj, key, descVal); errVal != nil {
							return errVal
						}
					}
				}
				// 其余原始值 (number/boolean/symbol): ToObject 后无自有可枚举属性 → 空处理。
			}
		}
		return obj
	}))

	// Object.getPrototypeOf(obj)
	o.SetProperty("getPrototypeOf", object.NewBuiltin("getPrototypeOf", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NullSingleton
		}
		// 规范 19.1.2.9 step 2-3: RequireObjectCoercible + ToObject —— 原始值
		// 装箱后取其 [[Prototype]]; null/undefined 抛 TypeError
		// (15.2.3.2-0-3 / 15.2.3.2-1 / 15.2.3.2-1-2 / 15.2.3.2-1-3, B8 rm4Zcc)。
		switch args[0].(type) {
		case *object.Null, *object.Undefined:
			return object.NewTypeError("Object.getPrototypeOf: called on null or undefined")
		case *object.Number:
			return object.NumberProto
		case *object.String:
			return object.StringProto
		case *object.Boolean:
			return object.GetBooleanProto()
		case *object.Symbol:
			return object.GetSymbolProto()
		case *object.BigInt:
			return object.GetBigIntProto()
		}
		// proxy: getPrototypeOf trap 分发 (T5); 结果必须是对象或 null
		if res, handled, errVal := proxyTrapDispatch(args[0], "getPrototypeOf"); errVal != nil {
			return errVal
		} else if handled {
			if res == nil || res == object.UndefinedSingleton || res == object.NullSingleton {
				res = object.NullSingleton
			} else if !object.IsObjectLike(res) {
				return object.NewTypeError("'getPrototypeOf' trap must return an object or null")
			}
			// 不变式 (10.5.4 step 13): target 不可扩展且 trap 结果与 target
			// 实际 [[Prototype]] 不同 → TypeError
			// (not-extensible-not-same-proto-throws / instanceof- 变体)。
			if p, isP := args[0].(*object.Proxy); isP && p.Target != nil {
				if ext, ev := targetIsExtensibleOf(p.Target); ev != nil {
					return ev
				} else if !ext {
					tp, ev2 := targetProtoOf(p.Target)
					if ev2 != nil {
						return ev2
					}
					if res != tp {
						return object.NewTypeError("'getPrototypeOf' trap must return the same value as the [[GetPrototypeOf]] of the proxy target")
					}
				}
			}
			return res
		}
		if g, ok := args[0].(*object.GlobalObject); ok {
			if g.Proto != nil {
				return g.Proto
			}
			return object.NullSingleton
		}
		if obj, ok := args[0].(*object.Object); ok {
			if obj.Proto != nil {
				return obj.Proto
			}
			return object.NullSingleton
		}
		// Error 实例: [[Prototype]] = 按 Name 解析的 NativeError.prototype
		// (B1 rwjnvX) —— TypeError 实例返回 %TypeError.prototype%, 不再恒
		// 回 %Error.prototype%; 未注册的 Name 回落旧口径 (B8 rm4Zcc,
		// 15.2.3.2-2-27)。
		if e, isErr := args[0].(*object.Error); isErr {
			if p := object.ErrorProtoOf(e); p != nil {
				return p
			}
			if stdlibErrorProto != nil {
				return stdlibErrorProto
			}
			return object.NullSingleton
		}
		if arr, ok := args[0].(*object.Array); ok {
			if p := arr.GetProto(); p != nil {
				return p
			}
			return object.NullSingleton
		}
		// TypedArray 实例: [[Prototype]] = 本类型 prototype (派生类实例则
		// 是 ProtoOverride —— super() 回填的派生类原型)。
		if ta, ok := args[0].(*object.TypedArray); ok {
			if p := ta.GetProto(); p != nil {
				return p
			}
			return object.NullSingleton
		}
		// Date 实例的 [[Prototype]] 取自包级注册表 (object.DateProto),
		// 与 Temporal 各类型同法 —— 缺这条分支会退化成 null, 于是
		// `Object.getPrototypeOf(new Date()) === Date.prototype` 为 false
		// (ryGXAJ)。
		if d, ok := args[0].(*object.Date); ok {
			if p := d.GetProto(); p != nil {
				return p
			}
			return object.NullSingleton
		}
		// RegExp 实例的 [[Prototype]] = %RegExp.prototype% (rEXjyz)。
		// *RegExp 的原型放在包级注册表 (object.RegExpProto), 缺这条分支会
		// 退化成 null, 于是 `Object.getPrototypeOf(/a/) === RegExp.prototype`
		// 为 false —— 与 *Date 同法。
		if re, ok := args[0].(*object.RegExp); ok {
			if p := re.GetProto(); p != nil {
				return p
			}
			return object.NullSingleton
		}
		// Promise 实例的 [[Prototype]] = %Promise.prototype% (rj9MwH)。
		// *Promise 的原型同样在包级注册表 (object.PromiseProto), 缺这条分支
		// 会退化成 null —— 于是 `Object.getPrototypeOf(Promise.resolve())
		// === Promise.prototype` 为 false。与 *Date / *RegExp 同法。
		if pr, ok := args[0].(*object.Promise); ok {
			if p := pr.GetProto(); p != nil {
				return p
			}
			return object.NullSingleton
		}
		// async generator 实例的 [[Prototype]] = 该 async generator 函数
		// 自己的 .prototype 对象 (其 [[Prototype]] 才是 %AsyncGeneratorPrototype%)。
		if ag, ok := args[0].(*object.AsyncGenerator); ok {
			if ag.Proto != nil {
				return ag.Proto
			}
			return object.NullSingleton
		}
		// TypedArray 各具体构造器: [[Prototype]] = %TypedArray%
		// (test262 harness 经 Object.getPrototypeOf(Int8Array) 取它)。
		if bf, ok := args[0].(*object.BuiltinFunction); ok {
			if _, isTA := object.LookupTAKind(bf.Name); isTA {
				if p := object.GetTypedArrayCtorProto(); p != nil {
					return p
				}
			}
		}
		// 函数对象: [[Prototype]] 按种类指向 Function/GeneratorFunction/
		// AsyncFunction/AsyncGeneratorFunction.prototype。
		if fp := object.FuncPrototypeOf(args[0]); fp != nil {
			return fp
		}
		// 兜底: 任意实现了鸭子类型入口 `GetProto() Value` 的内建实例
		// (Temporal 各类型等)。缺这条分支会退化成 null, 于是
		// `Object.getPrototypeOf(new Temporal.PlainDate(...))
		//   === Temporal.PlainDate.prototype` 为 false。
		if ph, ok := args[0].(interface{ GetProto() object.Value }); ok {
			if p := ph.GetProto(); p != nil {
				return p
			}
		}
		return object.NullSingleton
	}))

	// Object.entries already above

	// Object.fromEntries(iterable)
	o.SetProperty("fromEntries", object.NewBuiltin("fromEntries", func(args ...object.Value) object.Value {
		// RequireObjectCoercible(iterable): 缺参/null/undefined → TypeError
		// (requires-argument)。
		if len(args) == 0 {
			return object.NewTypeError("Object.fromEntries requires an iterable")
		}
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.fromEntries requires an iterable")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.fromEntries requires an iterable")
		}
		obj := object.NewObject()
		addEntry := func(entry object.Value) object.Value {
			// AddEntriesFromIterable: entry = ToObject(value) —— null/
			// undefined/原始值一律 TypeError (iterator-closed-for-null-entry /
			// string-entry-primitive-throws); key 走 ToPropertyKey —— Symbol
			// 键合法 (supports-symbols), 落符号键槽。
			if !object.IsObjectValue(entry) {
				return object.NewTypeError("Object.fromEntries: entry is not an object")
			}
			keyVal, _ := entry.GetProperty("0")
			if sym, isSym := keyVal.(*object.Symbol); isSym {
				obj.DefineOwnSymbolProperty(sym, object.PropertyDescriptor{
					Value: func() object.Value {
						if v, _ := entry.GetProperty("1"); v != nil {
							return v
						}
						return object.UndefinedSingleton
					}(),
					Writable:     true,
					Enumerable:   true,
					Configurable: true,
				})
				return nil
			}
			key, errVal := toPropertyKey(keyVal)
			if errVal != nil {
				return errVal
			}
			val, _ := entry.GetProperty("1")
			obj.SetProperty(key, val)
			return nil
		}
		closeIterator := func(iterator object.Value) {
			retFn, found := iterator.GetProperty("return")
			if found && retFn != nil && object.IsCallable(retFn) {
				object.CallFunction(retFn, iterator)
				object.TakeCallbackErrorValue() // 关闭期错误按规范吞掉
			}
		}
		runIterator := func(iterator object.Value) object.Value {
			for {
				nextFn, found := iterator.GetProperty("next")
				if !found || nextFn == nil || !object.IsCallable(nextFn) {
					// next 不可调用: 直接 TypeError, 不关闭迭代器
					// (iterator-not-closed-for-uncallable-next)。
					return object.NewTypeError("Object.fromEntries: iterator has no callable next")
				}
				result := object.CallFunction(nextFn, iterator)
				if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
					return ev // next 抛错: 按规范不关闭, 原样上抛
				}
				if !object.IsObjectValue(result) {
					// next 返回非对象: TypeError 且不关闭
					// (iterator-not-closed-for-next-returning-non-object)。
					return object.NewTypeError("Object.fromEntries: iterator result is not an object")
				}
				if doneVal, _ := result.GetProperty("done"); doneVal != nil && object.IsTruthyValue(doneVal) {
					return nil // done → 正常结束, 不调用 return
				}
				entry, _ := result.GetProperty("value")
				if errVal := addEntry(entry); errVal != nil {
					// 体内失败: 关闭迭代器后上抛 (iterator-closed-for-*-entry)。
					closeIterator(iterator)
					return errVal
				}
			}
		}
		// 数组快路径: 元素逐个走同一 addEntry 校验 (['ab'] 字符串项 → TypeError)。
		if arr, ok := args[0].(*object.Array); ok {
			for _, entry := range arr.Elements {
				if errVal := addEntry(entry); errVal != nil {
					return errVal
				}
			}
			return obj
		}
		// GetIterator: @@iterator 缺失/nullish → TypeError "not iterable";
		// 存在但不可调用 → TypeError; 调用结果非对象 → TypeError。
		sym := object.GetGlobalSymbol("Symbol.iterator")
		var iterFn object.Value
		if sym != nil {
			if sp, ok := args[0].(object.SymbolPropertyStore); ok {
				if v, found := sp.GetSymbolProperty(sym); found && v != nil &&
					v != object.UndefinedSingleton && v != object.NullSingleton {
					iterFn = v
				}
			}
		}
		if iterFn == nil {
			return object.NewTypeError("Object.fromEntries: argument is not iterable")
		}
		if !object.IsCallable(iterFn) {
			return object.NewTypeError("Object.fromEntries: [Symbol.iterator] is not a function")
		}
		iterator := object.CallFunction(iterFn, args[0])
		if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
			return ev
		}
		if !object.IsObjectValue(iterator) {
			return object.NewTypeError("Object.fromEntries: iterator is not an object")
		}
		if errVal := runIterator(iterator); errVal != nil {
			return errVal
		}
		return obj
	}))

	// Object.is(a, b): 同值相等比较
	o.SetProperty("is", object.NewBuiltin("is", func(args ...object.Value) object.Value {
		if len(args) < 2 {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(objectIs(args[0], args[1]))
	}))

	// Object.hasOwn(obj, prop): 检查对象是否有自身属性
	o.SetProperty("hasOwn", object.NewBuiltin("hasOwn", func(args ...object.Value) object.Value {
		if len(args) < 2 {
			return object.NewBoolean(false)
		}
		// RequireObjectCoercible(O) 先于 ToPropertyKey(P): undefined/null
		// 抛 TypeError 且不得触发 key 的 toString/valueOf
		// (toobject_null / toobject_before_topropertykey)。
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.hasOwn called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.hasOwn called on non-object")
		}
		key, keyErr := toPropertyKey(args[1])
		if keyErr != nil {
			return keyErr
		}
		// Symbol 键: 查 SymbolPropertyStore 的符号键槽 (字符串键查询不覆盖)。
		if sym, isSym := args[1].(*object.Symbol); isSym {
			if sp, ok := args[0].(object.SymbolPropertyStore); ok {
				_, found := sp.GetSymbolPropertyDescriptor(sym)
				return object.NewBoolean(found)
			}
			return object.NewBoolean(false)
		}
		// 统一自有属性接口: 普通对象 / globalThis / 数组 / 函数类一视同仁
		// (数组索引与 length、函数的 name/length/prototype 都是自有属性)。
		if store, ok := args[0].(object.OwnPropertyStore); ok {
			// 越界 TypedArray 的整数索引查询按规范抛 TypeError。
			v, errVal := ownPropResult(func() bool { return store.HasOwn(key) })
			if errVal != nil {
				return errVal
			}
			return object.NewBoolean(v)
		}
		return object.NewBoolean(false)
	}))

	// Object.defineProperty(obj, prop, descriptor)
	o.SetProperty("defineProperty", object.NewBuiltin("defineProperty", func(args ...object.Value) object.Value {
		// 规范: O/P/Attributes 缺一不可 —— 缺参 → ToObject/ToPropertyKey/
		// ToPropertyDescriptor 链上必然 TypeError (15.2.3.6 族)。
		if len(args) < 3 {
			return object.NewTypeError("Object.defineProperty called on non-object")
		}
		// Type(O) 不是 Object → TypeError (ES5 15.2.3.6 step 1 至今保留;
		// 15.2.3.6-1-x 族: undefined/null/number/string/boolean 全抛)。
		if !object.IsObjectValue(args[0]) {
			return object.NewTypeError("Object.defineProperty called on non-object")
		}
		// ToPropertyKey 先于描述符解析 (15.2.3.6-2-47: P 是既无 toString
		// 又无有效 valueOf 的对象 → TypeError)。
		if _, isSym := args[1].(*object.Symbol); !isSym {
			if key, errVal := toPropertyKey(args[1]); errVal != nil {
				return errVal
			} else {
				args[1] = object.NewString(key)
			}
		}
		// ToPropertyDescriptor: 非对象/混用访问器与数据字段/访问器不可调用
		// → TypeError (15.2.3.6-3-x, 在 trap 分发与 target 落点之前)。
		parsed, errVal := toPropertyDescriptor(args[2])
		if errVal != nil {
			return errVal
		}
		// globalThis: 在全局环境上定义自有属性 (var/函数声明之外的新绑定)。
		if g, ok := args[0].(*object.GlobalObject); ok {
			if _, isSym := args[1].(*object.Symbol); isSym {
				return args[0] // 全局环境无符号键绑定，静默忽略
			}
			desc := parsedToDesc(parsed)
			g.DefineGlobal(toStr(args[1]), desc)
			return args[0]
		}
		// proxy: defineProperty trap 分发 (T5); falsy → TypeError (始终严格语义)
		if res, handled, errVal := proxyTrapDispatch(args[0], "defineProperty", args[1], args[2]); errVal != nil {
			return errVal
		} else if handled {
			if !object.IsTruthyValue(res) {
				return object.NewTypeError("'defineProperty' trap returned false")
			}
			// [[DefineOwnProperty]] 不变式 (Proxy/defineProperty targetdesc-*):
			// trap 报 true 后仍须对 target 描述符做兼容性校验。
			return proxyDefinePropertyInvariant(args[0], args[1], parsed)
		}
		// 无 trap: 解包 target 走下方 OwnPropertyStore 通道 (此前 *Proxy 非
		// OwnPropertyStore, 静默 no-op —— trap-is-undefined 转发族)。
		// target 仍是 proxy (trap-is-null-target-is-proxy 族) 时逐层试 trap。
		for {
			p, isProxy := args[0].(*object.Proxy)
			if !isProxy || p.Target == nil {
				break
			}
			if res, handled, errVal := proxyTrapDispatch(args[0], "defineProperty", args[1], args[2]); errVal != nil {
				return errVal
			} else if handled {
				if !object.IsTruthyValue(res) {
					return object.NewTypeError("'defineProperty' trap returned false")
				}
				return proxyDefinePropertyInvariant(args[0], args[1], parsed)
			}
			args[0] = p.Target
		}
		// 统一自有属性接口: 普通对象 / 数组 / 函数类都经 OwnPropertyStore
		// 落地描述符 —— 数组索引访问器、函数 name/length 重定义等此前进
		// 非 *Object 分支一律静默 no-op, 现在走各类型自己的描述符通道。
		if store, ok := args[0].(object.OwnPropertyStore); ok {
			if sym, isSym := args[1].(*object.Symbol); isSym {
				// Symbol 键: *Object 有 SymbolProperties; *Array/*TypedArray
				// 经 SymbolPropertyStore 亦有符号键槽。其余实现
				// OwnPropertyStore 的类型 (函数类等) 无符号键属性, 静默忽略。
				if sp, ok := args[0].(object.SymbolPropertyStore); ok {
					if errVal := defineOneSymbolProperty(sp, sym, args[2], args[0]); errVal != nil {
						return errVal
					}
				}
				return args[0]
			}
			if errVal := defineOneProperty(args[0], store, toStr(args[1]), args[2]); errVal != nil {
				return errVal
			}
			return args[0]
		}
		// Date/Map/Set 等无属性槽位的对象类型: 保持 no-op (Gox 对象模型
		// 残余 —— 与赋值语义 d.foo=1 的现状一致, 待独立开单)。
		return args[0]
	}))

	// Object.getOwnPropertyDescriptor(obj, prop)
	o.SetProperty("getOwnPropertyDescriptor", object.NewBuiltin("getOwnPropertyDescriptor", func(args ...object.Value) object.Value {
		// Proxy: [[GetOwnProperty]] 走 trap 分发 (9.5.5), 支持规范不变式
		// 校验 (rFXXbq: Proxy/getOwnPropertyDescriptor 回归族)。
		// 缺第二参按 undefined 属性键处理 (null-handler.js: revoked proxy
		// 的单参调用必须经 [[GetOwnProperty]] 抛 TypeError)。
		if pxy, ok := args[0].(*object.Proxy); ok {
			prop := object.Value(object.UndefinedSingleton)
			if len(args) > 1 {
				prop = args[1]
			}
			return proxyGetOwnPropertyDescriptor(pxy, prop)
		}
		if len(args) < 2 {
			return object.UndefinedSingleton
		}
		// globalThis: 自有属性来自全局环境记录 (顶层 var/函数声明/隐式赋值/
		// 内建全局); 顶层 let/const/class 只存在于词法环境，不是自有属性。
		if g, ok := args[0].(*object.GlobalObject); ok {
			if _, isSym := args[1].(*object.Symbol); isSym {
				return object.UndefinedSingleton // 全局环境无符号键绑定
			}
			desc, exists := g.OwnDescriptor(toStr(args[1]))
			if !exists {
				return object.UndefinedSingleton
			}
			return describeProperty(desc)
		}
		// Symbol 键: *Object 与 *Array/*TypedArray 经 SymbolPropertyStore 查到
		// 各自符号键描述符; 其余实现 OwnPropertyStore 的类型无符号键槽, 按
		// 规范返回 undefined (而非 TypeError)。
		if sym, isSym := args[1].(*object.Symbol); isSym {
			if sobj, ok := args[0].(object.SymbolPropertyStore); ok {
				sdesc, found := sobj.GetSymbolPropertyDescriptor(sym)
				if !found {
					return object.UndefinedSingleton
				}
				return describeProperty(sdesc)
			}
			if _, ok := args[0].(object.OwnPropertyStore); ok {
				return object.UndefinedSingleton
			}
			// undefined/null (含 Symbol 键) 仍按 ToObject 抛 TypeError。
			if _, isUndef := args[0].(*object.Undefined); isUndef {
				return object.NewTypeError("Object.getOwnPropertyDescriptor called on non-object")
			}
			if _, isNull := args[0].(*object.Null); isNull {
				return object.NewTypeError("Object.getOwnPropertyDescriptor called on non-object")
			}
			// 规范 (ES2017+): 其它非对象返回 undefined, 不再抛 TypeError。
			return object.UndefinedSingleton
		}
		// 字符串键: 统一走自有属性接口 —— 普通对象查 Properties, 数组查
		// 索引/length/defineProperty 描述符, 函数查 name/length/prototype。
		if store, ok := args[0].(object.OwnPropertyStore); ok {
			desc, exists := store.OwnDescriptor(toStr(args[1]))
			if !exists {
				return object.UndefinedSingleton
			}
			return describeProperty(desc)
		}
		// 规范: ToObject(O) —— undefined/null 抛 TypeError (ES5 15.2.3.3-1-1/2
		// 亦然); 其余原始值经包装对象查找自有属性。String 包装的索引字符与
		// length 是真实的自有属性 (不可写/可枚举/不可配置), 必须描述出来
		// 而非一律 undefined。
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.getOwnPropertyDescriptor called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.getOwnPropertyDescriptor called on non-object")
		}
		// BigInt 不包装: Gox 的 BigInt 包装对象缺 [[BigIntData]] 内部槽语义,
		// 包装会改变 Object.prototype.toString 等标签判定 (实测回归), 且
		// BigInt 包装对象没有自有属性, 直接返回 undefined 即为正确结果。
		if _, isBig := args[0].(*object.BigInt); isBig {
			return object.UndefinedSingleton
		}
		wrapped := toObjectValue(args[0])
		if store, ok := wrapped.(object.OwnPropertyStore); ok {
			desc, exists := store.OwnDescriptor(toStr(args[1]))
			if !exists {
				return object.UndefinedSingleton
			}
			return describeProperty(desc)
		}
		return object.UndefinedSingleton
	}))

	// Object.setPrototypeOf(obj, proto)
	o.SetProperty("setPrototypeOf", object.NewBuiltin("setPrototypeOf", func(args ...object.Value) object.Value {
		// RequireObjectCoercible(O): undefined/null → TypeError
		// (o-not-obj-coercible)。
		if len(args) == 0 {
			return object.NewTypeError("Object.setPrototypeOf called on null or undefined")
		}
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.setPrototypeOf called on null or undefined")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.setPrototypeOf called on null or undefined")
		}
		// Type(proto) ∈ {Object, Null} 先校验 (proto-not-obj)。
		if args[1] != nil {
			if _, isNullP := args[1].(*object.Null); !isNullP && !object.IsObjectValue(args[1]) {
				return object.NewTypeError("Object.setPrototypeOf: proto must be an object or null")
			}
		}
		// proxy: setPrototypeOf trap 分发 (T5); falsy → TypeError, 成功返回 obj
		if res, handled, errVal := proxyTrapDispatch(args[0], "setPrototypeOf", args[1]); errVal != nil {
			return errVal
		} else if handled {
			if !object.IsTruthyValue(res) {
				return object.NewTypeError("'setPrototypeOf' trap returned false")
			}
			// 不变式 (10.5.5 step 26): target 不可扩展且 proto 与 target 实际
			// [[Prototype]] 不同 → TypeError。
			if p, isP := args[0].(*object.Proxy); isP && p.Target != nil {
				if ext, ev := targetIsExtensibleOf(p.Target); ev != nil {
					return ev
				} else if !ext {
					tp, ev2 := targetProtoOf(p.Target)
					if ev2 != nil {
						return ev2
					}
					var want object.Value = object.NullSingleton
					if args[1] != nil {
						if _, isNullP := args[1].(*object.Null); !isNullP {
							want = args[1]
						}
					}
					if want != tp {
						return object.NewTypeError("'setPrototypeOf' trap: proxy target is non-extensible but the prototype does not match the target's prototype")
					}
				}
			}
			return args[0]
		}
		obj, ok := args[0].(*object.Object)
		if !ok {
			return args[0]
		}
		// OrdinarySetPrototypeOf (9.1.2.1): 同原型直接成功; 不可扩展 → 失败;
		// 环检测 (proto 链中含 O) → 失败 —— 此前无环检测,
		// Object.setPrototypeOf(Object.prototype, Array.prototype) 直接造成
		// 全局属性查找栈溢出崩溃。
		var newProto object.Value
		if _, isNullP := args[1].(*object.Null); isNullP || args[1] == nil {
			newProto = nil
		} else {
			newProto = args[1]
		}
		if !sameProto(obj.Proto, newProto) {
			if !obj.Extensible {
				return object.NewTypeError("Object.setPrototypeOf: object is not extensible")
			}
			if newProto != nil {
				cur := newProto
				for {
					if cur == args[0] {
						return object.NewTypeError("Object.setPrototypeOf: prototype chain cycle detected")
					}
					next, ok2 := cur.(*object.Object)
					if !ok2 || next.Proto == nil {
						break
					}
					cur = next.Proto
				}
			}
			obj.Proto = newProto
		}
		return args[0]
	}))

	// Object.getOwnPropertyNames(obj)
	o.SetProperty("getOwnPropertyNames", object.NewBuiltin("getOwnPropertyNames", func(args ...object.Value) object.Value {
		// RequireObjectCoercible: undefined/null → TypeError
		// (15.2.3.4-1-2 / -1-3)。
		if len(args) == 0 {
			return object.NewTypeError("Cannot convert undefined or null to object")
		}
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.getOwnPropertyNames called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.getOwnPropertyNames called on non-object")
		}
		if g, ok := args[0].(*object.GlobalObject); ok {
			ks := g.OwnKeys()
			result := make([]object.Value, len(ks))
			for i, k := range ks {
				result[i] = object.NewString(k)
			}
			return object.NewArray(result)
		}
		// proxy: ownKeys trap 分发 + 不变式校验 (T6), 按键类型过滤字符串键。
		if p, ok := args[0].(*object.Proxy); ok {
			keys, errVal := proxyOwnKeysInvariant(p)
			if errVal != nil {
				return errVal
			}
			result := make([]object.Value, 0, len(keys))
			for _, k := range keys {
				if _, isSym := k.(*object.Symbol); !isSym {
					result = append(result, k)
				}
			}
			return object.NewArray(result)
		}
		// 统一自有属性接口: 普通对象 / 数组 (索引+length+defineProperty 键) /
		// 函数类 (length/name/prototype/静态成员) 一视同仁。
		if store, ok := args[0].(object.OwnPropertyStore); ok {
			ks := store.OwnKeys()
			result := make([]object.Value, len(ks))
			for i, k := range ks {
				result[i] = object.NewString(k)
			}
			return object.NewArray(result)
		}
		return object.NewArray([]object.Value{})
	}))

	// Object.getOwnPropertyDescriptors(obj)
	o.SetProperty("getOwnPropertyDescriptors", object.NewBuiltin("getOwnPropertyDescriptors", func(args ...object.Value) object.Value {
		// RequireObjectCoercible: undefined/null → TypeError
		// (exception-not-object-coercible)。
		if len(args) == 0 {
			return object.NewTypeError("Cannot convert undefined or null to object")
		}
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.getOwnPropertyDescriptors called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.getOwnPropertyDescriptors called on non-object")
		}
		if g, ok := args[0].(*object.GlobalObject); ok {
			result := object.NewObject()
			for _, k := range g.OwnKeys() {
				if d, ok := g.OwnDescriptor(k); ok {
					result.SetProperty(k, describeProperty(d))
				}
			}
			return result
		}
		if obj, ok := args[0].(*object.Object); ok {
			result := object.NewObject()
			for _, k := range obj.Keys() {
				propDesc, ok := obj.Properties[k]
				if !ok {
					continue
				}
				desc := object.NewObject()
				if acc, isAcc := propDesc.Value.(*object.Accessor); isAcc {
					if acc.Getter != nil {
						desc.SetProperty("get", acc.Getter)
					} else {
						desc.SetProperty("get", object.UndefinedSingleton)
					}
					if acc.Setter != nil {
						desc.SetProperty("set", acc.Setter)
					} else {
						desc.SetProperty("set", object.UndefinedSingleton)
					}
				} else {
					// 反映真实的 writable，而不是硬编码 true
					desc.SetProperty("value", propDesc.Value)
					desc.SetProperty("writable", object.NewBoolean(propDesc.Writable))
				}
				desc.SetProperty("enumerable", object.NewBoolean(propDesc.Enumerable))
				desc.SetProperty("configurable", object.NewBoolean(propDesc.Configurable))
				result.SetProperty(k, desc)
			}
			return result
		}
		// 统一自有属性接口: 数组 / 函数类同样返回逐属性描述符表。
		if store, ok := args[0].(object.OwnPropertyStore); ok {
			result := object.NewObject()
			for _, k := range store.OwnKeys() {
				if d, ok := store.OwnDescriptor(k); ok {
					result.SetProperty(k, describeProperty(d))
				}
			}
			return result
		}
		return object.NewObject()
	}))

	// Object.isExtensible(obj)
	// 经 object.IsExtensible 统一分发: *Object 与 *Array 承载真实状态,
	// 其余对象类型 (函数/代理/async generator 等) 按可扩展处理 ——
	// 之前只认 *Object, 于是 Object.isExtensible(AsyncGeneratorFunction)
	// 误报 false (built-ins/AsyncGeneratorFunction/extensibility.js),
	// Object.isExtensible(数组) 又读不到 preventExtensions 的效果。
	o.SetProperty("isExtensible", object.NewBuiltin("isExtensible", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(false)
		}
		// proxy: isExtensible trap 分发 (T5)
		if res, handled, errVal := proxyTrapDispatch(args[0], "isExtensible"); errVal != nil {
			return errVal
		} else if handled {
			r := object.IsTruthyValue(res)
			// 不变式 (10.5.3 step 9): trap 结果必须与 target 的 [[Extensible]]
			// 一致, 否则 TypeError (return-is-different-from-target)。
			if p, isP := args[0].(*object.Proxy); isP && p.Target != nil {
				if ext, ev := targetIsExtensibleOf(p.Target); ev != nil {
					return ev
				} else if ext != r {
					return object.NewTypeError("'isExtensible' trap result is inconsistent with the extensibility of the proxy target")
				}
			}
			return object.NewBoolean(r)
		}
		// 无 trap: 解包 target 取真实 Extensible 状态 (此前 *Proxy 不被
		// IsExtensible 识别, 无 trap 也误报 false)。
		if p, ok := args[0].(*object.Proxy); ok && p.Target != nil {
			return object.NewBoolean(object.IsExtensible(p.Target))
		}
		return object.NewBoolean(object.IsExtensible(args[0]))
	}))

	// Object.preventExtensions(obj)
	o.SetProperty("preventExtensions", object.NewBuiltin("preventExtensions", func(args ...object.Value) object.Value {
		if len(args) > 0 {
			// proxy: preventExtensions trap 分发 (T5); falsy → TypeError
			if res, handled, errVal := proxyTrapDispatch(args[0], "preventExtensions"); errVal != nil {
				return errVal
			} else if handled {
				if !object.IsTruthyValue(res) {
					return object.NewTypeError("'preventExtensions' trap returned false")
				}
				// 不变式 (10.5.2 step 15-18): 先把 target 置为不可扩展, 若
				// trap 返回 true 但 target 原本可扩展 → TypeError
				// (return-true-target-is-extensible)。
				if p, isP := args[0].(*object.Proxy); isP && p.Target != nil {
					if ext, ev := targetIsExtensibleOf(p.Target); ev != nil {
						return ev
					} else if ext {
						object.PreventExtensions(p.Target)
						return object.NewTypeError("'preventExtensions' trap returned true but the proxy target is extensible")
					}
				}
				return args[0]
			}
			// 无 trap: 解包 target 落地 PreventExtensions
			if p, ok := args[0].(*object.Proxy); ok && p.Target != nil {
				object.PreventExtensions(p.Target)
				return args[0]
			}
			object.PreventExtensions(args[0])
			return args[0]
		}
		return object.UndefinedSingleton
	}))

	// Object.defineProperties(obj, descriptors) (ES5)
	// 批量 defineProperty: descriptors 的每个自有属性 {value, get, set...}
	// 都定义到 obj 上，返回 obj。
	o.SetProperty("defineProperties", object.NewBuiltin("defineProperties", func(args ...object.Value) object.Value {
		// 规范: Type(O) 非 Object → TypeError (15.2.3.7 step 1); 属性不足
		// 一样过不了校验链。
		if len(args) < 2 || !object.IsObjectValue(args[0]) {
			return object.NewTypeError("Object.defineProperties called on non-object")
		}
		// Properties = ToObject(props): null/undefined → TypeError
		// (15.2.3.7-2-1/-2-2); 非空字符串包装的每个码元都是非对象 descObj →
		// ToPropertyDescriptor TypeError; 其余原始值包装无可枚举自有属性 → 空处理。
		if _, isUndef := args[1].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.defineProperties: properties cannot be undefined")
		}
		if _, isNull := args[1].(*object.Null); isNull {
			return object.NewTypeError("Object.defineProperties: properties cannot be null")
		}
		var propsStore object.OwnPropertyStore
		if s, isStr := args[1].(*object.String); isStr {
			if object.UTF16Len(s.Value) > 0 {
				return object.NewTypeError("Property description must be an object")
			}
			return args[0] // 空字符串: 无可枚举自有属性
		} else if store, ok := args[1].(object.OwnPropertyStore); ok {
			propsStore = store
		} else {
			return args[0] // number/boolean/symbol 等原始值: 包装无自有可枚举属性
		}
		// 统一自有属性接口: 普通对象 / 数组 / 函数类都可作 target。
		store, ok := args[0].(object.OwnPropertyStore)
		if !ok {
			return object.NewTypeError("Object.defineProperties: target must be an object")
		}
		for _, key := range propsStore.EnumerableOwnKeys() {
			descVal, found := args[1].GetProperty(key)
			if !found {
				continue
			}
			if errVal := defineOneProperty(args[0], store, key, descVal); errVal != nil {
				return errVal
			}
		}
		return args[0]
	}))

	// Object.getOwnPropertySymbols(obj) (ES6): 返回 Symbol 自有键
	o.SetProperty("getOwnPropertySymbols", object.NewBuiltin("getOwnPropertySymbols", func(args ...object.Value) object.Value {
		// RequireObjectCoercible: undefined/null → TypeError
		// (non-object-argument-invalid)。
		if len(args) == 0 {
			return object.NewTypeError("Object.getOwnPropertySymbols called on non-object")
		}
		if _, isUndef := args[0].(*object.Undefined); isUndef {
			return object.NewTypeError("Object.getOwnPropertySymbols called on non-object")
		}
		if _, isNull := args[0].(*object.Null); isNull {
			return object.NewTypeError("Object.getOwnPropertySymbols called on non-object")
		}
		result := []object.Value{}
		// proxy: ownKeys trap 分发 + 不变式校验 (T6), 按键类型过滤符号键。
		if p, ok := args[0].(*object.Proxy); ok {
			keys, errVal := proxyOwnKeysInvariant(p)
			if errVal != nil {
				return errVal
			}
			for _, k := range keys {
				if sym, isSym := k.(*object.Symbol); isSym {
					result = append(result, sym)
				}
			}
			return object.NewArray(result)
		}
		if obj, ok := args[0].(object.SymbolPropertyStore); ok {
			for _, sym := range obj.SymbolKeys() {
				result = append(result, sym)
			}
		}
		return object.NewArray(result)
	}))

	// Object.groupBy(items, callback) (ES2024): 按回调返回的键分组
	o.SetProperty("groupBy", object.NewBuiltin("groupBy", func(args ...object.Value) object.Value {
		return groupByImpl(args, false)
	}))

	return o
}

// objectIs 实现同值相等 (Object.is)。
func objectIs(a, b object.Value) bool {
	if a == nil || b == nil {
		return a == b
	}
	// 处理 NaN: Object.is(NaN, NaN) === true
	if aNum, ok := a.(*object.Number); ok {
		if bNum, ok := b.(*object.Number); ok {
			if isNaN(aNum.Value) && isNaN(bNum.Value) {
				return true
			}
			// 处理 +0 和 -0: Object.is(+0, -0) === false
			if aNum.Value == 0 && bNum.Value == 0 {
				return signbit(aNum.Value) == signbit(bNum.Value)
			}
			return aNum.Value == bNum.Value
		}
	}
	return strictEqualValues(a, b)
}

// isNaN 检查是否为 NaN
func isNaN(f float64) bool {
	return f != f
}

// signbit 检查符号位
func signbit(f float64) bool {
	return f < 0 || (f == 0 && 1/f < 0)
}

// setOrThrow 实现 Set(O, P, V, true): 写失败 (只读数据属性/访问器无 setter
// 拒绝的不可扩展新属性) → TypeError —— Object.assign 的规范落点
// (target-set-not-writable / target-is-frozen-* / target-is-sealed-* 族)。
func setOrThrow(target *object.Object, key string, val object.Value) object.Value {
	desc, exists := target.OwnDescriptor(key)
	if exists {
		if acc, isAcc := desc.Value.(*object.Accessor); isAcc {
			if acc.Setter != nil && object.IsCallable(acc.Setter) {
				object.CallFunction(acc.Setter, target, val)
				if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
					return ev
				}
			}
			return nil // 有访问器: setter 缺失/未定义按规范静默成功
		}
		if !desc.Writable {
			return object.NewTypeError("Cannot assign to read only property '%s' of object", key)
		}
		nd := desc
		nd.Value = val
		target.DefineOwn(key, nd)
		return nil
	}
	if !target.Extensible {
		return object.NewTypeError("Cannot add property '%s', object is not extensible", key)
	}
	target.DefineOwn(key, object.PropertyDescriptor{Value: val, Writable: true, Enumerable: true, Configurable: true})
	return nil
}

// sameProto 比较 [[Prototype]] 槽位 (nil 与 nil 算同值)。
func sameProto(a, b object.Value) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a == b
}

// ownKeysArg 解析 Object.keys/values/entries 的参数并返回键列表。
// 参数为 undefined/null 时抛 TypeError (规范要求)。
func ownKeysArg(args []object.Value, api string) ([]string, object.Value) {
	if len(args) == 0 {
		return nil, object.NewTypeError("%s: undefined cannot be converted to an object", api)
	}
	return ownKeys(args[0])
}

// proxyOwnKeysInvariant 实现 Proxy [[OwnPropertyKeys]] (10.5.8):
// trap 分发 + 规范不变式校验 (keys/names/symbols 的 proxy-invariant-* 族):
//   - trap 结果必须是数组, 元素必须是 string/symbol, 不得重复;
//   - target 每个"不可配置"的自有键 (字符串与符号) 都必须出现在 trap 结果里;
//   - target 不可扩展时, trap 结果必须与 target 自有键集合完全一致。
//
// 无 trap → 转发 target 的 OwnPropertyStore。
func proxyOwnKeysInvariant(p *object.Proxy) ([]object.Value, object.Value) {
	if p.IsRevoked {
		return nil, object.NewTypeError("Cannot perform 'ownKeys' on a proxy that has been revoked")
	}
	type keyInfo struct {
		val          object.Value
		configurable bool
	}
	targetKeys := map[string]keyInfo{}
	targetSyms := map[*object.Symbol]keyInfo{}
	var targetStore object.OwnPropertyStore
	if store, ok := p.Target.(object.OwnPropertyStore); ok {
		targetStore = store
	} else if wrapped, ok := toObjectValue(p.Target).(object.OwnPropertyStore); ok {
		targetStore = wrapped
	}
	if targetStore != nil {
		for _, k := range targetStore.OwnKeys() {
			d, found := targetStore.OwnDescriptor(k)
			targetKeys[k] = keyInfo{val: object.NewString(k), configurable: found && d.Configurable}
		}
	}
	if sp, ok := p.Target.(object.SymbolPropertyStore); ok {
		for _, sym := range sp.SymbolKeys() {
			d, found := sp.GetSymbolPropertyDescriptor(sym)
			targetSyms[sym] = keyInfo{val: sym, configurable: found && d.Configurable}
		}
	}
	res, handled, errVal := proxyTrapDispatch(p, "ownKeys")
	if errVal != nil {
		return nil, errVal
	}
	if !handled {
		// target 仍是 proxy → 递归深入内层 ownKeys (trap-is-missing/
		// null/undefined-target-is-proxy 族)。
		if tp, isProxy := p.Target.(*object.Proxy); isProxy {
			return proxyOwnKeysInvariant(tp)
		}
		var out []object.Value
		var fwdStore object.OwnPropertyStore
		if store, ok := p.Target.(object.OwnPropertyStore); ok {
			fwdStore = store
		} else if wrapped, ok := toObjectValue(p.Target).(object.OwnPropertyStore); ok {
			fwdStore = wrapped
		}
		if fwdStore != nil {
			for _, k := range fwdStore.OwnKeys() {
				out = append(out, object.NewString(k))
			}
		}
		if sp, ok := p.Target.(object.SymbolPropertyStore); ok {
			for _, sym := range sp.SymbolKeys() {
				out = append(out, sym)
			}
		}
		if out == nil {
			out = []object.Value{}
		}
		return out, nil
	}
	arr, ok := res.(*object.Array)
	if !ok {
		return nil, object.NewTypeError("'ownKeys' trap must return an array")
	}
	seen := map[string]bool{}
	seenSym := map[*object.Symbol]bool{}
	result := make([]object.Value, 0, len(arr.Elements))
	for _, el := range arr.Elements {
		switch k := el.(type) {
		case *object.String:
			if seen[k.Value] {
				return nil, object.NewTypeError("'ownKeys' trap returned duplicate entries")
			}
			seen[k.Value] = true
			result = append(result, k)
		case *object.Symbol:
			if seenSym[k] {
				return nil, object.NewTypeError("'ownKeys' trap returned duplicate entries")
			}
			seenSym[k] = true
			result = append(result, k)
		default:
			return nil, object.NewTypeError("'ownKeys' trap must return string or symbol entries")
		}
	}
	// 不变式 1: 不可配置 target 键必须出现。
	for _, info := range targetKeys {
		if !info.configurable {
			s := toStr(info.val)
			if !seen[s] {
				return nil, object.NewTypeError("'ownKeys' trap must include the non-configurable target key '%s'", s)
			}
		}
	}
	for _, info := range targetSyms {
		if !info.configurable && !seenSym[info.val.(*object.Symbol)] {
			return nil, object.NewTypeError("'ownKeys' trap must include the non-configurable target symbol key")
		}
	}
	// 不变式 2: target 不可扩展 → 结果与 target 键集合一致。
	if !object.IsExtensible(p.Target) {
		if len(result) != len(targetKeys)+len(targetSyms) {
			return nil, object.NewTypeError("'ownKeys' trap returned keys that do not match the non-extensible target")
		}
		for _, info := range targetKeys {
			if !seen[toStr(info.val)] {
				return nil, object.NewTypeError("'ownKeys' trap returned keys that do not match the non-extensible target")
			}
		}
		for _, info := range targetSyms {
			if !seenSym[info.val.(*object.Symbol)] {
				return nil, object.NewTypeError("'ownKeys' trap returned keys that do not match the non-extensible target")
			}
		}
	}
	return result, nil
}

// ownKeys 返回值的自有可枚举键列表，顺序遵循 OrdinaryOwnPropertyKeys。
// 支持普通对象、数组、函数 (Closure/BuiltinFunction/BuiltinMethod) 与字符串
// (字符串按 UTF-16 码元索引展开)。
// 对象型一律优先走 OwnPropertyStore 统一接口 —— 与 defineProperty /
// getOwnPropertyDescriptor / hasOwnProperty 同一收口点, 不再按类型穷举。
func ownKeys(v object.Value) ([]string, object.Value) {
	switch v.(type) {
	case *object.Undefined, *object.Null:
		return nil, object.NewTypeError("Cannot convert undefined or null to object")
	case *object.String:
		// 规范: Object.keys("ab") === ["0", "1"]
		s := v.(*object.String)
		n := object.UTF16Len(s.Value)
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = strconv.Itoa(i)
		}
		return keys, nil
	}
	// proxy: ownKeys trap 分发 + 不变式校验; Object.keys 语义是"自有键 ∩
	// 可枚举", 故经 [[GetOwnProperty]] (proxyGetOwnPropertyDescriptor)
	// 逐键过滤 (keys/proxy-non-enumerable-prop-invariant-* 族)。
	if p, ok := v.(*object.Proxy); ok {
		keys, errVal := proxyOwnKeysInvariant(p)
		if errVal != nil {
			return nil, errVal
		}
		var out []string
		for _, k := range keys {
			if _, isSym := k.(*object.Symbol); isSym {
				continue
			}
			descVal := proxyGetOwnPropertyDescriptor(p, k)
			if descVal == nil || descVal == object.UndefinedSingleton || !object.IsObjectValue(descVal) {
				continue
			}
			if resDescBool(descVal, "enumerable") {
				out = append(out, toStr(k))
			}
		}
		return out, nil
	}
	// 统一接口: 普通对象 / 数组 / 函数类 / globalThis 都实现 OwnPropertyStore。
	if store, ok := v.(object.OwnPropertyStore); ok {
		return store.EnumerableOwnKeys(), nil
	}
	return nil, nil
}

// getOwnProperty 取值的自有属性，未找到时返回 undefined。
// 只查自有 (不沿原型链): 数组的索引/length/defineProperty 描述符、
// globalThis 的非词法绑定、函数的 Props/描述符、字符串的码元索引。
func getOwnProperty(v object.Value, key string) (object.Value, bool) {
	switch val := v.(type) {
	case *object.GlobalObject:
		if _, ok := val.OwnDescriptor(key); !ok {
			return object.UndefinedSingleton, false
		}
		got, found := val.GetProperty(key)
		if !found || got == nil {
			return object.UndefinedSingleton, true
		}
		return got, true
	case *object.Object:
		if desc, ok := val.Properties[key]; ok {
			if desc.Value == nil {
				return object.UndefinedSingleton, true
			}
			return desc.Value, true
		}
		return object.UndefinedSingleton, false
	case *object.Array:
		// defineProperty 定义过的描述符优先: 访问器调 getter,
		// 数据属性取描述符值 (此时 Elements 同步占位, 不能直接读)。
		if d, ok := val.PropDescs[key]; ok {
			if acc, isAcc := d.Value.(*object.Accessor); isAcc {
				if acc.Getter != nil && object.IsCallable(acc.Getter) {
					return object.CallFunction(acc.Getter, val), true
				}
				return object.UndefinedSingleton, true
			}
			if d.Value == nil {
				return object.UndefinedSingleton, true
			}
			return d.Value, true
		}
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(val.Elements) {
			if e := val.Elements[i]; e != nil {
				return e, true
			}
			return object.UndefinedSingleton, true
		}
		return object.UndefinedSingleton, false
	case *object.String:
		if i, err := strconv.Atoi(key); err == nil {
			if ch, ok := object.CharAtUTF16(val.Value, i); ok {
				return object.NewString(ch), true
			}
		}
		return object.UndefinedSingleton, false
	}
	// 函数类 (Closure/BuiltinFunction/BuiltinMethod): 有自有描述符即取值,
	// 访问器描述符调 getter。取不到按未找到处理 (不暴露 undefined 自有)。
	if store, ok := v.(object.OwnPropertyStore); ok {
		if _, has := store.OwnDescriptor(key); has {
			got, found := v.(interface {
				GetProperty(string) (object.Value, bool)
			}).GetProperty(key)
			if !found || got == nil {
				return object.UndefinedSingleton, true
			}
			return got, true
		}
	}
	return object.UndefinedSingleton, false
}

// setupErrorTypes 设置 Error 构造器到全局环境。
// toStringViaPrimitive 对对象做规范的 ToPrimitive(string hint) → ToString:
// 先 toString 后 valueOf, 钩子抛错原样传播, 结果为 Symbol 抛 TypeError。
func toStringViaPrimitive(v object.Value) (string, object.Value) {
	for _, m := range []string{"toString", "valueOf"} {
		fn, ok := v.GetProperty(m)
		if !ok || !object.IsCallable(fn) {
			continue
		}
		r := object.CallFunction(fn, v)
		if t := callbackThrown(); t != nil {
			return "", t
		}
		if r != nil && !isObjectLikeValue(r) {
			if _, isSym := r.(*object.Symbol); isSym {
				return "", object.NewTypeError("Cannot convert a Symbol value to a string")
			}
			return object.ToString(r), nil
		}
	}
	return "", object.NewTypeError("Cannot convert object to primitive value")
}

// isObjectLikeValue 判定 toString 的 this 是否对象 (含包装对象/错误值)。
func isObjectLikeValue(v object.Value) bool {
	switch v.(type) {
	case *object.Object, *object.Array, *object.Error, *object.PrimitiveObject,
		*object.GlobalObject, *object.Proxy, *object.Date, *object.Closure:
		return true
	}
	return false
}

// stdlibErrorProto 是 Error.prototype 对象, 供后置装配的错误子类型
// (AggregateError) 挂原型链 (setupErrorTypes 先于 setupEvalAndMisc 执行)。
var stdlibErrorProto *object.Object

func setupErrorTypes(env *runtime.Environment) {
	// Error 构造器
	env.Declare("Error", func() *object.BuiltinFunction {
		f := object.NewBuiltin("Error", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return object.NewError(msg)
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("Error", f)
		// Error.isError(arg) (ES2025): 品牌校验 —— 实际是 *Error 才为 true,
		// instanceof 式判定无法跨构造器伪装 (T4 r8DmKW: Error/isError 族)。
		f.SetProperty("isError", object.NewBuiltin("isError", func(args ...object.Value) object.Value {
			if len(args) == 0 {
				return object.FalseSingleton
			}
			_, ok := args[0].(*object.Error)
			return object.NewBoolean(ok)
		}))
		return f
	}(), false)

	env.Declare("TypeError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("TypeError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "TypeError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("TypeError", f)
		return f
	}(), false)

	env.Declare("RangeError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("RangeError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "RangeError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("RangeError", f)
		return f
	}(), false)

	env.Declare("ReferenceError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("ReferenceError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "ReferenceError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("ReferenceError", f)
		return f
	}(), false)

	env.Declare("SyntaxError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("SyntaxError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "SyntaxError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("SyntaxError", f)
		return f
	}(), false)

	// EvalError (规范 19.5.6.1): 此前构造器从未注册 (typeof EvalError ===
	// 'undefined'), NativeErrors/EvalError 族 46 条全灭 —— 而 length 表
	// (builtin_lengths.go "EvalError": 1) 早已登记, 属注册遗漏而非未实现。
	// 其余 Error 子类 (TypeError/RangeError/…) 均走 RegisterErrorConstructor,
	// EvalError 对齐同一模型, 保证 is-a-constructor / instance-proto /
	// proto-from-ctor-realm 等反射口径一致。
	env.Declare("EvalError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("EvalError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "EvalError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("EvalError", f)
		return f
	}(), false)

	// SuppressedError (ES2023 explicit resource management): 释放资源时又抛错、
	// 且此前已有挂起异常时的合成错误 (error = 后发生的, suppressed = 被压制的)。
	env.Declare("SuppressedError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("SuppressedError", func(args ...object.Value) object.Value {
			var errV object.Value = object.UndefinedSingleton
			var suppressedV object.Value = object.UndefinedSingleton
			msg := ""
			if len(args) > 0 {
				errV = args[0]
			}
			if len(args) > 1 {
				suppressedV = args[1]
			}
			if len(args) > 2 {
				msg = toStr(args[2])
			}
			e := object.NewSuppressedError(errV, suppressedV)
			if msg != "" {
				e.Message = msg
			}
			return e
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("SuppressedError", f)
		return f
	}(), false)

	// URIError: 此前完全缺失, 用例里 `new URIError()` 会退化成 ReferenceError
	// (test262 dynamic-import eval-rqstd-abrupt-err-uri 断言 error.name)。
	env.Declare("URIError", func() *object.BuiltinFunction {
		f := object.NewBuiltin("URIError", func(args ...object.Value) object.Value {
			msg := ""
			if len(args) > 0 {
				msg = toStr(args[0])
			}
			return &object.Error{Message: msg, Name: "URIError"}
		})
		f.ReturnIsValue = true
		object.RegisterErrorConstructor("URIError", f)
		return f
	}(), false)

	// ===== Error.prototype 原型链 (规范 19.5.3, rFXXbq batch26) =====
	// 此前 Error.prototype 是 undefined, gOPD(Error.prototype, 'name') /
	// verifyProperty(Error.prototype, ...) 一族 (~107 例) 全数失败。
	// 每个错误构造器挂一个普通对象作 prototype: name (各型专名) /
	// message ("") / constructor 回引 / toString; NativeError.prototype
	// 的 [[Prototype]] 是 Error.prototype, Error.prototype 的是
	// Object.prototype。
	op := object.GetObjectPrototype()
	errorProto := object.NewObject()
	errorProto.Proto = op
	errorProto.SetBuiltinProperty("name", object.NewString("Error"))
	errorProto.SetBuiltinProperty("message", object.NewString(""))
	errorProto.SetBuiltinProperty("toString", object.NewBuiltinMethod("toString", func(this object.Value, args ...object.Value) object.Value {
		// 规范 Error.prototype.toString 第 1 步: O 不是对象抛 TypeError
		// (invalid-receiver 族: undefined/null/number/string/Symbol)。
		if !isObjectLikeValue(this) {
			return object.NewTypeError("Function.prototype.toString: 'this' is not an Object")
		}
		name := "Error"
		if n, ok := this.GetProperty("name"); ok && n != object.UndefinedSingleton && n != nil {
			if thrown := object.TakeThrownValue(); thrown != object.UndefinedSingleton {
				return thrown // getter 抛错原样传播 (tostring-message-throws-*)
			}
			name = toStr(n)
		}
		msg := ""
		if mv, ok := this.GetProperty("message"); ok && mv != object.UndefinedSingleton && mv != nil {
			if thrown := object.TakeThrownValue(); thrown != object.UndefinedSingleton {
				return thrown // getter 抛错原样传播
			}
			if _, isSym := mv.(*object.Symbol); isSym {
				// ToString(Symbol) 是 TypeError (tostring-message-throws-symbol)
				return object.NewTypeError("Cannot convert a Symbol value to a string")
			}
			if obj, isObj := mv.(*object.Object); isObj {
				// 对象 message: ToString 先走 ToPrimitive(string hint),
				// 钩子抛错原样传播 (tostring-message-throws-toprimitive)
				s, errV := toStringViaPrimitive(obj)
				if errV != nil {
					return errV
				}
				msg = s
			} else {
				msg = toStr(mv)
			}
		}
		// 规范: name 为空串 → 直接返回 msg; msg 为空串 → 返回 name
		if name == "" {
			return object.NewString(msg)
		}
		if msg == "" {
			return object.NewString(name)
		}
		return object.NewString(name + ": " + msg)
	}))
	attachPrototype(env, "Error", errorProto)
	stdlibErrorProto = errorProto

	nativeNames := []string{"TypeError", "RangeError", "ReferenceError", "SyntaxError", "URIError", "SuppressedError", "EvalError"}
	for _, n := range nativeNames {
		np := object.NewObject()
		np.Proto = errorProto
		np.SetBuiltinProperty("name", object.NewString(n))
		np.SetBuiltinProperty("message", object.NewString(""))
		if tv, ok := errorProto.GetProperty("toString"); ok {
			np.SetBuiltinProperty("toString", tv)
		}
		attachPrototype(env, n, np)
		// 规范 19.5.6.2: NativeError 构造器自身的 [[Prototype]] 是 %Error%
		// (不是 %Function.prototype%)。此前回落默认 Function.prototype,
		// NativeErrors/*/proto.js 与 15.2.3.2-2-12..17 一族直接断言
		// Object.getPrototypeOf(TypeError) === Error, 全数失败 (B8 rm4Zcc)。
		if errCtor, ok := env.Get("Error"); ok {
			if eb, isB := errCtor.(*object.BuiltinFunction); isB {
				if cv, ok2 := env.Get(n); ok2 {
					if cb, isB2 := cv.(*object.BuiltinFunction); isB2 {
						cb.FuncPrototype = eb
					}
				}
			}
		}
	}
}

// newParseIntBuiltin 构造 parseInt 内建函数。
// 全局 parseInt 与 Number.parseInt 共用同一份实现，避免两处语义漂移。
func newParseIntBuiltin() *object.BuiltinFunction {
	return object.NewBuiltin("parseInt", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewNumber(math.NaN())
		}
		radix := 0 // 0 表示调用方未指定基数
		if len(args) > 1 {
			radix = int(toInt(args[1]))
		}
		result, ok := parseIntString(toStr(args[0]), radix)
		if !ok {
			return object.NewNumber(math.NaN())
		}
		return object.NewNumber(float64(result))
	})
}

// newParseFloatBuiltin 构造 parseFloat 内建函数。
// 全局 parseFloat 与 Number.parseFloat 共用同一份实现。
func newParseFloatBuiltin() *object.BuiltinFunction {
	return object.NewBuiltin("parseFloat", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewNumber(math.NaN())
		}
		f, ok := parseFloatString(toStr(args[0]))
		if !ok {
			return object.NewNumber(math.NaN())
		}
		return object.NewNumber(f)
	})
}

// setupNumberFunctions 设置全局数字相关函数。
func setupNumberFunctions(env *runtime.Environment) {
	// 规范 sec-number.parseint/parsefloat: Number.parseInt / Number.parseFloat
	// 与全局 parseInt / parseFloat 是**同一函数对象**。此前两处各自 new,
	// Number/parseInt.js 与 Number/parseFloat.js 断言 === 失败 (B8 rm4Zcc)。
	globalParseInt := newParseIntBuiltin()
	globalParseFloat := newParseFloatBuiltin()
	env.Declare("parseInt", globalParseInt, false)
	env.Declare("parseFloat", globalParseFloat, false)
	if nv, ok := env.Get("Number"); ok {
		nv.SetProperty("parseInt", globalParseInt)
		nv.SetProperty("parseFloat", globalParseFloat)
	}

	env.Declare("isNaN", object.NewBuiltin("isNaN", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(true)
		}
		v := toFloat(args[0])
		return object.NewBoolean(v != v)
	}), false)

	env.Declare("isFinite", object.NewBuiltin("isFinite", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(false)
		}
		v := toFloat(args[0])
		// NaN 与 ±Infinity 都不是有限数
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(true)
	}), false)
}

// setupGlobalFunctions 设置全局函数。
//
// 这里是 String / Number / Boolean 三个构造器的**唯一注册点**。
// 过去 stdlib.go 会先用 setupStringGlobal/setupNumberGlobal/setupBooleanGlobal
// 注册一次，这里再注册一次，而 Environment.Declare 是覆盖写，导致前者的
// 静态成员 (Number.MAX_VALUE、Number.parseInt 等) 全部丢失。
func setupGlobalFunctions(env *runtime.Environment) {
	// ===== String() 构造器 =====
	strFn := object.NewBuiltin("String", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewString("")
		}
		return object.NewString(toStr(args[0]))
	})
	strFn.SetProperty("name", object.NewString("String"))
	// String.fromCharCode(...codes) — 规范: 每个码位按 ToUint16 截断
	strFn.SetProperty("fromCharCode", object.NewBuiltin("fromCharCode", func(args ...object.Value) object.Value {
		var b strings.Builder
		for _, arg := range args {
			code := int(toInt(arg)) & 0xFFFF
			b.WriteRune(rune(code))
		}
		return object.NewString(b.String())
	}))
	// String.fromCodePoint(...codePoints) — 越界码位抛 RangeError
	strFn.SetProperty("fromCodePoint", object.NewBuiltin("fromCodePoint", func(args ...object.Value) object.Value {
		var b strings.Builder
		for _, arg := range args {
			cp := toInt(arg)
			if cp < 0 || cp > 0x10FFFF {
				return object.NewRangeError("Invalid code point %d", cp)
			}
			b.WriteRune(rune(cp))
		}
		return object.NewString(b.String())
	}))
	// String.raw(template, ...substitutions)
	strFn.SetProperty("raw", object.NewBuiltin("raw", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewString("")
		}
		tpl, ok := args[0].(*object.Object)
		if !ok {
			return object.NewString("")
		}
		rawVal, found := tpl.GetProperty("raw")
		if !found {
			return object.NewString("")
		}
		rawArr, ok := rawVal.(*object.Array)
		if !ok {
			return object.NewString("")
		}
		var b strings.Builder
		for i, e := range rawArr.Elements {
			b.WriteString(toStr(e))
			if i < len(args)-1 {
				b.WriteString(toStr(args[i+1]))
			}
		}
		return object.NewString(b.String())
	}))
	env.Declare("String", strFn, false)

	// ===== Number() 构造器 =====
	numFn := object.NewBuiltin("Number", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewNumber(0)
		}
		// 规范 sec-number-constructor: ToNumeric 语义 —— Number(BigInt) 是
		// 唯一允许 BigInt 的数值转换 (𝔽(ℝ(value))), 例如 Number(2n) === 2。
		// 不能走 toFloat (它把 BigInt 当未知类型返回 NaN), 否则 test262
		// 的 ToNumbers/Convert 等 harness 辅助把 BigInt 元素读成 NaN。
		if bi, ok := object.UnwrapPrimitive(args[0]).(*object.BigInt); ok {
			return object.NewNumber(bi.ToFloat64())
		}
		return object.NewNumber(toFloat(args[0]))
	})
	numFn.SetProperty("name", object.NewString("Number"))
	numFn.DefineOwn("MAX_SAFE_INTEGER", object.PropertyDescriptor{Value: object.NewNumber(9007199254740991), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("MIN_SAFE_INTEGER", object.PropertyDescriptor{Value: object.NewNumber(-9007199254740991), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("MAX_VALUE", object.PropertyDescriptor{Value: object.NewNumber(math.MaxFloat64), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("MIN_VALUE", object.PropertyDescriptor{Value: object.NewNumber(5e-324), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("POSITIVE_INFINITY", object.PropertyDescriptor{Value: object.NewNumber(math.Inf(1)), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("NEGATIVE_INFINITY", object.PropertyDescriptor{Value: object.NewNumber(math.Inf(-1)), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("NaN", object.PropertyDescriptor{Value: object.NewNumber(math.NaN()), Writable: false, Enumerable: false, Configurable: false})
	numFn.DefineOwn("EPSILON", object.PropertyDescriptor{Value: object.NewNumber(2.220446049250313e-16), Writable: false, Enumerable: false, Configurable: false})
	// 以下 isXxx 方法遵循规范: 非 Number 类型的参数一律返回 false
	numFn.SetProperty("isInteger", object.NewBuiltin("isInteger", func(args ...object.Value) object.Value {
		num, ok := args[0].(*object.Number)
		if !ok || len(args) == 0 {
			return object.NewBoolean(false)
		}
		if math.IsNaN(num.Value) || math.IsInf(num.Value, 0) {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(num.Value == math.Trunc(num.Value))
	}))
	numFn.SetProperty("isSafeInteger", object.NewBuiltin("isSafeInteger", func(args ...object.Value) object.Value {
		num, ok := args[0].(*object.Number)
		if !ok || len(args) == 0 {
			return object.NewBoolean(false)
		}
		if math.IsNaN(num.Value) || math.IsInf(num.Value, 0) {
			return object.NewBoolean(false)
		}
		if num.Value != math.Trunc(num.Value) {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(math.Abs(num.Value) <= 9007199254740991)
	}))
	numFn.SetProperty("isFinite", object.NewBuiltin("isFinite", func(args ...object.Value) object.Value {
		num, ok := args[0].(*object.Number)
		if !ok || len(args) == 0 {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(!math.IsNaN(num.Value) && !math.IsInf(num.Value, 0))
	}))
	numFn.SetProperty("isNaN", object.NewBuiltin("isNaN", func(args ...object.Value) object.Value {
		num, ok := args[0].(*object.Number)
		if !ok || len(args) == 0 {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(math.IsNaN(num.Value))
	}))
	// Number.parseInt / Number.parseFloat: 由 setupNumberFunctions 用与全局
	// 同一函数对象补挂 (规范要求同一性, 见该函数 B8 rm4Zcc 注释)。
	env.Declare("Number", numFn, false)

	// ===== Boolean() 构造器 =====
	boolFn := object.NewBuiltin("Boolean", func(args ...object.Value) object.Value {
		if len(args) == 0 {
			return object.NewBoolean(false)
		}
		return object.NewBoolean(toBool(args[0]))
	})
	boolFn.SetProperty("name", object.NewString("Boolean"))
	// Boolean.prototype: Boolean 原始值没有自有属性，属性访问 (含
	// @@toStringTag) 一律沿它查找。规范里它**没有** @@toStringTag，
	// 因此 toString.call(true) 的 "Boolean" 标签来自 builtinTag。
	boolProto := object.NewObjectWithProto(objectPrototype)
	boolProto.SetBuiltinProperty("constructor", boolFn)
	// Boolean.prototype.toString / valueOf (ES2024 20.3.3.2 / 20.3.3.3)：
	// 此前原型上只有 constructor，`true.toString()` 因此取不到方法。
	boolProto.SetBuiltinProperty("toString", object.NewBuiltinMethod("toString", func(this object.Value, args ...object.Value) object.Value {
		if b, ok := object.UnwrapPrimitive(this).(*object.Boolean); ok {
			if b.Value {
				return object.NewString("true")
			}
			return object.NewString("false")
		}
		return object.NewTypeError("Boolean.prototype.toString requires that 'this' be a Boolean")
	}))
	boolProto.SetBuiltinProperty("valueOf", object.NewBuiltinMethod("valueOf", func(this object.Value, args ...object.Value) object.Value {
		if b, ok := object.UnwrapPrimitive(this).(*object.Boolean); ok {
			return b
		}
		return object.NewTypeError("Boolean.prototype.valueOf requires that 'this' be a Boolean")
	}))
	setCtorPrototype(boolFn, boolProto)
	object.SetBooleanProto(boolProto)
	env.Declare("Boolean", boolFn, false)

	// Function() 构造器已移至 setupFunctionIntrinsics (stdlib/function_proto.go)，
	// 因为它要参与函数对象 [[Prototype]] 链的早期装配。

	// NaN, Infinity, undefined 全局常量
	env.Declare("NaN", object.NewNumber(math.NaN()), true)
	env.Declare("Infinity", object.NewNumber(math.Inf(1)), true)
	env.Declare("undefined", object.UndefinedSingleton, true)
}

// dynamicFuncKind 是 CreateDynamicFunction 的函数种类。
type dynamicFuncKind int

const (
	dynFuncNormal         dynamicFuncKind = iota // Function
	dynFuncGenerator                             // GeneratorFunction
	dynFuncAsync                                 // AsyncFunction
	dynFuncAsyncGenerator                        // AsyncGeneratorFunction
)

// newDynamicFunction 实现 CreateDynamicFunction: Function / GeneratorFunction /
// AsyncFunction / AsyncGeneratorFunction 共用的动态函数构造。
// 规范语义: 除最后一个参数外都是形参名，最后一个参数是函数体源码；
// 将 (形参, 函数体) 拼成对应种类的函数表达式源码，经 object.CompileSource 桥
// 编译 (vm 包注册实现)，取顶层包装函数构建闭包。
//
// 闭包的 Env 是全局环境 (动态函数只访问全局作用域)；[[Prototype]] 按编译出的
// 函数种类装配 (FuncPrototypeForCompiled)，故造出的 GeneratorFunction 实例
// 继承 %GeneratorFunction.prototype%、其 .prototype 的 [[Prototype]] 指向
// %GeneratorPrototype% —— 与静态声明完全一致。
func newDynamicFunction(env *runtime.Environment, args []object.Value, kind dynamicFuncKind) object.Value {
	params := make([]string, 0, len(args))
	body := ""
	for i, a := range args {
		if i < len(args)-1 {
			params = append(params, toStr(a))
		} else {
			body = toStr(a)
		}
	}
	// 基本合法性检查: 形参不能含 ) { 等破坏结构的内容 (node 实测此类为 SyntaxError)。
	// 逗号**不在**此列 —— 它是合法的形参分隔符 (Function('a, b') 就是两个形参),
	// 误拒会让 Function/GeneratorFunction/AsyncGeneratorFunction 的多形参形式挂。
	for _, p := range params {
		for _, ch := range p {
			if ch == ')' || ch == '{' || ch == '}' || ch == '[' || ch == ']' {
				return object.NewErrorWithName("SyntaxError", fmt.Sprintf("Unexpected character %q in argument list", string(ch)))
			}
		}
	}
	var head string
	switch kind {
	case dynFuncGenerator:
		head = "function* anonymous"
	case dynFuncAsync:
		head = "async function anonymous"
	case dynFuncAsyncGenerator:
		head = "async function* anonymous"
	default:
		head = "function anonymous"
	}
	// 规范 CreateDynamicFunction 的合成源码: 参数表与 ')' 之间有换行 ——
	// 末位形参里的行注释 (//) 不能吞掉 ')', 且 toString 的 [[SourceText]]
	// 必须是**不带外层括号**的 "function anonymous(...) {\n<body>\n}"。
	// (test262 Function/prototype/toString/Function.js 精确断言该串。)
	paramText := strings.Join(params, ",")
	src := "(" + head + "(" + paramText + "\n) {\n" + body + "\n})"
	srcText := head + "(" + paramText + "\n) {\n" + body + "\n}"

	fn, err := object.CompileSource(src)
	if err != nil {
		return object.NewErrorWithName("SyntaxError", err.Error())
	}
	fn.SourceText = srcText
	return &object.Closure{
		Fn:            fn,
		Env:           env,
		FuncPrototype: object.FuncPrototypeForCompiled(fn),
	}
}

// proxyTrapDispatch 实现 proxy trap 的 GetMethod 分发 (T5 r8dXes):
//
//	非 proxy          → (nil, false, nil): 调用方走原有路径;
//	revoked           → TypeError;
//	trap 存在不可调用 → TypeError "'%s' trap must be callable";
//	trap 不存在       → (nil, false, nil): 调用方转发 target;
//	trap 可调用       → 调用 trap (this 绑 handler), 异常经回调桥重抛。
//
// 只做 GetMethod + 分发, 不做不变式校验 (invariant 族单列)。
func proxyTrapDispatch(v object.Value, trapName string, args ...object.Value) (object.Value, bool, object.Value) {
	for {
		p, ok := v.(*object.Proxy)
		if !ok {
			return nil, false, nil
		}
		if p.IsRevoked {
			return nil, true, object.NewTypeError("Cannot perform '%s' on a proxy that has been revoked", trapName)
		}
		trap, present := object.GetProxyTrapMethod(p.Handler, trapName)
		if !present {
			// trap 缺失且 target 仍是 proxy → 逐层深入内层 handler
			// (trap-is-null/undefined/missing-target-is-proxy 族)。
			if tp, isProxy := p.Target.(*object.Proxy); isProxy {
				v = tp
				continue
			}
			return nil, false, nil
		}
		if !object.IsCallable(trap) {
			return nil, true, object.NewTypeError("'%s' trap must be callable", trapName)
		}
		trapArgs := make([]object.Value, 0, len(args)+1)
		trapArgs = append(trapArgs, p.Target)
		trapArgs = append(trapArgs, args...)
		res := object.CallFunction(trap, p.Handler, trapArgs...)
		if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
			return nil, true, ev
		}
		return res, true, nil
	}
}

// targetProtoOf 取任意值的实际 [[Prototype]] (支持嵌套 proxy: 无 trap 时沿
// target 转发, 有 trap 时结果必须是对象或 null)。返回 (proto, errVal)。
func targetProtoOf(v object.Value) (object.Value, object.Value) {
	for {
		p, ok := v.(*object.Proxy)
		if !ok {
			break
		}
		res, handled, errVal := proxyTrapDispatch(v, "getPrototypeOf")
		if errVal != nil {
			return nil, errVal
		}
		if !handled {
			v = p.Target
			continue
		}
		if res == nil || res == object.UndefinedSingleton || res == object.NullSingleton {
			return object.NullSingleton, nil
		}
		if !object.IsObjectLike(res) {
			return nil, object.NewTypeError("'getPrototypeOf' trap must return an object or null")
		}
		return res, nil
	}
	switch t := v.(type) {
	case *object.GlobalObject:
		if t.Proto != nil {
			return t.Proto, nil
		}
		return object.NullSingleton, nil
	case *object.Object:
		if t.Proto != nil {
			return t.Proto, nil
		}
		return object.NullSingleton, nil
	case *object.Array:
		if p := t.GetProto(); p != nil {
			return p, nil
		}
		return object.NullSingleton, nil
	}
	if gp, ok := v.(interface{ GetProto() object.Value }); ok {
		if p := gp.GetProto(); p != nil {
			return p, nil
		}
	}
	return object.NullSingleton, nil
}

// targetIsExtensibleOf 取任意值的实际 [[Extensible]] (嵌套 proxy 沿 trap
// 转发)。返回 (extensible, errVal)。
func targetIsExtensibleOf(v object.Value) (bool, object.Value) {
	for {
		p, ok := v.(*object.Proxy)
		if !ok {
			break
		}
		res, handled, errVal := proxyTrapDispatch(v, "isExtensible")
		if errVal != nil {
			return false, errVal
		}
		if !handled {
			v = p.Target
			continue
		}
		return object.IsTruthyValue(res), nil
	}
	return object.IsExtensible(v), nil
}

// ProxyEnumerableOwnKeys 计算 proxy 的 for-in 键集 (规范
// EnumerateObjectProperties 13.7.5.15 的等价实现): ownKeys trap (嵌套转发)
// 收集自有键, 每个键经 [[GetOwnProperty]] (gOPD trap/转发) 过滤 enumerable
// 位; 符号键被排除 (for-in 仅字符串键)。出错 (不变式 TypeError) 返回空集。
func ProxyEnumerableOwnKeys(v object.Value) []string {
	p, ok := v.(*object.Proxy)
	if !ok {
		return nil
	}
	keys, errVal := proxyOwnKeysInvariant(p)
	if errVal != nil {
		return []string{}
	}
	out := []string{}
	for _, k := range keys {
		s, isStr := k.(*object.String)
		if !isStr {
			continue // for-in 仅枚举字符串键
		}
		d := proxyGetOwnPropertyDescriptor(p, k)
		if e, isErr := d.(*object.Error); isErr {
			_ = e // 不变式错误按空集处理 (for-in 不抛)
			return []string{}
		}
		if d == nil || d == object.UndefinedSingleton {
			continue
		}
		if resDescBool(d, "enumerable") {
			out = append(out, s.Value)
		}
	}
	return out
}

// proxyGetOwnPropertyDescriptor 实现 Proxy [[GetOwnProperty]] (9.5.5):
// trap 分发 + 规范不变式校验。错误以 *Error 值返回 (builtin throw 语义)。
// GetMethod 语义: trap 为 undefined/null → 视同缺失, 转发 target;
// trap 存在且不可调用 → TypeError; target 仍是 proxy → 逐层深入
// (trap-is-null/undefined[-target-is-proxy] 族)。
func proxyGetOwnPropertyDescriptor(pxy *object.Proxy, prop object.Value) object.Value {
	for {
		if pxy.IsRevoked {
			return object.NewTypeError("Cannot perform 'getOwnPropertyDescriptor' on a proxy that has been revoked")
		}
		trapVal, found := pxy.Handler.GetProperty("getOwnPropertyDescriptor")
		if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
			return ev
		}
		trapMissing := !found || trapVal == nil ||
			trapVal == object.UndefinedSingleton || trapVal == object.NullSingleton
		// GetMethod: 属性存在但不可调用 (且非 undefined/null) → TypeError
		// (trap-is-not-callable)。
		if found && !trapMissing && !object.IsCallable(trapVal) {
			return object.NewTypeError("'getOwnPropertyDescriptor' trap must be callable")
		}
		// target 描述符 (供不变式校验)。原始值 target (String 包装等)
		// 先 ToObject: 索引字符与 length 是真实自有属性
		// (trap-is-missing/undefined-target-is-proxy 的 String 变体)。
		var targetDesc *object.PropertyDescriptor
		var targetStore object.OwnPropertyStore
		if store, ok := pxy.Target.(object.OwnPropertyStore); ok {
			targetStore = store
		} else if wrapped, ok := toObjectValue(pxy.Target).(object.OwnPropertyStore); ok {
			targetStore = wrapped
		}
		if targetStore != nil {
			if d, exists := targetStore.OwnDescriptor(toStr(prop)); exists {
				td := d
				targetDesc = &td
			}
		}
		if trapMissing {
			// 无 trap → 转发 target; target 仍是 proxy → 深入内层
			// (trap-is-undefined/null/missing-target-is-proxy 族)。
			if tp, isProxy := pxy.Target.(*object.Proxy); isProxy {
				pxy = tp
				continue
			}
			if targetDesc != nil {
				return describeProperty(*targetDesc)
			}
			return object.UndefinedSingleton
		}
		res := object.CallFunction(trapVal, pxy.Handler, pxy.Target, prop, pxy)
		if ev := object.TakeCallbackErrorValue(); ev != object.UndefinedSingleton {
			return ev
		}
		if res == nil || res == object.UndefinedSingleton {
			// trap 结果 undefined: target 该属性不可配置或 target 不可扩展
			// → TypeError。
			if targetDesc != nil && !targetDesc.Configurable {
				return object.NewTypeError("Proxy 'getOwnPropertyDescriptor': trap returned undefined for a non-configurable target property")
			}
			if _, isSym := prop.(*object.Symbol); !isSym && !object.IsExtensible(pxy.Target) {
				if _, exists := pxyTargetOwnKeysContains(pxy, toStr(prop)); exists {
					return object.NewTypeError("Proxy 'getOwnPropertyDescriptor': trap returned undefined for a property of a non-extensible target")
				}
			}
			return object.UndefinedSingleton
		}
		if !object.IsObjectValue(res) {
			return object.NewTypeError("Proxy 'getOwnPropertyDescriptor' trap must return an Object or undefined")
		}
		if !resDescBool(res, "configurable") {
			if targetDesc == nil || targetDesc.Configurable {
				return object.NewTypeError("Proxy 'getOwnPropertyDescriptor': trap reported non-configurable for a configurable (or absent) target property")
			}
		}
		if targetDesc == nil && !object.IsExtensible(pxy.Target) {
			return object.NewTypeError("Proxy 'getOwnPropertyDescriptor': trap reported a new property on a non-extensible target")
		}
		// 描述符字段直接透传 trap 结果 (value/get/set/enumerable/configurable)。
		return res
	}
}

// pxyTargetOwnKeysContains 判断 target 自有字符串键是否包含给定键。
func pxyTargetOwnKeysContains(pxy *object.Proxy, key string) (struct{}, bool) {
	if store, ok := pxy.Target.(object.OwnPropertyStore); ok {
		for _, k := range store.OwnKeys() {
			if k == key {
				return struct{}{}, true
			}
		}
	}
	return struct{}{}, false
}

// resDescBool 读取 trap 结果对象上的布尔描述符字段 (缺失按 false)。
func resDescBool(v object.Value, key string) bool {
	val, found := v.GetProperty(key)
	if !found || val == nil {
		return false
	}
	return val.IsTruthy()
}

// describeProperty 把内部 PropertyDescriptor 转成 JS 描述符对象
// (Object.getOwnPropertyDescriptor 的返回形态)。
func describeProperty(desc object.PropertyDescriptor) object.Value {
	// 必须是"普通对象" ([[Prototype]] = Object.prototype): test262 大量用例
	// 在 gOPD 结果上调 hasOwnProperty/toString 等原型方法
	// (NewObject 的 Proto 是 null, 会得到 "undefined is not a function")。
	result := object.NewPlainObject()
	result.SetProperty("configurable", object.NewBoolean(desc.Configurable))
	result.SetProperty("enumerable", object.NewBoolean(desc.Enumerable))
	if acc, isAcc := desc.Value.(*object.Accessor); isAcc {
		if acc.Getter != nil {
			result.SetProperty("get", acc.Getter)
		} else {
			result.SetProperty("get", object.UndefinedSingleton)
		}
		if acc.Setter != nil {
			result.SetProperty("set", acc.Setter)
		} else {
			result.SetProperty("set", object.UndefinedSingleton)
		}
		return result
	}
	result.SetProperty("value", desc.Value)
	result.SetProperty("writable", object.NewBoolean(desc.Writable))
	return result
}

// defineOneSymbolProperty 按 property descriptor 定义一个 Symbol 键自有属性。
// 与 defineOneProperty 同一套 ToPropertyDescriptor 语义, 但落点在持有者的符号
// 键存储 (*Object.SymbolProperties 或 *Array/*TypedArray 同构槽), 与字符串键
// 空间隔离。
// toPropertyKey 实现 ToPropertyKey: Symbol 直接 TypeError, 对象先
// ToPrimitive (toString/valueOf, 两者都不产出原始值 → TypeError,
// 15.2.3.6-2-47 族), 其余 ToString。
func toPropertyKey(v object.Value) (string, object.Value) {
	if _, isSym := v.(*object.Symbol); isSym {
		return "", object.NewTypeError("Cannot convert a Symbol value to a property key")
	}
	if object.IsObjectValue(v) {
		return toStringViaPrimitive(v)
	}
	return object.ToString(v), nil
}

// parsedDesc 是 ToPropertyDescriptor 的解析结果: 描述符 + 各字段是否
// 显式存在。ValidateAndApply 的合并与兼容性校验都需要"存在"标记 ——
// 裸 PropertyDescriptor 无法区分"显式 false"与"缺省"。
type parsedDesc struct {
	desc            object.PropertyDescriptor
	hasEnumerable   bool
	hasConfigurable bool
	hasValue        bool
	hasWritable     bool
	hasGet          bool
	hasSet          bool
	getter, setter  object.Value // 访问器字段原始值 (已过 callable 校验)
}

// isAccessor 报告描述符是否为访问器描述符 (有 get 或 set 字段)。
func (p *parsedDesc) isAccessor() bool { return p.hasGet || p.hasSet }

// taDescFromParsed 把解析后的描述符转成 TypedArray.DefineOwnIndexed 需要的
// 字段视图 (保留 has 标记, exotic [[DefineOwnProperty]] 用它区分部分描述符)。
func taDescFromParsed(p parsedDesc) object.TypedArrayDesc {
	return object.TypedArrayDesc{
		IsAccessor:      p.isAccessor(),
		HasWritable:     p.hasWritable,
		Writable:        p.desc.Writable,
		HasEnumerable:   p.hasEnumerable,
		Enumerable:      p.desc.Enumerable,
		HasConfigurable: p.hasConfigurable,
		Configurable:    p.desc.Configurable,
		HasValue:        p.hasValue,
		Value:           p.desc.Value,
	}
}

// toPropertyDescriptor 实现 ToPropertyDescriptor (6.2.6.1) 的完整校验:
//  1. 非对象 → TypeError;
//  2. enumerable/configurable → ToBoolean;
//  3. value/writable → 数据字段;
//  4. get/set → 访问器字段: undefined 合法 (空访问器), 不可调用 → TypeError;
//  5. 访问器字段与数据字段混用 → TypeError (step 9.a —— 15.2.3.6-3-x /
//     defineProperties -5-b-26x / create -4-30x 族的根因)。
func toPropertyDescriptor(descVal object.Value) (parsedDesc, object.Value) {
	if !object.IsObjectValue(descVal) {
		return parsedDesc{}, object.NewTypeError("Property description must be an object")
	}
	get := func(n string) (object.Value, bool) { return descVal.GetProperty(n) }
	p := parsedDesc{}
	if v, found := get("enumerable"); found {
		p.desc.Enumerable = toBool(v)
		p.hasEnumerable = true
	}
	if v, found := get("configurable"); found {
		p.desc.Configurable = toBool(v)
		p.hasConfigurable = true
	}
	if v, found := get("value"); found {
		p.desc.Value = v
		p.hasValue = true
	}
	if v, found := get("writable"); found {
		p.desc.Writable = toBool(v)
		p.hasWritable = true
	}
	if v, found := get("get"); found {
		p.hasGet = true
		p.getter = v
		if v != nil && !isUndefinedValue(v) && !object.IsCallable(v) {
			return p, object.NewTypeError("Getter must be a function")
		}
	}
	if v, found := get("set"); found {
		p.hasSet = true
		p.setter = v
		if v != nil && !isUndefinedValue(v) && !object.IsCallable(v) {
			return p, object.NewTypeError("Setter must be a function")
		}
	}
	if p.isAccessor() && (p.hasValue || p.hasWritable) {
		return p, object.NewTypeError("Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	}
	return p, nil
}

// accField 归一化访问器字段: nil/undefined 统一为 undefined 单例。
func accField(v object.Value) object.Value {
	if v == nil || isUndefinedValue(v) {
		return object.UndefinedSingleton
	}
	return v
}

// validateAndApplyDesc 实现 ValidateAndApplyPropertyDescriptor (6.2.6.3):
//   - 属性不存在: target 不可扩展 → TypeError; 否则按缺省值 (false/undefined)
//     落最终描述符 (规范 step 3);
//   - 属性存在且不可配置: 逐条兼容性校验 (step 2.a.i-vi), 任一违反 → TypeError;
//   - 合并 (step 3): Desc 缺省的字段保留 current 原值 —— 访问器部分重定义
//     不再丢掉另一个访问器 (此前 NewAccessor(nil, s) 的实现缺陷)。
func validateAndApplyDesc(target object.Value, store object.OwnPropertyStore, key string, parsed parsedDesc) (object.PropertyDescriptor, object.Value) {
	existing, exists := store.OwnDescriptor(key)
	reject := object.NewTypeError("Cannot redefine property: %s", key)
	if !exists {
		if !object.IsExtensible(target) {
			return object.PropertyDescriptor{}, object.NewTypeError("Cannot define property %s: object is not extensible", key)
		}
		final := object.PropertyDescriptor{}
		if parsed.isAccessor() {
			final.Value = object.NewAccessor(accField(parsed.getter), accField(parsed.setter))
			final.Writable = false
		} else {
			if parsed.hasValue {
				final.Value = parsed.desc.Value
			} else {
				final.Value = object.UndefinedSingleton
			}
			final.Writable = parsed.desc.Writable
		}
		final.Enumerable = parsed.desc.Enumerable
		final.Configurable = parsed.desc.Configurable
		return final, nil
	}
	current := existing
	curAcc, curIsAcc := current.Value.(*object.Accessor)
	if !current.Configurable {
		if parsed.hasConfigurable && parsed.desc.Configurable {
			return current, reject
		}
		if parsed.hasEnumerable && parsed.desc.Enumerable != current.Enumerable {
			return current, reject
		}
		descIsAccessor := parsed.isAccessor()
		descIsGeneric := !descIsAccessor && !parsed.hasValue && !parsed.hasWritable
		if !descIsGeneric && curIsAcc != descIsAccessor {
			return current, reject
		}
		if !curIsAcc {
			if !current.Writable {
				if parsed.hasWritable && parsed.desc.Writable {
					return current, reject
				}
				if parsed.hasValue && !objectIs(parsed.desc.Value, current.Value) {
					return current, reject
				}
			}
		} else {
			// current 是访问器: get/set 只在 Desc 显式给出时才比较。
			if parsed.hasGet && !objectIs(accField(parsed.getter), accField(curAcc.Getter)) {
				return current, reject
			}
			if parsed.hasSet && !objectIs(accField(parsed.setter), accField(curAcc.Setter)) {
				return current, reject
			}
		}
	}
	// step 3: 以 current 为底, Desc 显式字段覆盖。
	final := current
	if parsed.isAccessor() {
		var g, s object.Value
		if curIsAcc {
			g, s = accField(curAcc.Getter), accField(curAcc.Setter)
		} else {
			g, s = object.UndefinedSingleton, object.UndefinedSingleton
		}
		if parsed.hasGet {
			g = accField(parsed.getter)
		}
		if parsed.hasSet {
			s = accField(parsed.setter)
		}
		final.Value = object.NewAccessor(g, s)
		final.Writable = false
	} else {
		if parsed.hasValue {
			final.Value = parsed.desc.Value
		}
		if parsed.hasWritable {
			final.Writable = parsed.desc.Writable
		}
	}
	if parsed.hasEnumerable {
		final.Enumerable = parsed.desc.Enumerable
	}
	if parsed.hasConfigurable {
		final.Configurable = parsed.desc.Configurable
	}
	return final, nil
}

// defineOneSymbolProperty 在符号键槽上按描述符定义一个属性。与字符串键
// 同一套 ToPropertyDescriptor / ValidateAndApply 校验 (T6)。
func defineOneSymbolProperty(obj object.SymbolPropertyStore, sym *object.Symbol, descVal object.Value, target object.Value) object.Value {
	parsed, errVal := toPropertyDescriptor(descVal)
	if errVal != nil {
		return errVal
	}
	existing, exists := obj.GetSymbolPropertyDescriptor(sym)
	nd := existing
	if !exists {
		nd = object.PropertyDescriptor{}
		if !object.IsExtensible(target) {
			return object.NewTypeError("Cannot define property: object is not extensible")
		}
	}
	reject := object.NewTypeError("Cannot redefine property: %s", sym.Description)
	existingAcc, existingIsAcc := nd.Value.(*object.Accessor)
	parsedAcc := parsed.isAccessor()
	if exists && !nd.Configurable {
		if parsed.hasConfigurable && parsed.desc.Configurable {
			return reject
		}
		if parsed.hasEnumerable && parsed.desc.Enumerable != nd.Enumerable {
			return reject
		}
		descIsGeneric := !parsedAcc && !parsed.hasValue && !parsed.hasWritable
		if !descIsGeneric && existingIsAcc != parsedAcc {
			return reject
		}
		if !existingIsAcc {
			if !nd.Writable {
				if parsed.hasWritable && parsed.desc.Writable {
					return reject
				}
				if parsed.hasValue && !objectIs(parsed.desc.Value, nd.Value) {
					return reject
				}
			}
		} else {
			if parsed.hasGet && !objectIs(accField(parsed.getter), accField(existingAcc.Getter)) {
				return reject
			}
			if parsed.hasSet && !objectIs(accField(parsed.setter), accField(existingAcc.Setter)) {
				return reject
			}
		}
	}
	if parsed.isAccessor() {
		var g, s object.Value
		if existingIsAcc {
			g, s = accField(existingAcc.Getter), accField(existingAcc.Setter)
		} else {
			g, s = object.UndefinedSingleton, object.UndefinedSingleton
		}
		if parsed.hasGet {
			g = accField(parsed.getter)
		}
		if parsed.hasSet {
			s = accField(parsed.setter)
		}
		nd.Value = object.NewAccessor(g, s)
		nd.Writable = false
	} else {
		if parsed.hasValue {
			nd.Value = parsed.desc.Value
		} else if !exists {
			nd.Value = object.UndefinedSingleton
		}
		if parsed.hasWritable {
			nd.Writable = parsed.desc.Writable
		}
	}
	if parsed.hasEnumerable {
		nd.Enumerable = parsed.desc.Enumerable
	}
	if parsed.hasConfigurable {
		nd.Configurable = parsed.desc.Configurable
	}
	obj.DefineOwnSymbolProperty(sym, nd)
	return nil
}

// defineOneProperty 按 property descriptor 定义一个属性。
// 访问器描述符 (get/set) 注册为访问器；数据描述符 (value/writable)
// 注册为数据属性。供 defineProperty / defineProperties 共用。
// descriptorFromJS 把 JS 描述符对象解析为内部 PropertyDescriptor
// (Object.defineProperty(globalThis, ...) 与 Reflect.defineProperty 用)。
// 非对象描述符抛 TypeError; 字段校验与混用拒绝统一走 toPropertyDescriptor。
func descriptorFromJS(descVal object.Value) (object.PropertyDescriptor, object.Value) {
	parsed, errVal := toPropertyDescriptor(descVal)
	if errVal != nil {
		return object.PropertyDescriptor{}, errVal
	}
	return parsedToDesc(parsed), nil
}

// parsedToDesc 把解析结果落成内部 PropertyDescriptor: 访问器描述符把
// getter/setter 打包进 Value; 数据描述符缺省 value 补 undefined。
func parsedToDesc(parsed parsedDesc) object.PropertyDescriptor {
	desc := parsed.desc
	if parsed.isAccessor() {
		desc.Value = object.NewAccessor(accField(parsed.getter), accField(parsed.setter))
		desc.Writable = false
	} else if !parsed.hasValue {
		desc.Value = object.UndefinedSingleton
	}
	return desc
}

// proxyDefinePropertyInvariant 校验 [[DefineOwnProperty]] trap 报 true 后
// 的 target 兼容性不变式 (10.5.7 step 25-29, targetdesc-* 族):
//   - target 无该属性且不可扩展 → TypeError;
//   - target 属性存在: 对 targetDesc 跑 ValidateAndApply (不落地),
//     任一违反 (不可配置属性报 configurable true / 值或访问器字段不兼容)
//     → TypeError。
func proxyDefinePropertyInvariant(p object.Value, key object.Value, parsed parsedDesc) object.Value {
	pxy, ok := p.(*object.Proxy)
	if !ok || pxy.Target == nil {
		return p
	}
	if sym, isSym := key.(*object.Symbol); isSym {
		if sp, ok2 := pxy.Target.(object.SymbolPropertyStore); ok2 {
			if _, exists := sp.GetSymbolPropertyDescriptor(sym); !exists {
				if !object.IsExtensible(pxy.Target) {
					return object.NewTypeError("Proxy 'defineProperty': trap reported a new property on a non-extensible target")
				}
				return p
			}
			// 符号键 target 描述符兼容性: 与字符串键同一校验逻辑,
			// 经字符串化路径不可用 —— 这里只校验"不可配置 → 报 true"。
			if td, exists2 := sp.GetSymbolPropertyDescriptor(sym); exists2 && !td.Configurable {
				if parsed.hasConfigurable && parsed.desc.Configurable {
					return object.NewTypeError("Proxy 'defineProperty': trap reported configurable true for a non-configurable target property")
				}
			}
		}
		return p
	}
	keyStr := toStr(key)
	store, ok2 := pxy.Target.(object.OwnPropertyStore)
	if !ok2 {
		return p
	}
	if _, exists := store.OwnDescriptor(keyStr); !exists {
		if !object.IsExtensible(pxy.Target) {
			return object.NewTypeError("Proxy 'defineProperty': trap reported a new property on a non-extensible target")
		}
		return p
	}
	if _, errVal := validateAndApplyDesc(pxy.Target, store, keyStr, parsed); errVal != nil {
		return object.NewTypeError("Proxy 'defineProperty': trap result is incompatible with the target property descriptor")
	}
	return p
}

// defineOneProperty 按 property descriptor 在任意 OwnPropertyStore 上定义
// 一个属性: 访问器描述符 (get/set) 注册为访问器；数据描述符
// (value/writable) 注册为数据属性。供 defineProperty / defineProperties
// 共用 (普通对象 / 数组 / 函数类一视同仁 —— 各类型的描述符落点由
// OwnPropertyStore.DefineOwn 负责)。
// 完整走 ToPropertyDescriptor (字段校验/混用拒绝) + ValidateAndApply
// (不可配置兼容性、±0 SameValue、不可扩展新属性) —— T6 的两处规范收口。
// target 用于可扩展性判定 (store 自身可能不带该状态)。
func defineOneProperty(target object.Value, store object.OwnPropertyStore, key string, descVal object.Value) object.Value {
	// 数组专属: "length" 走 ArraySetLength (规范 10.4.2.1); 越界索引要求
	// 数组可扩展且 length 可写 (rFXXbq: defineProperty length/索引族)。
	if arr, ok := store.(*object.Array); ok {
		if key == "length" {
			return defineArrayLengthDesc(arr, descVal)
		}
		if idx, err := strconv.Atoi(key); err == nil && idx >= 0 && idx >= len(arr.Elements) {
			if !arr.IsExtensible() || arr.NonWritableLength {
				return object.NewTypeError("Cannot define property %s: array length is non-writable or array is non-extensible", key)
			}
			// 定义索引 >= len 会按规范把 length 增长到 idx+1 —— 密实表示
			// 无法物化超过实现上限的长度 (无稀疏数组), 如实报 RangeError
			// 快失败 (15.2.3.6-4-183 / 15.2.3.7-6-a-179 族此前在此挂死)。
			if idx >= object.MaxDenseArrayElements {
				return object.NewRangeError("array index %s exceeds implementation limit", key)
			}
		}
	}
	if !object.IsObjectValue(descVal) {
		// 规范: ToPropertyDescriptor 对非对象 (原始值) 抛 TypeError。
		return object.NewTypeError("Property description must be an object")
	}
	parsed, errVal := toPropertyDescriptor(descVal)
	if errVal != nil {
		return errVal
	}
	// TypedArray 规范数值索引键: exotic [[DefineOwnProperty]] (10.7.8.3) ——
	// 无效索引 / 访问器描述符 / 收窄 writable-enumerable-configurable →
	// 失败 (Object.defineProperty 转 TypeError, Reflect.defineProperty 转
	// false); 这类键绝不落普通描述符存储。非规范索引键 ("1.0"/"+1"/"foo")
	// 继续走下方普通流程。
	if ta, ok := store.(*object.TypedArray); ok {
		if f, isCanonical := object.CanonicalNumericIndexString(key); isCanonical {
			if !ta.DefineOwnIndexed(f, taDescFromParsed(parsed)) {
				return object.NewTypeError("Cannot define property %s: invalid descriptor for typed array element", key)
			}
			return nil
		}
	}
	final, errVal := validateAndApplyDesc(target, store, key, parsed)
	if errVal != nil {
		return errVal
	}
	if !store.DefineOwn(key, final) {
		// 实现了 OwnPropertyStore 却拒绝该键: 函数类内部字段等 —— 规范的
		// [[DefineOwnProperty]] 对普通对象不会失败, 这里如实上抛。
		return object.NewTypeError("Cannot redefine property: %s", key)
	}
	return nil
}

// toUint32Float 实现 ToUint32 (供数组 length 校验)。
func toUint32Float(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	if f <= -1e18 || f >= 1e18 {
		// 超出精度/巨大值: 规范用 int32 位模式取模, 这里用 2^32 取模近似
		// (对 2^53 内的整数值精确)。
		m := math.Mod(math.Trunc(f), 4294967296)
		if m < 0 {
			m += 4294967296
		}
		return m
	}
	m := math.Mod(math.Trunc(f), 4294967296)
	if m < 0 {
		m += 4294967296
	}
	return m
}

// defineArrayLengthDesc 实现 ArraySetLength: Object.defineProperty(arr,
// "length", desc) 的语义 (截断/增长/RangeError/不可写 length)。
// get 为描述符字段读取器 (HasProperty+Get 语义)。
func defineArrayLengthDesc(arr *object.Array, descVal object.Value) object.Value {
	oldLen := len(arr.Elements)
	newLen := oldLen
	hasNewLen := false
	// ToPropertyDescriptor/ArraySetLength: value 字段存在 (即使 undefined)
	// 即触发长度变更; ToUint32(value) ≠ ToNumber(value) → RangeError。
	if v, found := descVal.GetProperty("value"); found {
		num, errVal := toNumberViaPrimitive(v)
		if errVal != nil {
			return errVal
		}
		u := toUint32Float(num)
		if u != num {
			return object.NewRangeError("Invalid array length")
		}
		newLen = int(u)
		hasNewLen = true
	}
	// length 恒不可配置、不可枚举: 显式写 true → TypeError。
	if v, found := descVal.GetProperty("enumerable"); found && toBool(v) {
		return object.NewTypeError("Cannot redefine property: length")
	}
	if v, found := descVal.GetProperty("configurable"); found && toBool(v) {
		return object.NewTypeError("Cannot redefine property: length")
	}
	// 访问器描述符: length 是不可配置的数据属性 → TypeError
	if _, hasGet := descVal.GetProperty("get"); hasGet {
		return object.NewTypeError("Cannot redefine property: length")
	}
	if _, hasSet := descVal.GetProperty("set"); hasSet {
		return object.NewTypeError("Cannot redefine property: length")
	}
	newWritable := !arr.NonWritableLength
	if v, found := descVal.GetProperty("writable"); found {
		newWritable = toBool(v)
	}
	// 现存 length 不可写时, 新描述符也不得把它改回可写。
	if v, found := descVal.GetProperty("writable"); found && toBool(v) && arr.NonWritableLength {
		return object.NewTypeError("Cannot redefine property: length")
	}
	if !hasNewLen {
		arr.NonWritableLength = !newWritable
		return nil
	}
	if newLen >= oldLen {
		// 增长: 现存 length 不可写时 newLen > oldLen 失败; 相等允许
		// (规范 15.2.3.6-4-163)。
		if arr.NonWritableLength && newLen > oldLen {
			return object.NewTypeError("Cannot redefine property: length")
		}
		// 密实表示无法物化超过实现上限的长度 (没有稀疏数组), 如实报
		// RangeError 快失败 (15.2.3.6-4-154 族此前在此 append 亿级元素挂死)。
		if newLen > object.MaxDenseArrayElements {
			return object.NewRangeError("array length exceeds implementation limit")
		}
		for len(arr.Elements) < newLen {
			arr.Elements = append(arr.Elements, object.UndefinedSingleton)
		}
		arr.NonWritableLength = !newWritable
		return nil
	}
	// 收缩: 现存 length 不可写 → TypeError (规范 15.2.3.6-4-164);
	// 从高位向低位逐个 [[Delete]]; 遇到不可配置元素停在 j+1 ——
	// 规范 OrdinaryDefineOwnProperty (数组 length 派生) 的 newLenFail:
	// 删除失败一律 reject (TypeError), 且不得把 length 写成不可写
	// (15.2.3.6-4-116 / -4-168 族: 此前静默部分截断且强写 NonWritableLength)。
	if arr.NonWritableLength {
		return object.NewTypeError("Cannot redefine property: length")
	}
	limit := newLen
	newLenFail := false
	for j := oldLen - 1; j >= newLen; j-- {
		if !arr.DeleteOwn(strconv.Itoa(j)) {
			limit = j + 1
			newLenFail = true
			break
		}
	}
	arr.TruncateTo(limit)
	arr.DropDescsFrom(limit)
	if newLenFail {
		return object.NewTypeError("Cannot redefine property: length")
	}
	arr.NonWritableLength = !newWritable
	return nil
}

// groupByImpl 实现 Object.groupBy / Map.groupBy 的共同逻辑。
// useMap=false 返回普通对象 (键经 ToString)，true 返回 Map (键保持原值)。
func groupByImpl(args []object.Value, useMap bool) object.Value {
	if len(args) < 2 || !object.IsCallable(args[1]) {
		return object.NewTypeError("groupBy: callback must be a function")
	}
	callback := args[1]
	result := object.NewMap()
	if !useMap {
		result = object.NewMap() // 先收集到 Map，最后按需转为对象
	}
	next, ok := object.Iterate(args[0])
	if !ok {
		return object.NewTypeError("groupBy: first argument is not iterable")
	}
	i := 0
	for {
		item, done := next()
		// next()/value getter 抛错 (iterator-next-throws 族): IteratorClose
		// 后如实上抛, 此前抛错被吞成 done, 用例假通过; 无限循环时挂死。
		if thrown := callbackThrown(); thrown != nil {
			object.CloseIterator(args[0])
			return thrown
		}
		if done {
			break
		}
		key := object.CallFunction(callback, object.UndefinedSingleton, item, object.NewInt(int64(i)))
		if thrown := callbackThrown(); thrown != nil {
			object.CloseIterator(args[0])
			return thrown
		}
		i++
		// 追加到该键的组
		entryMap := result
		var groupKey object.Value = key
		if !useMap {
			groupKey = object.NewString(toStr(key))
		}
		existing, found := entryMap.Get(groupKey)
		if found {
			if arr, isArr := existing.(*object.Array); isArr {
				arr.Elements = append(arr.Elements, item)
				continue
			}
		}
		entryMap.Set(groupKey, object.NewArray([]object.Value{item}))
	}
	if useMap {
		return result
	}
	// Map → 普通对象
	obj := object.NewObject()
	for _, entry := range result.Entries {
		obj.SetProperty(toStr(entry.Key), entry.Value)
	}
	return obj
}
