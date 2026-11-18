package stdlib

import (
	"fmt"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// setupTypedArrays 注册 ES2015+ TypedArray 家族、ArrayBuffer 与 DataView,
// 以及 ES2026 的 Uint8Array.fromBase64 / Uint8Array.prototype.toBase64。
//
// 支持三种构造方式:
//
//	new Uint8Array(n)            — 指定元素个数
//	new Uint8Array(typedArr)     — 复制另一视图
//	new Uint8Array(buffer[, off[, len]]) — 在已有 buffer 上创建视图
//	new Uint8Array(iterable)     — 从可迭代值构造
//
// setupTypedArrays 注册 TypedArray 家族全局 (T3-5 冷启动惰性化)。
//
// 家族覆盖: ArrayBuffer / SharedArrayBuffer / 十种 TypedArray 构造器 /
// %TypedArray% / DataView。CPU 剖面里 setupTypedArrays ≈ 11% 的
// EvalFileVM 首次调用；脚本用到与否五五开，全部惰性化收益明确。
//
// 采用"多名字共享工厂": 家族内部互依赖紧 (具体构造器 prototype 链到
// %TypedArray%.prototype、SAB 复用 AB 的 prototype)，拆开各自惰性会引入
// 装配顺序耦合；物化任意一个名字即整组 Declare 就绪，成本只付一次。
//
// SetupGlobals 收尾对惰性根跳过的三件事由工厂物化时补做:
//  1. builtinLengths 里家族条目的 length (applyBuiltinLengthsPrefixes)
//  2. prototype 链回 %Object.prototype% (linkTypedArrayProtos)
//  3. 构造器 @@species 访问器 (applyTASpeciesAccessors)
func setupTypedArrays(env *runtime.Environment) {
	materialized := false
	// shared 闭包按 env 逐次创建: SetupGlobals 可被多次调用 (每脚本一次
	// 全新环境)，状态随 env 走，不引入包级可变状态。
	materialize := func() object.Value {
		if materialized {
			return nil
		}
		materialized = true
		buildTypedArrayGlobals(env)
		applyBuiltinLengthsPrefixes(env, taGlobalNames())
		linkTypedArrayProtos(env)
		applyTASpeciesAccessors(env)
		return nil
	}
	for _, n := range taGlobalNames() {
		env.DeclareLazy(n, materialize)
	}
}

// taGlobalNames 返回 TypedArray 家族注册到全局的绑定名 (惰性声明与
// 物化后 length 前缀补齐共用)。
func taGlobalNames() []string {
	names := make([]string, 0, 6+len(object.TAKindList()))
	names = append(names, "ArrayBuffer", "SharedArrayBuffer", "TypedArray", "DataView")
	for _, k := range object.TAKindList() {
		names = append(names, k.Name)
	}
	return names
}

// linkTypedArrayProtos 把家族各构造器的 .prototype 链回 %Object.prototype%
// (SetupGlobals 末尾 linkBuiltinPrototypes 对惰性根跳过部分的补做)。
// 名单与 linkBuiltinPrototypes 保持一致: ArrayBuffer / DataView + 十种具体
// 构造器 (%TypedArray% 与 SharedArrayBuffer 不在原名单，行为不变)。
func linkTypedArrayProtos(env *runtime.Environment) {
	objProto := object.GetObjectPrototype()
	if objProto == nil {
		return
	}
	names := []string{"ArrayBuffer", "DataView"}
	for _, k := range object.TAKindList() {
		names = append(names, k.Name)
	}
	for _, n := range names {
		v, ok := env.Get(n)
		if !ok || v == nil {
			continue
		}
		if p, ok := v.GetProperty("prototype"); ok {
			linkProtoTo(p, objProto)
		}
	}
}

// buildTypedArrayGlobals 构造 TypedArray 家族并 Declare 到全局
// (惰性工厂主体，原 setupTypedArrays 装配体)。
func buildTypedArrayGlobals(env *runtime.Environment) {
	// ----- ArrayBuffer -----
	abFn := object.NewBuiltin("ArrayBuffer", func(args ...object.Value) object.Value {
		n := 0
		if len(args) > 0 {
			f := toFloat(args[0])
			if f < 0 || f != float64(int(f)) {
				return object.NewRangeError("ArrayBuffer: invalid length")
			}
			n = int(f)
		}
		// ES2024: new ArrayBuffer(n, {maxByteLength}) → resizable
		max := -1
		if len(args) > 1 && !isUndefinedValue(args[1]) {
			if mv, ok := args[1].GetProperty("maxByteLength"); ok && !isUndefinedValue(mv) {
				f := toFloat(mv)
				if f < 0 || f != float64(int(f)) || int(f) < n {
					return object.NewRangeError("ArrayBuffer: invalid maxByteLength")
				}
				max = int(f)
			}
		}
		if max >= 0 {
			return object.NewResizableArrayBuffer(n, max)
		}
		return object.NewArrayBuffer(n)
	})
	// ArrayBuffer.prototype: 让 ArrayBuffer.prototype.resize / .slice 及
	// 访问器 (byteLength/resizable/maxByteLength/detached) 可被反射访问。
	abProto := object.NewObject()
	abThis := func(this object.Value) (*object.ArrayBuffer, *object.Error) {
		buf, ok := this.(*object.ArrayBuffer)
		if !ok {
			return nil, object.NewTypeError("ArrayBuffer.prototype method: receiver must be an ArrayBuffer")
		}
		return buf, nil
	}
	abProto.SetBuiltinProperty("resize", object.NewBuiltinMethod("resize", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		return buf.Resize(argOrV(args, 0))
	}))
	abProto.SetBuiltinProperty("transfer", object.NewBuiltinMethod("transfer", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		nb, aerr := buf.Transfer(argOrV(args, 0), false)
		if aerr != nil {
			return aerr
		}
		return nb
	}))
	abProto.SetBuiltinProperty("transferToFixedLength", object.NewBuiltinMethod("transferToFixedLength", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		nb, aerr := buf.Transfer(argOrV(args, 0), true)
		if aerr != nil {
			return aerr
		}
		return nb
	}))
	abProto.SetBuiltinProperty("slice", object.NewBuiltinMethod("slice", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		m, _ := buf.GetProperty("slice")
		return object.CallFunction(m, buf, args...)
	}))
	// ES2026 immutable ArrayBuffer: transferToImmutable / sliceToImmutable。
	// 结果 buffer 标记 Immutable (Resize/Transfer/元素写/DataView set 全抛
	// TypeError), 挂载见 object/typedarray.go 的 GetProperty 分派。
	abProto.SetBuiltinProperty("transferToImmutable", object.NewBuiltinMethod("transferToImmutable", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		m, _ := buf.GetProperty("transferToImmutable")
		return object.CallFunction(m, buf, args...)
	}))
	abProto.SetBuiltinProperty("sliceToImmutable", object.NewBuiltinMethod("sliceToImmutable", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		m, _ := buf.GetProperty("sliceToImmutable")
		return object.CallFunction(m, buf, args...)
	}))
	// 规范: ArrayBuffer.isView(arg) —— arg 是 TypedArray / DataView 时 true。
	abFn.SetProperty("isView", func() object.Value {
		b := object.NewBuiltin("isView", func(args ...object.Value) object.Value {
			if len(args) == 0 {
				return object.FalseSingleton
			}
			switch args[0].(type) {
			case *object.TypedArray, *object.DataView:
				return object.TrueSingleton
			}
			return object.FalseSingleton
		})
		b.SetFunctionLength(1)
		return b
	}())
	abProto.DefineAccessor("byteLength", object.NewBuiltinMethod("get byteLength", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		v, _ := buf.GetProperty("byteLength")
		return v
	}), nil)
	abProto.DefineAccessor("resizable", object.NewBuiltinMethod("get resizable", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		v, _ := buf.GetProperty("resizable")
		return v
	}), nil)
	abProto.DefineAccessor("maxByteLength", object.NewBuiltinMethod("get maxByteLength", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		v, _ := buf.GetProperty("maxByteLength")
		return v
	}), nil)
	abProto.DefineAccessor("detached", object.NewBuiltinMethod("get detached", func(this object.Value, args ...object.Value) object.Value {
		buf, err := abThis(this)
		if err != nil {
			return err
		}
		v, _ := buf.GetProperty("detached")
		return v
	}), nil)
	setCtorPrototype(abFn, abProto)
	env.Declare("ArrayBuffer", abFn, false)
	// SharedArrayBuffer: Gox 是单线程运行时, 无真正共享内存 —— 以 ArrayBuffer
	// 同构实现 (共享语义无从观测, 视图/拷贝逻辑与 ArrayBuffer 完全一致),
	// 使 features:[SharedArrayBuffer] 的用例可运行而非 ReferenceError。
	sabFn := object.NewBuiltin("SharedArrayBuffer", func(args ...object.Value) object.Value {
		n := 0
		if len(args) > 0 {
			f := toFloat(args[0])
			if f < 0 || f != float64(int(f)) {
				return object.NewRangeError("SharedArrayBuffer: invalid length")
			}
			n = int(f)
		}
		return object.NewArrayBuffer(n)
	})
	setCtorPrototype(sabFn, abProto)
	env.Declare("SharedArrayBuffer", sabFn, false)

	// ----- 十种 TypedArray 构造器 -----
	for _, kind := range object.TAKindList() {
		kind := kind
		ctor := object.NewBuiltin(kind.Name, func(args ...object.Value) object.Value {
			return constructTypedArray(kind, args)
		})
		ctor.IsTypedArrayCtor = true
		ctor.SetProperty("BYTES_PER_ELEMENT", object.NewInt(int64(kind.ElemSize)))
		// 静态方法
		ctor.SetProperty("of", object.NewBuiltin("of", func(args ...object.Value) object.Value {
			ta := object.NewTypedArray(kind, len(args))
			for i, v := range args {
				ta.SetElement(i, v)
			}
			return ta
		}))
		ctor.SetProperty("from", object.NewBuiltin("from", func(args ...object.Value) object.Value {
			if len(args) == 0 {
				return object.NewTypeError("%s.from: source is required", kind.Name)
			}
			next, ok := object.Iterate(args[0])
			if !ok {
				return object.NewTypeError("%s.from: source is not iterable", kind.Name)
			}
			var elements []object.Value
			for {
				v, done := next()
				if done {
					break
				}
				elements = append(elements, v)
			}
			// mapFn 处理
			if len(args) > 1 && object.IsCallable(args[1]) {
				thisArg := object.Value(object.UndefinedSingleton)
				if len(args) > 2 {
					thisArg = args[2]
				}
				mapped := make([]object.Value, len(elements))
				for i, v := range elements {
					mapped[i] = object.CallFunction(args[1], thisArg, v, object.NewInt(int64(i)))
				}
				elements = mapped
			}
			ta := object.NewTypedArray(kind, len(elements))
			for i, v := range elements {
				ta.SetElement(i, v)
			}
			return ta
		}))
		// prototype 对象: 规范形态 —— 共享方法全部只在 %TypedArray%.prototype
		// (经下方 po.Proto = taProto 沿链可达), 具体原型只挂 constructor 与
		// BYTES_PER_ELEMENT。此前自挂一份方法副本导致
		// Float64Array.prototype.hasOwnProperty("values") 为 true,
		// prototype/values/inherited.js 等直接退潮 (T3 rewZ3Q)。
		proto := object.NewObject()
		// 规范: <Type>Array.prototype.constructor === <Type>Array (不可枚举、
		// 可写、可配置)。缺失会让 sample.constructor 解析为 undefined,
		// SpeciesConstructor 的默认构造器回退与  verifiyProperty 类用例失败。
		proto.SetBuiltinProperty("constructor", ctor)
		proto.SetBuiltinProperty("BYTES_PER_ELEMENT", object.NewInt(int64(kind.ElemSize)))
		setCtorPrototype(ctor, proto)
		object.SetTypedArrayProto(kind.Name, proto)
		env.Declare(kind.Name, ctor, false)
	}

	// ----- %TypedArray% 抽象构造器 -----
	// test262 大量用例经 TypedArray.prototype.xxx 反射访问共享方法,
	// 以及 Object.getOwnPropertyDescriptor(TypedArray.prototype, Symbol.toStringTag)。
	taFn := object.NewBuiltin("TypedArray", func(args ...object.Value) object.Value {
		return object.NewTypeError("TypedArray: abstract class cannot be constructed")
	})
	taProto := object.NewObject()
	// 规范: %TypedArray%.prototype.[[Prototype]] === %Object.prototype%。
	// NewObject 的 [[Prototype]] 是 null (宿主侧纯数据形态), 不回填会导致
	// Int8Array.prototype.hasOwnProperty / .toString 等经链查找全部落空
	// (T3 rewZ3Q: inherited.js 家族的直接根因)。
	if op := object.GetObjectPrototype(); op != nil {
		taProto.Proto = op
	}
	taMethods := []string{"set", "subarray", "slice", "join",
		"indexOf", "lastIndexOf", "includes", "at", "fill", "copyWithin",
		"reverse", "sort", "forEach", "map", "filter", "every", "some",
		"find", "findIndex", "findLast", "findLastIndex",
		"reduce", "reduceRight", "keys", "values", "entries",
		"toLocaleString", "toReversed", "toSorted", "with"}
	// 规范定义的各方法 length 值 (verifyProperty 会核对描述符)。
	taMethodLengths := map[string]int{
		"at": 1, "copyWithin": 2, "entries": 0, "every": 1, "fill": 1,
		"filter": 1, "find": 1, "findIndex": 1, "findLast": 1, "findLastIndex": 1,
		"forEach": 1, "includes": 1, "indexOf": 1, "join": 1, "keys": 0,
		"lastIndexOf": 1, "map": 1, "reduce": 1, "reduceRight": 1, "reverse": 0,
		"set": 1, "slice": 2, "some": 1, "sort": 1, "subarray": 2,
		"toLocaleString": 0, "toReversed": 0, "toSorted": 1, "values": 0, "with": 2,
	}
	for _, m := range taMethods {
		mName := m
		bm := object.NewBuiltinMethod(mName, func(this object.Value, margs ...object.Value) object.Value {
			ta, ok := this.(*object.TypedArray)
			if !ok {
				return object.NewTypeError("TypedArray.prototype.%s: receiver must be a TypedArray", mName)
			}
			if v, found := object.CallTypedArrayMethod(ta, mName, margs); found {
				return v
			}
			return object.UndefinedSingleton
		})
		bm.SetFunctionLength(taMethodLengths[mName])
		taProto.SetBuiltinProperty(mName, bm)
	}
	// %TypedArray%.prototype.toString === Array.prototype.toString (join 委托,
	// 分离/越界 → TypeError)。没有它, taProto 链回 Object.prototype 之后
	// TA.prototype.toString 会被 Object.prototype.toString 遮蔽, 分离用例退潮
	// (TypedArray/prototype/toString/detached-buffer, T3 rewZ3Q)。
	taProto.SetBuiltinProperty("toString", object.NewBuiltinMethod("toString", func(this object.Value, margs ...object.Value) object.Value {
		if ta, ok := this.(*object.TypedArray); ok {
			if v, found := object.CallTypedArrayMethod(ta, "join", nil); found {
				return v
			}
			return object.UndefinedSingleton
		}
		if op := object.GetObjectPrototype(); op != nil {
			if ts, found := op.GetProperty("toString"); found && object.IsCallable(ts) {
				return object.CallFunction(ts, this, margs...)
			}
		}
		return object.NewString("[object Object]")
	}))
	// Symbol.iterator = values (规范同函数对象; 这里语义等价即可)。
	if v, ok := taProto.GetProperty("values"); ok {
		taProto.SetBuiltinSymbolProperty(object.GetGlobalSymbol("Symbol.iterator"), v)
	}
	// Symbol.toStringTag getter: TA 实例返回构造器名, 其他 receiver 返回 undefined。
	taProto.DefineSymbolAccessor(object.GetGlobalSymbol("Symbol.toStringTag"),
		object.NewBuiltinMethod("get [Symbol.toStringTag]", func(this object.Value, args ...object.Value) object.Value {
			if ta, ok := this.(*object.TypedArray); ok {
				return object.NewString(ta.Kind.Name)
			}
			return object.UndefinedSingleton
		}), nil)
	// %TypedArray%.prototype 的访问器: buffer/byteLength/byteOffset/length。
	taAcc := func(prop string) object.Value {
		return object.NewBuiltinMethod("get "+prop, func(this object.Value, args ...object.Value) object.Value {
			ta, ok := this.(*object.TypedArray)
			if !ok {
				return object.NewTypeError("TypedArray.prototype.%s: receiver must be a TypedArray", prop)
			}
			v, _ := ta.GetProperty(prop)
			return v
		})
	}
	for _, prop := range []string{"buffer", "byteLength", "byteOffset", "length"} {
		taProto.DefineAccessor(prop, taAcc(prop), nil)
	}
	setCtorPrototype(taFn, taProto)
	env.Declare("TypedArray", taFn, false)
	object.SetTypedArrayCtorProto(taFn)

	// 各具体构造器的 prototype 链到 %TypedArray%.prototype。
	for _, kind := range object.TAKindList() {
		if ctor, ok := env.Get(kind.Name); ok {
			if pv, ok2 := ctor.GetProperty("prototype"); ok2 {
				if po, ok3 := pv.(*object.Object); ok3 {
					po.Proto = taProto
				}
			}
		}
	}

	// ES2026: Uint8Array.fromBase64(str)
	if u8, ok := env.Get("Uint8Array"); ok {
		u8.SetProperty("fromBase64", object.NewBuiltin("fromBase64", func(args ...object.Value) object.Value {
			s := ""
			if len(args) > 0 {
				s = toStr(args[0])
			}
			data, ok := object.Base64Decode(s)
			if !ok {
				return object.NewSyntaxError("Uint8Array.fromBase64: invalid base64")
			}
			ta := object.NewTypedArray(object.MustLookupTAKind("Uint8Array"), len(data))
			for i, b := range data {
				ta.SetElement(i, object.NewInt(int64(b)))
			}
			return ta
		}))
	}

	// ----- ES2026 Uint8Array hex/base64 家族 (T3 rewZ3Q) -----
	// 规范要求 source 必须是 String (不调 toString, 非字符串直接 TypeError);
	// 接收者 ValidateTypedArray (分离/越界 → TypeError)。
	if u8, ok := env.Get("Uint8Array"); ok {
		u8.SetProperty("fromHex", object.NewBuiltin("fromHex", func(args ...object.Value) object.Value {
			s, err := requireStringArg(args, "Uint8Array.fromHex")
			if err != nil {
				return err
			}
			data, ok := hexDecodeSpec(s)
			if !ok {
				return object.NewSyntaxError("Uint8Array.fromHex: invalid hex")
			}
			ta := object.NewTypedArray(object.MustLookupTAKind("Uint8Array"), len(data))
			for i, b := range data {
				ta.SetElement(i, object.NewInt(int64(b)))
			}
			return ta
		}))
		if pv, ok2 := u8.GetProperty("prototype"); ok2 {
			if proto, ok3 := pv.(*object.Object); ok3 {
				proto.SetBuiltinProperty("toHex", object.NewBuiltinMethod("toHex", func(this object.Value, margs ...object.Value) object.Value {
					ta, err := u8ReceiverValidate(this, "toHex")
					if err != nil {
						return err
					}
					var sb []byte
					for i := 0; i < ta.ArrLength(); i++ {
						v := ta.GetElement(i)
						b := byte(toFloat(v))
						const digits = "0123456789abcdef"
						sb = append(sb, digits[b>>4], digits[b&0xf])
					}
					return object.NewString(string(sb))
				}))
				proto.SetBuiltinProperty("setFromHex", object.NewBuiltinMethod("setFromHex", func(this object.Value, margs ...object.Value) object.Value {
					ta, err := u8ReceiverValidate(this, "setFromHex")
					if err != nil {
						return err
					}
					s, serr := requireStringArg(margs, "Uint8Array.prototype.setFromHex")
					if serr != nil {
						return serr
					}
					read, written, ok := hexDecodeInto(ta, s)
					if !ok {
						return object.NewSyntaxError("Uint8Array.prototype.setFromHex: invalid hex")
					}
					return newReadWrittenResult(read, written)
				}))
				proto.SetBuiltinProperty("setFromBase64", object.NewBuiltinMethod("setFromBase64", func(this object.Value, margs ...object.Value) object.Value {
					ta, err := u8ReceiverValidate(this, "setFromBase64")
					if err != nil {
						return err
					}
					s, serr := requireStringArg(margs, "Uint8Array.prototype.setFromBase64")
					if serr != nil {
						return serr
					}
					url := false
					lastChunk := "loose"
					if len(margs) > 1 && !isUndefinedValue(margs[1]) {
						if av, found := margs[1].GetProperty("alphabet"); found && !isUndefinedValue(av) {
							switch toStr(av) {
							case "base64url":
								url = true
							case "base64":
							default:
								return object.NewRangeError("Uint8Array.prototype.setFromBase64: invalid alphabet option")
							}
						}
						if lv, found := margs[1].GetProperty("lastChunkHandling"); found && !isUndefinedValue(lv) {
							lastChunk = toStr(lv)
							switch lastChunk {
							case "loose", "strict", "stop-before-partial":
							default:
								return object.NewRangeError("Uint8Array.prototype.setFromBase64: invalid lastChunkHandling option")
							}
						}
					}
					read, written, ok := base64DecodeInto(ta, s, url, lastChunk)
					if !ok {
						return object.NewSyntaxError("Uint8Array.prototype.setFromBase64: invalid base64")
					}
					return newReadWrittenResult(read, written)
				}))
				// Uint8Array.prototype.toBase64 (具体原型不再自挂方法, 单独挂)
				proto.SetBuiltinProperty("toBase64", object.NewBuiltinMethod("toBase64", func(this object.Value, margs ...object.Value) object.Value {
					ta, err := u8ReceiverValidate(this, "toBase64")
					if err != nil {
						return err
					}
					if v, found := object.CallTypedArrayMethod(ta, "toBase64", margs); found {
						return v
					}
					return object.UndefinedSingleton
				}))
			}
		}
	}

	// ----- DataView -----
	// 规范语义收口 (rFXXbq): 此前只有实例魔法属性、无真 prototype ——
	// DataView.prototype === undefined, 反射访问全灭 (T1-4 C3 簇)。
	// 现在装配真 prototype: 方法走 object.DataViewMethod 工厂 (接收者
	// 内部槽校验 / ToIndex / ToNumber / 分离 / 越界 / resizable 追踪 /
	// Float16 / BigInt 一次到位), 访问器 buffer/byteLength/byteOffset
	// 与 Symbol.toStringTag getter 为内建访问器形态。
	dvFn := object.NewBuiltin("DataView", func(args ...object.Value) object.Value {
		// 25.3.2.1 DataView(buffer, byteOffset, byteLength)
		if len(args) == 0 {
			return object.NewTypeError("DataView: buffer is required")
		}
		ab, ok := args[0].(*object.ArrayBuffer)
		if !ok {
			return object.NewTypeError("DataView: first argument must be an ArrayBuffer")
		}
		// 3. offset = ? ToIndex(byteOffset) —— 先于分离检查。
		offset := 0
		if len(args) > 1 {
			idx, errVal := object.ToIndexValue(args[1])
			if errVal != nil {
				return errVal
			}
			offset = int(idx)
		}
		// 4. 分离检查 (ToIndex 之后)。
		if ab.Detach {
			return object.NewTypeError("DataView: buffer is detached")
		}
		// 6. offset 越界。
		if offset > len(ab.Data) {
			return object.NewRangeError("DataView: start offset %d is outside the bounds of the buffer", offset)
		}
		// 7. byteLength: undefined → length-tracking (resizable buffer 随
		// resize 伸缩); 其余 ? ToIndex(byteLength) + 越界检查。
		if len(args) > 2 && !isUndefinedValue(args[2]) {
			ln, errVal := object.ToIndexValue(args[2])
			if errVal != nil {
				return errVal
			}
			if offset+int(ln) > len(ab.Data) {
				return object.NewRangeError("DataView: byte length %d is outside the bounds of the buffer", ln)
			}
			return object.NewDataViewFull(ab, offset, int(ln))
		}
		return object.NewDataViewFull(ab, offset, -1)
	})
	dvProto := object.NewObject()
	for _, name := range object.DataViewMethodNames() {
		dvProto.SetBuiltinProperty(name, object.DataViewMethod(name))
	}
	// 访问器: buffer / byteLength / byteOffset (接收者校验后委托实例分派)。
	dvAcc := func(prop string) object.Value {
		return object.NewBuiltinMethod("get "+prop, func(this object.Value, args ...object.Value) object.Value {
			dv, ok := this.(*object.DataView)
			if !ok {
				return object.NewTypeError("DataView.prototype.%s: receiver must be a DataView", prop)
			}
			v, _ := dv.GetProperty(prop)
			return v
		})
	}
	for _, prop := range []string{"buffer", "byteLength", "byteOffset"} {
		dvProto.DefineBuiltinAccessor(prop, dvAcc(prop), nil)
	}
	// Symbol.toStringTag: DataView 实例返回 "DataView"。
	dvProto.DefineSymbolAccessor(object.GetGlobalSymbol("Symbol.toStringTag"),
		object.NewBuiltinMethod("get [Symbol.toStringTag]", func(this object.Value, args ...object.Value) object.Value {
			if _, ok := this.(*object.DataView); ok {
				return object.NewString("DataView")
			}
			return object.UndefinedSingleton
		}), nil)
	dvProto.SetBuiltinProperty("constructor", dvFn)
	setCtorPrototype(dvFn, dvProto)
	object.SetDataViewProto(dvProto)
	env.Declare("DataView", dvFn, false)
}

