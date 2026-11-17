package object

// Boolean 表示 JavaScript 的布尔类型。
type Boolean struct {
	Value bool
}

func (b *Boolean) Type() ObjectType { return BOOLEAN_OBJ }
func (b *Boolean) Inspect() string {
	if b.Value {
		return "true"
	}
	return "false"
}

func (b *Boolean) IsTruthy() bool {
	return b.Value
}

func (b *Boolean) GetProperty(name string) (Value, bool) {
	// Boolean 原始值没有自有属性 (typeof / valueOf 等一律沿 %Boolean.prototype%
	// 查找)。此前直接返回 (nil,false) —— 于是 `true.toString()` /
	// `true.hasOwnProperty` 全部取不到。
	if BooleanProto != nil {
		return BooleanProto.GetProperty(name)
	}
	return nil, false
}

func (b *Boolean) SetProperty(name string, val Value) {
	// 原始类型不能设置属性
}

// BooleanProto 是 Boolean 原始值的原型对象 (Boolean.prototype)。
// 由 stdlib 包初始化时设置 —— Boolean 原始值没有自有属性，
// 属性访问 (含 @@toStringTag) 一律沿它查找。
var BooleanProto Value

// SetBooleanProto 设置全局布尔原型 (由 stdlib 调用)。
func SetBooleanProto(p Value) { BooleanProto = p }

// GetBooleanProto 返回全局布尔原型。
func GetBooleanProto() Value { return BooleanProto }

// NewBoolean 返回布尔原始值对应的对象。
//
// T3-4 第四批 (热点): 布尔原始值**不可变** (SetProperty 是 no-op, 类型只有
// 一个 Value 字段, 引擎内没有任何按实例可写的状态), 因此直接复用两个进程级
// 单例, 不再每次 `&Boolean{...}` 分配。fib28 剖面里 NewBoolean 的分配
// (含 mallocgc / GC 标记) 合计 ~4.8% CPU —— 关系比较与 `n < 2` 这类判断
// 每轮都在装箱。单例本身早就在 iteratorhelpers / typedarray 等路径上按
// 指针身份使用, 让所有构造点统一到同一对实例反而消除了"身份比较时灵时不灵"
// 的隐患。
func NewBoolean(v bool) *Boolean {
	if v {
		return TrueSingleton
	}
	return FalseSingleton
}

// 全局布尔单例 (避免重复分配)
var (
	TrueSingleton  = &Boolean{Value: true}
	FalseSingleton = &Boolean{Value: false}
)
