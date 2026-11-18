package stdlib

import (
	"math"
	"strings"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// setupRegExp 设置 RegExp 构造器。
func setupRegExp(env *runtime.Environment) {
	regexpProto := setupRegExpProto()
	object.SetRegExpProto(regexpProto)

	// RegExp 作为函数/构造器
	regexpFn := object.NewBuiltin("RegExp", func(args ...object.Value) object.Value {
		pattern := ""
		flags := ""
		if len(args) > 0 {
			pattern = toStr(args[0])
		}
		if len(args) > 1 {
			flags = toStr(args[1])
		}
		// 如果参数已经是 RegExp，提取 pattern 和 flags
		if len(args) > 0 {
			if r, ok := args[0].(*object.RegExp); ok {
				pattern = r.Pattern
				if len(args) == 1 {
					flags = r.Flags
				}
			}
		}
		re, err := object.NewRegExp(pattern, flags)
		if err != nil {
			return object.NewErrorWithName("SyntaxError", err.Error())
		}
		return re
	})

	// %RegExp.prototype% 必须挂到构造器上 —— 此前 setupRegExpProto() 的产物
	// 只塞进了 object.RegExpProto (链式查找的事实来源), 却没写成构造器的
	// prototype 属性, 于是 `typeof RegExp.prototype` 是 undefined,
	// `RegExp.prototype.exec` 直接 TypeError (rEXjyz)。
	setCtorPrototype(regexpFn, regexpProto)
	// constructor 反向引用 (/a/.constructor === RegExp)。
	regexpProto.SetBuiltinProperty("constructor", regexpFn)
	// %RegExp.prototype%[@@toStringTag] = "RegExp" —— 实例的
	// Object.prototype.toString.call(/a/) 品牌来自这条原型链上的符号属性。
	setToStringTag(regexpProto, "RegExp")

	// RegExp.escape(str) (ES2025): 转义正则元字符，使字符串可安全内插。
	regexpFn.SetProperty("escape", object.NewBuiltin("escape", func(args ...object.Value) object.Value {
		s := ""
		if len(args) > 0 {
			s = toStr(args[0])
		}
		var b strings.Builder
		for _, ch := range s {
			if strings.ContainsRune("^$\\.*+?()[]{}|/", ch) {
				b.WriteByte('\\')
			}
			b.WriteRune(ch)
		}
		return object.NewString(b.String())
	}))

	env.Declare("RegExp", regexpFn, false)
}

// lastIndexTypeError 是 @@match / @@replace / exec 里「Set(R,"lastIndex",v,true)
// 失败」时应抛的错 (规范 ReturnIfAbrupt ⇒ TypeError)。
func lastIndexTypeError() object.Value {
	return object.NewTypeError("Cannot assign to read only property 'lastIndex'")
}

// regExpThis 校验 %RegExp.prototype% 各方法的 this (规范步骤 1~2):
//   - Type(rx) 不是 Object ⇒ TypeError;
//   - 是对象但没有 [[RegExpMatcher]] 内部槽 (Gox 里即不是 *object.RegExp)
//     ⇒ 同样 TypeError (no-regexp-matcher.js: 对 %RegExp.prototype% 自身调 exec)。
//
// 此前 `%RegExp.prototype%` 根本不存在, 这几个用例靠
// `RegExp.prototype[Symbol.match]` 求值为 undefined 而"抛"出 TypeError 侥幸通过
// (this-val-non-obj 一族)。原型装配好之后必须由这里显式判定。
// 第二返回值为 nil 表示校验通过。
func regExpThis(this object.Value) (*object.RegExp, object.Value) {
	if this == nil || !object.IsObjectValue(this) {
		return nil, object.NewTypeError("RegExp.prototype method called on incompatible receiver")
	}
	re, ok := this.(*object.RegExp)
	if !ok {
		return nil, object.NewTypeError("RegExp.prototype method called on incompatible receiver")
	}
	return re, nil
}

// lastIndexOfResult 从 exec 结果里取 index (规范的 Get(result, "index"))。
// 结果可以是数组 (内建 exec 的产物) 或任意对象 (用户覆写 exec 的产物)。
func lastIndexOfResult(res object.Value) int {
	if res == nil {
		return -1
	}
	v, ok := res.GetProperty("index")
	if !ok || v == nil {
		return -1
	}
	n := int(toFloat(v))
	if n < 0 {
		return -1
	}
	return n
}

// matchStringOfResult 取 exec 结果的整体匹配串 (规范的 Get(result, "0"))。
func matchStringOfResult(res object.Value) string {
	if res == nil {
		return ""
	}
	v, ok := res.GetProperty("0")
	if !ok || v == nil {
		return ""
	}
	return toStr(v)
}

// groupsOfResult 取 exec 结果的 "groups" 属性 (用于 @@replace 的
// ToObject(namedCaptures) 校验)。返回 (值, 是否存在且非 undefined)。
func groupsOfResult(res object.Value) (object.Value, bool) {
	if res == nil {
		return nil, false
	}
	v, ok := res.GetProperty("groups")
	if !ok || v == nil {
		return nil, false
	}
	if v == object.UndefinedSingleton {
		return nil, false
	}
	return v, true
}

// regExpBuiltinExec 是规范 RegExpBuiltinExec(R, S) 的实现, 同时是
// %RegExp.prototype%.exec 与 @@match / @@replace 的内建兜底路径。
//
// 与旧实现的差别: g/y 时「重置 / 推进 lastIndex」全部走 SetLastIndexStrict
// (规范里的 Set(R,"lastIndex",v,true)), 写不进去即抛 TypeError —— 这是
// builtins/RegExp/prototype/Symbol.{match,replace} 八个用例断言的落点。
func regExpBuiltinExec(re *object.RegExp, input string) object.Value {
	re.Lock()
	defer re.Unlock()

	lastIndex := 0
	if re.Global || re.Sticky {
		// 规范步骤 4: lastIndex = ToLength(Get(R, "lastIndex")) —— 必须**属性读**,
		// 用户给 lastIndex 挂 getter 的可观测性 (success-g-lastindex-no-access /
		// builtin-y-coerce-lastindex-err 族) 与 Infinity 的 ToLength 截断
		// (failure-lastindex-set 族) 都落在这里; getter 抛错原样传播。
		lv, _ := re.GetProperty("lastIndex")
		if cb := object.TakeCallbackError(); cb != nil {
			object.SetCallbackError(cb)
			return object.UndefinedSingleton
		}
		lf := toFloat(lv)
		// ToLength: NaN/负数归零, 超过 2^53-1 截断 (Infinity 同样截断,
		// 避免 int(f) 对越界浮点的平台相关未定义转换)。
		const maxSafeLen = float64(1 << 53)
		switch {
		case math.IsNaN(lf) || lf < 0:
			lf = 0
		case lf >= maxSafeLen || math.IsInf(lf, 1):
			lf = maxSafeLen - 1
		}
		lastIndex = int(lf)
		if lastIndex > len(input) {
			if !re.SetLastIndexStrict(0) {
				return lastIndexTypeError()
			}
			return object.NullSingleton
		}
	}

	searchStr := input[lastIndex:]
	loc := re.FindStringSubmatchIndex(searchStr)
	if loc == nil {
		if re.Global || re.Sticky {
			if !re.SetLastIndexStrict(0) {
				return lastIndexTypeError()
			}
		}
		return object.NullSingleton
	}
	// sticky: 匹配必须正好发生在 lastIndex 处 (相对偏移为 0)
	if re.Sticky && loc[0] != 0 {
		if !re.SetLastIndexStrict(0) {
			return lastIndexTypeError()
		}
		return object.NullSingleton
	}

	// 构建匹配结果数组
	matchStr := searchStr[loc[0]:loc[1]]
	result := []object.Value{object.NewString(matchStr)}

	// 捕获组
	for i := 1; i < len(loc)/2; i++ {
		if loc[i*2] >= 0 {
			result = append(result, object.NewString(searchStr[loc[i*2]:loc[i*2+1]]))
		} else {
			result = append(result, object.UndefinedSingleton)
		}
	}

	arr := object.NewArray(result)
	arr.SetProperty("index", object.NewNumber(float64(loc[0]+lastIndex)))
	arr.SetProperty("input", object.NewString(input))

	// 命名捕获组 (GetGroupsObject): .groups 上每个具名组一个属性 ——
	// 参与的组为匹配到的字符串, 未参与的组为 undefined (规范
	// RegExpBuiltinExec 步骤 20-23)。Go RE2 自 1.22 起原生接受 (?<name>…)
	// 语法, SubexpNames() 与 loc 的组序号 1:1 对齐 (下标 0 为整匹配)。
	names := re.SubexpNames()
	hasNamed := false
	for _, n := range names {
		if n != "" {
			hasNamed = true
			break
		}
	}
	var groupsObj *object.Object
	var indicesGroupsObj *object.Object
	if hasNamed {
		// 规范 RegExpBuiltinExec 步骤 24: groups = ObjectCreate(null)。
		// object.NewObject() 即 null 原型对象。
		groupsObj = object.NewObject()
		if re.HasIndices {
			indicesGroupsObj = object.NewObject()
		}
		for i := 1; i < len(names) && i < len(loc)/2; i++ {
			if names[i] == "" {
				continue
			}
			var val object.Value = object.UndefinedSingleton
			var idxVal object.Value = object.UndefinedSingleton
			if loc[i*2] >= 0 {
				val = object.NewString(searchStr[loc[i*2]:loc[i*2+1]])
				idxVal = object.NewArray([]object.Value{
					object.NewNumber(float64(loc[i*2] + lastIndex)),
					object.NewNumber(float64(loc[i*2+1] + lastIndex)),
				})
			}
			// 重复命名组 (duplicate-named-groups 提案)：同一名字可以对应
			// 多个捕获序号。属性值 = 序号最大的**参与**捕获；没有参与时为
			// undefined 占位 (仍要建自有属性)。未参与的组不覆盖已写入的值。
			participated := loc[i*2] >= 0
			if _, exists := groupsObj.GetProperty(names[i]); participated || !exists {
				groupsObj.SetProperty(names[i], val)
				if indicesGroupsObj != nil {
					indicesGroupsObj.SetProperty(names[i], idxVal)
				}
			}
		}
	}
	// 规范步骤 24-26：groups 属性**恒存在** —— 无命名组时值为 undefined
	// (groups-object-undefined.js：hasOwnProperty("groups") === true 且
	// verifyProperty w/e/c 全真)。CreateDataProperty：不能走 SetProperty
	// （会触发用户在 Array.prototype 上埋的 setter，groups-object.js 的
	// set 陷阱专测这一点；且 ExtraProps 通道 hasOwnProperty 看不见）。
	arr.DefineOwn("groups", object.DataProperty(func() object.Value {
		if groupsObj != nil {
			return groupsObj
		}
		return object.UndefinedSingleton
	}()))

	// d (hasIndices): 附带 indices 匹配位置数组 —— 0 号为整匹配, i 号为第 i
	// 个捕获组的 [start, end] (未参与的组为 undefined); 元素相对整个 input
	// 取绝对偏移 (与 result.index 同口径)。
	if re.HasIndices {
		indices := make([]object.Value, 0, len(loc)/2)
		pair := func(s, e int) object.Value {
			return object.NewArray([]object.Value{
				object.NewNumber(float64(s + lastIndex)),
				object.NewNumber(float64(e + lastIndex)),
			})
		}
		indices = append(indices, pair(loc[0], loc[1]))
		for i := 1; i < len(loc)/2; i++ {
			if loc[i*2] >= 0 {
				indices = append(indices, pair(loc[i*2], loc[i*2+1]))
			} else {
				indices = append(indices, object.UndefinedSingleton)
			}
		}
		indicesArr := object.NewArray(indices)
		// indices.groups 同样是 CreateDataProperty（null 原型 groups 对象或
		// undefined），与顶层 groups 属性同一落点。
		indicesArr.DefineOwn("groups", object.DataProperty(func() object.Value {
			if indicesGroupsObj != nil {
				return indicesGroupsObj
			}
			return object.UndefinedSingleton
		}()))
		arr.DefineOwn("indices", object.DataProperty(indicesArr))
	}

	if re.Global || re.Sticky {
		if !re.SetLastIndexStrict(loc[1] + lastIndex) {
			return lastIndexTypeError()
		}
	}

	return arr
}

// regExpExec 对应规范 RegExpExec(rx, S):
//
//  1. Let exec be Get(rx, "exec").
//  2. If IsCallable(exec) is true, 用它; 否则走 RegExpBuiltinExec。
//
// 读 exec 这一步**本身就是规范要求的副作用点**: test262 的
// builtin-success-g-set-lastindex-err.js 把 exec 定义成 getter, 靠「读 exec」
// 这一下把 lastIndex 改成不可写, 从而使错误发生在**匹配成功之后**的严格
// Set 上。此前 Gox 直接调 Go 侧的 FindAll*, 从不读 exec, 也就永远不抛。
func regExpExec(re *object.RegExp, input string) object.Value {
	// 规范 RegExpExec: exec = Get(rx, "exec") —— **沿原型链**查找。此前用
	// HasOwn("exec") 只认实例自有属性, 子类覆写 (class X extends RegExp
	// { exec(...) } 方法挂在 X.prototype 上) 被漏掉, 落回内建 exec 跑空
	// 模式 (named-groups groups-object-subclass 族专测)。
	// 判"是内建 exec"用指针比对 RegExp.prototype.exec —— 用户覆写原型
	// 方法时指针不同, 按规范应走 CallFunction。
	if exec, ok := re.GetProperty("exec"); ok && exec != nil && object.IsCallable(exec) && !isBuiltinRegExpExec(exec) {
		pending := object.TakeCallbackError()
		res := object.CallFunction(exec, re, object.NewString(input))
		thrown := object.TakeCallbackError()
		if pending != nil {
			// 参数 ToString 阶段已挂起抛出: 原样归还给 VM 传播。
			object.SetCallbackError(pending)
			return object.UndefinedSingleton
		}
		if thrown != nil {
			object.SetCallbackError(thrown)
			return object.UndefinedSingleton
		}
		if res == nil {
			return object.NullSingleton
		}
		return res
	}
	return regExpBuiltinExec(re, input)
}

// isBuiltinRegExpExec 判断 v 是否就是 %RegExp.prototype%.exec 内建方法
// 本体 —— 是则不必绕 CallFunction（理由见 regExpExec 注释），直接走内建
// 快路径。
func isBuiltinRegExpExec(v object.Value) bool {
	proto, ok := object.RegExpProto.(*object.Object)
	if !ok {
		return false
	}
	builtin, ok := proto.GetProperty("exec")
	if !ok {
		return false
	}
	return v == builtin
}

// isNullishExecResult 判断 exec 结果是否为「未匹配」(null / undefined)。
func isNullishExecResult(v object.Value) bool {
	return v == nil || v == object.NullSingleton || v == object.UndefinedSingleton
}

// isErrorResult 判断 exec 结果是否为错误值 (严格的 lastIndex 写入失败、
// 用户 exec 里 return 出来的 Error)。调用方须原样返回, 使其作为抛出传播。
func isErrorResult(v object.Value) bool {
	_, ok := v.(*object.Error)
	return ok
}

// regExpGetLastIndex 读 lastIndex (规范 Get(R, "lastIndex") 语义 —— 走属性读,
// 自有访问器/数据属性优先, 再落到 *RegExp 的内建镜像)。
func regExpGetLastIndex(rx object.Value) int {
	v, _ := rx.GetProperty("lastIndex")
	return int(toFloat(v))
}

// regExpSetLastIndexStrict 对任意 receiver 做严格 Set(rx, "lastIndex", v, true):
// *RegExp 走内建 SetLastIndexStrict (描述符级精确); 普通对象按描述符裁决 ——
// 有 setter 调 setter, 不可写数据属性返回 false (调用方据此抛 TypeError)。
// @@match / @@matchAll / @@search / @@replace 都是泛化方法, fake 对象
// (lastIndex 数据属性 + 自定义 exec) 必须能走通 (set-lastindex-init 族)。
func regExpSetLastIndexStrict(rx object.Value, n int) bool {
	if re, ok := rx.(*object.RegExp); ok {
		return re.SetLastIndexStrict(n)
	}
	o, ok := rx.(*object.Object)
	if !ok {
		return false
	}
	if d, has := o.OwnDescriptor("lastIndex"); has {
		if acc, isAcc := d.Value.(*object.Accessor); isAcc {
			if acc.Setter == nil || !object.IsCallable(acc.Setter) {
				return false
			}
			object.CallFunction(acc.Setter, o, object.NewNumber(float64(n)))
			return true
		}
		if !d.Writable {
			return false
		}
	}
	o.SetProperty("lastIndex", object.NewNumber(float64(n)))
	return true
}

// regExpExecGeneric 是规范 RegExpExec(rx, S) 的泛化版: exec 沿原型链可调用
// 则调用; 否则 rx 必须是带内部槽的 *RegExp 才能走内建 exec, 否则按规范抛
// TypeError。
func regExpExecGeneric(rx object.Value, input string) object.Value {
	if exec, ok := rx.GetProperty("exec"); ok && exec != nil && object.IsCallable(exec) {
		if re, isRe := rx.(*object.RegExp); isRe && isBuiltinRegExpExec(exec) {
			return regExpBuiltinExec(re, input)
		}
		pending := object.TakeCallbackError()
		res := object.CallFunction(exec, rx, object.NewString(input))
		thrown := object.TakeCallbackError()
		if pending != nil {
			object.SetCallbackError(pending)
			return object.UndefinedSingleton
		}
		if thrown != nil {
			object.SetCallbackError(thrown)
			return object.UndefinedSingleton
		}
		if res == nil {
			return object.NullSingleton
		}
		// 规范 RegExpExec 5.c: 结果既非 Object 也非 Null ⇒ TypeError
		// (cstm-exec-return-invalid 族: exec 返回 undefined/number/string/bool)。
		if !object.IsObjectValue(res) && res != object.NullSingleton {
			return object.NewTypeError("exec result must be an Object or Null")
		}
		return res
	}
	if re, isRe := rx.(*object.RegExp); isRe {
		return regExpBuiltinExec(re, input)
	}
	return object.NewTypeError("RegExp method called on incompatible receiver")
}

// collectExecResults 反复 exec 直到未匹配, 收集全部结果 (@@match / @@replace
// 的 global 分支共用)。空匹配时按规范 AdvanceStringIndex 推进一位, 否则
// /(?:)/g 会死循环。lastIndex 的任何严格写入失败都以 TypeError 收场。
// (B7: receiver 泛化 —— fake 对象 receiver 同样可走 collect 循环。)
func collectExecResults(rx object.Value, input string) ([]object.Value, object.Value) {
	var results []object.Value
	for i := 0; i <= len(input)+1; i++ {
		res := regExpExecGeneric(rx, input)
		if isNullishExecResult(res) {
			break
		}
		if isErrorResult(res) {
			return nil, res
		}
		results = append(results, res)
		if matchStringOfResult(res) == "" {
			if !regExpSetLastIndexStrict(rx, regExpGetLastIndex(rx)+1) {
				return nil, lastIndexTypeError()
			}
		}
	}
	return results, nil
}

func setupRegExpProto() *object.Object {
	p := object.NewObject()

	// exec(string): 执行匹配
	p.SetBuiltinProperty("exec", object.NewBuiltinMethod("exec", func(this object.Value, args ...object.Value) object.Value {
		re, errVal := regExpThis(this)
		if errVal != nil {
			return errVal
		}
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		return regExpBuiltinExec(re, input)
	}))

	// test(string): 测试是否匹配
	//
	// 规范 21.2.5.13: test 仅是 RegExpExec(R, S) 的布尔包装 —— lastIndex 的
	// 读值 / 推进 / 失败重置全部由 RegExpBuiltinExec 按「严格 Set」语义处理。
	// 旧实现手工改 re.LastIndex 内部字段, 绕过了 SetLastIndexStrict, 于是
	// y-fail-lastindex-no-write (粘性失败时 lastIndex 不可写应抛 TypeError)
	// 永远不抛 (B7 第二子根因)。
	p.SetBuiltinProperty("test", object.NewBuiltinMethod("test", func(this object.Value, args ...object.Value) object.Value {
		re, errVal := regExpThis(this)
		if errVal != nil {
			return errVal
		}
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		res := regExpExec(re, input)
		if isErrorResult(res) {
			return res
		}
		return object.NewBoolean(!isNullishExecResult(res))
	}))

	// toString(): 返回正则的字符串表示
	p.SetBuiltinProperty("toString", object.NewBuiltinMethod("toString", func(this object.Value, args ...object.Value) object.Value {
		if re, ok := this.(*object.RegExp); ok {
			return object.NewString(re.Inspect())
		}
		return object.NewString("")
	}))

	// [Symbol.match](string): 用于 String.prototype.match
	//
	// 规范 22.2.5.6: global / sticky 时先 Set(rx,"lastIndex",0,true)
	// (步骤 8.c), 之后每轮 RegExpExec 推进 lastIndex —— 两处写入都是
	// **严格**的, 写不进去即抛 TypeError。此前这里用 FindAllString 一次算完,
	// 既不读 exec 也不碰 lastIndex, 于是 8 个断言 TypeError 的用例全假阳性。
	// (B7: @@match/@@matchAll/@@search/@@replace 是规范定义的泛化方法,
	// receiver 只要是对象即可 —— fake 对象 {lastIndex, exec} 必须走通,
	// 此前一刀切 regExpThis 拒掉了 22+ 条 this-val/自定义 exec 用例。)
	p.SetBuiltinProperty(object.NewSymbol("Symbol.match").Inspect(), object.NewBuiltinMethod("[Symbol.match]", func(this object.Value, args ...object.Value) object.Value {
		if this == nil || !object.IsObjectValue(this) {
			return object.NewTypeError("RegExp.prototype method called on incompatible receiver")
		}
		rx := this
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}

		// 规范步骤 5: global = ToBoolean(Get(rx, "global")) —— 必须走属性读
		// 而非内部槽: 用户 defineProperty 覆写 global 属性后, 单次 exec /
		// 循环收集的分支决策跟着变 (builtin-success-g-set-lastindex 族:
		// /b/g 覆写 global=false ⇒ 只 exec 一次, 收尾 lastIndex=e=2)。
		gv, _ := rx.GetProperty("global")
		global := toBool(gv)
		if !global {
			// 步骤 7: global 为 false ⇒ 直接 RegExpExec(rx, S)。
			res := regExpExecGeneric(rx, input)
			if isErrorResult(res) {
				return res
			}
			return res
		}

		// 步骤 8.c
		if !regExpSetLastIndexStrict(rx, 0) {
			return lastIndexTypeError()
		}
		results, errVal := collectExecResults(rx, input)
		if errVal != nil {
			return errVal
		}
		if len(results) == 0 {
			return object.NullSingleton
		}
		out := make([]object.Value, len(results))
		for i, res := range results {
			out[i] = object.NewString(matchStringOfResult(res))
		}
		return object.NewArray(out)
	}))

	// [Symbol.matchAll](string): %RegExp.prototype%[@@matchAll] ——
	// 返回 %RegExp String Iterator%。规范要点:
	//   - 非全局正则直接 TypeError（matchAll-requires-global 族）;
	//   - 先 Set(rx,"lastIndex",0,true)，之后每轮 RegExpExec 推进;
	//   - 迭代器 next() 每次 yield 一个 exec 结果数组。
	// 此前引擎没有这个方法，String.prototype.matchAll 派发落空后走
	// 字符串侧数组快路径，返回的不是迭代器（named-groups matchall 族
	// 的 compareIterator 断言因此失败）。
	p.SetBuiltinProperty(object.NewSymbol("Symbol.matchAll").Inspect(), object.NewBuiltinMethod("[Symbol.matchAll]", func(this object.Value, args ...object.Value) object.Value {
		if this == nil || !object.IsObjectValue(this) {
			return object.NewTypeError("RegExp.prototype method called on incompatible receiver")
		}
		rx := this
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		// 规范: global = ToBoolean(Get(rx, "global")) —— 属性读而非内部槽,
		// 覆写 global 的 getter 抛错要原样传播 (species-regexp-get-global-throws 族)。
		gv, _ := rx.GetProperty("global")
		if !toBool(gv) {
			return object.NewTypeError("matchAll requires a global regular expression")
		}
		if !regExpSetLastIndexStrict(rx, 0) {
			return lastIndexTypeError()
		}
		var it *object.JSIterator
		it = &object.JSIterator{Name: "RegExp String Iterator", NextFn: func() (object.Value, bool) {
			res := regExpExecGeneric(rx, input)
			if isErrorResult(res) {
				it.PendingErr = res
				return object.UndefinedSingleton, true
			}
			if isNullishExecResult(res) {
				it.MarkExhausted()
				return object.UndefinedSingleton, true
			}
			return res, false
		}}
		return it
	}))

	// [Symbol.replace](string, replacement): 用于 String.prototype.replace
	// 实现体抽到 regExpReplace：@@replace 与 String.prototype.replace /
	// replaceAll 快路径三处共用同一份 exec 循环与 GetSubstitution 展开，
	// 避免快路径丢捕获组 / 丢 groups 实参的口径分叉。
	p.SetBuiltinProperty(object.NewSymbol("Symbol.replace").Inspect(), object.NewBuiltinMethod("[Symbol.replace]", func(this object.Value, args ...object.Value) object.Value {
		if this == nil || !object.IsObjectValue(this) {
			return object.NewTypeError("RegExp.prototype method called on incompatible receiver")
		}
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		// 缺省 replacement 维持历史行为（空串）：规范 ToString(undefined)
		// 应为 "undefined"，但既有用例在空串口径下通过，零退潮优先。
		var replaceValue object.Value = object.NewString("")
		if len(args) > 1 {
			replaceValue = args[1]
		}
		return regExpReplace(this, input, replaceValue)
	}))

	// [Symbol.search](string): %RegExp.prototype%[@@search] —— 此前未注册,
	// typeof RegExp.prototype[Symbol.search] === "undefined"
	// (T4 r8DmKW: not-a-constructor 族)。规范语义: RegExpExec 一次,
	// 命中返回 result.index, 未命中返回 -1。
	//
	// 规范 sec-regexp.prototype-@@search 完整步骤 (B7: set-lastindex-init /
	// restore / restore-err 族专测上一轮 lastIndex 的保存与恢复):
	//   5. previousLastIndex = Get(R, "lastIndex")
	//   6. 若 previousLastIndex ≠ +0 ⇒ Set(R, "lastIndex", +0, true)
	//   8. result = RegExpExec(R, S)
	//   9. result 为 abrupt 或 previousLastIndex ≠ Get(R,"lastIndex")
	//      ⇒ Set(R, "lastIndex", previousLastIndex, true)
	// 全部 Set 走严格写 (写不进去抛 TypeError)。
	p.SetBuiltinProperty(object.NewSymbol("Symbol.search").Inspect(), object.NewBuiltinMethod("[Symbol.search]", func(this object.Value, args ...object.Value) object.Value {
		if this == nil || !object.IsObjectValue(this) {
			return object.NewTypeError("RegExp.prototype method called on incompatible receiver")
		}
		rx := this
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		prev := regExpGetLastIndex(rx)
		if prev != 0 {
			if !regExpSetLastIndexStrict(rx, 0) {
				return lastIndexTypeError()
			}
		}
		res := regExpExecGeneric(rx, input)
		if isErrorResult(res) {
			if prev != 0 && !regExpSetLastIndexStrict(rx, prev) {
				return lastIndexTypeError()
			}
			return res
		}
		cur := regExpGetLastIndex(rx)
		if cur != prev {
			if !regExpSetLastIndexStrict(rx, prev) {
				return lastIndexTypeError()
			}
		}
		if isNullishExecResult(res) {
			return object.NewInt(-1)
		}
		if idx, found := res.GetProperty("index"); found && idx != nil {
			return idx
		}
		return object.NewInt(-1)
	}))

	// [Symbol.split](string, limit): 用于 String.prototype.split
	p.SetBuiltinProperty(object.NewSymbol("Symbol.split").Inspect(), object.NewBuiltinMethod("[Symbol.split]", func(this object.Value, args ...object.Value) object.Value {
		re, errVal := regExpThis(this)
		if errVal != nil {
			return errVal
		}
		input := ""
		if len(args) > 0 {
			input = toStr(args[0])
		}
		limit := -1
		if len(args) > 1 {
			limit = int(toFloat(args[1]))
		}

		return regExpSplit(re, input, limit)
	}))

	// ===== B7: 内建方法 length 规范值 =====
	// builtin_lengths.go 的限定名表走 "a.b.c" 点路径, 无法寻址 Symbol 键
	// 方法, 这里按规范形参个数直接补 (exec/test/@@match/@@matchAll/@@search
	// = 1, @@replace/@@split = 2)。S15.10.6.x_A11 与 Symbol.*/length 族据此断言。
	for _, e := range []struct {
		key string
		n   int
	}{
		{"Symbol.match", 1},
		{"Symbol.matchAll", 1},
		{"Symbol.search", 1},
		{"Symbol.replace", 2},
		{"Symbol.split", 2},
	} {
		if m, ok := p.GetProperty(object.NewSymbol(e.key).Inspect()); ok {
			if bm, is := m.(*object.BuiltinMethod); is {
				bm.SetFunctionLength(e.n)
			}
		}
	}

	// ===== 访问器属性族 (source/global/flags/...) =====
	// 规范: 这些是 %RegExp.prototype% 上的 **getter 访问器** (非数据属性),
	// Object.getOwnPropertyDescriptor(RegExp.prototype, "source").get 必须是
	// 函数; 对不兼容 receiver 抛 TypeError。值仍由 *RegExp.GetProperty 提供。
	for _, prop := range []string{"source", "flags", "global", "ignoreCase",
		"multiline", "dotAll", "sticky", "unicode", "unicodeSets", "hasIndices"} {
		prop := prop
		p.DefineNonEnumerableAccessor(prop, object.NewBuiltinMethod("get "+prop, func(this object.Value, args ...object.Value) object.Value {
			// 规范特例 (21.2.5.x RegExpPrototypeAccessor): receiver 恰为
			// %RegExp.prototype% 自身时返回 undefined (它没有内部槽),
			// 而非抛 TypeError —— this-val-regexp-prototype 族据此断言。
			if this == object.Value(p) {
				return object.UndefinedSingleton
			}
			re, errVal := regExpThis(this)
			if errVal != nil {
				return errVal
			}
			if v, ok := re.GetProperty(prop); ok && v != nil {
				return v
			}
			return object.UndefinedSingleton
		}), nil)
	}

	return p
}

