package stdlib

import (
	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// setupFunctionIntrinsics 装配函数对象的内建原型链。
//
// 需在 Object.prototype 就绪之后、setupAsync 之前调用 —— setupAsync 要用
// %Function.prototype% 去链接 %AsyncGeneratorFunction.prototype%。
//
// 规范口径 (经 Node 22 实测):
//
//	%Function%               .[[Prototype]] = %Function.prototype%
//	                         .prototype     = %Function.prototype%
//	%Function.prototype%     [[Prototype]]  = %Object.prototype% (可调用, 返回 undefined)
//	%GeneratorFunction%      .[[Prototype]] = %Function%          (!)
//	                         .prototype     = %GeneratorFunction.prototype%
//	%GeneratorFunction.prototype%   [[Prototype]] = %Function.prototype%
//	                                .prototype     = %GeneratorPrototype%
//	%AsyncFunction%          .[[Prototype]] = %Function%
//	                         .prototype     = %AsyncFunction.prototype%
//	%AsyncFunction.prototype%       [[Prototype]] = %Function.prototype% (无 .prototype)
//	%AsyncGeneratorFunction% 由 setupAsyncGeneratorIntrinsics 装配, 此处仅链接。
//
// 这些对象通过 object.SetXxxPrototype 注册，供 vm.createClosure 按函数种类
// 选择函数对象的 [[Prototype]]。
func setupFunctionIntrinsics(env *runtime.Environment) {
	objProto := objectPrototype // 由 setupObjectPrototype / SetObjectPrototypeRef 提供
	tagSym := object.GetGlobalSymbol("Symbol.toStringTag")

	// %Function.prototype%: 可调用 (规范: Function.prototype() 返回 undefined)，
	// 自身 [[Prototype]] 是 Object.prototype。
	funcProto := object.NewBuiltin("", func(args ...object.Value) object.Value {
		return object.UndefinedSingleton
	})
	if objProto != nil {
		funcProto.FuncPrototype = objProto
	}
	object.SetFunctionPrototype(funcProto)

	// B5 (rFsmHG) 15.2.3.3-4-35..38: call/apply/bind/toString 必须是
	// %Function.prototype% 的自有数据属性 ({writable:true, enumerable:false,
	// configurable:true})。此前只存在于各可调用类型 GetProperty 的虚拟分发，
	// gOPD(Function.prototype, "call") 返回 undefined。物化后 funcProto 自身
	// 的 GetProperty 命中 PropDescs (优先于虚拟分发)；其余可调用类型仍走
	// funcProtoLookup 虚拟兜底 —— 运行时行为不变。
	for _, m := range []string{"call", "apply", "bind", "toString"} {
		if impl, ok := object.FuncProtoSharedMethod(m); ok {
			funcProto.DefineOwn(m, object.PropertyDescriptor{
				Value: impl, Writable: true, Enumerable: false, Configurable: true,
			})
		}
	}

	// %Function% 构造器: new Function(...) / Function(...) 动态编译源码。
	funcCtor := object.NewBuiltin("Function", func(args ...object.Value) object.Value {
		return newDynamicFunction(env, args, dynFuncNormal)
	})
	funcCtor.SetProperty("name", object.NewString("Function"))
	funcCtor.SetFunctionLength(1)
	funcCtor.FuncPrototype = funcProto // %Function%.[[Prototype]] = %Function.prototype%
	setCtorPrototype(funcCtor, funcProto)
	// constructor: {writable:true, enumerable:false, configurable:true} (规范)。
	// 此前 SetProperty 使其可枚举，Object.keys(Function.prototype) 多出一个键。
	funcProto.DefineOwn("constructor", object.PropertyDescriptor{
		Value: funcCtor, Writable: true, Enumerable: false, Configurable: true,
	})
	env.Declare("Function", funcCtor, false)

	// %GeneratorFunction% + %GeneratorFunction.prototype% + %GeneratorPrototype%
	// 本身是可构造的 (CreateDynamicFunction kind="generator")。
	genFunc := object.NewBuiltin("GeneratorFunction", func(args ...object.Value) object.Value {
		return newDynamicFunction(env, args, dynFuncGenerator)
	})
	genFuncProto := object.NewObjectWithProto(funcProto) // %GeneratorFunction.prototype%
	genProto := object.NewObjectWithProto(objProto)      // %GeneratorPrototype%
	genFuncProto.SetBuiltinProperty("prototype", genProto)
	genFuncProto.SetBuiltinProperty("constructor", genFunc)
	if tagSym != nil {
		genFuncProto.SetBuiltinSymbolProperty(tagSym, object.NewString("GeneratorFunction"))
	}
	genProto.SetBuiltinProperty("constructor", genFuncProto)
	if tagSym != nil {
		genProto.SetBuiltinSymbolProperty(tagSym, object.NewString("Generator"))
	}
	setCtorPrototype(genFunc, genFuncProto)
	genFunc.FuncPrototype = funcCtor // %GeneratorFunction%.[[Prototype]] = %Function%
	object.SetGeneratorFunctionPrototype(genFuncProto)
	object.SetGeneratorPrototype(genProto)

	// %GeneratorPrototype%.next/return/throw —— 生成器实例的这三个方法是
	// 虚拟属性 (object.Generator.GetProperty), 此前 %GeneratorPrototype% 上
	// 没有可见的函数对象: genProto.next === undefined,
	// isConstructor(genProto.next) 类用例 (T4 r8DmKW) 与原型可见性用例挂。
	// 这里用 BuiltinMethod 注册 (天然无 [[Construct]]), 实例虚拟属性优先,
	// 两者共用同一驱动回调, 语义一致。
	if gp, ok := object.GetGeneratorPrototype().(*object.Object); ok {
		genResult := func(val object.Value, done bool) object.Value {
			res := object.NewObject()
			res.SetProperty("value", val)
			res.SetProperty("done", object.NewBoolean(done))
			return res
		}
		genReceiver := func(this object.Value, method string) (*object.Generator, object.Value) {
			gen, ok := this.(*object.Generator)
			if !ok {
				return nil, object.NewTypeError("Method Generator.prototype.%s called on incompatible receiver", method)
			}
			return gen, nil
		}
		genArg := func(args []object.Value) object.Value {
			if len(args) > 0 {
				return args[0]
			}
			return object.UndefinedSingleton
		}
		gp.SetBuiltinProperty("next", object.NewBuiltinMethod("next", func(this object.Value, args ...object.Value) object.Value {
			gen, errVal := genReceiver(this, "next")
			if errVal != nil {
				return errVal
			}
			val, done := object.GeneratorNext(gen, genArg(args))
			return genResult(val, done)
		}))
		gp.SetBuiltinProperty("return", object.NewBuiltinMethod("return", func(this object.Value, args ...object.Value) object.Value {
			gen, errVal := genReceiver(this, "return")
			if errVal != nil {
				return errVal
			}
			val, done := object.GeneratorReturn(gen, genArg(args))
			return genResult(val, done)
		}))
		gp.SetBuiltinProperty("throw", object.NewBuiltinMethod("throw", func(this object.Value, args ...object.Value) object.Value {
			gen, errVal := genReceiver(this, "throw")
			if errVal != nil {
				return errVal
			}
			val, done := object.GeneratorThrow(gen, genArg(args))
			return genResult(val, done)
		}))
	}

	// %AsyncFunction% + %AsyncFunction.prototype% (后者无 .prototype 属性)
	// 本身是可构造的 (CreateDynamicFunction kind="async")。
	asyncFunc := object.NewBuiltin("AsyncFunction", func(args ...object.Value) object.Value {
		return newDynamicFunction(env, args, dynFuncAsync)
	})
	asyncFuncProto := object.NewObjectWithProto(funcProto) // %AsyncFunction.prototype%
	asyncFuncProto.SetBuiltinProperty("constructor", asyncFunc)
	// %AsyncFunction.prototype%[@@toStringTag] = "AsyncFunction":
	// async 函数对象的 "[object AsyncFunction]" 品牌来自这里
	// (普通函数无此标签，回落 builtinTag "Function")。
	if tagSym != nil {
		asyncFuncProto.SetBuiltinSymbolProperty(tagSym, object.NewString("AsyncFunction"))
	}
	setCtorPrototype(asyncFunc, asyncFuncProto)
	asyncFunc.FuncPrototype = funcCtor // %AsyncFunction%.[[Prototype]] = %Function%
	object.SetAsyncFunctionPrototype(asyncFuncProto)
}