// argOrV 返回 args[i] 或 undefined。
func argOrV(args []object.Value, i int) object.Value {
	if i < len(args) {
		return args[i]
	}
	return object.UndefinedSingleton
}

// constructTypedArray 实现 new XxxArray(...) 的分支逻辑。
func constructTypedArray(kind object.TAKind, args []object.Value) object.Value {
	if len(args) == 0 || args[0] == object.UndefinedSingleton {
		return object.NewTypedArray(kind, 0)
	}
	switch src := args[0].(type) {
	case *object.Number:
		// new Uint8Array(n)
		n := src.Value
		if n < 0 || n != float64(int(n)) {
			return object.NewRangeError("%s: invalid length", kind.Name)
		}
		return object.NewTypedArray(kind, int(n))
	case *object.TypedArray:
		ta := object.NewTypedArray(kind, src.ArrLength())
		for i := 0; i < src.ArrLength(); i++ {
			ta.SetElement(i, src.GetElement(i))
		}
		return ta
	case *object.ArrayBuffer:
		// new Uint8Array(buffer[, byteOffset[, length]])
		// length 省略时为 length-tracking 视图 (ES2024: 跟随 buffer 当前
		// 字节长, resizable buffer resize 后视图长度随之变化)。
		off := 0
		if len(args) > 1 {
			off = int(toFloat(args[1]))
		}
		if off < 0 || off > len(src.Data) || off%kind.ElemSize != 0 {
			return object.NewRangeError("%s: view extends beyond buffer bounds", kind.Name)
		}
		if len(args) > 2 && !isUndefinedValue(args[2]) {
			length := int(toFloat(args[2]))
			if length < 0 || off+length*kind.ElemSize > len(src.Data) {
				return object.NewRangeError("%s: view extends beyond buffer bounds", kind.Name)
			}
			return object.NewTypedArrayView(kind, src, off, length)
		}
		return object.NewTypedArrayView(kind, src, off, -1)
	}
	// 可迭代: Array / Set / string 等
	if next, ok := object.Iterate(args[0]); ok {
		var elements []object.Value
		for {
			v, done := next()
			if done {
				break
			}
			elements = append(elements, v)
		}
		ta := object.NewTypedArray(kind, len(elements))
		for i, v := range elements {
			ta.SetElement(i, v)
		}
		return ta
	}
	// array-like: { length: n, 0: v0, ... } (无 Symbol.iterator)
	if lv, ok := args[0].GetProperty("length"); ok {
		n := int(toFloat(lv))
		if n >= 0 {
			ta := object.NewTypedArray(kind, n)
			for i := 0; i < n; i++ {
				v, _ := args[0].GetProperty(fmt.Sprintf("%d", i))
				if v == nil {
					v = object.UndefinedSingleton
				}
				ta.SetElement(i, v)
			}
			return ta
		}
	}
	return object.NewTypeError("%s: invalid argument", kind.Name)
}