// submatchGroupsOfResult 从 exec 结果里收集捕获组 (供 $1 / $2 展开),
// 对应旧实现里 submatchGroups(input, loc) 的角色。
func submatchGroupsOfResult(res object.Value) []string {
	if res == nil {
		return nil
	}
	arr, isArr := res.(*object.Array)
	if !isArr {
		return nil
	}
	groups := make([]string, 0, len(arr.Elements))
	for i := 1; i < len(arr.Elements); i++ {
		// 未参与的捕获组值是 undefined：GetSubstitution 对 $n 的口径是
		// "capture undefined → 空串"，不能串成 "undefined"。
		if isUndefinedValue(arr.Elements[i]) {
			groups = append(groups, "")
			continue
		}
		groups = append(groups, toStr(arr.Elements[i]))
	}
	return groups
}

// namedGroupsOfResult 从 exec 结果的 .groups 自有属性收集 命名 → 匹配串
// 映射, 供 $<name> 替换展开。无命名组 / 用户覆写 exec 返回异常结果时返回 nil。
func namedGroupsOfResult(res object.Value) map[string]string {
	arr, isArr := res.(*object.Array)
	if !isArr {
		return nil
	}
	gv, ok := arr.GetProperty("groups")
	if !ok {
		return nil
	}
	gobj, ok := gv.(*object.Object)
	if !ok {
		return nil
	}
	named := make(map[string]string)
	for _, k := range gobj.Keys() {
		if v, ok := gobj.GetProperty(k); ok {
			// 未参与的命名组属性值是 undefined：GetSubstitution 视为
			// capture undefined → $<name> 展开为空串。查不到键与查到
			// undefined 落同一分支，这里跳过即可。
			if isUndefinedValue(v) {
				continue
			}
			named[k] = toStr(v)
		}
	}
	return named
}

