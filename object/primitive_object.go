package object

import "strconv"

// PrimitiveObject 是 new Boolean(..) / new Number(..) / new String(..)
// 产生的原始值包装对象: 普通对象 + 内部槽 [[PrimitiveValue]]。
// String 包装还按规范暴露 length 与索引字符 (只读)。
type PrimitiveObject struct {
	Object
	Primitive Value
}

// NewPrimitiveObject 以指定原型构造包装对象。
func NewPrimitiveObject(prim Value, proto Value) *PrimitiveObject {
	o := &PrimitiveObject{Primitive: prim}
	o.Proto = proto
	o.Extensible = true
	return o
}

func (w *PrimitiveObject) GetProperty(name string) (Value, bool) {
	if s, ok := w.Primitive.(*String); ok {
		if name == "length" {
			return NewInt(int64(UTF16Len(s.Value))), true
		}
		if idx, err := strconv.Atoi(name); err == nil && idx >= 0 {
			if ch, ok := CharAtUTF16(s.Value, idx); ok {
				return NewString(ch), true
			}
		}
	}
	return w.Object.GetProperty(name)
}

// Type 返回包装对象对应的内置类型 (Boolean/Number/String/Symbol 包装),
// 而不是嵌入的 *Object 的 OBJECT_OBJ。
//
// 规范里 `Object(false) instanceof Boolean` 为真: 包装对象是 Boolean 的
// 实例。MatchBuiltinType 走 Type() 判定, 若返回 OBJECT_OBJ 则整族
// "applied to primitive" 用例 (callbackfn 的第三个参数 obj instanceof X)
// 全部失败。
func (w *PrimitiveObject) Type() ObjectType {
	switch w.Primitive.(type) {
	case *Boolean:
		return BOOLEAN_OBJ
	case *Number:
		return NUMBER_OBJ
	case *String:
		return STRING_OBJ
	case *Symbol:
		return SYMBOL_OBJ
	}
	return OBJECT_OBJ
}

// SetProperty 对 String 包装的 length/索引按规范静默拒绝 (不可写)，
// 其余属性走普通对象语义。
func (w *PrimitiveObject) SetProperty(name string, val Value) {
	if s, ok := w.Primitive.(*String); ok {
		if name == "length" {
			return
		}
		if idx, err := strconv.Atoi(name); err == nil && idx >= 0 && idx < UTF16Len(s.Value) {
			return
		}
	}
	w.Object.SetProperty(name, val)
}

// OwnDescriptor 让 String 包装的 length 与索引字符按规范 10.4.3.1 暴露:
// length 为 {writable:false, enumerable:false, configurable:false};
// 索引字符为 {writable:false, enumerable:true, configurable:false}
// (注意索引字符**可枚举** —— for-in / Object.keys 语义依赖这一点)。
//
// 不补这一步, 嵌入的 *Object 只知道 Properties 里的用户键:
// Object.getOwnPropertyDescriptor(Object('ab'), "length") 返回 undefined,
// hasOwnProperty("length") 为 false, 于是
// `Array.prototype.push.call(”)` 的 "length 不可写 ⇒ 抛 TypeError" 判定
// 失效 (throws-with-string-receiver 族)。
func (w *PrimitiveObject) OwnDescriptor(name string) (PropertyDescriptor, bool) {
	if s, ok := w.Primitive.(*String); ok {
		if name == "length" {
			return PropertyDescriptor{Value: NewInt(int64(UTF16Len(s.Value))), Writable: false, Enumerable: false, Configurable: false}, true
		}
		if idx, err := strconv.Atoi(name); err == nil && idx >= 0 {
			if ch, ok := CharAtUTF16(s.Value, idx); ok {
				return PropertyDescriptor{Value: NewString(ch), Writable: false, Enumerable: true, Configurable: false}, true
			}
		}
	}
	return w.Object.OwnDescriptor(name)
}

// HasOwn 让 String 包装的 length/索引在 hasOwnProperty 下可见。
func (w *PrimitiveObject) HasOwn(name string) bool {
	if _, ok := w.OwnDescriptor(name); ok {
		return true
	}
	return w.Object.HasOwn(name)
}

// OwnKeys 让 String 包装的键枚举包含虚拟索引键与 length
// (规范 StringExoticObjectOwnKeys: 整数索引升序在前, 其后用户字符串键,
// "length" 为普通字符串键)。不覆盖则走嵌入 *Object 的实现, 只能看到用户键
// —— Object.keys(new String) / ownKeys target-is-proxy 的 gOPD 转发因此漏键。
func (w *PrimitiveObject) OwnKeys() []string {
	if s, ok := w.Primitive.(*String); ok {
		n := UTF16Len(s.Value)
		keys := make([]string, 0, n+1)
		for i := 0; i < n; i++ {
			keys = append(keys, strconv.Itoa(i))
		}
		// 用户显式 defineProperty("length") 的键去重 (虚拟 length 恒在后)。
		for _, k := range w.Object.OwnKeys() {
			if k != "length" {
				keys = append(keys, k)
			}
		}
		keys = append(keys, "length")
		return keys
	}
	return w.Object.OwnKeys()
}

// EnumerableOwnKeys 只报可枚举键: 索引字符可枚举, length 不可枚举
// (Object.keys(new String("ab")) === ["0", "1"]); 用户 defineProperty 出的
// 可枚举键照列 —— 不能丢, Object.create({}, new String()) 依赖枚举用户键。
func (w *PrimitiveObject) EnumerableOwnKeys() []string {
	if s, ok := w.Primitive.(*String); ok {
		n := UTF16Len(s.Value)
		keys := make([]string, 0, n)
		for i := 0; i < n; i++ {
			keys = append(keys, strconv.Itoa(i))
		}
		return append(keys, w.Object.EnumerableOwnKeys()...)
	}
	return w.Object.EnumerableOwnKeys()
}

// UnwrapPrimitive 拆包: 包装对象返回内部原始值，其余原样返回。
func UnwrapPrimitive(v Value) Value {
	if w, ok := v.(*PrimitiveObject); ok {
		return w.Primitive
	}
	return v
}
