package vm

import (
	"testing"
)

// ===== T5 r8dXes: proxy trap GetMethod 校验与分发 =====

// trap 存在但不可调用 → TypeError, 且必须能被 try/catch 捕获
// (合成错误走 *ThrowError 通道, 不允许逃出 JS try)。
func TestProxyTrapNotCallableThrows(t *testing.T) {
	for _, trap := range []string{"get", "set", "has", "deleteProperty", "apply", "construct", "defineProperty"} {
		t.Run(trap, func(t *testing.T) {
			var src string
			switch trap {
			case "get":
				src = `(() => { try { new Proxy({a:1}, {get: 123}).a; return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "set":
				// 注意: trap 为 null/undefined 是规范的"无 trap → 转发"形态,
				// 不抛 (GetMethod 语义); 这里用非 callable 非 null 的值。
				src = `(() => { try { new Proxy({}, {set: 123}).x = 1; return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "has":
				src = `(() => { try { "a" in new Proxy({}, {has: 123}); return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "deleteProperty":
				src = `(() => { try { delete new Proxy({a:1}, {deleteProperty: {}}).a; return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "apply":
				src = `(() => { try { new Proxy(function(){}, {apply: 123})(); return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "construct":
				src = `(() => { try { new (new Proxy(function(){}, {construct: "x"}))(); return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			case "defineProperty":
				src = `(() => { try { Object.defineProperty(new Proxy({}, {defineProperty: 9}), "b", {value: 2}); return "no-throw"; } catch (e) { return e instanceof TypeError; } })()`
			}
			assertBoolean(t, evalJS(t, src), true)
		})
	}
}

// trap 为 undefined/null → 转发 target (含嵌套 proxy target 的递归转发)
func TestProxyTrapMissingForwardsToTarget(t *testing.T) {
	assertBoolean(t, evalJS(t, `
		const inner = new Proxy({a: 1}, { get(t, k) { return k === "a" ? 99 : undefined; } });
		const outer = new Proxy(inner, { get: undefined });
		outer.a === 99;
	`), true)
	assertBoolean(t, evalJS(t, `
		const inner = new Proxy({a: 1}, {});
		const outer = new Proxy(inner, { has: null });
		"a" in outer;
	`), true)
}

// IsConstructor/IsCallable 的 proxy 语义: target 决定 (T4 r8DmKW)
func TestProxyConstructFollowsTarget(t *testing.T) {
	// target 不可构造: new 抛 TypeError
	assertBoolean(t, evalJS(t, `
		(() => { const p = new Proxy(eval, {}); try { new p(); return false; } catch (e) { return e instanceof TypeError; } })()
	`), true)
	// target 可构造: new 走 construct trap
	assertBoolean(t, evalJS(t, `
		const p = new Proxy(function(v){ this.v = v; }, { construct(t, args) { return { v: args[0] * 2 }; } });
		new p(21).v === 42;
	`), true)
}

// revoked proxy: 一切操作抛 TypeError (调用/构造/属性访问)
func TestProxyRevokedOperationsThrow(t *testing.T) {
	assertBoolean(t, evalJS(t, `
		const { proxy, revoke } = Proxy.revocable(function(){}, {});
		revoke();
		let ok = true;
		try { proxy(); ok = false; } catch (e) { ok = ok && e instanceof TypeError; }
		try { new proxy(); ok = false; } catch (e) { ok = ok && e instanceof TypeError; }
		try { proxy.x; ok = false; } catch (e) { ok = ok && e instanceof TypeError; }
		ok;
	`), true)
}

// callable proxy 的 typeof 为 "function"; revoked 后**仍为 "function"**
// (规范: revoke 置空 [[ProxyTarget]]/[[ProxyHandler]] 但不移除 ProxyCreate
// 装上的 [[Call]] 内部方法 —— typeof 语义不变, 只有真正调用时经 revoked
// 校验抛 TypeError; test262 Proxy/revocable/target-is-revoked-function-proxy,
// T5 r8dXes)。
func TestProxyTypeofCallableTarget(t *testing.T) {
	assertString(t, evalJS(t, `
		typeof new Proxy(function(){}, {});
	`), "function")
	assertString(t, evalJS(t, `
		const { proxy, revoke } = Proxy.revocable(function(){}, {});
		revoke();
		typeof proxy;
	`), "function")
}

// revocation function: 匿名 (name="") 且不可构造
func TestProxyRevocationFunctionNameAndConstruct(t *testing.T) {
	assertString(t, evalJS(t, `
		const { revoke } = Proxy.revocable({}, {});
		revoke.name;
	`), "")
	assertBoolean(t, evalJS(t, `
		(() => { const { revoke } = Proxy.revocable({}, {}); try { new revoke(); return false; } catch (e) { return e instanceof TypeError; } })()
	`), true)
}

// ===== T4 r8DmKW: 缺失内建注册 =====

func TestMissingBuiltinsRegistered(t *testing.T) {
	assertBoolean(t, evalJS(t, `
		typeof WeakRef.prototype.deref === "function" &&
		typeof FinalizationRegistry.prototype.register === "function" &&
		typeof FinalizationRegistry.prototype.unregister === "function" &&
		typeof Object.getPrototypeOf((function*(){}).prototype).next === "function" &&
		typeof RegExp.prototype[Symbol.search] === "function" &&
		typeof Math.sumPrecise === "function" &&
		typeof JSON.rawJSON === "function" &&
		typeof JSON.isRawJSON === "function" &&
		typeof Error.isError === "function" &&
		typeof Reflect.defineProperty === "function" &&
		typeof Reflect.getOwnPropertyDescriptor === "function" &&
		typeof Reflect.preventExtensions === "function";
	`), true)
}

// 内建函数对象默认不带 [[Construct]] (限定名注册也按裸名匹配名单)
func TestBuiltinNotConstructable(t *testing.T) {
	assertBoolean(t, evalJS(t, `
		let ok = true;
		for (const f of [Reflect.get, Reflect.set, Reflect.apply, Reflect.construct,
			Error.isError, Math.sumPrecise, JSON.rawJSON, WeakRef.prototype.deref,
			Object.getPrototypeOf((function*(){}).prototype).next]) {
			try { new f(); ok = false; } catch (e) { ok = ok && e instanceof TypeError; }
		}
		ok;
	`), true)
}