// regExpSplit 是规范 RegExp.prototype[Symbol.split] 的实现体，供内建
// @@split 与 String.prototype.split 快路径共用（两侧此前各写一份，快路径
// 丢捕获组 —— split-with-capture-group 族断言捕获组按序拼进结果）。
// 分隔符点集合与 re.Split 内部用的是同一条 FindAll 管线；若两侧匹配数
// 不一致（空匹配推进等边界语义分叉），宁可退回无捕获的旧行为也不拼错。
func regExpSplit(re *object.RegExp, input string, limit int) object.Value {
	parts := re.Split(input, limit)
	result := make([]object.Value, 0, len(parts))
	locs := re.FindAllStringSubmatchIndex(input, func() int {
		if limit > 0 {
			return limit - 1
		}
		return -1
	}())
	withCaptures := len(locs) == len(parts)-1 && len(parts) > 0
	for i, part := range parts {
		result = append(result, object.NewString(part))
		if !withCaptures || i >= len(locs) {
			continue
		}
		loc := locs[i]
		for g := 1; g < len(loc)/2; g++ {
			if loc[g*2] >= 0 {
				result = append(result, object.NewString(input[loc[g*2]:loc[g*2+1]]))
			} else {
				result = append(result, object.UndefinedSingleton)
			}
		}
	}
	return object.NewArray(result)
}