// ===== ES2026 Uint8Array hex/base64 家族的解码实现 (T3 rewZ3Q) =====

// u8Receiver 校验接收者是 Uint8Array (非 BigInt)。
func u8Receiver(this object.Value) (*object.TypedArray, bool) {
	ta, ok := this.(*object.TypedArray)
	if !ok || ta.Kind.BigInt || ta.Kind.Name != "Uint8Array" {
		return nil, false
	}
	return ta, true
}

// u8ReceiverValidate = RequireInternalSlot(Uint8Array) + ValidateTypedArray:
// 接收者不是 Uint8Array → TypeError; 分离/越界视图 → TypeError
// (ValidateTypedArray, ES2024 起 immutable/越界同路径)。
func u8ReceiverValidate(this object.Value, method string) (*object.TypedArray, object.Value) {
	ta, ok := u8Receiver(this)
	if !ok {
		return nil, object.NewTypeError("Uint8Array.prototype.%s: receiver must be a Uint8Array", method)
	}
	if ta.OutOfBounds() {
		return nil, object.NewTypeError("Uint8Array.prototype.%s: typed array is out of bounds", method)
	}
	return ta, nil
}

// requireStringArg 规范口径: hex/base64 家族的 source 参数必须是 String,
// 不执行 ToString —— Symbol / 带 throwing toString 的对象直接 TypeError。
func requireStringArg(args []object.Value, who string) (string, object.Value) {
	if len(args) == 0 {
		return "", object.NewTypeError("%s: argument must be a string", who)
	}
	if _, ok := args[0].(*object.String); !ok {
		return "", object.NewTypeError("%s: argument must be a string", who)
	}
	return object.ToString(args[0]), nil
}

