package stdlib

import (
	"strconv"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// setupReflectProxy 设置 Reflect 全局对象和 Proxy 构造器。
//
// Reflect: 提供与 Proxy trap 对应的反射方法，直接操作目标对象。
//   - Reflect.get / set / has / deleteProperty
//   - Reflect.getPrototypeOf / setPrototypeOf
//   - Reflect.ownKeys / isExtensible
//   - Reflect.apply / construct
//
// Proxy: 包装目标对象，通过 handler 上的 trap 拦截操作。
//
//	这里只负责创建代理。trap 的转发由 vm 包在属性访问/调用时完成。
func setupReflectProxy(env *runtime.Environment) {
	// ===== Proxy =====
	proxyFn := object.NewBuiltin("Proxy", func(args ...object.Value) object.Value {
		if len(args) < 2 {
			return object.NewTypeError("Proxy constructor requires 2 arguments")
		}
		target := args[0]
		handler := args[1]
		if target == nil || handler == nil {
			return object.NewTypeError("Cannot create proxy with a non-object as target or handler")
		}
		p, err := object.NewProxy(target, handler)
		if err != nil {
			return err
		}
		return p
	})

	// Proxy.revocable(target, handler): 返回 { proxy, revoke }
	proxyFn.Properties = map[string]object.Value{
		"revocable": object.NewBuiltin("revocable", func(args ...object.Value) object.Value {
			if len(args) < 2 {
				return object.NewTypeError("Proxy.revocable requires 2 arguments")
			}
			p, err := object.NewProxy(args[0], args[1])
			if err != nil {
				return err
			}
			// 简化: revoke 标记代理已撤销，撤销后操作抛 TypeError。
			// revocation function 是匿名内建: name="" 且无 [[Construct]]
			// (revocation-function-name / -not-a-constructor 族, T4/T5)。
			// 名单按 "revoke" 登记, 注册名先用它匹配名单再清空 Name。
			revoke := object.NewBuiltin("revoke", func(args ...object.Value) object.Value {
				p.IsRevoked = true
				return object.UndefinedSingleton
			})
			revoke.Name = ""
			res := object.NewObject()
			res.SetProperty("proxy", p)
			res.SetProperty("revoke", revoke)
			return res
		}),
	}

	env.Declare("Proxy", proxyFn, false)

	// ===== Reflect =====
	reflectObj := object.NewObject()
	setNamespaceProto(reflectObj)
	// %Reflect%[@@toStringTag] = "Reflect" —— 命名空间对象的品牌标签。
	setToStringTag(reflectObj, "Reflect")

	// ===== proxy trap 分发公共段 (T5 r8dXes) =====
	// reflectProxyTrap 对 proxy 目标做 GetMethod(handler, trapName) 语义分发:
	//   非 proxy            → (nil, false, nil): 调用方走原有 target 路径;
	//   revoked             → TypeError (所有内部方法共享);
	//   trap 存在不可调用   → TypeError "'%s' trap must be callable"
	//                         (trap-is-not-callable 族此前被静默吞掉);
	//   trap 不存在         → (nil, false, nil): 调用方转发 target;
	//   trap 可调用         → 调用 trap (this 绑 handler), trap 内异常经
	//                         回调桥由 VM 的 checkCallbackErr 重抛。
	// 注意: 这里不做不变式校验 (invariant 族单列), 只做 GetMethod + 转发。
	// trap 的首参一律是 [[ProxyTarget]] (规范 Proxy 内部方法语义) ——
	// 由 helper 注入, 调用点只传其余参数。此前调用点误传 proxy 本身,
	// trap 里再操作 proxy 会无限递归 (not-extensible-target-same-target-
	// prototype 退潮的直接根因)。
	//
	// 嵌套 target 转发 (trap-is-null/undefined/missing-target-is-proxy 族):
	// 外层 trap 缺失且 target 仍是 proxy 时, 逐层深入内层 handler —— trap
	// 首参始终是**当场那层** proxy 的 target。循环出口: 非 proxy (调用方走
	// 原 target 路径) / 某层 trap 命中 / 某层 revoked。
	reflectProxyTrap := func(v object.Value, trapName string, args ...object.Value) (object.Value, bool, object.Value) {
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

	// Reflect.get(target, key, receiver?) — 读取目标属性
	reflectObj.SetBuiltinProperty("get", object.NewBuiltin("Reflect.get", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.get")
		if errVal != nil {
			return errVal
		}
		if len(args) < 2 {
			return object.NewTypeError("Reflect.get: property key is required")
		}
		key := toPropKey(args[1])
		// trap 收到规范化的 Property Key: 字符串键用 toPropKey 结果,
		// Symbol 键原样传 (ToPropertyKey 不转换 Symbol)。
		trapKey := object.Value(object.NewString(key))
		if _, isSym := args[1].(*object.Symbol); isSym {
			trapKey = args[1]
		}
		var receiver object.Value = args[0]
		if len(args) > 2 {
			receiver = args[2]
		}
		// proxy: get trap 分发 (GetMethod 语义); 无 trap 时沿 target 转发
		if res, handled, errVal := reflectProxyTrap(target, "get", trapKey, receiver); errVal != nil {
			return errVal
		} else if handled {
			if res == nil {
				return object.UndefinedSingleton
			}
			return res
		}
		// 代理通过 VM 转发 get trap；这里 Reflect 直接读取目标
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		val, found := target.GetProperty(key)
		if !found {
			return object.UndefinedSingleton
		}
		return val
	}))

	// Reflect.set(target, key, value, receiver?) — 写入目标属性
	reflectObj.SetBuiltinProperty("set", object.NewBuiltin("Reflect.set", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.set")
		if errVal != nil {
			return errVal
		}
		if len(args) < 3 {
			return object.NewTypeError("Reflect.set: value is required")
		}
		key := toPropKey(args[1])
		val := args[2]
		// proxy: set trap 分发; trap 返回 falsy → Reflect.set 返回 false
		if res, handled, errVal := reflectProxyTrap(target, "set", args[1], val, args[0]); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		// TypedArray: [[Set]] (10.7.8.5) —— 规范数值索引静默写/忽略恒 true;
		// 其余键按 OrdinarySet 自有语义 (不可写 → false)。
		if ta, ok := target.(*object.TypedArray); ok {
			if sym, isSym := args[1].(*object.Symbol); isSym {
				ta.SetSymbolProperty(sym, val)
				return object.TrueSingleton
			}
			return object.NewBoolean(ta.SetOwn(key, val))
		}
		// 不可写 / 不可扩展时必须返回 false，而不是无条件 true
		if o, ok := target.(*object.Object); ok {
			if desc, exists := o.Properties[key]; exists {
				if !desc.Writable {
					return object.FalseSingleton
				}
			} else if !o.Extensible {
				return object.FalseSingleton
			}
		}
		target.SetProperty(key, val)
		return object.TrueSingleton
	}))

	// Reflect.has(target, key) — 检查属性是否存在 (含原型链)
	reflectObj.SetBuiltinProperty("has", object.NewBuiltin("Reflect.has", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.has")
		if errVal != nil {
			return errVal
		}
		if len(args) < 2 {
			return object.NewTypeError("Reflect.has: property key is required")
		}
		key := toPropKey(args[1])
		// proxy: has trap 分发
		if res, handled, errVal := reflectProxyTrap(target, "has", args[1]); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		// TypedArray: [[HasProperty]] —— 规范数值索引按 IsValidIntegerIndex
		// 裁决 (越界/-0/非整数 → false), 其余键 Ordinary 自有 + 原型链。
		if ta, ok := target.(*object.TypedArray); ok {
			if sym, isSym := args[1].(*object.Symbol); isSym {
				_, found := ta.GetSymbolPropertyDescriptor(sym)
				return object.NewBoolean(found)
			}
			return object.NewBoolean(ta.HasProperty(key))
		}
		_, found := target.GetProperty(key)
		// Array.GetProperty 对任意数字索引都返回 (undefined, true)，
		// 会让 Reflect.has(arr, "999") 恒为 true。需按索引范围修正。
		if found {
			if arr, ok := target.(*object.Array); ok {
				if idx, err := strconv.Atoi(key); err == nil {
					found = idx >= 0 && idx < len(arr.Elements)
				}
			}
		}
		return object.NewBoolean(found)
	}))

	// Reflect.deleteProperty(target, key) — 删除自有属性
	reflectObj.SetBuiltinProperty("deleteProperty", object.NewBuiltin("Reflect.deleteProperty", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.deleteProperty")
		if errVal != nil {
			return errVal
		}
		if len(args) < 2 {
			return object.NewTypeError("Reflect.deleteProperty: property key is required")
		}
		key := toPropKey(args[1])
		// proxy: deleteProperty trap 分发
		if res, handled, errVal := reflectProxyTrap(target, "deleteProperty", args[1]); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		// 走 DeleteProperty 而不是直接 delete map: 后者会漏掉
		// Object.InsertOrder 的清理，导致键重新加入后顺序错乱。
		switch t := target.(type) {
		case *object.Object:
			return object.NewBoolean(t.DeleteProperty(key))
		case *object.Array:
			if idx, err := strconv.Atoi(key); err == nil &&
				idx >= 0 && idx < len(t.Elements) {
				t.Elements[idx] = object.UndefinedSingleton
			}
			return object.TrueSingleton
		case *object.TypedArray:
			// [[Delete]] (10.7.8.6): 规范数值索引按 IsValidIntegerIndex 裁决
			// (范围内元素恒 false), 其余键 OrdinaryDelete。
			if sym, isSym := args[1].(*object.Symbol); isSym {
				t.DeleteSymbolProperty(sym)
				return object.TrueSingleton
			}
			return object.NewBoolean(t.DeleteOwn(key))
		}
		return object.TrueSingleton
	}))

	// Reflect.getPrototypeOf(target) — 获取原型
	reflectObj.SetBuiltinProperty("getPrototypeOf", object.NewBuiltin("Reflect.getPrototypeOf", func(args ...object.Value) object.Value {
		if len(args) < 1 {
			return object.NullSingleton
		}
		target := args[0]
		// 规范 26.1.8 step 2: ? RequireObjectCoercible; step 3: 若 Type(target)
		// 非 Object 抛 TypeError —— Reflect 不做 ToObject (与 Object.getPrototypeOf
		// 的分野; target-is-not-object-throws / target-is-symbol-throws, B8 rm4Zcc)。
		switch target.(type) {
		case *object.Null, *object.Undefined, *object.Number, *object.String,
			*object.Boolean, *object.Symbol, *object.BigInt:
			return object.NewTypeError("Reflect.getPrototypeOf: target must be an object")
		}
		// proxy: getPrototypeOf trap 分发; 结果必须是对象或 null
		if res, handled, errVal := reflectProxyTrap(target, "getPrototypeOf"); errVal != nil {
			return errVal
		} else if handled {
			if res == nil || res == object.UndefinedSingleton || res == object.NullSingleton {
				return object.NullSingleton
			}
			if !object.IsObjectLike(res) {
				return object.NewTypeError("'getPrototypeOf' trap must return an object or null")
			}
			return res
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		switch t := target.(type) {
		case *object.Object:
			return t.Proto
		case *object.Array:
			return t.GetProto()
		// Promise 实例的 [[Prototype]] = %Promise.prototype% (rj9MwH)。
		// 与 Object.getPrototypeOf 同一缺口: 缺这条分支会退化成 null。
		case *object.Promise:
			return t.GetProto()
		}
		// 函数对象 (Closure/BuiltinFunction/BuiltinMethod) 的 [[Prototype]]。
		if fp := object.FuncPrototypeOf(target); fp != nil {
			return fp
		}
		// 兜底: 鸭子类型入口 `GetProto() Value` (Temporal 各类型等)。
		if ph, ok := target.(interface{ GetProto() object.Value }); ok {
			if p := ph.GetProto(); p != nil {
				return p
			}
		}
		return object.NullSingleton
	}))

	// Reflect.setPrototypeOf(target, proto) — 设置原型
	reflectObj.SetBuiltinProperty("setPrototypeOf", object.NewBuiltin("Reflect.setPrototypeOf", func(args ...object.Value) object.Value {
		if len(args) < 2 {
			return object.NewBoolean(false)
		}
		target := args[0]
		proto := args[1]
		// proxy: setPrototypeOf trap 分发; 结果按 ToBoolean 返回
		if res, handled, errVal := reflectProxyTrap(target, "setPrototypeOf", proto); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		switch t := target.(type) {
		case *object.Object:
			t.Proto = proto
			return object.TrueSingleton
		case *object.Array:
			t.SetProto(proto)
			return object.TrueSingleton
		}
		return object.NewBoolean(false)
	}))

	// Reflect.ownKeys(target) — 返回自有属性键数组
	reflectObj.SetBuiltinProperty("ownKeys", object.NewBuiltin("Reflect.ownKeys", func(args ...object.Value) object.Value {
		if len(args) < 1 {
			return object.NewArray(nil)
		}
		target := args[0]
		// proxy: ownKeys trap 分发; 结果 CreateListFromArrayLike
		if res, handled, errVal := reflectProxyTrap(target, "ownKeys"); errVal != nil {
			return errVal
		} else if handled {
			if arr, ok := res.(*object.Array); ok {
				return object.NewArray(arr.Elements)
			}
			return object.NewTypeError("'ownKeys' trap must return an array")
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		var keys []object.Value
		switch t := target.(type) {
		case *object.Object:
			for _, k := range t.Keys() {
				keys = append(keys, object.NewString(k))
			}
		case *object.Array:
			for i := range t.Elements {
				keys = append(keys, object.NewString(strconv.Itoa(i)))
			}
			keys = append(keys, object.NewString("length"))
		default:
			// 统一自有属性接口: 函数类 (length/name/prototype + 静态成员) 与
			// 全局对象等也按 OrdinaryOwnPropertyKeys 返回自有字符串键。此前
			// default 直接返回空数组 —— Reflect.ownKeys(fn) 恒为 []。无
			// OwnPropertyStore 的类型 (原始值 / Map / Set 等) 维持历史行为。
			store, ok := target.(object.OwnPropertyStore)
			if !ok {
				return object.NewArray(nil)
			}
			for _, k := range store.OwnKeys() {
				keys = append(keys, object.NewString(k))
			}
		}
		// Symbol 键按插入顺序附在字符串键之后 (OrdinaryOwnPropertyKeys 末段)。
		if sp, ok := target.(object.SymbolPropertyStore); ok {
			for _, sym := range sp.SymbolKeys() {
				keys = append(keys, sym)
			}
		}
		return object.NewArray(keys)
	}))

	// Reflect.isExtensible(target) — 检查是否可扩展
	reflectObj.SetBuiltinProperty("isExtensible", object.NewBuiltin("Reflect.isExtensible", func(args ...object.Value) object.Value {
		if len(args) < 1 {
			return object.NewBoolean(false)
		}
		target := args[0]
		// proxy: isExtensible trap 分发
		if res, handled, errVal := reflectProxyTrap(target, "isExtensible"); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		if o, ok := target.(*object.Object); ok {
			return object.NewBoolean(o.Extensible)
		}
		return object.TrueSingleton
	}))

	// Reflect.apply(fn, thisArg, argsArray) — 以 thisArg 调用 fn
	reflectObj.SetBuiltinProperty("apply", object.NewBuiltin("Reflect.apply", func(args ...object.Value) object.Value {
		if len(args) < 1 {
			return object.UndefinedSingleton
		}
		fn := args[0]
		var thisArg object.Value = object.UndefinedSingleton
		if len(args) > 1 {
			thisArg = args[1]
		}
		var callArgs []object.Value
		if len(args) > 2 {
			if arr, ok := args[2].(*object.Array); ok {
				callArgs = arr.Elements
			}
		}
		return object.CallFunction(fn, thisArg, callArgs...)
	}))

	// Reflect.construct(target, argsArray[, newTarget]) — 以构造方式调用
	reflectObj.SetBuiltinProperty("construct", object.NewBuiltin("Reflect.construct", func(args ...object.Value) object.Value {
		if len(args) < 1 || !object.IsConstructor(args[0]) {
			return object.NewTypeError("Reflect.construct: target is not a constructor")
		}
		fn := args[0]

		var ctorArgs []object.Value
		if len(args) > 1 {
			if arr, ok := args[1].(*object.Array); ok {
				ctorArgs = append([]object.Value(nil), arr.Elements...)
			}
		}
		// newTarget 省略时等于 target
		newTarget := fn
		if len(args) > 2 && args[2] != nil && !isUndefinedValue(args[2]) {
			// 规范 sec-reflect.construct 第 3 步: newTarget 必须实现
			// [[Construct]] (IsConstructor)，否则抛 TypeError。
			// 这条校验正是 harness isConstructor() 的判据，漏掉它会让
			// 内建函数一律被误判为可构造 (parseInt 等 not-a-constructor 族)。
			if !object.IsConstructor(args[2]) {
				return object.NewTypeError("Reflect.construct: newTarget is not a constructor")
			}
			newTarget = args[2]
		}
		// 规范 sec-reflect.construct 步骤 2: IsConstructor(newTarget) 为假抛
		// TypeError。isConstructor harness 正是靠这一步判定原型方法 (无
		// [[Construct]] 的 BuiltinMethod) 不可 new。
		if !object.IsConstructor(newTarget) {
			return object.NewTypeError("Reflect.construct: newTarget is not a constructor")
		}

		// proxy 构造分发 ([[Construct]], trap-is-null/undefined-target-is-proxy
		// 族): 外层 trap 缺失 → 转发 target 逐层深入; trap 命中 → 以
		// (target, argsArray, newTarget) 三参调用, 结果必须是对象。
		for {
			p, isProxy := fn.(*object.Proxy)
			if !isProxy {
				break
			}
			if p.IsRevoked {
				return object.NewTypeError("Cannot perform 'construct' on a proxy that has been revoked")
			}
			trap, present := object.GetProxyTrapMethod(p.Handler, "construct")
			if !present {
				fn = p.Target
				continue
			}
			if !object.IsCallable(trap) {
				return object.NewTypeError("'construct' trap must be callable")
			}
			trapArgs := []object.Value{p.Target, object.NewArray(ctorArgs), newTarget}
			res := object.CallFunction(trap, p.Handler, trapArgs...)
			if thrown := callbackThrown(); thrown != nil {
				return thrown
			}
			if !object.IsObjectLike(res) {
				return object.NewTypeError("'construct' trap must return an object")
			}
			return res
		}
		if !object.IsConstructor(fn) {
			return object.NewTypeError("Reflect.construct: target is not a constructor")
		}

		// 1) 创建新对象并绑定原型到 newTarget.prototype
		// 旧实现直接 new Object()，丢失了原型绑定，且把构造器返回值
		// 原样返回 (构造器不返回对象时应返回新对象)，导致结果为 undefined。
		obj := object.NewObject()
		if proto, found := newTarget.GetProperty("prototype"); found && proto != nil {
			obj.Proto = proto
		}
		// GetPrototypeFromConstructor 的 Get(newTarget, "prototype") 若其
		// getter 抛出 (get-prototype-from-constructor-throws 族), 须在调用
		// 构造器前把该异常原样抛出 —— 否则会被构造器的其它错误掩盖。
		if thrown := callbackThrown(); thrown != nil {
			return thrown
		}

		// 2) 以新对象为 this 调用构造器
		// new.target 经闭包携带传入: VM 的 callClosure 在无 pending 构造目标时
		// 取闭包自带的 NewTarget 写入帧, 于是构造器内的 `new.target` 即规范要求的
		// newTarget 参数 (sec-reflect.construct, 缺省为 target)。闭包按值复制一份,
		// 避免污染原函数对象 (原闭包 NewTarget 恒 nil)。
		callTarget := fn
		if c, ok := fn.(*object.Closure); ok {
			c2 := *c
			c2.NewTarget = newTarget
			callTarget = &c2
		}
		// Reflect.construct 是规范意义上的 [[Construct]], 对 RequiresNew 内建
		// (Temporal 构造器) 须视为"经 new 调用" —— 否则 tgCtorGuard 会误判。
		object.EnterConstructCall()
		result := object.CallFunction(callTarget, obj, ctorArgs...)
		object.LeaveConstructCall()
		// 与 r6OWbQ 同口径: 优先回传**原始抛出值** (保住错误类型与 catch 侧
		// === 语义), 并**同时消费值槽** —— 过去只消费错误槽, 残留的原值会
		// 污染之后任意一次桥错误的抛出值 (rr1O8P)。
		if thrown := callbackThrown(); thrown != nil {
			return thrown
		}

		// 3) 构造器返回对象时用返回值，否则用新创建的对象
		if result != nil && object.IsObjectLike(result) {
			return result
		}
		return obj
	}))

	// Reflect.defineProperty(target, key, desc) — 定义属性, 返回布尔。
	// 此前未注册 (T4 r8DmKW: Reflect/defineProperty/not-a-constructor 族
	// 直接 ReferenceError)。
	reflectObj.SetBuiltinProperty("defineProperty", object.NewBuiltin("Reflect.defineProperty", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.defineProperty")
		if errVal != nil {
			return errVal
		}
		if len(args) < 3 {
			return object.NewTypeError("Reflect.defineProperty: property key and descriptor are required")
		}
		desc := args[2]
		// proxy: defineProperty trap 分发; trap 返回 falsy → false
		if res, handled, errVal := reflectProxyTrap(target, "defineProperty", args[1], desc); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			// 无 trap: 解包 target 继续走普通定义路径
			target = p.Target
		}
		if store, ok := target.(object.OwnPropertyStore); ok {
			key := toPropKey(args[1])
			parsed, errVal2 := toPropertyDescriptor(args[2])
			if errVal2 != nil {
				return errVal2
			}
			// TypedArray 规范数值索引键: exotic [[DefineOwnProperty]], 失败
			// 按 Reflect.defineProperty 规范返回 false。
			if ta, isTA := target.(*object.TypedArray); isTA {
				if f, isCanonical := object.CanonicalNumericIndexString(key); isCanonical {
					return object.NewBoolean(ta.DefineOwnIndexed(f, taDescFromParsed(parsed)))
				}
			}
			store.DefineOwn(key, parsedToDesc(parsed))
			return object.TrueSingleton
		}
		return object.FalseSingleton
	}))

	// Reflect.getOwnPropertyDescriptor(target, key) — 自有属性描述符。
	// proxy 走 getOwnPropertyDescriptor trap (含不变式校验, 见
	// proxyGetOwnPropertyDescriptor, 与 Object.getOwnPropertyDescriptor 同路)。
	reflectObj.SetBuiltinProperty("getOwnPropertyDescriptor", object.NewBuiltin("Reflect.getOwnPropertyDescriptor", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.getOwnPropertyDescriptor")
		if errVal != nil {
			return errVal
		}
		if len(args) < 2 {
			return object.NewTypeError("Reflect.getOwnPropertyDescriptor: property key is required")
		}
		if pxy, ok := target.(*object.Proxy); ok {
			return proxyGetOwnPropertyDescriptor(pxy, args[1])
		}
		if store, ok := target.(object.OwnPropertyStore); ok {
			key := toPropKey(args[1])
			if d, exists := store.OwnDescriptor(key); exists {
				return describeProperty(d)
			}
		}
		return object.UndefinedSingleton
	}))

	// Reflect.preventExtensions(target) — 阻止扩展, 返回布尔
	reflectObj.SetBuiltinProperty("preventExtensions", object.NewBuiltin("Reflect.preventExtensions", func(args ...object.Value) object.Value {
		target, errVal := reflectTargetArg(args, "Reflect.preventExtensions")
		if errVal != nil {
			return errVal
		}
		// proxy: preventExtensions trap 分发
		if res, handled, errVal := reflectProxyTrap(target, "preventExtensions"); errVal != nil {
			return errVal
		} else if handled {
			return object.NewBoolean(object.IsTruthyValue(res))
		}
		if p, ok := target.(*object.Proxy); ok {
			target = p.Target
		}
		if o, ok := target.(*object.Object); ok {
			o.Extensible = false
			return object.TrueSingleton
		}
		return object.FalseSingleton
	}))

	env.Declare("Reflect", reflectObj, false)
}

// reflectTargetArg 校验 Reflect API 的 target 参数。
// 规范: target 必须是对象，否则抛 TypeError (旧实现静默返回默认值)。
func reflectTargetArg(args []object.Value, api string) (object.Value, object.Value) {
	if len(args) < 1 || !object.IsObjectLike(args[0]) {
		return nil, object.NewTypeError("%s called on non-object", api)
	}
	return args[0], nil
}

// toPropKey 将属性键值转为字符串。
func toPropKey(v object.Value) string {
	switch k := v.(type) {
	case *object.String:
		return k.Value
	case *object.Number:
		return k.Inspect()
	case *object.Symbol:
		return k.Inspect()
	}
	return v.Inspect()
}