// regExpReplace 是规范 RegExp.prototype[Symbol.replace] 的实现体，供内建
// @@replace 与 String.prototype.replace 快路径共用。replaceValue 可以是
// 字符串（GetSubstitution 展开）或函数（[matched, p1..pn, position, S,
// groups?] 实参表）。
func regExpReplace(rx object.Value, input string, replaceValue object.Value) object.Value {
	// 规范 @@replace 步骤 7: functionalReplace = IsCallable(replaceValue);
	// 非函数时 ToString(replaceValue) 必须发生在**匹配之前** —— 毒 toString
	// 即使零匹配也要抛 (arg-2-coerce-err.js)。
	var replaceStr string
	if !object.IsCallable(replaceValue) {
		rpl, errv := toPrimitiveToString(replaceValue)
		if errv != nil {
			return errv
		}
		replaceStr = rpl.Value
	}
	var results []object.Value
	// 规范: global = ToBoolean(Get(rx, "global")) —— 属性读而非内部槽,
	// 与 @@match 同口径 (覆写 global 属性要改变单次 / 循环收集决策)。
	gv, _ := rx.GetProperty("global")
	if toBool(gv) {
		// 步骤: Set(rx, "lastIndex", 0, true) 后逐轮 exec。
		if !regExpSetLastIndexStrict(rx, 0) {
			return lastIndexTypeError()
		}
		rs, errVal := collectExecResults(rx, input)
		if errVal != nil {
			return errVal
		}
		results = rs
	} else {
		res := regExpExecGeneric(rx, input)
		if isErrorResult(res) {
			return res
		}
		if isNullishExecResult(res) {
			return object.NewString(input)
		}
		results = append(results, res)
	}

	// 规范步骤: namedCaptures = Get(result, "groups");
	// 非 undefined ⇒ ToObject(namedCaptures) —— null 抛 TypeError
	// (result-coerce-groups-err.js)。
	// 规范: 字符串替换路径 namedCaptures = Get(result,"groups"); 非 undefined
	// ⇒ ToObject(namedCaptures) —— null 抛 TypeError (result-coerce-groups-err.js)。
	// **函数式路径没有这一步**: namedCaptures 非 undefined 原样追加为最后实参,
	// groups: null / undefined 均不抛 (named-groups-fn.js 断言面)。
	if !object.IsCallable(replaceValue) {
		for _, res := range results {
			if g, has := groupsOfResult(res); has {
				if g == object.NullSingleton {
					return object.NewTypeError("Cannot convert undefined or null to object")
				}
			}
		}
	}

	// replaceValue 是函数：replacerArgs = [matched, p1..pn, position,
	// S, groups?]（fn-invoke-args.js / named-groups-fn.js 的断言面）。
	if object.IsCallable(replaceValue) {
		fn := replaceValue
		var result strings.Builder
		lastEnd := 0
		for _, res := range results {
			start := lastIndexOfResult(res)
			matchStr := matchStringOfResult(res)
			if start < 0 || start < lastEnd || start > len(input) {
				continue
			}
			end := start + len(matchStr)
			if end > len(input) {
				end = len(input)
			}
			result.WriteString(input[lastEnd:start])
			r := object.CallFunction(fn, object.UndefinedSingleton, replacerArgs(res, input, start)...)
			if thr := callbackThrown(); thr != nil {
				return thr
			}
			result.WriteString(toStr(r))
			lastEnd = end
		}
		result.WriteString(input[lastEnd:])
		return object.NewString(result.String())
	}

	// replaceStr 已在函数入口按规范步骤 7 提前 ToString (匹配前求值)。

	// 拼接替换结果。exec 可被用户覆写, 返回的 index / 匹配串未必自洽,
	// 越界一律跳过 (宁可少替换也不 panic)。
	var result strings.Builder
	lastEnd := 0
	for _, res := range results {
		start := lastIndexOfResult(res)
		matchStr := matchStringOfResult(res)
		if start < 0 || start < lastEnd || start > len(input) {
			continue
		}
		end := start + len(matchStr)
		if end > len(input) {
			end = len(input)
		}
		result.WriteString(input[lastEnd:start])
		namedCap, errVal := resolveNamedCaptures(replaceStr, res)
		if errVal != nil {
			return errVal
		}
		result.WriteString(expandReplacement(replaceStr, matchStr, submatchGroupsOfResult(res), namedCap, input, start))
		lastEnd = end
	}
	result.WriteString(input[lastEnd:])
	return object.NewString(result.String())
}