// newReadWrittenResult 构造 {read, written} 结果对象。
func newReadWrittenResult(read, written int) object.Value {
	res := object.NewPlainObject()
	res.SetBuiltinProperty("read", object.NewInt(int64(read)))
	res.SetBuiltinProperty("written", object.NewInt(int64(written)))
	return res
}

func isASCIIWhitespaceByte(c byte) bool {
	return c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// hexDecodeSpec 解析十六进制串 (fromHex 用): ASCII whitespace 可在任意两个
// 数字之间出现; 尾部孤位 / 非法字符 → 失败。
func hexDecodeSpec(s string) ([]byte, bool) {
	var out []byte
	hi := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIIWhitespaceByte(c) {
			continue
		}
		v, ok := hexNibble(c)
		if !ok {
			return nil, false
		}
		if hi < 0 {
			hi = int(v)
		} else {
			out = append(out, byte(hi<<4|int(v)))
			hi = -1
		}
	}
	if hi >= 0 {
		return nil, false // 尾部孤位
	}
	return out, true
}

// hexDecodeInto 实现 Uint8Array.prototype.setFromHex: 目标空间不足时停在
// 块边界 (read 回退到块起点), 非法输入整体失败。
func hexDecodeInto(ta *object.TypedArray, s string) (int, int, bool) {
	n := ta.ArrLength()
	read, written := 0, 0
	hi := -1
	pending := false // 当前块已有 1 个 nibble
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIIWhitespaceByte(c) {
			read++
			continue
		}
		v, ok := hexNibble(c)
		if !ok {
			return read, written, false
		}
		if hi < 0 {
			hi = int(v)
			pending = true
		} else {
			if written >= n {
				// 空间不足: 回退到本字节前 (read 不含已消费的两个 nibble)
				return read - 1, written, true
			}
			ta.SetElement(written, object.NewInt(int64(hi<<4|int(v))))
			written++
			hi = -1
			pending = false
		}
		read++
	}
	if pending {
		return read - 1, written, false // 尾部孤位 → SyntaxError
	}
	return read, written, true
}