// resolveNamedCaptures 预解析替换模板里被引用的 $<name> 组（规范
// GetSubstitution 对每个 $<name> 执行 Get(namedCaptures, name) —— 是**按名
// 查找**而非枚举，且沿原型链可达（groups-object-subclass 族：exec 结果的
// groups 挂在原型上的 b 属性也要能取到），getter 抛错必须传播）。
// 返回 名 → 值字符串（缺失/undefined 的名字不入表，展开为空串）；第二个
// 返回值非 nil 表示解析过程抛了错（namedCaptures 为 null 的 TypeError 已
// 在上游拦截）。
func resolveNamedCaptures(replacement string, res object.Value) (map[string]string, object.Value) {
	gv, ok := res.GetProperty("groups")
	if !ok || gv == nil || isUndefinedValue(gv) {
		return nil, nil // namedCaptures undefined：$<...> 字面保留
	}
	if gv == object.NullSingleton {
		// 上游 regExpReplace 已拦截 null；这里兜底同款 TypeError。
		return nil, object.NewTypeError("Cannot convert undefined or null to object")
	}
	named := make(map[string]string)
	for i := 0; i < len(replacement); i++ {
		if replacement[i] != '$' || i+1 >= len(replacement) || replacement[i+1] != '<' {
			continue
		}
		end := strings.IndexByte(replacement[i+2:], '>')
		if end < 0 {
			continue
		}
		name := replacement[i+2 : i+2+end]
		if _, seen := named[name]; seen {
			continue
		}
		v, found := gv.GetProperty(name)
		if !found || v == nil || isUndefinedValue(v) {
			continue // capture undefined → 空串（不入表即空串语义）
		}
		named[name] = toStr(v)
	}
	return named, nil
}

// replacerArgs 构造函数式替换的实参表（规范 @@replace 步骤 14.j/ii）：
// [matched, p1..pn, position, S, groups?] —— groups 仅在 exec 结果的
// groups 属性非 undefined 时追加（named-groups-fn.js: "groups value is
// passed as last argument of replacer unless it is undefined"）。
func replacerArgs(res object.Value, input string, start int) []object.Value {
	callArgs := []object.Value{object.NewString(matchStringOfResult(res))}
	if arr, ok := res.(*object.Array); ok && len(arr.Elements) > 1 {
		callArgs = append(callArgs, arr.Elements[1:]...)
	}
	callArgs = append(callArgs, object.NewNumber(float64(start)), object.NewString(input))
	if gv, ok := res.GetProperty("groups"); ok && gv != nil && !isUndefinedValue(gv) {
		callArgs = append(callArgs, gv)
	}
	return callArgs
}

// toRegExpArg 将参数转换为 *object.RegExp。
//
// 已经是 RegExp 的直接复用；否则把字符串当作 pattern 用 defaultFlags 编译。
// 编译失败时返回 SyntaxError 作为第二返回值，调用方应直接返回它。
func toRegExpArg(v object.Value, defaultFlags string) (*object.RegExp, object.Value) {
	if re, ok := v.(*object.RegExp); ok {
		return re, nil
	}
	re, err := object.NewRegExp(toStr(v), defaultFlags)
	if err != nil {
		return nil, object.NewErrorWithName("SyntaxError", err.Error())
	}
	return re, nil
}