const b64StdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const b64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// base64DecodeInto 实现 ES2026 FromBase64 (setFromBase64 共享核心):
// 逐 4 字符块解码, whitespace 不占块位; padding 只允许出现在最后一块;
// lastChunkHandling 控制 "loose / strict / stop-before-partial" 的尾部块语义。
// 返回 (read, written, ok); ok=false 表示 SyntaxError。
func base64DecodeInto(ta *object.TypedArray, s string, url bool, lastChunk string) (int, int, bool) {
	alpha := b64StdAlphabet
	if url {
		alpha = b64URLAlphabet
	}
	rev := [256]int8{}
	for i := range rev {
		rev[i] = -1
	}
	for i := 0; i < len(alpha); i++ {
		rev[alpha[i]] = int8(i)
	}
	n := ta.ArrLength()
	read, written := 0, 0
	sawPadding := false // 已遇到含 padding 的块, 其后不得再有 base64 字符
	for {
		offset := read
		var chars [4]byte
		clen := 0
		for clen < 4 && read < len(s) {
			c := s[read]
			if isASCIIWhitespaceByte(c) {
				read++
				continue
			}
			if sawPadding {
				return read, written, false // padding 块之后还有数据
			}
			chars[clen] = c
			clen++
			read++
		}
		if clen == 0 {
			return read, written, true // 输入耗尽, 无未决块
		}
		// 统计 padding 与尾块形态
		pad := 0
		for i := 0; i < clen; i++ {
			if chars[i] == '=' {
				pad++
				if i > 0 && chars[i-1] == '=' {
					continue
				}
				continue
			}
			if pad > 0 {
				return read, written, false // '=' 出现在非尾部位置
			}
		}
		// 解码块
		var vals [4]int8
		for i := 0; i < clen-pad; i++ {
			v := rev[chars[i]]
			if v < 0 {
				return read, written, false
			}
			vals[i] = v
		}
		var out [3]byte
		outN := 0
		switch {
		case clen == 4 && pad == 0:
			out[0] = byte(vals[0]<<2 | vals[1]>>4)
			out[1] = byte(vals[1]<<4 | vals[2]>>2)
			out[2] = byte(vals[2]<<6 | vals[3])
			outN = 3
		case clen == 4 && pad == 1: // XYZ=
			if vals[2]&3 != 0 {
				if lastChunk == "strict" {
					return read, written, false // 非零 padding bits
				}
			}
			out[0] = byte(vals[0]<<2 | vals[1]>>4)
			out[1] = byte(vals[1]<<4 | vals[2]>>2)
			outN = 2
		case clen == 4 && pad == 2: // XY==
			if vals[1]&0xf != 0 {
				if lastChunk == "strict" {
					return read, written, false
				}
			}
			out[0] = byte(vals[0]<<2 | vals[1]>>4)
			outN = 1
		case pad > 0: // 部分 padding ('Zg=' 形态或纯 pad)
			if lastChunk == "stop-before-partial" {
				return offset, written, true // 停在块前
			}
			return read, written, false
		case clen == 3: // 尾块无 padding, 3 字符 → 2 字节
			if lastChunk != "loose" {
				if lastChunk == "strict" {
					return read, written, false
				}
				// stop-before-partial: 停在块前
				return offset, written, true
			}
			out[0] = byte(vals[0]<<2 | vals[1]>>4)
			out[1] = byte(vals[1]<<4 | vals[2]>>2)
			outN = 2
		case clen == 2: // 尾块无 padding, 2 字符 → 1 字节
			if lastChunk != "loose" {
				if lastChunk == "strict" {
					return read, written, false
				}
				return offset, written, true
			}
			out[0] = byte(vals[0]<<2 | vals[1]>>4)
			outN = 1
		default:
			return read, written, false // 单字符尾块
		}
		if clen < 4 && pad > 0 {
			// 不可达 (pad>0 的块已在上面返回), 保险起见
			sawPadding = true
		}
		if clen == 4 && pad > 0 {
			sawPadding = true
		}
		// 目标空间不足: 回退整块
		if written+outN > n {
			return offset, written, true
		}
		for i := 0; i < outN; i++ {
			ta.SetElement(written, object.NewInt(int64(out[i])))
			written++
		}
	}
}