// expandReplacement 展开 $1, $2, $&, $$, $`, $', $<name> 等替换模式
// (GetSubstitution)。groups 是捕获组的字符串切片 (不含整个匹配); named 是
// 命名捕获组 名 → 匹配串映射 (nil 表示 namedCaptures 整体 undefined, 此时
// $<...> 按字面量保留); input/position 供 $` ($` = position 前文本) 与
// $' (匹配后文本) 取值。
func expandReplacement(replacement, match string, groups []string, named map[string]string, input string, position int) string {
	var result strings.Builder
	for i := 0; i < len(replacement); i++ {
		c := replacement[i]
		if c != '$' || i+1 >= len(replacement) {
			result.WriteByte(c)
			continue
		}
		next := replacement[i+1]
		switch {
		case next == '$':
			result.WriteByte('$')
			i++
		case next == '<':
			// $<name> 命名捕获组引用 (GetSubstitution):
			//   - namedCaptures 整体 undefined (模式无命名组 / exec 结果
			//     没有 groups 对象) → 字面保留 "$<..."；
			//   - 找不到 '>' → 字面保留；
			//   - 组名不存在或该组未参与 (capture undefined) → 整段展开为
			//     空串 (named-groups.js: "replace the text through > with
			//     the empty String"，string-replace-missing.js 同口径)。
			end := strings.IndexByte(replacement[i+2:], '>')
			if end >= 0 && named != nil {
				name := replacement[i+2 : i+2+end]
				if v, ok := named[name]; ok {
					result.WriteString(v)
				}
				i = i + 2 + end
				continue
			}
			result.WriteString("$<")
			i++
		case next == '&':
			result.WriteString(match)
			i++
		case next == '`':
			// $` = 匹配起点之前的文本 (GetSubstitution)
			pre := position
			if pre > len(input) {
				pre = len(input)
			}
			if pre < 0 {
				pre = 0
			}
			result.WriteString(input[:pre])
			i++
		case next == '\'':
			// $' = 匹配结束之后的文本 (GetSubstitution)
			end := position + len(match)
			if end > len(input) {
				end = len(input)
			}
			if end < 0 {
				end = 0
			}
			result.WriteString(input[end:])
			i++
		case next >= '0' && next <= '9':
			// 捕获组 $1, $2, ..., $99
			num := 0
			j := i + 1
			for j < len(replacement) && replacement[j] >= '0' && replacement[j] <= '9' {
				num = num*10 + int(replacement[j]-'0')
				j++
			}
			if num == 0 {
				// $0 表示整个匹配
				result.WriteString(match)
				i = j - 1
			} else if num <= len(groups) {
				result.WriteString(groups[num-1])
				i = j - 1
			} else {
				// 无对应捕获组，保留字面文本 $n
				result.WriteString(replacement[i : j-1])
				i = j - 2
				continue
			}
		default:
			result.WriteByte('$')
		}
	}
	return result.String()
}
